package classifier

import (
	"context"
	"math"

	"github.com/spencercnorton/bitagent/internal/classifier/classification"
	"github.com/spencercnorton/bitagent/internal/classifier/llmmatch"
	"github.com/spencercnorton/bitagent/internal/llmcapture"
	"github.com/spencercnorton/bitagent/internal/model"
	"github.com/spencercnorton/bitagent/internal/tmdb"
)

const attachTmdbContentByLLMSearchName = "attach_tmdb_content_by_llm_search"

// llmMatchedTagName distinguishes LLM-matcher attaches from heuristic TMDB
// attaches — contentSource is "tmdb" either way, so without the tag they are
// indistinguishable downstream (UI badges/filters, precision review queries).
const llmMatchedTagName = "llm-matched"

// attachTmdbContentByLLMSearchAction is the two-stage LLM fallback matcher. It
// runs only for movie/tv_show torrents that earlier deterministic steps left
// unattached. It extracts a canonical {title,year,type} from the release name,
// searches TMDB, then asks the model to pick the correct candidate. In shadow
// mode (EnableLive=false) it records the would-be match via metrics and returns
// ErrUnmatched without attaching content or adding the llm-matched tag. Normal
// unattached classification and existing sideband persistence remain unchanged.
type attachTmdbContentByLLMSearchAction struct{}

func (attachTmdbContentByLLMSearchAction) name() string {
	return attachTmdbContentByLLMSearchName
}

var attachTmdbContentByLLMSearchPayloadSpec = payloadLiteral[string]{
	literal:     attachTmdbContentByLLMSearchName,
	description: "Attempt to attach TMDB content via a two-stage LLM extract+rerank on the torrent name",
}

func (attachTmdbContentByLLMSearchAction) compileAction(ctx compilerContext) (action, error) {
	if _, err := attachTmdbContentByLLMSearchPayloadSpec.Unmarshal(ctx); err != nil {
		return action{}, ctx.error(err)
	}

	return action{
		run: func(ctx executionContext) (classification.Result, error) {
			cl := ctx.result

			// The matcher's stage sequence lives in matchRunner.decide so the
			// matcher-eval command measures this exact path. Here we turn a
			// matched decision into an attach; every other outcome is a clean
			// no-match. A non-nil error is a genuine infra failure (TMDB
			// search errored) and propagates so the torrent is retried.
			decisionCtx, _ := llmcapture.WithResultTrace(ctx.Context)
			dec, err := matchRunner{
				search:        ctx.search,
				tmdb:          ctx.tmdbClient,
				lm:            ctx.llmMatch,
				resolver:      ctx.animeResolver,
				parsedTitle:   cl.BaseTitle.String,
				altTitleMatch: ctx.altTitleMatch,
			}.decide(decisionCtx, ctx.torrent, cl.ContentType)
			if err != nil {
				return cl, err
			}
			dec.ParsedTitle = cl.BaseTitle.String
			return finishLLMMatch(
				decisionCtx, cl, ctx.llmMatch, ctx.torrent, dec,
				ctx.matchDecisionObserver,
			)
		},
	}, nil
}

func (attachTmdbContentByLLMSearchAction) JSONSchema() JSONSchema {
	return attachTmdbContentByLLMSearchPayloadSpec.JSONSchema()
}

// finishLLMMatch applies the shared post-rerank policy — the confidence floor,
// content resolution, resolved-year guard, observation, shadow recording, and
// live tagging — for both the local-mirror and API candidate paths. Shadow and
// live runs execute the same read-only policy; only the final attach/tag
// mutation is suppressed in shadow mode.
func finishLLMMatch(
	ctx context.Context,
	cl classification.Result,
	lm *llmmatch.Client,
	torrent model.Torrent,
	dec MatchDecision,
	observer MatchDecisionObserver,
) (classification.Result, error) {
	var minConfidence float64
	var live, requireSourceTitle bool
	if lm != nil {
		minConfidence = lm.MinConfidence()
		live = lm.Live()
		requireSourceTitle = lm.RequireSourceTitle()
	}
	if dec.Outcome != OutcomeMatched {
		if err := observeLLMMatchDecision(ctx, observer, torrent, dec, minConfidence, nil, false, live, requireSourceTitle); err != nil {
			return cl, err
		}
		return cl, classification.ErrUnmatched
	}
	if math.IsNaN(dec.Confidence) || math.IsInf(dec.Confidence, 0) ||
		dec.Confidence < 0 || dec.Confidence > 1 ||
		math.IsNaN(minConfidence) || math.IsInf(minConfidence, 0) ||
		minConfidence <= 0 || minConfidence > 1 ||
		dec.Confidence < minConfidence {
		dec.Outcome = OutcomeDeclined
		dec.GateReason = "confidence"
		if err := observeLLMMatchDecision(ctx, observer, torrent, dec, minConfidence, nil, false, live, requireSourceTitle); err != nil {
			return cl, err
		}
		return cl, classification.ErrUnmatched
	}
	if dec.resolve == nil {
		dec.Outcome = OutcomeDeclined
		dec.GateReason = "resolve"
		if err := observeLLMMatchDecision(ctx, observer, torrent, dec, minConfidence, nil, false, live, requireSourceTitle); err != nil {
			return cl, err
		}
		return cl, classification.ErrUnmatched
	}
	content, err := dec.resolve()
	if err != nil {
		dec.Outcome = OutcomeDeclined
		dec.GateReason = "resolve"
		if observeErr := observeLLMMatchDecision(ctx, observer, torrent, dec, minConfidence, nil, false, live, requireSourceTitle); observeErr != nil {
			return cl, observeErr
		}
		return cl, classification.ErrUnmatched
	}
	// Post-resolve year re-check. llmMatchYearCompatible runs pre-attach against
	// the CANDIDATE, whose Year comes from a TMDB *search* hit — and a search hit
	// with no release_date yields Year==0, which that invariant treats as "no
	// evidence" and lets through. The resolved record does carry the year, so
	// re-running the identical invariant here closes the blind spot. Both the
	// local-mirror and API paths route through this function, so one guard covers
	// both. A 2026-08-04 census of all 38,412 live llm-matched attachments found
	// 32 movies whose resolved year contradicted every year in the release name
	// (Labyrinth 1986 -> 2026, Josee 2003 -> 2020, Borat 2006 -> 2020,
	// Gantz II 2011 -> GANTZ:O 2016) — same-title, wrong-entry collapses that the
	// title gate cannot see because the titles are genuinely identical.
	if !llmMatchYearCompatible(
		torrent.Name, dec.Extract, dec.IsTV,
		llmmatch.Candidate{Year: int(content.ReleaseYear)},
	) {
		lm.RecordRerankGate("resolved_year")
		dec.Outcome = OutcomeDeclined
		dec.GateReason = "resolved_year"
		if err := observeLLMMatchDecision(ctx, observer, torrent, dec, minConfidence, &content, false, live, requireSourceTitle); err != nil {
			return cl, err
		}
		return cl, classification.ErrUnmatched
	}
	if err := observeLLMMatchDecision(ctx, observer, torrent, dec, minConfidence, &content, true, live, requireSourceTitle); err != nil {
		return cl, err
	}
	if !live {
		lm.RecordMatch(false, dec.IsTV)
		return cl, classification.ErrUnmatched
	}
	lm.RecordMatch(true, dec.IsTV)
	cl.AttachContent(&content)
	if cl.Tags == nil {
		cl.Tags = make(map[string]struct{})
	}
	cl.Tags[llmMatchedTagName] = struct{}{}
	return cl, nil
}

func observeLLMMatchDecision(
	ctx context.Context,
	observer MatchDecisionObserver,
	torrent model.Torrent,
	dec MatchDecision,
	minConfidence float64,
	resolved *model.Content,
	wouldAttach bool,
	live bool,
	requireSourceTitle bool,
) error {
	if observer == nil {
		return nil
	}

	candidates := make([]llmmatch.Candidate, len(dec.Candidates))
	copy(candidates, dec.Candidates)
	for i := range candidates {
		candidates[i].AltTitles = append([]string(nil), candidates[i].AltTitles...)
	}
	infoHash := append([]byte(nil), torrent.InfoHash[:]...)
	observation := MatchDecisionObservation{
		InfoHash:           infoHash,
		Outcome:            dec.Outcome,
		Extract:            dec.Extract,
		ParsedTitle:        dec.ParsedTitle,
		IsTV:               dec.IsTV,
		CandidateSource:    dec.CandidateSource,
		Candidates:         candidates,
		MatchedID:          dec.MatchedID,
		MatchedTitle:       dec.MatchedTitle,
		Confidence:         dec.Confidence,
		MinConfidence:      minConfidence,
		RequireSourceTitle: requireSourceTitle,
		GateReason:         dec.GateReason,
		Resolved:           resolved != nil,
		WouldAttach:        wouldAttach,
		Live:               live,
	}
	if resolved != nil {
		observation.ResolvedYear = int(resolved.ReleaseYear)
	}
	return observer.ObserveLLMMatchDecision(ctx, observation)
}

func candidatesFromMovie(results []tmdb.SearchMovieResult, max int) []llmmatch.Candidate {
	out := make([]llmmatch.Candidate, 0, max)
	for _, r := range results {
		if len(out) >= max {
			break
		}
		out = append(out, llmmatch.Candidate{
			ID:       r.ID,
			Title:    r.Title,
			Year:     yearFromDate(r.ReleaseDate),
			Overview: r.Overview,
		})
	}
	return out
}

func candidatesFromTv(results []tmdb.SearchTvResult, max int) []llmmatch.Candidate {
	out := make([]llmmatch.Candidate, 0, max)
	for _, r := range results {
		if len(out) >= max {
			break
		}
		out = append(out, llmmatch.Candidate{
			ID:       r.ID,
			Title:    r.Name,
			Year:     yearFromDate(r.FirstAirDate),
			Overview: r.Overview,
		})
	}
	return out
}

// yearFromDate pulls the leading YYYY out of a TMDB date string ("2011-03-25").
func yearFromDate(date string) int {
	if len(date) < 4 {
		return 0
	}
	y := 0
	for i := 0; i < 4; i++ {
		if date[i] < '0' || date[i] > '9' {
			return 0
		}
		y = y*10 + int(date[i]-'0')
	}
	return y
}

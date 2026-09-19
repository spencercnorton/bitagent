package classifier

import (
	"context"
	"errors"
	"regexp"
	"strconv"
	"strings"
	"unicode"

	"github.com/spencercnorton/bitagent/internal/animedb"
	"github.com/spencercnorton/bitagent/internal/classifier/classification"
	"github.com/spencercnorton/bitagent/internal/classifier/llmmatch"
	"github.com/spencercnorton/bitagent/internal/classifier/llmsignal"
	"github.com/spencercnorton/bitagent/internal/llmcapture"
	"github.com/spencercnorton/bitagent/internal/model"
	"github.com/spencercnorton/bitagent/internal/tmdb"
)

// MatchOutcome names where the LLM matcher's decision landed for one torrent.
type MatchOutcome string

const (
	OutcomeDisabled     MatchOutcome = "disabled"      // matcher off
	OutcomeWrongType    MatchOutcome = "wrong_type"    // not movie/tv_show
	OutcomeGated        MatchOutcome = "gated"         // pre-extract Allow gate (size/files/plausibility/privacy)
	OutcomeExtractEmpty MatchOutcome = "extract_empty" // model declined to name a title
	OutcomePack         MatchOutcome = "pack"          // multi-film bundle
	OutcomeAdult        MatchOutcome = "adult"         // adult release
	OutcomeAnimeRaw     MatchOutcome = "anime_raw"     // anime with no English track
	OutcomeNoCandidates MatchOutcome = "no_candidates" // search returned nothing
	OutcomeDeclined     MatchOutcome = "declined"      // rerank chose none
	OutcomeMatched      MatchOutcome = "matched"       // rerank chose a tmdb id
)

// MatchDecision is the matcher's verdict for one torrent, computed WITHOUT
// attaching. The workflow action turns a matched decision into an attach via
// finishLLMMatch; the matcher-eval command records it. Both callers route
// through matchRunner.decide, so the eval measures exactly the production path.
type MatchDecision struct {
	Outcome         MatchOutcome
	Extract         llmmatch.Extraction
	ParsedTitle     string
	IsTV            bool
	CandidateSource string // "local" | "api" | ""
	Candidates      []llmmatch.Candidate
	MatchedID       int64
	MatchedTitle    string
	Confidence      float64
	GateReason      string

	// resolve materialises the chosen content for a live attach; set only
	// when Outcome==OutcomeMatched. A local hit returns the stored row (no
	// API call); an API hit fetches full details.
	resolve func() (model.Content, error)
}

// MatchDecisionObserver receives the final policy verdict produced by the
// online matcher action. Implementations may durably record the verdict, but
// must return an error when they cannot do so: the action fails closed before
// any live content attachment or tag mutation.
//
// The context is the same result-traced context used for the extract and
// rerank calls. That lets an observer correlate an online decision with an
// admitted provider response without putting provider or capture details in
// this classifier-layer contract.
type MatchDecisionObserver interface {
	ObserveLLMMatchDecision(context.Context, MatchDecisionObservation) error
}

// MatchDecisionObservation is an immutable snapshot of the online matcher's
// final verdict. It intentionally excludes the raw torrent name and the
// resolve closure. Candidates and InfoHash are copied before an observer is
// called so implementations cannot mutate classification state.
type MatchDecisionObservation struct {
	InfoHash           []byte
	Outcome            MatchOutcome
	Extract            llmmatch.Extraction
	ParsedTitle        string
	IsTV               bool
	CandidateSource    string
	Candidates         []llmmatch.Candidate
	MatchedID          int64
	MatchedTitle       string
	Confidence         float64
	MinConfidence      float64
	RequireSourceTitle bool
	GateReason         string
	Resolved           bool
	ResolvedYear       int
	WouldAttach        bool
	Live               bool
}

// matchRunner bundles the matcher dependencies for a single decision.
type matchRunner struct {
	search LocalSearch
	tmdb   tmdb.Client
	lm     *llmmatch.Client
	// resolver drives deterministic anime romaji/AKA resolution. May be nil in
	// tests, in which case alias resolution is simply skipped.
	resolver *animedb.Resolver
	// parsedTitle is the deterministic BaseTitle from the parse step — the
	// preferred key for alias resolution. Empty on the matcher-eval path (which
	// has no upstream parse).
	parsedTitle string
	// altTitleMatch mirrors Config.AltTitleMatch; gates alias evidence in the
	// post-rerank identity gate.
	altTitleMatch bool
}

// decide runs the matcher's stages for one torrent and returns the verdict.
// A non-nil error is a genuine infrastructure failure (a TMDB search call
// failed) that the caller should propagate so the torrent is retried — it is
// NOT the same as a clean "no match" (which is an Outcome).
func (r matchRunner) decide(ctx context.Context, t model.Torrent, ct model.NullContentType) (MatchDecision, error) {
	lm := r.lm
	if !lm.Enabled() {
		return MatchDecision{Outcome: OutcomeDisabled}, nil
	}
	if !ct.Valid ||
		(ct.ContentType != model.ContentTypeMovie && ct.ContentType != model.ContentTypeTvShow) {
		return MatchDecision{Outcome: OutcomeWrongType}, nil
	}
	if !lm.Allow(ctx, t) {
		return MatchDecision{Outcome: OutcomeGated}, nil
	}

	// Deterministic adult-release gate. This MUST run before Extract, because the
	// model cannot police this class: it misreads the release and clears its own
	// adult flag in the same breath. 'spyfam.17.05.01.aubrey.sinclair.mp4' was
	// extracted as "SPY x FAMILY" with IsAdult=false, so the ext.IsAdult gate below
	// passed and the post-rerank identity gate then agreed with itself — extraction
	// and candidate were wrong together. 20 such attachments reached production
	// (SPY x FAMILY x6, Charlotte x5, Twin Peaks, The Boys, Agatha All Along...),
	// every one of them a performer or studio name colliding with a TV title. The
	// post-resolve year guard cannot see them either: it abstains for TV.
	// Independent evidence only, no model involved.
	if adultReleaseShape(t.Name) {
		lm.RecordExtractGate("adult_shape")
		return MatchDecision{Outcome: OutcomeAdult}, nil
	}

	// Stage 1 — extract canonical identity.
	ext, err := lm.Extract(ctx, t)
	if errors.Is(err, llmcapture.ErrPrivacyBlocked) {
		return MatchDecision{Outcome: OutcomeGated}, nil
	}
	if errors.Is(err, llmcapture.ErrCaptureUnavailable) {
		return MatchDecision{}, err
	}

	// Deterministic anime alias resolution — the metadata backbone. This
	// replaces the former hardcoded applyLLMMatchEdgeOverrides / forcedLLMMatchID
	// switches: a romaji/AKA/abbreviation title resolves to a vetted TMDB id via
	// the anime_titles table (or the baked seed set), overriding the LLM's read.
	var forcedID int64
	var forcedType model.ContentType
	if r.resolver != nil {
		if alias, ok := r.resolver.Lookup(r.parsedTitle, ext.Title, t.Name); ok {
			if alias.Adult {
				// A known adult title must never attach to a mainstream entry.
				lm.RecordExtractGate("adult")
				return MatchDecision{Outcome: OutcomeAdult, Extract: ext}, nil
			}
			// The resolver supplies title/type evidence for candidate discovery.
			// Only the small human-curated seed set carries authoritative adult
			// gates or direct ids: data-built AniDB/TMDB mappings can collapse
			// franchise/OVA identities onto the wrong series, so their target
			// remains only candidate evidence and must be reranked.
			ext.IsAnime = true
			ext.Title = alias.Display
			if alias.TMDBType == model.ContentTypeTvShow {
				ext.Type = "tv"
			} else if ext.Type == "" {
				ext.Type = "movie"
			}
			if ext.English == "" {
				ext.English = llmmatch.EnglishUnknown
			}
			ext.OK = true
			if alias.DirectAttach {
				forcedID = alias.TMDBID
				forcedType = alias.TMDBType
			}
		}
	}

	if err != nil {
		return MatchDecision{
			Outcome: OutcomeExtractEmpty, Extract: ext, GateReason: "extract_error",
		}, nil
	}
	if !ext.OK {
		return MatchDecision{Outcome: OutcomeExtractEmpty, Extract: ext}, nil
	}

	// Content gates from the extraction (pack/adult), then the anime English
	// gate. Same order and semantics as the live action.
	if ext.IsPack {
		lm.RecordExtractGate("pack")
		return MatchDecision{Outcome: OutcomePack, Extract: ext}, nil
	}
	if ext.IsAdult {
		lm.RecordExtractGate("adult")
		return MatchDecision{Outcome: OutcomeAdult, Extract: ext}, nil
	}

	// Persistence sideband: a definite anime English read must survive even
	// when the outcome below is unmatched (a rejected raw IS the 'none'
	// signal), and find_match discards action results on ErrUnmatched — so
	// this cannot ride the classification.Result. Placed AFTER the pack and
	// adult gates (matching the alias-adult early return above): gated
	// content must not persist LLM-derived metadata. A private-blocked
	// torrent never reached the model: its zero extraction records nothing.
	llmsignal.Record(ctx, ext.English, ext.IsAnime)

	if ext.IsAnime {
		ok := lm.AnimeEnglishOK(ext)
		lm.RecordAnime(ext.English, ok)
		if !ok {
			return MatchDecision{Outcome: OutcomeAnimeRaw, Extract: ext}, nil
		}
	}

	isTV := ct.ContentType == model.ContentTypeTvShow || ext.Type == "tv" || forcedType == model.ContentTypeTvShow
	searchType := model.ContentTypeMovie
	if isTV {
		searchType = model.ContentTypeTvShow
	}

	// Deterministic direct attach: a vetted TMDB id from the anime backbone is
	// authoritative, so attach it WITHOUT depending on TMDB search surfacing it
	// (romaji/AKA titles frequently return nothing from search — the exact gap
	// this backbone closes). Prefer the local mirror row (zero API calls); else
	// fetch the id from TMDB on resolve.
	if forcedID != 0 {
		lm.RecordCandidateSource("alias")
		if content, ok := r.localContentByTMDBID(ctx, forcedType, forcedID); ok {
			c := content
			return MatchDecision{
				Outcome:         OutcomeMatched,
				Extract:         ext,
				IsTV:            isTV,
				CandidateSource: "alias",
				MatchedID:       forcedID,
				MatchedTitle:    c.Title,
				Confidence:      1,
				resolve:         func() (model.Content, error) { return c, nil },
			}, nil
		}
		id := forcedID
		return MatchDecision{
			Outcome:         OutcomeMatched,
			Extract:         ext,
			IsTV:            isTV,
			CandidateSource: "alias",
			MatchedID:       id,
			MatchedTitle:    ext.Title,
			Confidence:      1,
			resolve:         func() (model.Content, error) { return tmdbContentByID(ctx, r.tmdb, isTV, id) },
		}, nil
	}

	// Stage 2a (local) — mirrored content table first; a hit attaches the
	// stored row directly, costing zero TMDB API calls.
	if locals, lerr := r.localLLMCandidates(ctx, searchType, ext, lm.MaxCandidates()); lerr == nil && len(locals) > 0 {
		localByID := make(map[int64]*model.Content, len(locals))
		cands := make([]llmmatch.Candidate, 0, len(locals))
		for i := range locals {
			if locals[i].Source != model.SourceTmdb {
				continue
			}
			id, perr := strconv.ParseInt(locals[i].ID, 10, 64)
			if perr != nil {
				continue
			}
			candidate := llmmatch.Candidate{
				ID:       id,
				Title:    locals[i].Title,
				Year:     int(locals[i].ReleaseYear),
				Overview: locals[i].Overview.String,
			}
			if r.altTitleMatch {
				candidate.AltTitles = altTitlesOf(locals[i])
			}
			cands = append(cands, candidate)
			localByID[id] = &locals[i]
		}
		if len(cands) > 0 {
			lm.RecordCandidateSource("local")
			chosenID, conf, rerr := lm.RerankForMediaType(
				ctx,
				t,
				ext,
				r.parsedTitle,
				isTV,
				cands,
				llmcapture.CandidateSourceLocal,
			)
			if errors.Is(rerr, llmcapture.ErrPrivacyBlocked) {
				return MatchDecision{Outcome: OutcomeGated, Extract: ext}, nil
			}
			if errors.Is(rerr, llmcapture.ErrCaptureUnavailable) {
				return MatchDecision{}, rerr
			}
			if rerr == nil && chosenID != 0 {
				if content := localByID[chosenID]; content != nil {
					c := *content
					chosen, _ := candidateByID(cands, chosenID)
					candidateGate := r.candidateGate(
						t.Name, ext, isTV, chosen, cands,
					)
					if candidateGate != "" {
						lm.RecordRerankGate(candidateGate)
						return MatchDecision{
							Outcome:         OutcomeDeclined,
							Extract:         ext,
							IsTV:            isTV,
							CandidateSource: "local",
							Candidates:      cands,
							MatchedID:       chosenID,
							MatchedTitle:    c.Title,
							Confidence:      conf,
							GateReason:      candidateGate,
						}, nil
					}
					return MatchDecision{
						Outcome:         OutcomeMatched,
						Extract:         ext,
						IsTV:            isTV,
						CandidateSource: "local",
						Candidates:      cands,
						MatchedID:       chosenID,
						MatchedTitle:    c.Title,
						Confidence:      conf,
						resolve:         func() (model.Content, error) { return c, nil },
					}, nil
				}
			}
			// Local candidates all declined — fall through to the API search
			// (the content may be new and simply not mirrored yet).
		}
	}

	// Stage 2a (api) — TMDB search for candidates.
	lm.RecordCandidateSource("api")
	cands, searchErr := r.apiLLMCandidates(ctx, ext, isTV, lm.MaxCandidates())
	if searchErr != nil {
		return MatchDecision{Extract: ext, IsTV: isTV, CandidateSource: "api"}, searchErr
	}
	if len(cands) == 0 {
		return MatchDecision{Outcome: OutcomeNoCandidates, Extract: ext, IsTV: isTV, CandidateSource: "api"}, nil
	}
	cands = r.withCandidateAltTitles(ctx, cands, searchType)

	// Stage 2b — rerank.
	chosenID, conf, rerr := lm.RerankForMediaType(
		ctx,
		t,
		ext,
		r.parsedTitle,
		isTV,
		cands,
		llmcapture.CandidateSourceAPI,
	)
	if errors.Is(rerr, llmcapture.ErrPrivacyBlocked) {
		return MatchDecision{Outcome: OutcomeGated, Extract: ext}, nil
	}
	if errors.Is(rerr, llmcapture.ErrCaptureUnavailable) {
		return MatchDecision{}, rerr
	}
	if rerr != nil {
		return MatchDecision{
			Outcome: OutcomeDeclined, Extract: ext, IsTV: isTV,
			CandidateSource: "api", Candidates: cands, GateReason: "rerank_error",
		}, nil
	}
	if chosenID == 0 {
		return MatchDecision{
			Outcome: OutcomeDeclined, Extract: ext, IsTV: isTV,
			CandidateSource: "api", Candidates: cands, Confidence: conf,
			GateReason: "model_declined",
		}, nil
	}
	chosen, ok := candidateByID(cands, chosenID)
	candidateGate := "candidate_not_offered"
	if ok {
		candidateGate = r.candidateGate(t.Name, ext, isTV, chosen, cands)
	}
	if candidateGate != "" {
		lm.RecordRerankGate(candidateGate)
		return MatchDecision{
			Outcome: OutcomeDeclined, Extract: ext, IsTV: isTV,
			CandidateSource: "api", Candidates: cands,
			MatchedID: chosenID, MatchedTitle: candidateTitle(cands, chosenID),
			Confidence: conf, GateReason: candidateGate,
		}, nil
	}
	return MatchDecision{
		Outcome:         OutcomeMatched,
		Extract:         ext,
		IsTV:            isTV,
		CandidateSource: "api",
		Candidates:      cands,
		MatchedID:       chosenID,
		MatchedTitle:    candidateTitle(cands, chosenID),
		Confidence:      conf,
		resolve: func() (model.Content, error) {
			return tmdbContentByID(ctx, r.tmdb, isTV, chosenID)
		},
	}, nil
}

func (r matchRunner) localLLMCandidates(
	ctx context.Context,
	searchType model.ContentType,
	ext llmmatch.Extraction,
	limit int,
) ([]model.Content, error) {
	for _, q := range llmCandidateQueries(ext) {
		locals, err := r.search.ContentCandidatesBySearch(ctx, searchType, q.title, q.year, limit)
		if err != nil || len(locals) > 0 {
			return locals, err
		}
	}
	return nil, nil
}

func (r matchRunner) apiLLMCandidates(
	ctx context.Context,
	ext llmmatch.Extraction,
	isTV bool,
	limit int,
) ([]llmmatch.Candidate, error) {
	if !isTV {
		resp, searchErr := r.tmdb.SearchMovie(ctx, tmdb.SearchMovieRequest{
			Query:        ext.Title,
			Year:         model.Year(ext.Year),
			IncludeAdult: true,
		})
		if searchErr != nil {
			return nil, searchErr
		}
		return candidatesFromMovie(resp.Results, limit), nil
	}
	for _, q := range llmCandidateQueries(ext) {
		resp, searchErr := r.tmdb.SearchTv(ctx, tmdb.SearchTvRequest{
			Query:            q.title,
			FirstAirDateYear: q.year,
			IncludeAdult:     true,
		})
		if searchErr != nil {
			return nil, searchErr
		}
		if cands := candidatesFromTv(resp.Results, limit); len(cands) > 0 {
			return cands, nil
		}
	}
	return nil, nil
}

type llmCandidateQuery struct {
	title string
	year  model.Year
}

func llmCandidateQueries(ext llmmatch.Extraction) []llmCandidateQuery {
	year := model.Year(ext.Year)
	out := []llmCandidateQuery{{title: ext.Title, year: year}}
	if ext.Type == "tv" && ext.Year > 0 {
		out = append(out, llmCandidateQuery{title: ext.Title})
	}
	if ext.IsAnime && ext.Type == "tv" {
		if base := animeBaseTitle(ext.Title); base != "" && !sameFold(base, ext.Title) {
			out = append(out, llmCandidateQuery{title: base, year: year})
			if ext.Year > 0 {
				out = append(out, llmCandidateQuery{title: base})
			}
		}
	}
	return dedupeLLMQueries(out)
}

func animeBaseTitle(title string) string {
	title = strings.TrimSpace(title)
	for _, sep := range []string{":", " - "} {
		if idx := strings.Index(title, sep); idx > 0 {
			return strings.TrimSpace(title[:idx])
		}
	}
	return title
}

func dedupeLLMQueries(in []llmCandidateQuery) []llmCandidateQuery {
	out := make([]llmCandidateQuery, 0, len(in))
	seen := make(map[string]struct{}, len(in))
	for _, q := range in {
		q.title = strings.TrimSpace(q.title)
		if q.title == "" {
			continue
		}
		key := strings.ToLower(q.title) + "|" + strconv.Itoa(int(q.year))
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, q)
	}
	return out
}

func sameFold(a, b string) bool {
	return strings.EqualFold(strings.TrimSpace(a), strings.TrimSpace(b))
}

// localContentByTMDBID looks up a stored TMDB content row by id, returning
// ok=false when it isn't mirrored locally (so the caller fetches from the API).
// Used by the deterministic alias direct-attach path.
func (r matchRunner) localContentByTMDBID(ctx context.Context, ct model.ContentType, id int64) (model.Content, bool) {
	content, err := r.search.ContentByID(ctx, model.ContentRef{
		Type:   ct,
		Source: "tmdb",
		ID:     strconv.FormatInt(id, 10),
	})
	if err != nil {
		return model.Content{}, false
	}
	return content, true
}

// withAltTitles fills chosen.AltTitles from the local content mirror so the
// identity gate can recognise a catalogued alias. Enrichment is deliberately
// done for the CHOSEN candidate only — one local read per accepted rerank,
// rather than one per offered candidate — and only when the operator has
// enabled alt-title matching for the deterministic path, so both paths honour
// the same switch.
//
// Failure is silent by design: a mirror miss (or alt titles never backfilled
// for this id) simply leaves AltTitles empty and the gate falls back to the
// canonical-title comparison. This can only ever ACCEPT matches the gate would
// otherwise reject; it can never cause one to be attached that the canonical
// comparison already approved.
func (r matchRunner) withAltTitles(
	ctx context.Context,
	chosen llmmatch.Candidate,
	ct model.ContentType,
) llmmatch.Candidate {
	if !r.altTitleMatch || chosen.ID == 0 {
		return chosen
	}
	content, ok := r.localContentByTMDBID(ctx, ct, chosen.ID)
	if !ok {
		return chosen
	}
	chosen.AltTitles = altTitlesOf(content)
	return chosen
}

// withCandidateAltTitles freezes the same catalogue aliases that the final
// identity gate can consume before the provider call is captured. The aliases
// remain absent from the model prompt through Candidate's json:"-" contract.
func (r matchRunner) withCandidateAltTitles(
	ctx context.Context,
	candidates []llmmatch.Candidate,
	ct model.ContentType,
) []llmmatch.Candidate {
	if !r.altTitleMatch {
		return candidates
	}
	for i := range candidates {
		candidates[i] = r.withAltTitles(ctx, candidates[i], ct)
	}
	return candidates
}

// altTitlesOf returns the catalogued aliases for a mirror row: the stored
// alternative/translated titles plus the original-language title. Mirrors the
// set contentMatchCandidates() offers the deterministic Levenshtein path,
// minus the canonical title (which the gate compares separately).
func altTitlesOf(content model.Content) []string {
	alts := make([]string, 0, len(content.Attributes)+1)
	for _, a := range content.Attributes {
		if strings.HasPrefix(a.Key, model.AltTitleAttributePrefix) {
			alts = append(alts, a.Value)
		}
	}
	if content.OriginalTitle.Valid {
		alts = append(alts, content.OriginalTitle.String)
	}
	return alts
}

func candidateTitle(cands []llmmatch.Candidate, id int64) string {
	c, ok := candidateByID(cands, id)
	if !ok {
		return ""
	}
	return c.Title
}

func candidateByID(cands []llmmatch.Candidate, id int64) (llmmatch.Candidate, bool) {
	for _, c := range cands {
		if c.ID == id {
			return c, true
		}
	}
	return llmmatch.Candidate{}, false
}

func llmMatchYearCompatible(name string, ext llmmatch.Extraction, isTV bool, chosen llmmatch.Candidate) bool {
	if isTV || chosen.Year == 0 {
		return true
	}
	if ext.Year > 0 && absInt(ext.Year-chosen.Year) > 1 {
		return false
	}
	rawYears := releaseYearsInName(name)
	if len(rawYears) == 0 {
		return true
	}
	for _, y := range rawYears {
		if absInt(y-chosen.Year) <= 1 {
			return true
		}
	}
	return false
}

// llmMatchTitleCompatible is the final invariant between the model's extracted
// canonical identity and the canonical title belonging to the selected TMDB
// id. TMDB search can return loosely related candidates, and an LLM assigning
// high confidence to one is not sufficient evidence to attach it. This final
// boundary requires exact normalized identity: nearby names such as "Cars 2"
// and "Cars 3", or franchise/spinoff titles, are not fuzzy equivalents.
// llmMatchTitleCompatible is the final invariant between the model's extracted
// canonical identity and the canonical title belonging to the selected TMDB
// id. TMDB search can return loosely related candidates, and an LLM assigning
// high confidence to one is not sufficient evidence to attach it. This final
// boundary requires exact normalized identity: nearby names such as "Cars 2"
// and "Cars 3", or franchise/spinoff titles, are not fuzzy equivalents.
func llmMatchTitleCompatible(
	extraction llmmatch.Extraction,
	chosen llmmatch.Candidate,
) bool {
	extractedTitle := normalizeLLMIdentityTitle(extraction.Title)
	candidateTitle := normalizeLLMIdentityTitle(chosen.Title)
	if strings.TrimSpace(extractedTitle) == "" ||
		strings.TrimSpace(candidateTitle) == "" {
		return false
	}
	if extractedTitle == candidateTitle {
		return true
	}
	// An exact match against a STORED alternative/translated title is
	// authoritative same-work evidence, so it satisfies the very invariant the
	// canonical comparison exists to enforce. This is NOT a loosening of the
	// string test — it is a second exact test against catalogued aliases.
	//
	// Measured over 14h of production, 2026-08-04: this gate rejected
	// 275 of 995 confident picks (27.6%), and a hand-read of 34 semantic
	// rejections split roughly half correct / half wrong — on the order of ~120
	// correct attachments discarded per day. Containment, prefix/suffix or
	// fuzzy similarity CANNOT fix that, because the wrong and right rejections
	// share identical surface patterns: "El padrecito" -> "Cantinflas: El
	// padrecito" is the same work, "Skull Island" -> "Kong: Skull Island" is
	// not. Only a catalogued alias separates them.
	for _, alt := range chosen.AltTitles {
		if normalizeLLMIdentityTitle(alt) == extractedTitle {
			return true
		}
	}
	return false
}

// llmMatchCandidateUnambiguous prevents the model from breaking an identity
// tie that the candidate metadata itself cannot distinguish. Multiple TV ids
// with the same canonical title are always an abstention: a season number does
// not reveal which TMDB series/OVA owns it. Same-title movie remakes are safe
// only when an explicit year in the raw release uniquely supports the chosen
// candidate and excludes every duplicate.
func llmMatchCandidateUnambiguous(
	name string,
	isTV bool,
	chosen llmmatch.Candidate,
	candidates []llmmatch.Candidate,
) bool {
	chosenTitle := normalizeLLMIdentityTitle(chosen.Title)
	duplicates := make([]llmmatch.Candidate, 0, 1)
	for _, candidate := range candidates {
		if candidate.ID != chosen.ID &&
			normalizeLLMIdentityTitle(candidate.Title) == chosenTitle {
			duplicates = append(duplicates, candidate)
		}
	}
	if len(duplicates) == 0 {
		return true
	}
	if isTV || chosen.Year == 0 {
		return false
	}
	rawYears := releaseYearsInName(name)
	chosenSupported := false
	for _, year := range rawYears {
		if absInt(year-chosen.Year) <= 1 {
			chosenSupported = true
			break
		}
	}
	if !chosenSupported {
		return false
	}
	for _, duplicate := range duplicates {
		if duplicate.Year == 0 {
			return false
		}
		for _, year := range rawYears {
			if absInt(year-duplicate.Year) <= 1 {
				return false
			}
		}
	}
	return true
}

func llmMatchCandidateGate(
	name string,
	extraction llmmatch.Extraction,
	isTV bool,
	chosen llmmatch.Candidate,
	candidates []llmmatch.Candidate,
) string {
	if !llmMatchYearCompatible(name, extraction, isTV, chosen) {
		return "candidate_year"
	}
	if !llmMatchTitleCompatible(extraction, chosen) {
		return "candidate_title"
	}
	if !llmMatchCandidateUnambiguous(name, isTV, chosen, candidates) {
		return "candidate_ambiguous"
	}
	return ""
}

func (r matchRunner) candidateGate(name string, extraction llmmatch.Extraction,
	isTV bool, chosen llmmatch.Candidate, candidates []llmmatch.Candidate,
) string {
	if gate := llmMatchCandidateGate(name, extraction, isTV, chosen, candidates); gate != "" {
		return gate
	}
	if r.lm.RequireSourceTitle() {
		return EvaluationLLMMatchSourceGate(name, r.parsedTitle, isTV, chosen)
	}
	return ""
}

// EvaluationLLMMatchSourceGate is shared by live matching and canary evaluation.
// It anchors the selected catalog entry to a title parsed independently of the
// model. Exact stored aliases are permitted; model-invented aliases are not.
// Movies also require a release year in the original name, not just in the
// model's extraction. Curated anime direct-ID mappings remain authoritative and
// do not pass through candidate selection.
func EvaluationLLMMatchSourceGate(name, parsedTitle string, isTV bool, chosen llmmatch.Candidate) string {
	if !llmMatchTitleCompatible(llmmatch.Extraction{Title: parsedTitle}, chosen) {
		return "source_title"
	}
	if !isTV {
		if chosen.Year == 0 || len(releaseYearsInName(name)) == 0 {
			return "source_year"
		}
		if !llmMatchYearCompatible(name, llmmatch.Extraction{}, false, chosen) {
			return "source_year"
		}
	}
	return ""
}

// EvaluationLLMMatchCandidateGate exposes the exact final production identity
// gate to the evaluator. An offered ID and high model confidence are not an
// attachment on their own: the release year, canonical title, and duplicate
// identity checks are part of the live action contract. The empty string
// means the candidate may proceed; non-empty values are bounded policy reasons.
func EvaluationLLMMatchCandidateGate(
	name string,
	extraction llmmatch.Extraction,
	isTV bool,
	chosen llmmatch.Candidate,
	candidates []llmmatch.Candidate,
) string {
	return llmMatchCandidateGate(
		name,
		extraction,
		isTV,
		chosen,
		candidates,
	)
}

// normalizeLLMIdentityTitle is the identity the exact-match gates compare.
// It preserves word boundaries and folds only apostrophes, so "Viper's" and
// "Vipers" agree instead of producing the useless token pair "viper s".
// Measured over 14h of production rerank decisions (995 accepted matches, 275
// of them gate-rejected on title): 7 of those rejections were the same work
// differing only by an apostrophe.
//
// Leading articles are ALREADY folded upstream by
// titlenorm.NormalizeTitleForMatch step 3, so "The Dark Knight" and "Dark
// Knight" have always compared equal here — that is pre-existing behaviour,
// guarded by llmMatchCandidateUnambiguous and llmMatchYearCompatible rather
// than by this function.
//
// ponytail: word-boundary differences are deliberately NOT folded. Collapsing
// spacing would recover a further 8 rejections in the same window ("Uma Musume"
// vs "Umamusume", "K.G.F" vs "KGF", "Hoop-La" vs "Hoopla") but it also merges
// distinct works — "Black Bird" vs "Blackbird" — so it needs a real
// discriminator (positive year corroboration plus uniqueness among candidates),
// which is not worth its complexity for ~14 attachments a day. Revisit via the
// alt-title table, which already carries these variants, rather than by
// loosening this comparison.
func normalizeLLMIdentityTitle(title string) string {
	normalized := strings.Map(func(r rune) rune {
		switch r {
		case '\'', '’', 'ʼ', '`':
			return -1
		}
		return r
	}, normalizeTitleForMatch(title))
	return strings.Join(strings.FieldsFunc(normalized, func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsNumber(r)
	}), " ")
}

// adultSceneShape matches the scene-adult filename convention
// "<studio>.<YY>.<MM>.<DD>.<performer...>" — a short lowercase alphanumeric studio
// tag followed by a two-digit date triple.
//
// episodeNotation is the escape hatch. Every legitimate dated-TV release carries
// SxxExx; adult scene releases do not. Requiring its ABSENCE means a future
// "tds.16.01.08.s01e02"-style collision stays matchable instead of being silently
// discarded, without weakening the gate for anything seen today.
var (
	adultSceneShape = regexp.MustCompile(`^[a-z0-9]{2,8}\.[0-9]{2}\.[0-9]{2}\.[0-9]{2}\.`)
	episodeNotation = regexp.MustCompile(`(?i)s[0-9]{2}e[0-9]{2}`)
)

// adultReleaseShape reports whether a release name is an adult scene release on
// deterministic evidence alone.
//
// Measured against the full production corpus 2026-08-09: 16,007 torrents match
// this shape; a random sample of 25 was uniformly adult (including 'tds.15.12.07',
// so even a prefix that looks like a talk-show abbreviation is not one); the top
// leading tokens are unambiguous studios (alsscan, hegre, femjoy, rkprime,
// metartx, bangbus, anilos); and ZERO of the 16,007 carry SxxExx episode notation.
//
// TWO restrictions are load-bearing, and both were found by measuring rather than
// by reasoning:
//
//   - Lowercase only. Case-insensitive matching pulls in legitimate SPORTS, which
//     shares the convention exactly — "NHL.15.10.13.San Jose Sharks vs St. Louis
//     Blues.540p.mkv", "NHL.19-05-08.West.Final.G6.DAL-DET.avi". Those carry no
//     SxxExx either, so the escape hatch below would not save them. Widening this
//     to catch mixed-case adult variants would silently discard live sports.
//
//   - The studio tag must contain a letter. A purely numeric leading token is not
//     a studio, it is an upload date on a MySiLU-style release whose TITLE starts
//     with a number: "2011.01.03.12.Monkeys.1995" (12 Monkeys), "09.02.04.21.Grams"
//     (21 Grams), "07.04.23.50.First.Dates" (50 First Dates). Ten such films were in
//     the corpus and the shape alone would have thrown every one of them away.
//
// The known residual: a mixed-case variant such as "SpyFam - 24.04.27 - ..." is NOT
// caught (1 of the 20 remediated attachments). Catching it needs a studio allowlist
// or a sports carve-out, and neither is worth trading against a silent recall bug
// on real content. Widening an untested pattern is how a precision gate becomes one.
func adultReleaseShape(name string) bool {
	if !adultSceneShape.MatchString(name) || episodeNotation.MatchString(name) {
		return false
	}
	studio, _, _ := strings.Cut(name, ".")
	return strings.IndexAny(studio, "abcdefghijklmnopqrstuvwxyz") >= 0
}

func releaseYearsInName(name string) []int {
	var years []int
	for i := 0; i+4 <= len(name); i++ {
		if i > 0 && isDigit(name[i-1]) {
			continue
		}
		if i+4 < len(name) && isDigit(name[i+4]) {
			continue
		}
		y := 0
		ok := true
		for j := 0; j < 4; j++ {
			ch := name[i+j]
			if !isDigit(ch) {
				ok = false
				break
			}
			y = y*10 + int(ch-'0')
		}
		if ok && y >= 1900 && y <= 2099 {
			years = append(years, y)
		}
	}
	return years
}

func isDigit(ch byte) bool {
	return ch >= '0' && ch <= '9'
}

func absInt(v int) int {
	if v < 0 {
		return -v
	}
	return v
}

// tmdbContentByID fetches full content details for a chosen tmdb id, mapping a
// TMDB not-found to ErrUnmatched (the id was valid at search time but the
// details endpoint 404s — treat as no match, not a hard error).
func tmdbContentByID(ctx context.Context, client tmdb.Client, isTV bool, id int64) (model.Content, error) {
	if isTV {
		d, err := client.TvDetails(ctx, tmdb.TvDetailsRequest{
			SeriesID:         id,
			AppendToResponse: []string{"external_ids", "alternative_titles", "translations"},
		})
		if err != nil {
			if errors.Is(err, tmdb.ErrNotFound) {
				return model.Content{}, classification.ErrUnmatched
			}
			return model.Content{}, err
		}
		return tmdb.TvShowDetailsToTvShowModel(d)
	}
	d, err := client.MovieDetails(ctx, tmdb.MovieDetailsRequest{
		ID:               id,
		AppendToResponse: []string{"alternative_titles", "translations"},
	})
	if err != nil {
		if errors.Is(err, tmdb.ErrNotFound) {
			return model.Content{}, classification.ErrUnmatched
		}
		return model.Content{}, err
	}
	return tmdb.MovieDetailsToMovieModel(d)
}

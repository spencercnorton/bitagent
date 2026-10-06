package classifier

import (
	"context"
	"time"

	"github.com/google/cel-go/common/types"
	"github.com/spencercnorton/bitagent/internal/classifier/classification"
	"github.com/spencercnorton/bitagent/internal/classifier/llmmatch"
	"github.com/spencercnorton/bitagent/internal/classifier/parsers"
	"github.com/spencercnorton/bitagent/internal/model"
)

const (
	enrichTypeFallbackName     = "enrich_type_fallback"
	typeLocalEnrichedTagName   = "type-local-enriched"
	typeLocalCandidateLimit    = 12
	typeLocalEnrichmentTimeout = 2 * time.Second
)

// enrichTypeFallbackAction continues an actual unknown-to-movie/TV prediction
// after its content policy. It never invokes a workflow, fallback, API or LLM.
type enrichTypeFallbackAction struct{}

// LocalTypeEnrichmentOptions preserves the caller's parser and local metadata
// policy. There is deliberately no remote metadata or inference option.
type LocalTypeEnrichmentOptions struct {
	ParseNoiseV2          bool
	SingleEpisodeMaxBytes int64
	LocalSearchEnabled    bool
	AltTitleMatch         bool
}

func (enrichTypeFallbackAction) name() string { return enrichTypeFallbackName }

var enrichTypeFallbackPayloadSpec = payloadLiteral[string]{
	literal:     enrichTypeFallbackName,
	description: "Parse and safely resolve a newly inferred movie/TV type using local metadata only",
}

func (enrichTypeFallbackAction) compileAction(ctx compilerContext) (action, error) {
	if _, err := enrichTypeFallbackPayloadSpec.Unmarshal(ctx); err != nil {
		return action{}, ctx.error(err)
	}
	return action{run: func(ctx executionContext) (classification.Result, error) {
		cl := ctx.result
		state := ctx.typeFallback
		if state == nil || state.enriched || !state.typeApplied.Valid ||
			cl.ContentType != state.typeApplied || cl.Content != nil {
			return cl, nil
		}
		state.enriched = true
		return EnrichTypeLocally(ctx.Context, ctx.torrent, cl, ctx.search, LocalTypeEnrichmentOptions{
			ParseNoiseV2: ctx.parseNoiseV2, SingleEpisodeMaxBytes: ctx.singleEpisodeMaxBytes,
			LocalSearchEnabled: ctx.flags["local_search_enabled"] == types.True,
			AltTitleMatch:      ctx.altTitleMatch,
		})
	}}, nil
}

func (enrichTypeFallbackAction) JSONSchema() JSONSchema {
	return enrichTypeFallbackPayloadSpec.JSONSchema()
}

// EnrichTypeLocally parses and optionally resolves an already authorized,
// unattached movie/TV result. It does not run a workflow, infer a type, call a
// provider, delete content or write storage. Callers must validate their type
// evidence/source and apply their content policy before and after this helper;
// any persistence must remain bound to that same source and policy.
func EnrichTypeLocally(ctx context.Context, t model.Torrent, cl classification.Result, search LocalSearch, opts LocalTypeEnrichmentOptions) (classification.Result, error) {
	if cl.Content != nil || !cl.ContentType.Valid ||
		(cl.ContentType.ContentType != model.ContentTypeMovie && cl.ContentType.ContentType != model.ContentTypeTvShow) {
		return cl, nil
	}
	if err := ctx.Err(); err != nil {
		return cl, err
	}
	attrs, parseErr := parsers.ParseVideoContentWithOptions(t, cl, parsers.ParseOptions{NoiseV2: opts.ParseNoiseV2})
	if parseErr == nil {
		cl.Merge(attrs)
		if opts.SingleEpisodeMaxBytes > 0 && isSeasonOnlyPack(cl.Episodes) &&
			int64(t.Size) > 0 && int64(t.Size) <= opts.SingleEpisodeMaxBytes {
			cl.Episodes = nil
		}
	}
	if !cl.BaseTitle.Valid || search == nil || !opts.LocalSearchEnabled {
		return cl, nil
	}
	// Episode/release years do not imply a series premiere year, but a
	// yearless movie cannot borrow a same-title remake's identity.
	if cl.ContentType.ContentType == model.ContentTypeMovie && cl.Date.Year.IsNil() {
		return cl, nil
	}
	localCtx, cancel := context.WithTimeout(ctx, typeLocalEnrichmentTimeout)
	defer cancel()
	candidates, searchErr := search.ContentCandidatesBySearch(localCtx, cl.ContentType.ContentType,
		cl.BaseTitle.String, cl.Date.Year, typeLocalCandidateLimit)
	if searchErr != nil || len(candidates) >= typeLocalCandidateLimit {
		// A saturated bounded result cannot establish uniqueness. Optional
		// lookup failures leave the new type and parsed attributes usable.
		return cl, nil
	}
	content, ok := uniqueTypeLocalContent(cl, candidates, opts.AltTitleMatch)
	if !ok {
		return cl, nil
	}
	cl.AttachContent(&content)
	if cl.Tags == nil {
		cl.Tags = make(map[string]struct{})
	}
	cl.Tags[typeLocalEnrichedTagName] = struct{}{}
	return cl, nil
}

// A type prediction supplies no title or catalogue authority. Only an exact
// source title (or a stored alias) and unique compatible local identity may
// attach. No similarity threshold or candidate retrieval order breaks a tie.
func uniqueTypeLocalContent(cl classification.Result, candidates []model.Content, altTitles bool) (model.Content, bool) {
	if !cl.ContentType.Valid || !cl.BaseTitle.Valid ||
		(cl.ContentType.ContentType != model.ContentTypeMovie && cl.ContentType.ContentType != model.ContentTypeTvShow) ||
		(cl.ContentType.ContentType == model.ContentTypeMovie && cl.Date.Year.IsNil()) {
		return model.Content{}, false
	}
	var selected model.Content
	var found bool
	for _, content := range candidates {
		if content.Type != cl.ContentType.ContentType || content.Source == "" || content.ID == "" {
			continue
		}
		candidate := llmmatch.Candidate{Title: content.Title}
		if altTitles {
			candidate.AltTitles = altTitlesOf(content)
		} else if content.OriginalTitle.Valid {
			candidate.AltTitles = []string{content.OriginalTitle.String}
		}
		if !llmMatchTitleCompatible(llmmatch.Extraction{Title: cl.BaseTitle.String}, candidate) {
			continue
		}
		if content.Type == model.ContentTypeMovie &&
			(content.ReleaseYear.IsNil() || absInt(int(cl.Date.Year)-int(content.ReleaseYear)) > 1) {
			continue
		}
		if found && selected.Ref() != content.Ref() {
			return model.Content{}, false
		}
		selected, found = content, true
	}
	return selected, found
}

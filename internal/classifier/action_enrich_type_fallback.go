package classifier

import (
	"context"
	"time"

	"github.com/google/cel-go/common/types"
	"github.com/spencercnorton/bitagent/internal/classifier/classification"
	"github.com/spencercnorton/bitagent/internal/classifier/llmmatch"
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

func (enrichTypeFallbackAction) name() string { return enrichTypeFallbackName }

var enrichTypeFallbackPayloadSpec = payloadLiteral[string]{
	literal:     enrichTypeFallbackName,
	description: "Parse and safely resolve a newly inferred movie/TV type using local metadata only",
}

func (enrichTypeFallbackAction) compileAction(ctx compilerContext) (action, error) {
	if _, err := enrichTypeFallbackPayloadSpec.Unmarshal(ctx); err != nil {
		return action{}, ctx.error(err)
	}
	parse, err := (parseVideoContentAction{}).compileAction(ctx.child(parseVideoContentName, parseVideoContentName))
	if err != nil {
		return action{}, err
	}
	return action{run: func(ctx executionContext) (classification.Result, error) {
		cl := ctx.result
		state := ctx.typeFallback
		if state == nil || state.enriched || !state.typeApplied.Valid ||
			cl.ContentType != state.typeApplied || cl.Content != nil {
			return cl, nil
		}
		state.enriched = true
		parsed, parseErr := parse.run(ctx)
		if parseErr != nil {
			return cl, nil
		}
		cl = parsed
		if !cl.BaseTitle.Valid || ctx.search == nil || ctx.flags["local_search_enabled"] != types.True {
			return cl, nil
		}
		// A yearless movie must not borrow a same-title remake's identity. TV
		// episode/release years do not imply the series premiere year.
		if cl.ContentType.ContentType == model.ContentTypeMovie && cl.Date.Year.IsNil() {
			return cl, nil
		}
		localCtx, cancel := context.WithTimeout(ctx.Context, typeLocalEnrichmentTimeout)
		defer cancel()
		candidates, searchErr := ctx.search.ContentCandidatesBySearch(
			localCtx, cl.ContentType.ContentType, cl.BaseTitle.String, cl.Date.Year, typeLocalCandidateLimit,
		)
		if searchErr != nil || len(candidates) >= typeLocalCandidateLimit {
			// A bounded, saturated result cannot establish uniqueness. An optional
			// lookup failure leaves the new type and parsed attributes usable.
			return cl, nil
		}
		content, ok := uniqueTypeLocalContent(cl, candidates, ctx.altTitleMatch)
		if !ok {
			return cl, nil
		}
		cl.AttachContent(&content)
		if cl.Tags == nil {
			cl.Tags = make(map[string]struct{})
		}
		cl.Tags[typeLocalEnrichedTagName] = struct{}{}
		return cl, nil
	}}, nil
}

func (enrichTypeFallbackAction) JSONSchema() JSONSchema {
	return enrichTypeFallbackPayloadSpec.JSONSchema()
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

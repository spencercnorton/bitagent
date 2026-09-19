package classifier

import (
	"context"

	"github.com/spencercnorton/bitagent/internal/classifier/llmmatch"
	"github.com/spencercnorton/bitagent/internal/database/search"
	"github.com/spencercnorton/bitagent/internal/model"
)

// NewEvaluationLocalSearch returns the exact local-mirror candidate search used
// by the production matcher. It exists for the read-only corpus exporter,
// which runs the search inside one repeatable-read transaction and never calls
// TMDB or an LLM.
func NewEvaluationLocalSearch(source search.Search, cfg Config) LocalSearch {
	return localSearch{
		Search:            source,
		altTitleMatch:     cfg.AltTitleMatch,
		fuzzyMatchEnabled: cfg.FuzzyMatchEnabled,
	}
}

// EvaluationLLMCandidates runs the exact local-mirror query sequence used by
// matchRunner before its rerank call. It deliberately has no TMDB client and
// therefore cannot take the production API fallback.
func EvaluationLLMCandidates(
	ctx context.Context,
	source LocalSearch,
	searchType model.ContentType,
	extraction llmmatch.Extraction,
	limit int,
) ([]model.Content, error) {
	for _, query := range llmCandidateQueries(extraction) {
		candidates, err := source.ContentCandidatesBySearch(
			ctx,
			searchType,
			query.title,
			query.year,
			limit,
		)
		if err != nil || len(candidates) > 0 {
			return candidates, err
		}
	}
	return nil, nil
}

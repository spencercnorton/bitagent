package classifier

import (
	"context"
	"github.com/spencercnorton/bitagent/internal/classifier/classification"
	"github.com/spencercnorton/bitagent/internal/classifier/llmmatch"
	"github.com/spencercnorton/bitagent/internal/model"
)

// FinalizeDeferredMatch reuses the workflow's complete final confidence,
// resolved-year, source-identity and receipt policy without reprocessing or
// deleting a torrent. Persistence belongs to the caller's narrow transaction.
func FinalizeDeferredMatch(ctx context.Context, t model.Torrent, cl classification.Result, dec MatchDecision, lm *llmmatch.Client, observer MatchDecisionObserver) (classification.Result, error) {
	return finishLLMMatch(ctx, cl, lm, t, dec, observer)
}

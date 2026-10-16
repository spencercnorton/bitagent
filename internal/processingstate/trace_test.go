package processingstate

import (
	"context"
	"github.com/stretchr/testify/require"
	"testing"
)

func TestNormalMatcherOutcomes(t *testing.T) {
	ctx, trace := WithTrace(context.Background())
	require.Equal(t, "not_attempted", trace.Outcome(false))
	Search(ctx, false)
	require.Equal(t, "completed_unmatched", trace.Outcome(false))
	Search(ctx, true)
	require.Equal(t, "deferred", trace.Outcome(false))
	require.Equal(t, "matched", trace.Outcome(true))
}

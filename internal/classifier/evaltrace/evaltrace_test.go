package evaltrace

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestNoTraceIsNoOp(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	Record(ctx, "attach_local_content_by_search")
	require.Nil(t, From(ctx))
	require.False(t, SkipPreempt(ctx))

	var nilTrace *Trace
	require.Empty(t, nilTrace.Stages())
	require.Equal(t, "", nilTrace.AttachedBy())
}

func TestRecordAndReadBack(t *testing.T) {
	t.Parallel()

	tr := &Trace{}
	ctx := With(context.Background(), tr)

	Record(ctx, "canonical_preempt")
	Record(ctx, "attach_tmdb_content_by_search")

	require.Equal(t, []string{"canonical_preempt", "attach_tmdb_content_by_search"}, tr.Stages())
	require.Equal(t, "attach_tmdb_content_by_search", tr.AttachedBy())
}

func TestSkipPreempt(t *testing.T) {
	t.Parallel()

	ctx := With(context.Background(), &Trace{SkipCanonicalPreempt: true})
	require.True(t, SkipPreempt(ctx))
}

func TestWithNilTraceReturnsSameContext(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	require.Equal(t, ctx, With(ctx, nil))
}

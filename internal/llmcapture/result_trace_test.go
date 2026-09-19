package llmcapture

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestResultTraceIsSourceScopedAndCopiesReceipts(t *testing.T) {
	ctx, trace := WithResultTrace(context.Background())
	require.Same(t, trace, ResultTraceFrom(ctx))
	require.Nil(t, ResultTraceFrom(context.Background()))
	receipt := ResultReceipt{CaptureKey: []byte{1}, ResponseSHA256: []byte{2}, FirstObservation: true}
	trace.RecordResult(TaskMatcherRerank, CandidateSourceLocal, receipt)
	receipt.CaptureKey[0] = 9
	got, ok := trace.Result(TaskMatcherRerank, CandidateSourceLocal)
	require.True(t, ok)
	require.Equal(t, byte(1), got.CaptureKey[0])
	got.ResponseSHA256[0] = 9
	again, _ := trace.Result(TaskMatcherRerank, CandidateSourceLocal)
	require.Equal(t, byte(2), again.ResponseSHA256[0])
	_, ok = trace.Result(TaskMatcherRerank, CandidateSourceAPI)
	require.False(t, ok)
	_, separate := WithResultTrace(ctx)
	_, ok = separate.Result(TaskMatcherRerank, CandidateSourceLocal)
	require.False(t, ok)
	var absent *ResultTrace
	absent.RecordResult(TaskMatcherExtract, CandidateSourceNone, receipt)
	_, ok = absent.Result(TaskMatcherExtract, CandidateSourceNone)
	require.False(t, ok)
}

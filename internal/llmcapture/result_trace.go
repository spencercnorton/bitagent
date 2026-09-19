package llmcapture

import (
	"context"
	"sync"
)

// ResultReceipt identifies one retained provider response, not a model-visible
// identity. Only the first provider completion for a captured request belongs
// to the natural first-observation cohort; cached decisions are not included.
type ResultReceipt struct {
	CaptureKey       []byte
	ResponseSHA256   []byte
	FirstObservation bool
	StatusCode       int
	ErrorClass       string
	// FromCache replays original evidence for an idempotent decision retry;
	// it is never a new HTTP observation or a new member of the cohort.
	FromCache bool
}

type resultTraceKey struct{}

type resultStage struct {
	task   Task
	source CandidateSource
}

// ResultTrace is scoped to one classifier action. It never crosses torrents
// and carries actual HTTP completions or explicitly marked cached receipt
// replays. A replay can retry final-decision persistence but is not a newly
// observed provider decision.
type ResultTrace struct {
	mu      sync.Mutex
	results map[resultStage]ResultReceipt
}

func WithResultTrace(ctx context.Context) (context.Context, *ResultTrace) {
	trace := &ResultTrace{results: make(map[resultStage]ResultReceipt)}
	return context.WithValue(ctx, resultTraceKey{}, trace), trace
}

func ResultTraceFrom(ctx context.Context) *ResultTrace {
	trace, _ := ctx.Value(resultTraceKey{}).(*ResultTrace)
	return trace
}

func copyResultReceipt(receipt ResultReceipt) ResultReceipt {
	receipt.CaptureKey = append([]byte(nil), receipt.CaptureKey...)
	receipt.ResponseSHA256 = append([]byte(nil), receipt.ResponseSHA256...)
	return receipt
}

func (t *ResultTrace) RecordResult(task Task, source CandidateSource, receipt ResultReceipt) {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.results[resultStage{task, source}] = copyResultReceipt(receipt)
}

func (t *ResultTrace) Result(task Task, source CandidateSource) (ResultReceipt, bool) {
	if t == nil {
		return ResultReceipt{}, false
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	receipt, ok := t.results[resultStage{task, source}]
	return copyResultReceipt(receipt), ok
}

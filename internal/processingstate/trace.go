// Package processingstate records normal matcher outcomes without inference.
package processingstate

import (
	"context"
	"github.com/spencercnorton/bitagent/internal/model"
	"sync"
)

type key struct{}
type Trace struct {
	mu                sync.Mutex
	attempted, failed bool
}

func WithTrace(ctx context.Context) (context.Context, *Trace) {
	t := &Trace{}
	return context.WithValue(ctx, key{}, t), t
}
func Search(ctx context.Context, technicalFailure bool) {
	if t, ok := ctx.Value(key{}).(*Trace); ok {
		t.mu.Lock()
		defer t.mu.Unlock()
		t.attempted = true
		t.failed = t.failed || technicalFailure
	}
}
func (t *Trace) Outcome(matched bool) string {
	if matched {
		return "matched"
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.failed {
		return "deferred"
	}
	if t.attempted {
		return "completed_unmatched"
	}
	return "not_attempted"
}

type Receipt struct {
	Processing string
	Torrent    model.Torrent
	ContentID  string
	Outcome    string
}
type Recorder interface {
	RecordCore(context.Context, []Receipt)
}

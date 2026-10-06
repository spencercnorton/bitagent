package llmwork

import (
	"context"
	"errors"
	"fmt"
	"time"
)

type DeferredError struct {
	Reason     string
	RetryAfter time.Time
}

func (e DeferredError) Error() string { return ErrDeferred.Error() + ": " + e.Reason }
func (e DeferredError) Unwrap() error { return ErrDeferred }

// Handler performs current source/policy validation and a narrow transaction.
// It must not invoke the generic reprocess/delete pipeline. Dispatch control is
// independently mandatory at every provider boundary, even during replay.
type Handler interface {
	Handle(context.Context, Task) error
}

type Backend interface {
	Claim(context.Context, string) (*Lease, error)
	Finish(context.Context, Lease, string, string, time.Time) error
	Heartbeat(context.Context, Lease) error
	Completed(context.Context, Lease) (bool, error)
}

type Engine struct {
	Store   *Store
	Backend Backend
	Handler Handler
	Config  Config
	Owner   string
}

// RunOne owns only one source case. Deferred work releases its lease; uncertain
// dispatch is held. Cleanup can persist the outcome after request cancellation
// but cannot extend model inference or buy another request.
func (e Engine) RunOne(ctx context.Context) (bool, error) {
	if !e.Config.Enabled || !e.Config.WorkerEnabled {
		return false, nil
	}
	if err := e.Config.Validate(); err != nil {
		return false, err
	}
	if e.Backend == nil || e.Handler == nil || e.Owner == "" {
		return false, fmt.Errorf("owned optional model worker is not configured")
	}
	l, err := e.Backend.Claim(ctx, e.Owner)
	if err != nil || l == nil {
		return false, err
	}
	work, cancel := context.WithTimeout(ctx, e.Config.TaskTimeout)
	defer cancel()
	work = WithExecution(work, e.Store, *l)
	heartbeatDone := make(chan struct{})
	defer close(heartbeatDone)
	go func() {
		t := time.NewTicker(e.Config.LeaseDuration / 3)
		defer t.Stop()
		for {
			select {
			case <-heartbeatDone:
				return
			case <-work.Done():
				return
			case <-t.C:
				if e.Backend.Heartbeat(work, *l) != nil {
					cancel()
					return
				}
			}
		}
	}()
	err = e.Handler.Handle(work, l.Task)
	if err == nil {
		check, done := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		completed, checkErr := e.Backend.Completed(check, *l)
		done()
		if checkErr != nil {
			return true, checkErr
		}
		if completed {
			return true, nil
		}
		err = ErrHeld
	}
	state, reason := "completed", "completed"
	retry := time.Now().UTC()
	var d DeferredError
	var deferred *DeferredError
	switch {
	case errors.As(err, &deferred):
		state, reason, retry = "deferred", deferred.Reason, deferred.RetryAfter
	case errors.As(err, &d):
		state, reason, retry = "deferred", d.Reason, d.RetryAfter
	case errors.Is(err, ErrObsolete):
		state, reason = "obsolete", "source_or_policy_changed"
	case err != nil:
		state, reason = "held", "reconciliation_required"
	}
	cleanup, done := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer done()
	if finishErr := e.Backend.Finish(cleanup, *l, state, reason, retry); finishErr != nil {
		return true, finishErr
	}
	return true, nil
}

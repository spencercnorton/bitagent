package llmcapture

import (
	"context"
	"errors"
	"time"
)

type CaseFence struct {
	TaskKey         []byte
	LeaseOwner      string
	LeaseGeneration int64
	SourceDigest    []byte
	PolicyDigest    []byte
}

type DispatchRequest struct {
	CaptureKey      []byte
	Task            Task
	CandidateSource CandidateSource
	InfoHash        []byte // admission-only; never retained in the dispatch fence
	// FreshCapture is true only for the same call's OutcomeRecorded. An
	// existing capture without dispatch evidence is not known undispatched.
	FreshCapture  bool
	Case          *CaseFence
	LeaseDuration time.Duration
}

type DispatchOutcome string

// DispatchDeferredError reports a positively undispatched retry boundary using
// the database's UTC clock. It does not authorize replay of an unknown intent.
type DispatchDeferredError struct {
	Reason        string
	RetryAfterUTC time.Time
}

func (e *DispatchDeferredError) Error() string {
	return "LLM request deferred before dispatch: " + e.Reason
}

const (
	DispatchPrepared DispatchOutcome = "prepared"
	DispatchReplay   DispatchOutcome = "replay"
	DispatchBusy     DispatchOutcome = "busy"
	DispatchUnknown  DispatchOutcome = "unknown"
)

var (
	ErrDispatchBusy    = errors.New("LLM dispatch is owned by another live attempt")
	ErrDispatchUnknown = errors.New("LLM dispatch outcome is unknown; automatic retry is forbidden")
	ErrDispatchLease   = errors.New("LLM dispatch lease or source-case fence is invalid")
)

// DispatchLease is a token-fenced request attempt. Tokens must not be logged.
type DispatchLease struct {
	CaptureKey []byte
	Owner      string
	Generation int64
	Case       *CaseFence
}

type HTTPReplay struct {
	Result  HTTPResult
	Receipt ResultReceipt
}

type DispatchRecovery struct {
	CaptureKey []byte
	State      string
	Replayable bool
	// SafeToRetry requires positive not-dispatched evidence. An intent,
	// unknown result or expired response body never confers retry authority.
	SafeToRetry bool
}

// DispatchControl keeps immutable capture/result evidence separate from the
// durable dispatch fence. Implementations retain digest-only intent fences
// after ordinary capture retention removes their raw request/response data.
type DispatchControl interface {
	Enabled() bool
	Prepare(context.Context, DispatchRequest) (DispatchLease, DispatchOutcome, error)
	Reserve(context.Context, DispatchLease, string, int, int) (bool, error)
	BeginDispatch(context.Context, DispatchLease) error
	DeferNoDispatch(context.Context, DispatchLease, string, time.Time) error
	ObserveResult(context.Context, DispatchLease, ResultReceipt) error
	Replay(context.Context, DispatchRequest) (HTTPReplay, error)
	TaskRecovery(context.Context, []byte) ([]DispatchRecovery, error)
}

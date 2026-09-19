package llmcapture

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

const CaptureSchemaVersion = 1

type Task string

const (
	TaskMatcherExtract Task = "matcher_extract"
	TaskMatcherRerank  Task = "matcher_rerank"
	TaskContentFilter  Task = "contentfilter"
	TaskJunkPurge      Task = "junkpurge"
	TaskClassifierType Task = "classifier_type"
)

type CandidateSource string

const (
	CandidateSourceNone  CandidateSource = ""
	CandidateSourceLocal CandidateSource = "local"
	CandidateSourceAPI   CandidateSource = "api"
)

type SamplingOrigin string

const (
	SamplingOriginNaturalCapture     SamplingOrigin = "natural_capture"
	SamplingOriginSafetyTopUpCapture SamplingOrigin = "safety_topup_capture"
)

type Outcome string

const (
	OutcomeDisabled  Outcome = "disabled"
	OutcomeRecorded  Outcome = "recorded"
	OutcomeDuplicate Outcome = "duplicate"
)

var (
	// ErrPrivacyBlocked means the last pre-persistence privacy check rejected
	// the input. Callers must not continue to a provider.
	ErrPrivacyBlocked = errors.New("llm evaluation capture: privacy blocked")
	// ErrCaptureUnavailable is fail-closed while capture is enabled. Callers
	// must not continue to a provider because doing so would create an
	// unobserved production cohort.
	ErrCaptureUnavailable = errors.New("llm evaluation capture: unavailable")
)

type ErrInvalidConfig string

func (e ErrInvalidConfig) Error() string {
	return "llm evaluation capture: invalid config: " + string(e)
}

// Request contains the exact model-visible envelope and its structured task
// input. InfoHash and GroupKey are admission-only material: Recorder hashes
// them before writing the capture row. PostgresStore keeps the raw InfoHash
// only in the separate, local admission mapping required for a future live
// privacy recheck; it is never written to the model/candidate artifact.
type Request struct {
	Task            Task
	CandidateSource CandidateSource
	// SamplingOrigin defaults to natural_capture. The only non-natural
	// origin is junkpurge's no-provider, historical-teacher-stratified
	// safety top-up path.
	SamplingOrigin SamplingOrigin
	InfoHash       []byte
	GroupKey       []byte
	NativePrivate  bool

	Model          string
	Endpoint       string
	PromptVersion  string
	SystemPrompt   string
	ModelInputJSON json.RawMessage
	TaskInputJSON  json.RawMessage
	BuildIdentity  string
	ContractID     string
}

// Capturer is intentionally small so classifier and processor tests can
// inject a fail-closed fake without importing database machinery.
type Capturer interface {
	Enabled() bool
	Capture(context.Context, Request) (Outcome, error)
}

// PrivacyStore is satisfied by evidence.Store.
type PrivacyStore interface {
	IsPrivateInfoHash(context.Context, []byte) (bool, error)
}

type persistOutcome uint8

const (
	persistRecorded persistOutcome = iota + 1
	persistDuplicate
	persistPrivacyBlocked
)

type storedCapture struct {
	CaptureKey       []byte
	Task             Task
	CandidateSource  CandidateSource
	SourceSHA256     []byte
	GroupSHA256      []byte
	InputSHA256      []byte
	ContractSHA256   []byte
	PromptSHA256     []byte
	EndpointSHA256   []byte
	Model            string
	PromptVersion    string
	BuildIdentity    string
	ContractID       string
	SystemPrompt     string
	ModelInputJSON   []byte
	TaskInputJSON    []byte
	CapturedAt       time.Time
	ExpiresAt        time.Time
	PrivacyCheckedAt time.Time
	SamplingOrigin   string
}

type captureStore interface {
	// PersistPublic performs the final database-native public/privacy
	// admission using rawInfoHash. It stores that value only in the local,
	// expiring admission mapping; provider-facing captures and exportable
	// artifacts contain only namespaced hashes.
	PersistPublic(
		context.Context,
		storedCapture,
		[]byte,
		int,
	) (persistOutcome, error)
}

func validateTaskSource(task Task, source CandidateSource) error {
	switch task {
	case TaskMatcherExtract, TaskContentFilter, TaskJunkPurge, TaskClassifierType:
		if source != CandidateSourceNone {
			return fmt.Errorf(
				"%w: task %q cannot have candidate source %q",
				ErrCaptureUnavailable,
				task,
				source,
			)
		}
	case TaskMatcherRerank:
		if source != CandidateSourceLocal && source != CandidateSourceAPI {
			return fmt.Errorf(
				"%w: rerank candidate source must be local or api",
				ErrCaptureUnavailable,
			)
		}
	default:
		return fmt.Errorf(
			"%w: unsupported task %q",
			ErrCaptureUnavailable,
			task,
		)
	}
	return nil
}

func normalizedSamplingOrigin(
	task Task,
	origin SamplingOrigin,
) (SamplingOrigin, error) {
	if origin == "" {
		origin = SamplingOriginNaturalCapture
	}
	switch origin {
	case SamplingOriginNaturalCapture:
		return origin, nil
	case SamplingOriginSafetyTopUpCapture:
		if task != TaskJunkPurge {
			return "", fmt.Errorf(
				"%w: safety top-up capture is supported only for junkpurge",
				ErrCaptureUnavailable,
			)
		}
		return origin, nil
	default:
		return "", fmt.Errorf(
			"%w: unsupported sampling origin %q",
			ErrCaptureUnavailable,
			origin,
		)
	}
}

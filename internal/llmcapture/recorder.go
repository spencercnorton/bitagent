package llmcapture

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"runtime/debug"
	"strings"
	"time"

	"github.com/spencercnorton/bitagent/internal/version"
)

type Recorder struct {
	cfg     Config
	privacy PrivacyStore
	store   captureStore
	now     func() time.Time
}

func NewRecorder(
	cfg Config,
	privacy PrivacyStore,
	store captureStore,
) (*Recorder, error) {
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	return &Recorder{
		cfg:     cfg,
		privacy: privacy,
		store:   store,
		now:     time.Now,
	}, nil
}

func (r *Recorder) Enabled() bool {
	return r != nil && r.cfg.Enabled
}

// CurrentBuildIdentity returns the injected release tag in production. A
// source-built binary falls back to Go's VCS build settings; if those are also
// absent, the explicit unversioned marker prevents an evaluator from mistaking
// the capture for a known build.
func CurrentBuildIdentity() string {
	if tag := strings.TrimSpace(version.GitTag); tag != "" {
		return tag
	}
	if info, ok := debug.ReadBuildInfo(); ok {
		var revision, modified string
		for _, setting := range info.Settings {
			switch setting.Key {
			case "vcs.revision":
				revision = setting.Value
			case "vcs.modified":
				modified = setting.Value
			}
		}
		if revision != "" {
			return "vcs:" + revision + ";modified:" + modified
		}
	}
	return "unversioned-build"
}

func (r *Recorder) Capture(
	ctx context.Context,
	req Request,
) (Outcome, error) {
	if !r.Enabled() {
		return OutcomeDisabled, nil
	}
	if err := validateTaskSource(req.Task, req.CandidateSource); err != nil {
		return "", err
	}
	samplingOrigin, err := normalizedSamplingOrigin(
		req.Task,
		req.SamplingOrigin,
	)
	if err != nil {
		return "", err
	}
	if req.NativePrivate {
		return "", ErrPrivacyBlocked
	}
	if len(req.InfoHash) != 20 {
		return "", fmt.Errorf(
			"%w: info hash must be exactly 20 bytes",
			ErrCaptureUnavailable,
		)
	}
	if len(req.GroupKey) == 0 {
		return "", fmt.Errorf(
			"%w: audited release-family group key is required",
			ErrCaptureUnavailable,
		)
	}
	if r.privacy == nil || r.store == nil {
		return "", fmt.Errorf(
			"%w: privacy store and capture store are required",
			ErrCaptureUnavailable,
		)
	}
	isPrivate, err := r.privacy.IsPrivateInfoHash(ctx, req.InfoHash)
	if err != nil {
		return "", fmt.Errorf(
			"%w: evidence privacy recheck: %v",
			ErrCaptureUnavailable,
			err,
		)
	}
	if isPrivate {
		return "", ErrPrivacyBlocked
	}

	if len(req.ModelInputJSON)+len(req.TaskInputJSON) > r.cfg.MaxInputBytes {
		return "", fmt.Errorf(
			"%w: raw input is %d bytes, limit is %d",
			ErrCaptureUnavailable,
			len(req.ModelInputJSON)+len(req.TaskInputJSON),
			r.cfg.MaxInputBytes,
		)
	}
	modelInput, err := canonicalJSONObject(req.ModelInputJSON)
	if err != nil {
		return "", fmt.Errorf(
			"%w: model input: %v",
			ErrCaptureUnavailable,
			err,
		)
	}
	taskInput, err := canonicalJSONObject(req.TaskInputJSON)
	if err != nil {
		return "", fmt.Errorf(
			"%w: task input: %v",
			ErrCaptureUnavailable,
			err,
		)
	}
	if len(modelInput)+len(taskInput) > r.cfg.MaxInputBytes {
		return "", fmt.Errorf(
			"%w: input is %d bytes, limit is %d",
			ErrCaptureUnavailable,
			len(modelInput)+len(taskInput),
			r.cfg.MaxInputBytes,
		)
	}
	if strings.TrimSpace(req.Model) == "" ||
		strings.TrimSpace(req.Endpoint) == "" ||
		strings.TrimSpace(req.PromptVersion) == "" ||
		strings.TrimSpace(req.SystemPrompt) == "" ||
		strings.TrimSpace(req.ContractID) == "" {
		return "", fmt.Errorf(
			"%w: model, endpoint, prompt version, system prompt, and contract id are required",
			ErrCaptureUnavailable,
		)
	}
	buildIdentity := strings.TrimSpace(req.BuildIdentity)
	if buildIdentity == "" {
		buildIdentity = CurrentBuildIdentity()
	}

	sourceDigest := namespacedDigest(
		"bitagent-llm-evaluation-source-v1",
		req.InfoHash,
	)
	groupDigest := namespacedDigest(
		"bitagent-llm-evaluation-group-v1",
		req.GroupKey,
	)
	inputDigest := digestParts(modelInput, taskInput)
	promptDigest := sha256.Sum256([]byte(req.SystemPrompt))
	endpointDigest := sha256.Sum256([]byte(req.Endpoint))
	contractDigest := digestParts(
		[]byte(buildIdentity),
		[]byte(req.ContractID),
		[]byte(req.Model),
		[]byte(req.PromptVersion),
		promptDigest[:],
		endpointDigest[:],
	)
	captureKey := digestParts(
		[]byte(req.Task),
		[]byte(req.CandidateSource),
		[]byte(samplingOrigin),
		sourceDigest[:],
		groupDigest[:],
		inputDigest[:],
		contractDigest[:],
	)
	now := r.now().UTC()
	record := storedCapture{
		CaptureKey:       captureKey[:],
		Task:             req.Task,
		CandidateSource:  req.CandidateSource,
		SourceSHA256:     sourceDigest[:],
		GroupSHA256:      groupDigest[:],
		InputSHA256:      inputDigest[:],
		ContractSHA256:   contractDigest[:],
		PromptSHA256:     promptDigest[:],
		EndpointSHA256:   endpointDigest[:],
		Model:            req.Model,
		PromptVersion:    req.PromptVersion,
		BuildIdentity:    buildIdentity,
		ContractID:       req.ContractID,
		SystemPrompt:     req.SystemPrompt,
		ModelInputJSON:   modelInput,
		TaskInputJSON:    taskInput,
		CapturedAt:       now,
		ExpiresAt:        now.Add(r.cfg.Retention),
		PrivacyCheckedAt: now,
		SamplingOrigin:   string(samplingOrigin),
	}
	persisted, err := r.store.PersistPublic(
		ctx,
		record,
		req.InfoHash,
		r.cfg.MaxRows,
	)
	if err != nil {
		return "", fmt.Errorf("%w: persist: %v", ErrCaptureUnavailable, err)
	}
	switch persisted {
	case persistRecorded:
		return OutcomeRecorded, nil
	case persistDuplicate:
		return OutcomeDuplicate, nil
	case persistPrivacyBlocked:
		return "", ErrPrivacyBlocked
	default:
		return "", fmt.Errorf(
			"%w: invalid persistence outcome",
			ErrCaptureUnavailable,
		)
	}
}

func canonicalJSONObject(raw json.RawMessage) ([]byte, error) {
	if len(raw) == 0 {
		return nil, fmt.Errorf("JSON object is required")
	}
	var value map[string]any
	decoder := json.NewDecoder(strings.NewReader(string(raw)))
	decoder.UseNumber()
	if err := decoder.Decode(&value); err != nil {
		return nil, err
	}
	if value == nil {
		return nil, fmt.Errorf("JSON value must be an object")
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		if err == nil {
			return nil, fmt.Errorf("trailing JSON value")
		}
		return nil, err
	}
	return json.Marshal(value)
}

func namespacedDigest(namespace string, value []byte) [32]byte {
	return digestParts([]byte(namespace), value)
}

func digestParts(parts ...[]byte) [32]byte {
	h := sha256.New()
	for _, part := range parts {
		var length [8]byte
		n := uint64(len(part))
		for i := 7; i >= 0; i-- {
			length[i] = byte(n)
			n >>= 8
		}
		_, _ = h.Write(length[:])
		_, _ = h.Write(part)
	}
	var out [32]byte
	copy(out[:], h.Sum(nil))
	return out
}

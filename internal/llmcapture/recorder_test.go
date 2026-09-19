package llmcapture

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type privacyStub struct {
	private bool
	err     error
	calls   int
}

func (s *privacyStub) IsPrivateInfoHash(
	context.Context,
	[]byte,
) (bool, error) {
	s.calls++
	return s.private, s.err
}

type captureStoreStub struct {
	outcome persistOutcome
	err     error
	calls   int
	records []storedCapture
	raw     [][]byte
}

func (s *captureStoreStub) PersistPublic(
	_ context.Context,
	record storedCapture,
	raw []byte,
	_ int,
) (persistOutcome, error) {
	s.calls++
	s.records = append(s.records, record)
	s.raw = append(s.raw, append([]byte(nil), raw...))
	if s.outcome == 0 {
		s.outcome = persistRecorded
	}
	return s.outcome, s.err
}

func validRequest() Request {
	return Request{
		Task:           TaskMatcherExtract,
		InfoHash:       make([]byte, 20),
		GroupKey:       []byte("release-family-a"),
		Model:          "model",
		Endpoint:       "https://provider.invalid/v1/chat/completions",
		PromptVersion:  "prompt-v1",
		SystemPrompt:   "system",
		ModelInputJSON: json.RawMessage(`{"messages":[{"role":"user","content":"A"}],"model":"model"}`),
		TaskInputJSON:  json.RawMessage(`{"release_name":"A","file_paths":[]}`),
		BuildIdentity:  "v1.2.3",
		ContractID:     "matcher-extract-v1",
	}
}

func TestRecorderRejectsMissingReleaseFamilyGroup(t *testing.T) {
	cfg := NewDefaultConfig()
	cfg.Enabled = true
	privacy := &privacyStub{}
	store := &captureStoreStub{}
	recorder, err := NewRecorder(cfg, privacy, store)
	require.NoError(t, err)
	req := validRequest()
	req.GroupKey = nil

	_, err = recorder.Capture(context.Background(), req)
	require.ErrorIs(t, err, ErrCaptureUnavailable)
	require.ErrorContains(t, err, "release-family group key is required")
	assert.Zero(t, privacy.calls)
	assert.Zero(t, store.calls)
}

func TestRecorderDisabledDoesNoPrivacyOrDatabaseWork(t *testing.T) {
	cfg := NewDefaultConfig()
	privacy := &privacyStub{}
	store := &captureStoreStub{}
	recorder, err := NewRecorder(cfg, privacy, store)
	require.NoError(t, err)

	outcome, err := recorder.Capture(context.Background(), validRequest())
	require.NoError(t, err)
	assert.Equal(t, OutcomeDisabled, outcome)
	assert.Zero(t, privacy.calls)
	assert.Zero(t, store.calls)
}

func TestRecorderPrivacyAdmissionFailsClosed(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(*Request)
		privacy *privacyStub
	}{
		{
			name: "native private",
			mutate: func(req *Request) {
				req.NativePrivate = true
			},
			privacy: &privacyStub{},
		},
		{
			name:    "evidence private",
			mutate:  func(*Request) {},
			privacy: &privacyStub{private: true},
		},
		{
			name:    "evidence error",
			mutate:  func(*Request) {},
			privacy: &privacyStub{err: errors.New("database down")},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg := NewDefaultConfig()
			cfg.Enabled = true
			store := &captureStoreStub{}
			recorder, err := NewRecorder(cfg, tc.privacy, store)
			require.NoError(t, err)
			req := validRequest()
			tc.mutate(&req)

			_, err = recorder.Capture(context.Background(), req)
			if tc.name == "evidence error" {
				require.ErrorIs(t, err, ErrCaptureUnavailable)
			} else {
				require.ErrorIs(t, err, ErrPrivacyBlocked)
			}
			assert.Zero(t, store.calls)
			if tc.name == "native private" {
				assert.Zero(t, tc.privacy.calls)
			} else {
				assert.Equal(t, 1, tc.privacy.calls)
			}
		})
	}
}

func TestRecorderCanonicalDeterministicDedupIdentity(t *testing.T) {
	cfg := NewDefaultConfig()
	cfg.Enabled = true
	privacy := &privacyStub{}
	store := &captureStoreStub{}
	recorder, err := NewRecorder(cfg, privacy, store)
	require.NoError(t, err)
	fixed := time.Date(2026, 7, 24, 12, 0, 0, 0, time.UTC)
	recorder.now = func() time.Time { return fixed }

	reqA := validRequest()
	reqB := validRequest()
	reqB.ModelInputJSON = json.RawMessage(
		`{ "model":"model", "messages": [ {"content":"A","role":"user"} ] }`,
	)
	reqB.TaskInputJSON = json.RawMessage(
		`{"file_paths":[],"release_name":"A"}`,
	)
	_, err = recorder.Capture(context.Background(), reqA)
	require.NoError(t, err)
	_, err = recorder.Capture(context.Background(), reqB)
	require.NoError(t, err)
	require.Len(t, store.records, 2)

	a, b := store.records[0], store.records[1]
	assert.Equal(t, hex.EncodeToString(a.CaptureKey), hex.EncodeToString(b.CaptureKey))
	assert.JSONEq(t, string(a.ModelInputJSON), string(b.ModelInputJSON))
	assert.JSONEq(t, string(a.TaskInputJSON), string(b.TaskInputJSON))
	assert.Len(t, a.SourceSHA256, 32)
	assert.Len(t, a.GroupSHA256, 32)
	assert.Len(t, a.EndpointSHA256, 32)
	assert.NotEqual(t, make([]byte, 20), a.SourceSHA256)
	assert.Equal(t, fixed.Add(cfg.Retention), a.ExpiresAt)
	assert.Equal(t, "verified", func() string {
		if a.PrivacyCheckedAt == fixed {
			return "verified"
		}
		return "missing"
	}())
}

func TestRecorderBindsAuditedSamplingOriginIntoCaptureIdentity(
	t *testing.T,
) {
	cfg := NewDefaultConfig()
	cfg.Enabled = true
	store := &captureStoreStub{}
	recorder, err := NewRecorder(cfg, &privacyStub{}, store)
	require.NoError(t, err)

	natural := validRequest()
	natural.Task = TaskJunkPurge
	topUp := natural
	topUp.SamplingOrigin = SamplingOriginSafetyTopUpCapture
	_, err = recorder.Capture(context.Background(), natural)
	require.NoError(t, err)
	_, err = recorder.Capture(context.Background(), topUp)
	require.NoError(t, err)
	require.Len(t, store.records, 2)
	assert.Equal(
		t,
		string(SamplingOriginNaturalCapture),
		store.records[0].SamplingOrigin,
	)
	assert.Equal(
		t,
		string(SamplingOriginSafetyTopUpCapture),
		store.records[1].SamplingOrigin,
	)
	assert.NotEqual(
		t,
		hex.EncodeToString(store.records[0].CaptureKey),
		hex.EncodeToString(store.records[1].CaptureKey),
	)

	invalid := validRequest()
	invalid.SamplingOrigin = SamplingOriginSafetyTopUpCapture
	_, err = recorder.Capture(context.Background(), invalid)
	require.ErrorIs(t, err, ErrCaptureUnavailable)
	require.ErrorContains(t, err, "supported only for junkpurge")
}

func TestRecorderRejectsOversizeAndInvalidRerankSource(t *testing.T) {
	cfg := NewDefaultConfig()
	cfg.Enabled = true
	cfg.MaxInputBytes = 40
	store := &captureStoreStub{}
	recorder, err := NewRecorder(cfg, &privacyStub{}, store)
	require.NoError(t, err)

	_, err = recorder.Capture(context.Background(), validRequest())
	require.ErrorIs(t, err, ErrCaptureUnavailable)
	assert.Zero(t, store.calls)

	cfg.MaxInputBytes = 256 << 10
	recorder, err = NewRecorder(cfg, &privacyStub{}, store)
	require.NoError(t, err)
	req := validRequest()
	req.Task = TaskMatcherRerank
	_, err = recorder.Capture(context.Background(), req)
	require.ErrorIs(t, err, ErrCaptureUnavailable)
	assert.Zero(t, store.calls)
}

func TestRecorderDatabasePrivacyRecheckBlocks(t *testing.T) {
	cfg := NewDefaultConfig()
	cfg.Enabled = true
	store := &captureStoreStub{outcome: persistPrivacyBlocked}
	recorder, err := NewRecorder(cfg, &privacyStub{}, store)
	require.NoError(t, err)

	_, err = recorder.Capture(context.Background(), validRequest())
	require.ErrorIs(t, err, ErrPrivacyBlocked)
	assert.Equal(t, 1, store.calls)
}

package llmcapture

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"math"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

type typeResultStoreStub struct {
	resultStoreStub
	typeDecisions [][]byte
	rechecks      int
}

func (s *typeResultStoreStub) RecheckTypeRequest(context.Context, []byte, []byte) error {
	s.rechecks++
	return s.err
}

func (s *typeResultStoreStub) PersistTypeDecision(_ context.Context, _ ResultReceipt, _ []byte, body []byte, _ time.Time) error {
	s.typeDecisions = append(s.typeDecisions, append([]byte(nil), body...))
	return s.err
}

func TestClassifierTypeCaptureHasSeparateNaturalIdentity(t *testing.T) {
	cfg := NewDefaultConfig()
	cfg.Enabled = true
	store := &captureStoreStub{}
	r, err := NewRecorder(cfg, &privacyStub{}, store)
	require.NoError(t, err)
	req := validRequest()
	_, err = r.Capture(context.Background(), req)
	require.NoError(t, err)
	req.Task = TaskClassifierType
	_, err = r.Capture(context.Background(), req)
	require.NoError(t, err)
	require.Len(t, store.records, 2)
	require.NotEqual(t, store.records[0].CaptureKey, store.records[1].CaptureKey)
	require.Equal(t, TaskClassifierType, store.records[1].Task)
	key, err := KeyForRequest(req)
	require.NoError(t, err)
	require.Equal(t, key, store.records[1].CaptureKey)
	req.CandidateSource = CandidateSourceLocal
	_, err = r.Capture(context.Background(), req)
	require.ErrorIs(t, err, ErrCaptureUnavailable)
	req.CandidateSource = CandidateSourceNone
	req.SamplingOrigin = SamplingOriginSafetyTopUpCapture
	_, err = r.Capture(context.Background(), req)
	require.ErrorIs(t, err, ErrCaptureUnavailable)
	req.SamplingOrigin = SamplingOriginNaturalCapture
	req.NativePrivate = true
	_, err = r.Capture(context.Background(), req)
	require.ErrorIs(t, err, ErrPrivacyBlocked)
	require.Len(t, store.records, 2)
}

func TestTypeRequestRecheckRequiresEnabledBoundSourceAndStore(t *testing.T) {
	cfg := NewDefaultConfig()
	cfg.Enabled = true
	store := &typeResultStoreStub{}
	r, err := NewRecorder(cfg, &privacyStub{}, store)
	require.NoError(t, err)
	key, infoHash := make([]byte, 32), make([]byte, 20)
	require.NoError(t, r.RecheckTypeRequest(context.Background(), key, infoHash))
	require.Equal(t, 1, store.rechecks)
	require.ErrorIs(t, r.RecheckTypeRequest(context.Background(), key[:31], infoHash), ErrCaptureUnavailable)
	require.ErrorIs(t, r.RecheckTypeRequest(context.Background(), key, infoHash[:19]), ErrCaptureUnavailable)
	require.Equal(t, 1, store.rechecks)
	store.err = errors.New("database unavailable")
	require.ErrorIs(t, r.RecheckTypeRequest(context.Background(), key, infoHash), ErrCaptureUnavailable)
	withoutStore, err := NewRecorder(cfg, &privacyStub{}, &captureStoreStub{})
	require.NoError(t, err)
	require.ErrorIs(t, withoutStore.RecheckTypeRequest(context.Background(), key, infoHash), ErrCaptureUnavailable)
	cfg.Enabled = false
	disabled, err := NewRecorder(cfg, nil, nil)
	require.NoError(t, err)
	require.ErrorIs(t, disabled.RecheckTypeRequest(context.Background(), key, infoHash), ErrCaptureUnavailable)
}

func TestTypeDecisionRecorderRequiresSuccessfulBoundReceiptAndStore(t *testing.T) {
	cfg := NewDefaultConfig()
	cfg.Enabled = true
	store := &typeResultStoreStub{}
	r, err := NewRecorder(cfg, &privacyStub{}, store)
	require.NoError(t, err)
	receipt := ResultReceipt{
		CaptureKey: bytes.Repeat([]byte{1}, 32), ResponseSHA256: bytes.Repeat([]byte{2}, 32),
		FirstObservation: true, StatusCode: 200, ErrorClass: "none",
	}
	decision := TypeDecision{Outcome: "classified", Category: "movie", Confidence: .9, MinConfidence: .75, WouldApply: true}
	infoHash := make([]byte, 20)
	require.NoError(t, r.RecordTypeDecision(context.Background(), receipt, infoHash, decision))
	var retained TypeDecision
	require.NoError(t, json.Unmarshal(store.typeDecisions[0], &retained))
	require.Equal(t, decision, retained)
	for _, mutate := range []func(*ResultReceipt){
		func(r *ResultReceipt) { r.CaptureKey = r.CaptureKey[:31] },
		func(r *ResultReceipt) { r.ResponseSHA256 = nil },
		func(r *ResultReceipt) { r.StatusCode = 503 },
		func(r *ResultReceipt) { r.ErrorClass = "envelope" },
	} {
		bad := receipt
		mutate(&bad)
		require.ErrorIs(t, r.RecordTypeDecision(context.Background(), bad, infoHash, decision), ErrCaptureUnavailable)
	}
	require.ErrorIs(t, r.RecordTypeDecision(context.Background(), receipt, infoHash[:19], decision), ErrCaptureUnavailable)
	badDecision := decision
	badDecision.Category = "provider supplied arbitrary label"
	require.ErrorIs(t, r.RecordTypeDecision(context.Background(), receipt, infoHash, badDecision), ErrCaptureUnavailable)
	require.Len(t, store.typeDecisions, 1)
	store.err = errors.New("database unavailable")
	require.ErrorIs(t, r.RecordTypeDecision(context.Background(), receipt, infoHash, decision), ErrCaptureUnavailable)
	withoutStore, err := NewRecorder(cfg, &privacyStub{}, &resultStoreStub{})
	require.NoError(t, err)
	require.ErrorIs(t, withoutStore.RecordTypeDecision(context.Background(), receipt, infoHash, decision), ErrCaptureUnavailable)
	cfg.Enabled = false
	disabled, err := NewRecorder(cfg, nil, nil)
	require.NoError(t, err)
	require.NoError(t, disabled.RecordTypeDecision(context.Background(), ResultReceipt{}, nil, TypeDecision{}))
}

func TestValidTypeDecision(t *testing.T) {
	for _, category := range []string{"movie", "tv", "music", "audiobook", "book"} {
		for _, live := range []bool{false, true} {
			require.True(t, validTypeDecision(TypeDecision{
				Outcome: "classified", Category: category, Confidence: .75, MinConfidence: .75, WouldApply: true, Live: live,
			}))
		}
	}
	cases := []struct {
		name string
		d    TypeDecision
		want bool
	}{
		{"unknown", TypeDecision{Outcome: "unknown", Category: "unknown", Confidence: 0, MinConfidence: 1}, true},
		{"unknown_live", TypeDecision{Outcome: "unknown", Category: "unknown", Confidence: 1, MinConfidence: .75, Live: true}, true},
		{"low_confidence", TypeDecision{Outcome: "low_confidence", Category: "tv", Confidence: .2, MinConfidence: .75}, true},
		{"zero_confidence_known", TypeDecision{Outcome: "low_confidence", Category: "tv", Confidence: 0, MinConfidence: .75}, true},
		{"invalid_response", TypeDecision{Outcome: "invalid_response", MinConfidence: .75}, true},
		{"positive_minimum", TypeDecision{Outcome: "classified", Category: "book", Confidence: 1e-8, MinConfidence: 1e-9, WouldApply: true}, true},
		{"maximum", TypeDecision{Outcome: "classified", Category: "music", Confidence: 1, MinConfidence: 1, WouldApply: true}, true},
		{"below_maximum_threshold", TypeDecision{Outcome: "classified", Category: "music", Confidence: .999, MinConfidence: 1, WouldApply: true}, false},
		{"nan_confidence", TypeDecision{Outcome: "unknown", Category: "unknown", Confidence: math.NaN(), MinConfidence: .75}, false},
		{"nan_minimum", TypeDecision{Outcome: "unknown", Category: "unknown", MinConfidence: math.NaN()}, false},
		{"inf_minimum", TypeDecision{Outcome: "unknown", Category: "unknown", MinConfidence: math.Inf(1)}, false},
		{"inf_confidence", TypeDecision{Outcome: "unknown", Category: "unknown", Confidence: math.Inf(-1), MinConfidence: .75}, false},
		{"negative_confidence", TypeDecision{Outcome: "unknown", Category: "unknown", Confidence: -.1, MinConfidence: .75}, false},
		{"excess_confidence", TypeDecision{Outcome: "unknown", Category: "unknown", Confidence: 1.1, MinConfidence: .75}, false},
		{"zero_minimum", TypeDecision{Outcome: "unknown", Category: "unknown", MinConfidence: 0}, false},
		{"negative_minimum", TypeDecision{Outcome: "unknown", Category: "unknown", MinConfidence: -.1}, false},
		{"excess_minimum", TypeDecision{Outcome: "unknown", Category: "unknown", MinConfidence: 1.1}, false},
		{"arbitrary_category", TypeDecision{Outcome: "classified", Category: "documentary", Confidence: .9, MinConfidence: .75, WouldApply: true}, false},
		{"whitespace_category", TypeDecision{Outcome: "classified", Category: "movie\n", Confidence: .9, MinConfidence: .75, WouldApply: true}, false},
		{"unknown_classified", TypeDecision{Outcome: "classified", Category: "unknown", Confidence: .9, MinConfidence: .75, WouldApply: true}, false},
		{"classified_not_applicable", TypeDecision{Outcome: "classified", Category: "movie", Confidence: .9, MinConfidence: .75}, false},
		{"classified_below_threshold", TypeDecision{Outcome: "classified", Category: "movie", Confidence: .7, MinConfidence: .75, WouldApply: true}, false},
		{"unknown_known_category", TypeDecision{Outcome: "unknown", Category: "movie", MinConfidence: .75}, false},
		{"unknown_applicable", TypeDecision{Outcome: "unknown", Category: "unknown", MinConfidence: .75, WouldApply: true}, false},
		{"low_confidence_at_threshold", TypeDecision{Outcome: "low_confidence", Category: "movie", Confidence: .75, MinConfidence: .75}, false},
		{"low_confidence_unknown", TypeDecision{Outcome: "low_confidence", Category: "unknown", MinConfidence: .75}, false},
		{"low_confidence_applicable", TypeDecision{Outcome: "low_confidence", Category: "movie", MinConfidence: .75, WouldApply: true}, false},
		{"invalid_response_with_category", TypeDecision{Outcome: "invalid_response", Category: "unknown", MinConfidence: .75}, false},
		{"invalid_response_with_confidence", TypeDecision{Outcome: "invalid_response", Confidence: .1, MinConfidence: .75}, false},
		{"invalid_response_applicable", TypeDecision{Outcome: "invalid_response", MinConfidence: .75, WouldApply: true}, false},
		{"arbitrary_outcome", TypeDecision{Outcome: "applied", Category: "movie", Confidence: .9, MinConfidence: .75, WouldApply: true}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.want, validTypeDecision(tc.d))
			toggled := tc.d
			toggled.Live = !toggled.Live
			require.Equal(t, tc.want, validTypeDecision(toggled), "live mode does not bypass policy validation")
		})
	}
}

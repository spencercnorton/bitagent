package llmcapture

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"math"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

type resultStoreStub struct {
	captureStoreStub
	first     bool
	err       error
	results   []storedHTTPResult
	decisions [][]byte
}

func (s *resultStoreStub) FindContentFilterReplay(context.Context, []byte, []byte) (ContentFilterReplay, error) {
	return ContentFilterReplay{}, s.err
}

func (s *resultStoreStub) PersistHTTPResult(_ context.Context, result storedHTTPResult) (bool, error) {
	s.results = append(s.results, result)
	return s.first, s.err
}

func (s *resultStoreStub) PersistMatchDecision(_ context.Context, _ ResultReceipt, _ []byte, body []byte, _ time.Time) error {
	s.decisions = append(s.decisions, append([]byte(nil), body...))
	return s.err
}

func (s *resultStoreStub) PersistContentFilterDecision(_ context.Context, _ ResultReceipt, _ []byte, body []byte, _ time.Time) error {
	s.decisions = append(s.decisions, append([]byte(nil), body...))
	return s.err
}

func TestRequestResultKeyMatchesActualRecorderAdmission(t *testing.T) {
	for _, source := range []CandidateSource{CandidateSourceNone, CandidateSourceLocal, CandidateSourceAPI} {
		cfg := NewDefaultConfig()
		cfg.Enabled = true
		store := &captureStoreStub{}
		recorder, err := NewRecorder(cfg, &privacyStub{}, store)
		require.NoError(t, err)
		req := validRequest()
		req.CandidateSource = source
		if source != CandidateSourceNone {
			req.Task = TaskMatcherRerank
		}
		_, err = recorder.Capture(context.Background(), req)
		require.NoError(t, err)
		key, err := KeyForRequest(req)
		require.NoError(t, err)
		require.Equal(t, store.records[0].CaptureKey, key)
	}
}

func TestResultRecorderBoundsHashesAndFailsClosed(t *testing.T) {
	cfg := NewDefaultConfig()
	cfg.Enabled = true
	store := &resultStoreStub{first: true}
	r, err := NewRecorder(cfg, &privacyStub{}, store)
	require.NoError(t, err)
	key := bytes.Repeat([]byte{1}, 32)
	result := HTTPResult{Body: []byte(`{"choices":[]}`), StatusCode: 200, ErrorClass: "none"}
	receipt, err := r.RecordHTTPResult(context.Background(), key, result)
	require.NoError(t, err)
	require.True(t, receipt.FirstObservation)
	want := sha256.Sum256(result.Body)
	require.Equal(t, want[:], receipt.ResponseSHA256)
	require.Equal(t, result.Body, store.results[0].Body)
	key[0] = 9
	result.Body[0] = 'x'
	require.Equal(t, byte(1), store.results[0].CaptureKey[0])
	require.Equal(t, byte('{'), store.results[0].Body[0])
	for _, invalid := range []HTTPResult{
		{Body: make([]byte, MaxResultBodyBytes+1), StatusCode: 200, ErrorClass: "none"},
		{StatusCode: 600, ErrorClass: "none"}, {StatusCode: 200, ErrorClass: "arbitrary_provider_message"},
	} {
		_, err := r.RecordHTTPResult(context.Background(), key, invalid)
		require.ErrorIs(t, err, ErrCaptureUnavailable)
	}
	require.Len(t, store.results, 1)
	store.err = errors.New("database unavailable")
	_, err = r.RecordHTTPResult(context.Background(), key, HTTPResult{StatusCode: 0, ErrorClass: "transport"})
	require.ErrorIs(t, err, ErrCaptureUnavailable)
}

func TestDecisionRecorderRequiresBoundFirstObservation(t *testing.T) {
	cfg := NewDefaultConfig()
	cfg.Enabled = true
	store := &resultStoreStub{first: true}
	r, err := NewRecorder(cfg, &privacyStub{}, store)
	require.NoError(t, err)
	receipt := ResultReceipt{CaptureKey: make([]byte, 32), ResponseSHA256: make([]byte, 32), FirstObservation: true}
	decision := MatchDecision{Outcome: "matched", ChosenID: 1, Confidence: .9, MinConfidence: .75, WouldAttach: true}
	require.NoError(t, r.RecordMatchDecision(context.Background(), receipt, make([]byte, 20), decision))
	for _, bad := range []MatchDecision{
		{Outcome: "matched", ChosenID: 0, Confidence: .9, MinConfidence: .75, WouldAttach: true},
		{Outcome: "matched", ChosenID: 1, Confidence: .5, MinConfidence: .75, WouldAttach: true},
		{Outcome: "matched", ChosenID: 1, Confidence: math.NaN(), MinConfidence: .75},
	} {
		require.ErrorIs(t, r.RecordMatchDecision(context.Background(), receipt, make([]byte, 20), bad), ErrCaptureUnavailable)
	}
	receipt.FirstObservation = false
	require.NoError(t, r.RecordMatchDecision(context.Background(), receipt, make([]byte, 20), decision), "store validates duplicate against already-written identical decision")
	require.Len(t, store.decisions, 2)
}

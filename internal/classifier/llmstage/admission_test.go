package llmstage

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/spencercnorton/bitagent/internal/classifier"
	"github.com/spencercnorton/bitagent/internal/classifier/classification"
	"github.com/spencercnorton/bitagent/internal/llmcapture"
	"github.com/stretchr/testify/require"
)

type testBudget struct {
	used atomic.Int32
	err  error
}

func (b *testBudget) Reserve(_ context.Context, daily, monthly int) (bool, error) {
	if b.err != nil {
		return false, b.err
	}
	limit := min(daily, monthly)
	if limit <= 0 {
		return false, nil
	}
	for {
		n := b.used.Load()
		if n >= int32(limit) {
			return false, nil
		}
		if b.used.CompareAndSwap(n, n+1) {
			return true, nil
		}
	}
}

type testAudit struct {
	duplicate                                      bool
	mu                                             sync.Mutex
	disabled                                       bool
	captureErr, resultErr, decisionErr, recheckErr error
	requests                                       []llmcapture.Request
	results                                        []llmcapture.HTTPResult
	decisions                                      []llmcapture.TypeDecision
	resultContextErr                               error
	resultDeadline                                 bool
}

func (a *testAudit) Enabled() bool                                            { return !a.disabled }
func (a *testAudit) RecheckTypeRequest(context.Context, []byte, []byte) error { return a.recheckErr }
func (a *testAudit) Capture(_ context.Context, r llmcapture.Request) (llmcapture.Outcome, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.requests = append(a.requests, r)
	if a.duplicate {
		return llmcapture.OutcomeDuplicate, a.captureErr
	}
	return llmcapture.OutcomeRecorded, a.captureErr
}
func (a *testAudit) RecordHTTPResult(ctx context.Context, key []byte, r llmcapture.HTTPResult) (llmcapture.ResultReceipt, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.results = append(a.results, r)
	a.resultContextErr = ctx.Err()
	_, a.resultDeadline = ctx.Deadline()
	h := sha256.Sum256(r.Body)
	return llmcapture.ResultReceipt{CaptureKey: key, ResponseSHA256: h[:], FirstObservation: true, StatusCode: r.StatusCode, ErrorClass: r.ErrorClass}, a.resultErr
}
func (a *testAudit) RecordTypeDecision(_ context.Context, _ llmcapture.ResultReceipt, _ []byte, d llmcapture.TypeDecision) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.decisions = append(a.decisions, d)
	return a.decisionErr
}

func TestAdmissionDenialsNeverDispatch(t *testing.T) {
	for _, tc := range []string{"missing_budget", "missing_capture", "disabled_capture", "capture_error", "budget_error", "zero_budget", "request_size", "missing_privacy", "bad_config", "final_privacy", "duplicate_capture"} {
		t.Run(tc, func(t *testing.T) {
			cfg := NewDefaultConfig()
			cfg.Enabled = true
			cfg.EnableLive = true
			var calls atomic.Int32
			s := newStageWithServer(t, cfg, fakeInner{err: classification.ErrUnmatched}, fakePrivacy{}, func(w http.ResponseWriter, r *http.Request) { calls.Add(1); respondWith(w, "tv", .99) })
			a := s.admission.Capture.(*testAudit)
			switch tc {
			case "missing_budget":
				s.admission.Budget = nil
			case "missing_capture":
				s.admission.Capture = nil
			case "disabled_capture":
				a.disabled = true
			case "capture_error":
				a.captureErr = errFake
			case "budget_error":
				s.admission.Budget = &testBudget{err: errFake}
			case "zero_budget":
				s.cfg.DailyCallLimit = 0
			case "request_size":
				s.cfg.MaxRequestBytes = 1
			case "missing_privacy":
				s.privacy = nil
			case "bad_config":
				s.cfg.Timeout = 0
			case "final_privacy":
				a.recheckErr = errFake
			case "duplicate_capture":
				a.duplicate = true
			}
			res, err := s.Run(context.Background(), "", classifier.Flags{}, baseTorrent())
			require.ErrorIs(t, err, classification.ErrUnmatched)
			require.False(t, res.ContentType.Valid)
			require.Zero(t, calls.Load())
			registry := prometheus.NewRegistry()
			require.NoError(t, registry.Register(s.metrics.callDuration))
			families, gatherErr := registry.Gather()
			require.NoError(t, gatherErr)
			for _, family := range families {
				for _, metric := range family.Metric {
					require.Zero(t, metric.GetHistogram().GetSampleCount(), "admission rejection is not an HTTP round trip")
				}
			}
		})
	}
}

func TestHTTPAndPolicyEvidencePrecedeApplication(t *testing.T) {
	for _, tc := range []string{"shadow", "live", "result_failure", "decision_failure", "unknown", "low_confidence", "malformed", "http_error", "too_large"} {
		t.Run(tc, func(t *testing.T) {
			cfg := NewDefaultConfig()
			cfg.Enabled = true
			cfg.EnableLive = tc != "shadow"
			var sent []byte
			s := newStageWithServer(t, cfg, fakeInner{err: classification.ErrUnmatched}, fakePrivacy{}, func(w http.ResponseWriter, r *http.Request) {
				sent, _ = io.ReadAll(r.Body)
				switch tc {
				case "unknown":
					respondWith(w, "unknown", .9)
				case "low_confidence":
					respondWith(w, "tv", .1)
				case "malformed":
					_, _ = w.Write([]byte(`{"choices":[]}`))
				case "http_error":
					w.WriteHeader(429)
					_, _ = w.Write([]byte(`{"error":"limited"}`))
				case "too_large":
					_, _ = w.Write([]byte(strings.Repeat("x", (64<<10)+1)))
				default:
					respondWith(w, "tv", .99)
				}
			})
			a := s.admission.Capture.(*testAudit)
			if tc == "result_failure" {
				a.resultErr = errFake
			}
			if tc == "decision_failure" {
				a.decisionErr = errFake
			}
			res, err := s.Run(context.Background(), "", classifier.Flags{}, baseTorrent())
			if tc == "live" {
				require.NoError(t, err)
				require.True(t, res.ContentType.Valid)
			} else {
				require.ErrorIs(t, err, classification.ErrUnmatched)
				require.False(t, res.ContentType.Valid)
			}
			require.Len(t, a.requests, 1)
			require.Equal(t, llmcapture.TaskClassifierType, a.requests[0].Task)
			require.Equal(t, "classifier-type-v1", a.requests[0].ContractID)
			require.JSONEq(t, string(sent), string(a.requests[0].ModelInputJSON))
			require.NotContains(t, string(sent), "test-key")
			require.Len(t, a.results, 1)
			require.True(t, a.resultDeadline)
			require.NoError(t, a.resultContextErr)
			if tc == "result_failure" || tc == "http_error" || tc == "too_large" {
				require.Empty(t, a.decisions)
			} else {
				require.Len(t, a.decisions, 1)
			}
			if tc == "malformed" {
				require.Equal(t, "invalid_response", a.decisions[0].Outcome)
			}
			if tc == "too_large" {
				require.Equal(t, 64<<10, len(a.results[0].Body))
				require.Equal(t, "read", a.results[0].ErrorClass)
			}
		})
	}
}

func TestCachePolicyBindingAndAuditRecheck(t *testing.T) {
	cfg := NewDefaultConfig()
	cfg.Enabled = true
	cfg.EnableLive = true
	var calls atomic.Int32
	s := newStageWithServer(t, cfg, fakeInner{err: classification.ErrUnmatched}, fakePrivacy{}, func(w http.ResponseWriter, r *http.Request) { calls.Add(1); respondWith(w, "movie", .99) })
	a := s.admission.Capture.(*testAudit)
	b := s.admission.Budget.(*testBudget)
	_, err := s.Run(context.Background(), "", nil, baseTorrent())
	require.NoError(t, err)
	a.decisionErr = errFake // e.g. source was privatized or capture expired
	_, err = s.Run(context.Background(), "", nil, baseTorrent())
	require.ErrorIs(t, err, classification.ErrUnmatched)
	require.Equal(t, int32(1), calls.Load())
	require.Equal(t, int32(1), b.used.Load())
	require.Len(t, a.decisions, 2)
	key := s.cacheKey(baseTorrent())
	tor := baseTorrent()
	tor.InfoHash[0]++
	require.NotEqual(t, key, s.cacheKey(tor))
	tor = baseTorrent()
	tor.Size++
	require.NotEqual(t, key, s.cacheKey(tor))
	tor = baseTorrent()
	tor.Name = strings.ToLower(tor.Name)
	require.NotEqual(t, key, s.cacheKey(tor))
	s.cfg.EnableLive = false
	require.NotEqual(t, key, s.cacheKey(baseTorrent()))
}

func TestConcurrencyAndDurableAllowanceBoundDispatch(t *testing.T) {
	cfg := NewDefaultConfig()
	cfg.Enabled = true
	cfg.DailyCallLimit = 1
	started, release := make(chan struct{}), make(chan struct{})
	s := newStageWithServer(t, cfg, fakeInner{err: classification.ErrUnmatched}, fakePrivacy{}, func(w http.ResponseWriter, r *http.Request) { close(started); <-release; respondWith(w, "tv", .99) })
	done := make(chan struct{})
	go func() { defer close(done); _, _ = s.Run(context.Background(), "", nil, baseTorrent()) }()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("no dispatch")
	}
	tor := baseTorrent()
	tor.InfoHash[0]++
	_, err := s.Run(context.Background(), "", nil, tor)
	require.ErrorIs(t, err, classification.ErrUnmatched)
	close(release)
	<-done
	_, err = s.Run(context.Background(), "", nil, tor)
	require.ErrorIs(t, err, classification.ErrUnmatched)
	require.Equal(t, int32(1), s.admission.Budget.(*testBudget).used.Load())
}

func TestCancellationStillPersistsBoundedHTTPReceipt(t *testing.T) {
	cfg := NewDefaultConfig()
	cfg.Enabled = true
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s := newStageWithServer(t, cfg, fakeInner{err: classification.ErrUnmatched}, fakePrivacy{}, func(w http.ResponseWriter, r *http.Request) { cancel(); respondWith(w, "tv", .99) })
	_, err := s.Run(ctx, "", nil, baseTorrent())
	require.ErrorIs(t, err, classification.ErrUnmatched)
	a := s.admission.Capture.(*testAudit)
	require.Len(t, a.results, 1)
	require.NoError(t, a.resultContextErr)
	require.True(t, a.resultDeadline)
	require.Empty(t, a.decisions)
}

func TestNoRedirectOrAutomaticRetry(t *testing.T) {
	var destinationCalls atomic.Int32
	destination := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { destinationCalls.Add(1) }))
	defer destination.Close()
	cfg := NewDefaultConfig()
	cfg.Enabled = true
	s := newStageWithServer(t, cfg, fakeInner{err: classification.ErrUnmatched}, fakePrivacy{}, func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, destination.URL, http.StatusTemporaryRedirect)
	})
	_, err := s.Run(context.Background(), "", nil, baseTorrent())
	require.ErrorIs(t, err, classification.ErrUnmatched)
	require.Zero(t, destinationCalls.Load())
	require.Len(t, s.admission.Capture.(*testAudit).results, 1)
}

func TestResponseSchemaRejectsIncompleteOrAmbiguousAnswers(t *testing.T) {
	for _, answer := range []string{`{"category":"movie"}`, `{"category":"movie","confidence":null}`, `{"category":"movie","confidence":.8}`, `{"category":"tv","confidence":0.9,"extra":true}`, `{"category":"movie","confidence":0.9} {}`} {
		raw, _ := json.Marshal(map[string]any{"choices": []any{map[string]any{"finish_reason": "stop", "message": map[string]string{"content": answer}}}})
		_, err := parseResponse(raw)
		require.Error(t, err)
	}
	for _, finish := range []string{"", "length", "content_filter"} {
		raw, _ := json.Marshal(map[string]any{"choices": []any{map[string]any{"finish_reason": finish, "message": map[string]string{"content": `{"category":"movie","confidence":0.99}`}}}})
		_, err := parseResponse(raw)
		require.Error(t, err)
	}
}

func TestEnabledConfigurationIsBounded(t *testing.T) {
	cfg := NewDefaultConfig()
	cfg.Enabled = true
	cfg.APIKey = "test"
	require.NoError(t, cfg.Validate())
	for _, endpoint := range []string{"http://example.com/v1", "https://user:" + "pass@example.com/v1", "https://example.com/v1?key=secret"} {
		c := cfg
		c.Endpoint = endpoint
		require.Error(t, c.Validate())
	}
	for _, mutate := range []func(*Config){func(c *Config) { c.MaxConcurrentCalls = 0 }, func(c *Config) { c.MaxRequestBytes = 0 }, func(c *Config) { c.MaxOutputTokens = 0 }, func(c *Config) { c.MonthlyCallLimit = -1 }, func(c *Config) { c.MinConfidence = 0 }} {
		c := cfg
		mutate(&c)
		require.Error(t, c.Validate())
	}
	cfg.DailyCallLimit = 0
	require.NoError(t, cfg.Validate(), "zero means no admission, not unlimited")
}

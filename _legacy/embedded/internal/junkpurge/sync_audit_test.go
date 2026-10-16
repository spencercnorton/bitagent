package junkpurge

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/spencercnorton/bitagent/internal/llmcapture"
	"github.com/stretchr/testify/require"
)

type syncAuditFake struct {
	requests                                       [][]llmcapture.Request
	results                                        []llmcapture.HTTPResult
	decisions                                      [][]llmcapture.JunkDecision
	captureErr, recheckErr, resultErr, decisionErr error
}

// Fixture sources are public. The real PostgreSQL capture store still repeats
// native and evidence privacy admission at each write/outbound boundary.
type publicJunkAuditPrivacy struct{}

func (publicJunkAuditPrivacy) IsPrivateInfoHash(context.Context, []byte) (bool, error) {
	return false, nil
}

func (*syncAuditFake) Enabled() bool { return true }
func (*syncAuditFake) Capture(context.Context, llmcapture.Request) (llmcapture.Outcome, error) {
	panic("single-source capture must not admit grouped text")
}
func (f *syncAuditFake) CaptureJunkGroup(_ context.Context, requests []llmcapture.Request) error {
	f.requests = append(f.requests, requests)
	return f.captureErr
}
func (f *syncAuditFake) RecheckJunkGroup(context.Context, [][]byte) error { return f.recheckErr }
func (f *syncAuditFake) RecordJunkHTTPResult(ctx context.Context, keys [][]byte, r llmcapture.HTTPResult) ([]llmcapture.ResultReceipt, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	f.results = append(f.results, r)
	if f.resultErr != nil {
		return nil, f.resultErr
	}
	d := sha256.Sum256(r.Body)
	out := make([]llmcapture.ResultReceipt, len(keys))
	for i, k := range keys {
		out[i] = llmcapture.ResultReceipt{CaptureKey: k, ResponseSHA256: d[:], FirstObservation: true, StatusCode: r.StatusCode, ErrorClass: r.ErrorClass}
	}
	return out, nil
}
func (f *syncAuditFake) RecordJunkDecisions(ctx context.Context, _ []llmcapture.ResultReceipt, _ [][]byte, d []llmcapture.JunkDecision) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	f.decisions = append(f.decisions, d)
	return f.decisionErr
}

func auditedTestJudge(url string, a llmcapture.Capturer) *ollamaJudge {
	cfg := NewDefaultConfig()
	cfg.LLMApiStyle = "chat"
	cfg.LLMBaseURL = url
	cfg.LLMModel = "synthetic-model"
	cfg.LLMAllowPaidSync = true
	return NewAuditedJudge(cfg, NewMetrics(), &stubCallBudget{remaining: 20}, a).(*ollamaJudge)
}
func syntheticAuditSources() []candidate {
	return []candidate{{infoHash: []byte("0123456789abcdefghij"), name: "Synthetic Film One"}, {infoHash: []byte("abcdefghij0123456789"), name: "Synthetic Software Two"}}
}

func TestAuditedGroupedJudgeRetainsActualWireAndPositionalDecisions(t *testing.T) {
	var sent []byte
	reply := []byte(`{"choices":[{"message":{"content":"[{\"i\":2,\"verdict\":\"junk\",\"confidence\":0.99},{\"i\":1,\"verdict\":\"real_mangled\",\"confidence\":0.9}]"}}],"usage":{"prompt_tokens":10,"completion_tokens":5}}`)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { sent, _ = io.ReadAll(r.Body); _, _ = w.Write(reply) }))
	defer srv.Close()
	a := &syncAuditFake{}
	sources := syntheticAuditSources()
	js, err := auditedTestJudge(srv.URL, a).JudgeBatch(withJunkSources(context.Background(), sources), []string{sources[0].name, sources[1].name})
	require.NoError(t, err)
	require.Equal(t, "real_mangled", js[0].Verdict)
	require.Equal(t, "junk", js[1].Verdict)
	require.Len(t, a.requests, 1)
	require.Len(t, a.requests[0], 2)
	for i, r := range a.requests[0] {
		require.Equal(t, sent, []byte(r.ModelInputJSON))
		require.Equal(t, junkAuditedSyncContractID, r.ContractID)
		var input map[string]any
		require.NoError(t, json.Unmarshal(r.TaskInputJSON, &input))
		require.Equal(t, float64(i+1), input["request_index"])
		require.Equal(t, float64(2), input["request_size"])
	}
	require.Equal(t, reply, a.results[0].Body)
	replayed, err := EvaluationParseHTTPJudgments(a.results[0].Body, 2)
	require.NoError(t, err)
	require.Equal(t, js, replayed)
	require.Len(t, a.decisions, 1)
	require.False(t, a.decisions[0][0].WouldQuarantine)
	require.True(t, a.decisions[0][1].WouldQuarantine)
	require.False(t, a.decisions[0][1].Live)
}

func TestAuditedJudgePrivacyAndStorageFailuresBlockDispatchOrApplication(t *testing.T) {
	for _, kind := range []string{"missing", "capture", "privacy_recheck", "result", "decision"} {
		t.Run(kind, func(t *testing.T) {
			calls := 0
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				calls++
				_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"{\"verdict\":\"junk\",\"confidence\":0.99}"}}]}`))
			}))
			defer srv.Close()
			a := &syncAuditFake{}
			var capture llmcapture.Capturer = a
			switch kind {
			case "missing":
				capture = nil
			case "capture":
				a.captureErr = errors.New("storage failed")
			case "privacy_recheck":
				a.recheckErr = llmcapture.ErrPrivacyBlocked
			case "result":
				a.resultErr = errors.New("result failed")
			case "decision":
				a.decisionErr = errors.New("decision failed")
			}
			sources := syntheticAuditSources()[:1]
			jm, err := auditedTestJudge(srv.URL, capture).Judge(withJunkSources(context.Background(), sources), sources[0].name)
			require.ErrorIs(t, err, ErrLLMUnavailable)
			require.Zero(t, jm)
			if kind == "missing" || kind == "capture" || kind == "privacy_recheck" {
				require.Zero(t, calls)
			} else {
				require.Equal(t, 1, calls)
			}
		})
	}
}

func TestAuditedJudgeRetainsMalformedNon200AndBoundedResponses(t *testing.T) {
	for _, kind := range []string{"malformed", "case_variant", "non200", "oversize"} {
		t.Run(kind, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				switch kind {
				case "malformed":
					_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"{\"verdict\":\"junk\",\"confidence\":9}"}}]}`))
				case "case_variant":
					_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"{\"verdict\":\"real_mangled\",\"Verdict\":\"junk\",\"confidence\":0.99}"}}]}`))
				case "non200":
					w.WriteHeader(201)
					_, _ = w.Write([]byte("unexpected status"))
				case "oversize":
					_, _ = w.Write([]byte(strings.Repeat("x", llmcapture.MaxResultBodyBytes+100)))
				}
			}))
			defer srv.Close()
			a := &syncAuditFake{}
			sources := syntheticAuditSources()[:1]
			_, err := auditedTestJudge(srv.URL, a).Judge(withJunkSources(context.Background(), sources), sources[0].name)
			require.Error(t, err)
			require.Len(t, a.results, 1)
			require.LessOrEqual(t, len(a.results[0].Body), llmcapture.MaxResultBodyBytes)
			require.NotEmpty(t, a.decisions)
			require.Equal(t, "invalid_response", a.decisions[0][0].Outcome)
			require.False(t, a.decisions[0][0].WouldQuarantine)
		})
	}
}

func invalidJunkHTTPEnvelopes() map[string][]byte {
	answer := `{"verdict":"junk","confidence":0.99}`
	choice := func(role, finish, refusal string) map[string]any {
		return map[string]any{"finish_reason": finish, "message": map[string]string{
			"role": role, "content": answer, "refusal": refusal,
		}}
	}
	cases := map[string]map[string]any{
		"provider_error": {"error": map[string]string{"message": "synthetic error"}, "choices": []any{choice("assistant", "stop", "")}},
		"refusal":        {"choices": []any{choice("assistant", "stop", "synthetic refusal")}},
		"non_assistant":  {"choices": []any{choice("user", "stop", "")}},
		"incomplete":     {"choices": []any{choice("assistant", "length", "")}},
		"filtered":       {"choices": []any{choice("assistant", "content_filter", "")}},
		"multiple":       {"choices": []any{choice("assistant", "stop", ""), choice("assistant", "stop", "")}},
		"empty":          {"choices": []any{}},
	}
	raw := make(map[string][]byte, len(cases))
	for name, body := range cases {
		raw[name], _ = json.Marshal(body)
	}
	return raw
}

func TestAuditedJudgeRejectsExplicitlyUnqualifiedHTTPEnvelopes(t *testing.T) {
	for name, raw := range invalidJunkHTTPEnvelopes() {
		t.Run(name, func(t *testing.T) {
			_, err := EvaluationParseHTTPJudgments(raw, 1)
			require.Error(t, err)
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write(raw) }))
			defer srv.Close()
			a := &syncAuditFake{}
			sources := syntheticAuditSources()[:1]
			jm, err := auditedTestJudge(srv.URL, a).Judge(withJunkSources(context.Background(), sources), sources[0].name)
			require.Error(t, err)
			require.Zero(t, jm)
			require.Len(t, a.results, 1)
			require.Equal(t, raw, a.results[0].Body)
			require.Len(t, a.decisions, 1)
			require.Equal(t, "invalid_response", a.decisions[0][0].Outcome)
			require.False(t, a.decisions[0][0].WouldQuarantine)
		})
	}
	// Old compatible endpoints may omit optional completion metadata. An
	// explicit stop/assistant reply is also admitted; null error is no error.
	for _, raw := range []string{
		`{"choices":[{"message":{"content":"{\"verdict\":\"junk\",\"confidence\":0.99}"}}]}`,
		`{"error":null,"choices":[{"finish_reason":"stop","message":{"role":"assistant","content":"{\"verdict\":\"junk\",\"confidence\":0.99}"}}]}`,
	} {
		js, err := EvaluationParseHTTPJudgments([]byte(raw), 1)
		require.NoError(t, err)
		require.Equal(t, []Judgment{{Verdict: verdictJunk, Confidence: .99}}, js)
	}
}

type junkAuditRoundTripper func(*http.Request) (*http.Response, error)

func (f junkAuditRoundTripper) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

type junkAuditReadError struct{}

func (junkAuditReadError) Read(b []byte) (int, error) {
	return copy(b, []byte("partial response")), io.ErrUnexpectedEOF
}
func (junkAuditReadError) Close() error { return nil }

func TestAuditedJudgeRetainsTransportAndReadFailuresAfterCancellation(t *testing.T) {
	for _, kind := range []string{"transport", "read", "canceled_after_response"} {
		t.Run(kind, func(t *testing.T) {
			a := &syncAuditFake{}
			j := auditedTestJudge("http://synthetic.invalid", a)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			j.http.Transport = junkAuditRoundTripper(func(_ *http.Request) (*http.Response, error) {
				cancel()
				if kind == "transport" {
					return nil, context.Canceled
				}
				var body io.ReadCloser = io.NopCloser(strings.NewReader(`{"choices":[{"message":{"content":"{\"verdict\":\"real_mangled\",\"confidence\":0.99}"}}]}`))
				if kind == "read" {
					body = junkAuditReadError{}
				}
				return &http.Response{StatusCode: 200, Body: body}, nil
			})
			sources := syntheticAuditSources()[:1]
			_, err := j.Judge(withJunkSources(ctx, sources), sources[0].name)
			require.Len(t, a.results, 1)
			require.NotEmpty(t, a.decisions)
			if kind == "canceled_after_response" {
				require.NoError(t, err)
				require.Equal(t, "judged", a.decisions[0][0].Outcome)
				require.Equal(t, "none", a.results[0].ErrorClass)
			} else {
				require.ErrorIs(t, err, ErrLLMUnavailable)
				require.Equal(t, "invalid_response", a.decisions[0][0].Outcome)
				require.Equal(t, kind, a.results[0].ErrorClass)
			}
		})
	}
}

func TestAuditedGroupedFallbackUsesEachExactSource(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var b struct{ Messages []struct{ Content string } }
		require.NoError(t, json.NewDecoder(r.Body).Decode(&b))
		content := `{"verdict":"real_mangled","confidence":0.9}`
		if strings.HasPrefix(b.Messages[1].Content, "1. ") {
			content = `[{"i":1,"verdict":"junk","confidence":2}]`
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{"message": map[string]string{"content": content}}}})
	}))
	defer srv.Close()
	a := &syncAuditFake{}
	sources := syntheticAuditSources()
	ctx := withJunkSources(context.Background(), sources)
	js, errs, n, unavailable := judgeGroupWithFallback(ctx, auditedTestJudge(srv.URL, a), []string{sources[0].name, sources[1].name}, NewMetrics())
	require.NoError(t, unavailable)
	require.Equal(t, 2, n)
	require.Equal(t, []error{nil, nil}, errs)
	require.Len(t, js, 2)
	require.Len(t, a.requests, 3)
	require.Equal(t, sources[0].infoHash, a.requests[1][0].InfoHash)
	require.Equal(t, sources[1].infoHash, a.requests[2][0].InfoHash)
	require.Equal(t, "invalid_response", a.decisions[0][0].Outcome)
}

func TestJunkDecisionRejectsDuplicateFields(t *testing.T) {
	for _, content := range []string{
		`{"verdict":"real_mangled","verdict":"junk","confidence":0.99}`,
		`{"verdict":"junk","confidence":0.1,"confidence":0.99}`,
		`{"verdict":"real_mangled","Verdict":"junk","confidence":0.99}`,
		`{"verdict":"junk","confidence":0.1,"Confidence":0.99}`,
		`{"verdict":"real_mangled","Verdi\u0063t":"junk","confidence":0.99}`,
	} {
		_, err := parseJudgment(content)
		require.Error(t, err)
	}
	for _, content := range []string{
		`[{"i":1,"i":2,"verdict":"junk","confidence":0.99},{"i":1,"verdict":"real_mangled","confidence":0.9}]`,
		`[{"i":1,"I":2,"verdict":"junk","confidence":0.99},{"i":1,"verdict":"real_mangled","confidence":0.9}]`,
		`[{"i":1,"verdict":"real_mangled","Verdict":"junk","confidence":0.99},{"i":2,"verdict":"real_mangled","confidence":0.9}]`,
	} {
		_, err := parseBatchJudgments(content, 2)
		require.Error(t, err)
	}
	for _, raw := range []string{
		`{"choices":[],"choices":[{"message":{"content":"{\"verdict\":\"junk\",\"confidence\":0.99}"}}]}`,
		`{"choices":[],"Choices":[{"message":{"content":"{\"verdict\":\"junk\",\"confidence\":0.99}"}}]}`,
		`{"choices":[],"choiceſ":[{"message":{"content":"{\"verdict\":\"junk\",\"confidence\":0.99}"}}]}`,
		`{"choices":[{"message":{"content":"{\"verdict\":\"real_mangled\",\"confidence\":0.99}"}}],"choice\u017f":[{"message":{"content":"{\"verdict\":\"junk\",\"confidence\":0.99}"}}]}`,
		`{"choices":[{"message":{"content":"{\"verdict\":\"real_mangled\",\"confidence\":0.99}"},"meſſage":{"content":"{\"verdict\":\"junk\",\"confidence\":0.99}"}}]}`,
		`{"choices":[{"finish_reason":"length","Finish_Reason":"stop","message":{"content":"{\"verdict\":\"junk\",\"confidence\":0.99}"}}]}`,
		`{"choices":[{"message":{"content":"{\"verdict\":\"real_mangled\",\"confidence\":0.99}","Content":"{\"verdict\":\"junk\",\"confidence\":0.99}"}}]}`,
		`{"choices":[{"message":{"content":"{\"verdict\":\"real_mangled\",\"Verdict\":\"junk\",\"confidence\":0.99}"}}]}`,
	} {
		_, err := EvaluationParseHTTPJudgments([]byte(raw), 1)
		require.Error(t, err)
	}
}

func TestEvaluationParseHTTPJudgmentsUsesProductionParser(t *testing.T) {
	for _, size := range []int{0, 51} {
		_, err := EvaluationParseHTTPJudgments(nil, size)
		require.Error(t, err)
	}
	raw := []byte(`{"choices":[{"message":{"content":"{\"verdict\":\"junk\",\"confidence\":0.99}"}}]}`)
	js, err := EvaluationParseHTTPJudgments(raw, 1)
	require.NoError(t, err)
	require.Equal(t, []Judgment{{Verdict: verdictJunk, Confidence: .99}}, js)
	_, err = EvaluationParseHTTPJudgments(raw, 2)
	require.Error(t, err, "a single decision cannot stand in for a grouped response")
	_, err = EvaluationParseHTTPJudgments([]byte(`{"choices":[]}`), 1)
	require.Error(t, err)
}

type malformedCycleJudge struct{}

func (malformedCycleJudge) Judge(_ context.Context, name string) (Judgment, error) {
	if strings.Contains(name, "invalid") {
		return Judgment{}, errors.New("synthetic malformed response")
	}
	if strings.Contains(name, "junk") {
		return Judgment{Verdict: verdictJunk, Confidence: .99}, nil
	}
	return Judgment{Verdict: verdictRealMangled, Confidence: .99}, nil
}
func (malformedCycleJudge) JudgeBatch(context.Context, []string) ([]Judgment, error) {
	return nil, errors.New("synthetic malformed grouped response")
}

package junkpurge

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/spencercnorton/bitagent/internal/telemetry/dualemit"
)

// newChatCaptureServer speaks OpenAI /chat/completions, records the decoded
// request body, and replies with the given content string.
func newChatCaptureServer(t *testing.T, status int, content string, got *map[string]any) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			t.Errorf("unexpected path %q (want /v1/chat/completions)", r.URL.Path)
		}
		body, _ := io.ReadAll(r.Body)
		if got != nil {
			var m map[string]any
			if err := json.Unmarshal(body, &m); err != nil {
				t.Errorf("request body not JSON: %v", err)
			}
			*got = m
		}
		w.WriteHeader(status)
		if status/100 == 2 {
			reply := map[string]any{
				"choices": []map[string]any{{"message": map[string]any{"content": content}}},
				"usage":   map[string]any{"prompt_tokens": 100, "completion_tokens": 40},
			}
			_ = json.NewEncoder(w).Encode(reply)
		} else {
			_, _ = w.Write([]byte("upstream error"))
		}
	}))
}

func chatTestJudge(url string) Judge {
	return NewJudge(
		Config{LLMBaseURL: url + "/v1", LLMModel: "m", LLMApiStyle: "chat", LLMTimeout: "5s"},
		NewMetrics(),
	)
}

type stubCallBudget struct {
	calls     int
	remaining int
	err       error
}

func (b *stubCallBudget) Reserve(context.Context, int, int) (bool, error) {
	b.calls++
	if b.err != nil {
		return false, b.err
	}
	if b.remaining <= 0 {
		return false, nil
	}
	b.remaining--
	return true, nil
}

func TestDataSharingJudgeFailsClosedOnDurableBudget(t *testing.T) {
	providerCalls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		providerCalls++
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{{"message": map[string]any{
				"content": `{"verdict":"unsure","confidence":0.4}`,
			}}},
			"usage": map[string]any{"prompt_tokens": 10, "completion_tokens": 5},
		})
	}))
	defer srv.Close()
	cfg := Config{
		LLMBaseURL:           srv.URL + "/v1",
		LLMModel:             "gpt-5.6-sol",
		LLMApiStyle:          "chat",
		LLMTimeout:           "5s",
		LLMOpenaiDataSharing: true,
		LLMDailyCallLimit:    1,
		LLMMonthlyCallLimit:  1,
	}

	_, err := NewJudgeWithBudget(cfg, NewMetrics(), nil).Judge(
		context.Background(), "No.Budget",
	)
	if llmFailureReason(err) != llmFailureBudgetUnavailable || providerCalls != 0 {
		t.Fatalf("missing budget: reason=%q provider_calls=%d err=%v", llmFailureReason(err), providerCalls, err)
	}

	budget := &stubCallBudget{remaining: 1}
	judge := NewJudgeWithBudget(cfg, NewMetrics(), budget)
	if _, err = judge.Judge(context.Background(), "First.Call"); err != nil {
		t.Fatalf("first budgeted call: %v", err)
	}
	_, err = judge.Judge(context.Background(), "Blocked.Call")
	if llmFailureReason(err) != llmFailureBudgetExhausted {
		t.Fatalf("exhausted budget reason=%q err=%v", llmFailureReason(err), err)
	}
	if providerCalls != 1 || budget.calls != 2 {
		t.Fatalf("provider_calls=%d budget_calls=%d, want 1/2", providerCalls, budget.calls)
	}
}

func messagesOf(t *testing.T, req map[string]any) (system, user string) {
	t.Helper()
	msgs, ok := req["messages"].([]any)
	if !ok || len(msgs) != 2 {
		t.Fatalf("want 2 messages, got %#v", req["messages"])
	}
	sys := msgs[0].(map[string]any)
	usr := msgs[1].(map[string]any)
	if sys["role"] != "system" || usr["role"] != "user" {
		t.Fatalf("unexpected roles: %v / %v", sys["role"], usr["role"])
	}
	return sys["content"].(string), usr["content"].(string)
}

// TestJudgeBatchChatParsesGroupedVerdicts proves the whole grouped
// round-trip: one request, numbered user list, policy prompt embedded
// verbatim, out-of-order reply indices mapped back to input positions.
func TestJudgeBatchChatParsesGroupedVerdicts(t *testing.T) {
	var req map[string]any
	// Reply deliberately out of order: index mapping, not reply order,
	// must determine alignment.
	srv := newChatCaptureServer(t, 200, `[
		{"i":3,"verdict":"junk","confidence":0.95},
		{"i":1,"verdict":"real_mangled","confidence":0.8},
		{"i":2,"verdict":"unsure","confidence":0.5}
	]`, &req)
	defer srv.Close()

	names := []string{"Movie.A.2020.1080p", "Movie.B.2021.720p", "Adult.Site.Clip"}
	js, err := chatTestJudge(srv.URL).JudgeBatch(context.Background(), names)
	if err != nil {
		t.Fatalf("JudgeBatch: %v", err)
	}
	if len(js) != 3 {
		t.Fatalf("want 3 judgments, got %d", len(js))
	}
	if js[0].Verdict != verdictRealMangled || js[1].Verdict != verdictUnsure || js[2].Verdict != verdictJunk {
		t.Fatalf("index mapping wrong: %+v", js)
	}
	if js[2].Confidence != 0.95 {
		t.Fatalf("confidence not carried: %+v", js[2])
	}

	system, user := messagesOf(t, req)
	if !strings.Contains(system, judgeInstructions) {
		t.Fatal("grouped system prompt does not embed judgeInstructions verbatim")
	}
	wantUser := "1. Movie.A.2020.1080p\n2. Movie.B.2021.720p\n3. Adult.Site.Clip"
	if user != wantUser {
		t.Fatalf("user content = %q, want %q", user, wantUser)
	}
	if got := req["max_completion_tokens"].(float64); int(got) != batchMaxTokens(3) {
		t.Fatalf("max_completion_tokens = %v, want %d", got, batchMaxTokens(3))
	}
}

// TestJudgeBatchSingleDelegatesToClassicShape pins the compatibility
// contract: a group of one produces the exact classic single-name request
// (no batch format instructions, classic completion cap), so
// llm_names_per_call=1 deployments are byte-identical to prior releases.
func TestJudgeBatchSingleDelegatesToClassicShape(t *testing.T) {
	var req map[string]any
	srv := newChatCaptureServer(t, 200, `{"verdict":"junk","confidence":0.9}`, &req)
	defer srv.Close()

	js, err := chatTestJudge(srv.URL).JudgeBatch(context.Background(), []string{"Some.Name"})
	if err != nil || len(js) != 1 || js[0].Verdict != verdictJunk {
		t.Fatalf("single delegate: js=%+v err=%v", js, err)
	}
	system, user := messagesOf(t, req)
	if system != judgeInstructions+"\n/no_think" {
		t.Fatalf("single-name system prompt diverged from classic shape: %q", system)
	}
	if user != "Some.Name" {
		t.Fatalf("single-name user content = %q", user)
	}
	if got := req["max_completion_tokens"].(float64); int(got) != judgeMaxTokens {
		t.Fatalf("max_completion_tokens = %v, want %d", got, judgeMaxTokens)
	}
}

func TestJudgeBatchOllamaStyle(t *testing.T) {
	srv := newOllamaServer(t, 200, `[{"i":1,"verdict":"junk","confidence":0.9},{"i":2,"verdict":"real_mangled","confidence":0.7}]`)
	defer srv.Close()
	js, err := testJudge(srv.URL).JudgeBatch(context.Background(), []string{"a", "b"})
	if err != nil {
		t.Fatalf("JudgeBatch ollama: %v", err)
	}
	if js[0].Verdict != verdictJunk || js[1].Verdict != verdictRealMangled {
		t.Fatalf("judgments: %+v", js)
	}
}

func TestJudgeBatchUnavailableOn5xx(t *testing.T) {
	srv := newChatCaptureServer(t, 503, "", nil)
	defer srv.Close()
	metrics := NewMetrics()
	judge := NewJudge(
		Config{
			LLMBaseURL:  srv.URL + "/v1",
			LLMModel:    "m",
			LLMApiStyle: "chat",
			LLMTimeout:  "5s",
		},
		metrics,
	)
	_, err := judge.JudgeBatch(context.Background(), []string{"a", "b"})
	if !errors.Is(err, ErrLLMUnavailable) {
		t.Fatalf("want ErrLLMUnavailable, got %v", err)
	}
	if got := llmFailureReason(err); got != llmFailureServerError {
		t.Fatalf("failure reason = %q, want %q", got, llmFailureServerError)
	}

	wasLegacy := dualemit.EmitLegacy
	dualemit.EmitLegacy = false
	t.Cleanup(func() { dualemit.EmitLegacy = wasLegacy })
	if got := testutil.ToFloat64(metrics.llmFailures.WithLabelValues(
		"grouped", "m", llmFailureServerError,
	)); got != 1 {
		t.Fatalf("server-error metric = %v, want 1", got)
	}
}

// TestParseBatchJudgmentsRejectsDefects: ANY defect fails the whole group —
// a partially-usable grouped reply must never be half-acted on.
func TestParseBatchJudgmentsRejectsDefects(t *testing.T) {
	cases := []struct {
		name string
		text string
		n    int
	}{
		{"wrong count", `[{"i":1,"verdict":"junk","confidence":0.9}]`, 2},
		{"duplicate index", `[{"i":1,"verdict":"junk","confidence":0.9},{"i":1,"verdict":"unsure","confidence":0.4}]`, 2},
		{"index out of range", `[{"i":1,"verdict":"junk","confidence":0.9},{"i":3,"verdict":"unsure","confidence":0.4}]`, 2},
		{"missing index", `[{"verdict":"junk","confidence":0.9},{"i":2,"verdict":"unsure","confidence":0.4}]`, 2},
		{"one invalid verdict", `[{"i":1,"verdict":"junky","confidence":0.9},{"i":2,"verdict":"unsure","confidence":0.4}]`, 2},
		{"one confidence out of range", `[{"i":1,"verdict":"junk","confidence":1.2},{"i":2,"verdict":"unsure","confidence":0.4}]`, 2},
		{"unknown field", `[{"i":1,"verdict":"junk","confidence":0.9,"why":"x"},{"i":2,"verdict":"unsure","confidence":0.4}]`, 2},
		{"trailing JSON", `[{"i":1,"verdict":"junk","confidence":0.9}] {"more":true}`, 1},
		{"not an array", `{"i":1,"verdict":"junk","confidence":0.9}`, 1},
		{"empty", ``, 1},
	}
	for _, tc := range cases {
		if _, err := parseBatchJudgments(tc.text, tc.n); err == nil {
			t.Errorf("%s: want error, got nil", tc.name)
		}
	}
}

func TestParseBatchJudgmentsStripsWrapping(t *testing.T) {
	text := "<think>positional stuff</think>\n```json\n[{\"i\":1,\"verdict\":\"junk\",\"confidence\":0.9}]\n```"
	js, err := parseBatchJudgments(text, 1)
	if err != nil || js[0].Verdict != verdictJunk {
		t.Fatalf("wrapped reply: js=%+v err=%v", js, err)
	}
}

func TestBatchUserContentFlattensNewlines(t *testing.T) {
	got := batchUserContent([]string{"a\nb", "c\rd"})
	if got != "1. a b\n2. c d" {
		t.Fatalf("batchUserContent = %q", got)
	}
}

// scriptedJudge drives judgeGroupWithFallback: JudgeBatch returns batchErr
// (or judgments), Judge returns per-name results.
type scriptedJudge struct {
	batchJS    []Judgment
	batchErr   error
	batchCalls int
	singles    map[string]Judgment
	singleErr  map[string]error
	callOrder  []string
}

type batchStep struct {
	judgments []Judgment
	err       error
	before    func()
}

type sequencedJudge struct {
	steps []batchStep
	calls int
}

type midFallbackCancelJudge struct {
	cancel      context.CancelFunc
	judgment    Judgment
	batchCalls  int
	singleCalls int
}

func (j *midFallbackCancelJudge) JudgeBatch(
	context.Context,
	[]string,
) ([]Judgment, error) {
	j.batchCalls++
	return nil, errors.New("grouped response is unusable")
}

func (j *midFallbackCancelJudge) Judge(
	context.Context,
	string,
) (Judgment, error) {
	j.singleCalls++
	if j.singleCalls != 1 {
		return Judgment{}, errors.New("single fallback continued after cancellation")
	}
	j.cancel()
	return j.judgment, nil
}

func (s *sequencedJudge) Judge(context.Context, string) (Judgment, error) {
	return Judgment{}, errors.New("unexpected single-name fallback")
}

func (s *sequencedJudge) JudgeBatch(_ context.Context, _ []string) ([]Judgment, error) {
	if s.calls >= len(s.steps) {
		return nil, errors.New("unexpected grouped call")
	}
	step := s.steps[s.calls]
	s.calls++
	if step.before != nil {
		step.before()
	}
	return step.judgments, step.err
}

func (s *scriptedJudge) Judge(_ context.Context, name string) (Judgment, error) {
	s.callOrder = append(s.callOrder, name)
	if err := s.singleErr[name]; err != nil {
		return Judgment{}, err
	}
	return s.singles[name], nil
}

func (s *scriptedJudge) JudgeBatch(_ context.Context, names []string) ([]Judgment, error) {
	s.batchCalls++
	if s.batchErr != nil {
		return nil, s.batchErr
	}
	return s.batchJS, nil
}

func TestJudgeGroupWithFallback(t *testing.T) {
	ctx := context.Background()
	names := []string{"a", "b"}

	t.Run("grouped success uses no singles", func(t *testing.T) {
		s := &scriptedJudge{batchJS: []Judgment{
			{Verdict: verdictJunk, Confidence: 0.9},
			{Verdict: verdictRealMangled, Confidence: 0.8},
		}}
		js, errs, processed, unavailableErr := judgeGroupWithFallback(ctx, s, names, nil)
		if unavailableErr != nil || processed != 2 || errs[0] != nil || errs[1] != nil {
			t.Fatalf("unavailableErr=%v processed=%d errs=%v", unavailableErr, processed, errs)
		}
		if js[0].Verdict != verdictJunk || js[1].Verdict != verdictRealMangled {
			t.Fatalf("judgments: %+v", js)
		}
		if len(s.callOrder) != 0 {
			t.Fatalf("singles called on grouped success: %v", s.callOrder)
		}
	})

	t.Run("unusable grouped reply falls back to singles", func(t *testing.T) {
		s := &scriptedJudge{
			batchErr: fmt.Errorf("junkpurge llm: grouped verdict: got 1 items, want 2"),
			singles: map[string]Judgment{
				"a": {Verdict: verdictJunk, Confidence: 0.9},
				"b": {Verdict: verdictUnsure, Confidence: 0.4},
			},
		}
		js, errs, processed, unavailableErr := judgeGroupWithFallback(ctx, s, names, NewMetrics())
		if unavailableErr != nil || processed != 2 {
			t.Fatalf("unavailableErr=%v processed=%d on a per-group defect", unavailableErr, processed)
		}
		if errs[0] != nil || errs[1] != nil {
			t.Fatalf("errs: %v", errs)
		}
		if js[0].Verdict != verdictJunk || js[1].Verdict != verdictUnsure {
			t.Fatalf("fallback judgments: %+v", js)
		}
		if s.batchCalls != 1 || len(s.callOrder) != 2 {
			t.Fatalf("batchCalls=%d singles=%v", s.batchCalls, s.callOrder)
		}
	})

	t.Run("grouped unavailability trips the breaker without singles", func(t *testing.T) {
		s := &scriptedJudge{batchErr: fmt.Errorf("%w: http 503", ErrLLMUnavailable)}
		_, _, processed, unavailableErr := judgeGroupWithFallback(ctx, s, names, nil)
		if !errors.Is(unavailableErr, ErrLLMUnavailable) || processed != 0 {
			t.Fatalf("want unavailable with processed=0, got err=%v processed=%d", unavailableErr, processed)
		}
		if len(s.callOrder) != 0 {
			t.Fatalf("singles called after unavailability: %v", s.callOrder)
		}
	})

	t.Run("single-path unavailability trips the breaker", func(t *testing.T) {
		s := &scriptedJudge{
			batchErr:  fmt.Errorf("junkpurge llm: grouped verdict: got 0 items, want 2"),
			singleErr: map[string]error{"a": fmt.Errorf("%w: timeout", ErrLLMUnavailable)},
		}
		_, _, processed, unavailableErr := judgeGroupWithFallback(ctx, s, names, NewMetrics())
		if !errors.Is(unavailableErr, ErrLLMUnavailable) || processed != 0 {
			t.Fatalf("want unavailable at the first single, got err=%v processed=%d", unavailableErr, processed)
		}
	})

	t.Run("per-item defect in fallback stays per-item", func(t *testing.T) {
		s := &scriptedJudge{
			batchErr: fmt.Errorf("junkpurge llm: grouped verdict: duplicate index 1"),
			singles: map[string]Judgment{
				"b": {Verdict: verdictRealMangled, Confidence: 0.7},
			},
			singleErr: map[string]error{
				"a": fmt.Errorf("junkpurge llm: verdict: unsupported verdict %q", "junky"),
			},
		}
		js, errs, processed, unavailableErr := judgeGroupWithFallback(ctx, s, names, NewMetrics())
		if unavailableErr != nil || processed != 2 {
			t.Fatalf("unavailableErr=%v processed=%d on a per-item defect", unavailableErr, processed)
		}
		if errs[0] == nil || errs[1] != nil {
			t.Fatalf("errs: %v", errs)
		}
		if js[1].Verdict != verdictRealMangled {
			t.Fatalf("surviving judgment: %+v", js[1])
		}
	})

	t.Run("mid-fallback unavailability preserves completed singles", func(t *testing.T) {
		// The review finding: singles that already succeeded before a
		// later call hit an outage must be RETURNED for recording, not
		// discarded and re-bought after the breaker window.
		s := &scriptedJudge{
			batchErr: fmt.Errorf("junkpurge llm: grouped verdict: got 1 items, want 2"),
			singles: map[string]Judgment{
				"a": {Verdict: verdictJunk, Confidence: 0.92},
			},
			singleErr: map[string]error{
				"b": fmt.Errorf("%w: http 503", ErrLLMUnavailable),
			},
		}
		js, errs, processed, unavailableErr := judgeGroupWithFallback(ctx, s, names, NewMetrics())
		if !errors.Is(unavailableErr, ErrLLMUnavailable) {
			t.Fatalf("want ErrLLMUnavailable, got %v", unavailableErr)
		}
		if processed != 1 {
			t.Fatalf("processed = %d, want 1 (the completed single)", processed)
		}
		if errs[0] != nil || js[0].Verdict != verdictJunk || js[0].Confidence != 0.92 {
			t.Fatalf("completed single not preserved: js[0]=%+v errs[0]=%v", js[0], errs[0])
		}
	})

	t.Run("a single-name group never calls JudgeBatch", func(t *testing.T) {
		s := &scriptedJudge{
			singles: map[string]Judgment{"a": {Verdict: verdictJunk, Confidence: 0.9}},
		}
		js, errs, processed, unavailableErr := judgeGroupWithFallback(ctx, s, []string{"a"}, nil)
		if unavailableErr != nil || processed != 1 || errs[0] != nil || js[0].Verdict != verdictJunk {
			t.Fatalf("js=%+v errs=%v processed=%d unavailableErr=%v", js, errs, processed, unavailableErr)
		}
		if s.batchCalls != 0 {
			t.Fatalf("JudgeBatch called for a single-name group")
		}
	})
}

func TestEvaluateCandidateGroupsIsolatesUnavailableCalls(t *testing.T) {
	items := make([]candidate, 8)
	for i := range items {
		items[i].name = fmt.Sprintf("item-%d", i)
	}
	valid := func(verdict string) []Judgment {
		return []Judgment{
			{Verdict: verdict, Confidence: 0.9},
			{Verdict: verdict, Confidence: 0.9},
		}
	}
	judge := &sequencedJudge{steps: []batchStep{
		{err: fmt.Errorf("%w: isolated 503", ErrLLMUnavailable)},
		{judgments: valid(verdictRealMangled)},
		{err: fmt.Errorf("%w: isolated timeout", ErrLLMUnavailable)},
		{judgments: valid(verdictJunk)},
	}}
	var consumed []string
	summary := evaluateCandidateGroups(
		context.Background(),
		judge,
		items,
		2,
		2,
		nil,
		func(_ context.Context, c candidate, _ Judgment, err error) {
			if err != nil {
				t.Fatalf("unexpected item error: %v", err)
			}
			consumed = append(consumed, c.name)
		},
	)
	if !summary.hadUnavailable || summary.halted {
		t.Fatalf("summary = %+v, want isolated failures without early halt", summary)
	}
	if summary.deferred != 4 || len(summary.unavailableGroups) != 2 {
		t.Fatalf("summary = %+v, want four deferred across two groups", summary)
	}
	if judge.calls != 4 {
		t.Fatalf("group calls = %d, want all 4 groups attempted", judge.calls)
	}
	want := []string{"item-2", "item-3", "item-6", "item-7"}
	if fmt.Sprint(consumed) != fmt.Sprint(want) {
		t.Fatalf("consumed = %v, want later successful groups %v", consumed, want)
	}
}

func TestEvaluateCandidateGroupsBoundsRealOutage(t *testing.T) {
	items := make([]candidate, 10)
	for i := range items {
		items[i].name = fmt.Sprintf("item-%d", i)
	}
	judge := &sequencedJudge{steps: []batchStep{
		{err: fmt.Errorf("%w: 503", ErrLLMUnavailable)},
		{err: fmt.Errorf("%w: 503", ErrLLMUnavailable)},
		{err: errors.New("third call must not happen")},
	}}
	consumed := 0
	summary := evaluateCandidateGroups(
		context.Background(), judge, items, 2, 2, nil,
		func(context.Context, candidate, Judgment, error) { consumed++ },
	)
	if !summary.hadUnavailable || !summary.halted {
		t.Fatalf("summary = %+v, want consecutive-outage halt", summary)
	}
	if summary.deferred != len(items) {
		t.Fatalf("deferred = %d, want %d", summary.deferred, len(items))
	}
	if judge.calls != 2 || consumed != 0 {
		t.Fatalf("calls=%d consumed=%d, want two bounded probes and no results", judge.calls, consumed)
	}
}

func TestEvaluateCandidateGroupsCancellationKeepsCompletedPrefix(t *testing.T) {
	items := make([]candidate, 4)
	ctx, cancel := context.WithCancel(context.Background())
	judge := &sequencedJudge{steps: []batchStep{
		{judgments: []Judgment{
			{Verdict: verdictRealMangled, Confidence: 0.9},
			{Verdict: verdictRealMangled, Confidence: 0.9},
		}},
		{before: cancel, err: context.Canceled},
	}}
	consumed := 0
	summary := evaluateCandidateGroups(
		ctx, judge, items, 2, 3, nil,
		func(context.Context, candidate, Judgment, error) { consumed++ },
	)
	if !errors.Is(summary.contextErr, context.Canceled) {
		t.Fatalf("context error = %v, want canceled", summary.contextErr)
	}
	if consumed != 2 || judge.calls != 2 {
		t.Fatalf("consumed=%d calls=%d, want completed prefix=2 and calls=2", consumed, judge.calls)
	}
}

func TestEvaluateCandidateGroupsCancellationKeepsMidFallbackPrefix(t *testing.T) {
	items := []candidate{{name: "first"}, {name: "second"}}
	ctx, cancel := context.WithCancel(context.Background())
	judge := &midFallbackCancelJudge{
		cancel: cancel,
		judgment: Judgment{
			Verdict: verdictRealMangled, Confidence: 0.9,
		},
	}
	consumed := 0
	consumeContextLive := false
	consumeContextBounded := false
	summary := evaluateCandidateGroups(
		ctx, judge, items, 2, 3, nil,
		func(recordCtx context.Context, _ candidate, judgment Judgment, itemErr error) {
			consumed++
			consumeContextLive = recordCtx.Err() == nil
			_, consumeContextBounded = recordCtx.Deadline()
			if itemErr != nil || judgment.Verdict != verdictRealMangled {
				t.Fatalf("completed fallback result = %+v, err=%v", judgment, itemErr)
			}
		},
	)
	if !errors.Is(summary.contextErr, context.Canceled) {
		t.Fatalf("context error = %v, want canceled", summary.contextErr)
	}
	if consumed != 1 || judge.batchCalls != 1 || judge.singleCalls != 1 {
		t.Fatalf(
			"consumed=%d batchCalls=%d singleCalls=%d, want 1/1/1",
			consumed, judge.batchCalls, judge.singleCalls,
		)
	}
	if !consumeContextLive || !consumeContextBounded {
		t.Fatalf(
			"recording context live=%v bounded=%v, want true/true",
			consumeContextLive, consumeContextBounded,
		)
	}
}

func TestCycleOutcomeMetricsKeepBreakerEvidenceVisible(t *testing.T) {
	wasLegacy := dualemit.EmitLegacy
	dualemit.EmitLegacy = false
	t.Cleanup(func() { dualemit.EmitLegacy = wasLegacy })

	metrics := NewMetrics()
	metrics.observeCycleOutcome(cycleOutcomeJunkRateAnomaly, 500, 289)
	if got := testutil.ToFloat64(metrics.cycleOutcomes.WithLabelValues(
		cycleOutcomeJunkRateAnomaly,
	)); got != 1 {
		t.Fatalf("cycle outcome = %v, want 1", got)
	}
	if got := testutil.ToFloat64(metrics.cycleJudgments.WithLabelValues(
		cycleOutcomeJunkRateAnomaly,
	)); got != 500 {
		t.Fatalf("breaker judgments = %v, want 500", got)
	}
	if got := testutil.ToFloat64(metrics.cycleJunk.WithLabelValues(
		cycleOutcomeJunkRateAnomaly,
	)); got != 289 {
		t.Fatalf("breaker confident junk = %v, want 289", got)
	}
}

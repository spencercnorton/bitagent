package contentfilter

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// deferConfig is a phase-2 config in ENFORCE mode (defer is an
// enforce-mode behaviour) with an unlimited budget (local/free LLM).
func deferConfig(deferOn bool) Config {
	cfg := phase2Config()
	cfg.Enforce = true
	cfg.LLMDeferOnUnavailable = deferOn
	cfg.LLMDailyBudget = -1 // unlimited
	return cfg
}

// --- Defer-on-unavailable (Phase 3) ---

func TestDecide_DefersWhenLLMUnavailableAndOptedIn(t *testing.T) {
	llm := &fakeLLM{err: ErrLLMUnavailable}
	f := NewWithLLM(deferConfig(true), llm, LLMCallbacks{})
	d := f.Decide(Input{Title: "Pelicula.2024.1080p.x264"})
	if !d.Defer {
		t.Fatalf("expected Defer=true, got %+v", d)
	}
	if d.Allow {
		t.Errorf("a Defer decision must not Allow persistence: %+v", d)
	}
}

func TestDecide_KeepsWhenLLMUnavailableButDeferDisabled(t *testing.T) {
	llm := &fakeLLM{err: ErrLLMUnavailable}
	f := NewWithLLM(deferConfig(false), llm, LLMCallbacks{})
	d := f.Decide(Input{Title: "Pelicula.2024.1080p.x264"})
	if d.Defer {
		t.Fatalf("defer disabled: must not Defer, got %+v", d)
	}
	if !d.Allow {
		t.Errorf("defer disabled + unavailable should fail-open-keep, got %+v", d)
	}
}

func TestDecide_DoesNotDeferOnNonAvailabilityError(t *testing.T) {
	// A non-availability error (bad response, 4xx) must keep — never
	// defer — even with defer enabled, so a permanently-bad response
	// can't defer-loop forever.
	llm := &fakeLLM{err: errors.New("llm: verdict: bad json")}
	f := NewWithLLM(deferConfig(true), llm, LLMCallbacks{})
	d := f.Decide(Input{Title: "Pelicula.2024.1080p.x264"})
	if d.Defer {
		t.Fatalf("non-availability error must not defer, got %+v", d)
	}
	if !d.Allow {
		t.Errorf("should fail-open-keep, got %+v", d)
	}
}

func TestDecide_NoDeferInShadowMode(t *testing.T) {
	// Shadow mode (Enforce=false) must never change persistence, even
	// when the LLM is down and defer is opted in.
	cfg := deferConfig(true)
	cfg.Enforce = false
	f := NewWithLLM(cfg, &fakeLLM{err: ErrLLMUnavailable}, LLMCallbacks{})
	d := f.Decide(Input{Title: "Pelicula.2024.1080p.x264"})
	if d.Defer {
		t.Fatalf("shadow mode must not defer, got %+v", d)
	}
	if !d.Allow {
		t.Errorf("shadow mode keeps everything, got %+v", d)
	}
}

// --- Unlimited / disabled budget ---

func TestDailyBudget_NegativeIsUnlimited(t *testing.T) {
	b := newDailyBudget(-1)
	now := time.Now()
	for i := 0; i < 10000; i++ {
		if !b.TryConsume(now) {
			t.Fatalf("negative (unlimited) budget refused at call %d", i)
		}
	}
}

func TestDailyBudget_ZeroIsDisabled(t *testing.T) {
	if newDailyBudget(0).TryConsume(time.Now()) {
		t.Fatal("zero budget must always refuse")
	}
}

// --- API-style resolution ---

func TestNewOpenAIClient_APIStyleResolution(t *testing.T) {
	cases := []struct{ base, explicit, want string }{
		{"", "", apiStyleResponses}, // default OpenAI base
		{"https://api.openai.com/v1", "", apiStyleResponses},
		{"http://localhost:11434/v1", "", apiStyleChat},               // self-hosted -> chat
		{"http://localhost:11434/v1", "responses", apiStyleResponses}, // explicit honoured
		{"https://api.openai.com/v1", "chat", apiStyleChat},           // explicit honoured
		{"http://localhost:11434/v1", "ollama", apiStyleOllama},       // ollama explicit
	}
	for _, c := range cases {
		got := NewOpenAIClient("k", "m", c.base, c.explicit, "v1", time.Second).(*openaiClient).APIStyle()
		if got != c.want {
			t.Errorf("base=%q explicit=%q: got %q want %q", c.base, c.explicit, got, c.want)
		}
	}
}

// --- Ollama native /api/chat path ---

func TestClassifyOllama_NativeApiChatStripsV1(t *testing.T) {
	var gotPath string
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		_, _ = w.Write([]byte(`{"message":{"content":"{\"is_english\":false,\"confidence\":0.95,\"reason\":\"portuguese\"}"}}`))
	}))
	defer srv.Close()
	// base URL ends in /v1; ollama style must strip it and POST <host>/api/chat
	c := NewOpenAIClient("", "qwen3.6:35b", srv.URL+"/v1", "ollama", "v1", 2*time.Second)
	v, err := c.Classify(context.Background(), "Operacao.Obscura.2020.DUAL")
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if gotPath != "/api/chat" {
		t.Errorf("ollama must POST /api/chat (/v1 stripped), got %s", gotPath)
	}
	if think, ok := gotBody["think"].(bool); !ok || think {
		t.Errorf("ollama body must set think:false, got %v", gotBody["think"])
	}
	if v.IsEnglish || v.Reason != "portuguese" {
		t.Errorf("bad verdict: %+v", v)
	}
}

// --- Chat path + availability classification (httptest) ---

func TestClassifyChat_ParsesChatCompletion(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			t.Errorf("chat style must POST <base>/chat/completions, got %s", r.URL.Path)
		}
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"{\"is_english\":false,\"confidence\":0.9,\"reason\":\"spanish-article\"}"}}]}`))
	}))
	defer srv.Close()
	c := NewOpenAIClient("", "qwen", srv.URL+"/v1", "chat", "v1", 2*time.Second)
	v, err := c.Classify(context.Background(), "La Pelicula 2024")
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if v.IsEnglish || v.Confidence != 0.9 || v.Reason != "spanish-article" {
		t.Errorf("bad verdict: %+v", v)
	}
}

func TestClassifyChat_StripsThinkBlock(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("{\"choices\":[{\"message\":{\"content\":\"<think>\\nhmm\\n</think>\\n{\\\"is_english\\\":true,\\\"confidence\\\":1.0,\\\"reason\\\":\\\"english-clear\\\"}\"}}]}"))
	}))
	defer srv.Close()
	c := NewOpenAIClient("", "qwen", srv.URL+"/v1", "chat", "v1", 2*time.Second)
	v, err := c.Classify(context.Background(), "The Batman 2022")
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if !v.IsEnglish || v.Reason != "english-clear" {
		t.Errorf("think-block not stripped / bad verdict: %+v", v)
	}
}

func TestClassify_HTTP503IsUnavailable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()
	c := NewOpenAIClient("", "qwen", srv.URL+"/v1", "chat", "v1", 2*time.Second)
	if _, err := c.Classify(context.Background(), "x"); !errors.Is(err, ErrLLMUnavailable) {
		t.Fatalf("503 must be ErrLLMUnavailable, got %v", err)
	}
}

func TestClassify_HTTP400IsNotUnavailable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
	}))
	defer srv.Close()
	c := NewOpenAIClient("", "qwen", srv.URL+"/v1", "chat", "v1", 2*time.Second)
	_, err := c.Classify(context.Background(), "x")
	if err == nil {
		t.Fatal("expected error")
	}
	if errors.Is(err, ErrLLMUnavailable) {
		t.Errorf("400 must NOT be ErrLLMUnavailable (endpoint up, request bad), got %v", err)
	}
}

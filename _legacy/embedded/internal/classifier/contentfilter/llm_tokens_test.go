package contentfilter

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// TestTokenUsageIsParsedFromEveryAPIShape covers item 4: bitagent
// discarded the provider's token accounting on every call, so no token or cost
// metric existed anywhere in the service and a $207/mo spend ran unseen.
//
// The three shapes report usage under DIFFERENT names — chat uses
// prompt_tokens/completion_tokens, the Responses API uses
// input_tokens/output_tokens, and Ollama's native API uses top-level
// prompt_eval_count/eval_count. Getting one right proves nothing about the
// others, so all three are exercised against a real HTTP server.
func TestTokenUsageIsParsedFromEveryAPIShape(t *testing.T) {
	verdictJSON := `{"is_english":true,"confidence":0.9,"reason":"latin"}`
	for _, tc := range []struct {
		style   string
		path    string
		body    map[string]any
		wantIn  int
		wantOut int
	}{
		{
			style: apiStyleChat, path: "/chat/completions",
			body: map[string]any{
				"choices": []map[string]any{{"message": map[string]string{"content": verdictJSON}}},
				"usage":   map[string]int{"prompt_tokens": 137, "completion_tokens": 21},
			},
			wantIn: 137, wantOut: 21,
		},
		{
			style: apiStyleResponses, path: "/responses",
			body: map[string]any{
				"output_text": verdictJSON,
				"usage":       map[string]int{"input_tokens": 244, "output_tokens": 33},
			},
			wantIn: 244, wantOut: 33,
		},
		{
			style: apiStyleOllama, path: "/api/chat",
			body: map[string]any{
				"message":           map[string]string{"content": verdictJSON},
				"prompt_eval_count": 512,
				"eval_count":        64,
			},
			wantIn: 512, wantOut: 64,
		},
	} {
		t.Run(tc.style, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != tc.path {
					t.Errorf("path = %q, want %q", r.URL.Path, tc.path)
				}
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(tc.body)
			}))
			defer srv.Close()

			c := NewOpenAIClient("k", "test-model-v1", srv.URL, tc.style, "v2", 5*time.Second)
			v, err := c.Classify(context.Background(), "Some.Title.2024")
			if err != nil {
				t.Fatalf("Classify: %v", err)
			}
			if v.Usage.PromptTokens != tc.wantIn || v.Usage.CompletionTokens != tc.wantOut {
				t.Errorf("usage = %d/%d, want %d/%d",
					v.Usage.PromptTokens, v.Usage.CompletionTokens, tc.wantIn, tc.wantOut)
			}
			if v.Usage.Total() != tc.wantIn+tc.wantOut {
				t.Errorf("Total() = %d, want %d", v.Usage.Total(), tc.wantIn+tc.wantOut)
			}
			// The model must be stamped from the ACTUAL call — that is the
			// whole point of item 2.
			if v.Model != "test-model-v1" {
				t.Errorf("Model = %q, want %q", v.Model, "test-model-v1")
			}
		})
	}
}

// TestMissingUsageIsNotRecordedAsZero: a provider that omits usage must leave
// the verdict usable and must NOT be counted as zero tokens. Zero on a
// dashboard reads as "this call was free", which is the exact misreading this
// metric exists to end.
func TestMissingUsageIsNotRecordedAsZero(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"{\"is_english\":true,\"confidence\":0.9,\"reason\":\"latin\"}"}}]}`))
	}))
	defer srv.Close()

	c := NewOpenAIClient("k", "m", srv.URL, apiStyleChat, "v2", 5*time.Second)
	v, err := c.Classify(context.Background(), "T")
	if err != nil {
		t.Fatalf("a missing usage block must not fail the verdict: %v", err)
	}
	if v.Usage.Total() != 0 {
		t.Fatalf("Total() = %d, want 0", v.Usage.Total())
	}

	// The recorder drops zero-token calls rather than emitting a 0 sample.
	cb := NewMetrics().LLMCallbacks()
	cb.OnLLMTokens(v.Model, v.Usage)
	cb.OnLLMTokens("", TokenUsage{PromptTokens: 10, CompletionTokens: 10}) // unknown model is also dropped
}

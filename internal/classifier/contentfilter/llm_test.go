package contentfilter

import (
	"strings"
	"testing"
)

// Test plan for llm.go:
//   1. parseLLMVerdict — strict JSON, fenced JSON, surrounding whitespace,
//      malformed input, confidence clamping.
//   2. extractOutputText — convenience field shape, nested envelope, missing.
//   3. truncate — boundary cases.
//
// We deliberately don't test Classify() against a real network; that's
// the wiring layer's job. The HTTP handler is exercised indirectly by
// constructing the request body in factory wiring.

func TestParseLLMVerdict_StrictJSON(t *testing.T) {
	v, err := parseLLMVerdict(`{"is_english": true, "confidence": 0.92, "reason": "english-clear"}`)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if !v.IsEnglish {
		t.Errorf("IsEnglish: want true, got false")
	}
	if v.Confidence != 0.92 {
		t.Errorf("Confidence: want 0.92, got %v", v.Confidence)
	}
	if v.Reason != "english-clear" {
		t.Errorf("Reason: want english-clear, got %q", v.Reason)
	}
}

func TestParseLLMVerdict_FencedJSON(t *testing.T) {
	// Some models still wrap structured output despite "no prose"
	// instructions. The parser tolerates a single ```json ... ```
	// fence.
	cases := []string{
		"```json\n{\"is_english\": false, \"confidence\": 0.88, \"reason\": \"russian-particle\"}\n```",
		"```\n{\"is_english\": false, \"confidence\": 0.88, \"reason\": \"russian-particle\"}\n```",
		"  ```json\n{\"is_english\": false, \"confidence\": 0.88, \"reason\": \"russian-particle\"}\n```  ",
	}
	for _, c := range cases {
		v, err := parseLLMVerdict(c)
		if err != nil {
			t.Errorf("case %q: unexpected err: %v", c, err)
			continue
		}
		if v.IsEnglish {
			t.Errorf("case %q: IsEnglish: want false", c)
		}
		if v.Reason != "russian-particle" {
			t.Errorf("case %q: Reason: want russian-particle, got %q", c, v.Reason)
		}
	}
}

func TestParseLLMVerdict_RejectsUnsafeConfidence(t *testing.T) {
	for _, input := range []string{
		`{"is_english":false,"confidence":1.5,"reason":"x"}`,
		`{"is_english":false,"confidence":-0.3,"reason":"x"}`,
		`{"is_english":false,"confidence":"NaN","reason":"x"}`,
	} {
		if _, err := parseLLMVerdict(input); err == nil {
			t.Errorf("expected unsafe confidence to fail for %q", input)
		}
	}
}

func TestParseLLMVerdict_Malformed(t *testing.T) {
	bad := []string{
		``,
		`not json at all`,
		`{"is_english": "yes"}`, // wrong type
		`{`,                     // truncated
		`null`,
		`{}`,
		`{"confidence":0.99,"reason":"x"}`,
		`{"is_english":null,"confidence":0.99,"reason":"x"}`,
		`{"is_english":false,"confidence":0.99,"reason":null}`,
		`{"is_english":false,"confidence":0.99,"reason":"x","extra":true}`,
		`{"is_english":false,"confidence":0.99,"reason":""}`,
		`{"is_english":false,"confidence":0.99,"reason":"xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx"}`,
	}
	for _, b := range bad {
		if _, err := parseLLMVerdict(b); err == nil {
			t.Errorf("expected error for %q, got nil", b)
		}
	}
}

func TestExtractOutputText_ConvenienceField(t *testing.T) {
	raw := []byte(`{"output_text":"{\"is_english\":true,\"confidence\":0.9,\"reason\":\"english-clear\"}"}`)
	got, err := extractOutputText(raw)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if !strings.Contains(got, "english-clear") {
		t.Errorf("got %q, want substring english-clear", got)
	}
}

func TestExtractOutputText_NestedShape(t *testing.T) {
	// Full Responses-API envelope.
	raw := []byte(`{
		"output": [
			{
				"content": [
					{"type": "output_text", "text": "{\"is_english\":false,\"confidence\":0.9,\"reason\":\"cyrillic-words\"}"}
				]
			}
		]
	}`)
	got, err := extractOutputText(raw)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if !strings.Contains(got, "cyrillic-words") {
		t.Errorf("got %q, want cyrillic-words substring", got)
	}
}

func TestExtractOutputText_NoOutput(t *testing.T) {
	raw := []byte(`{"id":"resp_x","status":"completed"}`)
	if _, err := extractOutputText(raw); err == nil {
		t.Errorf("expected error on missing output, got nil")
	}
}

func TestExtractOutputText_BadJSON(t *testing.T) {
	if _, err := extractOutputText([]byte(`{not json`)); err == nil {
		t.Errorf("expected error on malformed JSON")
	}
}

func TestTruncate(t *testing.T) {
	if got := truncate("short", 10); got != "short" {
		t.Errorf("short: got %q", got)
	}
	if got := truncate("12345678901234567890", 10); !strings.HasPrefix(got, "1234567890") {
		t.Errorf("long: got %q", got)
	}
	if got := truncate("12345678901234567890", 10); !strings.HasSuffix(got, "[truncated]") {
		t.Errorf("long: missing suffix in %q", got)
	}
}

func TestNewOpenAIClient_Defaults(t *testing.T) {
	// Empty values should fall through to documented defaults so the
	// caller doesn't have to track each.
	c := NewOpenAIClient("k", "", "", "", "", 0).(*openaiClient)
	if c.model != "gpt-5.4-nano" {
		t.Errorf("model default: got %q want gpt-5.4-nano", c.model)
	}
	if c.baseURL != "https://api.openai.com/v1" {
		t.Errorf("baseURL default: got %q", c.baseURL)
	}
	if c.promptVersion != "v1" {
		t.Errorf("promptVersion default: got %q", c.promptVersion)
	}
	if c.http.Timeout.Seconds() != 8 {
		t.Errorf("timeout default: got %v", c.http.Timeout)
	}
}

func TestNewOpenAIClient_TrimsTrailingSlash(t *testing.T) {
	c := NewOpenAIClient("k", "m", "https://example.com/v1/", "", "v2", 0).(*openaiClient)
	if c.baseURL != "https://example.com/v1" {
		t.Errorf("baseURL: got %q want https://example.com/v1", c.baseURL)
	}
}

func TestOpenAIDataSharingChatBodyIsDirectAndBounded(t *testing.T) {
	body := chatRequestBodyWithRouting("gpt-5.6-sol", "Example", "", 80, true)
	if body["reasoning_effort"] != "none" || body["store"] != false {
		t.Fatalf("data-sharing controls missing: %#v", body)
	}
	if _, ok := body["provider"]; ok {
		t.Fatal("direct OpenAI request contains OpenRouter provider policy")
	}
	if strings.Contains(body["messages"].([]map[string]string)[0]["content"], "/no_think") {
		t.Fatal("direct OpenAI request retained Ollama-only prompt hint")
	}
}

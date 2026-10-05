package contentfilter

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/spencercnorton/bitagent/internal/llmcapture"
	"github.com/stretchr/testify/require"
)

const evaluationHTTPVerdict = `{"is_english":false,"confidence":0.93,"reason":"synthetic-foreign"}`

func evaluationChatEnvelope(t *testing.T) []byte {
	t.Helper()
	return []byte(mustJSON(t, map[string]any{"model": "synthetic-model", "choices": []any{map[string]any{
		"finish_reason": "stop", "message": map[string]any{"role": "assistant", "content": evaluationHTTPVerdict},
	}}}))
}

func TestEvaluationHTTPVerdictUsesExactKnownContracts(t *testing.T) {
	chat := evaluationChatEnvelope(t)
	responses := []byte(mustJSON(t, map[string]any{"status": "completed", "output_text": evaluationHTTPVerdict}))
	nested := []byte(mustJSON(t, map[string]any{"status": "completed", "output": []any{
		map[string]any{"type": "reasoning", "status": "completed"},
		map[string]any{"type": "message", "status": "completed", "role": "assistant", "content": []any{map[string]any{"type": "output_text", "text": evaluationHTTPVerdict}}},
	}}))
	ollama := []byte(mustJSON(t, map[string]any{"done": true, "done_reason": "stop", "message": map[string]any{"role": "assistant", "content": evaluationHTTPVerdict}}))
	for _, tc := range []struct {
		name, contract string
		body           []byte
	}{
		{"responses convenience", "contentfilter-responses-model-input-v1", responses},
		{"responses nested", "contentfilter-responses-model-input-v1", nested},
		{"direct chat", "contentfilter-chat-model-input-v1", chat},
		{"pinned chat", "contentfilter-chat-model-input-v2-openrouter", chat},
		{"data-sharing chat", "contentfilter-chat-model-input-v3-openai-data-sharing", chat},
		{"native ollama", "contentfilter-ollama-model-input-v1", ollama},
	} {
		t.Run(tc.name, func(t *testing.T) {
			verdict, err := EvaluationParseHTTPVerdict(tc.body, tc.contract)
			require.NoError(t, err)
			require.False(t, verdict.IsEnglish)
			require.Equal(t, .93, verdict.Confidence)
			require.Equal(t, "synthetic-foreign", verdict.Reason)
		})
	}
	for _, contract := range []string{"", "contentfilter-chat-model-input-v4", "classifier-type-v1"} {
		_, err := EvaluationParseHTTPVerdict(chat, contract)
		require.Error(t, err)
	}
}

func TestEvaluationHTTPVerdictRejectsAmbiguousIncompleteOrRefusedEnvelopes(t *testing.T) {
	chat := evaluationChatEnvelope(t)
	var valid map[string]any
	require.NoError(t, json.Unmarshal(chat, &valid))
	for _, tc := range []struct {
		name, contract, style string
		body                  []byte
	}{
		{"duplicate outer field", "contentfilter-chat-model-input-v1", apiStyleChat, []byte(`{"choices":[],` + string(chat)[1:])},
		{"duplicate verdict field", "contentfilter-chat-model-input-v1", apiStyleChat, []byte(mustJSON(t, map[string]any{"choices": []any{map[string]any{"message": map[string]any{"content": `{"is_english":true,"is_english":false,"confidence":0.93,"reason":"synthetic-foreign"}`}}}}))},
		{"multiple choices", "contentfilter-chat-model-input-v1", apiStyleChat, []byte(mustJSON(t, map[string]any{"choices": []any{valid["choices"].([]any)[0], valid["choices"].([]any)[0]}}))},
		{"truncated chat", "contentfilter-chat-model-input-v1", apiStyleChat, []byte(strings.Replace(string(chat), `"stop"`, `"length"`, 1))},
		{"refused chat", "contentfilter-chat-model-input-v1", apiStyleChat, []byte(strings.Replace(string(chat), `"role":"assistant"`, `"role":"assistant","refusal":"declined"`, 1))},
		{"incomplete responses", "contentfilter-responses-model-input-v1", apiStyleResponses, []byte(mustJSON(t, map[string]any{"status": "incomplete", "output_text": evaluationHTTPVerdict}))},
		{"incomplete details", "contentfilter-responses-model-input-v1", apiStyleResponses, []byte(mustJSON(t, map[string]any{"status": "completed", "incomplete_details": map[string]any{"reason": "max_output_tokens"}, "output_text": evaluationHTTPVerdict}))},
		{"conflicting responses text", "contentfilter-responses-model-input-v1", apiStyleResponses, []byte(mustJSON(t, map[string]any{"output_text": evaluationHTTPVerdict, "output": []any{map[string]any{"content": []any{map[string]any{"type": "output_text", "text": `{"is_english":true,"confidence":1,"reason":"english-clear"}`}}}}}))},
		{"refused responses", "contentfilter-responses-model-input-v1", apiStyleResponses, []byte(mustJSON(t, map[string]any{"output_text": evaluationHTTPVerdict, "output": []any{map[string]any{"content": []any{map[string]any{"type": "refusal", "refusal": "declined"}}}}}))},
		{"incomplete ollama", "contentfilter-ollama-model-input-v1", apiStyleOllama, []byte(mustJSON(t, map[string]any{"done": false, "message": map[string]any{"content": evaluationHTTPVerdict}}))},
		{"provider error", "contentfilter-chat-model-input-v1", apiStyleChat, []byte(mustJSON(t, map[string]any{"error": map[string]any{"message": "failed"}, "choices": valid["choices"]}))},
		{"trailing document", "contentfilter-chat-model-input-v1", apiStyleChat, append(append([]byte{}, chat...), []byte(` {}`)...)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := EvaluationParseHTTPVerdict(tc.body, tc.contract)
			require.Error(t, err)
			// The same bytes delivered to the actual runtime client must also
			// fail open; the evaluator does not maintain a second decoder.
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write(tc.body) }))
			defer server.Close()
			client := NewOpenAIClient("test", "synthetic-model", server.URL, tc.style, "v1", time.Second)
			_, err = client.Classify(context.Background(), "Synthetic Feature 2026")
			require.Error(t, err)
		})
	}
}

func TestEvaluationHTTPVerdictBoundsBodyAndJSONDepth(t *testing.T) {
	for _, body := range [][]byte{
		nil,
		[]byte(strings.Repeat(" ", llmcapture.MaxResultBodyBytes+1)),
		[]byte(`{"metadata":` + strings.Repeat("[", 34) + `0` + strings.Repeat("]", 34) + `}`),
	} {
		_, err := EvaluationParseHTTPVerdict(body, "contentfilter-chat-model-input-v1")
		require.Error(t, err)
	}
}

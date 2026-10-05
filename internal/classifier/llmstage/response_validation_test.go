package llmstage

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"
)

func answerEnvelope(t *testing.T, answer string) []byte {
	t.Helper()
	raw, err := json.Marshal(map[string]any{
		"id": "synthetic-response", "object": "chat.completion",
		"choices": []any{map[string]any{
			"index": 0, "finish_reason": "stop",
			"message": map[string]any{"role": "assistant", "content": answer},
		}},
	})
	require.NoError(t, err)
	return raw
}

func TestLiveWorkflowRejectsAmbiguousTypeResponses(t *testing.T) {
	valid := `{"category":"movie","confidence":0.99}`
	for name, raw := range map[string][]byte{
		"provider_error":       []byte(strings.TrimSuffix(string(answerEnvelope(t, valid)), "}") + `,"error":{"message":"synthetic failure"}}`),
		"wrong_role":           []byte(strings.Replace(string(answerEnvelope(t, valid)), `"assistant"`, `"user"`, 1)),
		"null_role":            []byte(strings.Replace(string(answerEnvelope(t, valid)), `"assistant"`, `null`, 1)),
		"category_duplicate":   answerEnvelope(t, `{"category":"unknown","category":"movie","confidence":0.99}`),
		"confidence_duplicate": answerEnvelope(t, `{"category":"movie","confidence":0.01,"confidence":0.99}`),
		"escaped_duplicate":    answerEnvelope(t, `{"category":"unknown","cate\u0067ory":"movie","confidence":0.99}`),
		"case_variant":         answerEnvelope(t, `{"category":"unknown","Category":"movie","confidence":0.99}`),
		"unknown_field":        answerEnvelope(t, `{"category":"movie","confidence":0.99,"attach":true}`),
		"field_case":           answerEnvelope(t, `{"Category":"movie","confidence":0.99}`),
		"duplicate_choices":    []byte(`{"choices":[],"choices":[{"finish_reason":"stop","message":{"content":"{\"category\":\"movie\",\"confidence\":0.99}"}}]}`),
		"unicode_choices":      []byte(`{"choiceſ":[],"choiceſ":[{"finish_reason":"stop","message":{"content":"{\"category\":\"movie\",\"confidence\":0.99}"}}]}`),
		"duplicate_finish":     []byte(`{"choices":[{"finish_reason":"length","finish_reason":"stop","message":{"content":"{\"category\":\"movie\",\"confidence\":0.99}"}}]}`),
		"duplicate_content":    []byte(`{"choices":[{"finish_reason":"stop","message":{"content":"{}","content":"{\"category\":\"movie\",\"confidence\":0.99}"}}]}`),
	} {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			inner := coreWorkflowForTypeStage(t)
			torrent, flags := unknownTypeWorkflowTorrent(), offlineTypeWorkflowFlags()
			baseline, baselineErr := inner.Run(ctx, "default", flags, torrent)
			cfg := NewDefaultConfig()
			cfg.Enabled, cfg.EnableLive = true, true
			var calls atomic.Int32
			stage := newStageWithServer(t, cfg, inner, fakePrivacy{}, func(w http.ResponseWriter, _ *http.Request) {
				calls.Add(1)
				_, err := w.Write(raw)
				require.NoError(t, err)
			})
			result, err := stage.Run(ctx, "default", flags, torrent)
			require.Equal(t, baselineErr, err)
			require.Equal(t, baseline, result)
			require.EqualValues(t, 1, calls.Load())
			audit := stage.admission.Capture.(*testAudit)
			require.Len(t, audit.results, 1)
			require.Equal(t, raw, audit.results[0].Body)
			require.Len(t, audit.decisions, 1)
			require.Equal(t, "invalid_response", audit.decisions[0].Outcome)
			require.False(t, audit.decisions[0].WouldApply)
			_, parseErr := EvaluationParseResponse(raw)
			require.Error(t, parseErr)
		})
	}
	// Provider metadata stays extensible; decision fields stay exact.
	decision, err := EvaluationParseResponse(answerEnvelope(t, valid))
	require.NoError(t, err)
	require.Equal(t, .99, decision.Confidence)
	require.Equal(t, "movie", string(decision.MediaType))
}

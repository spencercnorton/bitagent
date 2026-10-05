package llmmatch

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/spencercnorton/bitagent/internal/llmcapture"
	"github.com/stretchr/testify/require"
)

func strictMatcherEnvelope(t *testing.T, content string) string {
	t.Helper()
	raw, err := json.Marshal(map[string]any{"choices": []any{map[string]any{
		"finish_reason": "stop", "message": map[string]any{"role": "assistant", "content": content},
	}}})
	require.NoError(t, err)
	return string(raw)
}

func TestMatcherChatRejectsAmbiguousOrExplicitlyIncompleteEnvelopes(t *testing.T) {
	valid := strictMatcherEnvelope(t, `{}`)
	for _, tc := range []struct {
		name, body string
	}{
		{"multiple choices", `{"choices":[{"message":{"content":"{}"}},{"message":{"content":"{}"}}]}`},
		{"truncated output", strings.Replace(valid, `"stop"`, `"length"`, 1)},
		{"tool output", strings.Replace(valid, `"stop"`, `"tool_calls"`, 1)},
		{"null finish reason", strings.Replace(valid, `"stop"`, `null`, 1)},
		{"empty finish reason", strings.Replace(valid, `"stop"`, `""`, 1)},
		{"non-assistant role", strings.Replace(valid, `"assistant"`, `"user"`, 1)},
		{"null role", strings.Replace(valid, `"assistant"`, `null`, 1)},
		{"refusal with content", strings.Replace(valid, `"role":"assistant"`, `"refusal":"declined","role":"assistant"`, 1)},
		{"malformed refusal", strings.Replace(valid, `"role":"assistant"`, `"refusal":false,"role":"assistant"`, 1)},
		{"provider error with content", `{"error":{"message":"failed"},` + valid[1:]},
		{"case duplicate choices", `{"Choices":[],` + valid[1:]},
		{"Unicode duplicate choices", `{"choice\u017f":[],` + valid[1:]},
		{"escaped duplicate choices", `{"\u0063hoices":[],` + valid[1:]},
		{"case duplicate content", strings.Replace(valid, `"content":`, `"Content":"ignored","content":`, 1)},
		{"escaped duplicate content", strings.Replace(valid, `"content":`, `"\u0063ontent":"ignored","content":`, 1)},
		{"case duplicate finish reason", strings.Replace(valid, `"finish_reason":"stop"`, `"Finish_Reason":"length","finish_reason":"stop"`, 1)},
		{"escaped duplicate finish reason", strings.Replace(valid, `"finish_reason":"stop"`, `"\u0066inish_reason":"length","finish_reason":"stop"`, 1)},
		{"nested metadata duplicate", `{"metadata":{"nested":{"id":1,"ID":2}},` + valid[1:]},
		{"depth limit", `{"metadata":` + strings.Repeat(`[`, 34) + `0` + strings.Repeat(`]`, 34) + `,` + valid[1:]},
		{"trailing document", valid + ` {}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			raw, content, err := ReadMatcherChatResponse(http.StatusOK, strings.NewReader(tc.body))
			require.ErrorIs(t, err, ErrMatcherChatEnvelope)
			require.Equal(t, tc.body, string(raw))
			require.Nil(t, content)
		})
	}
}

func TestMatcherRuntimeRejectsUnsafeChoicesRetainsEvidenceAndDoesNotCache(t *testing.T) {
	validDecision := `{"tmdb_id":7300101,"confidence":0.95}`
	valid := strictMatcherEnvelope(t, validDecision)
	for _, tc := range []struct {
		name, body, errorClass string
		extract                bool
	}{
		{"multiple choices", `{"choices":[{"message":{"content":"{\"tmdb_id\":7300101,\"confidence\":0.95}"}},{"message":{"content":"{\"tmdb_id\":7300102,\"confidence\":0.99}"}}]}`, "envelope", false},
		{"incomplete choice", strings.Replace(valid, `"stop"`, `"length"`, 1), "envelope", false},
		{"provider error", `{"error":{"message":"failed"},` + valid[1:], "envelope", false},
		{"refusal", strings.Replace(valid, `"role":"assistant"`, `"refusal":"declined","role":"assistant"`, 1), "envelope", false},
		{"non-assistant role", strings.Replace(valid, `"assistant"`, `"user"`, 1), "envelope", false},
		{"Unicode duplicate choices", `{"choice\u017f":[],` + valid[1:], "envelope", false},
		{"escaped duplicate content", strings.Replace(valid, `"content":`, `"\u0063ontent":"ignored","content":`, 1), "envelope", false},
		{"duplicate chosen id", strictMatcherEnvelope(t, `{"tmdb_id":7300102,"tmdb_id":7300101,"confidence":0.95}`), "none", false},
		{"case duplicate confidence", strictMatcherEnvelope(t, `{"tmdb_id":7300101,"Confidence":0.01,"confidence":0.95}`), "none", false},
		{"escaped duplicate confidence", strictMatcherEnvelope(t, `{"tmdb_id":7300101,"\u0063onfidence":0.01,"confidence":0.95}`), "none", false},
		{"unknown decision field", strictMatcherEnvelope(t, `{"tmdb_id":7300101,"confidence":0.95,"override":true}`), "none", false},
		{"multiple prose answers", strictMatcherEnvelope(t, "First answer: "+validDecision+` Revised: {"tmdb_id":7300102,"confidence":0.99}`), "none", false},
		{"truncated second answer", strictMatcherEnvelope(t, "First answer: "+validDecision+` Revised: {"tmdb_id":7300102`), "none", false},
		{"multiple fenced answers", strictMatcherEnvelope(t, "```json\n"+validDecision+"\n```\n```json\n"+validDecision+"\n```"), "none", false},
		{"duplicate extraction pack gate", strictMatcherEnvelope(t, `{"title":"Glass Acacia","year":2031,"type":"movie","is_anime":false,"is_pack":true,"i\u017f_pack":false,"is_adult":false}`), "none", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var calls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				calls.Add(1)
				_, _ = w.Write([]byte(tc.body))
			}))
			defer server.Close()
			audit := &resultCaptureProbe{first: true}
			client := matcherClientWithCapture(server.URL, audit)
			client.cfg.EnableLive = true
			torrent := mediaTorrent("Glass.Acacia.2031.mkv")
			for attempt := 0; attempt < 2; attempt++ {
				ctx, trace := llmcapture.WithResultTrace(context.Background())
				task, source := llmcapture.TaskMatcherRerank, llmcapture.CandidateSourceLocal
				if tc.extract {
					ext, err := client.Extract(ctx, torrent)
					require.Error(t, err)
					require.Equal(t, Extraction{}, ext)
					task, source = llmcapture.TaskMatcherExtract, llmcapture.CandidateSourceNone
				} else {
					id, confidence, err := client.RerankForMediaType(ctx, torrent, Extraction{Title: "Glass Acacia", Year: 2031, Type: "movie"}, "Glass Acacia", false,
						[]Candidate{{ID: 7300101, Title: "Glass Acacia", Year: 2031}, {ID: 7300102, Title: "Other Acacia", Year: 2031}}, source)
					require.Error(t, err)
					require.Zero(t, id)
					require.Zero(t, confidence)
				}
				receipt, ok := trace.Result(task, source)
				require.True(t, ok, "a rejected response retains its HTTP observation")
				require.False(t, receipt.FromCache)
				require.Equal(t, tc.errorClass, receipt.ErrorClass)
				digest := sha256.Sum256([]byte(tc.body))
				require.Equal(t, digest[:], receipt.ResponseSHA256)
				key, err := llmcapture.KeyForRequest(audit.requests[attempt])
				require.NoError(t, err)
				require.Equal(t, key, receipt.CaptureKey)
			}
			require.EqualValues(t, 2, calls.Load(), "rejected output must not become a cached match or decline")
			require.Len(t, audit.results, 2)
			for _, result := range audit.results {
				require.Equal(t, []byte(tc.body), result.Body)
				require.Equal(t, http.StatusOK, result.StatusCode)
			}
		})
	}
}

func TestBatchMatcherRejectsDuplicateMappingBeforeDecodingRows(t *testing.T) {
	valid := batchItemsJSON(2)
	for _, tc := range []struct{ name, content string }{
		{"case duplicate items", `{"Items":[],` + valid[1:]},
		{"escaped duplicate items", `{"\u0069tems":[],` + valid[1:]},
		{"duplicate row id", strings.Replace(valid, `"id":1`, `"id":2,"id":1`, 1)},
		{"case duplicate row title", strings.Replace(valid, `"title":`, `"Title":"Different","title":`, 1)},
		{"multiple fenced answers", "```json\n" + valid + "\n```\n```json\n" + valid + "\n```"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server, _ := chatServer(t, tc.content)
			client := testClient(server.URL)
			got, err := client.callBatchExtract(context.Background(), batchTorrents(2))
			require.Error(t, err)
			require.Nil(t, got)
		})
	}
	for _, content := range []string{
		valid,
		"```json\n" + valid + "\n```",
		"<think>There are two release names.</think>\n" + valid,
		"Here is the extraction: " + valid + " Done.",
	} {
		server, _ := chatServer(t, content)
		client := testClient(server.URL)
		got, err := client.callBatchExtract(context.Background(), batchTorrents(2))
		require.NoError(t, err)
		require.Len(t, got, 2)
	}
}

func TestBatchMatcherAbstainsRepeatedKnownIDsAndKeepsUnambiguousRows(t *testing.T) {
	first := `{"id":1,"title":"Glass Acacia","year":2031,"type":"movie","is_anime":false,"is_pack":false,"is_adult":false}`
	conflicting := strings.Replace(first, "Glass Acacia", "Other Acacia", 1)
	unambiguous := strings.Replace(first, `"id":1`, `"id":2`, 1)
	for _, duplicates := range []string{
		first + `,` + conflicting,
		first + `,` + conflicting + `,` + first,
		`{"id":1,"title":"Incomplete"},` + first,
	} {
		server, _ := chatServer(t, `{"items":[`+duplicates+`,`+unambiguous+`]}`)
		client := testClient(server.URL)
		got, err := client.callBatchExtract(context.Background(), batchTorrents(2))
		require.NoError(t, err)
		require.Len(t, got, 1)
		require.NotContains(t, got, 1, "all repeated known ids must abstain")
		require.Equal(t, "Glass Acacia", got[2].Title)
	}
}

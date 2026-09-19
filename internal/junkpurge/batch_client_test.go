package junkpurge

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestBuildBatchInputIsDeterministic(t *testing.T) {
	client := newOpenAIBatchClient(NewDefaultConfig())
	run := batchRun{
		ID: 17, Model: "gpt-test", Endpoint: "/v1/chat/completions",
		SystemPrompt: "persisted prompt", MaxTokens: 123, ReasoningEffort: "low",
	}
	items := []batchItem{
		{CustomID: "jp:17:000001", TorrentName: "first title"},
		{CustomID: "jp:17:000002", TorrentName: "second title"},
	}

	first, filename, digest, err := client.BuildInput(run, 2, items)
	require.NoError(t, err)
	second, secondFilename, secondDigest, err := client.BuildInput(run, 2, items)
	require.NoError(t, err)
	require.Equal(t, first, second)
	require.Equal(t, filename, secondFilename)
	require.Equal(t, digest, secondDigest)
	require.True(t, strings.HasSuffix(filename, digest[:16]+".jsonl"))
	require.Equal(t, byte('\n'), first[len(first)-1])

	sum := sha256.Sum256(first)
	require.Equal(t, hex.EncodeToString(sum[:]), digest)
	lines := strings.Split(strings.TrimSpace(string(first)), "\n")
	require.Len(t, lines, 2)
	var decoded struct {
		CustomID string         `json:"custom_id"`
		Method   string         `json:"method"`
		URL      string         `json:"url"`
		Body     map[string]any `json:"body"`
	}
	require.NoError(t, json.Unmarshal([]byte(lines[0]), &decoded))
	require.Equal(t, "jp:17:000001", decoded.CustomID)
	require.Equal(t, http.MethodPost, decoded.Method)
	require.Equal(t, "/v1/chat/completions", decoded.URL)
	require.Equal(t, "gpt-test", decoded.Body["model"])
	require.Equal(t, "persisted prompt",
		decoded.Body["messages"].([]any)[0].(map[string]any)["content"])
	require.Equal(t, float64(123), decoded.Body["max_completion_tokens"])
}

func TestParseBatchResultsCombinesUnorderedOutputAndErrors(t *testing.T) {
	padding := strings.Repeat("x", 70_000) // proves parsing is not Scanner-limited
	output := strings.Join([]string{
		fmt.Sprintf(`{"custom_id":"jp:1:000002","id":"outer-2","response":{"status_code":200,"request_id":"req-2","body":{"choices":[{"message":{"content":"{\"verdict\":\"real_mangled\",\"confidence\":0.91}"}}],"usage":{"prompt_tokens":100,"completion_tokens":10,"prompt_tokens_details":{"cached_tokens":20},"completion_tokens_details":{"reasoning_tokens":5}}}},"ignored":%q}`, padding),
		`{"custom_id":"jp:1:000001","response":{"status_code":200,"request_id":"req-1","body":{"choices":[{"message":{"content":"{\"verdict\":\"junk\",\"confidence\":0.95}"}}],"usage":{"prompt_tokens":90,"completion_tokens":8}}}}`,
	}, "\n") + "\n"
	errorOutput := strings.Join([]string{
		`{"custom_id":"jp:1:000003","response":{"status_code":429,"request_id":"req-3","body":{"error":{"code":"rate_limit_exceeded","message":"later"}}}}`,
		`{"custom_id":"jp:1:000004","error":{"code":"invalid_request_error","message":"bad input"}}`,
	}, "\n") + "\n"

	client := newOpenAIBatchClient(NewDefaultConfig())
	results, err := client.ParseResults([]byte(output), []byte(errorOutput))
	require.NoError(t, err)
	require.Len(t, results, 4)
	require.Equal(t, "jp:1:000002", results[0].CustomID)
	require.True(t, results[0].Succeeded)
	require.Equal(t, "req-2", results[0].ProviderRequest)
	require.Equal(t, int64(100), results[0].Usage.Input)
	require.Equal(t, int64(20), results[0].Usage.Cached)
	require.Equal(t, int64(5), results[0].Usage.Reasoning)
	require.True(t, results[1].Judgment.IsJunk(0.8))
	require.False(t, results[2].Succeeded)
	require.True(t, results[2].Retryable)
	require.Equal(t, http.StatusTooManyRequests, results[2].ResponseStatus)
	require.False(t, results[3].Retryable)
	require.Equal(t, "invalid_request_error", results[3].ErrorCode)
}

func TestParseBatchResultsTurnsInvalidModelReplyIntoTerminalItem(t *testing.T) {
	raw := `{"custom_id":"jp:1:000001","response":{"status_code":200,"body":{"choices":[{"message":{"content":"not json"}}],"usage":{"prompt_tokens":3,"completion_tokens":2}}}}`
	results, err := parseBatchJSONL([]byte(raw))
	require.NoError(t, err)
	require.Len(t, results, 1)
	require.False(t, results[0].Succeeded)
	require.True(t, results[0].Retryable)
	require.Equal(t, "invalid_response", results[0].ErrorCode)
	require.Equal(t, int64(3), results[0].Usage.Input)
}

func TestOpenAIBatchClientLifecycleAndRecovery(t *testing.T) {
	input := []byte("{\"custom_id\":\"one\"}\n")
	sum := sha256.Sum256(input)
	digest := hex.EncodeToString(sum[:])
	metadata := map[string]string{
		"bitagent_component": "junkpurge",
		"attempt_id":         "42",
	}
	var batchListCalls atomic.Int32
	var uploadSeen atomic.Bool
	var createSeen atomic.Bool

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "Bearer secret", r.Header.Get("Authorization"))
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/files":
			require.Equal(t, "batch", r.URL.Query().Get("purpose"))
			_, _ = fmt.Fprintf(w,
				`{"data":[{"id":"file-in","bytes":%d,"filename":"input.jsonl","purpose":"batch"}],"has_more":false}`,
				len(input),
			)
		case r.Method == http.MethodGet && r.URL.Path == "/v1/files/file-in/content":
			_, _ = w.Write(input)
		case r.Method == http.MethodPost && r.URL.Path == "/v1/files":
			uploadSeen.Store(true)
			require.NoError(t, r.ParseMultipartForm(1<<20))
			require.Equal(t, "batch", r.FormValue("purpose"))
			file, header, err := r.FormFile("file")
			require.NoError(t, err)
			defer file.Close()
			require.Equal(t, "new.jsonl", header.Filename)
			got, err := io.ReadAll(file)
			require.NoError(t, err)
			require.Equal(t, input, got)
			_, _ = w.Write([]byte(
				`{"id":"file-new","bytes":20,"filename":"new.jsonl","purpose":"batch"}`,
			))
		case r.Method == http.MethodGet && r.URL.Path == "/v1/batches":
			call := batchListCalls.Add(1)
			if call == 1 {
				require.Empty(t, r.URL.Query().Get("after"))
				_, _ = w.Write([]byte(`{"data":[{"id":"unrelated","input_file_id":"other","endpoint":"/v1/chat/completions","metadata":{}}],"has_more":true,"last_id":"unrelated"}`))
				return
			}
			require.Equal(t, "unrelated", r.URL.Query().Get("after"))
			_, _ = w.Write([]byte(`{"data":[{"id":"batch-42","status":"in_progress","input_file_id":"file-in","endpoint":"/v1/chat/completions","created_at":1700000000,"request_counts":{"total":1,"completed":0,"failed":0},"metadata":{"bitagent_component":"junkpurge","attempt_id":"42"}}],"has_more":false}`))
		case r.Method == http.MethodPost && r.URL.Path == "/v1/batches":
			createSeen.Store(true)
			var body map[string]any
			require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
			require.Equal(t, "file-in", body["input_file_id"])
			require.Equal(t, "24h", body["completion_window"])
			_, _ = w.Write([]byte(`{"id":"batch-new","status":"validating","input_file_id":"file-in","endpoint":"/v1/chat/completions","created_at":1700000001,"metadata":{}}`))
		case r.Method == http.MethodGet && r.URL.Path == "/v1/batches/batch-42":
			_, _ = w.Write([]byte(`{"id":"batch-42","status":"completed","input_file_id":"file-in","output_file_id":"file-out","request_counts":{"total":1,"completed":1,"failed":0}}`))
		case r.Method == http.MethodGet && r.URL.Path == "/v1/files/file-out/content":
			_, _ = w.Write([]byte("result"))
		case r.Method == http.MethodDelete && r.URL.Path == "/v1/files/file-out":
			http.Error(w, `{"error":{"message":"gone","code":"not_found"}}`, http.StatusNotFound)
		default:
			http.Error(w, r.Method+" "+r.URL.Path, http.StatusNotFound)
		}
	}))
	defer server.Close()

	cfg := NewDefaultConfig()
	cfg.LLMBaseURL = server.URL + "/v1"
	cfg.LLMApiKey = "secret"
	client := newOpenAIBatchClient(cfg)

	file, found, err := client.FindInputFile(
		context.Background(), "input.jsonl", digest, int64(len(input)),
	)
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, "file-in", file.ID)

	uploaded, err := client.UploadInputFile(context.Background(), "new.jsonl", input)
	require.NoError(t, err)
	require.Equal(t, "file-new", uploaded.ID)
	require.True(t, uploadSeen.Load())

	foundBatch, found, err := client.FindBatch(
		context.Background(), "file-in", "/v1/chat/completions", metadata,
	)
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, "batch-42", foundBatch.ID)
	require.Equal(t, 2, int(batchListCalls.Load()))

	created, err := client.CreateBatch(context.Background(), batchWorkerCreate{
		InputFileID:      "file-in",
		Endpoint:         "/v1/chat/completions",
		CompletionWindow: "24h",
		Metadata:         metadata,
	})
	require.NoError(t, err)
	require.Equal(t, "batch-new", created.ID)
	require.True(t, createSeen.Load())

	retrieved, err := client.RetrieveBatch(context.Background(), "batch-42")
	require.NoError(t, err)
	require.Equal(t, "completed", retrieved.Status)
	require.Equal(t, 1, retrieved.RequestCompleted)
	content, err := client.DownloadFile(context.Background(), "file-out")
	require.NoError(t, err)
	require.Equal(t, []byte("result"), content)
	require.NoError(t, client.DeleteFile(context.Background(), "file-out"))
}

func TestFindInputFileRejectsAmbiguousMatches(t *testing.T) {
	input := []byte("same")
	sum := sha256.Sum256(input)
	digest := hex.EncodeToString(sum[:])
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/v1/files":
			_, _ = w.Write([]byte(`{"data":[
			  {"id":"a","bytes":4,"filename":"same.jsonl","purpose":"batch"},
			  {"id":"b","bytes":4,"filename":"same.jsonl","purpose":"batch"}
			],"has_more":false}`))
		case strings.HasSuffix(r.URL.Path, "/content"):
			_, _ = w.Write(input)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	cfg := NewDefaultConfig()
	cfg.LLMBaseURL = server.URL + "/v1"
	client := newOpenAIBatchClient(cfg)
	_, _, err := client.FindInputFile(
		context.Background(), "same.jsonl", digest, int64(len(input)),
	)
	require.ErrorContains(t, err, "ambiguous")
}

func TestCreateBatchRejectsIncompleteOrMismatchedResponse(t *testing.T) {
	tests := []struct {
		name string
		body string
		want string
	}{
		{
			name: "missing id",
			body: `{"status":"validating"}`,
			want: "no id",
		},
		{
			name: "missing status",
			body: `{"id":"batch-1"}`,
			want: "no status",
		},
		{
			name: "wrong input file",
			body: `{"id":"batch-1","status":"validating","input_file_id":"other"}`,
			want: "input file",
		},
		{
			name: "wrong endpoint",
			body: `{"id":"batch-1","status":"validating","endpoint":"/v1/responses"}`,
			want: "endpoint",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(
				func(w http.ResponseWriter, r *http.Request) {
					require.Equal(t, http.MethodPost, r.Method)
					require.Equal(t, "/v1/batches", r.URL.Path)
					_, _ = w.Write([]byte(tt.body))
				},
			))
			defer server.Close()
			cfg := NewDefaultConfig()
			cfg.LLMBaseURL = server.URL + "/v1"
			client := newOpenAIBatchClient(cfg)
			_, err := client.CreateBatch(context.Background(), batchWorkerCreate{
				InputFileID:      "file-in",
				Endpoint:         "/v1/chat/completions",
				CompletionWindow: "24h",
			})
			require.ErrorContains(t, err, tt.want)
		})
	}
}

func TestUploadUsesMultipartFilePart(t *testing.T) {
	var contentDisposition string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reader, err := r.MultipartReader()
		require.NoError(t, err)
		for {
			part, err := reader.NextPart()
			if err == io.EOF {
				break
			}
			require.NoError(t, err)
			if part.FormName() == "file" {
				contentDisposition = part.Header.Get("Content-Disposition")
			}
		}
		_, _ = w.Write([]byte(`{"id":"f","bytes":2,"filename":"x.jsonl"}`))
	}))
	defer server.Close()
	cfg := NewDefaultConfig()
	cfg.LLMBaseURL = server.URL
	client := newOpenAIBatchClient(cfg)
	_, err := client.UploadInputFile(context.Background(), "x.jsonl", []byte("{}"))
	require.NoError(t, err)
	_, params, err := mime.ParseMediaType(contentDisposition)
	require.NoError(t, err)
	require.Equal(t, "x.jsonl", params["filename"])
}

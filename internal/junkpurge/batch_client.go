package junkpurge

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const (
	batchMaxRequests   = 50_000
	batchMaxInputBytes = 200_000_000
	batchMaxReadBytes  = 512_000_000
)

type openAIBatchClient struct {
	baseURL string
	apiKey  string
	http    *http.Client
}

var _ batchWorkerClient = (*openAIBatchClient)(nil)

func (c *openAIBatchClient) BaseURL() string {
	return c.baseURL
}

func newOpenAIBatchClient(cfg Config) *openAIBatchClient {
	return &openAIBatchClient{
		baseURL: strings.TrimRight(cfg.LLMBaseURL, "/"),
		apiKey:  cfg.LLMApiKey,
		http:    &http.Client{Timeout: cfg.timeout()},
	}
}

type providerAPIError struct {
	Status  int
	Code    string
	Message string
}

func (e *providerAPIError) Error() string {
	if e.Code == "" {
		return fmt.Sprintf("OpenAI Batch API http %d: %s", e.Status, e.Message)
	}
	return fmt.Sprintf("OpenAI Batch API http %d (%s): %s", e.Status, e.Code, e.Message)
}

func (c *openAIBatchClient) BuildInput(
	run batchRun,
	attemptNo int,
	items []batchItem,
) ([]byte, string, string, error) {
	if len(items) == 0 {
		return nil, "", "", errors.New("junkpurge Batch input has no items")
	}
	if len(items) > batchMaxRequests {
		return nil, "", "", fmt.Errorf(
			"junkpurge Batch input has %d requests; maximum is %d",
			len(items), batchMaxRequests,
		)
	}

	type inputLine struct {
		CustomID string         `json:"custom_id"`
		Method   string         `json:"method"`
		URL      string         `json:"url"`
		Body     map[string]any `json:"body"`
	}
	var payload bytes.Buffer
	for _, item := range items {
		line, err := json.Marshal(inputLine{
			CustomID: item.CustomID,
			Method:   http.MethodPost,
			URL:      run.Endpoint,
			Body: buildChatRequestWithPolicy(
				run.Model,
				item.TorrentName,
				run.SystemPrompt,
				run.MaxTokens,
				run.ReasoningEffort,
			),
		})
		if err != nil {
			return nil, "", "", fmt.Errorf(
				"junkpurge Batch marshal %s: %w", item.CustomID, err,
			)
		}
		if payload.Len()+len(line)+1 > batchMaxInputBytes {
			return nil, "", "", fmt.Errorf(
				"junkpurge Batch input exceeds %d bytes", batchMaxInputBytes,
			)
		}
		_, _ = payload.Write(line)
		_ = payload.WriteByte('\n')
	}

	raw := payload.Bytes()
	sum := sha256.Sum256(raw)
	digest := hex.EncodeToString(sum[:])
	filename := fmt.Sprintf(
		"bitagent-junkpurge-run-%020d-attempt-%03d-%s.jsonl",
		run.ID, attemptNo, digest[:16],
	)
	return append([]byte(nil), raw...), filename, digest, nil
}

type providerFile struct {
	ID        string `json:"id"`
	Bytes     int64  `json:"bytes"`
	CreatedAt int64  `json:"created_at"`
	Filename  string `json:"filename"`
	Purpose   string `json:"purpose"`
}

type providerFileList struct {
	Data    []providerFile `json:"data"`
	HasMore bool           `json:"has_more"`
	LastID  string         `json:"last_id"`
}

func (c *openAIBatchClient) FindInputFile(
	ctx context.Context,
	filename, expectedSHA string,
	expectedBytes int64,
) (batchWorkerFile, bool, error) {
	var matches []batchWorkerFile
	after := ""
	for {
		query := url.Values{
			"purpose": {"batch"},
			"limit":   {"10000"},
			"order":   {"desc"},
		}
		if after != "" {
			query.Set("after", after)
		}
		raw, err := c.do(ctx, http.MethodGet, "/files?"+query.Encode(), nil, "", true)
		if err != nil {
			return batchWorkerFile{}, false, err
		}
		var page providerFileList
		if err := json.Unmarshal(raw, &page); err != nil {
			return batchWorkerFile{}, false, fmt.Errorf("decode file list: %w", err)
		}
		for _, file := range page.Data {
			if file.Filename != filename || file.Bytes != expectedBytes ||
				file.Purpose != "batch" {
				continue
			}
			content, err := c.DownloadFile(ctx, file.ID)
			if err != nil {
				return batchWorkerFile{}, false, fmt.Errorf(
					"verify candidate input file %s: %w", file.ID, err,
				)
			}
			sum := sha256.Sum256(content)
			if hex.EncodeToString(sum[:]) == expectedSHA {
				matches = append(matches, batchWorkerFile{
					ID: file.ID, Filename: file.Filename, Bytes: file.Bytes,
				})
			}
		}
		if !page.HasMore {
			break
		}
		if page.LastID == "" || page.LastID == after {
			return batchWorkerFile{}, false, errors.New(
				"OpenAI file pagination did not advance",
			)
		}
		after = page.LastID
	}
	switch len(matches) {
	case 0:
		return batchWorkerFile{}, false, nil
	case 1:
		return matches[0], true, nil
	default:
		return batchWorkerFile{}, false, fmt.Errorf(
			"ambiguous OpenAI upload recovery: %d files match %s/%s",
			len(matches), filename, expectedSHA,
		)
	}
}

func (c *openAIBatchClient) UploadInputFile(
	ctx context.Context,
	filename string,
	payload []byte,
) (batchWorkerFile, error) {
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	if err := writer.WriteField("purpose", "batch"); err != nil {
		return batchWorkerFile{}, err
	}
	part, err := writer.CreateFormFile("file", filename)
	if err != nil {
		return batchWorkerFile{}, err
	}
	if _, err := part.Write(payload); err != nil {
		return batchWorkerFile{}, err
	}
	if err := writer.Close(); err != nil {
		return batchWorkerFile{}, err
	}

	raw, err := c.do(
		ctx, http.MethodPost, "/files", body.Bytes(), writer.FormDataContentType(), false,
	)
	if err != nil {
		return batchWorkerFile{}, err
	}
	var file providerFile
	if err := json.Unmarshal(raw, &file); err != nil {
		return batchWorkerFile{}, fmt.Errorf("decode uploaded file: %w", err)
	}
	if file.ID == "" {
		return batchWorkerFile{}, errors.New("uploaded file response has no id")
	}
	return batchWorkerFile{ID: file.ID, Filename: file.Filename, Bytes: file.Bytes}, nil
}

type providerBatch struct {
	ID           string            `json:"id"`
	Status       string            `json:"status"`
	InputFileID  string            `json:"input_file_id"`
	OutputFileID string            `json:"output_file_id"`
	ErrorFileID  string            `json:"error_file_id"`
	Endpoint     string            `json:"endpoint"`
	CreatedAt    int64             `json:"created_at"`
	Metadata     map[string]string `json:"metadata"`
	RequestCount struct {
		Total     int `json:"total"`
		Completed int `json:"completed"`
		Failed    int `json:"failed"`
	} `json:"request_counts"`
}

type providerBatchList struct {
	Data    []providerBatch `json:"data"`
	HasMore bool            `json:"has_more"`
	LastID  string          `json:"last_id"`
}

func (c *openAIBatchClient) FindBatch(
	ctx context.Context,
	inputFileID string,
	endpoint string,
	metadata map[string]string,
) (batchWorkerJob, bool, error) {
	var matches []batchWorkerJob
	after := ""
	for {
		query := url.Values{
			"limit": {"100"},
		}
		if after != "" {
			query.Set("after", after)
		}
		raw, err := c.do(ctx, http.MethodGet, "/batches?"+query.Encode(), nil, "", true)
		if err != nil {
			return batchWorkerJob{}, false, err
		}
		var page providerBatchList
		if err := json.Unmarshal(raw, &page); err != nil {
			return batchWorkerJob{}, false, fmt.Errorf("decode Batch list: %w", err)
		}
		for _, batch := range page.Data {
			if batch.InputFileID == inputFileID &&
				batch.Endpoint == endpoint &&
				metadataContains(batch.Metadata, metadata) {
				matches = append(matches, toBatchWorkerJob(batch))
			}
		}
		if !page.HasMore {
			break
		}
		if page.LastID == "" || page.LastID == after {
			return batchWorkerJob{}, false, errors.New(
				"OpenAI Batch pagination did not advance",
			)
		}
		after = page.LastID
	}
	switch len(matches) {
	case 0:
		return batchWorkerJob{}, false, nil
	case 1:
		return matches[0], true, nil
	default:
		return batchWorkerJob{}, false, fmt.Errorf(
			"ambiguous OpenAI Batch recovery: %d jobs match input file %s",
			len(matches), inputFileID,
		)
	}
}

func metadataContains(got, want map[string]string) bool {
	for key, value := range want {
		if got[key] != value {
			return false
		}
	}
	return true
}

func (c *openAIBatchClient) CreateBatch(
	ctx context.Context,
	create batchWorkerCreate,
) (batchWorkerJob, error) {
	body, err := json.Marshal(map[string]any{
		"input_file_id":     create.InputFileID,
		"endpoint":          create.Endpoint,
		"completion_window": create.CompletionWindow,
		"metadata":          create.Metadata,
	})
	if err != nil {
		return batchWorkerJob{}, err
	}
	raw, err := c.do(ctx, http.MethodPost, "/batches", body, "application/json", false)
	if err != nil {
		return batchWorkerJob{}, err
	}
	var batch providerBatch
	if err := json.Unmarshal(raw, &batch); err != nil {
		return batchWorkerJob{}, fmt.Errorf("decode created Batch: %w", err)
	}
	if batch.ID == "" {
		// The POST boundary is ambiguous: it may have succeeded upstream even
		// though a proxy returned a partial 2xx body. Returning an error keeps
		// the durable attempt in "submitting", so reconciliation remains
		// list-only during the ambiguity window instead of persisting an
		// unusable ID or issuing an immediate duplicate paid job.
		return batchWorkerJob{}, errors.New(
			"created Batch response has no id",
		)
	}
	if batch.Status == "" {
		return batchWorkerJob{}, errors.New(
			"created Batch response has no status",
		)
	}
	if batch.InputFileID != "" && batch.InputFileID != create.InputFileID {
		return batchWorkerJob{}, fmt.Errorf(
			"created Batch input file is %q, want %q",
			batch.InputFileID, create.InputFileID,
		)
	}
	if batch.Endpoint != "" && batch.Endpoint != create.Endpoint {
		return batchWorkerJob{}, fmt.Errorf(
			"created Batch endpoint is %q, want %q",
			batch.Endpoint, create.Endpoint,
		)
	}
	return toBatchWorkerJob(batch), nil
}

func (c *openAIBatchClient) RetrieveBatch(
	ctx context.Context,
	id string,
) (batchWorkerJob, error) {
	raw, err := c.do(
		ctx, http.MethodGet, "/batches/"+url.PathEscape(id), nil, "", true,
	)
	if err != nil {
		return batchWorkerJob{}, err
	}
	var batch providerBatch
	if err := json.Unmarshal(raw, &batch); err != nil {
		return batchWorkerJob{}, fmt.Errorf("decode retrieved Batch: %w", err)
	}
	return toBatchWorkerJob(batch), nil
}

func toBatchWorkerJob(batch providerBatch) batchWorkerJob {
	var created time.Time
	if batch.CreatedAt > 0 {
		created = time.Unix(batch.CreatedAt, 0)
	}
	return batchWorkerJob{
		ID:               batch.ID,
		Status:           batch.Status,
		InputFileID:      batch.InputFileID,
		OutputFileID:     batch.OutputFileID,
		ErrorFileID:      batch.ErrorFileID,
		RequestTotal:     batch.RequestCount.Total,
		RequestCompleted: batch.RequestCount.Completed,
		RequestFailed:    batch.RequestCount.Failed,
		CreatedAt:        created,
		Metadata:         batch.Metadata,
	}
}

func (c *openAIBatchClient) DownloadFile(
	ctx context.Context,
	id string,
) ([]byte, error) {
	return c.do(
		ctx, http.MethodGet, "/files/"+url.PathEscape(id)+"/content",
		nil, "", true,
	)
}

func (c *openAIBatchClient) DeleteFile(ctx context.Context, id string) error {
	_, err := c.do(
		ctx, http.MethodDelete, "/files/"+url.PathEscape(id), nil, "", true,
	)
	var apiErr *providerAPIError
	if errors.As(err, &apiErr) && apiErr.Status == http.StatusNotFound {
		return nil
	}
	return err
}

type providerBatchLine struct {
	ID       string `json:"id"`
	CustomID string `json:"custom_id"`
	Response *struct {
		StatusCode int             `json:"status_code"`
		RequestID  string          `json:"request_id"`
		Body       json.RawMessage `json:"body"`
	} `json:"response"`
	Error *struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

func (c *openAIBatchClient) ParseResults(
	output, errorOutput []byte,
) ([]batchItemResult, error) {
	results, err := parseBatchJSONL(output)
	if err != nil {
		return nil, fmt.Errorf("output JSONL: %w", err)
	}
	failures, err := parseBatchJSONL(errorOutput)
	if err != nil {
		return nil, fmt.Errorf("error JSONL: %w", err)
	}
	return append(results, failures...), nil
}

func parseBatchJSONL(raw []byte) ([]batchItemResult, error) {
	if len(bytes.TrimSpace(raw)) == 0 {
		return nil, nil
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	var results []batchItemResult
	for {
		var line providerBatchLine
		if err := decoder.Decode(&line); err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return nil, err
		}
		if line.CustomID == "" {
			return nil, errors.New("provider result has empty custom_id")
		}
		result := batchItemResult{
			CustomID:        line.CustomID,
			ProviderRequest: line.ID,
		}
		if line.Response != nil {
			result.ResponseStatus = line.Response.StatusCode
			if line.Response.RequestID != "" {
				result.ProviderRequest = line.Response.RequestID
			}
		}

		if line.Error != nil {
			result.ErrorCode = line.Error.Code
			result.ErrorMessage = line.Error.Message
			result.Retryable = retryableBatchFailure(
				result.ResponseStatus, result.ErrorCode,
			)
			results = append(results, result)
			continue
		}
		if line.Response == nil {
			result.ErrorCode = "missing_response"
			result.ErrorMessage = "provider result has neither response nor error"
			results = append(results, result)
			continue
		}
		if line.Response.StatusCode < 200 || line.Response.StatusCode >= 300 {
			code, message := responseBodyError(line.Response.Body)
			result.ErrorCode = code
			result.ErrorMessage = message
			result.Retryable = retryableBatchFailure(
				line.Response.StatusCode, code,
			)
			results = append(results, result)
			continue
		}
		judgment, usage, err := parseChatJudgment(line.Response.Body)
		result.Usage = usage
		if err != nil {
			result.ErrorCode = "invalid_response"
			result.ErrorMessage = truncate(err.Error(), 1_000)
			// A completed request with a malformed model reply should retry
			// only this item. Treating it as terminal would fail the logical
			// run and later rebill every otherwise-successful title.
			result.Retryable = true
			results = append(results, result)
			continue
		}
		result.Succeeded = true
		result.Judgment = judgment
		results = append(results, result)
	}
	return results, nil
}

func responseBodyError(raw json.RawMessage) (string, string) {
	var body struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
			Type    string `json:"type"`
		} `json:"error"`
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		return "", truncate(string(raw), 1_000)
	}
	code := body.Error.Code
	if code == "" {
		code = body.Error.Type
	}
	return code, body.Error.Message
}

func retryableBatchFailure(status int, code string) bool {
	switch status {
	case http.StatusRequestTimeout, http.StatusConflict,
		http.StatusTooEarly, http.StatusTooManyRequests:
		return true
	}
	if status >= 500 {
		return true
	}
	switch strings.ToLower(code) {
	case "batch_expired", "rate_limit_exceeded", "server_error",
		"internal_error", "timeout":
		return true
	default:
		return false
	}
}

func (c *openAIBatchClient) do(
	ctx context.Context,
	method, path string,
	body []byte,
	contentType string,
	retrySafe bool,
) ([]byte, error) {
	attempts := 1
	if retrySafe {
		attempts = 3
	}
	var lastErr error
	for attempt := 0; attempt < attempts; attempt++ {
		if attempt > 0 {
			delay := time.Duration(attempt*attempt) * 250 * time.Millisecond
			timer := time.NewTimer(delay)
			select {
			case <-ctx.Done():
				timer.Stop()
				return nil, ctx.Err()
			case <-timer.C:
			}
		}
		req, err := http.NewRequestWithContext(
			ctx, method, c.baseURL+path, bytes.NewReader(body),
		)
		if err != nil {
			return nil, err
		}
		if contentType != "" {
			req.Header.Set("Content-Type", contentType)
		}
		if c.apiKey != "" {
			req.Header.Set("Authorization", "Bearer "+c.apiKey)
		}
		req.Header.Set("User-Agent", "bitagent-junkpurge-batch/1")

		resp, err := c.http.Do(req)
		if err != nil {
			lastErr = err
			if retrySafe {
				continue
			}
			return nil, err
		}
		raw, readErr := readLimited(resp.Body, batchMaxReadBytes)
		_ = resp.Body.Close()
		if readErr != nil {
			lastErr = readErr
			if retrySafe {
				continue
			}
			return nil, readErr
		}
		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			return raw, nil
		}
		apiErr := decodeProviderAPIError(resp.StatusCode, raw)
		lastErr = apiErr
		if !retrySafe || !retryableBatchFailure(resp.StatusCode, apiErr.Code) {
			return nil, apiErr
		}
	}
	return nil, lastErr
}

func readLimited(reader io.Reader, max int64) ([]byte, error) {
	raw, err := io.ReadAll(io.LimitReader(reader, max+1))
	if err != nil {
		return nil, err
	}
	if int64(len(raw)) > max {
		return nil, fmt.Errorf("OpenAI Batch response exceeds %s bytes",
			strconv.FormatInt(max, 10))
	}
	return raw, nil
}

func decodeProviderAPIError(status int, raw []byte) *providerAPIError {
	var envelope struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
			Type    string `json:"type"`
		} `json:"error"`
	}
	_ = json.Unmarshal(raw, &envelope)
	code := envelope.Error.Code
	if code == "" {
		code = envelope.Error.Type
	}
	message := envelope.Error.Message
	if message == "" {
		message = truncate(string(raw), 1_000)
	}
	return &providerAPIError{Status: status, Code: code, Message: message}
}

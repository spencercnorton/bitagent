package junkpurge

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/spencercnorton/bitagent/internal/classifier/contentfilter"
	"github.com/spencercnorton/bitagent/internal/llmcapture"
)

const junkBatchCaptureContractID = "junkpurge-batch-chat-request-v1"

var errBatchCapturePrivacyBlocked = errors.New(
	"junkpurge Batch evaluation capture: privacy blocked",
)

type capturedBatchLine struct {
	CustomID string          `json:"custom_id"`
	Method   string          `json:"method"`
	URL      string          `json:"url"`
	Body     json.RawMessage `json:"body"`
}

func (w *purgeWorker) captureBatchAttempt(
	ctx context.Context,
	pool *pgxpool.Pool,
	attempt batchAttempt,
) error {
	if w.capture == nil || !w.capture.Enabled() {
		return nil
	}
	run, err := loadCaptureBatchRun(ctx, pool, attempt.RunID)
	if err != nil {
		return fmt.Errorf("load run: %w", err)
	}
	items, err := loadAttemptItems(ctx, pool, attempt.ID)
	if err != nil {
		return fmt.Errorf("load items: %w", err)
	}
	requests, err := buildBatchCaptureRequests(run, attempt, items)
	if err != nil {
		return err
	}
	for _, request := range requests {
		outcome, err := w.capture.Capture(ctx, request)
		if errors.Is(err, llmcapture.ErrPrivacyBlocked) {
			return errBatchCapturePrivacyBlocked
		}
		if err != nil {
			return fmt.Errorf(
				"%w: %v",
				llmcapture.ErrCaptureUnavailable,
				err,
			)
		}
		if outcome != llmcapture.OutcomeRecorded &&
			outcome != llmcapture.OutcomeDuplicate {
			return fmt.Errorf(
				"%w: enabled junk capture returned outcome %q",
				llmcapture.ErrCaptureUnavailable,
				outcome,
			)
		}
	}
	return nil
}

func buildBatchCaptureRequests(
	run batchRun,
	attempt batchAttempt,
	items []batchItem,
) ([]llmcapture.Request, error) {
	if len(items) == 0 || len(items) != attempt.ItemCount {
		return nil, fmt.Errorf(
			"junkpurge Batch evaluation capture: got %d items, want %d",
			len(items),
			attempt.ItemCount,
		)
	}
	lines, err := parseBatchCapturePayload(attempt.Payload)
	if err != nil {
		return nil, err
	}
	if len(lines) != len(items) {
		return nil, fmt.Errorf(
			"junkpurge Batch evaluation capture: got %d payload lines, want %d",
			len(lines),
			len(items),
		)
	}
	endpoint, err := hostedBatchEndpoint(run.ProviderBaseURL, run.Endpoint)
	if err != nil {
		return nil, err
	}
	requests := make([]llmcapture.Request, 0, len(items))
	for index, item := range items {
		line := lines[index]
		if line.CustomID != item.CustomID ||
			line.Method != http.MethodPost ||
			line.URL != run.Endpoint {
			return nil, fmt.Errorf(
				"junkpurge Batch evaluation capture: payload line %d does not match its durable item",
				index,
			)
		}
		expectedBody, err := json.Marshal(buildChatRequestWithPolicy(
			run.Model,
			item.TorrentName,
			run.SystemPrompt,
			run.MaxTokens,
			run.ReasoningEffort,
		))
		if err != nil {
			return nil, fmt.Errorf(
				"junkpurge Batch evaluation capture: build expected body: %w",
				err,
			)
		}
		if !bytes.Equal(line.Body, expectedBody) {
			return nil, fmt.Errorf(
				"junkpurge Batch evaluation capture: payload body %d does not match its durable run contract",
				index,
			)
		}
		taskInput, err := json.Marshal(map[string]string{
			"torrent_name": item.TorrentName,
		})
		if err != nil {
			return nil, fmt.Errorf(
				"junkpurge Batch evaluation capture: marshal task input: %w",
				err,
			)
		}
		groupKey := []byte(
			contentfilter.EvaluationGroupKey(item.TorrentName),
		)
		requests = append(requests, llmcapture.Request{
			Task:           llmcapture.TaskJunkPurge,
			InfoHash:       item.InfoHash,
			GroupKey:       groupKey,
			Model:          run.Model,
			Endpoint:       endpoint,
			PromptVersion:  run.PromptVersion,
			SystemPrompt:   run.SystemPrompt,
			ModelInputJSON: line.Body,
			TaskInputJSON:  taskInput,
			BuildIdentity:  llmcapture.CurrentBuildIdentity(),
			ContractID:     junkBatchCaptureContractID,
		})
	}
	return requests, nil
}

func (w *purgeWorker) captureSyncCandidate(
	ctx context.Context,
	item candidate,
) error {
	return w.captureSyncCandidateWithOrigin(
		ctx,
		item,
		llmcapture.SamplingOriginNaturalCapture,
	)
}

func (w *purgeWorker) captureSyncCandidateWithOrigin(
	ctx context.Context,
	item candidate,
	origin llmcapture.SamplingOrigin,
) error {
	if w.capture == nil || !w.capture.Enabled() ||
		w.cfg.LLMApiStyle != "chat" {
		return nil
	}
	body, err := json.Marshal(buildChatRequestForRoute(
		w.cfg.LLMModel,
		item.name,
		w.cfg.LLMOpenaiDataSharing,
	))
	if err != nil {
		return fmt.Errorf(
			"%w: marshal hosted synchronous junk request: %v",
			llmcapture.ErrCaptureUnavailable,
			err,
		)
	}
	taskInputObject := map[string]any{"torrent_name": item.name}
	if w.cfg.LLMOpenaiDataSharing {
		taskInputObject["openai_data_sharing"] = true
	}
	taskInput, err := json.Marshal(taskInputObject)
	if err != nil {
		return fmt.Errorf(
			"%w: marshal hosted synchronous junk task input: %v",
			llmcapture.ErrCaptureUnavailable,
			err,
		)
	}
	outcome, err := w.capture.Capture(ctx, llmcapture.Request{
		Task:           llmcapture.TaskJunkPurge,
		SamplingOrigin: origin,
		InfoHash:       item.infoHash,
		GroupKey:       []byte(contentfilter.EvaluationGroupKey(item.name)),
		Model:          w.cfg.LLMModel,
		Endpoint:       strings.TrimRight(w.cfg.LLMBaseURL, "/") + "/chat/completions",
		PromptVersion:  judgePromptVersion,
		SystemPrompt:   junkSyncSystemPrompt(w.cfg.LLMOpenaiDataSharing),
		ModelInputJSON: body,
		TaskInputJSON:  taskInput,
		BuildIdentity:  llmcapture.CurrentBuildIdentity(),
		ContractID:     junkSyncContractID(w.cfg.LLMOpenaiDataSharing),
	})
	if err != nil {
		return err
	}
	if outcome != llmcapture.OutcomeRecorded &&
		outcome != llmcapture.OutcomeDuplicate {
		return fmt.Errorf(
			"%w: enabled hosted synchronous junk capture returned outcome %q",
			llmcapture.ErrCaptureUnavailable,
			outcome,
		)
	}
	return nil
}

func junkSyncSystemPrompt(openAIDataSharing bool) string {
	if openAIDataSharing {
		return judgeInstructions
	}
	return judgeInstructions + "\n/no_think"
}

func junkSyncContractID(openAIDataSharing bool) string {
	if openAIDataSharing {
		return "junkpurge-sync-chat-request-v2-openai-data-sharing"
	}
	return "junkpurge-sync-chat-request-v1"
}

func parseBatchCapturePayload(payload []byte) ([]capturedBatchLine, error) {
	scanner := bufio.NewScanner(bytes.NewReader(payload))
	scanner.Buffer(make([]byte, 64*1024), batchMaxInputBytes)
	var lines []capturedBatchLine
	for scanner.Scan() {
		var line capturedBatchLine
		decoder := json.NewDecoder(strings.NewReader(scanner.Text()))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&line); err != nil {
			return nil, fmt.Errorf(
				"junkpurge Batch evaluation capture: decode payload line: %w",
				err,
			)
		}
		if line.CustomID == "" || line.Method == "" ||
			line.URL == "" || len(line.Body) == 0 {
			return nil, errors.New(
				"junkpurge Batch evaluation capture: incomplete payload line",
			)
		}
		lines = append(lines, line)
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf(
			"junkpurge Batch evaluation capture: scan payload: %w",
			err,
		)
	}
	return lines, nil
}

func hostedBatchEndpoint(baseURL, endpoint string) (string, error) {
	base, err := url.Parse(strings.TrimSpace(baseURL))
	if err != nil || base.Scheme == "" || base.Host == "" {
		return "", errors.New(
			"junkpurge Batch evaluation capture: invalid provider base URL",
		)
	}
	target, err := url.Parse(endpoint)
	if err != nil || !strings.HasPrefix(target.Path, "/") ||
		target.Scheme != "" || target.Host != "" ||
		target.RawQuery != "" || target.Fragment != "" {
		return "", errors.New(
			"junkpurge Batch evaluation capture: invalid provider endpoint",
		)
	}
	return base.Scheme + "://" + base.Host + target.Path, nil
}

func loadCaptureBatchRun(
	ctx context.Context,
	pool *pgxpool.Pool,
	runID int64,
) (batchRun, error) {
	var run batchRun
	err := pool.QueryRow(ctx, `
SELECT id, model, provider_base_url, endpoint, prompt_version, system_prompt,
       max_completion_tokens, reasoning_effort
FROM junkpurge_batch_runs
WHERE id=$1`, runID).Scan(
		&run.ID,
		&run.Model,
		&run.ProviderBaseURL,
		&run.Endpoint,
		&run.PromptVersion,
		&run.SystemPrompt,
		&run.MaxTokens,
		&run.ReasoningEffort,
	)
	return run, err
}

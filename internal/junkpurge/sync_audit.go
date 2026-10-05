package junkpurge

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/spencercnorton/bitagent/internal/classifier/contentfilter"
	"github.com/spencercnorton/bitagent/internal/llmcapture"
)

const junkAuditedSyncContractID = "junkpurge-sync-chat-request-v3-audited"

// EvaluationParseHTTPJudgments replays the production response parser against
// a retained HTTP body. It supplies model diagnostics, not independent labels
// or approval to quarantine. The caller must also verify its HTTP receipt and
// compare every result with the captured decision and request position.
func EvaluationParseHTTPJudgments(raw []byte, requestSize int) ([]Judgment, error) {
	if requestSize < 1 || requestSize > 50 {
		return nil, fmt.Errorf("junkpurge evaluation: invalid request size")
	}
	if requestSize == 1 {
		judgment, _, err := parseChatJudgment(raw)
		if err != nil {
			return nil, err
		}
		return []Judgment{judgment}, nil
	}
	content, _, err := decodeChatReply(raw)
	if err != nil {
		return nil, err
	}
	return parseBatchJudgments(content, requestSize)
}

type junkSourcesKey struct{}
type junkCallKey struct{}
type junkCallAudit struct {
	sources  []candidate
	keys     [][]byte
	receipts []llmcapture.ResultReceipt
	recorder llmcapture.JunkResultRecorder
}

func withJunkSources(ctx context.Context, sources []candidate) context.Context {
	return context.WithValue(ctx, junkSourcesKey{}, sources)
}

func withJunkCall(ctx context.Context) context.Context {
	sources, _ := ctx.Value(junkSourcesKey{}).([]candidate)
	return context.WithValue(ctx, junkCallKey{}, &junkCallAudit{sources: sources})
}

func (j *ollamaJudge) beginJunkAudit(ctx context.Context, endpoint string, body []byte) error {
	if j.capture == nil && !j.auditRequired {
		return nil
	}
	a, _ := ctx.Value(junkCallKey{}).(*junkCallAudit)
	recorder, ok := j.capture.(llmcapture.JunkResultRecorder)
	if j.capture == nil || !j.capture.Enabled() || !ok || a == nil || len(a.sources) == 0 {
		return fmt.Errorf("%w: enabled whole-request junk audit is required", llmcapture.ErrCaptureUnavailable)
	}
	var envelope struct {
		Messages []struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil || len(envelope.Messages) != 2 || envelope.Messages[0].Role != "system" || envelope.Messages[1].Role != "user" {
		return fmt.Errorf("%w: junk request envelope mismatch", llmcapture.ErrCaptureUnavailable)
	}
	names := make([]string, len(a.sources))
	for i, c := range a.sources {
		names[i] = c.name
	}
	want := names[0]
	if len(names) > 1 {
		want = batchUserContent(names)
	}
	if envelope.Messages[1].Content != want {
		return fmt.Errorf("%w: junk request source mismatch", llmcapture.ErrCaptureUnavailable)
	}
	requests := make([]llmcapture.Request, len(a.sources))
	a.keys = make([][]byte, len(a.sources))
	for i, c := range a.sources {
		input, err := json.Marshal(map[string]any{"torrent_name": c.name, "request_index": i + 1, "request_size": len(a.sources), "min_confidence": j.minConfidence, "live": j.enablePurge, "openai_data_sharing": j.openAIDataSharing})
		if err != nil {
			return err
		}
		requests[i] = llmcapture.Request{Task: llmcapture.TaskJunkPurge, InfoHash: c.infoHash, GroupKey: []byte(contentfilter.EvaluationGroupKey(c.name)), Model: j.model, Endpoint: endpoint, PromptVersion: judgePromptVersion, SystemPrompt: envelope.Messages[0].Content, ModelInputJSON: body, TaskInputJSON: input, BuildIdentity: llmcapture.CurrentBuildIdentity(), ContractID: junkAuditedSyncContractID}
		a.keys[i], err = llmcapture.KeyForRequest(requests[i])
		if err != nil {
			return err
		}
	}
	if err := recorder.CaptureJunkGroup(ctx, requests); err != nil {
		return err
	}
	a.recorder = recorder
	return recorder.RecheckJunkGroup(ctx, a.keys)
}

func recordJunkResponse(ctx context.Context, body []byte, status int, class string) error {
	a, _ := ctx.Value(junkCallKey{}).(*junkCallAudit)
	if a == nil || a.recorder == nil {
		return nil
	}
	// Cancellation does not erase an already-paid response. doJSON supplies a
	// detached, bounded context to retain the first observation after dispatch.
	var err error
	a.receipts, err = a.recorder.RecordJunkHTTPResult(ctx, a.keys, llmcapture.HTTPResult{Body: body, StatusCode: status, ErrorClass: class})
	return err
}

func (j *ollamaJudge) finishJunkAudit(ctx context.Context, judgments []Judgment) error {
	a, _ := ctx.Value(junkCallKey{}).(*junkCallAudit)
	if a == nil || a.recorder == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	if len(a.receipts) != len(a.sources) {
		return fmt.Errorf("%w: junk response receipt missing", llmcapture.ErrCaptureUnavailable)
	}
	decisions := make([]llmcapture.JunkDecision, len(a.sources))
	hashes := make([][]byte, len(a.sources))
	for i, c := range a.sources {
		hashes[i] = c.infoHash
		d := llmcapture.JunkDecision{Outcome: "invalid_response", MinConfidence: j.minConfidence, Live: j.enablePurge, RequestIndex: i + 1, RequestSize: len(a.sources)}
		if len(judgments) == len(a.sources) {
			d.Outcome = "judged"
			d.Verdict = judgments[i].Verdict
			d.Confidence = judgments[i].Confidence
			d.WouldQuarantine = judgments[i].IsJunk(j.minConfidence)
		}
		decisions[i] = d
	}
	return a.recorder.RecordJunkDecisions(ctx, a.receipts, hashes, decisions)
}

func junkAuditFailure(err error) error {
	return &llmRequestError{reason: "audit_unavailable", unavailable: true, cause: err, detail: "durable junk request/result evidence unavailable"}
}

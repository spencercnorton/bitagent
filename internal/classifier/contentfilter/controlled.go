package contentfilter

import (
	"bytes"
	"context"
	"errors"
	"github.com/spencercnorton/bitagent/internal/llmcapture"
	"github.com/spencercnorton/bitagent/internal/llmwork"
	"time"
)

func (f *Filter) consultControlled(ctx context.Context, in Input, source AuditSource) (DropReason, bool, error) {
	recorder, ok := f.admission.Capture.(llmcapture.ContentFilterResultRecorder)
	client, clientOK := f.llm.(AuditedLLMClient)
	if !ok || !clientOK || f.admission.Capture == nil || !f.admission.Capture.Enabled() {
		return ReasonNone, false, llmcapture.ErrCaptureUnavailable
	}
	contract, err := f.EvaluationCapture(in)
	if err != nil {
		return ReasonNone, false, err
	}
	request := llmcapture.Request{Task: llmcapture.TaskContentFilter, InfoHash: source.InfoHash, GroupKey: source.GroupKey, NativePrivate: in.Private,
		Model: contract.Model, Endpoint: contract.Endpoint, PromptVersion: contract.PromptVersion, SystemPrompt: contract.SystemPrompt,
		ModelInputJSON: contract.ModelInputJSON, TaskInputJSON: contract.TaskInputJSON, BuildIdentity: llmcapture.CurrentBuildIdentity(), ContractID: contract.ContractID}
	outcome, err := f.admission.Capture.Capture(ctx, request)
	if err != nil {
		return ReasonNone, false, err
	}
	key, err := llmcapture.KeyForRequest(request)
	if err != nil {
		return ReasonNone, false, err
	}
	semantic, err := llmcapture.SemanticKeyForRequest(request)
	if err != nil {
		return ReasonNone, false, err
	}
	dreq := llmcapture.DispatchRequest{CaptureKey: key, SemanticKey: semantic, Task: request.Task, InfoHash: source.InfoHash, FreshCapture: outcome == llmcapture.OutcomeRecorded, Case: llmwork.CaseFence(ctx), LeaseDuration: time.Minute}
	lease, state, err := f.admission.Dispatch.Prepare(ctx, dreq)
	if err != nil {
		return ReasonNone, false, controlledFilterError(ctx, err)
	}
	var verdict LLMVerdict
	var result llmcapture.HTTPResult
	var receipt llmcapture.ResultReceipt
	var callErr error
	if state == llmcapture.DispatchReplay {
		if err := llmwork.BeforeDispatch(ctx); err != nil {
			return ReasonNone, false, err
		}
		r, err := f.admission.Dispatch.Replay(ctx, dreq)
		if err != nil {
			return ReasonNone, false, err
		}
		result, receipt = r.Result, r.Receipt
		verdict, callErr = EvaluationParseHTTPVerdict(result.Body, contract.ContractID)
		if f.llmCb.OnCacheHit != nil {
			f.llmCb.OnCacheHit()
		}
	} else {
		if state != llmcapture.DispatchPrepared || llmwork.ReplayOnly(ctx) {
			return ReasonNone, false, llmwork.ErrHeld
		}
		if !bytes.Equal(lease.CaptureKey, key) {
			return ReasonNone, false, llmwork.ErrHeld
		}
		select {
		case f.slots <- struct{}{}:
			defer func() { <-f.slots }()
		default:
			retry := time.Now().UTC().Add(time.Second)
			if err = f.admission.Dispatch.DeferNoDispatch(ctx, lease, "concurrency", retry); err != nil {
				return ReasonNone, false, err
			}
			return ReasonNone, false, llmwork.RecordDeferral(ctx, "concurrency", retry)
		}
		allowed, err := f.admission.Dispatch.Reserve(ctx, lease, "contentfilter", f.cfg.LLMDailyBudget, f.cfg.LLMMonthlyBudget)
		if err != nil {
			return ReasonNone, false, controlledFilterError(ctx, err)
		}
		if !allowed {
			return ReasonNone, false, llmwork.ErrHeld
		}
		if err = recorder.RecheckContentFilterRequest(ctx, key, source.InfoHash); err != nil {
			return ReasonNone, false, err
		}
		if err = llmwork.BeforeDispatch(ctx); err != nil {
			_ = f.admission.Dispatch.DeferNoDispatch(ctx, lease, "source_changed", time.Now().UTC())
			return ReasonNone, false, err
		}
		if err = f.admission.Dispatch.BeginDispatch(ctx, lease); err != nil {
			return ReasonNone, false, err
		}
		timeout, _ := time.ParseDuration(f.cfg.LLMTimeout)
		if timeout <= 0 {
			timeout = 8 * time.Second
		}
		httpCtx, cancel := context.WithTimeout(ctx, timeout)
		started := time.Now()
		verdict, result, callErr = client.ClassifyWithResult(httpCtx, in.Title)
		cancel()
		f.observeLLMCall(verdict, callErr, time.Since(started))
		cleanup, done := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		receipt, err = recorder.RecordHTTPResult(cleanup, key, result)
		if err == nil {
			err = f.admission.Dispatch.ObserveResult(cleanup, lease, receipt)
		}
		done()
		if err != nil {
			return ReasonNone, false, err
		}
	}
	invalid := callErr != nil || result.StatusCode != 200 || result.ErrorClass != "none" || !validLLMVerdict(verdict)
	policy := contentFilterAuditDecisionForAction(verdict, f.cfg.LLMMinConfidenceForDrop, f.cfg.LLMEnforcementEnabled(), invalid, f.cfg.EffectiveLLMAction())
	cleanup, done := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer done()
	if err = recorder.RecordContentFilterDecision(cleanup, receipt, source.InfoHash, policy); err != nil {
		return ReasonNone, false, err
	}
	llmcapture.ResultTraceFrom(ctx).RecordResult(llmcapture.TaskContentFilter, llmcapture.CandidateSourceNone, receipt)
	if invalid {
		return ReasonNone, false, llmwork.ErrHeld
	}
	return f.verdictToReason(verdict), false, nil
}

func controlledFilterError(ctx context.Context, err error) error {
	var d *llmcapture.DispatchDeferredError
	if errors.As(err, &d) {
		return llmwork.RecordDeferral(ctx, d.Reason, d.RetryAfterUTC)
	}
	return err
}

package llmmatch

import (
	"bytes"
	"context"
	"errors"
	"github.com/spencercnorton/bitagent/internal/model"
	"github.com/spencercnorton/bitagent/internal/namepolicy"
	"net/http"
	"time"

	"github.com/spencercnorton/bitagent/internal/llmcapture"
	"github.com/spencercnorton/bitagent/internal/llmwork"
)

func (c *Client) callControlledWith(ctx context.Context, hc *http.Client, stage string, body []byte) ([]byte, error) {
	p, _ := ctx.Value(pendingCaptureContextKey{}).(*pendingCapture)
	if p == nil || len(p.key) != 32 || len(p.infoHash) != 20 {
		return nil, llmcapture.ErrCaptureUnavailable
	}
	if c.work != nil && c.work.Config().Accepts(llmwork.Matcher) && llmwork.ExecutionFrom(ctx) == nil && !llmwork.ReplayOnly(ctx) {
		return nil, llmwork.ErrHeld
	}
	request := llmcapture.DispatchRequest{CaptureKey: p.key, SemanticKey: p.semanticKey, Task: p.task, CandidateSource: p.source, InfoHash: p.infoHash,
		FreshCapture: p.outcome == llmcapture.OutcomeRecorded, Case: llmwork.CaseFence(ctx), LeaseDuration: time.Minute}
	lease, outcome, err := c.dispatch.Prepare(ctx, request)
	if err != nil {
		return nil, c.controlledError(ctx, err)
	}
	if outcome == llmcapture.DispatchReplay {
		if err := llmwork.BeforeDispatch(ctx); err != nil {
			return nil, err
		}
		r, err := c.dispatch.Replay(ctx, request)
		if err != nil {
			return nil, err
		}
		_, content, err := ReadMatcherChatResponse(r.Result.StatusCode, bytes.NewReader(r.Result.Body))
		if err != nil {
			return nil, err
		}
		llmcapture.ResultTraceFrom(ctx).RecordResult(p.task, p.source, r.Receipt)
		p.key = append([]byte(nil), r.Receipt.CaptureKey...)
		c.metrics.cacheHits.Inc()
		return content, llmwork.RecordProgress(ctx)
	}
	if outcome != llmcapture.DispatchPrepared {
		return nil, llmwork.ErrHeld
	}
	if !bytes.Equal(lease.CaptureKey, p.key) {
		return nil, llmwork.ErrHeld
	}
	if llmwork.ReplayOnly(ctx) {
		return nil, llmwork.ErrHeld
	}
	select {
	case c.slots <- struct{}{}:
		defer func() { <-c.slots }()
	default:
		retry := time.Now().UTC().Add(time.Second)
		if err = c.dispatch.DeferNoDispatch(ctx, lease, "concurrency", retry); err != nil {
			return nil, err
		}
		return nil, llmwork.RecordDeferral(ctx, "concurrency", retry)
	}
	allowed, err := c.dispatch.Reserve(ctx, lease, "matcher", c.cfg.DailyCallLimit, c.cfg.MonthlyCallLimit)
	if err != nil {
		return nil, c.controlledError(ctx, err)
	}
	if !allowed {
		return nil, llmwork.ErrHeld
	}
	if err = llmwork.BeforeDispatch(ctx); err != nil {
		_ = c.dispatch.DeferNoDispatch(ctx, lease, "source_changed", time.Now().UTC())
		return nil, err
	}
	d, admissionErr := c.namePolicy.AdmitContext(ctx, model.Torrent{}.InfoHash, "", "")
	if admissionErr != nil || !d.Eligible {
		c.namePolicy.Observe("model_dispatch", d)
		return nil, namepolicy.ErrExcluded
	}
	if err = c.dispatch.BeginDispatch(ctx, lease); err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.cfg.Endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if c.cfg.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.cfg.APIKey)
	}
	// A controlled route never follows redirects carrying request metadata.
	bounded := *hc
	bounded.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	c.metrics.calls.WithLabelValues(c.cfg.Model, stage).Inc()
	started := time.Now()
	resp, requestErr := bounded.Do(req)
	result := llmcapture.HTTPResult{ErrorClass: "transport"}
	var content []byte
	if requestErr == nil {
		defer resp.Body.Close()
		var err error
		result.Body, content, err = ReadMatcherChatResponse(resp.StatusCode, resp.Body)
		result.StatusCode = resp.StatusCode
		result.ErrorClass = matcherResponseErrorClass(err)
		requestErr = err
	}
	c.metrics.observeHTTP(c.cfg.Model, stage, started, result, requestErr)
	c.recordUsage(stage, result.Body)
	if err = c.recordCapturedResult(ctx, stage, result); err != nil {
		return nil, err
	}
	receipt, ok := llmcapture.ResultTraceFrom(ctx).Result(p.task, p.source)
	if !ok {
		return nil, llmwork.ErrHeld
	}
	cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	if err = c.dispatch.ObserveResult(cleanup, lease, receipt); err != nil {
		return nil, err
	}
	if requestErr != nil {
		return nil, requestErr
	}
	return content, llmwork.RecordProgress(cleanup)
}

func (c *Client) controlledError(ctx context.Context, err error) error {
	var d *llmcapture.DispatchDeferredError
	if errors.As(err, &d) {
		return llmwork.RecordDeferral(ctx, d.Reason, d.RetryAfterUTC)
	}
	return err
}

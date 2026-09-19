package llmmatch

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/spencercnorton/bitagent/internal/llmcapture"
)

type pendingCaptureContextKey struct{}

type pendingCapture struct {
	key    []byte
	task   llmcapture.Task
	source llmcapture.CandidateSource
}

func withPendingCapture(ctx context.Context) context.Context {
	return context.WithValue(ctx, pendingCaptureContextKey{}, &pendingCapture{})
}

func matcherResponseErrorClass(err error) string {
	switch {
	case err == nil:
		return "none"
	case errors.Is(err, ErrMatcherChatHTTPStatus):
		return "http_status"
	case errors.Is(err, ErrMatcherChatResponseRead):
		return "read"
	case errors.Is(err, ErrMatcherChatNoChoices):
		return "empty_choices"
	default:
		return "envelope"
	}
}

func (c *Client) recordCapturedResult(ctx context.Context, stage string, result llmcapture.HTTPResult) error {
	pending, _ := ctx.Value(pendingCaptureContextKey{}).(*pendingCapture)
	if pending == nil || len(pending.key) == 0 || c.capture == nil || !c.capture.Enabled() {
		return nil
	}
	recorder, ok := c.capture.(llmcapture.ResultRecorder)
	if !ok {
		c.metrics.auditResults.WithLabelValues(stage, "unavailable").Inc()
		return fmt.Errorf("%w: matcher result recorder is required", llmcapture.ErrCaptureUnavailable)
	}
	// Preserve a bounded terminal result during request cancellation/shutdown.
	// This context cannot extend model inference or retry the provider request.
	writeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Second)
	defer cancel()
	receipt, err := recorder.RecordHTTPResult(writeCtx, pending.key, result)
	if err != nil {
		c.metrics.auditResults.WithLabelValues(stage, "error").Inc()
		return fmt.Errorf("%w: matcher response evidence: %v", llmcapture.ErrCaptureUnavailable, err)
	}
	status := "duplicate"
	if receipt.FirstObservation {
		status = "recorded"
	}
	c.metrics.auditResults.WithLabelValues(stage, status).Inc()
	llmcapture.ResultTraceFrom(ctx).RecordResult(pending.task, pending.source, receipt)
	return nil
}

// RecordAuditDecision exposes only bounded outcome labels to the classifier
// observer; neither source identity nor model text becomes a metric label.
func (c *Client) RecordAuditDecision(outcome string) {
	if c == nil {
		return
	}
	switch outcome {
	case "recorded", "no_provider_result", "duplicate", "response_error", "error":
	default:
		outcome = "error"
	}
	c.metrics.auditDecisions.WithLabelValues(outcome).Inc()
}

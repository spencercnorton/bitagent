package classifier

import (
	"context"
	"fmt"
	"time"

	"github.com/spencercnorton/bitagent/internal/classifier/llmmatch"
	"github.com/spencercnorton/bitagent/internal/llmcapture"
)

const matchDecisionWriteTimeout = 2 * time.Second

// NewMatchDecisionObserver builds the bridge from the classifier's full
// in-memory policy verdict to the compact capture-result ledger. A nil or
// disabled capture is inert, preserving classifier-only/test graphs. An
// enabled capture must also implement ResultRecorder; production's Recorder
// does, and a miswired capture fails closed before a live attachment.
func NewMatchDecisionObserver(
	capture llmcapture.Capturer,
	matcher *llmmatch.Client,
) MatchDecisionObserver {
	observer := &captureMatchDecisionObserver{matcher: matcher}
	if capture == nil || !capture.Enabled() {
		return observer
	}
	observer.enabled = true
	observer.recorder, observer.hasRecorder = capture.(llmcapture.ResultRecorder)
	return observer
}

type captureMatchDecisionObserver struct {
	enabled     bool
	hasRecorder bool
	recorder    llmcapture.ResultRecorder
	matcher     *llmmatch.Client
}

func (o *captureMatchDecisionObserver) ObserveLLMMatchDecision(
	ctx context.Context,
	observation MatchDecisionObservation,
) error {
	if o == nil || !o.enabled {
		return nil
	}
	if !o.hasRecorder {
		o.record("error")
		return fmt.Errorf(
			"%w: enabled matcher capture does not implement result recording",
			llmcapture.ErrCaptureUnavailable,
		)
	}

	source, ok := matchDecisionCandidateSource(observation.CandidateSource)
	if !ok {
		o.record("no_provider_result")
		return nil
	}
	trace := llmcapture.ResultTraceFrom(ctx)
	receipt, ok := trace.Result(llmcapture.TaskMatcherRerank, source)
	if !ok {
		// Terminal decisions before rerank have no provider result. Audited
		// rerank cache hits replay their original receipt; a would-attach
		// verdict without it must not silently bypass audit persistence.
		if observation.WouldAttach {
			o.record("error")
			return fmt.Errorf("%w: matcher attachment has no bound result receipt", llmcapture.ErrCaptureUnavailable)
		}
		o.record("no_provider_result")
		return nil
	}
	if receipt.StatusCode != 200 || receipt.ErrorClass != "none" {
		if observation.WouldAttach {
			o.record("error")
			return fmt.Errorf("%w: failed provider result cannot authorize attachment", llmcapture.ErrCaptureUnavailable)
		}
		o.record("response_error")
		return nil
	}
	// A duplicate may validate an already-written identical decision. The
	// recorder decides whether that is safe; never silently bypass it here,
	// because a cache replay must also be able to recover a failed first
	// decision append.
	duplicate := !receipt.FirstObservation

	decision := llmcapture.MatchDecision{
		Outcome:            string(observation.Outcome),
		GateReason:         observation.GateReason,
		ChosenID:           observation.MatchedID,
		Confidence:         observation.Confidence,
		IsTV:               observation.IsTV,
		ResolvedYear:       observation.ResolvedYear,
		MinConfidence:      observation.MinConfidence,
		RequireSourceTitle: observation.RequireSourceTitle,
		WouldAttach:        observation.WouldAttach,
		Live:               observation.Live,
	}
	writeCtx, cancel := context.WithTimeout(
		context.WithoutCancel(ctx),
		matchDecisionWriteTimeout,
	)
	defer cancel()
	if err := o.recorder.RecordMatchDecision(
		writeCtx,
		receipt,
		append([]byte(nil), observation.InfoHash...),
		decision,
	); err != nil {
		o.record("error")
		return fmt.Errorf("record matcher audit decision: %w", err)
	}
	if duplicate {
		o.record("duplicate")
	} else {
		o.record("recorded")
	}
	return nil
}

func (o *captureMatchDecisionObserver) record(outcome string) {
	if o != nil && o.matcher != nil {
		o.matcher.RecordAuditDecision(outcome)
	}
}

func matchDecisionCandidateSource(source string) (llmcapture.CandidateSource, bool) {
	switch source {
	case string(llmcapture.CandidateSourceLocal):
		return llmcapture.CandidateSourceLocal, true
	case string(llmcapture.CandidateSourceAPI):
		return llmcapture.CandidateSourceAPI, true
	default:
		return llmcapture.CandidateSourceNone, false
	}
}

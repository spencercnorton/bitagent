package contentfilter

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/spencercnorton/bitagent/internal/llmcapture"
	"github.com/spencercnorton/bitagent/internal/llmwork"
)

type WorkPayload struct {
	Input  Input
	Source AuditSource
}

func (f *Filter) SetWork(work *llmwork.Store) { f.work = work }
func (f *Filter) WorkPolicy() any {
	cfg := f.cfg
	cfg.LLMOpenaiApiKey = ""
	return []any{cfg, llmcapture.CurrentBuildIdentity()}
}
func (f *Filter) SubmitWork(ctx context.Context, in Input, source AuditSource) error {
	if f.work == nil || !f.work.Config().Accepts(llmwork.Language) {
		return nil
	}
	t, ok := llmwork.SourceTorrent(ctx)
	if !ok || t.Name != in.Title || t.Private != in.Private {
		return fmt.Errorf("%w: language work requires current source", llmwork.ErrObsolete)
	}
	if f.cfg.EffectiveLLMAction() != LLMActionReview {
		return fmt.Errorf("%w: background language work is review-only", llmwork.ErrObsolete)
	}
	contract, err := f.EvaluationCapture(in)
	if err != nil {
		return err
	}
	return f.work.Submit(ctx, llmwork.Language, t, f.WorkPolicy(), contract.ModelInputJSON, WorkPayload{in, source}, source.GroupKey, f.cfg.LLMDailyBudget, f.cfg.LLMMonthlyBudget)
}
func (f *Filter) DecodeWork(body []byte) (WorkPayload, error) {
	var p WorkPayload
	err := json.Unmarshal(body, &p)
	return p, err
}

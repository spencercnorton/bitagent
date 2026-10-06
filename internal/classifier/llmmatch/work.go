package llmmatch

import (
	"context"
	"github.com/spencercnorton/bitagent/internal/classifier/contentfilter"
	"github.com/spencercnorton/bitagent/internal/llmcapture"
	"github.com/spencercnorton/bitagent/internal/llmwork"
	"github.com/spencercnorton/bitagent/internal/model"
)

type WorkPayload struct{ Type model.NullContentType }
type workTypeKey struct{}

func WithWorkType(ctx context.Context, t model.NullContentType) context.Context {
	return context.WithValue(ctx, workTypeKey{}, WorkPayload{t})
}
func (c *Client) SetWork(work *llmwork.Store, policy any) { c.work, c.workPolicy = work, policy }
func (c *Client) WorkPolicy(p WorkPayload) any {
	cfg := c.cfg
	cfg.APIKey = ""
	cfg.Embeddings.APIKey = ""
	return []any{cfg, c.workPolicy, p, llmcapture.CurrentBuildIdentity()}
}
func (c *Client) submitWork(ctx context.Context, t model.Torrent) error {
	if c.work == nil || !c.work.Config().Accepts(llmwork.Matcher) {
		return nil
	}
	p, _ := ctx.Value(workTypeKey{}).(WorkPayload)
	return c.work.Submit(ctx, llmwork.Matcher, t, c.WorkPolicy(p), c.newChatRequest(ExtractPrompt(), ExtractInput(t.Name, extractionModelFiles(t)), 120), p, contentfilter.EvaluationGroupKey(t.Name), c.cfg.DailyCallLimit, c.cfg.MonthlyCallLimit)
}
func (c *Client) WithDispatchControl(d llmcapture.DispatchControl) *Client { c.dispatch = d; return c }

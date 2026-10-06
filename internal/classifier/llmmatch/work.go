package llmmatch

import (
	"context"
	"github.com/spencercnorton/bitagent/internal/classifier/contentfilter"
	"github.com/spencercnorton/bitagent/internal/llmwork"
	"github.com/spencercnorton/bitagent/internal/model"
)

type WorkPayload struct{ Type model.NullContentType }
type workTypeKey struct{}

// WorkPolicyVersion advances with semantic matching/application changes.
// Capture generations remain build-bound; task continuity alone never grants
// replay or outbound authority.
const WorkPolicyVersion = "matcher-work-policy-v1"

func WithWorkType(ctx context.Context, t model.NullContentType) context.Context {
	return context.WithValue(ctx, workTypeKey{}, WorkPayload{t})
}
func (c *Client) SetWork(work *llmwork.Store, policy any) { c.work, c.workPolicy = work, policy }
func (c *Client) WorkPolicy(p WorkPayload) any {
	cfg := c.cfg
	cfg.APIKey = ""
	cfg.Embeddings.APIKey = ""
	return []any{WorkPolicyVersion, cfg, c.workPolicy, p}
}
func (c *Client) submitWork(ctx context.Context, t model.Torrent) error {
	if c.work == nil || !c.work.Config().Accepts(llmwork.Matcher) {
		return nil
	}
	p, _ := ctx.Value(workTypeKey{}).(WorkPayload)
	return c.work.Submit(ctx, llmwork.Matcher, t, c.WorkPolicy(p), c.newChatRequest(ExtractPrompt(), ExtractInput(t.Name, extractionModelFiles(t)), 120), p, contentfilter.EvaluationGroupKey(t.Name), c.cfg.DailyCallLimit, c.cfg.MonthlyCallLimit)
}

// WorkInputDigest binds the exact extraction request for the current source.
func (c *Client) WorkInputDigest(t model.Torrent) []byte {
	return llmwork.Digest(c.newChatRequest(ExtractPrompt(), ExtractInput(t.Name, extractionModelFiles(t)), 120))
}

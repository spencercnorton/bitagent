package llmstage

import (
	"context"
	"fmt"
	"github.com/spencercnorton/bitagent/internal/namepolicy"

	"github.com/spencercnorton/bitagent/internal/classifier"
	"github.com/spencercnorton/bitagent/internal/classifier/contentfilter"
	"github.com/spencercnorton/bitagent/internal/classifier/llmmatch"
	"github.com/spencercnorton/bitagent/internal/llmcapture"
	"github.com/spencercnorton/bitagent/internal/llmwork"
	"github.com/spencercnorton/bitagent/internal/model"
)

type workRunKey struct{}

// WorkPolicyVersion must advance when application semantics change. The
// evaluation capture retains its build generation independently of this
// semantic task identity, so a binary restart does not buy the same request.
const WorkPolicyVersion = "classifier-type-work-policy-v1"

type WorkPayload struct {
	Workflow string
	Flags    classifier.Flags
}
type DeferredPrediction struct {
	Type       model.NullContentType
	Confidence float64
	Qualified  bool
	Receipt    llmcapture.ResultReceipt
}

// SetWork is startup-only wiring; disabled queues retain inline behavior.
func (s *Stage) SetWork(work *llmwork.Store, policy classifier.Config) {
	s.work, s.workPolicy = work, policy
}
func (s *Stage) WorkPolicy(p WorkPayload) any {
	cfg := s.cfg
	cfg.APIKey = ""
	return namepolicy.BindWorkPolicy(s.namePolicy, []any{WorkPolicyVersion, cfg, s.workPolicy, p})
}

func (s *Stage) submitWork(ctx context.Context, t model.Torrent) error {
	if s.work == nil || !s.work.Config().Accepts(llmwork.Type) {
		return nil
	}
	p, _ := ctx.Value(workRunKey{}).(WorkPayload)
	if p.Workflow == "" {
		p.Workflow = s.workPolicy.Workflow
	}
	return s.work.Submit(ctx, llmwork.Type, t, s.WorkPolicy(p), buildBoundedRequestBody(s.cfg, t), p, contentfilter.EvaluationGroupKey(t.Name), s.cfg.DailyCallLimit, s.cfg.MonthlyCallLimit)
}

// EvaluateDeferred computes a bound type decision only. It neither runs the
// workflow nor changes/deletes source rows. The owned adapter performs policy
// checks and local enrichment before its compare-and-set transaction.
func (s *Stage) EvaluateDeferred(ctx context.Context, t model.Torrent, p WorkPayload) (DeferredPrediction, error) {
	ctx = context.WithValue(ctx, workRunKey{}, p)
	if s.work == nil || !s.work.Enabled() || llmwork.ExecutionFrom(ctx) == nil || !s.cfg.Enabled {
		return DeferredPrediction{}, llmwork.ErrObsolete
	}
	if err := s.submitWork(ctx, t); err != nil {
		return DeferredPrediction{}, err
	}
	if t.Private || !s.plausibleMedia(t) || s.privacy == nil {
		return DeferredPrediction{}, llmwork.ErrObsolete
	}
	private, err := s.privacy.IsPrivateInfoHash(ctx, t.InfoHash.Bytes())
	if err != nil || private {
		return DeferredPrediction{}, llmwork.ErrObsolete
	}
	d, err := s.classify(ctx, t)
	if err != nil {
		return DeferredPrediction{}, err
	}
	if err = s.recordDecision(ctx, t, d, false); err != nil {
		return DeferredPrediction{}, err
	}
	ct, ok := mediaTypeToContentType(d.MediaType)
	qualified := ok && s.cfg.EnableLive && d.Confidence >= s.cfg.MinConfidence && llmcapture.TypeLiveAllowed(string(d.MediaType), s.cfg.LiveAllowedTypes)
	typed := model.NullContentType{}
	if ok {
		typed = model.NewNullContentType(ct)
	}
	return DeferredPrediction{typed, d.Confidence, qualified, d.receipt}, nil
}

func (s *Stage) ValidateWork(p WorkPayload) error {
	if p.Workflow != "default" || s.workPolicy.Workflow != "default" {
		return fmt.Errorf("%w: deferred type requires the standard nondeleting adapter", llmwork.ErrObsolete)
	}
	return nil
}

// WorkInputDigest binds the exact bounded production request.
func (s *Stage) WorkInputDigest(t model.Torrent) []byte {
	return llmwork.Digest(buildBoundedRequestBody(s.cfg, t))
}

// SetMatcherWork allows the outer runner to retain a later committed identity
// before ordinary workflow actions can replace it.
func (s *Stage) SetMatcherWork(c *llmmatch.Client) { s.matcherWork = c }
func (s *Stage) preserveWork(ctx context.Context, t model.Torrent, p WorkPayload) (*llmwork.ApplicationSnapshot, error) {
	if source, ok := llmwork.SourceTorrent(ctx); ok {
		t = source
	}
	if s.matcherWork != nil {
		a, err := s.work.PreservePolicy(ctx, llmwork.Matcher, t, func(a llmwork.ApplicationSnapshot) any {
			return s.matcherWork.WorkPolicy(llmmatch.WorkPayload{Type: a.ContentType})
		})
		if err != nil || a != nil {
			return a, err
		}
	}
	return s.work.Preserve(ctx, llmwork.Type, t, s.WorkPolicy(p))
}

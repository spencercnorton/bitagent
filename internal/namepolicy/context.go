package namepolicy

import (
	"context"
	"github.com/spencercnorton/bitagent/internal/protocol"
)

type sourceKey struct{}
type policyKey struct{}
type admissionKey struct{}

// Admission rechecks a previously acquired source at a real provider boundary.
// Pure name-only endpoints use Evaluate and never install this callback.
type Admission func(context.Context, Source) (Decision, error)

func WithAdmission(ctx context.Context, check Admission) context.Context {
	return context.WithValue(ctx, admissionKey{}, check)
}

func (p *Policy) AdmitContext(ctx context.Context, h protocol.ID, name, contentType string) (Decision, error) {
	d := p.EvaluateContext(ctx, h, name, contentType)
	if !p.Enabled() || !d.Eligible {
		return d, nil
	}
	if check, ok := ctx.Value(admissionKey{}).(Admission); ok {
		s, exists := SourceFrom(ctx)
		if !exists {
			s = Source{h, name, contentType}
		}
		return check(ctx, s)
	}
	return d, nil
}

func WithPolicy(ctx context.Context, p *Policy) context.Context {
	return context.WithValue(ctx, policyKey{}, p)
}
func FromContext(ctx context.Context) *Policy { p, _ := ctx.Value(policyKey{}).(*Policy); return p }

type Source struct {
	InfoHash          protocol.ID
	Name, ContentType string
}

func WithSource(ctx context.Context, h protocol.ID, name, contentType string) context.Context {
	return context.WithValue(ctx, sourceKey{}, Source{h, name, contentType})
}
func SourceFrom(ctx context.Context) (Source, bool) {
	s, ok := ctx.Value(sourceKey{}).(Source)
	return s, ok
}
func (p *Policy) EvaluateContext(ctx context.Context, h protocol.ID, name, contentType string) Decision {
	if s, ok := SourceFrom(ctx); ok {
		return p.EvaluateClassified(s.InfoHash, s.Name, s.ContentType)
	}
	return p.EvaluateClassified(h, name, contentType)
}

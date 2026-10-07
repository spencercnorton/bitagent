package namepolicy

import (
	"context"
	"github.com/spencercnorton/bitagent/internal/protocol"
)

type sourceKey struct{}
type policyKey struct{}

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

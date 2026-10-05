package classifier

import (
	"context"

	"github.com/spencercnorton/bitagent/internal/classifier/classification"
)

// TypeFallback supplies an optional type-only prediction at the workflow's
// explicit policy boundary. The workflow remains responsible for applying its
// exclusion rules to the returned result before allowing persistence.
type TypeFallback func(context.Context, classification.Result) (classification.Result, error)

type typeFallbackContextKey struct{}

// WithTypeFallback installs a callback without coupling the workflow compiler
// to a provider or creating a classifier/LLM-stage dependency cycle.
func WithTypeFallback(ctx context.Context, fallback TypeFallback) context.Context {
	return context.WithValue(ctx, typeFallbackContextKey{}, fallback)
}

// RunTypeFallback invokes the optional callback for unknown, unattached content.
// Custom runners must execute their normal content policy after this boundary.
func RunTypeFallback(ctx context.Context, result classification.Result) (classification.Result, error) {
	if result.ContentType.Valid || result.Content != nil {
		return result, nil
	}
	fallback, ok := ctx.Value(typeFallbackContextKey{}).(TypeFallback)
	if !ok || fallback == nil {
		return result, nil
	}
	return fallback(ctx, result)
}

const typeFallbackName = "type_fallback"

type typeFallbackAction struct{}

func (typeFallbackAction) name() string { return typeFallbackName }

var typeFallbackPayloadSpec = payloadLiteral[string]{
	literal:     typeFallbackName,
	description: "Optionally infer an unknown content type before applying the workflow's content policy",
}

func (typeFallbackAction) compileAction(ctx compilerContext) (action, error) {
	if _, err := typeFallbackPayloadSpec.Unmarshal(ctx); err != nil {
		return action{}, ctx.error(err)
	}
	return action{run: func(ctx executionContext) (classification.Result, error) {
		return RunTypeFallback(ctx.Context, ctx.result)
	}}, nil
}

func (typeFallbackAction) JSONSchema() JSONSchema { return typeFallbackPayloadSpec.JSONSchema() }

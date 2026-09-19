package appfx

import (
	"testing"

	"go.uber.org/fx"
)

// TestGraphValidates resolves the whole dependency graph without
// constructing anything. Every module here wires itself through fx
// groups and named values, which unit tests cannot see — a mistyped
// group tag or a provider missing from a module compiles cleanly and
// only fails at process start, in production.
func TestGraphValidates(t *testing.T) {
	if err := fx.ValidateApp(New()); err != nil {
		t.Fatalf("app graph does not resolve: %v", err)
	}
}

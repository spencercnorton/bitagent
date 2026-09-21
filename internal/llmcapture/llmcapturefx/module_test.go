package llmcapturefx

import (
	"testing"

	"github.com/spencercnorton/bitagent/internal/llmcapture"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

// TestExpiryJanitorIsAWorker pins the janitor to the worker registry: it is
// keyed, constructed disabled, and nothing runs until the registry enables
// and starts it.
func TestExpiryJanitorIsAWorker(t *testing.T) {
	w, err := provideExpiryJanitorWorker(
		llmcapture.NewDefaultConfig(),
		llmcapture.NewPostgresStore(nil),
		zap.NewNop().Sugar(),
	)
	require.NoError(t, err)
	require.Equal(t, "llm_evaluation_capture_janitor", w.Key())
	require.False(t, w.Enabled())
}

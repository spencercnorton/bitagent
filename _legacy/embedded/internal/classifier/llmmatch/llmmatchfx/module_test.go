package llmmatchfx

import (
	"context"
	"testing"

	"github.com/spencercnorton/bitagent/internal/classifier/llmmatch"
	"github.com/spencercnorton/bitagent/internal/llmcapture"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

type embeddingWiringProbe struct{ enabled bool }

func (p embeddingWiringProbe) Enabled() bool { return p.enabled }
func (embeddingWiringProbe) Capture(context.Context, llmcapture.Request) (llmcapture.Outcome, error) {
	return llmcapture.OutcomeRecorded, nil
}
func (embeddingWiringProbe) RecheckEmbeddingRequest(context.Context, []byte, []byte) error {
	return nil
}
func (embeddingWiringProbe) RecordHTTPResult(context.Context, []byte, llmcapture.HTTPResult) (llmcapture.ResultReceipt, error) {
	return llmcapture.ResultReceipt{}, nil
}
func (embeddingWiringProbe) RecordMatchDecision(context.Context, llmcapture.ResultReceipt, []byte, llmcapture.MatchDecision) error {
	return nil
}

func TestEmbeddingWiringRequiresEnabledAuditedCapture(t *testing.T) {
	cfg := llmmatch.NewDefaultConfig()
	cfg.Enabled = true
	cfg.Embeddings.Enabled, cfg.Embeddings.Endpoint, cfg.Embeddings.Model = true, "http://127.0.0.1:1234/v1/embeddings", "synthetic-embedding"
	params := clientParams{Config: cfg, Metrics: llmmatch.NewMetrics(), Logger: zap.NewNop().Sugar()}
	_, err := provideClient(params)
	require.Error(t, err)
	params.Capture = embeddingWiringProbe{enabled: false}
	_, err = provideClient(params)
	require.Error(t, err)
	params.Capture = embeddingWiringProbe{enabled: true}
	client, err := provideClient(params)
	require.NoError(t, err)
	require.NotNil(t, client)
	params.Config.Enabled = false
	params.Capture = nil
	_, err = provideClient(params)
	require.NoError(t, err, "disabled matcher remains inert without capture/database initialization")
}

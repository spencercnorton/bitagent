package batchllmmatchcmd

import (
	"context"
	"flag"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/spencercnorton/bitagent/internal/classifier/llmmatch"
	"github.com/stretchr/testify/require"
	"github.com/urfave/cli/v2"
	"go.uber.org/zap"
)

type budgetProbe struct{ calls atomic.Int32 }

func (b *budgetProbe) Reserve(context.Context, int, int) (bool, error) {
	b.calls.Add(1)
	return true, nil
}

func TestGroupedBacklogRefusesBeforeDependenciesOrProvider(t *testing.T) {
	var providerCalls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		providerCalls.Add(1)
	}))
	defer srv.Close()
	cfg := llmmatch.NewDefaultConfig()
	cfg.Enabled, cfg.Endpoint = true, srv.URL
	budget := &budgetProbe{}
	client := llmmatch.NewClientWithBudget(cfg, nil, llmmatch.NewMetrics(), zap.NewNop().Sugar(), nil, budget)
	// Nil database/processor dependencies prove preflight happens before they
	// can be initialized; the endpoint and allowance independently count calls.
	p := Params{LLMMatch: client}
	flags := flag.NewFlagSet("test", flag.ContinueOnError)
	flags.Int("llmBatchSize", 32, "")
	err := p.action(cli.NewContext(cli.NewApp(), flags, nil))
	require.ErrorContains(t, err, "grouped extraction cannot warm single-item workflow")
	require.Zero(t, providerCalls.Load())
	require.Zero(t, budget.calls.Load())
}

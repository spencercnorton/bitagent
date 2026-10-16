package processor

import (
	"context"
	"crypto/sha256"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/spencercnorton/bitagent/internal/classifier/classification"
	"github.com/spencercnorton/bitagent/internal/classifier/contentfilter"
	"github.com/spencercnorton/bitagent/internal/llmcapture"
	"github.com/spencercnorton/bitagent/internal/model"
	"github.com/spencercnorton/bitagent/internal/protocol"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type processorCaptureProbe struct {
	err      error
	requests []llmcapture.Request
}

func (*processorCaptureProbe) Enabled() bool { return true }

func (p *processorCaptureProbe) Capture(
	_ context.Context,
	req llmcapture.Request,
) (llmcapture.Outcome, error) {
	p.requests = append(p.requests, req)
	return llmcapture.OutcomeRecorded, p.err
}

func (*processorCaptureProbe) RecheckContentFilterRequest(context.Context, []byte, []byte) error {
	return nil
}

func (p *processorCaptureProbe) FindContentFilterReplay(context.Context, []byte, []byte) (llmcapture.ContentFilterReplay, error) {
	return llmcapture.ContentFilterReplay{}, p.err
}

func (*processorCaptureProbe) RecordHTTPResult(_ context.Context, key []byte, result llmcapture.HTTPResult) (llmcapture.ResultReceipt, error) {
	digest := sha256.Sum256(result.Body)
	return llmcapture.ResultReceipt{
		CaptureKey: append([]byte(nil), key...), ResponseSHA256: digest[:],
		FirstObservation: true, StatusCode: result.StatusCode, ErrorClass: result.ErrorClass,
	}, nil
}

func (*processorCaptureProbe) RecordContentFilterDecision(context.Context, llmcapture.ResultReceipt, []byte, llmcapture.ContentFilterDecision) error {
	return nil
}

type processorBudgetProbe struct{}

func (processorBudgetProbe) Reserve(context.Context, int, int) (bool, error) { return true, nil }

func TestProcessCaptureFailurePreventsContentFilterModelCall(t *testing.T) {
	hash := processTestHash(0x46)
	torrent := processTestTorrent(
		hash,
		"Public Residual Title 2026",
		"mkv",
	)
	p, mock, _ := newProcessTestProcessor(
		t,
		[]model.Torrent{torrent},
		processRunnerStub{
			run: func(model.Torrent) (classification.Result, error) {
				return classification.Result{}, nil
			},
		},
	)
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		calls.Add(1)
	}))
	defer server.Close()
	cfg := contentfilter.NewDefaultConfig()
	cfg.Enabled, cfg.LLMEnabled, cfg.LLMApiStyle = true, true, "chat"
	llm := contentfilter.NewOpenAIClientWithPolicy(
		"test", cfg.LLMModel, server.URL, cfg.LLMApiStyle, cfg.LLMPromptVersion,
		"", cfg.LLMMaxOutputTokens, time.Second,
	)
	p.contentFilter = contentfilter.NewWithLLMAdmission(
		cfg, llm, contentfilter.LLMCallbacks{}, contentfilter.Admission{
			Budget: processorBudgetProbe{}, Capture: &processorCaptureProbe{err: errors.New("capture unavailable")},
		},
	)
	p.privacy = &fakePrivacy{}

	err := p.Process(context.Background(), MessageParams{
		InfoHashes: []protocol.ID{hash},
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "contentfilter audited decision")
	assert.Zero(t, calls.Load())
	require.NoError(t, mock.ExpectationsWereMet())
}

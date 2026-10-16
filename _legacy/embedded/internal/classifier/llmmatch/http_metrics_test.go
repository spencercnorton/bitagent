package llmmatch

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	dto "github.com/prometheus/client_model/go"
	"github.com/spencercnorton/bitagent/internal/llmcapture"
	"github.com/stretchr/testify/require"
)

type delayedMatcherBody struct {
	io.Reader
	wait time.Duration
	err  error
}

func (b *delayedMatcherBody) Read(p []byte) (int, error) {
	if b.wait > 0 {
		time.Sleep(b.wait)
		b.wait = 0
	}
	if b.err != nil {
		return 0, b.err
	}
	return b.Reader.Read(p)
}

func (*delayedMatcherBody) Close() error { return nil }

func requireMatcherHTTPDuration(t *testing.T, client *Client, stage string, minimum time.Duration) {
	t.Helper()
	metric := &dto.Metric{}
	require.NoError(t, client.metrics.callDuration.WithLabelValues(stage).(prometheus.Metric).Write(metric))
	require.EqualValues(t, 1, metric.GetHistogram().GetSampleCount())
	require.GreaterOrEqual(t, metric.GetHistogram().GetSampleSum(), minimum.Seconds(), "reading the body belongs to HTTP attempt latency")
}

func TestMatcherHTTPAccountingIncludesBodyAndRecordsFailuresBeforeAudit(t *testing.T) {
	for _, tc := range []struct {
		name, body, outcome string
		status              int
		readErr             error
	}{
		{"success", `{"choices":[{"message":{"content":"{}"}}]}`, "success", 200, nil},
		{"status", `{}`, "http_status", 503, nil},
		{"envelope", `{"choices":[]}`, "decode", 200, nil},
		{"read", "", "read", 200, errors.New("truncated body")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			probe := &resultCaptureProbe{first: true, resultErr: errors.New("audit database unavailable")}
			client := matcherClientWithCapture("http://synthetic.invalid", probe)
			client.http.Transport = roundTripFunc(func(*http.Request) (*http.Response, error) {
				return &http.Response{StatusCode: tc.status, Header: make(http.Header), Body: &delayedMatcherBody{Reader: strings.NewReader(tc.body), wait: 30 * time.Millisecond, err: tc.readErr}}, nil
			})
			_, err := client.Extract(context.Background(), mediaTorrent("Synthetic.Film.2020.mkv"))
			require.ErrorIs(t, err, llmcapture.ErrCaptureUnavailable)
			require.Equal(t, float64(1), testutil.ToFloat64(client.metrics.httpOutcomes.WithLabelValues(client.cfg.Model, "extract", tc.outcome)))
			requireMatcherHTTPDuration(t, client, "extract", 30*time.Millisecond)
			if tc.outcome != "success" {
				require.Equal(t, float64(1), testutil.ToFloat64(client.metrics.callErrors.WithLabelValues("extract", tc.outcome)))
			}
		})
	}
}

func TestMatcherHTTPTransportErrorsAreNotAllTimeouts(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
	}{
		{"transport", errors.New("connection refused")},
		{"timeout", context.DeadlineExceeded},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client := testClient("http://synthetic.invalid")
			client.http.Transport = roundTripFunc(func(*http.Request) (*http.Response, error) { return nil, tc.err })
			_, err := client.call(context.Background(), "rerank", "system", "input", 120)
			require.Error(t, err)
			require.Equal(t, float64(1), testutil.ToFloat64(client.metrics.httpOutcomes.WithLabelValues(client.cfg.Model, "rerank", tc.name)))
			require.Equal(t, float64(1), testutil.ToFloat64(client.metrics.callErrors.WithLabelValues("rerank", tc.name)))
		})
	}
}

func TestEmbeddingHTTPAccountingIncludesBodyAndSeparatesModel(t *testing.T) {
	probe, budget := &embeddingCaptureProbe{}, &embeddingBudgetProbe{}
	client := embeddingTestClient("https://synthetic.invalid", probe, budget)
	client.httpEmbedding.Transport = roundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: &delayedMatcherBody{Reader: strings.NewReader(embeddingTestResponse), wait: 30 * time.Millisecond}}, nil
	})
	ctx, _ := llmcapture.WithResultTrace(context.Background())
	torrent := mediaTorrent("Synthetic.Film.2020.mkv")
	ext := Extraction{Title: "Synthetic Film", Type: "movie", Year: 2020}
	_, _, err := client.embeddingShortlist(ctx, torrent, ext, embeddingTestCandidates(), llmcapture.CandidateSourceLocal)
	require.NoError(t, err)
	require.Equal(t, float64(1), testutil.ToFloat64(client.metrics.httpOutcomes.WithLabelValues(client.cfg.Embeddings.Model, "embedding", "success")))
	requireMatcherHTTPDuration(t, client, "embedding", 30*time.Millisecond)
	// The second invocation is an exact cache replay, not a provider attempt.
	_, _, err = client.embeddingShortlist(ctx, torrent, ext, embeddingTestCandidates(), llmcapture.CandidateSourceLocal)
	require.NoError(t, err)
	require.Equal(t, float64(1), testutil.ToFloat64(client.metrics.httpOutcomes.WithLabelValues(client.cfg.Embeddings.Model, "embedding", "success")))
	require.Equal(t, float64(42), testutil.ToFloat64(client.metrics.tokens.WithLabelValues(client.cfg.Embeddings.Model, "embedding", "input")))
}

func TestControlledMatcherHTTPAccountingPostgres(t *testing.T) {
	ctx, _, recorder, control, torrent, budget := embeddingDispatchFixture(t)
	ctx, _ = llmcapture.WithResultTrace(ctx)
	newClient := func() *Client {
		return embeddingTestClient("https://synthetic.invalid", recorder, budget).WithDispatchControl(control)
	}
	client := newClient()
	client.http.Transport = roundTripFunc(func(*http.Request) (*http.Response, error) {
		body := `{"choices":[{"message":{"content":"{\"title\":\"Synthetic Film\",\"year\":2020,\"type\":\"movie\",\"season\":0,\"episode\":0,\"is_anime\":false,\"english\":\"unknown\",\"is_pack\":false,\"is_adult\":false}"}}],"usage":{"prompt_tokens":100,"completion_tokens":40}}`
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: &delayedMatcherBody{Reader: strings.NewReader(body), wait: 30 * time.Millisecond}}, nil
	})
	ext, err := client.Extract(ctx, torrent)
	require.NoError(t, err)
	require.True(t, ext.OK)
	require.Equal(t, float64(1), testutil.ToFloat64(client.metrics.httpOutcomes.WithLabelValues(client.cfg.Model, "extract", "success")))
	requireMatcherHTTPDuration(t, client, "extract", 30*time.Millisecond)
	// A fresh client must replay the durable first result without dispatch,
	// latency observations or duplicate token accounting.
	replay := newClient()
	replay.http.Transport = roundTripFunc(func(*http.Request) (*http.Response, error) {
		t.Fatal("retained replay must not contact the provider")
		return nil, errors.New("unexpected dispatch")
	})
	ext, err = replay.Extract(ctx, torrent)
	require.NoError(t, err)
	require.True(t, ext.OK)
	require.Zero(t, testutil.ToFloat64(replay.metrics.httpOutcomes.WithLabelValues(replay.cfg.Model, "extract", "success")))
	require.Zero(t, testutil.ToFloat64(replay.metrics.calls.WithLabelValues(replay.cfg.Model, "extract")))
	require.Zero(t, testutil.ToFloat64(replay.metrics.tokens.WithLabelValues(replay.cfg.Model, "extract", "input")))
}

func TestControlledMatcherHTTPFailuresAreCountedPostgres(t *testing.T) {
	ctx, _, recorder, control, torrent, budget := embeddingDispatchFixture(t)
	ctx, _ = llmcapture.WithResultTrace(ctx)
	client := embeddingTestClient("https://synthetic.invalid", recorder, budget).WithDispatchControl(control)
	client.http.Transport = roundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 503, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{}`))}, nil
	})
	_, err := client.Extract(ctx, torrent)
	require.ErrorIs(t, err, ErrMatcherChatHTTPStatus)
	require.Equal(t, float64(1), testutil.ToFloat64(client.metrics.httpOutcomes.WithLabelValues(client.cfg.Model, "extract", "http_status")))
	require.Equal(t, float64(1), testutil.ToFloat64(client.metrics.callErrors.WithLabelValues("extract", "http_status")))
}

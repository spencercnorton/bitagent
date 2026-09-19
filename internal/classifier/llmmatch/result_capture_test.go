package llmmatch

import (
	"context"
	"crypto/sha256"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/spencercnorton/bitagent/internal/llmcapture"
	"github.com/stretchr/testify/require"
)

type resultCaptureProbe struct {
	captureProbe
	first           bool
	resultErr       error
	keys            [][]byte
	results         []llmcapture.HTTPResult
	writeContextsOK bool
}

func (p *resultCaptureProbe) RecordHTTPResult(ctx context.Context, key []byte, result llmcapture.HTTPResult) (llmcapture.ResultReceipt, error) {
	deadline, ok := ctx.Deadline()
	p.writeContextsOK = ok && ctx.Err() == nil && time.Until(deadline) <= 2*time.Second
	p.keys = append(p.keys, append([]byte(nil), key...))
	p.results = append(p.results, result)
	digest := sha256.Sum256(result.Body)
	return llmcapture.ResultReceipt{CaptureKey: key, ResponseSHA256: digest[:], FirstObservation: p.first, StatusCode: result.StatusCode, ErrorClass: result.ErrorClass}, p.resultErr
}

func (*resultCaptureProbe) RecordMatchDecision(context.Context, llmcapture.ResultReceipt, []byte, llmcapture.MatchDecision) error {
	return nil
}

func TestCapturedResultsBindActualHTTPAndExcludeCacheHits(t *testing.T) {
	srv, calls := chatServer(t, `{"title":"Dune","year":2021,"type":"movie","is_anime":false,"is_pack":false,"is_adult":false}`, `{"tmdb_id":1,"confidence":0.9}`)
	probe := &resultCaptureProbe{first: true}
	client := matcherClientWithCapture(srv.URL, probe)
	torrent := mediaTorrent("Dune.2021.mkv")
	ctx, trace := llmcapture.WithResultTrace(context.Background())
	ext, err := client.Extract(ctx, torrent)
	require.NoError(t, err)
	id, _, err := client.RerankForMediaType(ctx, torrent, ext, "Dune", false, []Candidate{{ID: 1, Title: "Dune", Year: 2021}}, llmcapture.CandidateSourceLocal)
	require.NoError(t, err)
	require.EqualValues(t, 1, id)
	require.Len(t, probe.results, 2)
	for i, task := range []llmcapture.Task{llmcapture.TaskMatcherExtract, llmcapture.TaskMatcherRerank} {
		key, err := llmcapture.KeyForRequest(probe.requests[i])
		require.NoError(t, err)
		require.Equal(t, key, probe.keys[i])
		source := llmcapture.CandidateSourceNone
		if i == 1 {
			source = llmcapture.CandidateSourceLocal
		}
		receipt, ok := trace.Result(task, source)
		require.True(t, ok)
		require.True(t, receipt.FirstObservation)
		require.Equal(t, 200, receipt.StatusCode)
		require.Equal(t, "none", receipt.ErrorClass)
		require.Equal(t, key, receipt.CaptureKey)
		require.Contains(t, string(probe.results[i].Body), `"choices"`)
	}
	require.True(t, probe.writeContextsOK)
	cacheCtx, cacheTrace := llmcapture.WithResultTrace(context.Background())
	_, err = client.Extract(cacheCtx, torrent)
	require.NoError(t, err)
	_, _, err = client.RerankForMediaType(cacheCtx, torrent, ext, "Dune", false, []Candidate{{ID: 1, Title: "Dune", Year: 2021}}, llmcapture.CandidateSourceLocal)
	require.NoError(t, err)
	cachedReceipt, ok := cacheTrace.Result(llmcapture.TaskMatcherRerank, llmcapture.CandidateSourceLocal)
	require.True(t, ok)
	require.True(t, cachedReceipt.FromCache, "replayed receipt is not a new HTTP observation")
	require.Len(t, probe.results, 2)
	require.EqualValues(t, 2, atomic.LoadInt32(calls))
}

func TestResultFailurePreventsCachingAndDoesNotRefundAllowance(t *testing.T) {
	srv, calls := chatServer(t, `{"title":"Dune","year":2021,"type":"movie","is_anime":false,"is_pack":false,"is_adult":false}`)
	probe := &resultCaptureProbe{first: true, resultErr: errors.New("database down")}
	client := matcherClientWithCapture(srv.URL, probe)
	client.cfg.DailyCallLimit = 1
	ctx, trace := llmcapture.WithResultTrace(context.Background())
	_, err := client.Extract(ctx, mediaTorrent("Dune.2021.mkv"))
	require.ErrorIs(t, err, llmcapture.ErrCaptureUnavailable)
	_, ok := trace.Result(llmcapture.TaskMatcherExtract, llmcapture.CandidateSourceNone)
	require.False(t, ok)
	probe.resultErr = nil
	_, err = client.Extract(ctx, mediaTorrent("Dune.2021.mkv"))
	require.ErrorIs(t, err, ErrCallBudget)
	require.EqualValues(t, 1, atomic.LoadInt32(calls))
	require.Len(t, probe.results, 1, "withheld request is not an HTTP observation")
}

type resultRoundTripper func(*http.Request) (*http.Response, error)

func (f resultRoundTripper) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

type captureWithoutResults struct{}

func (captureWithoutResults) Enabled() bool { return true }
func (captureWithoutResults) Capture(context.Context, llmcapture.Request) (llmcapture.Outcome, error) {
	return llmcapture.OutcomeRecorded, nil
}

func TestEnabledCaptureWithoutResultRecorderBlocksBeforeProvider(t *testing.T) {
	srv, calls := chatServer(t, `{}`)
	client := matcherClientWithCapture(srv.URL, captureWithoutResults{})
	_, err := client.Extract(context.Background(), mediaTorrent("Dune.2021.mkv"))
	require.ErrorIs(t, err, llmcapture.ErrCaptureUnavailable)
	require.Zero(t, atomic.LoadInt32(calls))
}

func TestCapturedTerminalHTTPFailuresAreBoundedAndCancellationSafe(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		body   string
		class  string
	}{
		{"transport", 0, "", "transport"},
		{"http_status", 503, "unavailable", "http_status"},
		{"too_large", 503, strings.Repeat("x", llmcapture.MaxResultBodyBytes+5), "read"},
		{"envelope", 200, "not JSON", "envelope"},
		{"no_choices", 200, `{"choices":[]}`, "empty_choices"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			probe := &resultCaptureProbe{first: true}
			client := matcherClientWithCapture("http://unused.invalid", probe)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			ctx, trace := llmcapture.WithResultTrace(ctx)
			client.http = &http.Client{Transport: resultRoundTripper(func(*http.Request) (*http.Response, error) {
				cancel()
				if tc.status == 0 {
					return nil, context.Canceled
				}
				return &http.Response{StatusCode: tc.status, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(tc.body))}, nil
			})}
			_, err := client.Extract(ctx, mediaTorrent("Dune.2021.mkv"))
			require.Error(t, err)
			require.Len(t, probe.results, 1)
			require.Equal(t, tc.class, probe.results[0].ErrorClass)
			require.Equal(t, tc.status, probe.results[0].StatusCode)
			require.LessOrEqual(t, len(probe.results[0].Body), llmcapture.MaxResultBodyBytes)
			require.True(t, probe.writeContextsOK)
			receipt, ok := trace.Result(llmcapture.TaskMatcherExtract, llmcapture.CandidateSourceNone)
			require.True(t, ok)
			require.Equal(t, tc.class, receipt.ErrorClass)
		})
	}
}

func TestDuplicateResultReceiptCannotClaimFirstCohortObservation(t *testing.T) {
	srv, _ := chatServer(t, `{"title":"Dune","year":2021,"type":"movie","is_anime":false,"is_pack":false,"is_adult":false}`)
	probe := &resultCaptureProbe{first: false}
	client := matcherClientWithCapture(srv.URL, probe)
	ctx, trace := llmcapture.WithResultTrace(context.Background())
	_, err := client.Extract(ctx, mediaTorrent("Dune.2021.mkv"))
	require.NoError(t, err)
	receipt, ok := trace.Result(llmcapture.TaskMatcherExtract, llmcapture.CandidateSourceNone)
	require.True(t, ok)
	require.False(t, receipt.FirstObservation)
	require.Equal(t, float64(1), testutil.ToFloat64(client.metrics.auditResults.WithLabelValues("extract", "duplicate")))
}

func TestRerankCacheReceiptIsBoundToExactSourceAndPolicy(t *testing.T) {
	for _, variant := range []string{"info_hash", "source", "parsed_title", "extraction", "alias", "confidence", "live"} {
		t.Run(variant, func(t *testing.T) {
			srv, calls := chatServer(t, `{"tmdb_id":1,"confidence":0.9}`)
			probe := &resultCaptureProbe{first: true}
			client := matcherClientWithCapture(srv.URL, probe)
			torrent := mediaTorrent("Dune.2021.mkv")
			ext := Extraction{Title: "Dune", Year: 2021, Type: "movie"}
			candidates := []Candidate{{ID: 1, Title: "Dune", Year: 2021}}
			source, parsed := llmcapture.CandidateSourceLocal, "Dune"
			ctx, _ := llmcapture.WithResultTrace(context.Background())
			_, _, err := client.RerankForMediaType(ctx, torrent, ext, parsed, false, candidates, source)
			require.NoError(t, err)
			switch variant {
			case "info_hash":
				torrent.InfoHash[0] = 1
			case "source":
				source = llmcapture.CandidateSourceAPI
			case "parsed_title":
				parsed = "Dune Part One"
			case "extraction":
				ext.Year = 2020
			case "alias":
				candidates[0].AltTitles = []string{"Dune Part One"}
			case "confidence":
				client.cfg.MinConfidence = .8
			case "live":
				client.cfg.EnableLive = true
			}
			changedCtx, changedTrace := llmcapture.WithResultTrace(context.Background())
			_, _, err = client.RerankForMediaType(changedCtx, torrent, ext, parsed, false, candidates, source)
			require.NoError(t, err)
			require.EqualValues(t, 2, atomic.LoadInt32(calls), "changed provenance cannot borrow first-receipt retry authority")
			receipt, ok := changedTrace.Result(llmcapture.TaskMatcherRerank, source)
			require.True(t, ok)
			require.False(t, receipt.FromCache)
		})
	}
}

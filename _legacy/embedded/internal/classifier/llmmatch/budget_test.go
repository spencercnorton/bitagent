package llmmatch

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/spencercnorton/bitagent/internal/lazy"
	"github.com/stretchr/testify/require"
)

func TestPostgresCallBudgetScopeAndLimitValidation(t *testing.T) {
	pool := lazy.New(func() (*pgxpool.Pool, error) {
		t.Fatal("invalid admission must not initialize the database pool")
		return nil, nil
	})
	for _, scope := range []string{"", "Classifier_Type", "classifier_type_extra", "matcher' OR true --"} {
		b := &PostgresCallBudget{pool: pool, scope: scope}
		ok, err := b.Reserve(context.Background(), 1, 1)
		require.ErrorIs(t, err, errCallBudgetScope)
		require.False(t, ok)
	}
	for _, b := range []*PostgresCallBudget{NewPostgresCallBudget(pool), NewPostgresTypeCallBudget(pool), NewPostgresContentFilterCallBudget(pool), NewPostgresJunkPurgeCallBudget(pool)} {
		for _, limits := range [][2]int{{0, 10}, {10, 0}, {-1, 10}, {10, -1}} {
			ok, err := b.Reserve(context.Background(), limits[0], limits[1])
			require.NoError(t, err)
			require.False(t, ok)
		}
	}
	require.Equal(t, matcherBudgetScope, NewPostgresCallBudget(pool).scope)
	require.Equal(t, typeBudgetScope, NewPostgresTypeCallBudget(pool).scope)
	require.Equal(t, contentFilterBudgetScope, NewPostgresContentFilterCallBudget(pool).scope)
	require.Equal(t, junkPurgeBudgetScope, NewPostgresJunkPurgeCallBudget(pool).scope)
}

func TestCallBudgetConcurrencyAndUTCRollover(t *testing.T) {
	now := time.Date(2026, 9, 14, 23, 0, 0, 0, time.UTC)
	b := &memoryCallBudget{now: func() time.Time { return now }}
	var admitted atomic.Int32
	var wg sync.WaitGroup
	for range 100 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ok, err := b.Reserve(context.Background(), 7, 10)
			if err == nil && ok {
				admitted.Add(1)
			}
		}()
	}
	wg.Wait()
	require.Equal(t, int32(7), admitted.Load())
	now = now.Add(24 * time.Hour)
	for i := range 4 {
		ok, err := b.Reserve(context.Background(), 7, 10)
		require.NoError(t, err)
		require.Equal(t, i < 3, ok, "month allowance must survive daily rollover")
	}
	now = time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	ok, err := b.Reserve(context.Background(), 7, 10)
	require.NoError(t, err)
	require.True(t, ok)
	for _, limits := range [][2]int{{0, 10}, {10, 0}, {-1, 10}} {
		ok, err = b.Reserve(context.Background(), limits[0], limits[1])
		require.NoError(t, err)
		require.False(t, ok)
	}
}

func TestMatcherFailedRequestsConsumeBudgetAndCacheHitsDoNot(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		fmt.Fprint(w, `{"choices":[{"message":{"content":"{\"title\":\"Dune\",\"year\":2021,\"type\":\"movie\",\"season\":0,\"episode\":0,\"is_anime\":false,\"english\":\"unknown\",\"is_pack\":false,\"is_adult\":false}"}}]}`)
	}))
	defer srv.Close()
	c := testClient(srv.URL)
	c.cfg.DailyCallLimit = 2
	tor := mediaTorrent("Dune.2021.1080p.mkv")
	_, err := c.Extract(context.Background(), tor)
	require.Error(t, err)
	_, err = c.Extract(context.Background(), tor)
	require.NoError(t, err)
	_, err = c.Extract(context.Background(), tor)
	require.NoError(t, err, "cached result remains available after budget exhaustion")
	_, err = c.Extract(context.Background(), mediaTorrent("Another.Movie.2021.mkv"))
	require.ErrorIs(t, err, ErrCallBudget)
	require.Equal(t, int32(2), calls.Load())
	require.Equal(t, float64(1), testutil.ToFloat64(c.metrics.budgetSkips.WithLabelValues("exhausted")))
}

type failingBudget struct{}

func (failingBudget) Reserve(context.Context, int, int) (bool, error) {
	return false, errors.New("database unavailable")
}

func TestMatcherDispatchGuardsNeverCallProvider(t *testing.T) {
	for _, kind := range []string{"missing_budget", "failed_budget", "oversize", "output_limit", "busy", "zero_limit"} {
		t.Run(kind, func(t *testing.T) {
			srv, calls := chatServer(t, `{}`)
			c := testClient(srv.URL)
			switch kind {
			case "missing_budget":
				c.budget = nil
			case "failed_budget":
				c.budget = failingBudget{}
			case "oversize":
				c.cfg.MaxRequestBytes = 1
			case "output_limit":
				c.cfg.MaxOutputTokens = 1
			case "busy":
				for range cap(c.slots) {
					c.slots <- struct{}{}
				}
			case "zero_limit":
				c.cfg.DailyCallLimit = 0
			}
			_, err := c.call(context.Background(), "extract", "system", "user", 120)
			require.Error(t, err)
			require.Zero(t, atomic.LoadInt32(calls))
		})
	}
}

func TestMatcherUsageAndConfiguredState(t *testing.T) {
	c := testClient("http://unused.invalid")
	c.recordUsage("extract", []byte(`{"usage":{"prompt_tokens":100,"completion_tokens":20,"prompt_tokens_details":{"cached_tokens":40},"completion_tokens_details":{"reasoning_tokens":5}}}`))
	for kind, expected := range map[string]float64{"input": 100, "output": 20, "cached_input": 40, "reasoning": 5} {
		require.Equal(t, expected, testutil.ToFloat64(c.metrics.tokens.WithLabelValues(c.cfg.Model, "extract", kind)))
	}
	c.recordUsage("extract", []byte(`{"usage":{"prompt_tokens":1,"completion_tokens":2,"prompt_tokens_details":{"cached_tokens":4}}}`))
	c.recordUsage("extract", []byte(`{}`))
	c.recordUsage("extract", []byte(`{"usage":{"prompt_tokens":"unknown","completion_tokens":0}}`))
	require.Equal(t, float64(3), testutil.ToFloat64(c.metrics.usageMissing.WithLabelValues(c.cfg.Model, "extract")))
	require.Equal(t, float64(1), testutil.ToFloat64(c.metrics.config.WithLabelValues("enabled")))
	require.Equal(t, float64(0), testutil.ToFloat64(c.metrics.config.WithLabelValues("live")))
}

func TestMatcherEmptyChoicesIsCountedAsResponseError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"choices":[],"usage":{"prompt_tokens":10,"completion_tokens":0}}`)
	}))
	defer srv.Close()
	c := testClient(srv.URL)
	_, err := c.call(context.Background(), "extract", "system", "user", 120)
	require.ErrorIs(t, err, ErrMatcherChatNoChoices)
	require.Equal(t, float64(1), testutil.ToFloat64(c.metrics.calls.WithLabelValues(c.cfg.Model, "extract")))
	require.Equal(t, float64(1), testutil.ToFloat64(c.metrics.callErrors.WithLabelValues("extract", "decode")))
	require.Equal(t, float64(0), testutil.ToFloat64(c.metrics.usageMissing.WithLabelValues(c.cfg.Model, "extract")))
}

// matcher-eval must neither be throttled by nor draw down the crawler's shared
// ledger: an isolated allowance admits exactly its own calls, whatever the
// configured limits say, and never touches the injected budget.
func TestIsolateBudgetAdmitsExactlyItsOwnCalls(t *testing.T) {
	srv, calls := chatServer(t, `{}`)
	c := testClient(srv.URL)
	c.budget = failingBudget{} // the shared ledger: any use of it fails the call
	c.cfg.DailyCallLimit = 0   // and the configured limit would admit nothing
	c.IsolateBudget(2)
	for i := range 3 {
		_, err := c.call(context.Background(), "extract", "system", "user", 120)
		if i < 2 {
			require.NoError(t, err)
		} else {
			require.ErrorIs(t, err, ErrCallBudget)
		}
	}
	require.Equal(t, int32(2), atomic.LoadInt32(calls))
}

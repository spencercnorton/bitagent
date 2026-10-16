package llmmatch

import (
	"context"
	"fmt"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/spencercnorton/bitagent/internal/lazy"
	"github.com/stretchr/testify/require"
)

// Run against a disposable PostgreSQL with BITAGENT_TEST_POSTGRES_DSN set.
// An isolated schema keeps the fixture away from application tables.
func newBudgetTestPool(t *testing.T) (*pgxpool.Pool, func() *pgxpool.Pool) {
	t.Helper()
	dsn := os.Getenv("BITAGENT_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("set BITAGENT_TEST_POSTGRES_DSN for the database integration test")
	}
	ctx := context.Background()
	admin, err := pgxpool.New(ctx, dsn)
	require.NoError(t, err)
	t.Cleanup(admin.Close)
	schema := fmt.Sprintf("bitagent_budget_test_%d", time.Now().UnixNano())
	quoted := pgx.Identifier{schema}.Sanitize()
	_, err = admin.Exec(ctx, "CREATE SCHEMA "+quoted)
	require.NoError(t, err)
	t.Cleanup(func() {
		_, cleanupErr := admin.Exec(ctx, "DROP SCHEMA "+quoted+" CASCADE")
		require.NoError(t, cleanupErr)
	})
	config, err := pgxpool.ParseConfig(dsn)
	require.NoError(t, err)
	config.ConnConfig.RuntimeParams["search_path"] = schema
	newPool := func() *pgxpool.Pool {
		pool, poolErr := pgxpool.NewWithConfig(ctx, config.Copy())
		require.NoError(t, poolErr)
		t.Cleanup(pool.Close)
		return pool
	}
	pool := newPool()
	sql, err := os.ReadFile("../../../migrations/00050_llm_request_budgets.sql")
	require.NoError(t, err)
	_, err = pool.Exec(ctx, strings.Split(string(sql), "-- +goose Down")[0])
	require.NoError(t, err)
	return pool, newPool
}

func TestPostgresCallBudgetAtomicAndDurable(t *testing.T) {
	for _, scope := range []string{matcherBudgetScope, typeBudgetScope, contentFilterBudgetScope, junkPurgeBudgetScope} {
		t.Run(scope, func(t *testing.T) {
			testPostgresCallBudgetAtomicAndDurable(t, scope)
		})
	}
}

func testPostgresCallBudgetAtomicAndDurable(t *testing.T, scope string) {
	t.Helper()
	ctx := context.Background()
	pool, newPool := newBudgetTestPool(t)
	newBudget := func() *PostgresCallBudget {
		return &PostgresCallBudget{
			pool:  lazy.New(func() (*pgxpool.Pool, error) { return pool, nil }),
			scope: scope,
		}
	}
	var calls atomic.Int32
	var failures atomic.Int32
	var wg sync.WaitGroup
	for range 40 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ok, err := newBudget().Reserve(ctx, 7, 10)
			if err != nil {
				failures.Add(1)
			}
			if ok {
				calls.Add(1)
			}
		}()
	}
	wg.Wait()
	require.Zero(t, failures.Load())
	require.Equal(t, int32(7), calls.Load())
	// A fresh pool and client cannot reset the database allowance.
	pool.Close()
	pool = newPool()
	ok, err := newBudget().Reserve(ctx, 7, 10)
	require.NoError(t, err)
	require.False(t, ok)
	_, err = pool.Exec(ctx, "UPDATE llm_request_budgets SET day_start = day_start - 1")
	require.NoError(t, err)
	for i := range 4 {
		ok, err = newBudget().Reserve(ctx, 7, 10)
		require.NoError(t, err)
		require.Equal(t, i < 3, ok)
	}
	// Moving the old row to last month leaves a new month's allowance.
	_, err = pool.Exec(ctx, "UPDATE llm_request_budgets SET month_start = (month_start - interval '1 month')::date")
	require.NoError(t, err)
	ok, err = newBudget().Reserve(ctx, 7, 10)
	require.NoError(t, err)
	require.True(t, ok)
	ok, err = newBudget().Reserve(ctx, 0, 10)
	require.NoError(t, err)
	require.False(t, ok)
	pool.Close()
	ok, err = newBudget().Reserve(ctx, 7, 10)
	require.Error(t, err)
	require.False(t, ok)
}

func TestPostgresCallBudgetScopesAreIndependent(t *testing.T) {
	ctx := context.Background()
	pool, _ := newBudgetTestPool(t)
	lazyPool := lazy.New(func() (*pgxpool.Pool, error) { return pool, nil })
	matcher, classifier, contentFilter, junkPurge := NewPostgresCallBudget(lazyPool), NewPostgresTypeCallBudget(lazyPool), NewPostgresContentFilterCallBudget(lazyPool), NewPostgresJunkPurgeCallBudget(lazyPool)
	var matcherCalls, classifierCalls, contentFilterCalls, junkPurgeCalls, failures atomic.Int32
	var wg sync.WaitGroup
	for range 40 {
		for _, tc := range []struct {
			budget   *PostgresCallBudget
			limit    int
			admitted *atomic.Int32
		}{{matcher, 7, &matcherCalls}, {classifier, 3, &classifierCalls}, {contentFilter, 5, &contentFilterCalls}, {junkPurge, 6, &junkPurgeCalls}} {
			wg.Add(1)
			go func() {
				defer wg.Done()
				ok, err := tc.budget.Reserve(ctx, tc.limit, 10)
				if err != nil {
					failures.Add(1)
				}
				if ok {
					tc.admitted.Add(1)
				}
			}()
		}
	}
	wg.Wait()
	require.Zero(t, failures.Load())
	require.Equal(t, int32(7), matcherCalls.Load())
	require.Equal(t, int32(3), classifierCalls.Load())
	require.Equal(t, int32(5), contentFilterCalls.Load())
	require.Equal(t, int32(6), junkPurgeCalls.Load())
	// Rollover of one scope cannot replenish the other scope.
	_, err := pool.Exec(ctx, "UPDATE llm_request_budgets SET day_start = day_start - 1 WHERE scope = $1", typeBudgetScope)
	require.NoError(t, err)
	ok, err := classifier.Reserve(ctx, 3, 10)
	require.NoError(t, err)
	require.True(t, ok)
	ok, err = matcher.Reserve(ctx, 7, 10)
	require.NoError(t, err)
	require.False(t, ok)
	// The SQL allowlist is fail-closed even when called without the Go wrapper.
	for _, scope := range []string{"", "other", "Matcher", "matcher' OR true --"} {
		var used int
		err = pool.QueryRow(ctx, reserveCallSQL, 10, 10, scope).Scan(&used)
		require.ErrorIs(t, err, pgx.ErrNoRows)
	}
	for _, scope := range []string{matcherBudgetScope, typeBudgetScope, contentFilterBudgetScope, junkPurgeBudgetScope} {
		for _, limits := range [][2]int{{0, 10}, {10, 0}, {-1, 10}, {10, -1}} {
			var used int
			err = pool.QueryRow(ctx, reserveCallSQL, limits[0], limits[1], scope).Scan(&used)
			require.ErrorIs(t, err, pgx.ErrNoRows)
		}
	}
	var count, matcherDaily, matcherMonthly, classifierDaily, classifierMonthly, contentDaily, contentMonthly, junkDaily, junkMonthly int
	require.NoError(t, pool.QueryRow(ctx, "SELECT COUNT(*) FROM llm_request_budgets").Scan(&count))
	require.Equal(t, 4, count)
	require.NoError(t, pool.QueryRow(ctx, "SELECT daily_calls, monthly_calls FROM llm_request_budgets WHERE scope = $1", matcherBudgetScope).Scan(&matcherDaily, &matcherMonthly))
	require.Equal(t, 7, matcherDaily)
	require.Equal(t, 7, matcherMonthly)
	require.NoError(t, pool.QueryRow(ctx, "SELECT daily_calls, monthly_calls FROM llm_request_budgets WHERE scope = $1", typeBudgetScope).Scan(&classifierDaily, &classifierMonthly))
	require.Equal(t, 1, classifierDaily)
	require.Equal(t, 4, classifierMonthly)
	require.NoError(t, pool.QueryRow(ctx, "SELECT daily_calls, monthly_calls FROM llm_request_budgets WHERE scope = $1", contentFilterBudgetScope).Scan(&contentDaily, &contentMonthly))
	require.Equal(t, 5, contentDaily)
	require.Equal(t, 5, contentMonthly)
	require.NoError(t, pool.QueryRow(ctx, "SELECT daily_calls, monthly_calls FROM llm_request_budgets WHERE scope = $1", junkPurgeBudgetScope).Scan(&junkDaily, &junkMonthly))
	require.Equal(t, 6, junkDaily)
	require.Equal(t, 6, junkMonthly)
}

package llmmatch

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/spencercnorton/bitagent/internal/lazy"
)

var ErrCallBudget = errors.New("matcher call allowance exhausted")

// CallBudget reserves before dispatch. A transport failure may still be billed,
// so reservations are never refunded. Cache hits never reach this boundary.
type CallBudget interface {
	Reserve(context.Context, int, int) (bool, error)
}

// memoryCallBudget serves standalone library users and tests. The fx production
// constructor always installs PostgresCallBudget instead.
type memoryCallBudget struct {
	mu             sync.Mutex
	now            func() time.Time
	day, month     string
	daily, monthly int
}

func (b *memoryCallBudget) Reserve(ctx context.Context, daily, monthly int) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	now := b.now().UTC()
	day, month := now.Format("2006-01-02"), now.Format("2006-01")
	if day != b.day {
		b.day, b.daily = day, 0
	}
	if month != b.month {
		b.month, b.monthly = month, 0
	}
	if daily <= 0 || monthly <= 0 || b.daily >= daily || b.monthly >= monthly {
		return false, nil
	}
	b.daily++
	b.monthly++
	return true, nil
}

// capBudget admits exactly the calls it was given, for the life of one process,
// and ignores the configured daily/monthly limits. It is a measurement
// command's own allowance (see Client.IsolateBudget).
type capBudget struct{ left atomic.Int64 }

func (b *capBudget) Reserve(ctx context.Context, _, _ int) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	return b.left.Add(-1) >= 0, nil
}

type PostgresCallBudget struct {
	pool  lazy.Lazy[*pgxpool.Pool]
	scope string
}

const (
	matcherBudgetScope       = "matcher"
	typeBudgetScope          = "classifier_type"
	contentFilterBudgetScope = "contentfilter"
	junkPurgeBudgetScope     = "junkpurge"
)

var errCallBudgetScope = errors.New("invalid LLM call budget scope")

func NewPostgresCallBudget(pool lazy.Lazy[*pgxpool.Pool]) *PostgresCallBudget {
	return &PostgresCallBudget{pool: pool, scope: matcherBudgetScope}
}

// NewPostgresTypeCallBudget reserves the type-only classifier's independent
// allowance. It cannot consume or reset the catalogue matcher's allowance.
func NewPostgresTypeCallBudget(pool lazy.Lazy[*pgxpool.Pool]) *PostgresCallBudget {
	return &PostgresCallBudget{pool: pool, scope: typeBudgetScope}
}

// NewPostgresContentFilterCallBudget reserves the content-filter language
// classifier's independent allowance. It cannot consume or reset matcher or
// type-classifier capacity.
func NewPostgresContentFilterCallBudget(pool lazy.Lazy[*pgxpool.Pool]) *PostgresCallBudget {
	return &PostgresCallBudget{pool: pool, scope: contentFilterBudgetScope}
}

// NewPostgresJunkPurgeCallBudget reserves the junk judgment worker's
// independent allowance. Grouped requests and every per-title fallback reserve
// at the provider boundary, so a malformed grouped reply cannot bypass the
// durable daily/monthly fuse.
func NewPostgresJunkPurgeCallBudget(pool lazy.Lazy[*pgxpool.Pool]) *PostgresCallBudget {
	return &PostgresCallBudget{pool: pool, scope: junkPurgeBudgetScope}
}

// One atomic statement serializes all classifier workers and replicas. The
// database UTC clock owns rollover, independent of the process timezone/boot.
const reserveCallSQL = `
INSERT INTO llm_request_budgets (scope, month_start, day_start, daily_calls, monthly_calls)
SELECT $3::text, date_trunc('month', now() AT TIME ZONE 'UTC')::date,
       (now() AT TIME ZONE 'UTC')::date, 1, 1
WHERE $1::integer > 0 AND $2::integer > 0
  AND $3::text IN ('matcher', 'classifier_type', 'contentfilter', 'junkpurge')
ON CONFLICT (scope, month_start) DO UPDATE SET
  day_start = EXCLUDED.day_start,
  daily_calls = CASE WHEN llm_request_budgets.day_start = EXCLUDED.day_start
                     THEN llm_request_budgets.daily_calls + 1 ELSE 1 END,
  monthly_calls = llm_request_budgets.monthly_calls + 1
WHERE llm_request_budgets.monthly_calls < $2
  AND (CASE WHEN llm_request_budgets.day_start = EXCLUDED.day_start
            THEN llm_request_budgets.daily_calls ELSE 0 END) < $1
RETURNING monthly_calls`

func (b *PostgresCallBudget) Reserve(ctx context.Context, daily, monthly int) (bool, error) {
	if b.scope != matcherBudgetScope && b.scope != typeBudgetScope &&
		b.scope != contentFilterBudgetScope && b.scope != junkPurgeBudgetScope {
		return false, errCallBudgetScope
	}
	if daily <= 0 || monthly <= 0 {
		return false, nil
	}
	pool, err := b.pool.Get()
	if err != nil {
		return false, err
	}
	var used int
	err = pool.QueryRow(ctx, reserveCallSQL, daily, monthly, b.scope).Scan(&used)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	return err == nil, err
}

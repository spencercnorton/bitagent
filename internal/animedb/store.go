package animedb

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/spencercnorton/bitagent/internal/lazy"
	"github.com/spencercnorton/bitagent/internal/model"
)

// Store is the persistence layer for the anime-titles alias backbone. The
// table is a pure derived cache, so the only write path is a wholesale replace.
type Store struct {
	pool lazy.Lazy[*pgxpool.Pool]
}

func NewStore(pool lazy.Lazy[*pgxpool.Pool]) *Store {
	return &Store{pool: pool}
}

var animeTitlesColumns = []string{
	"normalized", "tmdb_type", "tmdb_id", "anidb_id", "display_title", "title_source", "updated_at",
}

// ReplaceAll atomically swaps the entire table for a freshly built alias set:
// TRUNCATE then COPY, in one transaction. Because the table is a derived cache
// this is the correct (and only) write path — no partial upserts, no stale
// rows. Returns the number of rows written.
func (s *Store) ReplaceAll(ctx context.Context, aliases []Alias) (int, error) {
	pool, err := s.pool.Get()
	if err != nil {
		return 0, fmt.Errorf("animedb: acquire pool: %w", err)
	}
	tx, err := pool.Begin(ctx)
	if err != nil {
		return 0, fmt.Errorf("animedb: begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if _, err := tx.Exec(ctx, "truncate table anime_titles"); err != nil {
		return 0, fmt.Errorf("animedb: truncate: %w", err)
	}

	now := time.Now()
	n, err := tx.CopyFrom(ctx,
		pgx.Identifier{"anime_titles"},
		animeTitlesColumns,
		pgx.CopyFromSlice(len(aliases), func(i int) ([]any, error) {
			a := aliases[i]
			return []any{
				a.Normalized, string(a.TMDBType), a.TMDBID, a.AniDBID, a.Display, a.Source, now,
			}, nil
		}),
	)
	if err != nil {
		return 0, fmt.Errorf("animedb: copy: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, fmt.Errorf("animedb: commit: %w", err)
	}
	return int(n), nil
}

// LoadAll returns every persisted alias row for the in-memory resolver snapshot.
func (s *Store) LoadAll(ctx context.Context) ([]Alias, error) {
	pool, err := s.pool.Get()
	if err != nil {
		return nil, fmt.Errorf("animedb: acquire pool: %w", err)
	}
	const q = `select normalized, tmdb_type, tmdb_id, anidb_id, display_title, title_source from anime_titles`
	rows, err := pool.Query(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("animedb: load all: %w", err)
	}
	defer rows.Close()
	var out []Alias
	for rows.Next() {
		var a Alias
		var ct string
		if err := rows.Scan(&a.Normalized, &ct, &a.TMDBID, &a.AniDBID, &a.Display, &a.Source); err != nil {
			return nil, fmt.Errorf("animedb: scan: %w", err)
		}
		a.TMDBType = model.ContentType(ct)
		out = append(out, a)
	}
	return out, rows.Err()
}

// LastUpdated returns the most recent row timestamp (freshness of the persisted
// table). ok is false when the table is empty.
func (s *Store) LastUpdated(ctx context.Context) (t time.Time, ok bool, err error) {
	pool, perr := s.pool.Get()
	if perr != nil {
		return time.Time{}, false, fmt.Errorf("animedb: acquire pool: %w", perr)
	}
	var ts *time.Time
	if err := pool.QueryRow(ctx, "select max(updated_at) from anime_titles").Scan(&ts); err != nil {
		return time.Time{}, false, fmt.Errorf("animedb: last updated: %w", err)
	}
	if ts == nil {
		return time.Time{}, false, nil
	}
	return *ts, true, nil
}

// Count returns the number of persisted alias rows.
func (s *Store) Count(ctx context.Context) (int64, error) {
	pool, err := s.pool.Get()
	if err != nil {
		return 0, fmt.Errorf("animedb: acquire pool: %w", err)
	}
	var n int64
	if err := pool.QueryRow(ctx, "select count(*) from anime_titles").Scan(&n); err != nil {
		return 0, fmt.Errorf("animedb: count: %w", err)
	}
	return n, nil
}

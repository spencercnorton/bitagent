package priors

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/spencercnorton/bitagent/internal/lazy"
)

// pgSourceLookup is the production SourceLookup. It reads the
// torrents and torrents_torrent_sources tables to enrich a grab
// event with the catalog's view of the torrent — the source list and
// the primary file extension.
//
// Returns ([], "", nil) on a clean miss (the *arr just discovered an
// infohash bitagent has not catalogued yet). The resolver treats
// this as "extract features from title alone" — title-only features
// are still useful, and we do not want a missing source row to
// silently drop the entire grab.
type pgSourceLookup struct {
	pool lazy.Lazy[*pgxpool.Pool]
}

// NewPgSourceLookup constructs a SourceLookup against the application's
// shared pgxpool.
func NewPgSourceLookup(pool lazy.Lazy[*pgxpool.Pool]) SourceLookup {
	return &pgSourceLookup{pool: pool}
}

func (l *pgSourceLookup) SourcesForInfoHash(ctx context.Context, infoHash []byte) ([]string, string, error) {
	if len(infoHash) == 0 {
		return nil, "", nil
	}
	pool, err := l.pool.Get()
	if err != nil {
		return nil, "", fmt.Errorf("priors source lookup: acquire pool: %w", err)
	}

	const sourcesQuery = `
select source
from torrents_torrent_sources
where info_hash = $1`
	rows, err := pool.Query(ctx, sourcesQuery, infoHash)
	if err != nil {
		return nil, "", fmt.Errorf("priors source lookup: sources: %w", err)
	}
	var sources []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			rows.Close()
			return nil, "", fmt.Errorf("priors source lookup: scan: %w", err)
		}
		sources = append(sources, s)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, "", fmt.Errorf("priors source lookup: rows: %w", err)
	}

	var ext *string
	const extQuery = `select extension from torrents where info_hash = $1`
	if err := pool.QueryRow(ctx, extQuery, infoHash).Scan(&ext); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return sources, "", nil
		}
		return sources, "", fmt.Errorf("priors source lookup: extension: %w", err)
	}
	if ext == nil {
		return sources, "", nil
	}
	return sources, *ext, nil
}

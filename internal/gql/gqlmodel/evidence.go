package gqlmodel

import (
	"context"
	"strconv"
	"time"

	"github.com/spencercnorton/bitagent/internal/evidence"
	"github.com/spencercnorton/bitagent/internal/gql/gqlmodel/gen"
	"github.com/spencercnorton/bitagent/internal/protocol"
)

// EvidenceQuery is the GraphQL handle to the evidence store. It
// overrides gen.EvidenceQuery via autobind so the field-level
// list(input) resolver can take dependencies.
type EvidenceQuery struct {
	Store *evidence.Store
}

// List returns a page of evidence rows ordered by observed_at desc,
// plus a totalCount that callers can use for pagination UIs. Limit
// is bounded server-side; oversized requests are clamped silently.
func (q EvidenceQuery) List(
	ctx context.Context,
	input gen.EvidenceListInput,
) (gen.EvidenceListResult, error) {
	limit := input.Limit
	if limit < 1 {
		limit = 1
	}
	if limit > 200 {
		limit = 200
	}
	offset := input.Offset
	if offset < 0 {
		offset = 0
	}

	rows, total, err := q.Store.ListRecent(ctx, limit, offset)
	if err != nil {
		return gen.EvidenceListResult{}, err
	}

	items := make([]gen.EvidenceItem, 0, len(rows))
	for _, e := range rows {
		var ih *protocol.ID
		if len(e.InfoHash) == 20 {
			id := protocol.ID{}
			copy(id[:], e.InfoHash)
			ih = &id
		}
		items = append(items, gen.EvidenceItem{
			ID:             strconv.FormatInt(e.ID, 10),
			Source:         string(e.Source),
			Kind:           string(e.Kind),
			SourceInstance: e.SourceInstance,
			SourceObjectID: e.SourceObjectID,
			DownloadID:     e.DownloadID,
			InfoHash:       ih,
			Title:          e.Title,
			MediaType:      string(e.MediaType),
			MediaID:        e.MediaID,
			Category:       e.Category,
			ObservedAt:     e.ObservedAt,
			Strength:       int(e.Strength),
		})
	}

	return gen.EvidenceListResult{
		TotalCount: total,
		Items:      items,
	}, nil
}

// IndexerStats aggregates *arr grab evidence by winning indexer over a
// lookback window. Aggregate counts only — no release names or
// per-torrent detail cross this boundary (the grab corpus is not
// public-safe). Days is clamped to 1..365.
func (q EvidenceQuery) IndexerStats(
	ctx context.Context,
	input gen.EvidenceIndexerStatsInput,
) (gen.EvidenceIndexerStatsResult, error) {
	days := input.Days
	if days < 1 {
		days = 1
	}
	if days > 365 {
		days = 365
	}

	stats, err := q.Store.IndexerStats(ctx, time.Now().AddDate(0, 0, -days))
	if err != nil {
		return gen.EvidenceIndexerStatsResult{}, err
	}

	indexers := make([]gen.EvidenceIndexerCount, 0, len(stats.Indexers))
	for _, ic := range stats.Indexers {
		indexers = append(indexers, gen.EvidenceIndexerCount{
			Indexer: ic.Indexer,
			Grabs:   ic.Grabs,
		})
	}
	daysOut := make([]gen.EvidenceIndexerStatsDay, 0, len(stats.Days))
	for _, d := range stats.Days {
		daysOut = append(daysOut, gen.EvidenceIndexerStatsDay{
			Date:          d.Date,
			TotalGrabs:    d.TotalGrabs,
			BitagentGrabs: d.BitagentGrabs,
		})
	}

	var winRate float64
	if stats.TotalGrabs > 0 {
		winRate = float64(stats.BitagentGrabs) / float64(stats.TotalGrabs)
	}
	return gen.EvidenceIndexerStatsResult{
		TotalGrabs:      stats.TotalGrabs,
		BitagentGrabs:   stats.BitagentGrabs,
		BitagentWinRate: winRate,
		Indexers:        indexers,
		Days:            daysOut,
	}, nil
}

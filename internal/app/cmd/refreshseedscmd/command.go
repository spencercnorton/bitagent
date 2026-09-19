// Package refreshseedscmd runs the tracker-scrape seeds refresh on demand,
// outside the periodic worker. Two uses:
//
//   - measurement: `refresh-seeds --dryRun` scrapes one batch of never-checked
//     hashes and reports how many public trackers actually know about — the key
//     unknown before enabling the worker. Writes nothing, so it does not page.
//   - backfill: `refresh-seeds` (write mode) loops batches until the stale set
//     drains (or --limit is reached), writing the ledger + authoritative
//     'tracker' source rows. Each batch stamps checked_at, so successive batches
//     advance through the catalog. Resumable: re-running picks up where it left
//     off once MinRescrapeAge has elapsed for the earlier rows.
package refreshseedscmd

import (
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/spencercnorton/bitagent/internal/evidence/liveness"
	"github.com/spencercnorton/bitagent/internal/lazy"
	"github.com/spencercnorton/bitagent/internal/seeds"
	"github.com/urfave/cli/v2"
	"go.uber.org/fx"
	"go.uber.org/zap"
)

type Params struct {
	fx.In
	Config   seeds.Config
	Pool     lazy.Lazy[*pgxpool.Pool]
	Metrics  *seeds.Metrics
	Liveness *liveness.Store `optional:"true"`
	Logger   *zap.SugaredLogger
}

type Result struct {
	fx.Out
	Command *cli.Command `group:"commands"`
}

func New(p Params) Result {
	return Result{Command: &cli.Command{
		Name:  "refresh-seeds",
		Usage: "Scrape public trackers to refresh torrent seeders/leechers (BEP-15 UDP)",
		Flags: []cli.Flag{
			&cli.UintFlag{
				Name:  "limit",
				Value: 0,
				Usage: "stop after scraping this many hashes (0 = drain the stale set)",
			},
			&cli.BoolFlag{
				Name:  "dryRun",
				Usage: "scrape and report coverage but persist nothing (single batch)",
			},
		},
		Action: p.action,
	}}
}

func (p Params) action(cctx *cli.Context) error {
	ctx := cctx.Context
	logger := p.Logger.Named("refresh-seeds")

	limit := int(cctx.Uint("limit"))
	dryRun := cctx.Bool("dryRun")
	write := !dryRun

	cfg := p.Config
	if len(cfg.TrackerUrls) == 0 {
		return fmt.Errorf("refresh-seeds: no trackers configured (SEEDS_TRACKER_URLS)")
	}
	// On a small limit, don't scrape a whole default batch.
	if limit > 0 && limit < cfg.BatchSize {
		cfg.BatchSize = limit
	}

	var livenessRec seeds.LivenessRecorder
	if p.Liveness != nil {
		livenessRec = p.Liveness
	}
	runner := seeds.NewRunner(cfg, p.Pool, p.Metrics, livenessRec, logger)

	var total seeds.Stats
	batchNum := 0
	for {
		st, err := runner.RunBatch(ctx, write)
		if err != nil {
			return err
		}
		batchNum++
		total.Selected += st.Selected
		total.Positive += st.Positive
		total.KnownZero += st.KnownZero
		total.Unknown += st.Unknown
		total.SourcesUpserted += st.SourcesUpserted
		total.SourcesCleared += st.SourcesCleared

		logger.Infow("refresh-seeds batch",
			"mode", modeLabel(write),
			"batch", batchNum,
			"selected", st.Selected,
			"positive", st.Positive,
			"known_zero", st.KnownZero,
			"unknown", st.Unknown,
			"sources_upserted", st.SourcesUpserted,
			"sources_cleared", st.SourcesCleared,
		)

		if st.Selected == 0 {
			break // stale set drained
		}
		if !write {
			break // dry-run cannot page (no checked_at written)
		}
		if limit > 0 && total.Selected >= limit {
			break
		}
	}

	runner.UpdateCoverageGauges(ctx)

	pct := func(n int) float64 {
		if total.Selected == 0 {
			return 0
		}
		return 100 * float64(n) / float64(total.Selected)
	}
	logger.Infow("refresh-seeds done",
		"mode", modeLabel(write),
		"batches", batchNum,
		"scraped", total.Selected,
		"positive", total.Positive,
		"positive_pct", fmt.Sprintf("%.1f", pct(total.Positive)),
		"known_zero", total.KnownZero,
		"unknown", total.Unknown,
		"unknown_pct", fmt.Sprintf("%.1f", pct(total.Unknown)),
		"sources_upserted", total.SourcesUpserted,
		"sources_cleared", total.SourcesCleared,
	)
	return nil
}

func modeLabel(write bool) string {
	if write {
		return "LIVE"
	}
	return "DRY-RUN"
}

// Package refreshanimetitlescmd runs the anime-titles alias-backbone refresh on
// demand, outside the periodic worker. Two uses:
//
//   - measurement: `refresh-anime-titles --dryRun` downloads and builds the
//     alias set and reports the mapping/title/alias counts without writing —
//     the coverage check before enabling the worker.
//   - build: `refresh-anime-titles` (write mode) replaces the anime_titles table
//     with a fresh build and hot-swaps the running resolver.
package refreshanimetitlescmd

import (
	"fmt"

	"github.com/spencercnorton/bitagent/internal/animedb"
	"github.com/urfave/cli/v2"
	"go.uber.org/fx"
	"go.uber.org/zap"
)

type Params struct {
	fx.In
	Runner *animedb.Runner
	Logger *zap.SugaredLogger
}

type Result struct {
	fx.Out
	Command *cli.Command `group:"commands"`
}

func New(p Params) Result {
	return Result{Command: &cli.Command{
		Name:  "refresh-anime-titles",
		Usage: "Rebuild the deterministic anime alias table from AniDB + Anime-Lists data",
		Flags: []cli.Flag{
			&cli.BoolFlag{
				Name:  "dryRun",
				Usage: "download and build but persist nothing (report coverage only)",
			},
		},
		Action: p.action,
	}}
}

func (p Params) action(cctx *cli.Context) error {
	logger := p.Logger.Named("refresh-anime-titles")
	write := !cctx.Bool("dryRun")

	stats, err := p.Runner.Refresh(cctx.Context, write)
	if err != nil {
		return fmt.Errorf("refresh-anime-titles: %w", err)
	}
	logger.Infow("refresh-anime-titles done",
		"mode", modeLabel(write),
		"mappings", stats.Mappings,
		"titles", stats.Titles,
		"aliases", stats.Aliases,
		"rows_written", stats.Rows,
	)
	return nil
}

func modeLabel(write bool) string {
	if write {
		return "LIVE"
	}
	return "DRY-RUN"
}

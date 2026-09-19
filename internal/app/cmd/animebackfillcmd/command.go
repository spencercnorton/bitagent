// Package animebackfillcmd backfills the persisted torrent_contents.is_anime
// flag over existing rows by running the deterministic anime detector
// (internal/anime.Detect) against each torrent name. Detection is Go, not SQL,
// so this cannot be a plain UPDATE; it pages the table and corrects rows whose
// stored flag disagrees with the detector. Newly classified torrents already
// get the correct value inline in the processor — this is the one-off pass for
// rows classified before the column existed.
package animebackfillcmd

import (
	"context"
	"fmt"

	"github.com/spencercnorton/bitagent/internal/anime"
	"github.com/spencercnorton/bitagent/internal/database/dao"
	"github.com/spencercnorton/bitagent/internal/lazy"
	"github.com/spencercnorton/bitagent/internal/model"
	"github.com/urfave/cli/v2"
	"go.uber.org/fx"
	"go.uber.org/zap"
)

type Params struct {
	fx.In
	Dao    lazy.Lazy[*dao.Query]
	Logger *zap.SugaredLogger
}

type Result struct {
	fx.Out
	Command *cli.Command `group:"commands"`
}

func New(p Params) (Result, error) {
	return Result{Command: &cli.Command{
		Name: "anime-backfill",
		Usage: "Backfill the persisted torrent_contents.is_anime flag by running the deterministic " +
			"anime detector over existing torrent names",
		Flags: []cli.Flag{
			&cli.IntFlag{
				Name:  "pageSize",
				Value: 1000,
				Usage: "torrent_contents rows to scan per page",
			},
			&cli.UintFlag{
				Name:  "limit",
				Value: 0,
				Usage: "stop after scanning this many rows (0 = whole table)",
			},
			&cli.BoolFlag{
				Name:  "write",
				Value: false,
				Usage: "persist corrections; default is a dry-run that only reports how many rows would flip",
			},
		},
		Action: p.action,
	}}, nil
}

type runStats struct {
	scanned  int
	pages    int
	setTrue  int
	setFalse int
}

func (p Params) action(ctx *cli.Context) error {
	d, err := p.Dao.Get()
	if err != nil {
		return err
	}

	pageSize := ctx.Int("pageSize")
	if pageSize <= 0 {
		pageSize = 1000
	}
	limit := int(ctx.Uint("limit"))
	write := ctx.Bool("write")

	var (
		st     runStats
		lastID string
	)

	for limit <= 0 || st.scanned < limit {
		remaining := pageSize
		if limit > 0 && limit-st.scanned < remaining {
			remaining = limit - st.scanned
		}

		rows, findErr := d.TorrentContent.WithContext(ctx.Context).
			Select(
				d.TorrentContent.ID,
				d.TorrentContent.InfoHash,
				d.TorrentContent.IsAnime,
			).
			Preload(d.TorrentContent.Torrent).
			Where(d.TorrentContent.ID.Gt(lastID)).
			Order(d.TorrentContent.ID).
			Limit(remaining).
			Find()
		if findErr != nil {
			return findErr
		}
		if len(rows) == 0 {
			break
		}

		st.pages++
		st.scanned += len(rows)
		lastID = rows[len(rows)-1].ID

		toTrue, toFalse := partitionByAnime(rows)

		if write {
			if updErr := applyCorrections(ctx.Context, d, toTrue, toFalse); updErr != nil {
				return updErr
			}
		}

		st.setTrue += len(toTrue)
		st.setFalse += len(toFalse)

		p.Logger.Infow("anime-backfill progress",
			"mode", modeLabel(write),
			"scanned", st.scanned,
			"pages", st.pages,
			"setTrue", st.setTrue,
			"setFalse", st.setFalse,
			"lastID", lastID,
		)
	}

	verb := "would flip"
	if write {
		verb = "flipped"
	}
	_, _ = fmt.Fprintf(ctx.App.Writer,
		"Done (%s): scanned=%d pages=%d %s setTrue=%d setFalse=%d\n",
		modeLabel(write), st.scanned, st.pages, verb, st.setTrue, st.setFalse)

	return nil
}

// partitionByAnime splits a page into the IDs whose stored is_anime disagrees
// with the detector: toTrue = detected anime currently flagged false, toFalse =
// currently flagged true but no longer detected as anime. Rows already correct
// are omitted so writes touch only what changed.
func partitionByAnime(rows []*model.TorrentContent) (toTrue, toFalse []string) {
	for _, row := range rows {
		want := anime.Detect(row.Torrent.Name).IsAnime()
		switch {
		case want && !row.IsAnime:
			toTrue = append(toTrue, row.ID)
		case !want && row.IsAnime:
			toFalse = append(toFalse, row.ID)
		}
	}
	return toTrue, toFalse
}

// applyCorrections updates only the mismatched rows. UpdateColumn is used
// deliberately so the backfill does NOT bump updated_at — this is a metadata
// correction, not a re-classification, and must not perturb updated_at-ordered
// consumers.
func applyCorrections(ctx context.Context, d *dao.Query, toTrue, toFalse []string) error {
	return d.Transaction(func(tx *dao.Query) error {
		if len(toTrue) > 0 {
			if _, err := tx.TorrentContent.WithContext(ctx).
				Where(d.TorrentContent.ID.In(toTrue...)).
				UpdateColumn(d.TorrentContent.IsAnime, true); err != nil {
				return err
			}
		}
		if len(toFalse) > 0 {
			if _, err := tx.TorrentContent.WithContext(ctx).
				Where(d.TorrentContent.ID.In(toFalse...)).
				UpdateColumn(d.TorrentContent.IsAnime, false); err != nil {
				return err
			}
		}
		return nil
	})
}

func modeLabel(write bool) string {
	if write {
		return "write"
	}
	return "dry-run"
}

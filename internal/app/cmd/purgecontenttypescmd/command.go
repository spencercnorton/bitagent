// Package purgecontenttypescmd hard-deletes torrents of out-of-scope content
// types from the catalog (dry-run by default). BitAgent's catalog is TV +
// movies only; this command removes already-stored music/ebook/audiobook/etc.
// torrents in bounded chunks and records the purged hashes in the blocking
// bloom filter so the crawler skips them on re-announce. Pair with
// CLASSIFIER_DELETE_CONTENT_TYPES so newly crawled out-of-scope content is
// dropped at classify time instead of stored — write mode enforces that
// pairing unless --force is given.
package purgecontenttypescmd

import (
	"context"
	"database/sql/driver"
	"fmt"
	"time"

	"github.com/spencercnorton/bitagent/internal/blocking"
	"github.com/spencercnorton/bitagent/internal/classifier"
	"github.com/spencercnorton/bitagent/internal/database/dao"
	"github.com/spencercnorton/bitagent/internal/lazy"
	"github.com/spencercnorton/bitagent/internal/model"
	"github.com/spencercnorton/bitagent/internal/protocol"
	"github.com/spencercnorton/bitagent/internal/slice"
	"github.com/urfave/cli/v2"
	"go.uber.org/fx"
	"go.uber.org/zap"
)

// blockTrancheSize bounds how many purged hashes accumulate before they are
// pushed to the blocking bloom filter. Every push rewrites the whole filter
// large object (~25 MB, see blocking/manager.go flush), so tranches keep that
// to ~a dozen rewrites for a million-row purge instead of one per batch.
const blockTrancheSize = 100_000

type Params struct {
	fx.In
	ClassifierConfig classifier.Config
	Dao              lazy.Lazy[*dao.Query]
	BlockingManager  lazy.Lazy[blocking.Manager]
	Logger           *zap.SugaredLogger
}

type Result struct {
	fx.Out
	Command *cli.Command `group:"commands"`
}

func New(p Params) (Result, error) {
	return Result{Command: &cli.Command{
		Name: "purge-content-types",
		Usage: "Hard-delete torrents of out-of-scope content types (cascade delete + crawler block); " +
			"dry-run by default",
		Flags: []cli.Flag{
			&cli.StringSliceFlag{
				Name:     "types",
				Required: true,
				Usage:    "content types to purge (e.g. music,ebook,audiobook); movie/tv_show are refused",
			},
			&cli.IntFlag{
				Name:  "batchSize",
				Value: 2000,
				Usage: "torrents to delete per batch",
			},
			&cli.Int64Flag{
				Name:  "limit",
				Value: 0,
				Usage: "stop after this many deleted torrents (0 = no limit)",
			},
			&cli.DurationFlag{
				Name:  "sleep",
				Value: 250 * time.Millisecond,
				Usage: "pause between batches so the purge yields to live load",
			},
			&cli.IntFlag{
				Name:  "sampleSize",
				Value: 20,
				Usage: "sample torrent names to print per type in dry-run mode",
			},
			&cli.BoolFlag{
				Name:  "write",
				Value: false,
				Usage: "actually delete; default is a dry-run that only reports counts and samples",
			},
			&cli.BoolFlag{
				Name: "force",
				Usage: "allow --write even when a requested type is not in CLASSIFIER_DELETE_CONTENT_TYPES " +
					"(re-crawled hashes would then be re-stored instead of dropped at classify time)",
			},
		},
		Action: p.action,
	}}, nil
}

// parseTypes validates the requested types and refuses in-scope or
// unparseable ones so a fat-fingered invocation cannot touch the movie/TV
// catalog.
func parseTypes(raw []string) ([]model.ContentType, error) {
	if len(raw) == 0 {
		return nil, fmt.Errorf("at least one content type is required")
	}

	seen := make(map[model.ContentType]struct{}, len(raw))
	types := make([]model.ContentType, 0, len(raw))

	for _, s := range raw {
		ct, err := model.ParseContentType(s)
		if err != nil {
			return nil, err
		}

		if ct == model.ContentTypeMovie || ct == model.ContentTypeTvShow {
			return nil, fmt.Errorf("refusing to purge in-scope content type %q", ct)
		}

		if _, ok := seen[ct]; ok {
			continue
		}

		seen[ct] = struct{}{}
		types = append(types, ct)
	}

	return types, nil
}

// missingClassifyTimeDeletes returns the requested types absent from the
// classifier's DeleteContentTypes config. Purging without classify-time
// deletes in place lets the crawler re-store the same content on re-announce.
func missingClassifyTimeDeletes(types []model.ContentType, configured []string) []model.ContentType {
	set := make(map[string]struct{}, len(configured))
	for _, s := range configured {
		set[s] = struct{}{}
	}

	var missing []model.ContentType

	for _, ct := range types {
		if _, ok := set[string(ct)]; !ok {
			missing = append(missing, ct)
		}
	}

	return missing
}

func (p Params) action(ctx *cli.Context) error {
	types, err := parseTypes(ctx.StringSlice("types"))
	if err != nil {
		return err
	}

	if ctx.Int64("limit") < 0 {
		return fmt.Errorf("--limit must be >= 0")
	}

	d, err := p.Dao.Get()
	if err != nil {
		return err
	}

	typeNames := make([]string, len(types))
	for i, ct := range types {
		typeNames[i] = string(ct)
	}

	rowCounts, totalRows, err := p.countRowsByType(ctx.Context, d, types)
	if err != nil {
		return err
	}

	totalTorrents, err := d.TorrentContent.WithContext(ctx.Context).Where(
		d.TorrentContent.ContentType.In(typeNames...),
	).Distinct(d.TorrentContent.InfoHash).Count()
	if err != nil {
		return err
	}

	for _, ct := range types {
		_, _ = fmt.Fprintf(ctx.App.Writer, "%-10s %d torrent_contents rows\n", ct, rowCounts[ct])
	}

	_, _ = fmt.Fprintf(ctx.App.Writer, "total rows        %d\ndistinct torrents %d\n", totalRows, totalTorrents)

	if !ctx.Bool("write") {
		if sampleErr := p.printSamples(ctx, d, types); sampleErr != nil {
			return sampleErr
		}

		_, _ = fmt.Fprintf(ctx.App.Writer,
			"Dry-run only. Re-run with --write to delete these torrents (cascades to all torrent rows) "+
				"and block their hashes.\n")

		return nil
	}

	if missing := missingClassifyTimeDeletes(types, p.ClassifierConfig.DeleteContentTypes); len(missing) > 0 && !ctx.Bool("force") {
		return fmt.Errorf(
			"types %v are not in CLASSIFIER_DELETE_CONTENT_TYPES (%v): re-crawled hashes would be "+
				"re-stored instead of dropped at classify time; set the env first or pass --force",
			missing, p.ClassifierConfig.DeleteContentTypes,
		)
	}

	return p.purge(ctx, d, typeNames, totalTorrents)
}

func (p Params) countRowsByType(
	ctx context.Context,
	d *dao.Query,
	types []model.ContentType,
) (map[model.ContentType]int64, int64, error) {
	counts := make(map[model.ContentType]int64, len(types))

	var total int64

	for _, ct := range types {
		n, err := d.TorrentContent.WithContext(ctx).Where(
			d.TorrentContent.ContentType.Eq(string(ct)),
		).Count()
		if err != nil {
			return nil, 0, err
		}

		counts[ct] = n
		total += n
	}

	return counts, total, nil
}

func (p Params) printSamples(ctx *cli.Context, d *dao.Query, types []model.ContentType) error {
	sampleSize := ctx.Int("sampleSize")
	if sampleSize <= 0 {
		return nil
	}

	for _, ct := range types {
		rows, err := d.TorrentContent.WithContext(ctx.Context).
			Preload(d.TorrentContent.Torrent).
			Where(d.TorrentContent.ContentType.Eq(string(ct))).
			Limit(sampleSize).
			Find()
		if err != nil {
			return err
		}

		_, _ = fmt.Fprintf(ctx.App.Writer, "\nsample %s:\n", ct)

		for _, row := range rows {
			_, _ = fmt.Fprintf(ctx.App.Writer, "  %s\n", row.Torrent.Name)
		}
	}

	return nil
}

func (p Params) purge(ctx *cli.Context, d *dao.Query, typeNames []string, totalTorrents int64) error {
	bm, err := p.BlockingManager.Get()
	if err != nil {
		return err
	}

	batchSize := ctx.Int("batchSize")
	if batchSize <= 0 {
		batchSize = 2000
	}

	limit := ctx.Int64("limit")
	sleep := ctx.Duration("sleep")
	startedAt := time.Now()

	var (
		deleted int64
		pending []protocol.ID
	)

	for limit <= 0 || deleted < limit {
		rows, findErr := d.TorrentContent.WithContext(ctx.Context).
			Select(d.TorrentContent.InfoHash).
			Where(d.TorrentContent.ContentType.In(typeNames...)).
			Limit(batchSize).
			Find()
		if findErr != nil {
			return findErr
		}

		if len(rows) == 0 {
			break
		}

		hashes := uniqueHashes(rows)
		if limit > 0 && deleted+int64(len(hashes)) > limit {
			hashes = hashes[:limit-deleted]
		}

		// Delete first so the next SELECT makes progress; the classify-time
		// delete config (enforced above unless --force) covers any hash the
		// crawler re-announces before its block tranche lands below.
		valuers := slice.Map(hashes, func(infoHash protocol.ID) driver.Valuer {
			return infoHash
		})

		if _, deleteErr := d.Torrent.WithContext(ctx.Context).Where(
			d.Torrent.InfoHash.In(valuers...),
		).Delete(); deleteErr != nil {
			return deleteErr
		}

		deleted += int64(len(hashes))
		pending = append(pending, hashes...)

		if len(pending) >= blockTrancheSize {
			if blockErr := bm.Block(ctx.Context, pending, true); blockErr != nil {
				return blockErr
			}

			pending = pending[:0]
		}

		p.Logger.Infow("purge-content-types progress",
			"deletedTorrents", deleted,
			"ofTorrents", totalTorrents,
			"elapsed", time.Since(startedAt).Round(time.Second).String(),
		)

		select {
		case <-ctx.Context.Done():
			return ctx.Context.Err()
		case <-time.After(sleep):
		}
	}

	if len(pending) > 0 {
		if blockErr := bm.Block(ctx.Context, pending, true); blockErr != nil {
			return blockErr
		}
	}

	_, _ = fmt.Fprintf(ctx.App.Writer,
		"Done (write): deleted %d of %d torrents in %s. Consider VACUUM ANALYZE on torrents/torrent_contents.\n",
		deleted, totalTorrents, time.Since(startedAt).Round(time.Second))

	return nil
}

func uniqueHashes(rows []*model.TorrentContent) []protocol.ID {
	seen := make(map[protocol.ID]struct{}, len(rows))
	hashes := make([]protocol.ID, 0, len(rows))

	for _, row := range rows {
		if _, ok := seen[row.InfoHash]; ok {
			continue
		}

		seen[row.InfoHash] = struct{}{}
		hashes = append(hashes, row.InfoHash)
	}

	return hashes
}

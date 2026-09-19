// Package refreshalttitlescmd backfills alternative/translated titles onto
// content rows that were fetched before alt-title ingestion existed.
//
// Content tsvectors are computed at write time, so rows persisted before this
// feature never see the new alt_title:* attributes — without this backfill,
// alt-title matching only benefits content fetched after deploy, and any
// recall measurement over existing content silently reads as zero lift.
//
// The command re-fetches TMDB details (with alternative_titles/translations
// appended) for each locally stored tmdb content row, rebuilds the model via
// the same transformers the classifier uses, recomputes the tsvector and
// upserts. A per-row alt_titles_checked marker attribute makes reruns cheap
// and interrupted runs resumable: already-processed rows are skipped unless
// --force is given. TMDB rate limiting is enforced by the shared requester.
package refreshalttitlescmd

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/spencercnorton/bitagent/internal/database/dao"
	"github.com/spencercnorton/bitagent/internal/lazy"
	"github.com/spencercnorton/bitagent/internal/model"
	"github.com/spencercnorton/bitagent/internal/tmdb"
	"github.com/urfave/cli/v2"
	"go.uber.org/fx"
	"go.uber.org/zap"
	"gorm.io/gorm/clause"
)

// checkedAttributeKey marks a content row as having been swept by this
// command (value: RFC 3339 timestamp). Rows with zero alternative titles
// would otherwise be re-fetched on every run.
const checkedAttributeKey = "alt_titles_checked"

// consecutiveErrorLimit aborts the run early when TMDB fails this many times
// in a row — almost always a dead API key or network problem, not bad rows.
const consecutiveErrorLimit = 10

type Params struct {
	fx.In
	Dao        lazy.Lazy[*dao.Query]
	TmdbClient lazy.Lazy[tmdb.Client]
	Logger     *zap.SugaredLogger
}

type Result struct {
	fx.Out
	Command *cli.Command `group:"commands"`
}

func New(p Params) (Result, error) {
	return Result{Command: &cli.Command{
		Name:  "refresh-alt-titles",
		Usage: "Backfill alternative/translated titles (and refreshed metadata) onto existing tmdb content rows",
		Flags: []cli.Flag{
			&cli.IntFlag{
				Name:  "batchSize",
				Value: 100,
				Usage: "number of content rows to load per database page",
			},
			&cli.UintFlag{
				Name:  "limit",
				Value: 0,
				Usage: "stop after refreshing this many rows (0 = no limit)",
			},
			&cli.StringSliceFlag{
				Name:    "contentType",
				Aliases: []string{"contentTypes"},
				Usage:   "restrict to the specified content type(s) (default: movie, tv_show, xxx)",
			},
			&cli.BoolFlag{
				Name:  "force",
				Usage: "re-fetch rows already marked as checked or already carrying alt titles",
			},
			&cli.BoolFlag{
				Name:  "dryRun",
				Usage: "fetch and report, but persist nothing",
			},
		},
		Action: p.action,
	}}, nil
}

type stats struct {
	processed int
	updated   int
	skipped   int
	notFound  int
	errored   int
}

func (p Params) action(ctx *cli.Context) error {
	d, err := p.Dao.Get()
	if err != nil {
		return err
	}

	client, err := p.TmdbClient.Get()
	if err != nil {
		return err
	}

	contentTypes, err := getContentTypes(ctx)
	if err != nil {
		return err
	}

	var (
		st                stats
		consecutiveErrors int
	)

	limit := int(ctx.Uint("limit"))
	batchSize := ctx.Int("batchSize")
	force := ctx.Bool("force")
	dryRun := ctx.Bool("dryRun")

	for _, contentType := range contentTypes {
		lastID := ""

		for limit <= 0 || st.updated < limit {
			rows, findErr := d.Content.WithContext(ctx.Context).
				Preload(d.Content.Attributes).
				Preload(d.Content.Collections).
				Where(
					d.Content.Type.Eq(string(contentType)),
					d.Content.Source.Eq(model.SourceTmdb),
					d.Content.ID.Gt(lastID),
				).
				Order(d.Content.ID).
				Limit(batchSize).
				Find()
			if findErr != nil {
				return findErr
			}

			if len(rows) == 0 {
				break
			}

			lastID = rows[len(rows)-1].ID

			for _, row := range rows {
				if limit > 0 && st.updated >= limit {
					break
				}

				st.processed++

				if !force && alreadyProcessed(row) {
					st.skipped++
					continue
				}

				refreshErr := p.refreshOne(ctx.Context, d, client, row, dryRun, &st)

				switch {
				case refreshErr == nil:
					consecutiveErrors = 0
				case errors.Is(refreshErr, context.Canceled):
					return refreshErr
				default:
					st.errored++
					consecutiveErrors++

					p.Logger.Warnw(
						"refresh failed",
						"type",
						row.Type,
						"id",
						row.ID,
						"error",
						refreshErr,
					)

					if consecutiveErrors >= consecutiveErrorLimit {
						return fmt.Errorf(
							"aborting after %d consecutive errors (last: %w)",
							consecutiveErrors,
							refreshErr,
						)
					}
				}

				if st.processed%100 == 0 {
					p.Logger.Infow("refresh progress",
						"processed", st.processed,
						"updated", st.updated,
						"skipped", st.skipped,
						"notFound", st.notFound,
						"errored", st.errored,
						"lastID", row.ID,
					)
				}
			}
		}
	}

	_, _ = fmt.Fprintf(ctx.App.Writer,
		"Done: processed=%d updated=%d skipped=%d notFound=%d errored=%d\n",
		st.processed, st.updated, st.skipped, st.notFound, st.errored)

	return nil
}

// alreadyProcessed reports whether a row was already swept: it carries either
// an alt-title attribute or the checked marker.
func alreadyProcessed(row *model.Content) bool {
	for _, a := range row.Attributes {
		if a.Key == checkedAttributeKey || strings.HasPrefix(a.Key, model.AltTitleAttributePrefix) {
			return true
		}
	}

	return false
}

func (p Params) refreshOne(
	ctx context.Context,
	d *dao.Query,
	client tmdb.Client,
	row *model.Content,
	dryRun bool,
	st *stats,
) error {
	id, idErr := strconv.ParseInt(row.ID, 10, 64)
	if idErr != nil {
		// Non-numeric ids can't be re-fetched from TMDB; skip quietly.
		st.skipped++
		return nil //nolint:nilerr
	}

	var (
		fresh    model.Content
		freshErr error
	)

	switch row.Type {
	case model.ContentTypeMovie, model.ContentTypeXxx:
		details, detailsErr := client.MovieDetails(ctx, tmdb.MovieDetailsRequest{
			ID:               id,
			AppendToResponse: []string{"alternative_titles", "translations"},
		})
		if detailsErr != nil {
			freshErr = detailsErr
		} else {
			fresh, freshErr = tmdb.MovieDetailsToMovieModel(details)
		}
	case model.ContentTypeTvShow:
		details, detailsErr := client.TvDetails(ctx, tmdb.TvDetailsRequest{
			SeriesID:         id,
			AppendToResponse: []string{"external_ids", "alternative_titles", "translations"},
		})
		if detailsErr != nil {
			freshErr = detailsErr
		} else {
			fresh, freshErr = tmdb.TvShowDetailsToTvShowModel(details)
		}
	default:
		st.skipped++
		return nil
	}

	if freshErr != nil {
		if errors.Is(freshErr, tmdb.ErrNotFound) {
			// Deleted from TMDB; mark checked so reruns skip it.
			st.notFound++

			if dryRun {
				return nil
			}

			return persistCheckedMarker(ctx, d, row)
		}

		return freshErr
	}

	if fresh.Type != row.Type {
		// The row's type (part of the primary key) changed upstream, e.g.
		// TMDB reflagged adult status. Upserting would create a duplicate
		// row under the new key; leave it to the classifier to sort out.
		p.Logger.Warnw("content type changed upstream, skipping",
			"id", row.ID, "was", row.Type, "now", fresh.Type)

		st.skipped++

		return nil
	}

	fresh.Attributes = append(fresh.Attributes, checkedMarker())
	fresh.UpdateTsv()

	if dryRun {
		altCount := 0

		for _, a := range fresh.Attributes {
			if strings.HasPrefix(a.Key, model.AltTitleAttributePrefix) {
				altCount++
			}
		}

		p.Logger.Infow("dry run", "type", row.Type, "id", row.ID, "title", fresh.Title, "altTitles", altCount)

		st.updated++

		return nil
	}

	freshPtr := &fresh
	if createErr := d.Content.WithContext(ctx).Clauses(
		clause.OnConflict{UpdateAll: true},
	).Create(freshPtr); createErr != nil {
		return createErr
	}

	st.updated++

	return nil
}

func persistCheckedMarker(ctx context.Context, d *dao.Query, row *model.Content) error {
	marker := checkedMarker()
	marker.ContentType = row.Type
	marker.ContentSource = row.Source
	marker.ContentID = row.ID

	return d.ContentAttribute.WithContext(ctx).Clauses(
		clause.OnConflict{UpdateAll: true},
	).Create(&marker)
}

func checkedMarker() model.ContentAttribute {
	return model.ContentAttribute{
		Source: model.SourceTmdb,
		Key:    checkedAttributeKey,
		Value:  time.Now().UTC().Format(time.RFC3339),
	}
}

func getContentTypes(ctx *cli.Context) ([]model.ContentType, error) {
	names := ctx.StringSlice("contentType")
	if len(names) == 0 {
		return []model.ContentType{
			model.ContentTypeMovie,
			model.ContentTypeTvShow,
			model.ContentTypeXxx,
		}, nil
	}

	contentTypes := make([]model.ContentType, 0, len(names))

	for _, name := range names {
		ct, err := model.ParseContentType(name)
		if err != nil {
			return nil, err
		}

		contentTypes = append(contentTypes, ct)
	}

	return contentTypes, nil
}

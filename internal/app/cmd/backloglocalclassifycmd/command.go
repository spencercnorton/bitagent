// Package backloglocalclassifycmd runs a local-only classifier pass over the
// unmatched movie/tv backlog without filling the live queue.
package backloglocalclassifycmd

import (
	"context"
	"errors"
	"fmt"

	"github.com/spencercnorton/bitagent/internal/classifier"
	"github.com/spencercnorton/bitagent/internal/classifier/classification"
	"github.com/spencercnorton/bitagent/internal/database/dao"
	"github.com/spencercnorton/bitagent/internal/lazy"
	"github.com/spencercnorton/bitagent/internal/model"
	"github.com/spencercnorton/bitagent/internal/processor"
	"github.com/spencercnorton/bitagent/internal/protocol"
	"github.com/urfave/cli/v2"
	"go.uber.org/fx"
	"go.uber.org/zap"
)

const consecutiveFailureLimit = 5

type Params struct {
	fx.In
	ClassifierConfig classifier.Config
	Dao              lazy.Lazy[*dao.Query]
	Processor        lazy.Lazy[processor.Processor]
	Runner           lazy.Lazy[classifier.Runner]
	Logger           *zap.SugaredLogger
}

type Result struct {
	fx.Out
	Command *cli.Command `group:"commands"`
}

func New(p Params) (Result, error) {
	return Result{Command: &cli.Command{
		Name: "backlog-local-classify",
		Usage: "Run a local-only deterministic classifier pass over unmatched movie/tv torrents " +
			"without enqueuing live queue jobs",
		Flags: []cli.Flag{
			&cli.IntFlag{
				Name:  "pageSize",
				Value: 500,
				Usage: "unmatched torrent_contents rows to process per page",
			},
			&cli.UintFlag{
				Name:  "limit",
				Value: 0,
				Usage: "stop after this many unique infohashes (0 = no limit)",
			},
			&cli.StringSliceFlag{
				Name:    "contentType",
				Aliases: []string{"contentTypes"},
				Usage:   "restrict to movie and/or tv_show (default: both)",
			},
			&cli.BoolFlag{
				Name:  "rematch",
				Value: false,
				Usage: "ignore previous classifications and classify from scratch",
			},
			&cli.BoolFlag{
				Name:  "write",
				Value: false,
				Usage: "persist results; default is a dry-run that only reports decisions",
			},
			&cli.IntFlag{
				Name: "minSeeders",
				Usage: "only process rows with torrent_contents.seeders >= this value " +
					"(0 = no filter). Targets the alive/wanted slice: blanket residual " +
					"re-run yields ~2%, the seeded slice ~10x that. Requires fresh seeder " +
					"counts (seeds denorm-sync).",
			},
		},
		Action: p.action,
	}}, nil
}

type runStats struct {
	seen      int
	submitted int
	pages     int
	matched   int
	typedOnly int
	unmatched int
	deleted   int
	failed    int
	passFails int
}

func (p Params) action(ctx *cli.Context) error {
	d, err := p.Dao.Get()
	if err != nil {
		return err
	}

	contentTypes, err := getContentTypes(ctx)
	if err != nil {
		return err
	}

	typeNames := make([]string, 0, len(contentTypes))
	for _, ct := range contentTypes {
		typeNames = append(typeNames, string(ct))
	}

	flags := localOnlyFlags()
	classifyMode := processor.ClassifyModeDefault
	if ctx.Bool("rematch") {
		classifyMode = processor.ClassifyModeRematch
	}

	limit := int(ctx.Uint("limit"))
	pageSize := ctx.Int("pageSize")
	if pageSize <= 0 {
		pageSize = 500
	}

	write := ctx.Bool("write")
	var (
		st                  runStats
		lastID              string
		consecutiveFailures int
	)

	var pr processor.Processor
	var runner classifier.Runner
	if write {
		pr, err = p.Processor.Get()
	} else {
		runner, err = p.Runner.Get()
	}
	if err != nil {
		return err
	}

	minSeeders := ctx.Int("minSeeders")

	for limit <= 0 || st.submitted < limit {
		q := d.TorrentContent.WithContext(ctx.Context).
			Preload(d.TorrentContent.Torrent).
			Preload(d.TorrentContent.Torrent.Files).
			Preload(d.TorrentContent.Torrent.Hint).
			Preload(d.TorrentContent.Torrent.Sources).
			Where(
				d.TorrentContent.ContentType.In(typeNames...),
				d.TorrentContent.ContentID.IsNull(),
				d.TorrentContent.ID.Gt(lastID),
			)
		if minSeeders > 0 {
			q = q.Where(d.TorrentContent.Seeders.Gte(model.NewNullUint(uint(minSeeders))))
		}

		rows, findErr := q.
			Order(d.TorrentContent.ID).
			Limit(pageSize).
			Find()
		if findErr != nil {
			return findErr
		}
		if len(rows) == 0 {
			break
		}

		st.pages++
		lastID = rows[len(rows)-1].ID
		hashes, torrents := uniquePage(rows)
		st.seen += len(rows)

		if limit > 0 && st.submitted+len(hashes) > limit {
			n := limit - st.submitted
			hashes = hashes[:n]
			torrents = torrents[:n]
		}
		if len(hashes) == 0 {
			continue
		}

		if write {
			processErr := pr.Process(ctx.Context, processor.MessageParams{
				ClassifyMode:      classifyMode,
				ClassifierFlags:   flags,
				SkipContentFilter: true,
				InfoHashes:        hashes,
			})
			switch {
			case processErr == nil:
				consecutiveFailures = 0
			case errors.Is(processErr, context.Canceled):
				return processErr
			default:
				st.passFails++
				consecutiveFailures++
				p.Logger.Warnw("local backlog classifier page failed", "lastID", lastID, "error", processErr)
				if consecutiveFailures >= consecutiveFailureLimit {
					return fmt.Errorf(
						"aborting after %d consecutive classifier-pass failures (last: %w)",
						consecutiveFailures,
						processErr,
					)
				}
			}
		} else {
			p.dryRunPage(ctx.Context, runner, flags, torrents, &st)
		}

		st.submitted += len(hashes)
		p.Logger.Infow("backlog local-classify progress",
			"mode", modeLabel(write),
			"seen", st.seen,
			"submitted", st.submitted,
			"matched", st.matched,
			"typedOnly", st.typedOnly,
			"unmatched", st.unmatched,
			"deleted", st.deleted,
			"failed", st.failed,
			"passFails", st.passFails,
			"lastID", lastID,
		)
	}

	_, _ = fmt.Fprintf(ctx.App.Writer,
		"Done (%s): seen=%d submitted=%d pages=%d matched=%d typedOnly=%d unmatched=%d deleted=%d failed=%d passFails=%d\n",
		modeLabel(write), st.seen, st.submitted, st.pages, st.matched, st.typedOnly, st.unmatched,
		st.deleted, st.failed, st.passFails)

	return nil
}

func (p Params) dryRunPage(
	ctx context.Context,
	runner classifier.Runner,
	flags classifier.Flags,
	torrents []model.Torrent,
	st *runStats,
) {
	for _, t := range torrents {
		cl, err := runner.Run(ctx, p.ClassifierConfig.Workflow, flags, t)
		switch {
		case errors.Is(err, classification.ErrDeleteTorrent):
			st.deleted++
		case err != nil:
			st.failed++
		case cl.Content != nil:
			st.matched++
		case cl.ContentType.Valid:
			st.typedOnly++
		default:
			st.unmatched++
		}
	}
}

func localOnlyFlags() classifier.Flags {
	return classifier.Flags{
		"apis_enabled":      false,
		"llm_match_enabled": false,
		"llm_stage_enabled": false,
	}
}

func uniquePage(rows []*model.TorrentContent) ([]protocol.ID, []model.Torrent) {
	hashes := make([]protocol.ID, 0, len(rows))
	torrents := make([]model.Torrent, 0, len(rows))
	seen := make(map[protocol.ID]struct{}, len(rows))
	for _, row := range rows {
		if _, ok := seen[row.InfoHash]; ok {
			continue
		}
		seen[row.InfoHash] = struct{}{}
		hashes = append(hashes, row.InfoHash)
		torrents = append(torrents, row.Torrent)
	}
	return hashes, torrents
}

func modeLabel(write bool) string {
	if write {
		return "write"
	}
	return "dry-run"
}

func getContentTypes(ctx *cli.Context) ([]model.ContentType, error) {
	names := ctx.StringSlice("contentType")
	if len(names) == 0 {
		return []model.ContentType{model.ContentTypeMovie, model.ContentTypeTvShow}, nil
	}

	contentTypes := make([]model.ContentType, 0, len(names))
	for _, name := range names {
		ct, err := model.ParseContentType(name)
		if err != nil {
			return nil, err
		}
		if ct != model.ContentTypeMovie && ct != model.ContentTypeTvShow {
			return nil, fmt.Errorf("unsupported content type %q: local backlog classifier only handles movie and tv_show", name)
		}
		contentTypes = append(contentTypes, ct)
	}
	return contentTypes, nil
}

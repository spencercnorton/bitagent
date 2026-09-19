// Package batchllmmatchcmd drives the LLM TMDB matcher over the existing
// backlog of unmatched movie/tv torrents.
//
// The per-torrent workflow action (attach_tmdb_content_by_llm_search) is the
// right shape for the live crawl but not for a multi-million-row backfill:
// one stage-1 call per torrent is a non-starter on a local model. This
// command pages unmatched torrent_contents, pre-warms the matcher's stage-1
// cache with BATCHED extract prompts (25-50 names per call), then submits the
// page to the standard classifier pass — which gets pure cache hits on
// extract and otherwise behaves exactly like a normal reprocess (deterministic
// steps first, LLM rerank as fallback, shadow/live semantics from the
// classifier_llm_match config). No persistence logic is duplicated.
//
// Operational notes:
//   - The matcher config is read from THIS process's environment, so a shadow
//     measurement run needs no daemon redeploy:
//     docker exec -e CLASSIFIER_LLM_MATCH_ENABLED=true <ctr> bitagent batch-llm-match --limit 300
//   - Do not run concurrently with refresh-alt-titles: each CLI process has
//     its own TMDB rate limiter, so two runs can exceed the API limit combined.
package batchllmmatchcmd

import (
	"context"
	"errors"
	"fmt"

	"github.com/spencercnorton/bitagent/internal/classifier"
	"github.com/spencercnorton/bitagent/internal/classifier/llmmatch"
	"github.com/spencercnorton/bitagent/internal/database/dao"
	"github.com/spencercnorton/bitagent/internal/lazy"
	"github.com/spencercnorton/bitagent/internal/model"
	"github.com/spencercnorton/bitagent/internal/processor"
	"github.com/spencercnorton/bitagent/internal/protocol"
	"github.com/urfave/cli/v2"
	"go.uber.org/fx"
	"go.uber.org/zap"
)

// consecutiveFailureLimit aborts the run when this many classifier passes fail
// in a row — almost always a dead endpoint or database problem, not bad rows.
const consecutiveFailureLimit = 5

type Params struct {
	fx.In
	Dao       lazy.Lazy[*dao.Query]
	Processor lazy.Lazy[processor.Processor]
	LLMMatch  *llmmatch.Client
	Logger    *zap.SugaredLogger
}

type Result struct {
	fx.Out
	Command *cli.Command `group:"commands"`
}

func New(p Params) (Result, error) {
	return Result{Command: &cli.Command{
		Name: "batch-llm-match",
		Usage: "Run the LLM TMDB matcher over the unmatched movie/tv backlog with batched extract prompts " +
			"(requires CLASSIFIER_LLM_MATCH_ENABLED=true in this process's env; do not run alongside refresh-alt-titles)",
		Flags: []cli.Flag{
			&cli.IntFlag{
				Name:  "pageSize",
				Value: 50,
				Usage: "torrents per database page (also the classifier-pass chunk, i.e. the rerank concurrency bound)",
			},
			&cli.IntFlag{
				Name:  "llmBatchSize",
				Value: 32,
				Usage: "release names per batched extract prompt (25-50 is sensible)",
			},
			&cli.UintFlag{
				Name:  "limit",
				Value: 0,
				Usage: "stop after submitting this many torrents to the classifier (0 = no limit)",
			},
			&cli.StringSliceFlag{
				Name:    "contentType",
				Aliases: []string{"contentTypes"},
				Usage:   "restrict to movie and/or tv_show (default: both)",
			},
		},
		Action: p.action,
	}}, nil
}

type runStats struct {
	seen      int
	gated     int
	submitted int
	passFails int
	batch     llmmatch.BatchStats
}

func (p Params) action(ctx *cli.Context) error {
	d, err := p.Dao.Get()
	if err != nil {
		return err
	}

	pr, err := p.Processor.Get()
	if err != nil {
		return err
	}

	lm := p.LLMMatch
	if !lm.Enabled() {
		return errors.New(
			"llm matcher is disabled in this process; run with CLASSIFIER_LLM_MATCH_ENABLED=true " +
				"(shadow) and optionally CLASSIFIER_LLM_MATCH_ENABLE_LIVE=true (attach), " +
				"e.g. docker exec -e CLASSIFIER_LLM_MATCH_ENABLED=true <container> bitagent batch-llm-match",
		)
	}

	contentTypes, err := getContentTypes(ctx)
	if err != nil {
		return err
	}

	typeNames := make([]string, 0, len(contentTypes))
	for _, ct := range contentTypes {
		typeNames = append(typeNames, string(ct))
	}

	// llm_match_enabled is the CEL-level gate in the core workflow; without it
	// the action never runs regardless of the client config.
	flags := classifier.Flags{"llm_match_enabled": true}

	var (
		st                  runStats
		consecutiveFailures int
	)

	limit := int(ctx.Uint("limit"))
	pageSize := ctx.Int("pageSize")
	llmBatchSize := ctx.Int("llmBatchSize")
	lastID := ""

	for limit <= 0 || st.submitted < limit {
		rows, findErr := d.TorrentContent.WithContext(ctx.Context).
			Preload(d.TorrentContent.Torrent).
			Preload(d.TorrentContent.Torrent.Files).
			Where(
				d.TorrentContent.ContentType.In(typeNames...),
				d.TorrentContent.ContentID.IsNull(),
				d.TorrentContent.ID.Gt(lastID),
			).
			Order(d.TorrentContent.ID).
			Limit(pageSize).
			Find()
		if findErr != nil {
			return findErr
		}

		if len(rows) == 0 {
			break
		}

		lastID = rows[len(rows)-1].ID

		page := make([]model.Torrent, 0, len(rows))
		hashes := make([]protocol.ID, 0, len(rows))

		for _, tc := range rows {
			st.seen++

			// Same gates the workflow action applies (size, files,
			// plausibility, privacy) — applied here too so gated-out names
			// never occupy a batch-prompt slot.
			if !lm.Allow(ctx.Context, tc.Torrent) {
				st.gated++
				continue
			}

			page = append(page, tc.Torrent)
			hashes = append(hashes, tc.InfoHash)
		}

		if limit > 0 && st.submitted+len(hashes) > limit {
			n := limit - st.submitted
			page = page[:n]
			hashes = hashes[:n]
		}

		if len(hashes) == 0 {
			continue
		}

		st.batch.Add(lm.ExtractMany(ctx.Context, page, llmBatchSize))

		processErr := pr.Process(ctx.Context, processor.MessageParams{
			InfoHashes:      hashes,
			ClassifyMode:    processor.ClassifyModeDefault,
			ClassifierFlags: flags,
		})

		switch {
		case processErr == nil:
			consecutiveFailures = 0
		case errors.Is(processErr, context.Canceled):
			return processErr
		default:
			// Failed hashes were re-queued by the processor; the daemon will
			// retry them. Count the page and keep paging.
			st.passFails++
			consecutiveFailures++

			p.Logger.Warnw("classifier pass failed", "lastID", lastID, "error", processErr)

			if consecutiveFailures >= consecutiveFailureLimit {
				return fmt.Errorf(
					"aborting after %d consecutive classifier-pass failures (last: %w)",
					consecutiveFailures,
					processErr,
				)
			}
		}

		st.submitted += len(hashes)

		p.Logger.Infow("batch llm-match progress",
			"seen", st.seen,
			"gated", st.gated,
			"submitted", st.submitted,
			"extractOK", st.batch.OK,
			"extractGated", st.batch.Gated,
			"extractEmpty", st.batch.Empty,
			"extractCached", st.batch.Cached,
			"extractFailed", st.batch.Failed,
			"batches", st.batch.Batches,
			"singles", st.batch.Singles,
			"passFails", st.passFails,
			"lastID", lastID,
		)
	}

	_, _ = fmt.Fprintf(ctx.App.Writer,
		"Done: seen=%d gated=%d submitted=%d extractOK=%d extractGated=%d extractEmpty=%d extractCached=%d extractFailed=%d batches=%d singles=%d passFails=%d\n",
		st.seen, st.gated, st.submitted,
		st.batch.OK, st.batch.Gated, st.batch.Empty, st.batch.Cached, st.batch.Failed,
		st.batch.Batches, st.batch.Singles, st.passFails)

	return nil
}

// getContentTypes restricts the sweep to movie/tv_show — the only types the
// matcher understands.
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
			return nil, fmt.Errorf("unsupported content type %q: the llm matcher only handles movie and tv_show", name)
		}

		contentTypes = append(contentTypes, ct)
	}

	return contentTypes, nil
}

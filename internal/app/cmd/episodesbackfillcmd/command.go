// Package episodesbackfillcmd corrects torrent_contents.episodes rows that were
// stored by the pre-v0.46.0 parser, which capped episode numbers at 2 digits
// (S20E048 stored as episode 4) and kept only the first of a concatenated
// multi-episode (S01E01E02 stored as E01). It re-parses each candidate's name
// through the SAME production parser (parsers.ParseVideoContentWithOptions with
// the live ParseNoiseV2 setting) and, for rows whose recomputed episodes differ
// from what is stored, updates only the episodes column.
//
// Candidates are restricted to tv_show rows whose name carries a 3+ digit or
// concatenated episode token — the only rows the parser fix can change — so the
// pass never touches season packs (whose recomputed value can differ for the
// unrelated size-backstop reason). Newly crawled and reprocessed rows already
// parse correctly inline; this is the one-off pass for rows classified before
// the fix.
package episodesbackfillcmd

import (
	"context"
	"fmt"
	"regexp"

	"github.com/spencercnorton/bitagent/internal/classifier"
	"github.com/spencercnorton/bitagent/internal/classifier/classification"
	"github.com/spencercnorton/bitagent/internal/classifier/parsers"
	"github.com/spencercnorton/bitagent/internal/database/dao"
	"github.com/spencercnorton/bitagent/internal/lazy"
	"github.com/spencercnorton/bitagent/internal/model"
	"github.com/urfave/cli/v2"
	"go.uber.org/fx"
	"go.uber.org/zap"
)

// candidateNameRegex matches the release-name shapes the pre-v0.46.0 parser
// mis-stored: a 3+ digit episode token (SxxE123, "episode 123", 1x123) or a
// concatenated multi-episode (SxxE01E02). Anything else is left untouched.
var candidateNameRegex = regexp.MustCompile(
	`(?i)(?:s\d{1,2}[ ._-]?e\d{3,}` + // SxxExxx (3+ digit)
		`|s\d{1,2}[ ._-]?e\d{1,4}[ ._-]?e\d{1,4}` + // SxxEyyEzz (concatenated)
		`|(?:episode|ep)[ ._-]?\d{3,}` + // "episode 123"
		`|\b\d{1,2}x\d{3,}` + // NNxNNN (3+ digit x-format)
		`|s(?:18|19|20)\d{2}[ ._-]?e\d{1,4}` + // SYYYYExx year-season (stored as S20 pre-v0.50.0)
		`|e\d{1,4}\s*-\s*(?:480|576|720|1080|2160)p?\b)`, // "E106 - 1080p" resolution-as-range-end rows
)

type Params struct {
	fx.In
	Dao              lazy.Lazy[*dao.Query]
	ClassifierConfig classifier.Config
	Logger           *zap.SugaredLogger
}

type Result struct {
	fx.Out
	Command *cli.Command `group:"commands"`
}

func New(p Params) (Result, error) {
	return Result{Command: &cli.Command{
		Name: "episodes-backfill",
		Usage: "Re-parse tv_show rows whose name carries a 3+ digit or concatenated episode token and " +
			"correct the stored episodes column (fixes pre-v0.46.0 truncation)",
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
				Usage: "persist corrections; default is a dry-run that only reports how many rows would change",
			},
			&cli.IntFlag{
				Name:  "sampleN",
				Value: 10,
				Usage: "print this many before/after examples (dry-run insight)",
			},
		},
		Action: p.action,
	}}, nil
}

type runStats struct {
	scanned    int
	pages      int
	candidates int
	changed    int
}

type change struct {
	id      string
	name    string
	oldEps  string
	newEps  string
	newVal  model.Episodes
	newGran model.NullReleaseGranularity
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
	sampleN := ctx.Int("sampleN")

	var (
		st      runStats
		lastID  string
		samples []change
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
				d.TorrentContent.ContentType,
				d.TorrentContent.Episodes,
			).
			Preload(d.TorrentContent.Torrent).
			Where(d.TorrentContent.ContentType.Eq(string(model.ContentTypeTvShow))).
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

		changes := p.computeChanges(rows, &st)

		for _, c := range changes {
			if len(samples) < sampleN {
				samples = append(samples, c)
			}
		}

		if write && len(changes) > 0 {
			if updErr := applyCorrections(ctx.Context, d, changes); updErr != nil {
				return updErr
			}
		}

		st.changed += len(changes)

		p.Logger.Infow("episodes-backfill progress",
			"mode", modeLabel(write),
			"scanned", st.scanned,
			"pages", st.pages,
			"candidates", st.candidates,
			"changed", st.changed,
			"lastID", lastID,
		)
	}

	for _, c := range samples {
		_, _ = fmt.Fprintf(ctx.App.Writer, "  %s: %q  %s -> %s\n", c.id, c.name, c.oldEps, c.newEps)
	}

	verb := "would change"
	if write {
		verb = "changed"
	}
	_, _ = fmt.Fprintf(ctx.App.Writer,
		"Done (%s): scanned=%d pages=%d candidates=%d %s=%d\n",
		modeLabel(write), st.scanned, st.pages, st.candidates, verb, st.changed)

	return nil
}

// computeChanges filters a page to candidate rows and recomputes their episodes
// through the production parser, returning the rows whose stored episodes
// differ from the recomputed value.
func (p Params) computeChanges(rows []*model.TorrentContent, st *runStats) []change {
	var changes []change
	for _, row := range rows {
		name := row.Torrent.Name
		if !candidateNameRegex.MatchString(name) {
			continue
		}
		st.candidates++

		attrs, err := parsers.ParseVideoContentWithOptions(
			row.Torrent,
			classification.Result{ContentAttributes: classification.ContentAttributes{
				ContentType: model.NewNullContentType(model.ContentTypeTvShow),
			}},
			parsers.ParseOptions{NoiseV2: p.ClassifierConfig.ParseNoiseV2},
		)
		if err != nil || len(attrs.Episodes) == 0 {
			// Parse failure or no episodes recovered — never overwrite a stored
			// value with nothing.
			continue
		}

		oldStr := row.Episodes.String()
		newStr := attrs.Episodes.String()
		if oldStr == newStr {
			continue
		}

		changes = append(changes, change{
			id:     row.ID,
			name:   name,
			oldEps: oldStr,
			newEps: newStr,
			newVal: attrs.Episodes,
			// Rewriting episodes invalidates the granularity stamped from the
			// corrupt value (E35E36 stored as E35 was labelled 'episode') —
			// re-derive it from the corrected episodes in the same pass.
			newGran: model.DeriveReleaseGranularity(
				model.NewNullContentType(model.ContentTypeTvShow), attrs.Episodes, name),
		})
	}
	return changes
}

// applyCorrections updates the episodes column and its derived
// release_granularity for the changed rows. UpdateColumn is used deliberately
// so the backfill does NOT bump updated_at — this is a metadata correction,
// not a re-classification.
func applyCorrections(ctx context.Context, d *dao.Query, changes []change) error {
	return d.Transaction(func(tx *dao.Query) error {
		for _, c := range changes {
			if _, err := tx.TorrentContent.WithContext(ctx).
				Where(d.TorrentContent.ID.Eq(c.id)).
				UpdateColumn(d.TorrentContent.Episodes, c.newVal); err != nil {
				return err
			}
			if _, err := tx.TorrentContent.WithContext(ctx).
				Where(d.TorrentContent.ID.Eq(c.id)).
				UpdateColumn(d.TorrentContent.ReleaseGranularity, c.newGran); err != nil {
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

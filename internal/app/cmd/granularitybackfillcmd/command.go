// Package granularitybackfillcmd populates the derived columns on
// torrent_contents — release_granularity (00037), release_date (00039),
// anime_absolute_episode (00040) and english_audio (00042) — for rows
// classified before they existed. The migration
// deliberately does NOT backfill (a multi-minute UPDATE inside the goose
// startup transaction would be killed by the :3333 healthcheck + autoheal and
// crash-loop the container — see the 00037 comment); this command applies the
// same model.DeriveReleaseGranularity the processor stamps inline, in bounded
// pages, resumable at any point because it only touches rows whose stored
// value differs from the derived one.
package granularitybackfillcmd

import (
	"context"
	"fmt"
	"io"

	"github.com/spencercnorton/bitagent/internal/anime"
	"github.com/spencercnorton/bitagent/internal/classifier/parsers"
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
		Name:    "derived-backfill",
		Aliases: []string{"granularity-backfill"},
		Usage: "Populate the derived torrent_contents columns (release_granularity, release_date, anime_absolute_episode, english_audio) for " +
			"rows classified before they existed, using the same derivations the classifier stamps inline",
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
				Usage: "persist labels; default is a dry-run that only reports per-label counts",
			},
		},
		Action: func(ctx *cli.Context) error {
			d, err := p.Dao.Get()
			if err != nil {
				return err
			}
			st, err := run(ctx.Context, d, opts{
				pageSize: ctx.Int("pageSize"),
				limit:    int(ctx.Uint("limit")),
				write:    ctx.Bool("write"),
			}, p.Logger)
			if err != nil {
				return err
			}
			report(ctx.App.Writer, st)
			return nil
		},
	}}, nil
}

type opts struct {
	pageSize int
	limit    int
	write    bool
}

type stats struct {
	scanned    int
	pages      int
	changed    int
	dates      int
	absEps     int
	engAudio   int
	animeFlags int
	write      bool
	byLabel    map[string]int
}

// nullLabel is the bucket key for rows whose derived granularity is NULL but
// whose stored value is not — possible only after a derivation-rule change,
// but handled so re-runs always converge on the current rule.
const nullLabel = "<null>"

func run(ctx context.Context, d *dao.Query, o opts, logger *zap.SugaredLogger) (stats, error) {
	if o.pageSize <= 0 {
		o.pageSize = 1000
	}

	st := stats{write: o.write, byLabel: map[string]int{}}
	var lastID string

	for o.limit <= 0 || st.scanned < o.limit {
		remaining := o.pageSize
		if o.limit > 0 && o.limit-st.scanned < remaining {
			remaining = o.limit - st.scanned
		}

		rows, findErr := d.TorrentContent.WithContext(ctx).
			Select(
				d.TorrentContent.ID,
				d.TorrentContent.InfoHash,
				d.TorrentContent.ContentType,
				d.TorrentContent.Episodes,
				d.TorrentContent.ReleaseGranularity,
				d.TorrentContent.ReleaseDate,
				d.TorrentContent.IsAnime,
				d.TorrentContent.AnimeAbsoluteEpisode,
				d.TorrentContent.EnglishAudio,
				d.TorrentContent.EnglishAudioSource,
			).
			Preload(d.TorrentContent.Torrent).
			// No content-type filter: the scan scope must match the processor's
			// stamp scope for every derived column. Each derivation self-gates
			// (granularity is tv-only inside DeriveReleaseGranularity; dates and
			// anime markers apply to any type, exactly like the inline path).
			Where(d.TorrentContent.ID.Gt(lastID)).
			Order(d.TorrentContent.ID).
			Limit(remaining).
			Find()
		if findErr != nil {
			return st, findErr
		}
		if len(rows) == 0 {
			break
		}

		st.pages++
		st.scanned += len(rows)
		lastID = rows[len(rows)-1].ID

		buckets := bucketChanges(rows)
		dates := dateChanges(rows)
		absEps := absoluteChanges(rows)
		engAudio := englishAudioChanges(rows)
		flagTrue, flagFalse := animeFlagChanges(rows)

		for label, ids := range buckets {
			st.byLabel[label] += len(ids)
			st.changed += len(ids)
		}
		st.dates += len(dates)
		st.absEps += len(absEps)
		st.engAudio += len(engAudio)
		st.animeFlags += len(flagTrue) + len(flagFalse)

		if o.write && (len(buckets) > 0 || len(dates) > 0 || len(absEps) > 0 ||
			len(engAudio) > 0 || len(flagTrue) > 0 || len(flagFalse) > 0) {
			if updErr := applyChanges(ctx, d, buckets, dates, absEps, engAudio, flagTrue, flagFalse); updErr != nil {
				return st, updErr
			}
		}

		logger.Infow("granularity-backfill progress",
			"mode", modeLabel(o.write),
			"scanned", st.scanned,
			"pages", st.pages,
			"changed", st.changed,
			"dates", st.dates,
			"absEps", st.absEps,
			"engAudio", st.engAudio,
			"animeFlags", st.animeFlags,
			"lastID", lastID,
		)
	}

	return st, nil
}

// bucketChanges derives the granularity for each row and groups the IDs whose
// stored value differs, keyed by target label, so writes are one batched
// UPDATE per label per page instead of one per row.
func bucketChanges(rows []*model.TorrentContent) map[string][]string {
	buckets := map[string][]string{}
	for _, row := range rows {
		want := model.DeriveReleaseGranularity(row.ContentType, row.Episodes, row.Torrent.Name)
		// semantic compare — struct equality would also compare the Set flag,
		// which differs between a zero value and a scanned SQL NULL, and would
		// make every unknown row rewrite NULL→NULL on every run.
		if want.Valid == row.ReleaseGranularity.Valid &&
			(!want.Valid || want.ReleaseGranularity == row.ReleaseGranularity.ReleaseGranularity) {
			continue
		}
		key := nullLabel
		if want.Valid {
			key = string(want.ReleaseGranularity)
		}
		buckets[key] = append(buckets[key], row.ID)
	}
	return buckets
}

// dateChange pairs a row with its newly derived release_date.
type dateChange struct {
	id   string
	date model.Date
}

// dateChanges derives release_date from each row's name via the same
// parsers.ParseDate the classifier's parse_date action uses. Dates are only
// ever FILLED or REPLACED, never cleared: a stored date the name cannot
// re-derive may have come from a hint, and clearing it would lose information
// the backfill cannot reconstruct.
func dateChanges(rows []*model.TorrentContent) []dateChange {
	var changes []dateChange
	for _, row := range rows {
		want := parsers.ParseDate(row.Torrent.Name)
		if !want.IsValid() || want == row.ReleaseDate {
			continue
		}
		changes = append(changes, dateChange{id: row.ID, date: want})
	}
	return changes
}

// absoluteChange pairs a row with its newly derived anime absolute episode
// (zero-valued NullUint = clear).
type absoluteChange struct {
	id  string
	val model.NullUint
}

// absoluteChanges derives anime_absolute_episode via the same anime.Detect
// the processor uses, gated on the full anime signal. Unlike dates, absolute
// episodes ARE cleared when no longer derived: anime.Detect is the single
// writer (no hint path), so full semantic diff keeps re-runs convergent on
// the current rule.
func absoluteChanges(rows []*model.TorrentContent) []absoluteChange {
	var changes []absoluteChange
	for _, row := range rows {
		sig := anime.Detect(row.Torrent.Name)
		want := model.NullUint{}
		if sig.IsAnimeIndependentOfAbsolute() && sig.AbsoluteEpisode > 0 && !sig.AbsoluteRange {
			want = model.NewNullUint(uint(sig.AbsoluteEpisode))
		}
		if want.Valid == row.AnimeAbsoluteEpisode.Valid &&
			(!want.Valid || want.Uint == row.AnimeAbsoluteEpisode.Uint) {
			continue
		}
		changes = append(changes, absoluteChange{id: row.ID, val: want})
	}
	return changes
}

// animeFlagChanges partitions rows whose stored is_anime disagrees with the
// current detector — keeping the (is_anime, anime_absolute_episode) pair
// coherent in one command instead of requiring a paired anime-backfill run.
func animeFlagChanges(rows []*model.TorrentContent) (toTrue, toFalse []string) {
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

// englishAudioChange pairs a row with its newly derived english_audio value
// (zero NullEnglishAudio + zero NullString = clear both).
type englishAudioChange struct {
	id  string
	val model.NullEnglishAudio
	src model.NullString
}

// englishAudioChanges derives english_audio via the same signals the
// processor stamps: anime-gated, deterministic name conventions. Full
// semantic diff incl. clearing for 'name'-sourced (and legacy NULL-source)
// rows — but an 'llm'-sourced row is NEVER cleared or overwritten without a
// fresh deterministic signal: this backfill has no LLM access, so it can
// only preserve that tier, not recompute it (the T4 precedence rule and the
// second half of the tripwire on model.DeriveEnglishAudio).
func englishAudioChanges(rows []*model.TorrentContent) []englishAudioChange {
	var changes []englishAudioChange
	for _, row := range rows {
		sig := anime.Detect(row.Torrent.Name)
		want := model.DeriveEnglishAudio(sig.IsAnime(), sig.EnglishAudioSignal, sig.EnglishSubSignal)
		var wantSrc model.NullString
		if want.Valid {
			wantSrc = model.NewNullString(model.EnglishAudioSourceName)
		} else if row.EnglishAudioSource.Valid &&
			row.EnglishAudioSource.String == model.EnglishAudioSourceLLM {
			continue
		}
		if want.Valid == row.EnglishAudio.Valid &&
			(!want.Valid || want.EnglishAudio == row.EnglishAudio.EnglishAudio) &&
			wantSrc == row.EnglishAudioSource {
			continue
		}
		changes = append(changes, englishAudioChange{id: row.ID, val: want, src: wantSrc})
	}
	return changes
}

// applyChanges writes granularity buckets and per-row dates with UpdateColumn
// — deliberately NOT Update, so the backfill does not bump updated_at: this is
// a metadata population, not a re-classification.
func applyChanges(ctx context.Context, d *dao.Query, buckets map[string][]string, dates []dateChange, absEps []absoluteChange, engAudio []englishAudioChange, flagTrue, flagFalse []string) error {
	return d.Transaction(func(tx *dao.Query) error {
		for label, ids := range buckets {
			val := model.NullReleaseGranularity{}
			if label != nullLabel {
				val = model.NewNullReleaseGranularity(model.ReleaseGranularity(label))
			}
			if _, err := tx.TorrentContent.WithContext(ctx).
				Where(d.TorrentContent.ID.In(ids...)).
				UpdateColumn(d.TorrentContent.ReleaseGranularity, val); err != nil {
				return err
			}
		}
		for _, dc := range dates {
			if _, err := tx.TorrentContent.WithContext(ctx).
				Where(d.TorrentContent.ID.Eq(dc.id)).
				UpdateColumn(d.TorrentContent.ReleaseDate, dc.date); err != nil {
				return err
			}
		}
		for _, ac := range absEps {
			if _, err := tx.TorrentContent.WithContext(ctx).
				Where(d.TorrentContent.ID.Eq(ac.id)).
				UpdateColumn(d.TorrentContent.AnimeAbsoluteEpisode, ac.val); err != nil {
				return err
			}
		}
		for _, ec := range engAudio {
			if _, err := tx.TorrentContent.WithContext(ctx).
				Where(d.TorrentContent.ID.Eq(ec.id)).
				UpdateColumn(d.TorrentContent.EnglishAudio, ec.val); err != nil {
				return err
			}
			if _, err := tx.TorrentContent.WithContext(ctx).
				Where(d.TorrentContent.ID.Eq(ec.id)).
				UpdateColumn(d.TorrentContent.EnglishAudioSource, ec.src); err != nil {
				return err
			}
		}
		if len(flagTrue) > 0 {
			if _, err := tx.TorrentContent.WithContext(ctx).
				Where(d.TorrentContent.ID.In(flagTrue...)).
				UpdateColumn(d.TorrentContent.IsAnime, true); err != nil {
				return err
			}
		}
		if len(flagFalse) > 0 {
			if _, err := tx.TorrentContent.WithContext(ctx).
				Where(d.TorrentContent.ID.In(flagFalse...)).
				UpdateColumn(d.TorrentContent.IsAnime, false); err != nil {
				return err
			}
		}
		return nil
	})
}

func report(w io.Writer, st stats) {
	verb := "would set"
	if st.write {
		verb = "set"
	}
	_, _ = fmt.Fprintf(w, "Done (%s): scanned=%d pages=%d %s=%d dates=%d absEps=%d engAudio=%d animeFlags=%d\n",
		modeLabel(st.write), st.scanned, st.pages, verb, st.changed, st.dates, st.absEps, st.engAudio, st.animeFlags)
	for label, n := range st.byLabel {
		_, _ = fmt.Fprintf(w, "  %-16s %d\n", label, n)
	}
}

func modeLabel(write bool) string {
	if write {
		return "write"
	}
	return "dry-run"
}

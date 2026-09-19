// Package evalreplaycmd replays a frozen evaluation segment through the
// classifier (roadmap P0/WI0.1) without writing anything. Each frozen torrent
// runs the real, fully-wired workflow with per-pass attribution (evaltrace)
// and the canonical-label preemption bypassed by default, so gold rows
// exercise the matching ladder instead of the evidence shortcut. Results are
// written as deterministic JSONL (rows ordered by info_hash) plus a summary,
// making byte-stable replays diffable across classifier versions.
package evalreplaycmd

import (
	"database/sql/driver"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/spencercnorton/bitagent/internal/classifier"
	"github.com/spencercnorton/bitagent/internal/classifier/classification"
	"github.com/spencercnorton/bitagent/internal/classifier/evaltrace"
	"github.com/spencercnorton/bitagent/internal/database/dao"
	"github.com/spencercnorton/bitagent/internal/lazy"
	"github.com/spencercnorton/bitagent/internal/model"
	"github.com/spencercnorton/bitagent/internal/protocol"
	"github.com/urfave/cli/v2"
	"go.uber.org/fx"
	"go.uber.org/zap"
)

type Params struct {
	fx.In
	ClassifierConfig classifier.Config
	Pool             lazy.Lazy[*pgxpool.Pool]
	Dao              lazy.Lazy[*dao.Query]
	Runner           lazy.Lazy[classifier.Runner]
	Logger           *zap.SugaredLogger
}

type Result struct {
	fx.Out
	Command *cli.Command `group:"commands"`
}

func New(p Params) (Result, error) {
	return Result{Command: &cli.Command{
		Name:  "eval-replay",
		Usage: "Replay a frozen eval segment through the classifier (dry, deterministic, attributed)",
		Flags: []cli.Flag{
			&cli.StringFlag{
				Name:     "segment",
				Required: true,
				Usage:    "eval_frozen segment to replay",
			},
			&cli.StringFlag{
				Name:     "out",
				Required: true,
				Usage:    "output JSONL path (one decision per frozen row, ordered by info_hash)",
			},
			&cli.BoolFlag{
				Name:  "apisEnabled",
				Usage: "allow TMDB API candidates (default local-only; LLM stages are always off)",
			},
			&cli.BoolFlag{
				Name:  "canonicalPreempt",
				Usage: "let arr canonical labels preempt the workflow (default bypassed so the ladder is exercised)",
			},
			&cli.IntFlag{
				Name:  "batchSize",
				Value: 200,
				Usage: "frozen rows to load per batch",
			},
			&cli.Int64Flag{
				Name:  "limit",
				Value: 0,
				Usage: "stop after this many rows (0 = whole segment)",
			},
		},
		Action: p.action,
	}}, nil
}

type frozenRow struct {
	infoHash []byte
	name     string
	expected map[string]any
}

type decision struct {
	InfoHash    string   `json:"infoHash"`
	Name        string   `json:"name"`
	Outcome     string   `json:"outcome"`
	ContentType string   `json:"contentType,omitempty"`
	Source      string   `json:"source,omitempty"`
	ContentID   string   `json:"contentId,omitempty"`
	Title       string   `json:"title,omitempty"`
	AttachedBy  string   `json:"attachedBy,omitempty"`
	Stages      []string `json:"stages,omitempty"`
	ExpectedID  string   `json:"expectedId,omitempty"`
	Agree       *bool    `json:"agree,omitempty"`
	Error       string   `json:"error,omitempty"`
}

type summary struct {
	total, missing, matched, typedOnly, unmatched, deleted, failed int
	agree, disagree, expectedTotal, expectedMatched                int
	attachedBy                                                     map[string]int
}

func (p Params) action(ctx *cli.Context) error {
	pool, err := p.Pool.Get()
	if err != nil {
		return err
	}

	d, err := p.Dao.Get()
	if err != nil {
		return err
	}

	runner, err := p.Runner.Get()
	if err != nil {
		return err
	}

	out, err := os.Create(ctx.String("out"))
	if err != nil {
		return err
	}
	defer out.Close()

	flags := classifier.Flags{
		"apis_enabled":      ctx.Bool("apisEnabled"),
		"llm_match_enabled": false,
		"llm_stage_enabled": false,
	}

	segment := ctx.String("segment")
	batchSize := ctx.Int("batchSize")

	if batchSize <= 0 {
		batchSize = 200
	}

	limit := ctx.Int64("limit")
	st := summary{attachedBy: map[string]int{}}
	enc := json.NewEncoder(out)

	var lastHash []byte

	for limit <= 0 || int64(st.total) < limit {
		rows, err := loadFrozenBatch(ctx, pool, segment, lastHash, batchSize)
		if err != nil {
			return err
		}

		if len(rows) == 0 {
			break
		}

		lastHash = rows[len(rows)-1].infoHash

		if limit > 0 && int64(st.total+len(rows)) > limit {
			rows = rows[:limit-int64(st.total)]
		}

		torrents, err := loadTorrents(ctx, d, rows)
		if err != nil {
			return err
		}

		for _, row := range rows {
			dec := p.replayOne(ctx, runner, flags, row, torrents, &st)
			if err := enc.Encode(dec); err != nil {
				return err
			}
		}

		st.total += len(rows)

		p.Logger.Infow("eval-replay progress", "segment", segment, "done", st.total)
	}

	_, _ = fmt.Fprintf(ctx.App.Writer,
		"Replay %q: total=%d matched=%d typedOnly=%d unmatched=%d deleted=%d failed=%d missingTorrent=%d\n",
		segment, st.total, st.matched, st.typedOnly, st.unmatched, st.deleted, st.failed, st.missing)

	if st.expectedTotal > 0 {
		_, _ = fmt.Fprintf(ctx.App.Writer,
			"vs expected (all rows with resolved tmdb id): n=%d agree=%d disagree=%d autoMatched=%d notAutoMatched=%d\n",
			st.expectedTotal, st.agree, st.disagree, st.expectedMatched, st.expectedTotal-st.expectedMatched)
	}

	for stage, n := range st.attachedBy {
		_, _ = fmt.Fprintf(ctx.App.Writer, "attributed %-40s %d\n", stage, n)
	}

	return nil
}

func loadFrozenBatch(
	ctx *cli.Context,
	pool *pgxpool.Pool,
	segment string,
	after []byte,
	batchSize int,
) ([]frozenRow, error) {
	rows, err := pool.Query(ctx.Context, `
		SELECT info_hash, name, coalesce(expected, 'null'::jsonb)
		FROM eval_frozen
		WHERE segment = $1 AND ($2::bytea IS NULL OR info_hash > $2)
		ORDER BY info_hash
		LIMIT $3`, segment, after, batchSize)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []frozenRow

	for rows.Next() {
		var (
			r        frozenRow
			expected []byte
		)

		if err := rows.Scan(&r.infoHash, &r.name, &expected); err != nil {
			return nil, err
		}

		_ = json.Unmarshal(expected, &r.expected)
		out = append(out, r)
	}

	return out, rows.Err()
}

func loadTorrents(
	ctx *cli.Context,
	d *dao.Query,
	rows []frozenRow,
) (map[protocol.ID]*torrentRecord, error) {
	valuers := make([]driver.Valuer, 0, len(rows))

	for _, r := range rows {
		var id protocol.ID
		copy(id[:], r.infoHash)
		valuers = append(valuers, id)
	}

	found, err := d.Torrent.WithContext(ctx.Context).
		Preload(d.Torrent.Files).
		Preload(d.Torrent.Hint).
		Preload(d.Torrent.Sources).
		Where(d.Torrent.InfoHash.In(valuers...)).
		Find()
	if err != nil {
		return nil, err
	}

	out := make(map[protocol.ID]*torrentRecord, len(found))
	for _, t := range found {
		out[t.InfoHash] = &torrentRecord{torrent: *t}
	}

	return out, nil
}

type torrentRecord struct {
	torrent model.Torrent
}

func (p Params) replayOne(
	ctx *cli.Context,
	runner classifier.Runner,
	flags classifier.Flags,
	row frozenRow,
	torrents map[protocol.ID]*torrentRecord,
	st *summary,
) decision {
	dec := decision{InfoHash: hex.EncodeToString(row.infoHash), Name: row.name}

	expectedID, _ := row.expected["tmdb_id"].(string)
	expectedType, _ := row.expected["tmdb_type"].(string)
	dec.ExpectedID = expectedID

	var id protocol.ID
	copy(id[:], row.infoHash)

	rec, ok := torrents[id]
	if !ok {
		dec.Outcome = "missing_torrent"
		st.missing++
		scoreExpectedIdentity(&dec, expectedID, expectedType, st)

		return dec
	}

	trace := &evaltrace.Trace{SkipCanonicalPreempt: !ctx.Bool("canonicalPreempt")}
	runCtx := evaltrace.With(ctx.Context, trace)

	cl, err := runner.Run(runCtx, p.ClassifierConfig.Workflow, flags, rec.torrent)

	dec.Stages = trace.Stages()
	dec.AttachedBy = trace.AttachedBy()

	switch {
	case errors.Is(err, classification.ErrDeleteTorrent):
		dec.Outcome = "delete"
		st.deleted++
	case err != nil:
		dec.Outcome = "failed"
		dec.Error = err.Error()
		st.failed++
	case cl.Content != nil:
		dec.Outcome = "matched"
		dec.ContentType = string(cl.Content.Type)
		dec.Source = cl.Content.Source
		dec.ContentID = cl.Content.ID
		dec.Title = cl.Content.Title
		st.matched++
		st.attachedBy[dec.AttachedBy]++
	case cl.ContentType.Valid:
		dec.Outcome = "typed_only"
		dec.ContentType = string(cl.ContentType.ContentType)
		st.typedOnly++
	default:
		dec.Outcome = "unmatched"
		st.unmatched++
	}

	scoreExpectedIdentity(&dec, expectedID, expectedType, st)

	return dec
}

// scoreExpectedIdentity compares every replay decision that has a resolved
// TMDB expectation. The denominator is the frozen expectation set, not the
// subset that the current classifier happened to match: abstentions, deletes,
// failures and missing live torrent rows are therefore explicit disagreements.
// expectedMatched is tracked separately so output consumers can distinguish
// auto-match coverage from identity accuracy.
func scoreExpectedIdentity(dec *decision, expectedID, expectedType string, st *summary) {
	if expectedID == "" {
		return
	}

	st.expectedTotal++
	if dec.Outcome == "matched" {
		st.expectedMatched++
	}

	agree := dec.Outcome == "matched" &&
		dec.Source == "tmdb" &&
		dec.ContentID == expectedID &&
		(expectedType == "" || dec.ContentType == expectedType)
	dec.Agree = &agree

	if agree {
		st.agree++
	} else {
		st.disagree++
	}
}

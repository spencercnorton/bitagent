// Package matcherevalcmd runs the LLM TMDB matcher over a sample of unmatched
// torrents in SHADOW (never attaches) and emits one structured JSON decision
// per torrent. It exists to measure matcher quality — most usefully to compare
// two models (e.g. the local qwen vs an OpenAI endpoint) over the exact same
// sample:
//
//	# run 1 — capture the sample it processed for exact replay
//	bitagent matcher-eval --limit 300 --out qwen.jsonl --writeSample sample.txt --label qwen
//	# run 2 — same torrents, different model via env, no attach either way
//	CLASSIFIER_LLM_MATCH_MODEL=gpt-5.4-nano \
//	CLASSIFIER_LLM_MATCH_ENDPOINT=https://api.openai.com/v1/chat/completions \
//	CLASSIFIER_LLM_MATCH_API_KEY=sk-... \
//	  bitagent matcher-eval --sample sample.txt --out nano.jsonl --label nano
//
// It routes through classifier.Runner.EvalMatch, i.e. the exact production
// decide path (local-mirror-first candidates, pack/adult/anime gates, rerank),
// so the measurement reflects what live matching would do. CLASSIFIER_LLM_MATCH_
// ENABLED must be true; ENABLE_LIVE is irrelevant here (nothing is attached).
// It spends its own allowance — three provider calls per sampled torrent
// (extract, local rerank, API rerank) — never the crawler's shared daily and
// monthly call ledger.
package matcherevalcmd

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/spencercnorton/bitagent/internal/classifier"
	"github.com/spencercnorton/bitagent/internal/classifier/llmmatch"
	"github.com/spencercnorton/bitagent/internal/database/dao"
	"github.com/spencercnorton/bitagent/internal/lazy"
	"github.com/spencercnorton/bitagent/internal/model"
	"github.com/urfave/cli/v2"
	"go.uber.org/fx"
	"go.uber.org/zap"
)

type Params struct {
	fx.In
	Dao      lazy.Lazy[*dao.Query]
	Runner   lazy.Lazy[classifier.Runner]
	LLMMatch *llmmatch.Client
	Logger   *zap.SugaredLogger
}

type Result struct {
	fx.Out
	Command *cli.Command `group:"commands"`
}

func New(p Params) (Result, error) {
	return Result{Command: &cli.Command{
		Name: "matcher-eval",
		Usage: "Shadow-run the LLM matcher over an unmatched-torrent sample and emit per-decision JSONL " +
			"(model comes from CLASSIFIER_LLM_MATCH_* env; never attaches)",
		Flags: []cli.Flag{
			&cli.IntFlag{
				Name:  "limit",
				Value: 300,
				Usage: "number of torrents to evaluate when paging unmatched content (ignored with --sample)",
			},
			&cli.StringFlag{
				Name:  "sample",
				Usage: "path to a file of torrent_content ids (one per line) to evaluate — for exact A/B replay",
			},
			&cli.StringFlag{
				Name:  "writeSample",
				Usage: "write the torrent_content ids processed to this file (feed to --sample on the paired run)",
			},
			&cli.StringFlag{
				Name:  "out",
				Usage: "write JSONL decisions to this path (default: stdout)",
			},
			&cli.StringFlag{
				Name:  "label",
				Value: "eval",
				Usage: "label stamped on every emitted record (e.g. the model name)",
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

// evalRecord is one JSONL line: the matcher's decision for one torrent.
type evalRecord struct {
	Label        string  `json:"label"`
	Model        string  `json:"model"`
	TCID         string  `json:"tc_id"`
	InfoHash     string  `json:"info_hash"`
	Name         string  `json:"name"`
	ContentType  string  `json:"content_type"`
	Outcome      string  `json:"outcome"`
	ExtractTitle string  `json:"extract_title"`
	ExtractYear  int     `json:"extract_year"`
	ExtractType  string  `json:"extract_type"`
	IsAnime      bool    `json:"is_anime"`
	English      string  `json:"english,omitempty"`
	IsPack       bool    `json:"is_pack"`
	IsAdult      bool    `json:"is_adult"`
	CandSource   string  `json:"candidate_source"`
	NumCands     int     `json:"num_candidates"`
	MatchedID    int64   `json:"matched_tmdb_id"`
	MatchedTitle string  `json:"matched_title"`
	Confidence   float64 `json:"confidence"`
	GateReason   string  `json:"gate_reason,omitempty"`
	Err          string  `json:"error,omitempty"`
}

func (p Params) action(ctx *cli.Context) error {
	d, err := p.Dao.Get()
	if err != nil {
		return err
	}
	runner, err := p.Runner.Get()
	if err != nil {
		return err
	}
	if !p.LLMMatch.Enabled() {
		return errors.New(
			"llm matcher is disabled in this process; run with CLASSIFIER_LLM_MATCH_ENABLED=true " +
				"(shadow is fine — matcher-eval never attaches)",
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

	// Output sink.
	out := ctx.App.Writer
	if path := ctx.String("out"); path != "" {
		f, ferr := os.Create(path)
		if ferr != nil {
			return ferr
		}
		defer f.Close()
		bw := bufio.NewWriter(f)
		defer bw.Flush()
		out = bw
	}

	var sampleWriter *bufio.Writer
	if path := ctx.String("writeSample"); path != "" {
		f, ferr := os.Create(path)
		if ferr != nil {
			return ferr
		}
		defer f.Close()
		sampleWriter = bufio.NewWriter(f)
		defer sampleWriter.Flush()
	}

	rows, err := p.loadSample(ctx, d, typeNames)
	if err != nil {
		return err
	}

	p.LLMMatch.IsolateBudget(3 * len(rows))

	model := p.LLMMatch.Model()
	label := ctx.String("label")
	enc := json.NewEncoder(out)

	var counts = map[string]int{}
	var matched, localSrc, apiSrc int

	for i, tc := range rows {
		if ctx.Context.Err() != nil {
			return ctx.Context.Err()
		}

		dec, decErr := runner.EvalMatch(ctx.Context, tc.Torrent, tc.ContentType)

		rec := evalRecord{
			Label:        label,
			Model:        model,
			TCID:         tc.ID,
			InfoHash:     tc.InfoHash.String(),
			Name:         tc.Torrent.Name,
			ContentType:  string(tc.ContentType.ContentType),
			Outcome:      string(dec.Outcome),
			ExtractTitle: dec.Extract.Title,
			ExtractYear:  dec.Extract.Year,
			ExtractType:  dec.Extract.Type,
			IsAnime:      dec.Extract.IsAnime,
			English:      dec.Extract.English,
			IsPack:       dec.Extract.IsPack,
			IsAdult:      dec.Extract.IsAdult,
			CandSource:   dec.CandidateSource,
			NumCands:     len(dec.Candidates),
			MatchedID:    dec.MatchedID,
			MatchedTitle: dec.MatchedTitle,
			Confidence:   dec.Confidence,
			GateReason:   dec.GateReason,
		}
		if decErr != nil {
			rec.Err = decErr.Error()
			rec.Outcome = "error"
		}
		if encErr := enc.Encode(&rec); encErr != nil {
			return encErr
		}
		if sampleWriter != nil {
			_, _ = fmt.Fprintln(sampleWriter, tc.ID)
		}

		counts[rec.Outcome]++
		switch dec.CandidateSource {
		case "local":
			localSrc++
		case "api":
			apiSrc++
		}
		if dec.Outcome == classifier.OutcomeMatched {
			matched++
		}

		if (i+1)%25 == 0 {
			p.Logger.Infow("matcher-eval progress",
				"done", i+1, "total", len(rows), "matched", matched,
				"local", localSrc, "api", apiSrc)
		}
	}

	_, _ = fmt.Fprintf(ctx.App.ErrWriter,
		"Done: evaluated=%d matched=%d (%.1f%%) candidateSource{local=%d api=%d} outcomes=%v\n",
		len(rows), matched, pct(matched, len(rows)), localSrc, apiSrc, counts)

	return nil
}

// loadSample returns the torrent_contents to evaluate — either a fixed id list
// (--sample) or a page of the unmatched movie/tv backlog (--limit).
func (p Params) loadSample(ctx *cli.Context, d *dao.Query, typeNames []string) ([]*model.TorrentContent, error) {
	if path := ctx.String("sample"); path != "" {
		ids, err := readLines(path)
		if err != nil {
			return nil, err
		}
		if len(ids) == 0 {
			return nil, fmt.Errorf("sample file %q is empty", path)
		}
		return d.TorrentContent.WithContext(ctx.Context).
			Preload(d.TorrentContent.Torrent).
			Preload(d.TorrentContent.Torrent.Files).
			Where(d.TorrentContent.ID.In(ids...)).
			Order(d.TorrentContent.ID).
			Find()
	}

	limit := ctx.Int("limit")
	if limit <= 0 {
		limit = 300
	}
	return d.TorrentContent.WithContext(ctx.Context).
		Preload(d.TorrentContent.Torrent).
		Preload(d.TorrentContent.Torrent.Files).
		Where(
			d.TorrentContent.ContentType.In(typeNames...),
			d.TorrentContent.ContentID.IsNull(),
		).
		Order(d.TorrentContent.ID).
		Limit(limit).
		Find()
}

func readLines(path string) ([]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	var out []string
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line != "" {
			out = append(out, line)
		}
	}
	return out, sc.Err()
}

func pct(n, total int) float64 {
	if total == 0 {
		return 0
	}
	return 100 * float64(n) / float64(total)
}

func getContentTypes(ctx *cli.Context) ([]model.ContentType, error) {
	names := ctx.StringSlice("contentType")
	if len(names) == 0 {
		return []model.ContentType{model.ContentTypeMovie, model.ContentTypeTvShow}, nil
	}
	out := make([]model.ContentType, 0, len(names))
	for _, name := range names {
		ct, err := model.ParseContentType(name)
		if err != nil {
			return nil, err
		}
		if ct != model.ContentTypeMovie && ct != model.ContentTypeTvShow {
			return nil, fmt.Errorf("unsupported content type %q: the matcher only handles movie and tv_show", name)
		}
		out = append(out, ct)
	}
	return out, nil
}

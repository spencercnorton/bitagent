// Package evalfreezecmd freezes evaluation corpora into the eval_frozen table
// (roadmap P0/WI0.1). Segments are immutable measuring sticks: torrent names
// are copied at freeze time and ground truth (when available) is stored as
// jsonb, so classifier changes can be replayed against a stable corpus.
package evalfreezecmd

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/spencercnorton/bitagent/internal/lazy"
	"github.com/urfave/cli/v2"
	"go.uber.org/fx"
	"go.uber.org/zap"
)

type Params struct {
	fx.In
	Pool   lazy.Lazy[*pgxpool.Pool]
	Logger *zap.SugaredLogger
}

type Result struct {
	fx.Out
	Command *cli.Command `group:"commands"`
}

func New(p Params) (Result, error) {
	return Result{Command: &cli.Command{
		Name:  "eval-freeze",
		Usage: "Freeze an evaluation corpus segment into eval_frozen",
		Flags: []cli.Flag{
			&cli.StringFlag{
				Name:     "segment",
				Required: true,
				Usage:    "segment name, e.g. gold-unmatched, residual-movietv, ab2k",
			},
			&cli.StringFlag{
				Name:  "fromCanonicalLabels",
				Usage: "freeze from arr canonical labels: unmatched | matched | all",
			},
			&cli.StringFlag{
				Name:  "tvdbMap",
				Usage: "path to a tvdb→tmdb map json ({\"<tvdbid>\": {\"tmdb_tv\": N, \"tmdb_movie\": N}}) used to resolve expected ids",
			},
			&cli.StringFlag{
				Name:  "fromFile",
				Usage: "freeze infohashes from a file: .json ({\"sample\": [{\"infoHash\": ...}]} or bare array) or text lines whose first field is a 40-hex hash",
			},
			&cli.BoolFlag{
				Name:  "replace",
				Usage: "delete existing rows for the segment before freezing",
			},
		},
		Action: p.action,
	}}, nil
}

func (p Params) action(ctx *cli.Context) error {
	fromLabels := ctx.String("fromCanonicalLabels")
	fromFile := ctx.String("fromFile")

	if (fromLabels == "") == (fromFile == "") {
		return fmt.Errorf("exactly one of --fromCanonicalLabels or --fromFile is required")
	}

	pool, err := p.Pool.Get()
	if err != nil {
		return err
	}

	segment := ctx.String("segment")

	if ctx.Bool("replace") {
		if _, err := pool.Exec(ctx.Context, "DELETE FROM eval_frozen WHERE segment = $1", segment); err != nil {
			return err
		}
	}

	var inserted int64

	if fromLabels != "" {
		inserted, err = p.freezeFromCanonicalLabels(ctx.Context, pool, segment, fromLabels, ctx.String("tvdbMap"))
	} else {
		inserted, err = p.freezeFromFile(ctx.Context, pool, segment, fromFile)
	}

	if err != nil {
		return err
	}

	var total int64
	if err := pool.QueryRow(ctx.Context,
		"SELECT count(*) FROM eval_frozen WHERE segment = $1", segment).Scan(&total); err != nil {
		return err
	}

	_, _ = fmt.Fprintf(ctx.App.Writer, "Frozen segment %q: inserted %d (segment total %d)\n", segment, inserted, total)

	return nil
}

type tvdbMapping struct {
	TmdbTV    *int64 `json:"tmdb_tv"`
	TmdbMovie *int64 `json:"tmdb_movie"`
}

func loadTvdbMap(path string) (map[string]tvdbMapping, error) {
	if path == "" {
		return map[string]tvdbMapping{}, nil
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}

	m := make(map[string]tvdbMapping)
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, err
	}

	return m, nil
}

// resolveExpected turns an arr media reference into expected-jsonb fields.
// tmdb ids resolve directly; tvdb ids resolve through the provided map;
// sonarr/radarr internal ids stay unresolved (raw reference retained).
func resolveExpected(mediaType, mediaID string, tvdbMap map[string]tvdbMapping) map[string]any {
	expected := map[string]any{
		"media_type": mediaType,
		"media_id":   mediaID,
	}

	ns, id, ok := strings.Cut(mediaID, ":")
	if !ok {
		return expected
	}

	switch ns {
	case "tmdb":
		if mediaType == "movie" {
			expected["tmdb_type"] = "movie"
		} else {
			expected["tmdb_type"] = "tv_show"
		}

		expected["tmdb_id"] = id
	case "tvdb":
		m, found := tvdbMap[id]
		switch {
		case found && m.TmdbTV != nil:
			expected["tmdb_type"] = "tv_show"
			expected["tmdb_id"] = fmt.Sprintf("%d", *m.TmdbTV)
		case found && m.TmdbMovie != nil:
			expected["tmdb_type"] = "movie"
			expected["tmdb_id"] = fmt.Sprintf("%d", *m.TmdbMovie)
		}
	}

	return expected
}

func (p Params) freezeFromCanonicalLabels(
	ctx context.Context,
	pool *pgxpool.Pool,
	segment string,
	mode string,
	tvdbMapPath string,
) (int64, error) {
	var matchFilter string

	switch mode {
	case "unmatched":
		matchFilter = "AND tc.content_id IS NULL"
	case "matched":
		matchFilter = "AND tc.content_id IS NOT NULL"
	case "all":
		matchFilter = ""
	default:
		return 0, fmt.Errorf("--fromCanonicalLabels must be unmatched, matched or all (got %q)", mode)
	}

	tvdbMap, err := loadTvdbMap(tvdbMapPath)
	if err != nil {
		return 0, err
	}

	//nolint:gosec // matchFilter is one of three fixed literals selected above.
	rows, err := pool.Query(ctx, `
		SELECT DISTINCT ON (tcl.info_hash)
		       tcl.info_hash, coalesce(t.name,''), tcl.media_type, tcl.media_id,
		       coalesce(tc.content_id,''), coalesce(tc.content_type,'')
		FROM torrent_canonical_labels tcl
		JOIN torrent_contents tc ON tc.info_hash = tcl.info_hash
		LEFT JOIN torrents t ON t.info_hash = tcl.info_hash
		WHERE tcl.media_id IS NOT NULL AND tcl.media_id <> '' `+matchFilter+`
		ORDER BY tcl.info_hash`)
	if err != nil {
		return 0, err
	}
	defer rows.Close()

	type frozenRow struct {
		infoHash []byte
		name     string
		expected []byte
	}

	var pending []frozenRow

	for rows.Next() {
		var (
			infoHash                                     []byte
			name, mediaType, mediaID, attachedID, ctType string
		)

		if err := rows.Scan(&infoHash, &name, &mediaType, &mediaID, &attachedID, &ctType); err != nil {
			return 0, err
		}

		expected := resolveExpected(mediaType, mediaID, tvdbMap)
		if attachedID != "" {
			expected["attached_id"] = attachedID
			expected["attached_type"] = ctType
		}

		expectedJSON, err := json.Marshal(expected)
		if err != nil {
			return 0, err
		}

		pending = append(pending, frozenRow{infoHash: infoHash, name: name, expected: expectedJSON})
	}

	if err := rows.Err(); err != nil {
		return 0, err
	}

	var inserted int64

	for _, r := range pending {
		tag, err := pool.Exec(ctx, `
			INSERT INTO eval_frozen (segment, info_hash, name, expected)
			VALUES ($1, $2, $3, $4)
			ON CONFLICT (segment, info_hash) DO NOTHING`,
			segment, r.infoHash, r.name, r.expected)
		if err != nil {
			return inserted, err
		}

		inserted += tag.RowsAffected()
	}

	return inserted, nil
}

var hexHashRe = regexp.MustCompile(`^[0-9a-fA-F]{40}$`)

// parseHashesFromFile extracts 40-hex infohashes from a corpus file. JSON
// files may be a bare array or wrapped in a "sample" key, with items either
// hex strings or objects carrying an infoHash field. Text files contribute
// the first tab/pipe/whitespace-delimited field of each line when it parses
// as a hash.
func parseHashesFromFile(path string) ([]string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}

	var out []string

	seen := make(map[string]struct{})

	add := func(h string) {
		h = strings.ToLower(strings.TrimSpace(h))
		if !hexHashRe.MatchString(h) {
			return
		}

		if _, ok := seen[h]; ok {
			return
		}

		seen[h] = struct{}{}
		out = append(out, h)
	}

	trimmed := strings.TrimSpace(string(raw))
	if strings.HasPrefix(trimmed, "{") || strings.HasPrefix(trimmed, "[") {
		var doc any
		if err := json.Unmarshal(raw, &doc); err != nil {
			return nil, fmt.Errorf("parse %s as json: %w", path, err)
		}

		if m, ok := doc.(map[string]any); ok {
			if s, ok := m["sample"]; ok {
				doc = s
			}
		}

		items, ok := doc.([]any)
		if !ok {
			return nil, fmt.Errorf("%s: expected a json array (or object with a \"sample\" array)", path)
		}

		for _, item := range items {
			switch v := item.(type) {
			case string:
				add(v)
			case map[string]any:
				if h, ok := v["infoHash"].(string); ok {
					add(h)
				} else if h, ok := v["info_hash"].(string); ok {
					add(h)
				}
			}
		}

		return out, nil
	}

	for _, line := range strings.Split(trimmed, "\n") {
		for _, sep := range []string{"\t", "|"} {
			if first, _, ok := strings.Cut(line, sep); ok {
				line = first
			}
		}

		add(strings.Fields(line + " ")[0])
	}

	return out, nil
}

func (p Params) freezeFromFile(
	ctx context.Context,
	pool *pgxpool.Pool,
	segment string,
	path string,
) (int64, error) {
	hashes, err := parseHashesFromFile(path)
	if err != nil {
		return 0, err
	}

	if len(hashes) == 0 {
		return 0, fmt.Errorf("no infohashes found in %s", path)
	}

	var inserted int64

	for _, h := range hashes {
		raw, err := hex.DecodeString(h)
		if err != nil {
			continue
		}

		tag, err := pool.Exec(ctx, `
			INSERT INTO eval_frozen (segment, info_hash, name, expected)
			SELECT $1, $2, coalesce((SELECT name FROM torrents WHERE info_hash = $2), ''), NULL
			ON CONFLICT (segment, info_hash) DO NOTHING`,
			segment, raw)
		if err != nil {
			return inserted, err
		}

		inserted += tag.RowsAffected()
	}

	p.Logger.Infow("eval-freeze from file", "segment", segment, "parsed", len(hashes), "inserted", inserted)

	return inserted, nil
}

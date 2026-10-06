// Package serving defines reversible consumer visibility without changing stored data.
package serving

import (
	"fmt"
	"strings"
	"unicode"

	"github.com/spencercnorton/bitagent/internal/keywords"
	"gorm.io/gorm/clause"
)

// Config applies to consumer APIs, not processing or operator recovery reads.
type Config struct {
	ExcludeAdult bool `yaml:"exclude_adult"`
}

// Policy retains source-complete quarantine exclusion and optionally excludes
// known adult releases. Legacy incomplete snapshots need separate review.
type Policy struct {
	excludeAdult    bool
	strongPattern   string
	mediaExtensions []string
}

// NewPolicy uses only the core classifier's precision-vetted strong keywords.
// Weak keywords and the matcher's abstention heuristic are not serving evidence.
func NewPolicy(cfg Config, strongKeywords, mediaExtensions []string) (*Policy, error) {
	if len(strongKeywords) == 0 || len(mediaExtensions) == 0 {
		return nil, fmt.Errorf("serving adult evidence is empty")
	}
	r, err := keywords.NewRegexFromKeywords(strongKeywords...)
	if err != nil {
		return nil, fmt.Errorf("serving adult evidence: %w", err)
	}
	// PostgreSQL ARE does not support Go's Unicode property escape. Expand the
	// exact Go letter set instead of locale-dependent [:alpha:], which can widen
	// keyword boundaries on C-locale databases. Go's \d is ASCII, too.
	pattern := strings.ReplaceAll(r.String(), `\p{L}`, postgresLetterRanges())
	pattern = strings.ReplaceAll(pattern, `\d`, `0-9`)
	return &Policy{excludeAdult: cfg.ExcludeAdult, strongPattern: pattern, mediaExtensions: append([]string(nil), mediaExtensions...)}, nil
}

func postgresLetterRanges() string {
	var out strings.Builder
	write := func(lo, hi rune, stride uint32) {
		if stride == 1 {
			out.WriteRune(lo)
			if hi != lo {
				out.WriteByte('-')
				out.WriteRune(hi)
			}
			return
		}
		for r := lo; r <= hi; r += rune(stride) {
			out.WriteRune(r)
		}
	}
	for _, r := range unicode.L.R16 {
		write(rune(r.Lo), rune(r.Hi), uint32(r.Stride))
	}
	for _, r := range unicode.L.R32 {
		write(rune(r.Lo), rune(r.Hi), r.Stride)
	}
	return out.String()
}

// TorrentCondition is applied to the base query before client filtering, counts,
// grouping or pagination. A source-complete quarantine cannot reappear after
// recrawl. Snapshot expiry does not confer or revoke its visibility authority.
func (p *Policy) TorrentCondition(table string) clause.Expr {
	switch table {
	case "torrents", "torrent_contents", "torrent_files", "torrent_tags":
	default:
		panic("invalid serving torrent table")
	}
	root := table + ".info_hash"
	// Authentic source snapshots were added with the complete recovery producer.
	// Do not silently elevate legacy marker-only records into consumer authority.
	// Keep the qualifying snapshot durable rather than depending on a capture
	// receipt that may have a shorter retention period. Shape and hash bindings
	// reject incomplete/mismatched snapshots without unsafe JSON casts.
	sql := "NOT EXISTS (SELECT 1 FROM junkpurge_quarantine serving_q WHERE serving_q.info_hash = " + root + `
 AND jsonb_typeof(serving_q.sources_snapshot) = 'array'
 AND jsonb_typeof(serving_q.torrent_snapshot) = 'object'
 AND serving_q.torrent_snapshot->>'info_hash' = chr(92) || 'x' || encode(serving_q.info_hash, 'hex')
 AND serving_q.torrent_snapshot->>'name' = serving_q.torrent_name
 AND serving_q.torrent_snapshot->'private' = 'false'::jsonb
 AND (serving_q.files_snapshot IS NULL OR jsonb_typeof(serving_q.files_snapshot) = 'array')
 AND NOT EXISTS (SELECT 1 FROM jsonb_array_elements(CASE
  WHEN jsonb_typeof(serving_q.files_snapshot) = 'array' THEN serving_q.files_snapshot
  ELSE '[]'::jsonb END) serving_file
  WHERE jsonb_typeof(serving_file) <> 'object' OR serving_file->>'info_hash'
   IS DISTINCT FROM chr(92) || 'x' || encode(serving_q.info_hash, 'hex')
   OR jsonb_typeof(serving_file->'path') IS DISTINCT FROM 'string')
 AND NOT EXISTS (SELECT 1 FROM jsonb_array_elements(CASE
  WHEN jsonb_typeof(serving_q.sources_snapshot) = 'array' THEN serving_q.sources_snapshot
  ELSE '[]'::jsonb END) serving_source
  WHERE jsonb_typeof(serving_source) <> 'object' OR serving_source->>'info_hash'
   IS DISTINCT FROM chr(92) || 'x' || encode(serving_q.info_hash, 'hex')
   OR jsonb_typeof(serving_source->'source') IS DISTINCT FROM 'string'
   OR COALESCE(serving_source->>'source', '') = ''
   OR jsonb_typeof(serving_source->'source_metadata') IS DISTINCT FROM 'object'
   OR serving_source->'source_metadata'->>'key' IS DISTINCT FROM serving_source->>'source'))`
	var args []any
	if p.excludeAdult {
		sql += ` AND NOT EXISTS (
 SELECT 1 FROM torrent_contents serving_tc
 LEFT JOIN content serving_c ON serving_c.type = serving_tc.content_type
  AND serving_c.source = serving_tc.content_source AND serving_c.id = serving_tc.content_id
 WHERE serving_tc.info_hash = ` + root + `
  AND (serving_tc.content_type = 'xxx' OR serving_c.adult IS TRUE))`
		// This independently catches vetted strong native signals even when a
		// stale ordinary-content attachment prevented the classifier from typing XXX.
		// The positive media-byte test mirrors core XXX typing; arbitrary archives
		// and a single weak title token never become adult evidence here.
		placeholders := strings.TrimSuffix(strings.Repeat("?,", len(p.mediaExtensions)), ",")
		sql += ` AND NOT EXISTS (
 SELECT 1 FROM torrents serving_t WHERE serving_t.info_hash = ` + root + `
  AND (CASE WHEN serving_t.files_status = 'single'
   THEN serving_t.size > 0 AND serving_t.extension IN (` + placeholders + `)
   ELSE COALESCE((SELECT sum(CASE WHEN serving_f.extension IN (` + placeholders + `)
    THEN serving_f.size ELSE -serving_f.size END) FROM torrent_files serving_f
    WHERE serving_f.info_hash = serving_t.info_hash), 0) > 0 END)
  AND ((CASE WHEN serving_t.files_status = 'single' AND serving_t.extension IS NOT NULL
   THEN left(serving_t.name, greatest(char_length(serving_t.name)-char_length(serving_t.extension)-1, 0))
   ELSE serving_t.name END) ~* ? OR EXISTS (SELECT 1 FROM torrent_files serving_f
   WHERE serving_f.info_hash = serving_t.info_hash AND
    (CASE WHEN serving_f.extension IS NOT NULL
     THEN left(serving_f.path, greatest(char_length(serving_f.path)-char_length(serving_f.extension)-1, 0))
     ELSE serving_f.path END) ~* ?)))`
		for range 2 {
			for _, extension := range p.mediaExtensions {
				args = append(args, extension)
			}
		}
		args = append(args, p.strongPattern, p.strongPattern)
	}
	return clause.Expr{SQL: sql, Vars: args}
}

// ContentCondition leaves common metadata stored and hides only positive adult metadata.
func (p *Policy) ContentCondition() clause.Expr {
	if !p.excludeAdult {
		return clause.Expr{SQL: "TRUE"}
	}
	return clause.Expr{SQL: "content.type <> 'xxx' AND content.adult IS NOT TRUE"}
}

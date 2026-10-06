package cataloguepolicy

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

const maxFiles = 256
const maxObservations = 64

var ErrPrivate = errors.New("policy shadow: private source excluded")

type fileInput struct {
	Path      string
	UpdatedAt time.Time
}

// Input binds a receipt to the current raw name, members, privacy and public
// observations. It deliberately omits TMDB original/title language and legacy
// english_audio, which cannot prove a release's advertised track language.
type Input struct {
	InfoHash        []byte        `json:"info_hash"`
	Name            string        `json:"name"`
	UpdatedAt       time.Time     `json:"updated_at"`
	IsAnime         bool          `json:"is_anime"`
	Files           []fileInput   `json:"files"`
	IncompleteFiles bool          `json:"incomplete_files"`
	Observations    []Observation `json:"observations"`
}

// Receipt is a bounded source snapshot and two review-only decisions.
type Receipt struct {
	Mode               string               `json:"mode"`
	InputDigest        string               `json:"input_digest"`
	PolicyVersion      string               `json:"policy_version"`
	EvaluatedAt        time.Time            `json:"evaluated_at"`
	ExpiresAt          time.Time            `json:"expires_at"`
	Input              Input                `json:"input"`
	English            EnglishDecision      `json:"english"`
	Availability       AvailabilityDecision `json:"availability"`
	AvailabilityConfig AvailabilityConfig   `json:"availability_config"`
}

type Store struct{ pool *pgxpool.Pool }

func NewStore(pool *pgxpool.Pool) *Store { return &Store{pool: pool} }

// Candidates pages public hashes by their primary key. Limits are mandatory;
// no command offers a drain-all or enforcement mode.
func (s *Store) Candidates(ctx context.Context, after []byte, limit int) ([][]byte, error) {
	if limit < 1 || limit > 128 {
		return nil, fmt.Errorf("policy shadow: page must be 1..128")
	}
	rows, err := s.pool.Query(ctx, `select t.info_hash from torrents t
where t.info_hash > $1 and not t.private
and not exists (select 1 from torrent_canonical_labels c where c.info_hash=t.info_hash and lower(trim(c.category)) in ('private','bitgrab'))
and not exists (select 1 from torrent_tags g where g.info_hash=t.info_hash and lower(trim(g.name)) in ('private','bitgrab'))
order by t.info_hash limit $2`, after, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out [][]byte
	for rows.Next() {
		var h []byte
		if err = rows.Scan(&h); err != nil {
			return nil, err
		}
		out = append(out, h)
	}
	return out, rows.Err()
}

// Evaluate uses a repeatable source snapshot. Persisted evaluation serializes
// workers on a receipt row and uses PostgreSQL serializable isolation, so a
// the receipt is a consistent historical snapshot rather than a claim that
// source/privacy evidence cannot subsequently change. Every run rechecks it.
// Serialization failures are returned for a later ordinary run, not retried
// against invisible data. Dry-run is a read-only transaction.
func (s *Store) Evaluate(ctx context.Context, hash []byte, write, allowAnimeSubs bool, now time.Time, cfg AvailabilityConfig) (Receipt, error) {
	var zero Receipt
	if len(hash) != 20 {
		return zero, fmt.Errorf("policy shadow: expected 20-byte hash")
	}
	opts := pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly}
	if write {
		opts = pgx.TxOptions{IsoLevel: pgx.Serializable}
	}
	tx, err := s.pool.BeginTx(ctx, opts)
	if err != nil {
		return zero, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	if write {
		if _, err = tx.Exec(ctx, `insert into catalogue_policy_shadow(info_hash,evaluated_at,expires_at) values($1,'epoch','epoch') on conflict do nothing`, hash); err != nil {
			return zero, err
		}
	}
	var previousJSON []byte
	q := `select receipt from catalogue_policy_shadow where info_hash=$1`
	if write {
		q += ` for update`
	}
	err = tx.QueryRow(ctx, q, hash).Scan(&previousJSON)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return zero, err
	}
	in, err := loadInput(ctx, tx, hash, cfg)
	if err != nil {
		return zero, err
	}
	var prev Receipt
	if len(previousJSON) > 0 && string(previousJSON) != "{}" {
		if err = json.Unmarshal(previousJSON, &prev); err != nil {
			return zero, fmt.Errorf("policy shadow: previous receipt: %w", err)
		}
	}
	in.Observations = mergeObservations(in.Observations, prev.Input.Observations, now, cfg)
	digestBytes, err := json.Marshal(in)
	if err != nil {
		return zero, err
	}
	digest := sha256.Sum256(digestBytes)
	version := ParserVersion + "/" + AvailabilityVersion
	if allowAnimeSubs {
		version += "/anime-subs-draft"
	} else {
		version += "/audio-only-draft"
	}
	evidence := []EnglishEvidence{ParseEnglishClaims(in.Name, "release_name", in.UpdatedAt)}
	media := []EnglishEvidence{}
	for _, f := range in.Files {
		lower := strings.ToLower(f.Path)
		if strings.HasSuffix(lower, ".mkv") || strings.HasSuffix(lower, ".mp4") || strings.HasSuffix(lower, ".avi") || strings.HasSuffix(lower, ".ts") || strings.HasSuffix(lower, ".m2ts") {
			media = append(media, ParseEnglishClaims(f.Path, "media_file_name", f.UpdatedAt))
		}
	}
	// Multi-member packs require evidence for each member. For a single media
	// file the release claim and member claim form one advertised release.
	if len(media) > 1 {
		evidence = media
	} else if len(media) == 1 {
		media[0].Audio = combine(evidence[0].Audio, media[0].Audio)
		media[0].Subtitles = combine(evidence[0].Subtitles, media[0].Subtitles)
		media[0].Claims = append(evidence[0].Claims, media[0].Claims...)
		evidence = append(evidence, media[0])
		// Keep both original provenance receipts; evaluate the combined member.
	}
	eng := EvaluateEnglish(evidence, in.IsAnime, allowAnimeSubs, in.IncompleteFiles)
	if len(media) == 1 {
		eng = EvaluateEnglish(media, in.IsAnime, allowAnimeSubs, in.IncompleteFiles)
		eng.Evidence = evidence
	}
	availability := EvaluateAvailability(in.Observations, now, cfg)
	receipt := Receipt{Mode: "shadow", InputDigest: hex.EncodeToString(digest[:]), PolicyVersion: version, EvaluatedAt: now, ExpiresAt: now.Add(cfg.FreshFor), Input: in, English: eng, Availability: availability, AvailabilityConfig: cfg}
	if write {
		body, e := json.Marshal(receipt)
		if e != nil {
			return zero, e
		}
		changed, err := tx.Exec(ctx, `update catalogue_policy_shadow set input_digest=$2,policy_version=$3,receipt=$4::jsonb,evaluated_at=$5,expires_at=$6 where info_hash=$1 and evaluated_at <= $5`, hash, receipt.InputDigest, version, string(body), now, receipt.ExpiresAt)
		if err != nil {
			return zero, err
		}
		if changed.RowsAffected() != 1 {
			return zero, fmt.Errorf("policy shadow: newer evaluation already persisted")
		}
	}
	if err = tx.Commit(ctx); err != nil {
		return zero, err
	}
	return receipt, nil
}

func loadInput(ctx context.Context, tx pgx.Tx, hash []byte, cfg AvailabilityConfig) (Input, error) {
	in := Input{InfoHash: hash, Files: []fileInput{}, Observations: []Observation{}}
	var private bool
	var count int
	err := tx.QueryRow(ctx, `select t.name,t.updated_at,
 t.private or exists(select 1 from torrent_canonical_labels c where c.info_hash=t.info_hash and lower(trim(c.category)) in ('private','bitgrab'))
 or exists(select 1 from torrent_tags g where g.info_hash=t.info_hash and lower(trim(g.name)) in ('private','bitgrab')),
 exists(select 1 from torrent_contents c where c.info_hash=t.info_hash and c.is_anime),
 coalesce(t.files_count,0) from torrents t where info_hash=$1`, hash).Scan(&in.Name, &in.UpdatedAt, &private, &in.IsAnime, &count)
	if err != nil {
		return in, err
	}
	if private {
		return in, ErrPrivate
	}
	rows, err := tx.Query(ctx, `select path,updated_at from torrent_files where info_hash=$1 order by index limit $2`, hash, maxFiles+1)
	if err != nil {
		return in, err
	}
	for rows.Next() {
		var f fileInput
		if err = rows.Scan(&f.Path, &f.UpdatedAt); err != nil {
			rows.Close()
			return in, err
		}
		in.Files = append(in.Files, f)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return in, err
	}
	in.IncompleteFiles = len(in.Files) > maxFiles || count > maxFiles || (count > 1 && len(in.Files) < count)
	if len(in.Files) > maxFiles {
		in.Files = in.Files[:maxFiles]
	}
	var checked time.Time
	var positive *time.Time
	var known bool
	var seeders, leechers *int
	err = tx.QueryRow(ctx, `select checked_at,tracker_known,seeders,leechers,last_positive_at from torrent_tracker_seeds where info_hash=$1`, hash).Scan(&checked, &known, &seeders, &leechers, &positive)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return in, err
	}
	if err == nil {
		class := "unknown"
		qualified := known && seeders != nil && leechers != nil && *seeders >= 0 && *leechers >= 0
		if qualified {
			class = "zero"
			if *seeders > 0 || *leechers > 0 {
				class = "positive"
			}
		}
		in.Observations = append(in.Observations, newObservation("tracker", checked, class, qualified, cfg))
		if positive != nil {
			in.Observations = append(in.Observations, newObservation("tracker_positive_history", *positive, "positive", true, cfg))
		}
	}
	rows, err = tx.Query(ctx, `select created_at,seeders,leechers from torrents_torrent_sources where info_hash=$1 and source='dht'`, hash)
	if err != nil {
		return in, err
	}
	for rows.Next() {
		var at time.Time
		var s, l *int
		if err = rows.Scan(&at, &s, &l); err != nil {
			rows.Close()
			return in, err
		}
		if (s != nil && *s > 0) || (l != nil && *l > 0) {
			in.Observations = append(in.Observations, newObservation("dht_approximation", at, "positive", true, cfg))
		}
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return in, err
	}
	rows, err = tx.Query(ctx, `select id,observed_at,coalesce(raw_payload->>'state',''),coalesce(raw_payload->>'network_healthy','false') from label_evidence
where info_hash=$1 and source='qbittorrent' and source_kind='qb_state_observation' and lower(trim(coalesce(category,''))) not in ('private','bitgrab') order by observed_at desc,id desc limit $2`, hash, maxObservations)
	if err != nil {
		return in, err
	}
	for rows.Next() {
		var id int64
		var at time.Time
		var state, healthy string
		if err = rows.Scan(&id, &at, &state, &healthy); err != nil {
			rows.Close()
			return in, err
		}
		class := "unknown"
		qualified := false
		switch strings.ToLower(state) {
		case "seeding", "uploading", "downloading", "forcedup", "stalledup":
			class = "positive"
			qualified = true
		case "stalleddl", "metadl":
			if healthy == "true" {
				class = "stall"
				qualified = true
			}
		}
		o := newObservation("public_qb", at, class, qualified, cfg)
		o.ID = fmt.Sprintf("public_qb:%d", id)
		in.Observations = append(in.Observations, o)
	}
	err = rows.Err()
	rows.Close()
	return in, err
}

func newObservation(source string, at time.Time, class string, qualified bool, cfg AvailabilityConfig) Observation {
	return Observation{ID: source + ":" + at.UTC().Format(time.RFC3339Nano), Source: source, Class: class, ObservedAt: at, ExpiresAt: at.Add(cfg.FreshFor), Qualified: qualified}
}

func mergeObservations(current, previous []Observation, now time.Time, cfg AvailabilityConfig) []Observation {
	out := []Observation{}
	seen := map[string]bool{}
	for _, list := range [][]Observation{current, previous} {
		for _, o := range list {
			if seen[o.ID] || o.Private || o.ObservedAt.After(now) || now.Sub(o.ObservedAt) > cfg.RecentPositiveFor {
				continue
			}
			seen[o.ID] = true
			out = append(out, o)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].ObservedAt.Equal(out[j].ObservedAt) {
			return out[i].ID < out[j].ID
		}
		return out[i].ObservedAt.After(out[j].ObservedAt)
	})
	if len(out) > maxObservations {
		out = out[:maxObservations]
	}
	return out
}

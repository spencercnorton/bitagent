package processors

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"github.com/jackc/pgx/v5"
	"github.com/spencercnorton/bitagent/internal/catalogueguard"
	"github.com/spencercnorton/bitagent/internal/classifier/classification"
	"github.com/spencercnorton/bitagent/internal/classifier/parsers"
	"github.com/spencercnorton/bitagent/internal/evidence/priors"
	llmwork "github.com/spencercnorton/bitagent/internal/legacyresults"
	"github.com/spencercnorton/bitagent/internal/model"
	"github.com/spencercnorton/bitagent/internal/protocol"
	v1 "github.com/spencercnorton/bitagent/processorapi/v1"
	"net/url"
	"reflect"
	"strings"
	"time"
)

// A single SQL snapshot binds metadata, file evidence, catalogue facts and
// independent privacy/authority. Volatile counts and timestamps are excluded.
const sourceSQL = `SELECT jsonb_build_object(
 'name',t.name,'size',t.size,'private',t.private,'files_status',t.files_status,
 'file_count',t.files_count,'extension',t.extension,
 'files',COALESCE((SELECT jsonb_agg(x ORDER BY x->>'path',x->>'index') FROM
  (SELECT jsonb_build_object('path',path,'size',size,'index',"index",'extension',extension) x
   FROM torrent_files WHERE info_hash=t.info_hash ORDER BY "index",path LIMIT 257) f),'[]'),
 'receipt',(SELECT to_jsonb(r)-'recorded_at' FROM processor_core_receipts r WHERE r.info_hash=t.info_hash),
 'reprocess',(SELECT request_id FROM processor_reprocess_requests r WHERE r.info_hash=t.info_hash AND r.state IN('ready','queued','settled','core_only') ORDER BY created_at DESC,request_id DESC LIMIT 1),
 'hint',(SELECT to_jsonb(h)-'created_at'-'updated_at' FROM torrent_hints h WHERE h.info_hash=t.info_hash),
 'contents',COALESCE((SELECT jsonb_agg(x ORDER BY x->>'id') FROM
  (SELECT to_jsonb(c)-'created_at'-'updated_at'-'seeders'-'leechers'-'published_at'-'tsv' x
   FROM torrent_contents c WHERE c.info_hash=t.info_hash ORDER BY id LIMIT 2) c),'[]'),
 'tags',COALESCE((SELECT jsonb_agg(name ORDER BY name) FROM torrent_tags WHERE info_hash=t.info_hash),'[]'),
 'canonical',(SELECT to_jsonb(c)-'created_at'-'updated_at' FROM torrent_canonical_labels c WHERE c.info_hash=t.info_hash),
 'private_evidence',EXISTS(SELECT 1 FROM label_evidence WHERE info_hash=t.info_hash AND ` + `%s` + `),
 'held',EXISTS(SELECT 1 FROM torrent_verdict_state WHERE info_hash=t.info_hash AND verdict IN('quarantined','blacklisted','tombstoned'))
 OR EXISTS(SELECT 1 FROM junkpurge_quarantine WHERE info_hash=t.info_hash AND expired_at IS NULL)
) FROM torrents t WHERE t.info_hash=$1`

type sourceFile struct {
	Path      string  `json:"path"`
	Size      int64   `json:"size"`
	Index     int     `json:"index"`
	Extension *string `json:"extension"`
}
type source struct {
	Receipt         json.RawMessage   `json:"receipt"`
	Name            string            `json:"name"`
	Size            int64             `json:"size"`
	Private         bool              `json:"private"`
	FilesStatus     string            `json:"files_status"`
	FileCount       *int64            `json:"file_count"`
	Extension       *string           `json:"extension"`
	Files           []sourceFile      `json:"files"`
	Hint            json.RawMessage   `json:"hint"`
	Contents        []json.RawMessage `json:"contents"`
	Tags            []string          `json:"tags"`
	Canonical       json.RawMessage   `json:"canonical"`
	PrivateEvidence bool              `json:"private_evidence"`
	Held            bool              `json:"held"`
}
type contentFact struct {
	ID        string         `json:"id"`
	Type      *string        `json:"content_type"`
	Source    *string        `json:"content_source"`
	ContentID *string        `json:"content_id"`
	Episodes  model.Episodes `json:"episodes"`
}

func readSource(ctx context.Context, tx pgx.Tx, hash []byte, lock bool) (source, json.RawMessage, error) {
	var out source
	if lock {
		// Lock order matches core persistence: independent authority, then parent
		// and child rows. The short transaction has a deadline and no network I/O.
		if _, e := tx.Exec(ctx, `LOCK TABLE label_evidence,torrent_canonical_labels,torrent_verdict_state,junkpurge_quarantine,torrent_files,torrent_hints,torrent_contents,torrent_tags IN SHARE MODE`); e != nil {
			return out, nil, e
		}
		if _, e := tx.Exec(ctx, `SELECT info_hash FROM torrents WHERE info_hash=$1 FOR UPDATE`, hash); e != nil {
			return out, nil, e
		}
	}
	q := strings.Replace(sourceSQL, "%s", catalogueguard.QBPrivacySQL("source", "category", "$2"), 1)
	var raw []byte
	if e := tx.QueryRow(ctx, q, hash, catalogueguard.TagWhitespace).Scan(&raw); e != nil {
		return out, nil, e
	}
	if len(raw) > 262144 {
		return out, nil, errors.New("source_too_large")
	}
	if e := json.Unmarshal(raw, &out); e != nil {
		return out, nil, e
	}
	return out, canonical(raw), nil
}
func (s source) admitted() bool {
	if s.Private || s.PrivateEvidence || s.Held || len(s.Files) > 256 || len(s.Contents) != 1 || s.Size < 0 || len(s.Name) > 4096 {
		return false
	}
	for _, tag := range s.Tags {
		if catalogueguard.ProtectedTag(tag, false) {
			return false
		}
	}
	var c struct {
		MediaID  *string `json:"media_id"`
		Category *string `json:"category"`
	}
	_ = json.Unmarshal(s.Canonical, &c)
	if c.MediaID != nil && *c.MediaID != "" {
		return false
	}
	if c.Category != nil {
		cat := strings.ToLower(strings.TrimSpace(*c.Category))
		if cat == "private" || cat == "bitgrab" {
			return false
		}
	}
	return true
}
func (s source) torrent(hash string) model.Torrent {
	id, _ := protocol.ParseID(hash)
	t := model.Torrent{InfoHash: id, Name: s.Name, Size: uint(s.Size), Private: s.Private, FilesStatus: model.FilesStatus(s.FilesStatus)}
	if s.Extension != nil {
		t.Extension = model.NewNullString(*s.Extension)
	}
	if s.FileCount != nil && *s.FileCount >= 0 {
		t.FilesCount = model.NewNullUint(uint(*s.FileCount))
	}
	for _, f := range s.Files {
		x := model.TorrentFile{InfoHash: id, Path: f.Path, Size: uint(f.Size), Index: uint(f.Index)}
		if f.Extension != nil {
			x.Extension = model.NewNullString(*f.Extension)
		}
		t.Files = append(t.Files, x)
	}
	_ = decodeRow(s.Hint, &t.Hint)
	return t
}
func (s source) input(hash []byte, policy string, noiseV2 bool) v1.Input {
	in := v1.Input{InfoHash: hex.EncodeToString(hash), Name: s.Name, Size: s.Size, FileCount: s.FileCount, Extension: s.Extension, Files: []v1.File{}, Candidates: []v1.Candidate{}, PriorResults: []v1.Result{}}
	for _, f := range s.Files {
		in.Files = append(in.Files, v1.File{Path: f.Path, Size: f.Size})
	}
	sortFiles(in.Files)
	in.FilesComplete = s.FilesStatus == "single" || (s.FilesStatus == "multi" && s.FileCount != nil && int64(len(s.Files)) == *s.FileCount)
	in.Deterministic.MatchingOutcome = "not_attempted"
	in.Deterministic.ProcessingOutcome = "not_attempted"
	if len(s.Contents) == 1 {
		var c contentFact
		_ = json.Unmarshal(s.Contents[0], &c)
		if c.Type != nil {
			in.Deterministic.ContentType = *c.Type
		}
		if c.Source != nil && c.ContentID != nil {
			in.Deterministic.Match = &v1.MatchRef{Source: *c.Source, ID: *c.ContentID}
			in.Deterministic.MatchingOutcome = "matched"
		}
		{
			var receipt struct {
				Processing string `json:"processing_outcome"`
				Digest     string `json:"source_digest"`
				ContentID  string `json:"content_id"`
				Outcome    string `json:"outcome"`
				Policy     string `json:"core_policy"`
			}
			_ = json.Unmarshal(s.Receipt, &receipt)
			if receipt.Policy == policy && receipt.Digest == `\x`+hex.EncodeToString(llmwork.SourceDigest(s.torrent(in.InfoHash))) {
				if receipt.ContentID == c.ID && in.Deterministic.Match == nil {
					in.Deterministic.MatchingOutcome = receipt.Outcome
				}
				in.Deterministic.ProcessingOutcome = receipt.Processing
			}
		}
	}
	cl := classification.Result{}
	if in.Deterministic.ContentType != "" {
		cl.ContentType = model.NewNullContentType(model.ContentType(in.Deterministic.ContentType))
	}
	parsed, e := parsers.ParseVideoContentWithOptions(s.torrent(in.InfoHash), cl, parsers.ParseOptions{NoiseV2: noiseV2})
	if e == nil {
		in.Deterministic.Title = parsed.BaseTitle.String
		in.Deterministic.Year = int(parsed.Date.Year)
	}
	return in
}

// Translate database column names using generated model tags, avoiding another
// hand-maintained model. Bytea identity and volatile dates are supplied by core.
func decodeRow(raw []byte, out any) error {
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	var row map[string]json.RawMessage
	if err := json.Unmarshal(raw, &row); err != nil {
		return err
	}
	values := map[string]json.RawMessage{}
	typ := reflect.TypeOf(out).Elem()
	for i := 0; i < typ.NumField(); i++ {
		f := typ.Field(i)
		col := ""
		for _, tag := range strings.Split(f.Tag.Get("gorm"), ";") {
			if strings.HasPrefix(tag, "column:") {
				col = strings.TrimPrefix(tag, "column:")
			}
		}
		if col == "info_hash" || col == "created_at" || col == "updated_at" || col == "published_at" || col == "tsv" {
			continue
		}
		if value, ok := row[col]; ok {
			values[strings.Split(f.Tag.Get("json"), ",")[0]] = value
		}
	}
	body, _ := json.Marshal(values)
	return json.Unmarshal(body, out)
}

// swarmAndSources reads public-safe tracker scrape telemetry, DHT counts, and
// liveness observations for processor input enrichment without mutating the
// canonical SourceSnapshot or InputRevision. Every lookup is best-effort:
// a missing telemetry table or row leaves the field unset, never an error.
func (s *Store) swarmAndSources(ctx context.Context, tx pgx.Tx, hash []byte, name string, ext *string) ([]v1.SourceObservation, *v1.SwarmInput) {
	var sources []v1.SourceObservation
	rows, err := tx.Query(ctx, `SELECT source,seeders,leechers FROM torrents_torrent_sources WHERE info_hash=$1 AND source IN ('dht','tracker') ORDER BY source LIMIT 8`, hash)
	if err == nil {
		for rows.Next() {
			var obs v1.SourceObservation
			if scanErr := rows.Scan(&obs.Source, &obs.Seeders, &obs.Leechers); scanErr == nil {
				if obs.Seeders != nil || obs.Leechers != nil {
					sources = append(sources, obs)
				}
			}
		}
		rows.Close()
	}

	var sw v1.SwarmInput
	hasSwarm := false
	for _, obs := range sources {
		if obs.Source == "dht" {
			sw.DHTSeeders = obs.Seeders
			sw.DHTLeechers = obs.Leechers
			hasSwarm = true
		}
	}

	var trKnown bool
	var trSeeders, trLeechers, trCompleted, prevSeeders, peakSeeders *int
	var bestTracker *string
	var lastPos, checkedAt *time.Time
	err = tx.QueryRow(ctx, `SELECT tracker_known,seeders,leechers,completed,prev_seeders,peak_seeders,best_tracker,last_positive_at,checked_at FROM torrent_tracker_seeds WHERE info_hash=$1`, hash).
		Scan(&trKnown, &trSeeders, &trLeechers, &trCompleted, &prevSeeders, &peakSeeders, &bestTracker, &lastPos, &checkedAt)
	if err == nil {
		hasSwarm = true
		sw.TrackerScraped = true
		sw.TrackerKnown = trKnown
		sw.TrackerSeeders = trSeeders
		sw.TrackerLeechers = trLeechers
		sw.TrackerCompleted = trCompleted
		sw.PrevSeeders = prevSeeders
		sw.PeakSeeders = peakSeeders
		if bestTracker != nil {
			if safe, ok := publicTrackerURL(*bestTracker); ok {
				sw.BestTracker = safe
			}
		}
		if lastPos != nil {
			sw.LastPositiveAt = lastPos.UTC().Format(time.RFC3339Nano)
		}
		if checkedAt != nil {
			sw.CheckedAt = checkedAt.UTC().Format(time.RFC3339Nano)
		}
	}

	var livStatus string
	var lastQB, aliveSrc *string
	var suspectObs *int
	if err := tx.QueryRow(ctx, `SELECT status,last_qb_state,suspect_observations,alive_source FROM torrent_liveness WHERE info_hash=$1`, hash).
		Scan(&livStatus, &lastQB, &suspectObs, &aliveSrc); err == nil && livStatus != "" {
		hasSwarm = true
		// Only contract values cross the boundary; an unexpected stored
		// status is omitted rather than failing job validation.
		if livStatus == "alive" || livStatus == "suspect" || livStatus == "dead" || livStatus == "unknown" {
			sw.LivenessStatus = livStatus
		}
		if lastQB != nil && len(*lastQB) <= 64 {
			sw.LastQBState = *lastQB
		}
		sw.SuspectObservations = suspectObs
		if aliveSrc != nil {
			if safe, ok := publicAliveSource(*aliveSrc); ok {
				sw.AliveSource = safe
			}
		}
	}

	if found := outcomePriors(ctx, tx, name, ext); len(found) > 0 {
		sw.OutcomePriors = found
		hasSwarm = true
	}

	if !hasSwarm {
		return sources, nil
	}
	return sources, &sw
}

// publicTrackerURL admits only credential-free announce URLs. Query strings,
// userinfo, and passkey-bearing paths are dropped, never disclosed.
func publicTrackerURL(raw string) (string, bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" || len(raw) > 256 || catalogueguard.ProtectedTag(raw, false) {
		return "", false
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return "", false
	}
	if u.Scheme != "udp" && u.Scheme != "http" && u.Scheme != "https" {
		return "", false
	}
	if u.Path != "" && u.Path != "/announce" && u.Path != "/announce/" {
		return "", false
	}
	lower := strings.ToLower(raw)
	for _, bad := range []string{"passkey", "authkey", "apikey", "passthepopcorn", "broadcastthenet", "broadcasthenet", "torrentleech", "bitgrab", "private"} {
		if strings.Contains(lower, bad) {
			return "", false
		}
	}
	return raw, true
}

// publicAliveSource admits only short lowercase audit tokens such as
// qb_state or tracker_scrape.
func publicAliveSource(raw string) (string, bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" || len(raw) > 64 || catalogueguard.ProtectedTag(raw, false) {
		return "", false
	}
	for _, r := range raw {
		if (r < 'a' || r > 'z') && (r < '0' || r > '9') && r != '_' && r != '-' {
			return "", false
		}
	}
	for _, bad := range []string{"passkey", "authkey", "passthepopcorn", "broadcastthenet", "torrentleech", "bitgrab", "private"} {
		if strings.Contains(raw, bad) {
			return "", false
		}
	}
	return raw, true
}

// outcomePriors returns title-derived grab-outcome priors for the release.
// Source-typed priors are deliberately excluded: their key values name
// indexers, which processors must never learn from swarm enrichment.
// A missing grab_outcome_priors table (minimal deployments, older schemas)
// degrades to no priors, matching the ranker fallback.
func outcomePriors(ctx context.Context, tx pgx.Tx, name string, ext *string) []v1.PriorObservation {
	extStr := ""
	if ext != nil {
		extStr = *ext
	}
	keys := priors.Extract(name, nil, extStr)
	if len(keys) == 0 {
		return nil
	}
	types := make([]string, 0, len(keys))
	values := make([]string, 0, len(keys))
	for _, k := range keys {
		types = append(types, k.Type)
		values = append(values, k.Value)
	}
	// A savepoint keeps a missing optional table (42P01) from aborting the
	// surrounding Prepare transaction.
	if _, err := tx.Exec(ctx, `SAVEPOINT swarm_priors`); err != nil {
		return nil
	}
	rows, err := tx.Query(ctx, `SELECT key_type,key_value,alpha,beta FROM grab_outcome_priors WHERE (key_type,key_value) IN (SELECT * FROM unnest($1::text[],$2::text[]))`, types, values)
	if err != nil {
		_, _ = tx.Exec(ctx, `ROLLBACK TO SAVEPOINT swarm_priors`)
		return nil
	}
	found := make(map[priors.FeatureKey]v1.PriorObservation, len(keys))
	for rows.Next() {
		var p v1.PriorObservation
		if scanErr := rows.Scan(&p.KeyType, &p.KeyValue, &p.Alpha, &p.Beta); scanErr != nil {
			rows.Close()
			_, _ = tx.Exec(ctx, `ROLLBACK TO SAVEPOINT swarm_priors`)
			return nil
		}
		if p.Alpha < 1 || p.Beta < 1 {
			continue
		}
		if p.KeyValue == "private" || p.KeyValue == "bitgrab" || strings.Contains(p.KeyValue, "passkey") || strings.Contains(p.KeyValue, "authkey") {
			continue
		}
		found[priors.FeatureKey{Type: p.KeyType, Value: p.KeyValue}] = p
	}
	rowsErr := rows.Err()
	rows.Close()
	if rowsErr != nil {
		_, _ = tx.Exec(ctx, `ROLLBACK TO SAVEPOINT swarm_priors`)
		return nil
	}
	_, _ = tx.Exec(ctx, `RELEASE SAVEPOINT swarm_priors`)
	if len(found) == 0 {
		return nil
	}
	out := make([]v1.PriorObservation, 0, len(found))
	for _, k := range keys {
		if p, ok := found[k]; ok {
			out = append(out, p)
		}
	}
	if len(out) > 32 {
		out = out[:32]
	}
	return out
}

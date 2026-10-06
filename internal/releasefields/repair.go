// Package releasefields owns bounded, frozen filename-claim repairs. It does
// not invoke classification workflows, providers, identity matching or deletion.
package releasefields

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/spencercnorton/bitagent/internal/classifier/classification"
	"github.com/spencercnorton/bitagent/internal/classifier/parsers"
	"github.com/spencercnorton/bitagent/internal/llmcapture"
	"github.com/spencercnorton/bitagent/internal/llmwork"
	"github.com/spencercnorton/bitagent/internal/model"
	"github.com/spencercnorton/bitagent/internal/protocol"
)

const Schema = "release-field-repair-v1"

var ErrChanged = errors.New("frozen release repair source, version or ownership changed")

type Fields struct {
	Tsv               *string                  `json:"tsv"`
	VideoSource       model.NullVideoSource    `json:"videoSource"`
	ReleaseAttributes *model.ReleaseAttributes `json:"releaseAttributes"`
}
type Entry struct {
	SourceDigest     []byte      `json:"sourceDigest"`
	InfoHash         protocol.ID `json:"infoHash"`
	TargetID         string      `json:"targetId"`
	SourceNameSHA256 string      `json:"sourceNameSha256"`
	UpdatedAt        time.Time   `json:"updatedAt"`
	Before           Fields      `json:"before"`
	After            Fields      `json:"after"`
	// Non-null historical source changes require reviewed ownership evidence.
	// This is supplied in a reviewed frozen plan, never inferred by the parser.
	SourceOwnershipEvidence string `json:"sourceOwnershipEvidence,omitempty"`
}
type Plan struct {
	Schema  string  `json:"schema"`
	Parser  string  `json:"parser"`
	Build   string  `json:"build"`
	NoiseV2 bool    `json:"noiseV2"`
	Entries []Entry `json:"entries"`
}
type Outcome struct {
	TargetID string `json:"targetId"`
	State    string `json:"state"`
}

func nameHash(name string) string { s := sha256.Sum256([]byte(name)); return hex.EncodeToString(s[:]) }
func digest(v any) []byte         { body, _ := json.Marshal(v); sum := sha256.Sum256(body); return sum[:] }
func equal(a, b any) bool         { return bytes.Equal(digest(a), digest(b)) }

// Freeze exports current fields and deterministic proposals for explicitly
// selected hashes. Original names and paths are omitted; the planned index contains source
// tokens and remains a private maintenance artefact. Historical
// non-null source changes remain unauthorised until the plan is reviewed.
func Freeze(ctx context.Context, pool *pgxpool.Pool, hashes []protocol.ID, noise bool) (Plan, error) {
	if len(hashes) == 0 || len(hashes) > 1000 {
		return Plan{}, fmt.Errorf("cohort must contain 1..1000 hashes")
	}
	plan := Plan{Schema: Schema, Parser: model.ReleaseAttributesParser, Build: llmcapture.CurrentBuildIdentity(), NoiseV2: noise}
	seen := map[protocol.ID]bool{}
	for _, hash := range hashes {
		if seen[hash] {
			return Plan{}, fmt.Errorf("duplicate cohort hash")
		}
		seen[hash] = true
		tx, err := pool.Begin(ctx)
		if err != nil {
			return plan, err
		}
		t, tc, err := current(ctx, tx, hash)
		if err != nil {
			tx.Rollback(ctx)
			return plan, err
		}
		attrs, err := parsers.ParseVideoContentWithOptions(t, classification.Result{ContentAttributes: classification.ContentAttributes{ContentType: tc.ContentType}}, parsers.ParseOptions{NoiseV2: noise})
		if err != nil {
			tx.Rollback(ctx)
			return plan, err
		}
		before := fields(tc)
		after := before
		if before.ReleaseAttributes == nil {
			after.ReleaseAttributes = attrs.ReleaseAttributes
		}
		if attrs.VideoSource.Valid {
			after.VideoSource = attrs.VideoSource
		}
		if !equal(before.VideoSource, after.VideoSource) {
			projected := tc
			projected.VideoSource = after.VideoSource
			projected.UpdateTsv()
			after.Tsv = tsv(projected)
		}
		plan.Entries = append(plan.Entries, Entry{SourceDigest: llmwork.SourceDigest(t), InfoHash: hash, TargetID: tc.ID, SourceNameSHA256: nameHash(t.Name), UpdatedAt: tc.UpdatedAt, Before: before, After: after})
		if err = tx.Commit(ctx); err != nil {
			return plan, err
		}
	}
	return plan, nil
}

func current(ctx context.Context, tx pgx.Tx, hash protocol.ID) (model.Torrent, model.TorrentContent, error) {
	var t model.Torrent
	var tc model.TorrentContent
	t.InfoHash = hash
	tc.InfoHash = hash
	if _, err := tx.Exec(ctx, `LOCK TABLE torrent_hints,label_evidence,torrent_canonical_labels,torrent_verdict_state,junkpurge_quarantine IN SHARE MODE`); err != nil {
		return t, tc, err
	}
	err := tx.QueryRow(ctx, `SELECT name,size,private,files_status,extension,files_count FROM torrents WHERE info_hash=$1 FOR UPDATE`, hash.Bytes()).Scan(&t.Name, &t.Size, &t.Private, &t.FilesStatus, &t.Extension, &t.FilesCount)
	if err != nil {
		return t, tc, err
	}
	var blocked bool
	err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM torrent_hints WHERE info_hash=$1) OR EXISTS(SELECT 1 FROM torrent_canonical_labels WHERE info_hash=$1) OR EXISTS(SELECT 1 FROM label_evidence WHERE info_hash=$1 AND source='qbittorrent' AND lower(category) IN('private','bitgrab')) OR EXISTS(SELECT 1 FROM torrent_verdict_state WHERE info_hash=$1 AND verdict IN('quarantined','blacklisted','tombstoned')) OR EXISTS(SELECT 1 FROM junkpurge_quarantine WHERE info_hash=$1 AND expired_at IS NULL)`, hash.Bytes()).Scan(&blocked)
	if err != nil {
		return t, tc, err
	}
	if t.Private || blocked {
		return t, tc, ErrChanged
	}
	tags, err := tx.Query(ctx, `SELECT name FROM torrent_tags WHERE info_hash=$1 FOR SHARE`, hash.Bytes())
	if err != nil {
		return t, tc, err
	}
	for tags.Next() {
		var name string
		if err = tags.Scan(&name); err != nil {
			tags.Close()
			return t, tc, err
		}
		name = strings.ToLower(strings.TrimSpace(name))
		for _, prefix := range []string{"wanted", "manual", "reference", "bitgrab"} {
			if name == prefix || strings.HasPrefix(name, prefix+":") {
				blocked = true
			}
		}
	}
	err = tags.Err()
	tags.Close()
	if err != nil {
		return t, tc, err
	}
	if blocked {
		return t, tc, ErrChanged
	}

	fileRows, err := tx.Query(ctx, `SELECT "index",path,size,extension FROM torrent_files WHERE info_hash=$1 ORDER BY "index",path FOR SHARE`, hash.Bytes())
	if err != nil {
		return t, tc, err
	}
	for fileRows.Next() {
		var f model.TorrentFile
		f.InfoHash = hash
		if err = fileRows.Scan(&f.Index, &f.Path, &f.Size, &f.Extension); err != nil {
			fileRows.Close()
			return t, tc, err
		}
		t.Files = append(t.Files, f)
	}
	err = fileRows.Err()
	fileRows.Close()
	if err != nil {
		return t, tc, err
	}
	var raw, languages, episodes []byte
	rows, err := tx.Query(ctx, `SELECT id,content_type,content_source,content_id,languages,episodes,video_resolution,video_source,video_codec,video_3d,video_modifier,release_group,release_attributes,tsv::text,updated_at FROM torrent_contents WHERE info_hash=$1 FOR UPDATE`, hash.Bytes())
	if err != nil {
		return t, tc, err
	}
	n := 0
	for rows.Next() {
		n++
		err = rows.Scan(&tc.ID, &tc.ContentType, &tc.ContentSource, &tc.ContentID, &languages, &episodes, &tc.VideoResolution, &tc.VideoSource, &tc.VideoCodec, &tc.Video3D, &tc.VideoModifier, &tc.ReleaseGroup, &raw, &tc.Tsv, &tc.UpdatedAt)
		if err != nil {
			rows.Close()
			return t, tc, err
		}
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return t, tc, err
	}
	if n != 1 || !tc.ContentType.Valid || (tc.ContentType.ContentType != model.ContentTypeMovie && tc.ContentType.ContentType != model.ContentTypeTvShow) {
		return t, tc, ErrChanged
	}
	if len(raw) > 0 {
		if err = json.Unmarshal(raw, &tc.ReleaseAttributes); err != nil {
			return t, tc, err
		}
	}
	if len(languages) > 0 {
		if err = json.Unmarshal(languages, &tc.Languages); err != nil {
			return t, tc, err
		}
	}
	if len(episodes) > 0 {
		if err = json.Unmarshal(episodes, &tc.Episodes); err != nil {
			return t, tc, err
		}
	}
	tc.Torrent = t
	if tc.ContentID.Valid {
		tc.Content.Type, tc.Content.Source, tc.Content.ID = tc.ContentType.ContentType, tc.ContentSource.String, tc.ContentID.String
		if err = tx.QueryRow(ctx, `SELECT tsv::text FROM content WHERE type=$1 AND source=$2 AND id=$3 FOR SHARE`, tc.Content.Type.String(), tc.Content.Source, tc.Content.ID).Scan(&tc.Content.Tsv); err != nil {
			return t, tc, err
		}
	}

	return t, tc, nil
}

func validate(plan Plan) error {
	body, err := json.Marshal(plan)
	if err != nil || len(body) > 4<<20 {
		return ErrChanged
	}
	if plan.Schema != Schema || plan.Parser != model.ReleaseAttributesParser || plan.Build != llmcapture.CurrentBuildIdentity() || len(plan.Entries) == 0 || len(plan.Entries) > 1000 {
		return ErrChanged
	}
	seen := map[string]bool{}
	for _, e := range plan.Entries {
		if seen[e.TargetID] || e.TargetID == "" || len(e.SourceNameSHA256) != 64 || len(e.SourceDigest) != 32 || e.UpdatedAt.IsZero() {
			return ErrChanged
		}
		seen[e.TargetID] = true
		if e.Before.ReleaseAttributes != nil && !equal(e.Before.ReleaseAttributes, e.After.ReleaseAttributes) {
			return ErrChanged
		}
		if e.After.ReleaseAttributes != nil && (e.After.ReleaseAttributes.Version != 1 || e.After.ReleaseAttributes.Parser != plan.Parser || e.After.ReleaseAttributes.SourceNameSHA256 != e.SourceNameSHA256) {
			return ErrChanged
		}
		if !equal(e.Before.VideoSource, e.After.VideoSource) && e.Before.VideoSource.Valid && strings.TrimSpace(e.SourceOwnershipEvidence) == "" {
			return fmt.Errorf("%w: non-null source repair requires reviewed ownership evidence", ErrChanged)
		}
	}
	return nil
}

// Apply executes one short transaction per frozen entry. The journal and any
// associated application snapshot change commit with the field update. Restart
// resumes from that journal without reapplying a completed entry.
func Apply(ctx context.Context, pool *pgxpool.Pool, plan Plan, write bool) ([]Outcome, error) {
	if err := validate(plan); err != nil {
		return nil, err
	}
	key := digest(plan)
	out := make([]Outcome, 0, len(plan.Entries))
	for _, e := range plan.Entries {
		tx, err := pool.Begin(ctx)
		if err != nil {
			return out, err
		}
		state, err := applyEntry(ctx, tx, key, e, plan.NoiseV2, write)
		if err != nil {
			tx.Rollback(ctx)
			return out, err
		}
		if err = tx.Commit(ctx); err != nil {
			return out, err
		}
		out = append(out, Outcome{e.TargetID, state})
	}
	return out, nil
}
func applyEntry(ctx context.Context, tx pgx.Tx, key []byte, e Entry, noise, write bool) (string, error) {
	t, tc, err := current(ctx, tx, e.InfoHash)
	if err != nil {
		return "", err
	}
	var journalAt time.Time
	err = tx.QueryRow(ctx, `SELECT applied_updated_at FROM release_field_repair_journal WHERE plan_digest=$1 AND target_id=$2 AND rolled_back_at IS NULL FOR UPDATE`, key, e.TargetID).Scan(&journalAt)
	if err == nil {
		if tc.ID != e.TargetID || nameHash(t.Name) != e.SourceNameSHA256 || !bytes.Equal(llmwork.SourceDigest(t), e.SourceDigest) || !tc.UpdatedAt.Equal(journalAt) || !equal(fields(tc), e.After) {
			return "", ErrChanged
		}
		return "already_applied", nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return "", err
	}
	if tc.ID != e.TargetID || !tc.UpdatedAt.Equal(e.UpdatedAt) || nameHash(t.Name) != e.SourceNameSHA256 || !bytes.Equal(llmwork.SourceDigest(t), e.SourceDigest) || !equal(fields(tc), e.Before) {
		return "", ErrChanged
	}
	attrs, err := parsers.ParseVideoContentWithOptions(t, classification.Result{ContentAttributes: classification.ContentAttributes{ContentType: tc.ContentType}}, parsers.ParseOptions{NoiseV2: noise})
	// Only the exact parser proposal may be applied. Configuration-dependent
	// plans must be regenerated under their matching parser option.
	if err != nil {
		return "", err
	}
	if e.Before.ReleaseAttributes == nil && !equal(attrs.ReleaseAttributes, e.After.ReleaseAttributes) {
		return "", ErrChanged
	}
	if !equal(e.Before.VideoSource, e.After.VideoSource) && !equal(attrs.VideoSource, e.After.VideoSource) {
		return "", ErrChanged
	}
	projected := tc
	if !equal(e.Before.VideoSource, e.After.VideoSource) {
		projected.VideoSource = e.After.VideoSource
		projected.UpdateTsv()
	}
	if !equal(tsv(projected), e.After.Tsv) {
		return "", ErrChanged
	}
	if equal(e.Before, e.After) {
		return "unchanged", nil
	}
	if !write {
		return "would_apply", nil
	}
	before, _ := json.Marshal(e.Before)
	after, _ := json.Marshal(e.After)
	appBefore, appAfter, err := repairApplications(ctx, tx, e.InfoHash, e.Before, e.After)
	if err != nil {
		return "", err
	}
	var raw any
	if e.After.ReleaseAttributes != nil {
		body, _ := json.Marshal(e.After.ReleaseAttributes)
		raw = string(body)
	}
	var appliedAt time.Time
	err = tx.QueryRow(ctx, `UPDATE torrent_contents SET video_source=$2,release_attributes=$3::jsonb,tsv=$5::tsvector,updated_at=clock_timestamp() WHERE id=$1 AND updated_at=$4 RETURNING updated_at`, e.TargetID, e.After.VideoSource, raw, e.UpdatedAt, e.After.Tsv).Scan(&appliedAt)
	if err != nil {
		return "", err
	}
	_, err = tx.Exec(ctx, `INSERT INTO release_field_repair_journal(plan_digest,target_id,info_hash,source_name_sha256,before_fields,after_fields,application_before,application_after,applied_updated_at) VALUES($1,$2,$3,$4,$5::jsonb,$6::jsonb,$7::jsonb,$8::jsonb,$9)`, key, e.TargetID, e.InfoHash.Bytes(), e.SourceNameSHA256, string(before), string(after), string(appBefore), string(appAfter), appliedAt)
	return "applied", err
}

func repairApplications(ctx context.Context, tx pgx.Tx, hash protocol.ID, old, new Fields) (json.RawMessage, json.RawMessage, error) {
	before := map[string]json.RawMessage{}
	after := map[string]json.RawMessage{}
	if !equal(old, new) {
		rows, err := tx.Query(ctx, `SELECT task_key,applied_snapshot FROM llm_work_applications WHERE info_hash=$1 FOR UPDATE`, hash.Bytes())
		if err != nil {
			return nil, nil, err
		}
		for rows.Next() {
			var key, raw []byte
			if err = rows.Scan(&key, &raw); err != nil {
				rows.Close()
				return nil, nil, err
			}
			var snapshot map[string]json.RawMessage
			if err = json.Unmarshal(raw, &snapshot); err != nil {
				rows.Close()
				return nil, nil, err
			}
			var source model.NullVideoSource
			if json.Unmarshal(snapshot["videoSource"], &source) != nil || !equal(source, old.VideoSource) {
				rows.Close()
				return nil, nil, ErrChanged
			}
			before[hex.EncodeToString(key)] = raw
			var oldAttributes *model.ReleaseAttributes
			if value := snapshot["releaseAttributes"]; len(value) > 0 {
				if json.Unmarshal(value, &oldAttributes) != nil {
					rows.Close()
					return nil, nil, ErrChanged
				}
			}
			if !equal(oldAttributes, old.ReleaseAttributes) {
				rows.Close()
				return nil, nil, ErrChanged
			}
			value, _ := json.Marshal(new.VideoSource)
			snapshot["videoSource"] = value
			if new.ReleaseAttributes == nil {
				delete(snapshot, "releaseAttributes")
			} else {
				value, _ := json.Marshal(new.ReleaseAttributes)
				snapshot["releaseAttributes"] = value
			}
			body, _ := json.Marshal(snapshot)
			after[hex.EncodeToString(key)] = body
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return nil, nil, err
		}
		for key, body := range after {
			decoded, _ := hex.DecodeString(key)
			if _, err = tx.Exec(ctx, `UPDATE llm_work_applications SET applied_snapshot=$2::jsonb WHERE task_key=$1`, decoded, string(body)); err != nil {
				return nil, nil, err
			}
		}
	}
	left, _ := json.Marshal(before)
	right, _ := json.Marshal(after)
	return left, right, nil
}

// Rollback restores only still-current journalled fields and application facts.
// An intervening edit or refresh requires a new reviewed plan instead.
func Rollback(ctx context.Context, pool *pgxpool.Pool, plan Plan, write bool) ([]Outcome, error) {
	if err := validate(plan); err != nil {
		return nil, err
	}
	key := digest(plan)
	out := []Outcome{}
	for _, e := range plan.Entries {
		tx, err := pool.Begin(ctx)
		if err != nil {
			return out, err
		}
		state, err := rollbackEntry(ctx, tx, key, e, write)
		if err != nil {
			tx.Rollback(ctx)
			return out, err
		}
		if err = tx.Commit(ctx); err != nil {
			return out, err
		}
		out = append(out, Outcome{e.TargetID, state})
	}
	return out, nil
}
func rollbackEntry(ctx context.Context, tx pgx.Tx, key []byte, e Entry, write bool) (string, error) {
	t, tc, err := current(ctx, tx, e.InfoHash)
	if err != nil {
		return "", err
	}
	var at time.Time
	var before, after []byte
	err = tx.QueryRow(ctx, `SELECT applied_updated_at,application_before,application_after FROM release_field_repair_journal WHERE plan_digest=$1 AND target_id=$2 AND rolled_back_at IS NULL FOR UPDATE`, key, e.TargetID).Scan(&at, &before, &after)
	if errors.Is(err, pgx.ErrNoRows) {
		return "not_applied", nil
	}
	if err != nil {
		return "", err
	}
	if tc.ID != e.TargetID || nameHash(t.Name) != e.SourceNameSHA256 || !bytes.Equal(llmwork.SourceDigest(t), e.SourceDigest) || !tc.UpdatedAt.Equal(at) || !equal(fields(tc), e.After) {
		return "", ErrChanged
	}
	if !write {
		return "would_restore", nil
	}
	var oldApps, newApps map[string]json.RawMessage
	if json.Unmarshal(before, &oldApps) != nil || json.Unmarshal(after, &newApps) != nil {
		return "", ErrChanged
	}
	for key, old := range oldApps {
		decoded, _ := hex.DecodeString(key)
		var current []byte
		if err = tx.QueryRow(ctx, `SELECT applied_snapshot FROM llm_work_applications WHERE task_key=$1 FOR UPDATE`, decoded).Scan(&current); err != nil {
			return "", err
		}
		var a, b any
		_ = json.Unmarshal(current, &a)
		_ = json.Unmarshal(newApps[key], &b)
		if !equal(a, b) {
			return "", ErrChanged
		}
		if _, err = tx.Exec(ctx, `UPDATE llm_work_applications SET applied_snapshot=$2::jsonb WHERE task_key=$1`, decoded, string(old)); err != nil {
			return "", err
		}
	}
	var raw any
	if e.Before.ReleaseAttributes != nil {
		body, _ := json.Marshal(e.Before.ReleaseAttributes)
		raw = string(body)
	}
	_, err = tx.Exec(ctx, `UPDATE torrent_contents SET video_source=$2,release_attributes=$3::jsonb,tsv=$4::tsvector,updated_at=clock_timestamp() WHERE id=$1`, e.TargetID, e.Before.VideoSource, raw, e.Before.Tsv)
	if err != nil {
		return "", err
	}
	_, err = tx.Exec(ctx, `UPDATE release_field_repair_journal SET rolled_back_at=clock_timestamp() WHERE plan_digest=$1 AND target_id=$2`, key, e.TargetID)
	return "restored", err
}

func fields(tc model.TorrentContent) Fields {
	return Fields{Tsv: tsv(tc), VideoSource: tc.VideoSource, ReleaseAttributes: tc.ReleaseAttributes}
}

func tsv(tc model.TorrentContent) *string {
	if tc.Tsv == nil {
		return nil
	}
	value := tc.Tsv.String()
	return &value
}

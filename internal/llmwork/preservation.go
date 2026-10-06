package llmwork

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"sort"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/spencercnorton/bitagent/internal/classifier/classification"
	"github.com/spencercnorton/bitagent/internal/model"
)

// ApplicationSnapshot records already committed model application facts. It is
// not a provider response, a canonical identity label or independently reviewed
// gold. Source text and model response bytes are deliberately absent.
type ApplicationSnapshot struct {
	content            *model.Content
	Schema             string                    `json:"schema"`
	Kind               Kind                      `json:"kind"`
	ContentType        model.NullContentType     `json:"contentType"`
	ContentSource      model.NullString          `json:"contentSource"`
	ContentID          model.NullString          `json:"contentId"`
	Languages          model.Languages           `json:"languages"`
	Episodes           model.Episodes            `json:"episodes"`
	VideoResolution    model.NullVideoResolution `json:"videoResolution"`
	VideoSource        model.NullVideoSource     `json:"videoSource"`
	VideoCodec         model.NullVideoCodec      `json:"videoCodec"`
	Video3D            model.NullVideo3D         `json:"video3d"`
	VideoModifier      model.NullVideoModifier   `json:"videoModifier"`
	ReleaseGroup       model.NullString          `json:"releaseGroup"`
	EnglishAudio       model.NullEnglishAudio    `json:"englishAudio"`
	EnglishAudioSource model.NullString          `json:"englishAudioSource"`
	ContentCreatedAt   time.Time                 `json:"contentCreatedAt"`
	Tags               []string                  `json:"tags"`
}

func NewApplicationSnapshot(kind Kind, tc model.TorrentContent, tags []string) ApplicationSnapshot {
	sorted := append([]string(nil), tags...)
	sort.Strings(sorted)
	return ApplicationSnapshot{&tc.Content, "llm-application-v1", kind, tc.ContentType, tc.ContentSource, tc.ContentID,
		tc.Languages, tc.Episodes, tc.VideoResolution, tc.VideoSource, tc.VideoCodec, tc.Video3D, tc.VideoModifier,
		tc.ReleaseGroup, tc.EnglishAudio, tc.EnglishAudioSource, tc.Content.CreatedAt, sorted}
}

func (a ApplicationSnapshot) Attributes() classification.ContentAttributes {
	return classification.ContentAttributes{ContentType: a.ContentType, Languages: a.Languages, Episodes: a.Episodes,
		VideoResolution: a.VideoResolution, VideoSource: a.VideoSource, VideoCodec: a.VideoCodec,
		Video3D: a.Video3D, VideoModifier: a.VideoModifier, ReleaseGroup: a.ReleaseGroup}
}

func (a ApplicationSnapshot) Valid() bool {
	if a.Schema != "llm-application-v1" || (a.Kind != Type && a.Kind != Language && a.Kind != Matcher) || len(a.Tags) > 4 {
		return false
	}
	if a.Kind == Type || a.Kind == Matcher {
		if !a.ContentType.Valid || (a.ContentType.ContentType != model.ContentTypeMovie && a.ContentType.ContentType != model.ContentTypeTvShow) {
			return false
		}
	}
	if a.ContentSource.Valid != a.ContentID.Valid {
		return false
	}
	if a.Kind == Matcher && (!a.ContentID.Valid || a.ContentSource.String == "" || a.ContentID.String == "") {
		return false
	}
	seen := map[string]bool{}
	for _, tag := range a.Tags {
		if seen[tag] || (tag != "type-local-enriched" && tag != "llm-matched" && tag != "llm-language-review") {
			return false
		}
		seen[tag] = true
	}
	return true
}

// Preserve returns only facts still committed under this exact source and
// current policy. It does not recreate removed HTTP evidence or authorize new
// actions. A missing/changed committed target is held, never ordinary fallback.
func (s *Store) Preserve(ctx context.Context, kind Kind, t model.Torrent, policy any) (*ApplicationSnapshot, error) {
	return s.PreservePolicy(ctx, kind, t, func(ApplicationSnapshot) any { return policy })
}

// PreservePolicy derives current policy from committed typed facts, never from
// a scrubbed task payload. A nil result means no application has ever committed;
// a changed or missing fact is an explicit hold.
func (s *Store) PreservePolicy(ctx context.Context, kind Kind, t model.Torrent, policy func(ApplicationSnapshot) any) (*ApplicationSnapshot, error) {
	if s == nil || !s.Enabled() {
		return nil, nil
	}
	pool, err := s.pool.Get()
	if err != nil {
		return nil, err
	}
	tx, err := pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(context.WithoutCancel(ctx))
	var raw, source, policyHash []byte
	err = tx.QueryRow(ctx, `SELECT a.applied_snapshot,a.source_digest,a.policy_digest FROM llm_work_applications a JOIN llm_work_tasks w USING(task_key)
 WHERE a.info_hash=$1 AND w.kind=$2 AND w.state='completed' ORDER BY a.applied_at DESC LIMIT 1 FOR SHARE OF a,w`, t.InfoHash.Bytes(), string(kind)).Scan(&raw, &source, &policyHash)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, tx.Commit(ctx)
	}
	if err != nil {
		return nil, err
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	var a ApplicationSnapshot
	if decoder.Decode(&a) != nil || !a.Valid() || a.Kind != kind || !bytes.Equal(policyHash, Digest(policy(a))) {
		return nil, ErrHeld
	}
	currentSource, err := LockedSource(ctx, tx, Task{Draft: Draft{InfoHash: t.InfoHash.Bytes(), SourceDigest: source}})
	if err != nil {
		if errors.Is(err, ErrObsolete) {
			return nil, ErrHeld
		}
		return nil, err
	}
	if !bytes.Equal(SourceDigest(t), SourceDigest(currentSource)) {
		return nil, ErrHeld
	}
	var tc model.TorrentContent
	rows, err := tx.Query(ctx, `SELECT content_type,content_source,content_id,languages,episodes,video_resolution,video_source,video_codec,video_3d,video_modifier,release_group,english_audio,english_audio_source
 FROM torrent_contents WHERE info_hash=$1 FOR SHARE`, t.InfoHash.Bytes())
	if err != nil {
		return nil, err
	}
	n := 0
	for rows.Next() {
		n++
		err = rows.Scan(&tc.ContentType, &tc.ContentSource, &tc.ContentID, &tc.Languages, &tc.Episodes, &tc.VideoResolution, &tc.VideoSource, &tc.VideoCodec, &tc.Video3D, &tc.VideoModifier, &tc.ReleaseGroup, &tc.EnglishAudio, &tc.EnglishAudioSource)
		if err != nil {
			rows.Close()
			return nil, err
		}
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	if n != 1 {
		return nil, ErrHeld
	}
	if tc.ContentID.Valid {
		tc.Content.Type, tc.Content.Source, tc.Content.ID = tc.ContentType.ContentType, tc.ContentSource.String, tc.ContentID.String
		err = tx.QueryRow(ctx, `SELECT title,release_year,original_language,original_title,adult,tsv,created_at,updated_at FROM content WHERE type=$1 AND source=$2 AND id=$3 FOR SHARE`, tc.ContentType.ContentType.String(), tc.ContentSource.String, tc.ContentID.String).Scan(&tc.Content.Title, &tc.Content.ReleaseYear, &tc.Content.OriginalLanguage, &tc.Content.OriginalTitle, &tc.Content.Adult, &tc.Content.Tsv, &tc.Content.CreatedAt, &tc.Content.UpdatedAt)
		if err != nil {
			return nil, ErrHeld
		}
	}
	current := NewApplicationSnapshot(kind, tc, a.Tags)
	left, _ := json.Marshal(a)
	right, _ := json.Marshal(current)
	var l, r any
	_ = json.Unmarshal(left, &l)
	_ = json.Unmarshal(right, &r)
	if !reflect.DeepEqual(l, r) {
		return nil, ErrHeld
	}
	for _, tag := range a.Tags {
		var present bool
		err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM torrent_tags WHERE info_hash=$1 AND name=$2)`, t.InfoHash.Bytes(), tag).Scan(&present)
		if err != nil {
			return nil, err
		}
		if !present {
			return nil, ErrHeld
		}
	}
	if err = tx.Commit(ctx); err != nil {
		return nil, err
	}
	a.content = &tc.Content
	return &a, nil
}

// Result carries stored catalogue identity without allowing normal persistence
// to replace its authoritative content record.
func (a ApplicationSnapshot) Result() classification.Result {
	r := classification.Result{ContentAttributes: a.Attributes(), Tags: map[string]struct{}{}}
	for _, tag := range a.Tags {
		r.Tags[tag] = struct{}{}
	}
	if a.ContentID.Valid {
		if a.content != nil {
			r.Content = a.content
		} else {
			r.Content = &model.Content{Type: a.ContentType.ContentType, Source: a.ContentSource.String, ID: a.ContentID.String, CreatedAt: a.ContentCreatedAt}
		}
	}
	return r
}

// RecordApplication is called only inside the same transaction as the checked
// target mutation and owned task completion.
func RecordApplication(ctx context.Context, tx pgx.Tx, task Task, snapshot ApplicationSnapshot) error {
	if !snapshot.Valid() {
		return ErrHeld
	}
	body, err := json.Marshal(snapshot)
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `INSERT INTO llm_work_applications(task_key,info_hash,source_digest,policy_digest,applied_snapshot) VALUES($1,$2,$3,$4,$5::jsonb)`, task.Key, task.InfoHash, task.SourceDigest, task.PolicyDigest, string(body))
	return err
}

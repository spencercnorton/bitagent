package processor

import (
	"bytes"
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/spencercnorton/bitagent/internal/llmwork"
	"github.com/spencercnorton/bitagent/internal/model"
	"github.com/spencercnorton/bitagent/internal/protocol"
)

// lockedDeferredSource reconstructs exactly the evidence hashed by SourceDigest
// while locking native privacy/name and existing classification authority.
// It is independent of the optional later release-attribute schema.
func lockedDeferredSource(ctx context.Context, tx pgx.Tx, task llmwork.Task) (model.Torrent, error) {
	if len(task.InfoHash) != len(protocol.ID{}) || len(task.SourceDigest) != 32 {
		return model.Torrent{}, llmwork.ErrObsolete
	}
	var t model.Torrent
	copy(t.InfoHash[:], task.InfoHash)
	err := tx.QueryRow(ctx, `SELECT name,size,private,files_status,extension,files_count FROM torrents WHERE info_hash=$1 FOR UPDATE`, task.InfoHash).
		Scan(&t.Name, &t.Size, &t.Private, &t.FilesStatus, &t.Extension, &t.FilesCount)
	if errors.Is(err, pgx.ErrNoRows) {
		return t, llmwork.ErrObsolete
	}
	if err != nil {
		return t, err
	}
	if t.Private {
		return t, llmwork.ErrObsolete
	}
	var blocked bool
	err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM label_evidence WHERE info_hash=$1 AND source='qbittorrent' AND lower(category) IN('private','bitgrab'))
OR EXISTS(SELECT 1 FROM torrent_verdict_state WHERE info_hash=$1 AND verdict IN('quarantined','blacklisted','tombstoned'))
OR EXISTS(SELECT 1 FROM junkpurge_quarantine WHERE info_hash=$1 AND expired_at IS NULL)`, task.InfoHash).Scan(&blocked)
	if err != nil {
		return t, err
	}
	if blocked {
		return t, llmwork.ErrObsolete
	}
	rows, err := tx.Query(ctx, `SELECT "index",path,size,extension FROM torrent_files WHERE info_hash=$1 ORDER BY "index",path FOR SHARE`, task.InfoHash)
	if err != nil {
		return t, err
	}
	for rows.Next() {
		var f model.TorrentFile
		f.InfoHash = t.InfoHash
		err = rows.Scan(&f.Index, &f.Path, &f.Size, &f.Extension)
		if err != nil {
			rows.Close()
			return t, err
		}
		t.Files = append(t.Files, f)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return t, err
	}
	err = tx.QueryRow(ctx, `SELECT info_hash,content_type,content_source,content_id,title,release_year,languages,episodes,video_resolution,video_source,video_codec,video_3d,video_modifier,release_group,created_at,updated_at
FROM torrent_hints WHERE info_hash=$1 FOR SHARE`, task.InfoHash).Scan(&t.Hint.InfoHash, &t.Hint.ContentType, &t.Hint.ContentSource, &t.Hint.ContentID, &t.Hint.Title, &t.Hint.ReleaseYear, &t.Hint.Languages, &t.Hint.Episodes, &t.Hint.VideoResolution, &t.Hint.VideoSource, &t.Hint.VideoCodec, &t.Hint.Video3D, &t.Hint.VideoModifier, &t.Hint.ReleaseGroup, &t.Hint.CreatedAt, &t.Hint.UpdatedAt)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return t, err
	}
	if !bytes.Equal(llmwork.SourceDigest(t), task.SourceDigest) {
		return t, llmwork.ErrObsolete
	}
	return t, nil
}

// lockedUnknownDeferredTarget rejects all known/manual catalogue authority.
// Absence is a transient ingestion/persistence race, never permission to create
// an arbitrary catalogue row. The task can retry without generic reprocessing.
func lockedUnknownDeferredTarget(ctx context.Context, tx pgx.Tx, t model.Torrent, requireUnknownType bool) (model.TorrentContent, error) {
	if !t.Hint.IsNil() {
		return model.TorrentContent{}, llmwork.ErrObsolete
	}
	rows, err := tx.Query(ctx, `SELECT id,info_hash,content_type,content_source,content_id,languages,episodes,video_resolution,video_source,video_codec,video_3d,video_modifier,release_group,created_at,updated_at,english_audio,english_audio_source
FROM torrent_contents WHERE info_hash=$1 ORDER BY id FOR UPDATE`, t.InfoHash.Bytes())
	if err != nil {
		return model.TorrentContent{}, err
	}
	defer rows.Close()
	var selected model.TorrentContent
	count := 0
	for rows.Next() {
		var tc model.TorrentContent
		err = rows.Scan(&tc.ID, &tc.InfoHash, &tc.ContentType, &tc.ContentSource, &tc.ContentID, &tc.Languages, &tc.Episodes, &tc.VideoResolution, &tc.VideoSource, &tc.VideoCodec, &tc.Video3D, &tc.VideoModifier, &tc.ReleaseGroup, &tc.CreatedAt, &tc.UpdatedAt, &tc.EnglishAudio, &tc.EnglishAudioSource)
		if err != nil {
			return selected, err
		}
		count++
		selected = tc
		if tc.ContentSource.Valid || tc.ContentID.Valid || (requireUnknownType && tc.ContentType.Valid) {
			return selected, llmwork.ErrObsolete
		}
	}
	if err = rows.Err(); err != nil {
		return selected, err
	}
	if count == 0 {
		return selected, llmwork.DeferredError{Reason: "target_not_persisted", RetryAfter: time.Now().UTC().Add(10 * time.Second)}
	}
	if count != 1 {
		return selected, llmwork.ErrObsolete
	}
	selected.Torrent = t
	return selected, nil
}

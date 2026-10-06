package processor

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/spencercnorton/bitagent/internal/llmwork"
	"github.com/spencercnorton/bitagent/internal/model"
)

func lockedDeferredSource(ctx context.Context, tx pgx.Tx, task llmwork.Task) (model.Torrent, error) {
	return llmwork.LockedSource(ctx, tx, task)
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

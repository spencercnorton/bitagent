package llmwork

import (
	"bytes"
	"context"
	"errors"
	"github.com/jackc/pgx/v5"
	"github.com/spencercnorton/bitagent/internal/model"
	"github.com/spencercnorton/bitagent/internal/protocol"
)

// LockedSource reconstructs exactly the evidence hashed by SourceDigest
// while locking native privacy/name and existing classification authority.
// It is independent of the optional later release-attribute schema.
func LockedSource(ctx context.Context, tx pgx.Tx, task Task) (model.Torrent, error) {
	if len(task.InfoHash) != len(protocol.ID{}) || len(task.SourceDigest) != 32 {
		return model.Torrent{}, ErrObsolete
	}
	// Keep source/privacy writers out of the final short application boundary.
	// The ledgers do not all reference the torrent parent, so row locks alone
	// cannot exclude a newly inserted private or quarantine fact.
	if _, err := tx.Exec(ctx, `LOCK TABLE label_evidence,torrent_verdict_state,junkpurge_quarantine IN SHARE MODE`); err != nil {
		return model.Torrent{}, err
	}
	var t model.Torrent
	copy(t.InfoHash[:], task.InfoHash)
	err := tx.QueryRow(ctx, `SELECT name,size,private,files_status,extension,files_count FROM torrents WHERE info_hash=$1 FOR UPDATE`, task.InfoHash).
		Scan(&t.Name, &t.Size, &t.Private, &t.FilesStatus, &t.Extension, &t.FilesCount)
	if errors.Is(err, pgx.ErrNoRows) {
		return t, ErrObsolete
	}
	if err != nil {
		return t, err
	}
	if t.Private {
		return t, ErrObsolete
	}
	var blocked bool
	err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM label_evidence WHERE info_hash=$1 AND source='qbittorrent' AND lower(category) IN('private','bitgrab'))
OR EXISTS(SELECT 1 FROM torrent_verdict_state WHERE info_hash=$1 AND verdict IN('quarantined','blacklisted','tombstoned'))
OR EXISTS(SELECT 1 FROM junkpurge_quarantine WHERE info_hash=$1 AND expired_at IS NULL)`, task.InfoHash).Scan(&blocked)
	if err != nil {
		return t, err
	}
	if blocked {
		return t, ErrObsolete
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
	if !bytes.Equal(SourceDigest(t), task.SourceDigest) {
		return t, ErrObsolete
	}
	return t, nil
}

package llmwork

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"github.com/jackc/pgx/v5"
	"github.com/spencercnorton/bitagent/internal/model"
	"github.com/spencercnorton/bitagent/internal/protocol"
	"strings"
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
	if _, err := tx.Exec(ctx, `LOCK TABLE label_evidence,torrent_canonical_labels,torrent_verdict_state,junkpurge_quarantine IN SHARE MODE`); err != nil {
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
	if err = lockedAuthority(ctx, tx, task); err != nil {
		return t, err
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
	var languages, episodes []byte
	err = tx.QueryRow(ctx, `SELECT info_hash,content_type,content_source,content_id,title,release_year,languages,episodes,video_resolution,video_source,video_codec,video_3d,video_modifier,release_group,created_at,updated_at
FROM torrent_hints WHERE info_hash=$1 FOR SHARE`, task.InfoHash).Scan(&t.Hint.InfoHash, &t.Hint.ContentType, &t.Hint.ContentSource, &t.Hint.ContentID, &t.Hint.Title, &t.Hint.ReleaseYear, &languages, &episodes, &t.Hint.VideoResolution, &t.Hint.VideoSource, &t.Hint.VideoCodec, &t.Hint.Video3D, &t.Hint.VideoModifier, &t.Hint.ReleaseGroup, &t.Hint.CreatedAt, &t.Hint.UpdatedAt)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return t, err
	}
	if len(languages) > 0 {
		if err = json.Unmarshal(languages, &t.Hint.Languages); err != nil {
			return t, err
		}
	}
	if len(episodes) > 0 {
		if err = json.Unmarshal(episodes, &t.Hint.Episodes); err != nil {
			return t, err
		}
	}
	if !bytes.Equal(SourceDigest(t), task.SourceDigest) {
		return t, ErrObsolete
	}
	return t, nil
}

// Wanted tags protect against deletion but carry no catalogue identity. They do
// not prevent ordinary public matching. Manual/reference tags explicitly hold
// optional application; bitgrab remains a privacy boundary.
func authorityTag(name string) bool {
	name = strings.ToLower(strings.TrimSpace(name))
	for _, prefix := range []string{"manual", "reference", "bitgrab"} {
		if name == prefix {
			return true
		}
		for _, separator := range []string{":", "/", "-", "_"} {
			if strings.HasPrefix(name, prefix+separator) {
				return true
			}
		}
	}
	return false
}
func lockedAuthority(ctx context.Context, tx pgx.Tx, task Task) error {
	rows, err := tx.Query(ctx, `SELECT name FROM torrent_tags WHERE info_hash=$1 FOR SHARE`, task.InfoHash)
	if err != nil {
		return err
	}
	blocked := false
	for rows.Next() {
		var name string
		if err = rows.Scan(&name); err != nil {
			rows.Close()
			return err
		}
		blocked = blocked || authorityTag(name)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	if blocked {
		return ErrObsolete
	}
	var media, id model.NullString
	err = tx.QueryRow(ctx, `SELECT media_type,media_id FROM torrent_canonical_labels WHERE info_hash=$1 FOR SHARE`, task.InfoHash).Scan(&media, &id)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	var payload struct{ Type model.NullContentType }
	_ = json.Unmarshal(task.Payload, &payload)
	if !CanonicalAuthorityAllows(task.Kind, media.String, id.String, payload.Type) {
		return ErrObsolete
	}
	return nil

}

// CanonicalAuthorityAllows preserves supported canonical types and explicit
// identities. A compatible type-only label may use the ordinary public matcher.
func CanonicalAuthorityAllows(kind Kind, media, id string, expected model.NullContentType) bool {
	if strings.TrimSpace(id) != "" {
		return false
	}
	var ct model.ContentType
	switch media {
	case "movie":
		ct = model.ContentTypeMovie
	case "tv":
		ct = model.ContentTypeTvShow
	case "music":
		ct = model.ContentTypeMusic
	case "audiobook":
		ct = model.ContentTypeAudiobook
	case "book":
		ct = model.ContentTypeEbook
	}
	return ct == "" || kind == Matcher && expected.Valid && expected.ContentType == ct
}

// HasAuthorityTag distinguishes manual/reference identity authority and
// bitgrab privacy from a wanted-only protection against deletion.
func HasAuthorityTag(name string) bool { return authorityTag(name) }

package processor

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql/driver"
	"encoding/hex"
	"encoding/json"
	"errors"
	"github.com/spencercnorton/bitagent/internal/classifier/classification"
	"github.com/spencercnorton/bitagent/internal/llmwork"
	"sort"

	"github.com/spencercnorton/bitagent/internal/database/dao"
	"github.com/spencercnorton/bitagent/internal/model"
	"github.com/spencercnorton/bitagent/internal/protocol"
	"gorm.io/gorm/clause"
)

type persistPayload struct {
	applications     map[protocol.ID]*classification.ApplicationPreservation
	torrentContents  []model.TorrentContent
	deleteIDs        []string
	deleteInfoHashes []protocol.ID
	addTags          map[protocol.ID]map[string]struct{}
}

func (p persistPayload) isEmpty() bool {
	return len(p.torrentContents) == 0 &&
		len(p.deleteIDs) == 0 &&
		len(p.deleteInfoHashes) == 0 &&
		len(p.addTags) == 0
}

// restoreLLMEnglishAudio protects the LLM tier from the OnConflict UpdateAll
// upsert below it (the tripwire on model.DeriveEnglishAudio): an incoming row
// that carries no english_audio signal this cycle (source NULL — the
// deterministic layer saw nothing and the LLM didn't run or wasn't definite)
// would otherwise erase a previously persisted 'llm' value. Copy those values
// from the existing rows into the incoming structs so the upsert writes them
// back verbatim. A deterministic-explicit incoming row (source 'name') is not
// touched — it outranks 'llm' by design.
//
// Keyed by info_hash, NOT row id, and it must run BEFORE the deleteIDs
// delete: an unmatched->matched transition (or content-type re-parse)
// changes InferID, so the old row is deleted and replaced — and a
// deterministic attach skips the LLM entirely (the workflow gates it on
// !hasAttachedContent), so the replacement row arrives signal-less. English
// availability is a property of the release name, so carrying it across the
// torrent's content rows is correct. Deliberate: the restore also retains a
// value computed while a torrent was public after the torrent later turns
// private — the privacy gates bound EGRESS (nothing new ever leaves), and
// every other classified column persists the same way. Read-then-upsert in
// one transaction; the only 'llm' writer is this code path, and the
// worst-case lost race re-heals on the next (cached) LLM pass.
func (c processor) restoreLLMEnglishAudio(ctx context.Context, tx *dao.Query, tcs []*model.TorrentContent) error {
	byHash := make(map[protocol.ID][]*model.TorrentContent, len(tcs))
	hashes := make([]driver.Valuer, 0, len(tcs))
	for _, tc := range tcs {
		if tc.EnglishAudioSource.Valid {
			continue
		}
		if _, ok := byHash[tc.InfoHash]; !ok {
			hashes = append(hashes, tc.InfoHash)
		}
		byHash[tc.InfoHash] = append(byHash[tc.InfoHash], tc)
	}
	if len(hashes) == 0 {
		return nil
	}
	existing, err := tx.TorrentContent.WithContext(ctx).
		Select(c.dao.TorrentContent.InfoHash, c.dao.TorrentContent.EnglishAudio, c.dao.TorrentContent.EnglishAudioSource).
		Where(
			c.dao.TorrentContent.InfoHash.In(hashes...),
			c.dao.TorrentContent.EnglishAudioSource.Eq(model.EnglishAudioSourceLLM),
		).Find()
	if err != nil {
		return err
	}
	for _, ex := range existing {
		for _, tc := range byHash[ex.InfoHash] {
			tc.EnglishAudio = ex.EnglishAudio
			tc.EnglishAudioSource = ex.EnglishAudioSource
		}
	}
	return nil
}

func (c processor) persist(ctx context.Context, payload persistPayload) error {
	contentsMap := make(map[model.ContentRef]struct{}, len(payload.torrentContents))
	contentsPtr := make([]*model.Content, 0, len(payload.torrentContents))
	torrentContentsPtr := make([]*model.TorrentContent, 0, len(payload.torrentContents))
	torrentTagsPtr := make([]*model.TorrentTag, 0, len(payload.addTags))

	sourceNames := map[protocol.ID]string{}
	for _, tc := range payload.torrentContents {
		sourceNames[tc.InfoHash] = tc.Torrent.Name
		tcCopy := tc
		tcCopy.Torrent = model.Torrent{}

		if tcCopy.ContentID.Valid && tcCopy.Content.CreatedAt.IsZero() {
			contentRef := tcCopy.Content.Ref()
			if _, ok := contentsMap[contentRef]; !ok {
				contentsMap[contentRef] = struct{}{}
				contentCopy := tcCopy.Content
				contentsPtr = append(contentsPtr, &contentCopy)
			}
		}

		tcCopy.Content = model.Content{}
		torrentContentsPtr = append(torrentContentsPtr, &tcCopy)
	}

	for infoHash, tags := range payload.addTags {
		for tag := range tags {
			torrentTagsPtr = append(torrentTagsPtr, &model.TorrentTag{
				InfoHash: infoHash,
				Name:     tag,
			})
		}
	}

	// Persist independent keep/review work before attempting removal. A held
	// destructive action must neither erase its source nor stall ordinary rows
	// in the same processor batch. Only the shared blocking writer may remove
	// a raw torrent, after its complete recovery snapshot has committed.
	ordinary := payload
	ordinary.deleteInfoHashes = nil
	if !ordinary.isEmpty() {
		if err := c.dao.Transaction(func(tx *dao.Query) error {
			if err := guardPreservedApplications(ctx, tx, payload.applications); err != nil {
				return err
			}
			if len(contentsPtr) > 0 {
				if createContentErr := tx.Content.WithContext(ctx).Clauses(
					clause.OnConflict{
						UpdateAll: true,
					}).CreateInBatches(contentsPtr, 100); createContentErr != nil {
					return createContentErr
				}
			}

			// The restore must read the old rows before the deleteIDs delete
			// removes them — an ID-changing replacement (unmatched->matched) is
			// exactly the case where the 'llm' value lives on a row about to die.
			if len(torrentContentsPtr) > 0 {
				if restoreErr := c.restoreReleaseAttributes(ctx, tx, torrentContentsPtr, sourceNames); restoreErr != nil {
					return restoreErr
				}
				if restoreErr := c.restoreLLMEnglishAudio(ctx, tx, torrentContentsPtr); restoreErr != nil {
					return restoreErr
				}
			}

			if len(payload.deleteIDs) > 0 {
				if _, deleteErr := tx.TorrentContent.WithContext(ctx).Where(
					c.dao.TorrentContent.ID.In(payload.deleteIDs...),
				).Delete(); deleteErr != nil {
					return deleteErr
				}
			}

			if len(torrentContentsPtr) > 0 {
				if createErr := tx.TorrentContent.WithContext(ctx).Clauses(
					clause.OnConflict{
						UpdateAll: true,
					},
				).CreateInBatches(torrentContentsPtr, 100); createErr != nil {
					return createErr
				}
			}

			if len(torrentTagsPtr) > 0 {
				if createErr := tx.TorrentTag.WithContext(ctx).Clauses(
					clause.OnConflict{
						DoNothing: true,
					},
				).CreateInBatches(torrentTagsPtr, 100); createErr != nil {
					return createErr
				}
			}

			return nil
		}); err != nil {
			return err
		}
	}
	if len(payload.deleteInfoHashes) > 0 {
		return c.blockingManager.Block(ctx, payload.deleteInfoHashes, false)
	}
	return nil
}

var errReleaseAttributesChanged = errors.New("retained release attribute source or ownership changed")

// Non-null advertised claims are name-owned retained facts. A later cycle that
// skips parsing must not erase them, and automatic processing never overwrites
// a retained value with another interpretation of the same name.
func (c processor) restoreReleaseAttributes(ctx context.Context, tx *dao.Query, tcs []*model.TorrentContent, names map[protocol.ID]string) error {
	byHash := map[protocol.ID][]*model.TorrentContent{}
	for _, tc := range tcs {
		byHash[tc.InfoHash] = append(byHash[tc.InfoHash], tc)
	}
	hashes := make([]protocol.ID, 0, len(byHash))
	for hash := range byHash {
		hashes = append(hashes, hash)
	}
	sort.Slice(hashes, func(i, j int) bool { return hashes[i].String() < hashes[j].String() })
	for _, hash := range hashes {
		raw, err := tx.Torrent.WithContext(ctx).Clauses(clause.Locking{Strength: "UPDATE"}).Where(tx.Torrent.InfoHash.Eq(hash)).First()
		if err != nil {
			return err
		}
		rows, err := tx.TorrentContent.WithContext(ctx).Clauses(clause.Locking{Strength: "UPDATE"}).Where(tx.TorrentContent.InfoHash.Eq(hash)).Find()
		if err != nil {
			return err
		}
		var retained *model.ReleaseAttributes
		sum := sha256.Sum256([]byte(raw.Name))
		nameHash := hex.EncodeToString(sum[:])
		for _, row := range rows {
			if row.ReleaseAttributes == nil {
				continue
			}
			a := row.ReleaseAttributes
			if a.Version != 1 || a.Parser != model.ReleaseAttributesParser || a.SourceNameSHA256 != nameHash {
				return errReleaseAttributesChanged
			}
			if retained != nil {
				left, _ := json.Marshal(retained)
				right, _ := json.Marshal(a)
				if string(left) != string(right) {
					return errReleaseAttributesChanged
				}
			}
			retained = a
		}
		for _, tc := range byHash[hash] {
			if names[hash] != raw.Name {
				return errReleaseAttributesChanged
			}
			if retained != nil {
				tc.ReleaseAttributes = retained
			} else if tc.ReleaseAttributes != nil && tc.ReleaseAttributes.SourceNameSHA256 != nameHash {
				return errReleaseAttributesChanged
			}
		}
	}
	return nil
}

// Recheck normal refreshes in their writing transaction. This closes the gap
// between a read-only preservation lookup and the processor's UpdateAll upsert.
func guardPreservedApplications(ctx context.Context, tx *dao.Query, guards map[protocol.ID]*classification.ApplicationPreservation) error {
	if len(guards) == 0 {
		return nil
	}
	db := tx.Torrent.WithContext(ctx).UnderlyingDB()
	if err := db.Exec(`LOCK TABLE label_evidence,torrent_canonical_labels,torrent_verdict_state,junkpurge_quarantine IN SHARE MODE`).Error; err != nil {
		return err
	}
	hashes := make([]protocol.ID, 0, len(guards))
	for hash := range guards {
		hashes = append(hashes, hash)
	}
	sort.Slice(hashes, func(i, j int) bool { return hashes[i].String() < hashes[j].String() })
	for _, hash := range hashes {
		g := guards[hash]
		var matched bool
		if err := db.Raw(`SELECT EXISTS(SELECT 1 FROM llm_work_applications a JOIN llm_work_tasks w USING(task_key) WHERE a.task_key=? AND a.info_hash=? AND a.source_digest=? AND a.policy_digest=? AND w.state='completed')`, g.TaskKey, hash.Bytes(), g.SourceDigest, g.PolicyDigest).Scan(&matched).Error; err != nil {
			return err
		}
		if !matched {
			return llmwork.ErrHeld
		}
		t, err := tx.Torrent.WithContext(ctx).Clauses(clause.Locking{Strength: "UPDATE"}).Where(tx.Torrent.InfoHash.Eq(hash)).Preload(tx.Torrent.Files, tx.Torrent.Hint).First()
		if err != nil {
			return err
		}
		if err = db.Exec(`SELECT 1 FROM torrent_files WHERE info_hash=? FOR SHARE`, hash.Bytes()).Error; err != nil {
			return err
		}
		if err = db.Exec(`SELECT 1 FROM torrent_hints WHERE info_hash=? FOR SHARE`, hash.Bytes()).Error; err != nil {
			return err
		}
		// Hydrate again after child locks to exclude a concurrent edit of an existing file.
		t, err = tx.Torrent.WithContext(ctx).Where(tx.Torrent.InfoHash.Eq(hash)).Preload(tx.Torrent.Files, tx.Torrent.Hint).First()
		if err != nil {
			return err
		}
		if !bytes.Equal(llmwork.SourceDigest(*t), g.SourceDigest) {
			return llmwork.ErrHeld
		}
		var blocked bool
		if err = db.Raw(`SELECT EXISTS(SELECT 1 FROM label_evidence WHERE info_hash=? AND source='qbittorrent' AND lower(category) IN('private','bitgrab')) OR EXISTS(SELECT 1 FROM torrent_verdict_state WHERE info_hash=? AND verdict IN('quarantined','blacklisted','tombstoned')) OR EXISTS(SELECT 1 FROM junkpurge_quarantine WHERE info_hash=? AND expired_at IS NULL)`, hash.Bytes(), hash.Bytes(), hash.Bytes()).Scan(&blocked).Error; err != nil {
			return err
		}
		if blocked {
			return llmwork.ErrHeld
		}
		rows, err := tx.TorrentContent.WithContext(ctx).Clauses(clause.Locking{Strength: "UPDATE"}).Where(tx.TorrentContent.InfoHash.Eq(hash)).Preload(tx.TorrentContent.Content).Find()
		if err != nil {
			return err
		}
		if len(rows) != 1 {
			return llmwork.ErrHeld
		}
		if err = db.Exec(`SELECT name FROM torrent_tags WHERE info_hash=? FOR SHARE`, hash.Bytes()).Error; err != nil {
			return err
		}
		var tags []string
		if err = db.Raw(`SELECT name FROM torrent_tags WHERE info_hash=?`, hash.Bytes()).Scan(&tags).Error; err != nil {
			return err
		}
		for _, tag := range tags {
			if llmwork.HasAuthorityTag(tag) {
				return llmwork.ErrHeld
			}
		}
		var label struct {
			MediaType *string
			MediaID   *string
		}
		if err = db.Raw(`SELECT media_type,media_id FROM torrent_canonical_labels WHERE info_hash=?`, hash.Bytes()).Scan(&label).Error; err != nil {
			return err
		}
		media, id := "", ""
		if label.MediaType != nil {
			media = *label.MediaType
		}
		if label.MediaID != nil {
			id = *label.MediaID
		}
		if !llmwork.CanonicalAuthorityAllows(llmwork.Kind(g.Kind), media, id, rows[0].ContentType) {
			return llmwork.ErrHeld
		}
		snapshot := llmwork.NewApplicationSnapshot(llmwork.Kind(g.Kind), *rows[0], g.Tags)
		if !bytes.Equal(llmwork.Digest(snapshot), g.SnapshotDigest) {
			return llmwork.ErrHeld
		}
		for _, tag := range g.Tags {
			var exists bool
			if err = db.Raw(`SELECT EXISTS(SELECT 1 FROM torrent_tags WHERE info_hash=? AND name=?)`, hash.Bytes(), tag).Scan(&exists).Error; err != nil {
				return err
			}
			if !exists {
				return llmwork.ErrHeld
			}
		}
	}
	return nil
}

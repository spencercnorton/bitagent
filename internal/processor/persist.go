package processor

import (
	"context"
	"database/sql/driver"

	"github.com/spencercnorton/bitagent/internal/database/dao"
	"github.com/spencercnorton/bitagent/internal/model"
	"github.com/spencercnorton/bitagent/internal/protocol"
	"github.com/spencercnorton/bitagent/internal/slice"
	"gorm.io/gorm/clause"
)

type persistPayload struct {
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

	for _, tc := range payload.torrentContents {
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

	if len(payload.deleteInfoHashes) > 0 {
		if blockErr := c.blockingManager.Block(ctx, payload.deleteInfoHashes, false); blockErr != nil {
			return blockErr
		}
	}

	return c.dao.Transaction(func(tx *dao.Query) error {
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

		if len(payload.deleteInfoHashes) > 0 {
			valuers := slice.Map(payload.deleteInfoHashes, func(infoHash protocol.ID) driver.Valuer {
				return infoHash
			})

			if _, deleteErr := tx.Torrent.WithContext(ctx).Where(
				c.dao.Torrent.InfoHash.In(valuers...),
			).Delete(); deleteErr != nil {
				return deleteErr
			}
		}

		return nil
	})
}

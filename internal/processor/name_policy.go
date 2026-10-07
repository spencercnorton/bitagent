package processor

import (
	"context"
	"database/sql/driver"
	"github.com/spencercnorton/bitagent/internal/catalogueguard"
	"github.com/spencercnorton/bitagent/internal/database/dao"
	"github.com/spencercnorton/bitagent/internal/llmwork"
	"github.com/spencercnorton/bitagent/internal/model"
	"github.com/spencercnorton/bitagent/internal/namepolicy"
	"github.com/spencercnorton/bitagent/internal/protocol"
	"gorm.io/gorm/clause"
	"sort"
	"time"
)

// Resolve minimum authoritative names before loading files, attached metadata
// or running the classifier. Denial is a terminal no-op, never a delete/retry.
func (c processor) admitNames(ctx context.Context, hashes []protocol.ID) ([]protocol.ID, error) {
	values := make([]driver.Valuer, len(hashes))
	for i, h := range hashes {
		values[i] = h
	}
	rows, err := c.dao.Torrent.WithContext(ctx).Select(c.dao.Torrent.InfoHash, c.dao.Torrent.Name).Where(c.dao.Torrent.InfoHash.In(values...)).Find()
	if err != nil {
		return nil, err
	}
	byHash := map[protocol.ID]model.Torrent{}
	for _, t := range rows {
		byHash[t.InfoHash] = *t
	}
	kept := make([]protocol.ID, 0, len(hashes))
	for _, h := range hashes {
		d := c.namePolicy.Evaluate(h, byHash[h].Name)
		if !d.Eligible {
			c.namePolicy.Observe("processor", d)
			continue
		}
		kept = append(kept, h)
	}
	if len(kept) == 0 {
		return kept, nil
	}
	values = values[:0]
	for _, h := range kept {
		values = append(values, h)
	}
	contents, err := c.dao.TorrentContent.WithContext(ctx).Select(c.dao.TorrentContent.InfoHash, c.dao.TorrentContent.ContentType).Where(c.dao.TorrentContent.InfoHash.In(values...)).Find()
	if err != nil {
		return nil, err
	}
	adult := map[protocol.ID]bool{}
	for _, tc := range contents {
		adult[tc.InfoHash] = adult[tc.InfoHash] || tc.ContentType.Valid && tc.ContentType.ContentType == model.ContentTypeXxx
	}
	eligible := kept[:0]
	for _, h := range kept {
		if adult[h] {
			d := c.namePolicy.EvaluateClassified(h, byHash[h].Name, "xxx")
			c.namePolicy.Observe("processor", d)
			continue
		}
		eligible = append(eligible, h)
	}
	return eligible, nil
}

// Provider admission uses the current stored release name; a stale caller name
// cannot authorize an existing raw hash. Database errors stop external work.
func (c processor) providerNameAdmission(ctx context.Context, source namepolicy.Source) (namepolicy.Decision, error) {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	var current struct {
		Name           string
		Private, Adult bool
	}
	db := c.dao.Torrent.WithContext(ctx).UnderlyingDB()
	r := db.Raw(`SELECT name, private OR EXISTS(SELECT 1 FROM label_evidence e WHERE e.info_hash=t.info_hash AND `+catalogueguard.QBPrivacySQL("e.source", "e.category", "?")+`) AS private, EXISTS(SELECT 1 FROM torrent_contents tc WHERE tc.info_hash=t.info_hash AND tc.content_type='xxx') AS adult FROM torrents t WHERE info_hash=?`, catalogueguard.TagWhitespace, catalogueguard.TagWhitespace, source.InfoHash.Bytes()).Scan(&current)
	if r.Error != nil {
		return namepolicy.Decision{Reason: namepolicy.ReasonMissingName, Version: namepolicy.Version}, r.Error
	}
	if r.RowsAffected != 1 || current.Private {
		return namepolicy.Decision{Reason: namepolicy.ReasonNotServed, Version: namepolicy.Version}, nil
	}
	kind := ""
	if current.Adult {
		kind = "xxx"
	}
	d := c.namePolicy.EvaluateClassified(source.InfoHash, current.Name, kind)
	if d.Eligible && current.Name != source.Name {
		return namepolicy.Decision{Reason: namepolicy.ReasonMissingName, Version: namepolicy.Version}, nil
	}
	return d, nil
}

// Direct classification can complete after a stored name changes. Lock and
// re-read that source before any derived rows/tags are written, leaving the raw
// source intact and the response unapplied when its admission no longer holds.
func (c processor) guardNameApplications(ctx context.Context, tx *dao.Query, names map[protocol.ID]string) error {
	if !c.namePolicy.Enabled() {
		return nil
	}
	hashes := make([]protocol.ID, 0, len(names))
	for h := range names {
		hashes = append(hashes, h)
	}
	sort.Slice(hashes, func(i, j int) bool { return hashes[i].String() < hashes[j].String() })
	for _, h := range hashes {
		t, err := tx.Torrent.WithContext(ctx).Select(tx.Torrent.InfoHash, tx.Torrent.Name).Where(tx.Torrent.InfoHash.Eq(h)).Clauses(clause.Locking{Strength: "UPDATE"}).First()
		if err != nil {
			return err
		}
		d := c.namePolicy.Evaluate(h, t.Name)
		if !d.Eligible {
			c.namePolicy.Observe("model_apply", d)
			return llmwork.ErrHeld
		}
		if t.Name != names[h] {
			return llmwork.ErrHeld
		}
		contents, err := tx.TorrentContent.WithContext(ctx).Select(tx.TorrentContent.ContentType).Where(tx.TorrentContent.InfoHash.Eq(h)).Clauses(clause.Locking{Strength: "SHARE"}).Find()
		if err != nil {
			return err
		}
		for _, tc := range contents {
			if tc.ContentType.Valid && tc.ContentType.ContentType == model.ContentTypeXxx {
				d := c.namePolicy.EvaluateClassified(h, t.Name, "xxx")
				c.namePolicy.Observe("model_apply", d)
				return llmwork.ErrHeld
			}
		}

	}
	return nil
}

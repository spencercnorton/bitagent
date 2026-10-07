package processor

import (
	"context"
	"database/sql/driver"
	"github.com/spencercnorton/bitagent/internal/model"
	"github.com/spencercnorton/bitagent/internal/protocol"
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
	return kept, nil
}

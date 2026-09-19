package search

import (
	"time"

	"github.com/spencercnorton/bitagent/internal/database/query"
	"github.com/spencercnorton/bitagent/internal/maps"
	"github.com/spencercnorton/bitagent/internal/model"
)

func torrentCreatedAtCriteria(operator string, at time.Time) query.Criteria {
	return query.RawCriteria{
		Query: model.TableNameTorrent + ".created_at " + operator + " ?",
		Args:  []interface{}{at},
		Joins: maps.NewInsertMap(
			maps.MapEntry[string, struct{}]{Key: model.TableNameTorrent},
		),
	}
}

// TorrentCreatedAfterCriteria includes the lower endpoint. Paired with
// TorrentCreatedBeforeCriteria it defines a half-open [start, end) window over
// the immutable torrent insertion time, not a mutable source or classifier row.
func TorrentCreatedAfterCriteria(at time.Time) query.Criteria {
	return torrentCreatedAtCriteria(">=", at)
}

// TorrentCreatedBeforeCriteria excludes the upper endpoint so adjacent
// windows never double-count a torrent created exactly at their boundary.
func TorrentCreatedBeforeCriteria(at time.Time) query.Criteria {
	return torrentCreatedAtCriteria("<", at)
}

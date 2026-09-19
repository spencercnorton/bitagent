package search

import (
	"fmt"

	"github.com/spencercnorton/bitagent/internal/database/query"
	"github.com/spencercnorton/bitagent/internal/model"
)

// TorrentContentReleaseDateCriteria filters torrent_contents by the parsed
// per-release air date — the daily-show query path (Torznab
// season=YYYY&ep=MM/DD). The date is interpolated as an ISO literal rather
// than bound: DBCriteria.SQL is raw SQL where GORM treats `?` as a bind
// placeholder (same constraint as the episodes criteria), and the value is
// server-constructed from integers already validated by model.Date.IsValid,
// so no untrusted text can reach the literal.
func TorrentContentReleaseDateCriteria(date model.Date) query.Criteria {
	return query.DBCriteria{
		SQL: fmt.Sprintf("torrent_contents.release_date = '%s'", date.IsoDateString()),
	}
}

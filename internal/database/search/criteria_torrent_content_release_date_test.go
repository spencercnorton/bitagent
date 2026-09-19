package search

import (
	"testing"

	"github.com/spencercnorton/bitagent/internal/database/query"
	"github.com/spencercnorton/bitagent/internal/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestTorrentContentReleaseDateCriteriaSQL locks the emitted SQL: an ISO date
// literal equality on torrent_contents.release_date (raw SQL — `?` would be
// misread as a GORM placeholder, same constraint as the episodes criteria).
func TestTorrentContentReleaseDateCriteriaSQL(t *testing.T) {
	t.Parallel()

	c := TorrentContentReleaseDateCriteria(model.NewDateFromParts(2026, 6, 15))
	db, ok := c.(query.DBCriteria)
	require.True(t, ok)
	assert.Equal(t, "torrent_contents.release_date = '2026-06-15'", db.SQL)
}

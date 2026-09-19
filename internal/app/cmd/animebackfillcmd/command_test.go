package animebackfillcmd

import (
	"testing"

	"github.com/spencercnorton/bitagent/internal/model"
	"github.com/stretchr/testify/assert"
)

func row(id, name string, stored bool) *model.TorrentContent {
	return &model.TorrentContent{
		ID:      id,
		IsAnime: stored,
		Torrent: model.Torrent{Name: name},
	}
}

// partitionByAnime must emit only the rows whose stored flag disagrees with the
// detector, split by the direction of the correction — so a backfill write
// touches nothing that is already right.
func TestPartitionByAnime(t *testing.T) {
	rows := []*model.TorrentContent{
		// detected anime, stored false -> flip true (incl. the non-leading
		// fansub-bracket recall case the old prefix filter missed).
		row("a", "[SubsPlease] Frieren - 12 (1080p)", false),
		row("b", "Kimi no Na wa - 137 (1080p) [Erai-raws]", false),
		// detected anime, already stored true -> no-op.
		row("c", "[Judas] Some Show - 05 (1080p)", true),
		// not anime, stored false -> no-op.
		row("d", "The Wire S01E01 1080p BluRay x264-GROUP", false),
		// not anime, stored true (stale) -> clear false.
		row("e", "The Batman 2022 1080p BluRay x264", true),
	}

	toTrue, toFalse := partitionByAnime(rows)

	assert.ElementsMatch(t, []string{"a", "b"}, toTrue, "rows to set true")
	assert.ElementsMatch(t, []string{"e"}, toFalse, "rows to set false")
}

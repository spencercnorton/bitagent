package search

import (
	"testing"
	"time"

	"github.com/spencercnorton/bitagent/internal/model"
)

func TestTorrentCreatedAtCriteriaUsesIndexedHalfOpenWindow(t *testing.T) {
	t.Parallel()

	start := time.Date(2026, 7, 10, 12, 0, 0, 0, time.UTC)
	end := start.Add(7 * 24 * time.Hour)

	after, err := TorrentCreatedAfterCriteria(start).Raw(nil)
	if err != nil {
		t.Fatalf("torrent-created-after criteria: %v", err)
	}
	before, err := TorrentCreatedBeforeCriteria(end).Raw(nil)
	if err != nil {
		t.Fatalf("torrent-created-before criteria: %v", err)
	}

	if got, want := after.Query, "torrents.created_at >= ?"; got != want {
		t.Fatalf("torrent-created-after SQL = %q, want %q", got, want)
	}
	if got, want := before.Query, "torrents.created_at < ?"; got != want {
		t.Fatalf("torrent-created-before SQL = %q, want %q", got, want)
	}
	if len(after.Args) != 1 || after.Args[0] != start {
		t.Fatalf("torrent-created-after args = %#v, want [%v]", after.Args, start)
	}
	if len(before.Args) != 1 || before.Args[0] != end {
		t.Fatalf("torrent-created-before args = %#v, want [%v]", before.Args, end)
	}
	if !after.Joins.Has(model.TableNameTorrent) || !before.Joins.Has(model.TableNameTorrent) {
		t.Fatal("torrent creation-time criteria must require the indexed torrents join")
	}
}

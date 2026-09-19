package search

import (
	"strings"
	"testing"
)

// These tests pin the grouping SQL fragments used by groupByContent. They are
// the correctness-critical strings: the DISTINCT ON key, the representative
// (highest-seeded) ordering, and the deterministic info_hash tiebreak. The
// covering index (migration 00036) mirrors innerOrderSQL column-for-column, so
// drift here silently regresses the query to a full corpus sort.

func TestTorrentContentGroupKeySQL(t *testing.T) {
	t.Parallel()

	const want = "torrent_contents.content_type, torrent_contents.content_source, torrent_contents.content_id"
	if torrentContentGroupKeySQL != want {
		t.Fatalf("group key SQL drifted:\n got: %s\nwant: %s", torrentContentGroupKeySQL, want)
	}
}

func TestTorrentContentGroupInnerOrderSQL(t *testing.T) {
	t.Parallel()

	got := torrentContentGroupInnerOrderSQL

	// Postgres requires the inner ORDER BY to lead with the DISTINCT ON key.
	if !strings.HasPrefix(got, torrentContentGroupKeySQL+",") {
		t.Errorf("inner order must lead with the group key; got: %s", got)
	}

	// Highest-seeded representative; NULL treated as -1 so null-seeder torrents
	// sort last (matches order_torrent_content.go's seeders comparator).
	if !strings.Contains(got, "COALESCE(torrent_contents.seeders, -1) DESC") {
		t.Errorf("inner order must pick the highest-seeded representative; got: %s", got)
	}

	// Final tiebreak on info_hash — unique within a content group, so the
	// representative is a deterministic function of the current snapshot and
	// offset pagination cannot duplicate/skip a title on a seeder tie.
	if !strings.HasSuffix(got, "torrent_contents.info_hash") {
		t.Errorf("inner order must end in the info_hash tiebreak; got: %s", got)
	}
}

func TestTorrentContentGroupByContentOptionIsSet(t *testing.T) {
	t.Parallel()

	if TorrentContentGroupByContentOption() == nil {
		t.Fatal("expected a non-nil grouping option")
	}
}

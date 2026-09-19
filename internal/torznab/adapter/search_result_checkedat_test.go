package adapter

import (
	"testing"
	"time"

	"github.com/spencercnorton/bitagent/internal/model"
	"github.com/spencercnorton/bitagent/internal/torznab"
)

func checkedAtAttr(item torznab.SearchResultItem) (string, bool) {
	for _, attr := range item.TorznabAttrs {
		if attr.AttrName == torznab.AttrSeedsCheckedAt {
			return attr.AttrValue, true
		}
	}
	return "", false
}

// TestSeedsCheckedAtAttr locks the freshness exposure: present with the
// tracker row's RFC3339 updated_at when an authoritative verdict exists,
// absent otherwise (honest-unknown — DHT counts stay unlabelled).
func TestSeedsCheckedAtAttr(t *testing.T) {
	verified := time.Date(2026, 7, 19, 8, 0, 0, 0, time.UTC)

	trackerSrc := sourceWithSeeders(model.SourceKeyTracker, 9, true, verified)
	trackerSrc.UpdatedAt = verified
	withTracker := makeItem(1, sourceWithSeeders("dht", 4, true, verified), trackerSrc)
	if v, ok := checkedAtAttr(torrentContentResultItemToTorznabResultItem(withTracker, true)); !ok || v != "2026-07-19T08:00:00Z" {
		t.Errorf("tracker verdict must carry seedscheckedat; got %q ok=%v", v, ok)
	}

	dhtOnly := makeItem(2, sourceWithSeeders("dht", 4, true, verified))
	if _, ok := checkedAtAttr(torrentContentResultItemToTorznabResultItem(dhtOnly, true)); ok {
		t.Error("no tracker verdict -> no freshness attr")
	}
}

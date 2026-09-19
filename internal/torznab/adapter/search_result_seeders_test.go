package adapter

import (
	"testing"

	"github.com/spencercnorton/bitagent/internal/model"
	"github.com/spencercnorton/bitagent/internal/torznab"
)

func seedersAttr(item torznab.SearchResultItem) (string, bool) {
	for _, attr := range item.TorznabAttrs {
		if attr.AttrName == torznab.AttrSeeders {
			return attr.AttrValue, true
		}
	}
	return "", false
}

// Under authoritative-zero-only, a bloom-approximated 0 is honest-unknown:
// the seeders attr is omitted so *arr minimumSeeders treats the release as
// unknown instead of rejecting a young, alive swarm. A tracker verdict of 0
// and any positive count are still reported.
func TestSeedersAttr_AuthoritativeZeroOnly(t *testing.T) {
	bloomZero := makeItem(1, sourceWithSeeders("dht", 0, true, fixedNow))
	if _, ok := seedersAttr(torrentContentResultItemToTorznabResultItem(bloomZero, true)); ok {
		t.Error("bloom-only zero must omit the seeders attr")
	}
	if v, ok := seedersAttr(torrentContentResultItemToTorznabResultItem(bloomZero, false)); !ok || v != "0" {
		t.Errorf("legacy mode must report bloom zero as 0; got %q ok=%v", v, ok)
	}

	trackerZero := makeItem(2, sourceWithSeeders(model.SourceKeyTracker, 0, true, fixedNow))
	if v, ok := seedersAttr(torrentContentResultItemToTorznabResultItem(trackerZero, true)); !ok || v != "0" {
		t.Errorf("authoritative zero must be reported as 0; got %q ok=%v", v, ok)
	}

	bloomPositive := makeItem(3, sourceWithSeeders("dht", 5, true, fixedNow))
	if v, ok := seedersAttr(torrentContentResultItemToTorznabResultItem(bloomPositive, true)); !ok || v != "5" {
		t.Errorf("bloom positive must be reported; got %q ok=%v", v, ok)
	}

	unknown := makeItem(4, sourceWithSeeders("dht", 0, false, fixedNow))
	if _, ok := seedersAttr(torrentContentResultItemToTorznabResultItem(unknown, true)); ok {
		t.Error("invalid seeders must omit the attr")
	}
}

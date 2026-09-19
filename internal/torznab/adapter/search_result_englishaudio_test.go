package adapter

import (
	"testing"
	"time"

	"github.com/spencercnorton/bitagent/internal/model"
	"github.com/spencercnorton/bitagent/internal/torznab"
)

func englishAudioAttr(item torznab.SearchResultItem) (string, bool) {
	for _, attr := range item.TorznabAttrs {
		if attr.AttrName == torznab.AttrEnglishAudio {
			return attr.AttrValue, true
		}
	}
	return "", false
}

// TestEnglishAudioAttr locks the exposure: emitted verbatim from the
// persisted column when set, absent when NULL (honest-unknown, same
// convention as the seeders attr).
func TestEnglishAudioAttr(t *testing.T) {
	now := time.Date(2026, 7, 19, 8, 0, 0, 0, time.UTC)

	for _, val := range []model.EnglishAudio{
		model.EnglishAudioDub, model.EnglishAudioSub, model.EnglishAudioNone,
	} {
		item := makeItem(1, sourceWithSeeders("dht", 4, true, now))
		item.EnglishAudio = model.NewNullEnglishAudio(val)
		got, ok := englishAudioAttr(torrentContentResultItemToTorznabResultItem(item, true))
		if !ok || got != string(val) {
			t.Errorf("english_audio=%s must emit the attr verbatim; got %q ok=%v", val, got, ok)
		}
	}

	unknown := makeItem(2, sourceWithSeeders("dht", 4, true, now))
	if _, ok := englishAudioAttr(torrentContentResultItemToTorznabResultItem(unknown, true)); ok {
		t.Error("NULL english_audio -> no attr")
	}
}

package anime

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// TestEnglishSplitSignals locks the split audio/sub detection incl. the bare
// forms the exam found missing, and proves EnglishTrack (the IsAnime input)
// is untouched by the new patterns.
func TestEnglishSplitSignals(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name        string
		audio, sub  bool
		legacyTrack bool
	}{
		{"[Judas] Show - 12 [Dual-Audio]", true, false, true},
		{"[Group] Show - 05 (Dub) [1080p]", true, false, false},
		{"[Group] Show S01 Dubbed 1080p", true, false, false},
		{"[Group] Show - 07 English Audio", true, false, false},
		{"[Erai-raws] Show - 03 [Multiple Subtitle]", false, true, true},
		{"[SubsPlease] Show - 09 [EngSub]", false, true, true}, // legacy eng..sub matches EngSub too
		{"[Group] Show - 11 Subbed", false, true, false},
		{"[Group] Show - 02 SoftSubs", false, true, false},
		{"Dual (2022) 1080p WEB", false, false, false},
		{"[Ohys-Raws] Show - 04 (BS11 1280x720)", false, false, false},
		{"Plain.Show.S01E05.1080p", false, false, false},
		{"[Anime-World] One Piece - 500 [Hindi Dubbed]", false, false, false},
		{"[Grupo] Show - 12 Spanish Subbed", false, false, false},
		{"[Tamil-Fansub] Naruto - 220 Tamil Dubbed 1080p", false, false, false},
		{"[Group] Show - 08 English Dubbed", true, false, true}, // legacy eng..dub substring-matches too
	}
	for _, tc := range cases {
		s := Detect(tc.name)
		assert.Equal(t, tc.audio, s.EnglishAudioSignal, "audio: %s", tc.name)
		assert.Equal(t, tc.sub, s.EnglishSubSignal, "sub: %s", tc.name)
		assert.Equal(t, tc.legacyTrack, s.EnglishTrack, "legacy EnglishTrack must be byte-stable: %s", tc.name)
	}
}

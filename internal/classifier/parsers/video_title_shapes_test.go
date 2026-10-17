package parsers

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// The remaining source-title shapes from the 2026-09 benchmark residual:
// a broadcaster prefix, a "[Group]" bracket glued onto the title, and a
// Spanish title with the English one in parentheses. All are NoiseV2-only;
// the legacy parse must stay byte-identical.
func TestTitleShapesV2(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name       string
		wantTitle  string
		wantLegacy string
	}{
		// Broadcaster prefix: the network is not the first title word.
		{"PBS.NOVA.2015.Inside.Einsteins.Mind.720p.HDTV.x264.AAC.MVGroup.org.mp4", "NOVA", "PBS NOVA"},
		{"PBS.American.Experience.2006.Jesse.James.720p.x264.AAC.MVGroup.org.mp4", "American Experience", "PBS American Experience"},
		{"BBC.Storyville.2020.Fishing.for.Love.1080p.HDTV.x265.AAC.MVGroup.org.mkv", "Storyville", "BBC Storyville"},
		{"BBC Call the Midwife S07E08 11th Mar 2018 HD 1920x1080 (Deep61) [WWRG]", "Call the Midwife", "BBC Call the Midwife"},
		// Known fansub group, conventional SxxExx name: the episodes branch
		// types it before the fansub title override can run, so the generic
		// title must not keep the bracket glued on.
		{"[LostYears] Berserk - The Golden Age Arc - Memorial Edition - S01E08 (WEB 1080p x264 10-bit AAC) [DB297790].mkv",
			"Berserk The Golden Age Arc Memorial Edition", "[LostYears ]Berserk The Golden Age Arc Memorial Edition"},
		// Unknown group bracket before a year-anchored title.
		{"[DigitalPlayground] Some Movie 2020 1080p BluRay", "Some Movie", "Some Movie"},
		// Spanish edition with the English title in parentheses: the
		// bracketed [Esp] tag is what licenses preferring the alternate.
		{"Criaturas luminosas (Remarkably Bright Creatures) (2026) [4k 2160p][Esp]",
			"Remarkably Bright Creatures", "Criaturas luminosas (Remarkably Bright Creatures)"},
		{"Hermanito (Little Brother) (2026) [4k 2160p][Esp]", "Little Brother", "Hermanito (Little Brother)"},
		// No Spanish tag: the pairing direction cannot be told from the
		// titles, so the lead title is kept.
		{"El Hoyo (The Platform) 2019 1080p BluRay x264-GRP", "El Hoyo (The Platform)", "El Hoyo (The Platform)"},
		{"Layer Cake (Crimen organizado) (2004) [DVDRip] [Castellano]", "Layer Cake (Crimen organizado)", "Layer Cake (Crimen organizado)"},
		// A bracket that is a title, and one holding the name's only year.
		{"[REC].2007.1080p.BluRay.x264-GRP", "REC", "REC"},
		{"[01.31.2014] Some Studio - Some Scene 480p [.mp4]", "01 31", "01 31"},
	}

	for _, c := range cases {
		attrs, err := parseV2(t, c.name, true)
		require.NoError(t, err, c.name)
		require.Equal(t, c.wantTitle, attrs.BaseTitle.String, "v2: %s", c.name)

		legacy, err := parseV2(t, c.name, false)
		require.NoError(t, err, c.name)
		require.Equal(t, c.wantLegacy, legacy.BaseTitle.String, "legacy: %s", c.name)
	}
}

// A known fansub group's absolute-numbered release still takes the fansub
// branch and its cleaned title; the generic bracket strip must not disturb it.
func TestTitleShapesKnownFansubUnaffected(t *testing.T) {
	t.Parallel()

	attrs, err := parseV2(t, "[SubsPlease] One Piece - 1077 (1080p) [3FC90F00].mkv", true)
	require.NoError(t, err)
	require.Equal(t, "One Piece", attrs.BaseTitle.String)
	require.Equal(t, "SubsPlease", attrs.ReleaseGroup.String)
}

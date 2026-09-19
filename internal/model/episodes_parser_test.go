package model

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// TestParseEpisodes verifies the full episode-notation parser, including
// the x-separator variants (SxxXxx, NNxNN, S-prefix) that were previously
// dropped or mis-indexed.
func TestParseEpisodes(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name  string
		input string
		want  Episodes
	}{
		// ── x-separator forms (the main bug fixes) ──
		{
			"S-prefix uppercase X",
			"S02x20",
			Episodes{2: {20: {}}},
		},
		{
			"S-prefix uppercase X variant",
			"S17X02",
			Episodes{17: {2: {}}},
		},
		{
			"leading-zero NNxNN",
			"01x06",
			Episodes{1: {6: {}}},
		},
		{
			"bare NNxNN no leading zero",
			"2x09",
			Episodes{2: {9: {}}},
		},
		{
			"x-format range",
			"3x01-6",
			Episodes{3: {1: {}, 2: {}, 3: {}, 4: {}, 5: {}, 6: {}}},
		},

		// ── standard SxxExx forms (must not regress) ──
		{
			"SxxExx standard",
			"S01E05",
			Episodes{1: {5: {}}},
		},
		{
			"SxxExx uppercase",
			"S02E20",
			Episodes{2: {20: {}}},
		},
		{
			"season only",
			"S01",
			Episodes{1: {}},
		},

		// ── 3-4 digit episodes (previously truncated to 2 digits) ──
		{
			// Pokémon S20E048 — the pre-fix cap stored this as episode 4.
			"three-digit episode with leading zero",
			"S20E048",
			Episodes{20: {48: {}}},
		},
		{
			// The Bold and the Beautiful S39E206 — was stored as episode 20.
			"three-digit daily-soap episode",
			"S39E206",
			Episodes{39: {206: {}}},
		},
		{
			"three-digit episode",
			"S01E100",
			Episodes{1: {100: {}}},
		},
		{
			"four-digit absolute-numbered episode",
			"S01E1000",
			Episodes{1: {1000: {}}},
		},
		{
			"three-digit episode range",
			"S01E100-E102",
			Episodes{1: {100: {}, 101: {}, 102: {}}},
		},

		// ── concatenated multi-episode (previously kept only the first) ──
		{
			"concatenated double episode",
			"S01E01E02",
			Episodes{1: {1: {}, 2: {}}},
		},
		{
			"concatenated triple episode",
			"S03E05E06E07",
			Episodes{3: {5: {}, 6: {}, 7: {}}},
		},
		{
			"concatenated three-digit episodes",
			"S02E100E101",
			Episodes{2: {100: {}, 101: {}}},
		},

		// ── seasons stay at 2 digits (guard against year mis-parse) ──
		{
			// year-shape seasons parse whole since the daily-show MR
			"four-digit year season parses as one season",
			"S2020",
			Episodes{2020: {}},
		},

		// ── range-span guard: an adversarial/implausible range must not
		//    expand into thousands of keys (clampRangeEnd degrades to start) ──
		{
			"oversized episode range clamps to start",
			"S01E01-E9999",
			Episodes{1: {1: {}}},
		},
		{
			"episode range within span still expands",
			"S01E01-E05",
			Episodes{1: {1: {}, 2: {}, 3: {}, 4: {}, 5: {}}},
		},

		// ── resolution patterns must NOT be parsed as episodes ──
		{
			"1920x1080 not an episode",
			"1920x1080",
			nil,
		},
		{
			"1280x720 not an episode",
			"1280x720",
			nil,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := ParseEpisodes(tc.input)
			assert.Equal(t, tc.want, got, "ParseEpisodes(%q)", tc.input)
		})
	}
}

// TestYearSeason covers the year-as-season daily-show form, fixed in the
// daily-show release_date MR via the year-shape-first season number token
// ((18|19|20)\d{2}|\d{1,2}). The resolution-token guards must stay: a bare
// non-year 3-4 digit run after s is NOT a season, so "s1080p"/"s2160p" keep
// their legacy non-episode behavior.
func TestYearSeason(t *testing.T) {
	t.Parallel()
	assert.Equal(t, Episodes{2022: {16: {}}}, ParseEpisodes("S2022E16"))
	assert.Equal(t, Episodes{2023: {145: {}}}, ParseEpisodes("S2023E145"))
	assert.Equal(t, Episodes{2023: {}}, ParseEpisodes("S2023"))
	assert.Equal(t, Episodes{1998: {}, 1999: {}, 2000: {}}, ParseEpisodes("S1998-S2000"))
	assert.Nil(t, ParseEpisodes("s1080p"))
	assert.Nil(t, ParseEpisodes("s2160p"))
	// mixed-magnitude range degrades to start via clampRangeEnd
	assert.Equal(t, Episodes{1: {}}, ParseEpisodes("S1-2004"))
}

// TestResolutionRangeEndGuard: "E106 - 1080p" style names must not read the
// resolution as an episode-range end (would store ~975 bogus episodes), while
// batches with an explicit E-marker on the end — which proves an episode, not
// a resolution — always survive, whatever the span.
func TestResolutionRangeEndGuard(t *testing.T) {
	t.Parallel()
	assert.Equal(t, Episodes{1: {106: {}}}, ParseEpisodes("S01E106 - 1080"))
	got := ParseEpisodes("S01E900-E1080")
	assert.Len(t, got[1], 181, "legit batch within span 300 is kept")
	got = ParseEpisodes("S01E779-E1080")
	assert.Len(t, got[1], 302, "explicit E-marker on the end disables the resolution guard")
	got = ParseEpisodes("1x100-480")
	assert.Len(t, got[1], 1, "x-format has no E-marker; bare resolution end degrades to start")
}

// TestCommaListPrefixedItems: list items keep their s/S / e/E prefix; the old
// Split+ParseInt path silently turned "S2021" into season 0.
func TestCommaListPrefixedItems(t *testing.T) {
	t.Parallel()
	assert.Equal(t, Episodes{2020: {}, 2021: {}}, ParseEpisodes("S2020,S2021"))
	assert.Equal(t, Episodes{1: {}, 2: {}, 3: {}}, ParseEpisodes("S1,S2,S3"))
	assert.Equal(t, Episodes{2023: {1: {}, 3: {}}}, ParseEpisodes("S2023E01,E03"))
}

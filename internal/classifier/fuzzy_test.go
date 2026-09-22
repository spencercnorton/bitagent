package classifier

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// --- normalizeTitleForMatch unit tests ---

func TestNormalizeTitleForMatch_Articles(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"The Matrix", "matrix"},
		{"A Beautiful Mind", "beautiful mind"},
		// "An Officer and a Gentleman": only the LEADING article is stripped;
		// mid-sentence "a" is NOT an article in this context.
		{"An Officer and a Gentleman", "officer and a gentleman"},
		// Non-article first word: untouched.
		{"Inception", "inception"},
		// Leading article but only one token: do NOT strip (would leave empty string).
		{"The", "the"},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			assert.Equal(t, tc.want, normalizeTitleForMatch(tc.in))
		})
	}
}

func TestNormalizeTitleForMatch_RomanNumerals(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"Rocky IV", "rocky 4"},
		{"Rocky 4", "rocky 4"},
		{"Star Wars Episode V", "star wars episode 5"},
		{"Star Wars Episode 5", "star wars episode 5"},
		// Part normalisation.
		{"Part II", "part 2"},
		{"Part 2", "part 2"},
		// Boundary: Se7en must NOT be mangled ("se7en" is not a pure roman numeral).
		{"Se7en", "se7en"},
		// Roman I through XX.
		{"Rocky I", "rocky 1"},
		{"Rocky X", "rocky 10"},
		{"Rocky XX", "rocky 20"},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			assert.Equal(t, tc.want, normalizeTitleForMatch(tc.in))
		})
	}
}

func TestNormalizeTitleForMatch_Ampersand(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"Fast & Furious", "fast and furious"},
		{"Fast and Furious", "fast and furious"},
		{"Rock & Roll", "rock and roll"},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			assert.Equal(t, tc.want, normalizeTitleForMatch(tc.in))
		})
	}
}

// --- tokenSetRatio unit tests ---

func TestTokenSetRatio(t *testing.T) {
	cases := []struct {
		a, b string
		min  float64
		max  float64
	}{
		// Identical sets -> 1.0.
		{"rocky 4", "rocky 4", 1.0, 1.0},
		// One token diff in short title -> below 0.90.
		{"rocky", "rocky 4", 0.0, 0.89},
		// Fast and furious vs fast and furious -> 1.0.
		{"fast and furious", "fast and furious", 1.0, 1.0},
		// Empty strings.
		{"", "", 1.0, 1.0},
		{"foo", "", 0.0, 0.0},
	}
	for _, tc := range cases {
		t.Run(tc.a+"_vs_"+tc.b, func(t *testing.T) {
			r := tokenSetRatio(tc.a, tc.b)
			assert.GreaterOrEqual(t, r, tc.min, "ratio below minimum")
			assert.LessOrEqual(t, r, tc.max, "ratio above maximum")
		})
	}
}

// --- fuzzyFindBestMatch table tests ---

// matchItem holds a title set and release year for each candidate, mirroring
// what real search/tmdb callers provide.
type matchItem struct {
	titles []string
	year   int
}

func runFuzzyMatch(t *testing.T, query string, queryYear int, items []matchItem, fuzzy bool) (matchItem, bool) {
	t.Helper()
	got, ok := fuzzyFindBestMatch(
		query,
		items,
		func(m matchItem) []string { return m.titles },
		func(m matchItem) int { return yearProximityPenalty(queryYear, m.year) },
		fuzzy,
	)
	return got, ok
}

// POSITIVES -- must match when fuzzy is ON.
func TestFuzzyFindBestMatch_Positives(t *testing.T) {
	t.Parallel()

	t.Run("RockyIV_vs_Rocky4", func(t *testing.T) {
		items := []matchItem{
			{titles: []string{"Rocky 4"}, year: 1985},
		}
		got, ok := runFuzzyMatch(t, "Rocky IV", 1985, items, true)
		require.True(t, ok, "expected a match for Rocky IV -> Rocky 4")
		assert.Equal(t, []string{"Rocky 4"}, got.titles)
	})

	t.Run("StarWarsEpisodeV_vs_Episode5", func(t *testing.T) {
		items := []matchItem{
			{titles: []string{"Star Wars Episode 5"}, year: 1980},
		}
		got, ok := runFuzzyMatch(t, "Star Wars Episode V", 1980, items, true)
		require.True(t, ok, "expected a match for Star Wars Episode V -> Star Wars Episode 5")
		assert.Equal(t, []string{"Star Wars Episode 5"}, got.titles)
	})

	t.Run("TheMatrix_vs_Matrix", func(t *testing.T) {
		// Article strip: "The Matrix" -> "matrix"; candidate "Matrix" -> "matrix".
		// Both normalize identically -- exact Levenshtein match.
		items := []matchItem{
			{titles: []string{"Matrix"}, year: 1999},
		}
		got, ok := runFuzzyMatch(t, "The Matrix", 1999, items, true)
		require.True(t, ok, "expected The Matrix to match Matrix (leading article strip)")
		assert.Equal(t, []string{"Matrix"}, got.titles)
	})

	t.Run("FastAndFurious_ampersand", func(t *testing.T) {
		// "&" expanded to "and" before normalisation.
		items := []matchItem{
			{titles: []string{"Fast and Furious"}, year: 2001},
		}
		got, ok := runFuzzyMatch(t, "Fast & Furious", 2001, items, true)
		require.True(t, ok, "expected Fast & Furious -> Fast and Furious")
		assert.Equal(t, []string{"Fast and Furious"}, got.titles)
	})

	t.Run("YearOffByOne", func(t *testing.T) {
		// Same title, year off by one -- penalty=1, still accepted (year is not
		// a hard gate when fuzzy mode is on).
		items := []matchItem{
			{titles: []string{"Inception"}, year: 2011},
		}
		got, ok := runFuzzyMatch(t, "Inception", 2010, items, true)
		require.True(t, ok, "expected match when year is off by one")
		assert.Equal(t, []string{"Inception"}, got.titles)
	})

	t.Run("ExactYearBeatsOffByOne", func(t *testing.T) {
		// Two candidates with identical title; one exact-year match, one year+1.
		// The exact-year candidate (penalty=0) must win.
		items := []matchItem{
			{titles: []string{"Inception"}, year: 2011},
			{titles: []string{"Inception"}, year: 2010},
		}
		got, ok := runFuzzyMatch(t, "Inception", 2010, items, true)
		require.True(t, ok)
		assert.Equal(t, 2010, got.year, "exact year (2010) should beat year+1 (2011)")
	})
}

// NEGATIVES -- must NOT match when fuzzy is ON (precision gate).
func TestFuzzyFindBestMatch_Negatives(t *testing.T) {
	t.Parallel()

	t.Run("TheBatman_vs_BatmanBegins", func(t *testing.T) {
		// After article strip: "The Batman" -> "batman" (len=6, threshold=1).
		// "Batman Begins" -> "batman begins". lev("batman","batman begins")=7>1.
		// tsr("batman","batman begins")=1/2=0.50<0.90. Must NOT match.
		items := []matchItem{
			{titles: []string{"Batman Begins"}, year: 2005},
		}
		_, ok := runFuzzyMatch(t, "The Batman", 2022, items, true)
		assert.False(t, ok, "The Batman must NOT match Batman Begins")
	})

	t.Run("Rocky_vs_RockyIV", func(t *testing.T) {
		// "rocky" (len=5, threshold=1) vs "rocky 4": lev=2>1, tsr=0.50<0.90.
		items := []matchItem{
			{titles: []string{"Rocky 4"}, year: 1985},
		}
		_, ok := runFuzzyMatch(t, "Rocky", 1985, items, true)
		assert.False(t, ok, "Rocky must NOT match Rocky 4 -- distinct entries")
	})

	t.Run("Se7en_roman_safety", func(t *testing.T) {
		// "Se7en" -> "se7en" after NormalizeString (digit breaks pure-roman check).
		// "Seven" -> "seven". First tokens: "se7en" != "seven" -> precision gate blocks.
		items := []matchItem{
			{titles: []string{"Seven"}, year: 1995},
		}
		_, ok := runFuzzyMatch(t, "Se7en", 1995, items, true)
		assert.False(t, ok, "Se7en must not match Seven -- first token gate blocks (se7en != seven)")
	})

	t.Run("DifferentTitle_different_firstToken", func(t *testing.T) {
		items := []matchItem{
			{titles: []string{"Interstellar"}, year: 2014},
		}
		_, ok := runFuzzyMatch(t, "Inception", 2010, items, true)
		assert.False(t, ok, "Inception must not match Interstellar -- first tokens differ")
	})
}

// YearOffByThree -- documents that year is a tiebreaker NOT a hard gate.
func TestFuzzyFindBestMatch_YearIsNotHardGate(t *testing.T) {
	// Same title, year off by 3. The spec requires year must NOT be a hard gate
	// when fuzzy is on. The match should still be accepted (with a large penalty,
	// but it wins because there are no better candidates).
	items := []matchItem{
		{titles: []string{"Inception"}, year: 2013},
	}
	got, ok := runFuzzyMatch(t, "Inception", 2010, items, true)
	require.True(t, ok, "Inception should match even with year off by 3 -- year is tiebreaker, not gate")
	assert.Equal(t, []string{"Inception"}, got.titles)
}

// FLAG OFF -- cases that only the new logic would match must stay unmatched
// when FuzzyMatchEnabled is false.
func TestFuzzyFindBestMatch_FlagOff(t *testing.T) {
	t.Parallel()

	t.Run("ArticleDiff_stays_unmatched_when_flag_off", func(t *testing.T) {
		// "The Fast and the Furious Tokyo Drift" vs "Fast and Furious Tokyo Drift":
		// Without fuzzy: levenshteinNormalizeString produces no article stripping.
		// old norms: "the fast and the furious tokyo drift" vs "fast and furious tokyo drift"
		// lev = 8 > levenshteinThreshold(5) -> no match.
		// With fuzzy: article strip + lev=4 <= threshold(5) -> match.
		items := []matchItem{
			{titles: []string{"Fast and Furious Tokyo Drift"}, year: 2006},
		}
		_, ok := runFuzzyMatch(t, "The Fast and the Furious Tokyo Drift", 2006, items, false)
		assert.False(t, ok, "flag=off: article+word diff must NOT match via old Levenshtein path (lev=8>5)")
	})

	t.Run("FlagOn_same_case_matches", func(t *testing.T) {
		// Confirm the flag-on path accepts the same pair to validate the test symmetry.
		items := []matchItem{
			{titles: []string{"Fast and Furious Tokyo Drift"}, year: 2006},
		}
		got, ok := runFuzzyMatch(t, "The Fast and the Furious Tokyo Drift", 2006, items, true)
		require.True(t, ok, "flag=on: article strip + scaled lev should accept this pair")
		assert.Equal(t, []string{"Fast and Furious Tokyo Drift"}, got.titles)
	})

	t.Run("RockyIV_vs_Rocky4_unchanged", func(t *testing.T) {
		// Rocky IV -> Rocky 4: old lev("rocky iv","rocky 4")=2 <= 5, so this
		// matches even WITHOUT the fuzzy flag. Confirms backward-compatible behavior.
		items := []matchItem{
			{titles: []string{"Rocky 4"}, year: 1985},
		}
		_, ok := runFuzzyMatch(t, "Rocky IV", 1985, items, false)
		assert.True(t, ok, "flag=off: Rocky IV -> Rocky 4 still matches via old Levenshtein (lev=2<=5)")
	})
}

// yearProximityPenalty unit tests.
func TestYearProximityPenalty(t *testing.T) {
	cases := []struct {
		query, candidate, want int
	}{
		{2010, 2010, 0},
		{2010, 2011, 1},
		{2010, 2009, 1},
		{2010, 2012, 100},
		{2010, 2007, 100},
		{0, 2010, 0},  // zero queryYear -> no penalty (unknown year)
		{2010, 0, 0},  // zero candidateYear -> no penalty (unknown year)
	}
	for _, tc := range cases {
		t.Run("", func(t *testing.T) {
			got := yearProximityPenalty(tc.query, tc.candidate)
			assert.Equal(t, tc.want, got)
		})
	}
}

// yearFromDateString unit tests.
func TestYearFromDateString(t *testing.T) {
	cases := []struct {
		in   string
		want int
	}{
		{"2021-03-05", 2021},
		{"1985-11-22", 1985},
		{"", 0},
		{"not-a-date", 0},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			assert.Equal(t, tc.want, yearFromDateString(tc.in))
		})
	}
}

// TestFuzzyFindBestMatch_PunctuatedCanonicalTitles pins the 2026-09-22
// benchmark regression: with fuzzy on, a catalogue title carrying punctuation
// was discarded by the first-token gate ("planes," != "planes") before the
// Levenshtein distance that would have absorbed it. Every query below is the
// base title the production parser extracts from a real-world release name;
// every candidate is the catalogue title it belongs to. All of them matched
// with fuzzy OFF and were lost with it ON.
func TestFuzzyFindBestMatch_PunctuatedCanonicalTitles(t *testing.T) {
	t.Parallel()

	cases := []struct{ query, tmdb string }{
		{"Diners Drive Ins And Dives", "Diners, Drive-Ins and Dives"}, // 221 gold rows
		{"Spider Noir", "Spider-Noir"},
		{"Dexter Resurrection", "Dexter: Resurrection"},
		{"Sabrina The Teenage Witch", "Sabrina, the Teenage Witch"},
		{"X Men 97", "X-Men '97"}, // was "10 men 97" vs "x-men '97"
		{"The Ultimatum Marry or Move On", "The Ultimatum: Marry or Move On"},
		{"Jesus His Life", "Jesus: His Life"},
		{"Jackass Best and Last", "Jackass: Best and Last"},
		{"9 1 1", "9-1-1"},
		{"Berserk The Golden Age Arc Memorial Edition", "Berserk: The Golden Age Arc – Memorial Edition"},
		{"Mr Robot", "Mr. Robot"},
		{"Jimmy Kimmel Live", "Jimmy Kimmel Live!"},
		{"Lee Cronins The Mummy", "Lee Cronin's The Mummy"},
		{"Greys Anatomy", "Grey's Anatomy"},
	}
	for _, tc := range cases {
		t.Run(tc.tmdb, func(t *testing.T) {
			items := []matchItem{{titles: []string{tc.tmdb}}}
			_, ok := runFuzzyMatch(t, tc.query, 0, items, true)
			assert.True(t, ok, "fuzzy ON must match %q -> %q", tc.query, tc.tmdb)
			_, ok = runFuzzyMatch(t, tc.query, 0, items, false)
			assert.True(t, ok, "fuzzy OFF matched %q -> %q before the fix; keep it that way", tc.query, tc.tmdb)
		})
	}
}

// Folding punctuation must not loosen the precision gate: a different first
// word is still a different title, with or without punctuation around it.
func TestFuzzyFindBestMatch_PunctuationDoesNotLoosenGate(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct{ query, other string }{
		{"The Batman", "Batman: Begins"},
		{"Dexter New Blood", "Dexter's Laboratory"},
		{"Spider Man", "Spider-Noir"},
		{"Love Island", "Love Is Blind"},
	} {
		t.Run(tc.query, func(t *testing.T) {
			_, ok := runFuzzyMatch(t, tc.query, 0, []matchItem{{titles: []string{tc.other}}}, true)
			assert.False(t, ok, "%q must not match %q", tc.query, tc.other)
		})
	}
}

func TestFoldTitlePunct(t *testing.T) {
	t.Parallel()

	for in, want := range map[string]string{
		"Diners, Drive-Ins and Dives": "Diners  Drive Ins and Dives",
		"Grey's Anatomy":              "Greys Anatomy",
		"X-Men '97":                   "X Men 97",
		"Fast & Furious":              "Fast & Furious", // '&' left for NormalizeTitleForMatch
		"Berserk – Memorial":          "Berserk   Memorial",
		"進撃の巨人":                       "進撃の巨人", // letters untouched
	} {
		assert.Equal(t, want, foldTitlePunct(in), in)
	}
}

package classifier

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// --- normalizeTitleForMatch: Japanese-aware behaviour ---

//nolint:gosmopolitan // Japanese titles are exactly what this path normalises.
func TestNormalizeTitleForMatch_Japanese(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		in   string
		want string
	}{
		// Kana → Hepburn (via kanaToRomaji before unidecode), not the
		// unidecode-native "shingekinokiyozin"/"kauboibibatsupu" spellings.
		{"all_hiragana", "しんげきのきょじん", "shingekinokyojin"},
		{"katakana_cowboy", "カウボーイビバップ", "kaubooibibappu"},
		{"katakana_onepiece", "ワンピース", "wanpiisu"},
		// Mixed kanji+kana: の romanises to "no", but the surrounding kanji
		// still go through unidecode (Mandarin pinyin, "noju" merges with the
		// next kanji), so romanisation is NOT the fix path for kanji-bearing
		// titles — that is handled by the native-key match, tested below.
		{"mixed_kanji_kana", "進撃の巨人", "jin ji noju ren"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tc.want, normalizeTitleForMatch(tc.in))
		})
	}
}

// --- normalizeTitleForMatch: non-Japanese titles are byte-for-byte unchanged ---
//
// Every input here has no kana, so kanaToRomaji is never invoked and the
// output must equal the pre-change behaviour. This is the core regression
// guard for finding C3's caution: the normalisation path serves ALL titles.
//
//nolint:gosmopolitan // includes CJK inputs to prove Han-only titles are unaffected.
func TestNormalizeTitleForMatch_NonJapaneseUnchanged(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		in   string
		want string
	}{
		{"accented_amelie", "Amélie", "amelie"},
		{"accented_leon", "Léon", "leon"},
		{"accented_pokemon", "Pokémon", "pokemon"},
		{"german_umlaut", "Über", "uber"},
		{"cyrillic_войнаимир", "Война и мир", "voina 1 mir"}, // pre-existing unidecode+roman behaviour
		{"chinese_han", "你好世界", "ni hao shi jie"},            // Han-only: unidecode pinyin, no kana pass
		{"korean", "오징어 게임", "ojingeo geim"},
		{"plain_english", "Attack on Titan", "attack on titan"},
		{"article_strip_still_works", "The Matrix", "matrix"},
		{"roman_still_works", "Rocky IV", "rocky 4"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tc.want, normalizeTitleForMatch(tc.in))
		})
	}
}

// --- fuzzyFindBestMatch: Japanese positives ---

//nolint:gosmopolitan // native-script anime titles are the subject under test.
func TestFuzzyFindBestMatch_JapanesePositives(t *testing.T) {
	t.Parallel()

	t.Run("kana_query_matches_romaji_candidate", func(t *testing.T) {
		t.Parallel()
		// The C3 kana case: an all-kana title must reach its romaji alt-title.
		// kanaToRomaji → "shingekinokyojin"; candidate "Shingeki no Kyojin" →
		// "shingeki no kyojin"; lev=2 ≤ threshold(3) with the first-token gate
		// dropped for the Japanese target.
		items := []matchItem{
			{titles: []string{"Attack on Titan", "Shingeki no Kyojin"}, year: 2013},
		}
		got, ok := runFuzzyMatch(t, "しんげきのきょじん", 2013, items, true)
		require.True(t, ok, "kana title must match its romaji alt-title")
		assert.Equal(t, 2013, got.year)
	})

	t.Run("kanji_query_matches_native_original_name", func(t *testing.T) {
		t.Parallel()
		// The C3 kanji case: unidecode turns 進撃の巨人 into Mandarin pinyin, but
		// the native-key match compares kanji↔kanji against original_name.
		items := []matchItem{
			{titles: []string{"Attack on Titan", "進撃の巨人"}, year: 2013},
		}
		got, ok := runFuzzyMatch(t, "進撃の巨人", 2013, items, true)
		require.True(t, ok, "kanji title must match native original_name")
		assert.Equal(t, 2013, got.year)
	})

	t.Run("kanji_query_matches_despite_spacing", func(t *testing.T) {
		t.Parallel()
		// Native key strips spaces/punctuation, so a spaced alt-title spelling
		// still matches.
		items := []matchItem{
			{titles: []string{"Demon Slayer", "鬼滅 の 刃"}, year: 2019},
		}
		got, ok := runFuzzyMatch(t, "鬼滅の刃", 2019, items, true)
		require.True(t, ok, "kanji title must match spaced native alt-title")
		assert.Equal(t, 2019, got.year)
	})

	t.Run("native_match_prefers_exact_year", func(t *testing.T) {
		t.Parallel()
		// Two native-key hits; the exact-year one (penalty 0) must win over the
		// year+2 one (penalty 100).
		items := []matchItem{
			{titles: []string{"進撃の巨人"}, year: 2015},
			{titles: []string{"進撃の巨人"}, year: 2013},
		}
		got, ok := runFuzzyMatch(t, "進撃の巨人", 2013, items, true)
		require.True(t, ok)
		assert.Equal(t, 2013, got.year, "exact-year native match should win")
	})
}

// --- fuzzyFindBestMatch: Japanese negatives (precision preserved) ---

//nolint:gosmopolitan // native-script anime titles are the subject under test.
func TestFuzzyFindBestMatch_JapaneseNegatives(t *testing.T) {
	t.Parallel()

	t.Run("different_kanji_do_not_match", func(t *testing.T) {
		t.Parallel()
		// Distinct native titles must not native-match, and their pinyin forms
		// are Levenshtein-far.
		items := []matchItem{
			{titles: []string{"Demon Slayer", "鬼滅の刃"}, year: 2019},
		}
		_, ok := runFuzzyMatch(t, "進撃の巨人", 2013, items, true)
		assert.False(t, ok, "Attack on Titan (kanji) must not match Demon Slayer (kanji)")
	})

	t.Run("kana_query_unrelated_romaji_does_not_match", func(t *testing.T) {
		t.Parallel()
		// Even with the gate dropped, the Levenshtein threshold still rejects an
		// unrelated romaji candidate.
		items := []matchItem{
			{titles: []string{"Cowboy Bebop", "Kaubooi Bibappu"}, year: 1998},
		}
		_, ok := runFuzzyMatch(t, "しんげきのきょじん", 2013, items, true)
		assert.False(t, ok, "kana title must not match an unrelated romaji candidate")
	})
}

// --- fuzzyFindBestMatch: non-Japanese precision gate still applies ---
//
// Re-asserts the existing negative-precision behaviour to prove the gate-drop
// is scoped strictly to Japanese-origin targets.
func TestFuzzyFindBestMatch_NonJapaneseGateStillApplies(t *testing.T) {
	t.Parallel()

	t.Run("TheBatman_still_blocked", func(t *testing.T) {
		t.Parallel()
		items := []matchItem{{titles: []string{"Batman Begins"}, year: 2005}}
		_, ok := runFuzzyMatch(t, "The Batman", 2022, items, true)
		assert.False(t, ok, "non-JP first-token gate must still block The Batman ↔ Batman Begins")
	})

	t.Run("accented_latin_positive_still_matches", func(t *testing.T) {
		t.Parallel()
		// A non-JP accented title still matches via the normal path (unidecode
		// handles the accent; gate applies and passes).
		items := []matchItem{{titles: []string{"Amelie"}, year: 2001}}
		got, ok := runFuzzyMatch(t, "Amélie", 2001, items, true)
		require.True(t, ok, "Amélie must still match Amelie")
		assert.Equal(t, []string{"Amelie"}, got.titles)
	})
}

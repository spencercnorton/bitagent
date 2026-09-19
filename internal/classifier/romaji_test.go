package classifier

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

//nolint:gosmopolitan // Japanese kana/kanji are exactly what this transliterator targets.
func TestKanaToRomaji(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		in   string
		want string
	}{
		// Basic gojūon, hiragana and katakana share one table.
		{"hiragana_basic", "しんげきのきょじん", "shingekinokyojin"},
		{"katakana_basic", "デスノート", "desunooto"},
		// Yōon digraphs: きょ→kyo (not "kiyo"), じ→ji (not "zi").
		{"yoon_kyo", "きょう", "kyou"},
		{"yoon_sha_ja_cha", "しゃじゃちゃ", "shajacha"},
		// Sokuon (っ) geminates the next consonant.
		{"sokuon_double", "きって", "kitte"},
		{"sokuon_pocket", "ポケット", "poketto"},
		// Sokuon before ch → tch (Hepburn).
		{"sokuon_tch", "まっちゃ", "matcha"},
		// Chōonpu (ー) lengthens the preceding vowel.
		{"choonpu_ramen", "ラーメン", "raamen"},
		{"choonpu_onepiece", "ワンピース", "wanpiisu"},
		// Foreign-sound combos.
		{"foreign_va", "ヴァイオレット", "vaioretto"},
		{"foreign_fa_she_che", "ファシェチェ", "fasheche"},
		// Syllabic n.
		{"syllabic_n", "けいおん", "keion"},
		// Mixed script: only kana is transliterated; kanji/Latin pass through
		// untouched for the downstream unidecode pass.
		{"mixed_kanji_kept", "進撃の巨人", "進撃no巨人"},
		{"mixed_latin_kept", "TVアニメ", "TVanime"},
		// No kana: returned unchanged (defensive — caller gates on kana).
		{"no_kana", "Attack on Titan", "Attack on Titan"},
		{"empty", "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tc.want, kanaToRomaji(tc.in))
		})
	}
}

//nolint:gosmopolitan // native-script keys are the whole point of this helper.
func TestNativeMatchKey(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		in   string
		want string
	}{
		// Spacing and punctuation are stripped so alt-title spellings collapse.
		{"kanji", "進撃の巨人", "進撃の巨人"},
		{"kanji_spaced", "進撃 の 巨人", "進撃の巨人"},
		{"kanji_punct", "進撃・の・巨人！", "進撃の巨人"},
		// Full-width digits/letters NFKC-fold to half-width, lower-cased.
		{"fullwidth_fold", "進撃の巨人 Ｓｅａｓｏｎ ３", "進撃の巨人season3"},
		{"latin_digits_kept", "進撃の巨人 Season 3", "進撃の巨人season3"},
		// Kana native key.
		{"kana", "デスノート", "デスノート"},
		// No Japanese script → empty key (also the "not native" predicate).
		{"latin_only", "Attack on Titan", ""},
		{"cyrillic_only", "Война и мир", ""},
		{"empty", "", ""},
		// Chinese Han also yields a key (native-to-native matching is correct).
		{"chinese_han", "你好 世界", "你好世界"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tc.want, nativeMatchKey(tc.in))
		})
	}
}

package titlenorm

import (
	"strings"
	"unicode"

	"github.com/spencercnorton/bitagent/internal/classifier/contentfilter"
	"golang.org/x/text/unicode/norm"
)

// This file adds Japanese-aware normalisation for the fuzzy title matcher.
//
// The problem: normalizeTitleForMatch romanises via go-unidecode, which maps
// Han (kanji) to MANDARIN PINYIN — 進撃の巨人 → "Jin Ji noJu Ren", not the
// Japanese "Shingeki no Kyojin" — and romanises kana with a non-Hepburn scheme
// that also drops word spacing (しんげきのきょじん → "shingekinokiyozin"). A
// native-script anime title therefore lands nowhere near its correct romaji
// alt-title, and the first-token precision gate blocks it outright.
//
// Two complementary fixes live here:
//
//   - KanaToRomaji: a modified-Hepburn kana→romaji pass applied BEFORE
//     unidecode (kana only; kanji still fall through to unidecode). Correct
//     digraphs (きょ→kyo, じ→ji), sokuon gemination (っぱ→ppa), and long
//     vowels (ー) bring kana titles within Levenshtein range of their romaji.
//
//   - NativeMatchKey: an NFKC-folded, punctuation-stripped key over the
//     Japanese-script characters of a title, for direct kanji-to-kanji /
//     kana-to-kana comparison against TMDB original_name / ja / alt_title:*
//     candidates — robust to spacing and Unicode form, where the unidecode
//     path is not.

// kanaDigraph maps two-kana sequences (yōon and foreign-sound combos) to
// Hepburn. Keyed in KATAKANA; hiragana input is folded to katakana first so a
// single table serves both syllabaries. Checked before kanaMono so the
// two-rune form wins.
var kanaDigraph = map[string]string{
	// yōon (palatalised): base i-row kana + small ゃ/ゅ/ょ
	"キャ": "kya", "キュ": "kyu", "キョ": "kyo",
	"ギャ": "gya", "ギュ": "gyu", "ギョ": "gyo",
	"シャ": "sha", "シュ": "shu", "ショ": "sho",
	"ジャ": "ja", "ジュ": "ju", "ジョ": "jo",
	"チャ": "cha", "チュ": "chu", "チョ": "cho",
	"ヂャ": "ja", "ヂュ": "ju", "ヂョ": "jo",
	"ニャ": "nya", "ニュ": "nyu", "ニョ": "nyo",
	"ヒャ": "hya", "ヒュ": "hyu", "ヒョ": "hyo",
	"ビャ": "bya", "ビュ": "byu", "ビョ": "byo",
	"ピャ": "pya", "ピュ": "pyu", "ピョ": "pyo",
	"ミャ": "mya", "ミュ": "myu", "ミョ": "myo",
	"リャ": "rya", "リュ": "ryu", "リョ": "ryo",
	// foreign-sound combos common in transliterated titles
	"シェ": "she", "ジェ": "je", "チェ": "che",
	"ティ": "ti", "トゥ": "tu", "ディ": "di", "ドゥ": "du",
	"ツァ": "tsa", "ツィ": "tsi", "ツェ": "tse", "ツォ": "tso",
	"ファ": "fa", "フィ": "fi", "フェ": "fe", "フォ": "fo", "フュ": "fyu",
	"ウィ": "wi", "ウェ": "we", "ウォ": "wo",
	"ヴァ": "va", "ヴィ": "vi", "ヴェ": "ve", "ヴォ": "vo", "ヴュ": "vyu",
}

// kanaMono maps single katakana to Hepburn.
var kanaMono = map[rune]string{
	'ア': "a", 'イ': "i", 'ウ': "u", 'エ': "e", 'オ': "o",
	'カ': "ka", 'キ': "ki", 'ク': "ku", 'ケ': "ke", 'コ': "ko",
	'ガ': "ga", 'ギ': "gi", 'グ': "gu", 'ゲ': "ge", 'ゴ': "go",
	'サ': "sa", 'シ': "shi", 'ス': "su", 'セ': "se", 'ソ': "so",
	'ザ': "za", 'ジ': "ji", 'ズ': "zu", 'ゼ': "ze", 'ゾ': "zo",
	'タ': "ta", 'チ': "chi", 'ツ': "tsu", 'テ': "te", 'ト': "to",
	'ダ': "da", 'ヂ': "ji", 'ヅ': "zu", 'デ': "de", 'ド': "do",
	'ナ': "na", 'ニ': "ni", 'ヌ': "nu", 'ネ': "ne", 'ノ': "no",
	'ハ': "ha", 'ヒ': "hi", 'フ': "fu", 'ヘ': "he", 'ホ': "ho",
	'バ': "ba", 'ビ': "bi", 'ブ': "bu", 'ベ': "be", 'ボ': "bo",
	'パ': "pa", 'ピ': "pi", 'プ': "pu", 'ペ': "pe", 'ポ': "po",
	'マ': "ma", 'ミ': "mi", 'ム': "mu", 'メ': "me", 'モ': "mo",
	'ヤ': "ya", 'ユ': "yu", 'ヨ': "yo",
	'ラ': "ra", 'リ': "ri", 'ル': "ru", 'レ': "re", 'ロ': "ro",
	'ワ': "wa", 'ヰ': "wi", 'ヱ': "we", 'ヲ': "o", 'ン': "n",
	'ヴ': "vu",
	// small vowels / y-kana as a fallback when not consumed by a digraph
	'ァ': "a", 'ィ': "i", 'ゥ': "u", 'ェ': "e", 'ォ': "o",
	'ャ': "ya", 'ュ': "yu", 'ョ': "yo",
	'ヵ': "ka", 'ヶ': "ke",
}

const (
	sokuon  = 'ッ' // small tsu — geminates the following consonant
	choonpu = 'ー' // long-vowel mark — repeats the preceding vowel
)

// KanaToRomaji transliterates the kana runs of s to modified-Hepburn romaji,
// leaving every non-kana rune (kanji, Latin, digits, punctuation) untouched
// for the downstream unidecode + NormalizeString passes. Hiragana is folded to
// katakana first so one table drives both.
func KanaToRomaji(s string) string {
	runes := []rune(s)
	// Fold the hiragana block (U+3041..U+3096) to its katakana counterpart
	// (offset +0x60); this also maps small kana, the sokuon, and ゔ.
	for i, r := range runes {
		if r >= 0x3041 && r <= 0x3096 {
			runes[i] = r + 0x60
		}
	}

	var b strings.Builder
	geminate := false
	var lastVowel byte

	emit := func(rom string) {
		if rom == "" {
			return
		}
		if geminate {
			geminate = false
			// Hepburn geminates by doubling the consonant, with ch → tch.
			switch {
			case strings.HasPrefix(rom, "ch"):
				b.WriteByte('t')
			case isConsonantByte(rom[0]):
				b.WriteByte(rom[0])
			}
		}
		b.WriteString(rom)

		for i := len(rom) - 1; i >= 0; i-- {
			if isVowelByte(rom[i]) {
				lastVowel = rom[i]
				break
			}
		}
	}

	for i := 0; i < len(runes); i++ {
		r := runes[i]

		switch r {
		case sokuon:
			geminate = true
			continue
		case choonpu:
			if lastVowel != 0 {
				b.WriteByte(lastVowel)
			}

			continue
		}

		if i+1 < len(runes) {
			if rom, ok := kanaDigraph[string(r)+string(runes[i+1])]; ok {
				emit(rom)
				i++

				continue
			}
		}
		if rom, ok := kanaMono[r]; ok {
			emit(rom)
			continue
		}

		// Non-kana: a dangling sokuon geminates nothing, and a following
		// long-vowel mark has no kana vowel to repeat.
		geminate = false
		lastVowel = 0
		b.WriteRune(r)
	}

	return b.String()
}

func isVowelByte(b byte) bool {
	switch b {
	case 'a', 'i', 'u', 'e', 'o':
		return true
	}
	return false
}

func isConsonantByte(b byte) bool {
	return b >= 'a' && b <= 'z' && !isVowelByte(b)
}

// NativeMatchKey returns a normalised key for direct native-script comparison
// of Japanese (or, incidentally, Chinese) titles: NFKC-folded, lower-cased,
// with whitespace, punctuation and symbols removed (letters and digits kept).
// It returns "" when s carries no Han/Hiragana/Katakana character, so a
// non-empty key doubles as the "this is a native-script title" predicate.
//
// NFKC folds full-width/half-width and compatibility forms together, and
// stripping spaces/punctuation makes "進撃 の 巨人", "進撃の巨人" and any
// alt-title spelling compare equal — where unidecode's Mandarin-pinyin output
// is neither stable nor correct.
func NativeMatchKey(s string) string {
	if !contentfilter.ContainsJapaneseScript(s) {
		return ""
	}
	return NativeKey(s)
}

// NativeKey is NativeMatchKey without the Japanese-script gate: the NFKC-folded
// letters+digits key over ANY script. The family registry uses it to re-key
// entities whose title erases to nothing under the Latin normalizer (native-
// script-only titles: CJK, hangul, etc.), so they collide on their real key
// instead of all colliding on the empty key.
func NativeKey(s string) string {
	var b strings.Builder
	for _, r := range norm.NFKC.String(s) {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			b.WriteRune(unicode.ToLower(r))
		}
	}
	return b.String()
}

package contentfilter

import "unicode"

// nonLatinScriptChar reports true iff `r` belongs to a non-Latin
// script that's a strong signal of non-English content. Conservative
// list: only the scripts where presence in a torrent title reliably
// indicates the title is not English. Latin extensions (accented
// characters used in many European languages) are NOT treated as
// non-Latin here — French/Spanish/German titles often contain them
// and are caught by the language-tag filter, not the script filter.
//
// The script set:
//
//	Cyrillic   — Russian, Ukrainian, Bulgarian, Serbian, etc.
//	Han        — Chinese, traditional/simplified
//	Hiragana   — Japanese
//	Katakana   — Japanese
//	Hangul     — Korean
//	Arabic     — Arabic, Persian, Urdu
//	Hebrew     — Hebrew
//	Devanagari — Hindi, Sanskrit, Marathi
//	Bengali    — Bengali, Assamese
//	Tamil      — Tamil
//	Thai       — Thai
//	Greek      — kept OUT of this list intentionally; lots of
//	             English titles use ε π etc. as symbols. False-
//	             positive risk too high.
func nonLatinScriptChar(r rune) bool {
	switch {
	case unicode.Is(unicode.Cyrillic, r):
		return true
	case unicode.Is(unicode.Han, r):
		return true
	case unicode.Is(unicode.Hiragana, r):
		return true
	case unicode.Is(unicode.Katakana, r):
		return true
	case unicode.Is(unicode.Hangul, r):
		return true
	case unicode.Is(unicode.Arabic, r):
		return true
	case unicode.Is(unicode.Hebrew, r):
		return true
	case unicode.Is(unicode.Devanagari, r):
		return true
	case unicode.Is(unicode.Bengali, r):
		return true
	case unicode.Is(unicode.Tamil, r):
		return true
	case unicode.Is(unicode.Thai, r):
		return true
	}
	return false
}

// titleContainsNonLatinScript returns true iff `s` has at least one
// character from one of the non-Latin scripts above. A single
// non-Latin char is enough — torrent titles are short enough that a
// run-length threshold would just add false negatives without
// reducing false positives.
func titleContainsNonLatinScript(s string) bool {
	for _, r := range s {
		if nonLatinScriptChar(r) {
			return true
		}
	}
	return false
}

// --- Japanese-narrow script detectors ---
//
// These are the same unicode.Is checks as nonLatinScriptChar, narrowed to
// the scripts used in written Japanese, and EXPORTED for the classifier's
// fuzzy matcher: a native-script (kanji/kana) anime title romanises through
// go-unidecode to Mandarin pinyin, not Japanese romaji, so the matcher needs
// to recognise Japanese-origin titles to transliterate kana to Hepburn and to
// compare native-script titles directly. Kept here so the script predicates
// live in one place.

// IsJapaneseScriptChar reports whether r belongs to a script used to write
// Japanese: Han (kanji), Hiragana, or Katakana. Han is shared with Chinese,
// so a Han character alone is "CJK", not exclusively Japanese; callers use
// this as a cheap "native East-Asian title" signal, not a language ID.
func IsJapaneseScriptChar(r rune) bool {
	return unicode.Is(unicode.Han, r) ||
		unicode.Is(unicode.Hiragana, r) ||
		unicode.Is(unicode.Katakana, r)
}

// IsKanaChar reports whether r is a Hiragana or Katakana character (excludes
// Han). Kana romanises deterministically to Hepburn; kanji does not.
func IsKanaChar(r rune) bool {
	return unicode.Is(unicode.Hiragana, r) || unicode.Is(unicode.Katakana, r)
}

// ContainsJapaneseScript reports whether s contains at least one Han,
// Hiragana, or Katakana character.
func ContainsJapaneseScript(s string) bool {
	for _, r := range s {
		if IsJapaneseScriptChar(r) {
			return true
		}
	}
	return false
}

// ContainsKana reports whether s contains at least one Hiragana or Katakana
// character.
func ContainsKana(s string) bool {
	for _, r := range s {
		if IsKanaChar(r) {
			return true
		}
	}
	return false
}

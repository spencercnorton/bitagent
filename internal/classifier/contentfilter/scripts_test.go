package contentfilter

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

//nolint:gosmopolitan // CJK script is exactly what these detectors classify.
func TestContainsJapaneseScript(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		in   string
		want bool
	}{
		{"kanji", "進撃の巨人", true},
		{"all-hiragana", "しんげきのきょじん", true},
		{"katakana", "カウボーイビバップ", true},
		{"han-only-chinese", "你好世界", true}, // Han is shared; treated as native East-Asian
		{"latin", "Attack on Titan", false},
		{"accented-latin", "Amélie", false},
		{"cyrillic", "Война и мир", false},
		{"korean-hangul", "오징어 게임", false}, // Hangul is deliberately excluded
		{"digits-punct", "2013 - 1080p", false},
		{"empty", "", false},
		{"mixed-latin-and-kanji", "Shingeki 進撃", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tc.want, ContainsJapaneseScript(tc.in))
		})
	}
}

//nolint:gosmopolitan // CJK script is exactly what these detectors classify.
func TestContainsKana(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		in   string
		want bool
	}{
		{"hiragana", "しんげき", true},
		{"katakana", "デスノート", true},
		{"kanji-only-no-kana", "巨人", false}, // Han is not kana
		{"chinese-han", "你好", false},
		{"latin", "Death Note", false},
		{"mixed-kanji-and-hiragana", "進撃の巨人", true}, // の is hiragana
		{"empty", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tc.want, ContainsKana(tc.in))
		})
	}
}

func TestIsKanaChar(t *testing.T) {
	t.Parallel()

	assert.True(t, IsKanaChar('し'))  // hiragana
	assert.True(t, IsKanaChar('ア'))  // katakana
	assert.False(t, IsKanaChar('巨')) // kanji
	assert.False(t, IsKanaChar('a'))
	assert.False(t, IsKanaChar('오')) // hangul
}

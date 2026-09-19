package animedb

import "testing"

func TestNormalize(t *testing.T) {
	t.Parallel()
	cases := []struct {
		in, want string
	}{
		{"KiseKoi", "kisekoi"},
		{"Kise Koi", "kisekoi"},
		{"Steins;Gate", "steinsgate"},
		{"STEINS·GATE", "steinsgate"},
		{"Fate/Zero", "fatezero"},
		{"3x3 Eyes", "3x3eyes"},
		{"Tōkyō Ghoul", "tokyoghoul"}, // macron fold
		{"Pokémon", "pokemon"},        // accent fold
		{"  Attack on Titan  ", "attackontitan"},
		{"Re:Zero -Starting Life-", "rezerostartinglife"},
		{"", ""},
	}
	for _, c := range cases {
		if got := Normalize(c.in); got != c.want {
			t.Errorf("Normalize(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// Non-Latin script is preserved (only Latin diacritics are folded), so a
// Japanese title stays matchable by its native form.
func TestNormalizePreservesKanji(t *testing.T) {
	t.Parallel()
	got := Normalize("進撃の巨人")
	if got != "進撃の巨人" {
		t.Errorf("Normalize kept-kanji = %q, want the kanji preserved", got)
	}
}

package titlenorm

import "testing"

func TestNormalizePartSpellings(t *testing.T) {
	t.Parallel()

	cases := map[string]string{
		"Dune Pt. 1":        "dune part 1",
		"Dune Pt 1":         "dune part 1",
		"Dune: Part One":    "dune: part 1",
		"Dune Part II":      "dune part 2",
		"Dune Pt II":        "dune part 2",
		"Movie Part Two":    "movie part 2",
		"Se7en":             "se7en",
		"Pt Reyes Station":  "pt reyes station",
		"The Parting Glass": "parting glass",
	}
	for in, want := range cases {
		if got := NormalizeTitleForMatch(in); got != want {
			t.Errorf("NormalizeTitleForMatch(%q) = %q, want %q", in, got, want)
		}
	}
}

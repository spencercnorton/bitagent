package classifier

import (
	"testing"

	"github.com/spencercnorton/bitagent/internal/classifier/llmmatch"
	"github.com/spencercnorton/bitagent/internal/model"
)

// TestLLMMatchTitleCompatibleAltTitles locks the alias evidence path.
// The cases are drawn from the 2026-08-04 production measurement: the gate was
// rejecting ~27.6% of the matcher's confident picks, roughly half of them
// wrongly. The critical property is asymmetry — an alias ACCEPTS, but the
// absence of an alias must still REJECT the look-alike pairs the gate exists
// to catch, which share identical surface shape with the correct ones.
func TestLLMMatchTitleCompatibleAltTitles(t *testing.T) {
	for _, tc := range []struct {
		label     string
		extracted string
		candidate string
		altTitles []string
		want      bool
	}{
		// Accepted: a catalogued alias is authoritative same-work evidence.
		{"JP->EN official title", "Dragon Quest: Dai no Daibouken",
			"Dragon Quest: The Adventure of Dai",
			[]string{"Dragon Quest: Dai no Daibouken"}, true},
		{"prefix added, same work", "El padrecito", "Cantinflas: El padrecito",
			[]string{"El Padrecito"}, true},
		{"suffix added, same work", "Cazuza", "Cazuza: Good News",
			[]string{"Cazuza"}, true},
		{"alias differs only by punctuation", "Rental a Girlfriend",
			"Kanojo, Okarishimasu", []string{"Rental-a-Girlfriend"}, true},

		// STILL rejected: identical surface shape, but no catalogued alias.
		// These are the pairs a containment or fuzzy rule would wrongly merge.
		{"prefix added, DIFFERENT work", "Skull Island", "Kong: Skull Island",
			[]string{"Kong: La Isla Calavera"}, false},
		{"sequel prefix, DIFFERENT work", "The Mighty Ducks", "D2: The Mighty Ducks",
			[]string{"D2: Superpatos"}, false},
		{"suffix added, DIFFERENT work", "Bluey", "Bluey Minisodes", nil, false},
		{"subtitle dropped, DIFFERENT work",
			"The Lord of the Rings: The Return of the King", "The Return of the King",
			[]string{"El retorno del rey"}, false},

		// Degenerate inputs keep rejecting.
		{"empty extraction", "", "Anything", []string{"Anything"}, false},
		{"empty alt title never matches empty-ish", "Some Film", "Other Film",
			[]string{""}, false},
		{"no alt titles at all, exact canonical", "Heat", "Heat", nil, true},
	} {
		got := llmMatchTitleCompatible(
			llmmatch.Extraction{Title: tc.extracted},
			llmmatch.Candidate{Title: tc.candidate, AltTitles: tc.altTitles},
		)
		if got != tc.want {
			t.Errorf("%s: extracted=%q candidate=%q alts=%v -> %v, want %v",
				tc.label, tc.extracted, tc.candidate, tc.altTitles, got, tc.want)
		}
	}
}

// TestAltTitlesOfIncludesOriginalTitle proves the alias set the gate sees is
// the same one contentMatchCandidates offers the deterministic path.
func TestAltTitlesOfIncludesOriginalTitle(t *testing.T) {
	content := model.Content{
		Title:         "Rental a Girlfriend",
		OriginalTitle: model.NewNullString("彼女、お借りします"),
		Attributes: []model.ContentAttribute{
			{Key: model.AltTitleAttributePrefix + "jp:abc", Value: "Kanojo, Okarishimasu"},
			{Key: "id", Value: "not-a-title"},
		},
	}
	got := altTitlesOf(content)
	want := map[string]bool{"Kanojo, Okarishimasu": true, "彼女、お借りします": true}
	if len(got) != len(want) {
		t.Fatalf("altTitlesOf = %v, want exactly %v", got, want)
	}
	for _, g := range got {
		if !want[g] {
			t.Errorf("altTitlesOf returned unexpected %q (non-title attributes must be excluded)", g)
		}
	}
}

package classifier

import (
	"testing"

	"github.com/spencercnorton/bitagent/internal/classifier/llmmatch"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

func TestMatcherCanaryRequiresIndependentSourceIdentity(t *testing.T) {
	for _, tc := range []struct {
		name, parsed string
		tv           bool
		chosen       llmmatch.Candidate
		want         string
	}{
		{"Dune.2021.1080p", "Dune", false, llmmatch.Candidate{Title: "Dune", Year: 2021}, ""},
		{"Dune.1984.1080p", "Dune", false, llmmatch.Candidate{Title: "Dune", Year: 2021}, "source_year"},
		{"Dune.1080p", "Dune", false, llmmatch.Candidate{Title: "Dune", Year: 2021}, "source_year"},
		{"Dune.2021.1080p", "Dune", false, llmmatch.Candidate{Title: "Dune"}, "source_year"},
		{"spyfam.17.05.01.aubrey.sinclair", "spyfam", true, llmmatch.Candidate{Title: "SPY x FAMILY"}, "source_title"},
		{"Mystery.2021.1080p", "Mystery", false, llmmatch.Candidate{Title: "Dune", Year: 2021}, "source_title"},
		{"El.padrecito.1964.1080p", "El padrecito", false, llmmatch.Candidate{Title: "Cantinflas: El padrecito", Year: 1964, AltTitles: []string{"El padrecito"}}, ""},
		{"Skull.Island.S01E01", "Skull Island", true, llmmatch.Candidate{Title: "Kong: Skull Island"}, "source_title"},
		{"The.Office.S01E01", "The Office", true, llmmatch.Candidate{Title: "The Office"}, ""},
		{"Dune.2021", "", false, llmmatch.Candidate{Title: "Dune", Year: 2021}, "source_title"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.want, EvaluationLLMMatchSourceGate(tc.name, tc.parsed, tc.tv, tc.chosen))
		})
	}
}

func TestSourceGateIsWiredIntoBothCandidatePaths(t *testing.T) {
	// Both local-mirror and API paths invoke matchRunner.candidateGate.
	cfg := llmmatch.NewDefaultConfig()
	cfg.RequireSourceTitle = true
	r := matchRunner{
		lm:          llmmatch.NewClient(cfg, nil, llmmatch.NewMetrics(), zap.NewNop().Sugar()),
		parsedTitle: "Some Other Film",
	}
	chosen := llmmatch.Candidate{ID: 1, Title: "Dune", Year: 2021}
	ext := llmmatch.Extraction{Title: "Dune", Year: 2021}
	require.Equal(t, "source_title", r.candidateGate("Some.Other.Film.2021", ext, false, chosen, []llmmatch.Candidate{chosen}))
	r.parsedTitle = "Dune"
	require.Empty(t, r.candidateGate("Dune.2021", ext, false, chosen, []llmmatch.Candidate{chosen}))
}

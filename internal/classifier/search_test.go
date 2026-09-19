package classifier

import (
	"testing"

	"github.com/spencercnorton/bitagent/internal/database/search"
	"github.com/spencercnorton/bitagent/internal/model"
	"github.com/stretchr/testify/require"
)

func TestLocalContentSearchQueries_FuzzyAddsBroadPasses(t *testing.T) {
	t.Parallel()

	got := localContentSearchQueries("The Raid Redemption", true)

	require.Equal(t, []string{
		"\"The Raid Redemption\"",
		"the raid redemption",
		"raid | redemption",
	}, got)
}

func TestLocalContentSearchQueries_FuzzyOffKeepsLegacyPhraseOnly(t *testing.T) {
	t.Parallel()

	got := localContentSearchQueries("The Raid Redemption", false)

	require.Equal(t, []string{"\"The Raid Redemption\""}, got)
}

func TestLocalContentSearchPasses_TVAddsYearlessFallback(t *testing.T) {
	t.Parallel()

	passes := localContentSearchPasses("Jeopardy", model.ContentTypeTvShow, model.Year(2024), true, 10)

	require.NotEmpty(t, passes)
	var yearless int
	for _, pass := range passes {
		if !pass.useYear {
			yearless++
		}
	}
	require.Greater(t, yearless, 0)
}

func TestLocalContentSearchPasses_MovieKeepsYearGuard(t *testing.T) {
	t.Parallel()

	passes := localContentSearchPasses("The Batman", model.ContentTypeMovie, model.Year(2022), true, 10)

	require.NotEmpty(t, passes)
	for _, pass := range passes {
		require.True(t, pass.useYear)
	}
}

func TestLocalContentYearPenalty_TVIgnoresTorrentYear(t *testing.T) {
	t.Parallel()

	item := search.ContentResultItem{
		Content: model.Content{ReleaseYear: model.Year(1984)},
	}

	require.Nil(t, localContentYearPenalty(model.ContentTypeTvShow, model.Year(2024)))
	require.Equal(t, 100, localContentYearPenalty(model.ContentTypeMovie, model.Year(2024))(item))
}

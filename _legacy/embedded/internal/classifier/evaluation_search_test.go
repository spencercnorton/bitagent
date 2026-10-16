package classifier_test

import (
	"context"
	"testing"

	"github.com/spencercnorton/bitagent/internal/classifier"
	"github.com/spencercnorton/bitagent/internal/classifier/llmmatch"
	classifier_mocks "github.com/spencercnorton/bitagent/internal/classifier/mocks"
	"github.com/spencercnorton/bitagent/internal/model"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

func TestEvaluationLLMCandidatesUsesProductionLocalQuerySequence(t *testing.T) {
	search := classifier_mocks.NewLocalSearch(t)
	search.On(
		"ContentCandidatesBySearch",
		mock.Anything,
		model.ContentTypeTvShow,
		"Example Show",
		model.Year(2024),
		6,
	).
		Return([]model.Content{}, nil).
		Once()
	expected := []model.Content{{
		Type:   model.ContentTypeTvShow,
		Source: model.SourceTmdb,
		ID:     "123",
		Title:  "Example Show",
	}}
	search.On(
		"ContentCandidatesBySearch",
		mock.Anything,
		model.ContentTypeTvShow,
		"Example Show",
		model.Year(0),
		6,
	).
		Return(expected, nil).
		Once()

	got, err := classifier.EvaluationLLMCandidates(
		context.Background(),
		search,
		model.ContentTypeTvShow,
		llmmatch.Extraction{
			Title: "Example Show",
			Year:  2024,
			Type:  "tv",
			OK:    true,
		},
		6,
	)
	require.NoError(t, err)
	require.Equal(t, expected, got)
}

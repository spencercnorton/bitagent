package purgecontenttypescmd

import (
	"testing"

	"github.com/spencercnorton/bitagent/internal/model"
	"github.com/stretchr/testify/require"
)

func TestParseTypesAcceptsOutOfScopeTypes(t *testing.T) {
	t.Parallel()

	types, err := parseTypes([]string{"music", "ebook", "audiobook", "comic", "game", "software"})
	require.NoError(t, err)
	require.Equal(t, []model.ContentType{
		model.ContentTypeMusic,
		model.ContentTypeEbook,
		model.ContentTypeAudiobook,
		model.ContentTypeComic,
		model.ContentTypeGame,
		model.ContentTypeSoftware,
	}, types)
}

func TestParseTypesDeduplicates(t *testing.T) {
	t.Parallel()

	types, err := parseTypes([]string{"music", "music", "ebook"})
	require.NoError(t, err)
	require.Equal(t, []model.ContentType{model.ContentTypeMusic, model.ContentTypeEbook}, types)
}

func TestParseTypesRefusesInScopeTypes(t *testing.T) {
	t.Parallel()

	for _, inScope := range []string{"movie", "tv_show"} {
		_, err := parseTypes([]string{"music", inScope})
		require.ErrorContains(t, err, "refusing to purge in-scope content type")
	}
}

func TestParseTypesRejectsUnknownAndInvalid(t *testing.T) {
	t.Parallel()

	_, err := parseTypes([]string{"unknown"})
	require.Error(t, err)

	_, err = parseTypes([]string{"musik"})
	require.Error(t, err)

	_, err = parseTypes(nil)
	require.Error(t, err)
}

func TestMissingClassifyTimeDeletes(t *testing.T) {
	t.Parallel()

	types := []model.ContentType{model.ContentTypeMusic, model.ContentTypeEbook}

	require.Empty(t, missingClassifyTimeDeletes(types, []string{"music", "ebook", "audiobook"}))
	require.Equal(t,
		[]model.ContentType{model.ContentTypeEbook},
		missingClassifyTimeDeletes(types, []string{"music"}),
	)
	require.Equal(t, types, missingClassifyTimeDeletes(types, nil))
}

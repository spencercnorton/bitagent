package tmdb

import (
	"fmt"
	"strings"
	"testing"

	"github.com/spencercnorton/bitagent/internal/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func altTitle(iso, title string) AlternativeTitle {
	return AlternativeTitle{Iso3166_1: iso, Title: title}
}

func translation(iso, title, name string) Translation {
	t := Translation{Iso3166_1: iso, Iso639_1: strings.ToLower(iso)}
	t.Data.Title = title
	t.Data.Name = name

	return t
}

//nolint:gosmopolitan // CJK alt-titles are exactly what this feature matches.
func TestAltTitleAttributes(t *testing.T) {
	t.Parallel()

	t.Run("maps alternative titles and translations", func(t *testing.T) {
		t.Parallel()

		attrs := AltTitleAttributes(
			[]string{"Demon Slayer: Kimetsu no Yaiba"},
			[]AlternativeTitle{altTitle("JP", "鬼滅の刃")},
			[]Translation{translation("BR", "", "Demon Slayer: Kimetsu no Yaiba - Em Busca de Vingança")},
		)

		require.Len(t, attrs, 2)

		values := []string{attrs[0].Value, attrs[1].Value}
		assert.Contains(t, values, "鬼滅の刃")
		assert.Contains(t, values, "Demon Slayer: Kimetsu no Yaiba - Em Busca de Vingança")

		for _, a := range attrs {
			assert.Equal(t, model.SourceTmdb, a.Source)
			assert.True(t, strings.HasPrefix(a.Key, model.AltTitleAttributePrefix), a.Key)
		}
	})

	t.Run("drops titles equal to a canonical title", func(t *testing.T) {
		t.Parallel()

		attrs := AltTitleAttributes(
			[]string{"The Matrix", "La Matrice"},
			[]AlternativeTitle{
				altTitle("US", "The  Matrix"), // whitespace + case normalize
				altTitle("FR", "la matrice"),
				altTitle("DE", "Die Matrix"),
			},
			nil,
		)

		require.Len(t, attrs, 1)
		assert.Equal(t, "Die Matrix", attrs[0].Value)
	})

	t.Run("dedupes across alternative titles and translations", func(t *testing.T) {
		t.Parallel()

		attrs := AltTitleAttributes(
			[]string{"Oldboy"},
			[]AlternativeTitle{altTitle("KR", "올드보이")},
			[]Translation{translation("KR", "올드보이", "")},
		)

		require.Len(t, attrs, 1)
	})

	t.Run("keys are deterministic regardless of input order", func(t *testing.T) {
		t.Parallel()

		forward := AltTitleAttributes(
			[]string{"X"},
			[]AlternativeTitle{altTitle("DE", "Ein Titel"), altTitle("FR", "Un Titre")},
			nil,
		)
		reversed := AltTitleAttributes(
			[]string{"X"},
			[]AlternativeTitle{altTitle("FR", "Un Titre"), altTitle("DE", "Ein Titel")},
			nil,
		)

		require.Equal(t, forward, reversed)
	})

	t.Run("skips empty titles and caps the total", func(t *testing.T) {
		t.Parallel()

		alts := []AlternativeTitle{altTitle("US", "  ")}
		for i := range 100 {
			alts = append(alts, altTitle("US", fmt.Sprintf("Title Variant %d", i)))
		}

		attrs := AltTitleAttributes([]string{"X"}, alts, nil)

		assert.Len(t, attrs, maxAltTitleAttributes)
	})
}

func TestMovieDetailsToMovieModel_altTitles(t *testing.T) {
	t.Parallel()

	details := MovieDetailsResponse{
		ID:            603,
		Title:         "The Matrix",
		OriginalTitle: "The Matrix",
	}
	details.AlternativeTitles.Titles = []AlternativeTitle{altTitle("DE", "Die Matrix")}
	details.Translations.Translations = []Translation{translation("FR", "La Matrice", "")}

	movie, err := MovieDetailsToMovieModel(details)
	require.NoError(t, err)

	var altValues []string

	for _, a := range movie.Attributes {
		if strings.HasPrefix(a.Key, model.AltTitleAttributePrefix) {
			altValues = append(altValues, a.Value)
		}
	}

	assert.ElementsMatch(t, []string{"Die Matrix", "La Matrice"}, altValues)
}

//nolint:gosmopolitan // CJK alt-titles are exactly what this feature matches.
func TestTvShowDetailsToTvShowModel_altTitles(t *testing.T) {
	t.Parallel()

	details := TvDetailsResponse{
		ID:           1429,
		Name:         "Attack on Titan",
		OriginalName: "進撃の巨人",
	}
	details.AlternativeTitles.Results = []AlternativeTitle{altTitle("JP", "Shingeki no Kyojin")}
	details.Translations.Translations = []Translation{translation("BR", "", "Ataque dos Titãs")}

	tvShow, err := TvShowDetailsToTvShowModel(details)
	require.NoError(t, err)

	var altValues []string

	for _, a := range tvShow.Attributes {
		if strings.HasPrefix(a.Key, model.AltTitleAttributePrefix) {
			altValues = append(altValues, a.Value)
		}
	}

	assert.ElementsMatch(t, []string{"Shingeki no Kyojin", "Ataque dos Titãs"}, altValues)
}

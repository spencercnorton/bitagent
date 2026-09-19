package tmdb

import (
	"crypto/sha1"
	"encoding/hex"
	"sort"
	"strconv"
	"strings"

	"github.com/spencercnorton/bitagent/internal/classifier/classification"
	"github.com/spencercnorton/bitagent/internal/model"
	"github.com/spencercnorton/bitagent/internal/slice"
)

// maxAltTitleAttributes bounds how many alternative-title attributes a single
// content row can accumulate — some franchise titles carry 50+ AKAs and the
// long tail is noise for matching purposes.
const maxAltTitleAttributes = 64

// normalizeAltTitleKey collapses whitespace and case so the same title spelled
// slightly differently across alternative_titles and translations dedupes.
func normalizeAltTitleKey(s string) string {
	return strings.ToLower(strings.Join(strings.Fields(s), " "))
}

// AltTitleAttributes converts the alternative_titles + translations appendices
// of a details response into content attributes. Titles equal to any canonical
// title are dropped; keys are alt_title:<iso3166>:<hash8> — deterministic
// across re-fetches (TMDB does not guarantee ordering) so repeated syncs
// upsert the same rows instead of accumulating duplicates.
func AltTitleAttributes(
	canonicalTitles []string,
	altTitles []AlternativeTitle,
	translations []Translation,
) []model.ContentAttribute {
	seen := make(map[string]struct{}, len(canonicalTitles))
	for _, t := range canonicalTitles {
		seen[normalizeAltTitleKey(t)] = struct{}{}
	}

	var attrs []model.ContentAttribute

	add := func(iso3166, title string) {
		title = strings.TrimSpace(title)
		if title == "" {
			return
		}

		norm := normalizeAltTitleKey(title)
		if _, ok := seen[norm]; ok {
			return
		}

		seen[norm] = struct{}{}

		sum := sha1.Sum([]byte(norm))
		attrs = append(attrs, model.ContentAttribute{
			Source: model.SourceTmdb,
			Key: model.AltTitleAttributePrefix + strings.ToLower(
				iso3166,
			) + ":" + hex.EncodeToString(
				sum[:4],
			),
			Value: title,
		})
	}

	for _, t := range altTitles {
		add(t.Iso3166_1, t.Title)
	}

	for _, t := range translations {
		title := t.Data.Title
		if title == "" {
			title = t.Data.Name
		}

		add(t.Iso3166_1, title)
	}

	// Sort before capping so the retained subset is deterministic.
	sort.Slice(attrs, func(i, j int) bool { return attrs[i].Key < attrs[j].Key })

	if len(attrs) > maxAltTitleAttributes {
		attrs = attrs[:maxAltTitleAttributes]
	}

	return attrs
}

func MovieDetailsToMovieModel(details MovieDetailsResponse) (movie model.Content, err error) {
	releaseDate := model.Date{}

	if details.ReleaseDate != "" {
		parsedDate, parseDateErr := model.NewDateFromIsoString(details.ReleaseDate)
		if parseDateErr != nil {
			err = parseDateErr
			return
		}

		releaseDate = parsedDate
	}

	//nolint:prealloc
	var collections []model.ContentCollection

	if details.BelongsToCollection.ID != 0 {
		collections = append(collections, model.ContentCollection{
			Type:   "franchise",
			Source: model.SourceTmdb,
			ID:     strconv.Itoa(int(details.BelongsToCollection.ID)),
			Name:   details.BelongsToCollection.Name,
		})
	}

	for _, genre := range details.Genres {
		collections = append(collections, model.ContentCollection{
			Type:   "genre",
			Source: model.SourceTmdb,
			ID:     strconv.Itoa(int(genre.ID)),
			Name:   genre.Name,
		})
	}

	var attributes []model.ContentAttribute
	if details.IMDbID != "" {
		attributes = append(attributes, model.ContentAttribute{
			Source: "imdb",
			Key:    "id",
			Value:  details.IMDbID,
		})
	}

	if details.PosterPath != "" {
		attributes = append(attributes, model.ContentAttribute{
			Source: "tmdb",
			Key:    "poster_path",
			Value:  details.PosterPath,
		})
	}

	if details.BackdropPath != "" {
		attributes = append(attributes, model.ContentAttribute{
			Source: "tmdb",
			Key:    "backdrop_path",
			Value:  details.BackdropPath,
		})
	}

	attributes = append(attributes, AltTitleAttributes(
		[]string{details.Title, details.OriginalTitle},
		details.AlternativeTitles.Titles,
		details.Translations.Translations,
	)...)

	releaseYear := releaseDate.Year

	contentType := model.ContentTypeMovie

	if details.Adult {
		contentType = model.ContentTypeXxx
	}

	return model.Content{
		Type:             contentType,
		Source:           model.SourceTmdb,
		ID:               strconv.Itoa(int(details.ID)),
		Title:            details.Title,
		ReleaseDate:      releaseDate,
		ReleaseYear:      releaseYear,
		Adult:            model.NewNullBool(details.Adult),
		OriginalLanguage: model.ParseLanguage(details.OriginalLanguage),
		OriginalTitle:    model.NewNullString(details.OriginalTitle),
		Overview: model.NullString{
			String: details.Overview,
			Valid:  details.Overview != "",
		},
		Runtime: model.NullUint16{
			Uint16: uint16(details.Runtime),
			Valid:  details.Runtime > 0,
		},
		Popularity:  model.NewNullFloat32(details.Popularity),
		VoteAverage: model.NewNullFloat32(details.VoteAverage),
		VoteCount:   model.NewNullUint(uint(details.VoteCount)),
		Collections: collections,
		Attributes:  attributes,
	}, nil
}

func TvShowDetailsToTvShowModel(details TvDetailsResponse) (movie model.Content, err error) {
	firstAirDate := model.Date{}

	if details.FirstAirDate != "" {
		parsedDate, parseDateErr := model.NewDateFromIsoString(details.FirstAirDate)
		if parseDateErr != nil {
			err = parseDateErr
			return
		}

		firstAirDate = parsedDate
	}

	collections := slice.Map(details.Genres, func(genre Genre) model.ContentCollection {
		return model.ContentCollection{
			Type:   "genre",
			Source: model.SourceTmdb,
			ID:     strconv.Itoa(int(genre.ID)),
			Name:   genre.Name,
		}
	})

	var attributes []model.ContentAttribute

	if details.ExternalIDs.IMDbID != "" {
		attributes = append(attributes, model.ContentAttribute{
			Source: model.SourceImdb,
			Key:    "id",
			Value:  details.ExternalIDs.IMDbID,
		})
	}

	if details.ExternalIDs.TVDBID != 0 {
		attributes = append(attributes, model.ContentAttribute{
			Source: model.SourceTvdb,
			Key:    "id",
			Value:  strconv.Itoa(int(details.ExternalIDs.TVDBID)),
		})
	}

	releaseYear := firstAirDate.Year

	if details.PosterPath != "" {
		attributes = append(attributes, model.ContentAttribute{
			Source: model.SourceTmdb,
			Key:    "poster_path",
			Value:  details.PosterPath,
		})
	}

	if details.BackdropPath != "" {
		attributes = append(attributes, model.ContentAttribute{
			Source: model.SourceTmdb,
			Key:    "backdrop_path",
			Value:  details.BackdropPath,
		})
	}

	attributes = append(attributes, AltTitleAttributes(
		[]string{details.Name, details.OriginalName},
		details.AlternativeTitles.Results,
		details.Translations.Translations,
	)...)

	return model.Content{
		Type:             model.ContentTypeTvShow,
		Source:           model.SourceTmdb,
		ID:               strconv.Itoa(int(details.ID)),
		Title:            details.Name,
		ReleaseDate:      firstAirDate,
		ReleaseYear:      releaseYear,
		OriginalLanguage: model.ParseLanguage(details.OriginalLanguage),
		OriginalTitle:    model.NewNullString(details.OriginalName),
		Overview: model.NullString{
			String: details.Overview,
			Valid:  details.Overview != "",
		},
		Popularity:  model.NewNullFloat32(details.Popularity),
		VoteAverage: model.NewNullFloat32(details.VoteAverage),
		VoteCount:   model.NewNullUint(uint(details.VoteCount)),
		Collections: collections,
		Attributes:  attributes,
	}, nil
}

func ExternalSource(ref model.ContentRef) (externalSource string, externalID string, err error) {
	switch {
	case (ref.Type == model.ContentTypeMovie ||
		ref.Type == model.ContentTypeTvShow ||
		ref.Type == model.ContentTypeXxx) &&
		ref.Source == model.SourceImdb:
		externalSource = "imdb_id"
		externalID = ref.ID
	case ref.Type == model.ContentTypeTvShow && ref.Source == model.SourceTvdb:
		externalSource = "tvdb_id"
		externalID = ref.ID
	default:
		err = classification.ErrUnmatched
	}

	return
}

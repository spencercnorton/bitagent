package classifier

import (
	"strconv"
	"strings"

	"github.com/spencercnorton/bitagent/internal/classifier/classification"
	"github.com/spencercnorton/bitagent/internal/model"
	"github.com/spencercnorton/bitagent/internal/tmdb"
)

func (c executionContext) tmdbSearchMovie(title string, year model.Year) (model.Content, error) {
	req := tmdb.SearchMovieRequest{
		Query:        title,
		IncludeAdult: true,
	}
	if !year.IsNil() && !c.fuzzyMatchEnabled {
		// Exact year filter: when fuzzy mode is off, pass year to the API so
		// TMDB pre-filters the result set, matching historical behavior.
		req.Year = year
	}
	// When fuzzy mode is on the year param is omitted so TMDB returns
	// results from any year; year proximity is used as a scoring tiebreaker
	// inside fuzzyFindBestMatch.

	searchResult, searchErr := c.tmdbClient.SearchMovie(c.Context, req)
	if searchErr != nil {
		return model.Content{}, searchErr
	}

	queryYear := int(year)
	bestMatch, ok := fuzzyFindBestMatch[tmdb.SearchMovieResult](
		title,
		searchResult.Results,
		func(item tmdb.SearchMovieResult) []string {
			return []string{item.Title, item.OriginalTitle}
		},
		func(item tmdb.SearchMovieResult) int {
			return yearProximityPenalty(queryYear, yearFromDateString(item.ReleaseDate))
		},
		c.fuzzyMatchEnabled,
	)

	if !ok {
		return model.Content{}, classification.ErrUnmatched
	}

	return c.tmdbGetMovieByTMDBID(bestMatch.ID)
}

func (c executionContext) tmdbSearchTVShow(title string, year model.Year) (model.Content, error) {
	req := tmdb.SearchTvRequest{
		Query:        title,
		IncludeAdult: true,
	}
	if !year.IsNil() && !c.fuzzyMatchEnabled {
		// Exact year filter: same logic as tmdbSearchMovie.
		req.FirstAirDateYear = year
	}

	searchResult, searchErr := c.tmdbClient.SearchTv(c.Context, req)
	if searchErr != nil {
		return model.Content{}, searchErr
	}

	queryYear := int(year)
	bestMatch, ok := fuzzyFindBestMatch[tmdb.SearchTvResult](
		title,
		searchResult.Results,
		func(item tmdb.SearchTvResult) []string {
			return []string{item.Name, item.OriginalName}
		},
		func(item tmdb.SearchTvResult) int {
			return yearProximityPenalty(queryYear, yearFromDateString(item.FirstAirDate))
		},
		c.fuzzyMatchEnabled,
	)

	if !ok {
		return model.Content{}, classification.ErrUnmatched
	}

	return c.tmdbGetTVShowByTMDBID(bestMatch.ID)
}

// yearFromDateString extracts the year component from a TMDB date string of
// the form "YYYY-MM-DD". Returns 0 on parse failure or empty input.
func yearFromDateString(s string) int {
	if s == "" {
		return 0
	}
	parts := strings.SplitN(s, "-", 2)
	if len(parts) == 0 {
		return 0
	}
	y, err := strconv.Atoi(parts[0])
	if err != nil {
		return 0
	}
	return y
}

func (c executionContext) tmdbGetMovieByTMDBID(id int64) (model.Content, error) {
	return tmdbContentByID(c.Context, c.tmdbClient, false, id)
}

func (c executionContext) tmdbGetTVShowByTMDBID(id int64) (model.Content, error) {
	return tmdbContentByID(c.Context, c.tmdbClient, true, id)
}

func (c executionContext) tmdbGetTMDBIDByExternalID(ref model.ContentRef) (int64, error) {
	externalSource, externalID, externalSourceErr := tmdb.ExternalSource(ref)
	if externalSourceErr != nil {
		return 0, externalSourceErr
	}

	byIDResult, byIDErr := c.tmdbClient.FindByID(c.Context, tmdb.FindByIDRequest{
		ExternalSource: externalSource,
		ExternalID:     externalID,
	})
	if byIDErr != nil {
		return 0, byIDErr
	}

	switch ref.Type {
	case model.ContentTypeMovie, model.ContentTypeXxx:
		if len(byIDResult.MovieResults) == 0 {
			return 0, classification.ErrUnmatched
		}

		return byIDResult.MovieResults[0].ID, nil
	case model.ContentTypeTvShow:
		if len(byIDResult.TvResults) == 0 {
			return 0, classification.ErrUnmatched
		}

		return byIDResult.TvResults[0].ID, nil
	default:
		return 0, classification.ErrUnmatched
	}
}

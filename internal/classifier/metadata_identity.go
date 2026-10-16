package classifier

import (
	"context"
	"errors"
	"github.com/spencercnorton/bitagent/internal/classifier/classification"
	"github.com/spencercnorton/bitagent/internal/model"
	"github.com/spencercnorton/bitagent/internal/tmdb"
)

func absInt(v int) int {
	if v < 0 {
		return -v
	}
	return v
}

// tmdbContentByID fetches full content details for a chosen tmdb id, mapping a
// TMDB not-found to ErrUnmatched (the id was valid at search time but the
// details endpoint 404s — treat as no match, not a hard error).
func tmdbContentByID(ctx context.Context, client tmdb.Client, isTV bool, id int64) (model.Content, error) {
	if isTV {
		d, err := client.TvDetails(ctx, tmdb.TvDetailsRequest{
			SeriesID:         id,
			AppendToResponse: []string{"external_ids", "alternative_titles", "translations"},
		})
		if err != nil {
			if errors.Is(err, tmdb.ErrNotFound) {
				return model.Content{}, classification.ErrUnmatched
			}
			return model.Content{}, err
		}
		return tmdb.TvShowDetailsToTvShowModel(d)
	}
	d, err := client.MovieDetails(ctx, tmdb.MovieDetailsRequest{
		ID:               id,
		AppendToResponse: []string{"alternative_titles", "translations"},
	})
	if err != nil {
		if errors.Is(err, tmdb.ErrNotFound) {
			return model.Content{}, classification.ErrUnmatched
		}
		return model.Content{}, err
	}
	return tmdb.MovieDetailsToMovieModel(d)
}

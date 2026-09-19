package torznab

import (
	"github.com/spencercnorton/bitagent/internal/model"
)

type SearchRequest struct {
	Profile Profile
	Query   string
	Type    string
	Cats    []int
	IMDBID  model.NullString
	TMDBID  model.NullString
	Season  model.NullInt
	Episode model.NullInt
	// AirDate is the daily-show form of an episode request: Torznab
	// season=YYYY&ep=MM/DD. Zero when the request is not date-based.
	AirDate  model.Date
	Attrs    []string
	Extended bool
	Limit    model.NullUint
	Offset   model.NullUint
}

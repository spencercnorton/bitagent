package httpserver

import (
	"testing"
	"time"

	"github.com/spencercnorton/bitagent/internal/model"
	"github.com/stretchr/testify/assert"
)

func TestParseDailyEpisode(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name   string
		year   int
		ep     string
		want   model.Date
		wantOK bool
	}{
		{"daily form", 2026, "06/15", model.NewDateFromParts(2026, time.June, 15), true},
		{"single-digit parts", 2023, "6/5", model.NewDateFromParts(2023, time.June, 5), true},
		{"numeric season is not a year", 26, "06/15", model.Date{}, false},
		{"plain episode is not a date", 2026, "615", model.Date{}, false},
		{"invalid month", 2026, "13/05", model.Date{}, false},
		{"invalid day", 2026, "02/30", model.Date{}, false},
		{"junk", 2026, "aa/bb", model.Date{}, false},
		{"uint8-wrapping day must not truncate to a valid date", 2026, "06/270", model.Date{}, false},
		{"uint8-wrapping day variant", 2026, "06/527", model.Date{}, false},
		{"month overflow", 2026, "270/06", model.Date{}, false},
		{"too many parts", 2026, "06/15/2026", model.Date{}, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, ok := parseDailyEpisode(tc.year, tc.ep)
			assert.Equal(t, tc.wantOK, ok)
			assert.Equal(t, tc.want, got)
		})
	}
}

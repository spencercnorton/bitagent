package model

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestDeriveReleaseGranularity(t *testing.T) {
	t.Parallel()

	tv := NewNullContentType(ContentTypeTvShow)
	movie := NewNullContentType(ContentTypeMovie)

	cases := []struct {
		name        string
		contentType NullContentType
		episodes    Episodes
		relName     string
		want        NullReleaseGranularity
	}{
		// shape-driven
		{"single episode", tv, Episodes{1: {5: {}}}, "Show.S01E05.1080p", NewNullReleaseGranularity(ReleaseGranularityEpisode)},
		{"double episode", tv, Episodes{4: {35: {}, 36: {}}}, "PAW.Patrol.S04E35E36.720p", NewNullReleaseGranularity(ReleaseGranularityMultiEpisode)},
		{"triple episode", tv, Episodes{3: {5: {}, 6: {}, 7: {}}}, "Show.S03E05E06E07", NewNullReleaseGranularity(ReleaseGranularityMultiEpisode)},
		{"four episodes is a partial season", tv, Episodes{1: {1: {}, 2: {}, 3: {}, 4: {}}}, "Show.S01E01-E04", NewNullReleaseGranularity(ReleaseGranularityPartialSeason)},
		{"half season range", tv, Episodes{2: {1: {}, 2: {}, 3: {}, 4: {}, 5: {}, 6: {}, 7: {}}}, "Show.S02E01-E07.1080p", NewNullReleaseGranularity(ReleaseGranularityPartialSeason)},
		{"whole season", tv, Episodes{5: {}}, "Show.S05.COMPLETE.1080p", NewNullReleaseGranularity(ReleaseGranularitySeason)},
		{"multi season", tv, Episodes{1: {}, 2: {}, 3: {}}, "Show.S01-S03.1080p", NewNullReleaseGranularity(ReleaseGranularityMultiSeason)},
		{"mixed multi season detail still multi season", tv, Episodes{1: {}, 2: {5: {}}}, "Show.S01.S02E05", NewNullReleaseGranularity(ReleaseGranularityMultiSeason)},

		// keyword-driven
		{"complete series no markers", tv, nil, "Show.Complete.Series.1080p.WEB", NewNullReleaseGranularity(ReleaseGranularityCompleteSeries)},
		{"bracketed anime batch", tv, nil, "[Group] Show (01-24) [Batch]", NewNullReleaseGranularity(ReleaseGranularityCompleteSeries)},
		{"complete series overrides multi season markers", tv, Episodes{1: {}, 2: {}}, "Show.S01-S02.Complete.Series", NewNullReleaseGranularity(ReleaseGranularityCompleteSeries)},
		{"complete series does not override single season", tv, Episodes{5: {}}, "Show.S05.Complete.Series...weird", NewNullReleaseGranularity(ReleaseGranularitySeason)},

		// guards
		{"bad batch title is not a batch", tv, Episodes{2: {3: {}}}, "Star.Wars.The.Bad.Batch.S02E03.1080p", NewNullReleaseGranularity(ReleaseGranularityEpisode)},
		{"bare batch word is not a series claim", tv, nil, "Batch.Cooking.With.Jamie.1080p", NullReleaseGranularity{}},
		{"bare complete is not a series claim", tv, nil, "Show.COMPLETE.1080p", NullReleaseGranularity{}},
		{"incomplete series is not a series claim", tv, nil, "Doctor.Who.The.Incomplete.Series.1080p.WEB", NullReleaseGranularity{}},
		{"miniseries complete is not a series claim", tv, nil, "Chernobyl.Miniseries.Complete.1080p", NullReleaseGranularity{}},
		{"complete series at name start", tv, nil, "Complete.Series.Show.1080p", NewNullReleaseGranularity(ReleaseGranularityCompleteSeries)},
		{"tv with no info stays unknown", tv, nil, "Show.1080p.WEB", NullReleaseGranularity{}},
		{"empty episodes map same as nil", tv, Episodes{}, "Show.1080p.WEB", NullReleaseGranularity{}},
		{"movie never gets granularity", movie, Episodes{1: {5: {}}}, "Movie.Complete.Series.2020", NullReleaseGranularity{}},
		{"invalid content type never gets granularity", NullContentType{}, Episodes{1: {5: {}}}, "Show.S01E05", NullReleaseGranularity{}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := DeriveReleaseGranularity(tc.contentType, tc.episodes, tc.relName)
			assert.Equal(t, tc.want, got)
		})
	}
}

package granularitybackfillcmd

import (
	"testing"

	"github.com/spencercnorton/bitagent/internal/model"
	"github.com/stretchr/testify/assert"
)

func TestBucketChanges(t *testing.T) {
	t.Parallel()

	tv := model.NewNullContentType(model.ContentTypeTvShow)
	rows := []*model.TorrentContent{
		{ // NULL -> episode
			ID: "a", ContentType: tv, Episodes: model.Episodes{1: {5: {}}},
			Torrent: model.Torrent{Name: "Show.S01E05.1080p"},
		},
		{ // already correct -> untouched
			ID: "b", ContentType: tv, Episodes: model.Episodes{1: {5: {}}},
			Torrent:            model.Torrent{Name: "Show.S01E05.1080p"},
			ReleaseGranularity: model.NewNullReleaseGranularity(model.ReleaseGranularityEpisode),
		},
		{ // stale label -> multi_episode
			ID: "c", ContentType: tv, Episodes: model.Episodes{4: {35: {}, 36: {}}},
			Torrent:            model.Torrent{Name: "PAW.Patrol.S04E35E36.720p"},
			ReleaseGranularity: model.NewNullReleaseGranularity(model.ReleaseGranularityEpisode),
		},
		{ // derives NULL, stored NULL -> untouched (Set-flag mismatch must not matter)
			ID: "d", ContentType: tv,
			Torrent: model.Torrent{Name: "Show.1080p.WEB"},
		},
		{ // derives NULL, stored non-NULL -> cleared
			ID: "e", ContentType: tv,
			Torrent:            model.Torrent{Name: "Show.1080p.WEB"},
			ReleaseGranularity: model.NewNullReleaseGranularity(model.ReleaseGranularitySeason),
		},
	}

	buckets := bucketChanges(rows)

	assert.Equal(t, map[string][]string{
		"episode":       {"a"},
		"multi_episode": {"c"},
		nullLabel:       {"e"},
	}, buckets)
}

func TestDateChanges(t *testing.T) {
	t.Parallel()

	tv := model.NewNullContentType(model.ContentTypeTvShow)
	rows := []*model.TorrentContent{
		{ // NULL -> parsed date
			ID: "a", ContentType: tv,
			Torrent: model.Torrent{Name: "The.Daily.Show.2026.03.11.720p.WEB"},
		},
		{ // already equal -> untouched
			ID: "b", ContentType: tv,
			Torrent:     model.Torrent{Name: "The.Daily.Show.2026.03.11.720p.WEB"},
			ReleaseDate: model.NewDateFromParts(2026, 3, 11),
		},
		{ // no date in name, stored date -> NEVER cleared
			ID: "c", ContentType: tv,
			Torrent:     model.Torrent{Name: "Show.S01E05.1080p"},
			ReleaseDate: model.NewDateFromParts(2026, 1, 1),
		},
		{ // no date anywhere -> untouched
			ID: "d", ContentType: tv,
			Torrent: model.Torrent{Name: "Show.S01E05.1080p"},
		},
	}

	got := dateChanges(rows)
	assert.Equal(t, []dateChange{{id: "a", date: model.NewDateFromParts(2026, 3, 11)}}, got)
}

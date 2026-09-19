package granularitybackfillcmd

import (
	"testing"

	"github.com/spencercnorton/bitagent/internal/model"
	"github.com/stretchr/testify/assert"
)

func TestAbsoluteChanges(t *testing.T) {
	t.Parallel()

	tv := model.NewNullContentType(model.ContentTypeTvShow)
	rows := []*model.TorrentContent{
		{ // anime with absolute -> filled
			ID: "a", ContentType: tv,
			Torrent: model.Torrent{Name: "[SubsPlease] One Piece - 1071 (1080p) [ABCD1234].mkv"},
		},
		{ // already correct -> untouched
			ID: "b", ContentType: tv,
			Torrent:              model.Torrent{Name: "[SubsPlease] One Piece - 1071 (1080p).mkv"},
			AnimeAbsoluteEpisode: model.NewNullUint(1071),
		},
		{ // dash-number but NOT anime -> no stamp
			ID: "c", ContentType: tv,
			Torrent: model.Torrent{Name: "Some Western Show - 320 WEB"},
		},
		{ // rule change / stale value on non-anime -> cleared
			ID: "d", ContentType: tv,
			Torrent:              model.Torrent{Name: "Plain.Show.S01E05.1080p"},
			AnimeAbsoluteEpisode: model.NewNullUint(5),
		},
		{ // anime but resolution after dash -> rejected by detector -> no stamp
			ID: "e", ContentType: tv,
			Torrent: model.Torrent{Name: "[SubsPlease] Movie - 1080 [Dual Audio]"},
		},
	}

	got := absoluteChanges(rows)
	assert.Equal(t, []absoluteChange{
		{id: "a", val: model.NewNullUint(1071)},
		{id: "d", val: model.NullUint{}},
	}, got)
}

// TestAbsoluteChangesReviewClasses locks the adversarial-review gates: batch
// ranges never stamp their start, circular unlisted-group names never stamp,
// and an unlisted group WITH an independent anime marker still stamps.
func TestAbsoluteChangesReviewClasses(t *testing.T) {
	t.Parallel()

	tv := model.NewNullContentType(model.ContentTypeTvShow)
	rows := []*model.TorrentContent{
		{ID: "batch", ContentType: tv,
			Torrent: model.Torrent{Name: "[Erai-raws] Show - 01 ~ 12 [1080p][Batch]"}},
		{ID: "batch2", ContentType: tv,
			Torrent: model.Torrent{Name: "[SubsPlease] Show - 01 - 24 [Batch]"}},
		{ID: "special", ContentType: tv,
			Torrent: model.Torrent{Name: "[SubsPlease] Show - 05.5 (1080p)"}},
		{ID: "circular", ContentType: tv,
			Torrent: model.Torrent{Name: "[Brazzers] Scene Name - 12"}},
		{ID: "circular2", ContentType: tv,
			Torrent: model.Torrent{Name: "[Multi] Thing - 12"}},
		{ID: "unlisted-ok", ContentType: tv,
			Torrent: model.Torrent{Name: "[NewSubGroup] Show - 12 [Eng Sub]"}},
	}

	got := absoluteChanges(rows)
	assert.Equal(t, []absoluteChange{{id: "unlisted-ok", val: model.NewNullUint(12)}}, got)
}

func TestAnimeFlagChanges(t *testing.T) {
	t.Parallel()

	tv := model.NewNullContentType(model.ContentTypeTvShow)
	rows := []*model.TorrentContent{
		{ID: "stale-false", ContentType: tv, IsAnime: false,
			Torrent: model.Torrent{Name: "[SubsPlease] One Piece - 1071 (1080p)"}},
		{ID: "stale-true", ContentType: tv, IsAnime: true,
			Torrent: model.Torrent{Name: "Plain.Show.S01E05.1080p"}},
		{ID: "correct", ContentType: tv, IsAnime: true,
			Torrent: model.Torrent{Name: "[Erai-raws] Frieren - 28 [1080p]"}},
	}
	toTrue, toFalse := animeFlagChanges(rows)
	assert.Equal(t, []string{"stale-false"}, toTrue)
	assert.Equal(t, []string{"stale-true"}, toFalse)
}

func TestEnglishAudioChanges(t *testing.T) {
	t.Parallel()

	tv := model.NewNullContentType(model.ContentTypeTvShow)
	rows := []*model.TorrentContent{
		{ID: "dub", ContentType: tv,
			Torrent: model.Torrent{Name: "[Judas] Show - 12 [Dual-Audio][1080p]"}},
		{ID: "sub", ContentType: tv,
			Torrent: model.Torrent{Name: "[SubsPlease] Show - 09 (1080p) [Multi-Subs]"}},
		{ID: "correct", ContentType: tv,
			Torrent:            model.Torrent{Name: "[Judas] Show - 12 [Dual-Audio]"},
			EnglishAudio:       model.NewNullEnglishAudio(model.EnglishAudioDub),
			EnglishAudioSource: model.NewNullString(model.EnglishAudioSourceName)},
		{ID: "stale-clear", ContentType: tv,
			Torrent:            model.Torrent{Name: "Plain.Show.S01E05.1080p"},
			EnglishAudio:       model.NewNullEnglishAudio(model.EnglishAudioSub),
			EnglishAudioSource: model.NewNullString(model.EnglishAudioSourceName)},
		{ID: "non-anime-dual", ContentType: tv,
			Torrent: model.Torrent{Name: "Some.Movie.2022.Dual.Audio.1080p"}},
	}

	nameSrc := model.NewNullString(model.EnglishAudioSourceName)
	got := englishAudioChanges(rows)
	assert.Equal(t, []englishAudioChange{
		{id: "dub", val: model.NewNullEnglishAudio(model.EnglishAudioDub), src: nameSrc},
		{id: "sub", val: model.NewNullEnglishAudio(model.EnglishAudioSub), src: nameSrc},
		{id: "stale-clear", val: model.NullEnglishAudio{}, src: model.NullString{}},
	}, got)
}

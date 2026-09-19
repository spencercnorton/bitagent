package granularitybackfillcmd

import (
	"testing"

	"github.com/spencercnorton/bitagent/internal/model"
	"github.com/stretchr/testify/assert"
)

// englishAudioChanges must keep its full clearing/self-correction behaviour
// for 'name'-sourced (and legacy NULL-source) rows while NEVER downgrading an
// 'llm'-sourced row without a fresh deterministic signal — the backfill has
// no LLM access, so 'llm' values can only be preserved or outranked, not
// recomputed.
func TestEnglishAudioChanges_SourcePrecedence(t *testing.T) {
	t.Parallel()

	name := model.NewNullString(model.EnglishAudioSourceName)
	llm := model.NewNullString(model.EnglishAudioSourceLLM)

	rows := []*model.TorrentContent{
		{ // fresh deterministic signal, empty row -> stamped (value+source)
			ID:      "a",
			Torrent: model.Torrent{Name: "[Anime Time] Naruto Dual Audio 1080p"},
		},
		{ // llm row, no deterministic signal -> UNTOUCHED (the tripwire case)
			ID:                 "b",
			Torrent:            model.Torrent{Name: "[Ohys-Raws] Sousou no Frieren - 12 (1080p)"},
			EnglishAudio:       model.NewNullEnglishAudio(model.EnglishAudioNone),
			EnglishAudioSource: llm,
		},
		{ // llm row, fresh deterministic signal -> overwritten ('name' outranks)
			ID:                 "c",
			Torrent:            model.Torrent{Name: "[Anime Time] Bleach Dual Audio 1080p"},
			EnglishAudio:       model.NewNullEnglishAudio(model.EnglishAudioNone),
			EnglishAudioSource: llm,
		},
		{ // name row whose signal is gone (non-anime) -> cleared, as before
			ID:                 "d",
			Torrent:            model.Torrent{Name: "Plain.Show.S01E05.1080p"},
			EnglishAudio:       model.NewNullEnglishAudio(model.EnglishAudioDub),
			EnglishAudioSource: name,
		},
		{ // legacy pre-00044 row missed by the stamp (value, NULL source) -> source converges to 'name'
			ID:           "e",
			Torrent:      model.Torrent{Name: "[Anime Time] One Piece Dual Audio 1080p"},
			EnglishAudio: model.NewNullEnglishAudio(model.EnglishAudioDub),
		},
		{ // correct name row -> untouched
			ID:                 "f",
			Torrent:            model.Torrent{Name: "[Anime Time] Frieren Dual Audio 1080p"},
			EnglishAudio:       model.NewNullEnglishAudio(model.EnglishAudioDub),
			EnglishAudioSource: name,
		},
		{ // empty row, no signal from anywhere -> untouched
			ID:      "g",
			Torrent: model.Torrent{Name: "Plain.Show.S02E01.1080p"},
		},
	}

	got := englishAudioChanges(rows)
	assert.Equal(t, []englishAudioChange{
		{id: "a", val: model.NewNullEnglishAudio(model.EnglishAudioDub), src: name},
		{id: "c", val: model.NewNullEnglishAudio(model.EnglishAudioDub), src: name},
		{id: "d", val: model.NullEnglishAudio{}, src: model.NullString{}},
		{id: "e", val: model.NewNullEnglishAudio(model.EnglishAudioDub), src: name},
	}, got)
}

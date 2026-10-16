package processor

import (
	"github.com/spencercnorton/bitagent/internal/classifier/classification"
	"github.com/spencercnorton/bitagent/internal/model"
	"github.com/stretchr/testify/require"
	"testing"
)

func TestNewTorrentContentEnglishAudioUsesOnlyReleaseEvidence(t *testing.T) {
	for _, name := range []string{"[Ohys-Raws] Sousou no Frieren - 12 (1080p)", "[SomeGroup] Show Title - 05 (1080p)", "The Wire S01E01 1080p BluRay x264-GROUP"} {
		got := newTorrentContent(model.Torrent{Name: name}, classification.Result{})
		require.False(t, got.EnglishAudio.Valid, name)
		require.False(t, got.EnglishAudioSource.Valid, name)
	}
	got := newTorrentContent(model.Torrent{Name: "[Anime Time] Naruto Dual Audio [ENG+JPN] 1080p"}, classification.Result{})
	require.Equal(t, model.NewNullEnglishAudio(model.EnglishAudioDub), got.EnglishAudio)
	require.Equal(t, model.NewNullString(model.EnglishAudioSourceName), got.EnglishAudioSource)
}

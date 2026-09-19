package processor

import (
	"testing"

	"github.com/spencercnorton/bitagent/internal/classifier/classification"
	"github.com/spencercnorton/bitagent/internal/model"
	"github.com/stretchr/testify/assert"
)

// newTorrentContent must persist the full deterministic anime signal from the
// release name into the is_anime flag, so the server-side cat=5070 filter and
// the Torznab 5070 emission read a stored value instead of re-detecting.
func TestNewTorrentContent_SetsIsAnime(t *testing.T) {
	cases := []struct {
		name  string
		want  bool
		notes string
	}{
		{
			name:  "[SubsPlease] Sousou no Frieren - 12 (1080p) [ABCD].mkv",
			want:  true,
			notes: "leading known fansub bracket",
		},
		{
			// The recall case the old leading-bracket prefix-LIKE filter
			// missed: the fansub bracket is NOT leading.
			name:  "Kimi no Na wa - 137 (1080p) [Erai-raws]",
			want:  true,
			notes: "known fansub bracket not leading",
		},
		{
			name:  "The Wire S01E01 1080p BluRay x264-GROUP",
			want:  false,
			notes: "ordinary western TV",
		},
	}

	for _, tc := range cases {
		got := newTorrentContent(model.Torrent{Name: tc.name}, classification.Result{}, model.NullEnglishAudio{})
		assert.Equalf(t, tc.want, got.IsAnime, "%s (%s)", tc.name, tc.notes)
	}
}

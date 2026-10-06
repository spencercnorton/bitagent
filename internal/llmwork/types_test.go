package llmwork

import (
	"math"
	"testing"
	"time"

	"github.com/spencercnorton/bitagent/internal/model"
	"github.com/spencercnorton/bitagent/internal/protocol"
	"github.com/stretchr/testify/require"
)

func draftFor(n byte) Draft {
	h := make([]byte, 20)
	h[0] = n
	return Draft{Kind: Type, InfoHash: h, SourceDigest: Digest("source"), PolicyDigest: Digest("policy"), InputDigest: Digest("input"), FamilyDigest: Digest("family"), Payload: []byte(`{"case":"synthetic"}`), DailyLimit: 15, MonthlyLimit: 450}
}

func TestTaskIdentityAndBounds(t *testing.T) {
	d := draftFor(1)
	k, err := d.Key()
	require.NoError(t, err)
	again, err := d.Key()
	require.NoError(t, err)
	require.Equal(t, k, again)
	for _, field := range []string{"source", "policy", "input", "hash", "kind"} {
		x := d
		switch field {
		case "source":
			x.SourceDigest = Digest("new")
		case "policy":
			x.PolicyDigest = Digest("new")
		case "input":
			x.InputDigest = Digest("new")
		case "hash":
			x.InfoHash = append([]byte(nil), d.InfoHash...)
			x.InfoHash[1] = 2
		case "kind":
			x.Kind = Matcher
		}
		other, err := x.Key()
		require.NoError(t, err)
		require.NotEqual(t, k, other)
	}
	d.InputDigest = Digest(math.NaN())
	_, err = d.Key()
	require.Error(t, err)
	d = draftFor(1)
	d.Payload = []byte(`[]`)
	_, err = d.Key()
	require.Error(t, err)
}

func TestSourceDigestTracksEvidenceNotSwarmRefresh(t *testing.T) {
	torrent := model.Torrent{InfoHash: protocol.ID{1}, Name: "Example.Movie.2024.mkv", Size: 100, Files: []model.TorrentFile{{Index: 1, Path: "movie.eng.srt"}, {Index: 0, Path: "movie.mkv"}}}
	first := SourceDigest(torrent)
	reordered := torrent
	reordered.Files = []model.TorrentFile{torrent.Files[1], torrent.Files[0]}
	require.Equal(t, first, SourceDigest(reordered))
	changed := torrent
	changed.Name = "Another.Movie.2024.mkv"
	require.NotEqual(t, first, SourceDigest(changed))
	changed = torrent
	changed.Private = true
	require.NotEqual(t, first, SourceDigest(changed))
	changed = torrent
	changed.Files = append([]model.TorrentFile(nil), torrent.Files...)
	changed.Files[0].Path = "movie.fra.srt"
	require.NotEqual(t, first, SourceDigest(changed))
}

func TestConfigurationIsInertAndBounded(t *testing.T) {
	c := NewDefaultConfig()
	require.False(t, c.Enabled)
	require.NoError(t, c.Validate())
	c.Enabled = true
	require.NoError(t, c.Validate())
	c.TaskTimeout = c.LeaseDuration
	require.Error(t, c.Validate())
	c = NewDefaultConfig()
	c.Enabled = true
	c.Kinds = []Kind{Type, Type}
	require.Error(t, c.Validate())
}

func TestSourceDigestExcludesHintObservationTimes(t *testing.T) {
	t0 := model.Torrent{InfoHash: protocol.ID{1}, Name: "Example.Movie.2026.mkv", Hint: model.TorrentHint{ContentType: model.ContentTypeMovie, Title: model.NewNullString("Example Movie"), CreatedAt: time.Unix(1, 0), UpdatedAt: time.Unix(2, 0)}}
	t1 := t0
	t1.Hint.CreatedAt = time.Unix(3, 0)
	t1.Hint.UpdatedAt = time.Unix(4, 0)
	require.Equal(t, SourceDigest(t0), SourceDigest(t1))
	t1.Hint.Title = model.NewNullString("Another Movie")
	require.NotEqual(t, SourceDigest(t0), SourceDigest(t1))
}

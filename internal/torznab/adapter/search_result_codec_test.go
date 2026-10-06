package adapter

import (
	"testing"

	"github.com/spencercnorton/bitagent/internal/database/search"
	"github.com/spencercnorton/bitagent/internal/model"
	"github.com/spencercnorton/bitagent/internal/torznab"
	"github.com/stretchr/testify/require"
)

func TestTorznabSerializesCodecAndGroupClaimsWithoutChangingReleaseName(t *testing.T) {
	for _, codec := range []model.VideoCodec{model.VideoCodecX265, model.VideoCodecHEVC, model.VideoCodecAV1} {
		t.Run(codec.String(), func(t *testing.T) {
			name := "Amber.Signal.2025.1080p." + codec.String() + "-GROUP.mkv"
			item := torrentContentResultItemToTorznabResultItem(search.TorrentContentResultItem{TorrentContent: model.TorrentContent{
				Torrent: model.Torrent{Name: name}, ContentType: model.NewNullContentType(model.ContentTypeMovie),
				VideoCodec: model.NewNullVideoCodec(codec), ReleaseGroup: model.NewNullString("GROUP"),
			}}, false)
			require.Equal(t, name, item.Title)
			attrs := make(map[string]string)
			for _, attr := range item.TorznabAttrs {
				attrs[attr.AttrName] = attr.AttrValue
			}
			require.Equal(t, codec.String(), attrs[torznab.AttrVideo])
			require.Equal(t, "GROUP", attrs[torznab.AttrTeam])
		})
	}
}

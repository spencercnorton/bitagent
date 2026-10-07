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

func TestTorznabEmitsOnlyExplicitClaimExtensions(t *testing.T) {
	claims := model.InferReleaseAttributes("source", "HEVC.HDR10.DV.DDP5.1.Atmos.PROPER.REPACK")
	item := torrentContentResultItemToTorznabResultItem(search.TorrentContentResultItem{TorrentContent: model.TorrentContent{ReleaseAttributes: claims}}, false)
	attrs := map[string]string{}
	for _, attr := range item.TorznabAttrs {
		attrs[attr.AttrName] = attr.AttrValue
	}
	require.Equal(t, "DOLBY_VISION,HDR10", attrs[torznab.AttrClaimedHDR])
	require.Equal(t, "EAC3", attrs[torznab.AttrClaimedAudio])
	require.Equal(t, "5.1", attrs[torznab.AttrClaimedAudioChannels])
	require.Equal(t, "ATMOS", attrs[torznab.AttrClaimedAudioFeatures])
	require.Equal(t, "PROPER,REPACK", attrs[torznab.AttrClaimedRevision])
	require.NotContains(t, attrs, torznab.AttrClaimedEncoder)
}

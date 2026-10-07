package search

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestReleaseAttributeCriteriaBindsValuesAndRejectsUnknownFields(t *testing.T) {
	raw, err := TorrentContentReleaseAttributeCriteria("hdrFormats", "HDR10").Raw(nil)
	require.NoError(t, err)
	require.Equal(t, "torrent_contents.release_attributes @> ?::jsonb", raw.Query)
	require.Len(t, raw.Args, 1)
	require.JSONEq(t, `{"version":1,"hdrFormats":["HDR10"]}`, raw.Args[0].(string))
	for _, field := range []string{"unknown", "audioFormats'};DROP TABLE torrents;--"} {
		_, err := TorrentContentReleaseAttributeCriteria(field, "HDR10").Raw(nil)
		require.Error(t, err)
	}
	_, err = TorrentContentReleaseAttributeCriteria("encoder", "HEVC").Raw(nil)
	require.Error(t, err, "HEVC is not an advertised encoder")
}

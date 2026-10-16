package metainfo_test

import (
	"bytes"
	"crypto/sha1"
	"os"
	"testing"

	"github.com/anacrolix/torrent/bencode"
	"github.com/spencercnorton/bitagent/internal/protocol"
	"github.com/spencercnorton/bitagent/internal/protocol/metainfo"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestReadTorrentFile(t *testing.T) {
	t.Parallel()

	input, readErr := os.ReadFile("examples/ubuntu-23.04-desktop-amd64.iso.torrent")
	if readErr != nil {
		t.Fatalf("error reading torrent file: %s", readErr)
	}

	torrentFile, parseErr := metainfo.ReadTorrentFileBytes(input)
	if parseErr != nil {
		t.Fatalf("error parsing torrent file: %s", parseErr)
	}

	assert.NotEmpty(t, torrentFile.Info.Pieces)
	torrentFile.Info.Pieces = nil
	assert.Equal(t, metainfo.TorrentFile{
		Info: metainfo.Info{
			Name:        "ubuntu-23.04-desktop-amd64.iso",
			PieceLength: 262144,
			Length:      4932407296,
		},
		Announce: "https://torrent.ubuntu.com/announce",
		AnnounceList: [][]string{
			{"https://torrent.ubuntu.com/announce"},
			{"https://ipv6.torrent.ubuntu.com/announce"},
		},
		CreationDate: 1681992794,
		Comment:      "Ubuntu CD releases.ubuntu.com",
		CreatedBy:    "mktorrent 1.1",
	}, torrentFile)
}

// syntheticTorrent returns a single-file .torrent for content and its info hash.
func syntheticTorrent(t *testing.T, content []byte, pieceLength int) ([]byte, protocol.ID) {
	t.Helper()
	var pieces []byte
	for off := 0; off < len(content); off += pieceLength {
		sum := sha1.Sum(content[off:min(off+pieceLength, len(content))])
		pieces = append(pieces, sum[:]...)
	}
	info, err := bencode.Marshal(map[string]any{
		"name": "Synthetic.Release.2026.mkv", "length": len(content), "piece length": pieceLength, "pieces": pieces,
	})
	require.NoError(t, err)
	torrent, err := bencode.Marshal(map[string]any{"announce": "udp://tracker.invalid:6969", "info": bencode.Bytes(info)})
	require.NoError(t, err)
	return torrent, protocol.ID(sha1.Sum(info))
}

func TestParseTorrentFileInfoBindsPiecesToInfoHash(t *testing.T) {
	t.Parallel()

	content := bytes.Repeat([]byte("synthetic release bytes "), 2000) // 48,000 bytes: 2 whole pieces and a tail
	torrent, hash := syntheticTorrent(t, content, 16384)

	info, err := metainfo.ParseTorrentFileInfo(hash, torrent)
	require.NoError(t, err)
	assert.Equal(t, int64(16384), info.PieceLength)
	require.Len(t, info.Pieces, 3*20)
	for i := range 3 {
		piece := sha1.Sum(content[i*16384 : min((i+1)*16384, len(content))])
		assert.Equal(t, piece[:], info.Pieces[i*20:(i+1)*20], "piece %d", i)
	}

	other := hash
	other[0] ^= 0xff
	_, err = metainfo.ParseTorrentFileInfo(other, torrent)
	require.Error(t, err, "info bytes must hash to the expected info hash")

	_, err = metainfo.ParseTorrentFileInfo(hash, torrent[:len(torrent)/2])
	require.Error(t, err, "a truncated .torrent must not parse")

	noInfo, err := bencode.Marshal(map[string]any{"announce": "udp://tracker.invalid:6969"})
	require.NoError(t, err)
	_, err = metainfo.ParseTorrentFileInfo(hash, noInfo)
	require.Error(t, err, "a .torrent without an info dictionary must not parse")
}

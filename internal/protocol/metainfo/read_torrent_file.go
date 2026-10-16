package metainfo

import (
	"fmt"

	"github.com/anacrolix/torrent/bencode"
	"github.com/spencercnorton/bitagent/internal/protocol"
)

type TorrentFile struct {
	Info Info `bencode:"info"`

	Announce     string      `bencode:"announce"`
	AnnounceList [][]string  `bencode:"announce-list,omitempty"`
	CreationDate int64       `bencode:"creation date,omitempty"`
	Comment      string      `bencode:"comment,omitempty"`
	CreatedBy    string      `bencode:"created by,omitempty"`
	URLList      interface{} `bencode:"url-list,omitempty"`
}

func ReadTorrentFileBytes(bytes []byte) (TorrentFile, error) {
	var torrentFile TorrentFile
	if unmarshalErr := bencode.Unmarshal(bytes, &torrentFile); unmarshalErr != nil {
		return TorrentFile{}, fmt.Errorf("error unmarshaling torrent file: %w", unmarshalErr)
	}

	return torrentFile, nil
}

// ParseTorrentFileInfo returns the info dictionary of a .torrent file after
// the same checks as BEP-9 metadata, including SHA-1(info) == infoHash, so the
// result is bound to that info hash.
func ParseTorrentFileInfo(infoHash protocol.ID, torrentFile []byte) (Info, error) {
	var raw struct {
		Info bencode.Bytes `bencode:"info"`
	}
	if unmarshalErr := bencode.Unmarshal(torrentFile, &raw); unmarshalErr != nil {
		return Info{}, fmt.Errorf("error unmarshaling torrent file: %w", unmarshalErr)
	}

	return ParseMetaInfoBytes(infoHash, raw.Info)
}

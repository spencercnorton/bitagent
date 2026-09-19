package metainfo

import (
	"errors"
	"fmt"

	"github.com/anacrolix/torrent/bencode"
	mi "github.com/anacrolix/torrent/metainfo"
	"github.com/spencercnorton/bitagent/internal/protocol"
)

// ParseMetaInfoBytes parses bencoded `info` bytes received from a peer
// (BEP-9) or a .torrent file. Hardening order:
//
//  1. Size cap (defence in depth — the BEP-9 receive layer caps too,
//     but the parser is the security boundary and must not trust callers).
//  2. SHA-1 hash check against the expected infohash. Done BEFORE
//     unmarshal so we never spend bencode parsing on bytes that don't
//     match what we asked for.
//  3. bencode.Unmarshal into Info.
//  4. Post-unmarshal validation: UTF-8, NUL-byte, path-traversal, length
//     and depth bounds on Name + Files[i].Path. Hostile peers can craft
//     metainfo with invalid UTF-8 or null-byte truncation that would
//     break Postgres inserts (SQLSTATE 22021) or downstream filesystem ops.
func ParseMetaInfoBytes(infoHash protocol.ID, metaInfoBytes []byte) (Info, error) {
	if len(metaInfoBytes) > MaxMetaInfoBytes {
		return Info{}, fmt.Errorf("metainfo bytes (%d): %w", len(metaInfoBytes), ErrMetaInfoTooLarge)
	}

	if protocol.ID(mi.HashBytes(metaInfoBytes)) != infoHash {
		return Info{}, errors.New("info bytes have wrong hash")
	}

	var info Info
	if unmarshalErr := bencode.Unmarshal(metaInfoBytes, &info); unmarshalErr != nil {
		return Info{}, fmt.Errorf("error unmarshaling info bytes: %w", unmarshalErr)
	}

	if validateErr := validateInfo(info); validateErr != nil {
		return Info{}, validateErr
	}

	return info, nil
}

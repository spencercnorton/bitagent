package csamblocklist

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"

	"github.com/spencercnorton/bitagent/internal/protocol"
)

// DoubleHashLen is the byte length of a double-hash digest (SHA-256
// output size).
const DoubleHashLen = sha256.Size

// DoubleHash is the canonical CSAM-blocklist hash function:
// SHA-256 of the raw 20-byte SHA-1 infohash bytes. The result is
// one-way — there is no algorithmically-feasible way to recover the
// underlying SHA-1 infohash from the SHA-256 digest, which is the
// privacy property that makes a public blocklist non-useful as a
// CSAM directory.
//
// This function is the only encoding the wire format uses. Feeds
// MUST publish digests as the lowercase hex of this 32-byte output.
// Any feed using a different hash function (e.g. SHA-1 of SHA-1, or
// SHA-256 of the hex string of the infohash) will not match.
func DoubleHash(infoHash protocol.ID) [DoubleHashLen]byte {
	return sha256.Sum256(infoHash[:])
}

// DoubleHashHex returns the lowercase 64-character hex encoding of
// the double-hash. This is the on-the-wire and on-disk format.
func DoubleHashHex(infoHash protocol.ID) string {
	d := DoubleHash(infoHash)
	return hex.EncodeToString(d[:])
}

// ParseDoubleHashHex decodes a 64-character lowercase hex string into
// a digest. Whitespace is trimmed; empty strings and any non-64-char
// non-hex inputs are rejected.
//
// The strict length check exists because feeds may inadvertently
// publish SHA-1 infohashes (40 hex) or some other size; we want a
// clear error rather than silently mis-keying the bloom filter.
func ParseDoubleHashHex(s string) ([DoubleHashLen]byte, error) {
	var out [DoubleHashLen]byte
	s = strings.TrimSpace(s)
	if s == "" {
		return out, errors.New("csamblocklist: empty double-hash")
	}
	if len(s) != DoubleHashLen*2 {
		return out, errors.New("csamblocklist: double-hash must be exactly 64 lowercase hex chars (SHA-256)")
	}
	// Reject uppercase to keep the wire format canonical — easier
	// matching on dedup, easier sanity-checks on operator-managed
	// feeds.
	if strings.ToLower(s) != s {
		return out, errors.New("csamblocklist: double-hash must be lowercase hex")
	}
	b, err := hex.DecodeString(s)
	if err != nil {
		return out, errors.New("csamblocklist: invalid hex in double-hash")
	}
	copy(out[:], b)
	return out, nil
}

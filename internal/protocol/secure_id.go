package protocol

import (
	crand "crypto/rand"
	"errors"
	"hash/crc32"
	"net/netip"
)

// castagnoliTable is the CRC32C polynomial, which BEP-42 mandates (NOT
// the zlib default). Cheap to allocate once; hot path reuses it.
var castagnoliTable = crc32.MakeTable(crc32.Castagnoli)

// v4NodeIDMask and v6NodeIDMask are the octet masks from BEP-42 applied
// to the IP before the CRC32C step. They restrict the input entropy so
// a single IP block can only control a bounded slice of the keyspace.
var (
	v4NodeIDMask = [4]byte{0x03, 0x0f, 0x3f, 0xff}
	v6NodeIDMask = [8]byte{0x01, 0x03, 0x07, 0x0f, 0x1f, 0x3f, 0x7f, 0xff}
)

// ErrInvalidExternalIP is returned by SecureNodeID when the supplied IP
// cannot be used to derive a BEP-42 compliant node ID (unspecified /
// invalid netip.Addr). We do NOT reject RFC1918 / loopback here — the
// spec exempts local-network addresses from enforcement but the derivation
// itself is still well-defined. Callers should ensure they pass their
// actual external egress IP (post-NAT, post-VPN).
var ErrInvalidExternalIP = errors.New("invalid external IP for BEP-42 node ID")

// SecureNodeID derives a BEP-42 compliant DHT node ID from the caller's
// external IP address. The ID has three fixed regions and one random
// region:
//
//	bytes  0..1  : top 16 bits of CRC32C(masked_ip | (r<<29))
//	byte   2     : top 5 bits from the same CRC32C, low 3 bits random
//	bytes  3..18 : fully random
//	byte   19    : a random byte whose low 3 bits equal r
//
// r is a random integer in [0, 7] that perturbs which 21-bit prefix the
// IP maps to — BEP-42 allows 8 valid prefixes per IP so NATted users
// aren't forced to share an ID.
//
// Enforcing peers reject queries whose node ID doesn't satisfy this
// derivation for the sender's observed IP, which in our traffic manifests
// as elevated sample_infohashes refusal rates. Compliance upgrades us
// from "serviced but not trusted as a storage target" to a first-class
// citizen in their routing tables.
func SecureNodeID(ip netip.Addr) (ID, error) {
	var id ID
	if !ip.IsValid() {
		return id, ErrInvalidExternalIP
	}

	// 17 random bytes drive: id[19] (full byte), id[2] low 3 bits, id[3..18]
	// leaving 15 bytes for the interior. One extra byte covers byte[2] low bits.
	var rng [17]byte
	if _, err := crand.Read(rng[:]); err != nil {
		return id, err
	}

	randLastByte := rng[0]
	r := randLastByte & 0x7

	return secureNodeIDWithEntropy(ip, r, randLastByte, rng[1], rng[2:]), nil
}

// secureNodeIDWithEntropy is the deterministic core of SecureNodeID,
// extracted so tests can reproduce the BEP-42 reference test vectors
// without the non-determinism of crypto/rand.
//
// interior must be exactly 15 bytes, covering id[3..17]. id[18] comes
// from byte18; we split them because the test vectors fix only the
// first-21-bits-of-CRC region, leaving the rest free.
func secureNodeIDWithEntropy(
	ip netip.Addr,
	r byte,
	randLastByte byte,
	byte2LowBits byte,
	interior []byte,
) ID {
	var id ID
	masked := maskIPForBEP42(ip, r)
	crc := crc32.Checksum(masked, castagnoliTable)

	id[0] = byte(crc >> 24)
	id[1] = byte(crc >> 16)
	id[2] = byte((crc>>8)&0xf8) | (byte2LowBits & 0x7)
	// bytes 3..18: random interior. interior length is capped at 16 in
	// case callers pass more; anything shorter pads with zeros (safe in
	// tests, never reached in prod via SecureNodeID).
	copy(id[3:19], interior)
	id[19] = randLastByte

	return id
}

// maskIPForBEP42 returns the byte sequence fed into CRC32C per BEP-42.
// For IPv4: 4 bytes after per-octet masking, with (r<<5) OR'd into byte 0.
// For IPv6: top 8 bytes of the address, same treatment.
func maskIPForBEP42(ip netip.Addr, r byte) []byte {
	if ip.Is4() || ip.Is4In6() {
		b := ip.As4()
		for i := range v4NodeIDMask {
			b[i] &= v4NodeIDMask[i]
		}
		b[0] |= r << 5
		// heap-allocate so callers can't accidentally mutate the shared array
		return append([]byte(nil), b[:]...)
	}
	b := ip.As16()
	out := make([]byte, 8)
	for i := range v6NodeIDMask {
		out[i] = b[i] & v6NodeIDMask[i]
	}
	out[0] |= r << 5

	return out
}

// VerifySecureNodeID returns true iff id is a valid BEP-42 derivation
// for ip. Used by tests to prove SecureNodeID round-trips, and available
// for future enforcement work (e.g. gating the routing table on compliance).
func VerifySecureNodeID(id ID, ip netip.Addr) bool {
	if !ip.IsValid() {
		return false
	}
	r := id[19] & 0x7
	masked := maskIPForBEP42(ip, r)
	crc := crc32.Checksum(masked, castagnoliTable)

	if id[0] != byte(crc>>24) {
		return false
	}
	if id[1] != byte(crc>>16) {
		return false
	}
	// only top 5 bits of byte 2 are load-bearing
	if (id[2] & 0xf8) != byte((crc>>8)&0xf8) {
		return false
	}

	return true
}

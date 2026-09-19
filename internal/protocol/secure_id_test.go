package protocol

import (
	"encoding/hex"
	"net/netip"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// bep42Vectors are the reference test vectors from
// https://www.bittorrent.org/beps/bep_0042.html §"Node ID restriction".
//
// The spec publishes the node ID as 20 hex bytes where the first 21 bits
// and the last byte are fixed by the (ip, randByte) inputs and the rest
// is random. We reproduce the published IDs verbatim in expectNodeID
// and feed the exact `randByte` + interior bytes derived from those
// hex values back into secureNodeIDWithEntropy, which should yield
// byte-for-byte the same ID.
var bep42Vectors = []struct {
	ip         string
	randByte   byte
	expectHex  string
	expectPfx  string // first 5 hex nibbles + "8" since low 3 bits of byte 2 are random but the spec shows them set here
	expectLast byte
}{
	{ip: "124.31.75.21", randByte: 1, expectHex: "5fbfbff10c5d6a4ec8a88e4c6ab4c28b95eee401", expectLast: 0x01},
	{ip: "21.75.31.124", randByte: 86, expectHex: "5a3ce9c14e7a08645677bbd1cfe7d8f956d53256", expectLast: 0x56},
	{ip: "65.23.51.170", randByte: 22, expectHex: "a5d43220bc8f112a3d426c84764f8c2a1150e616", expectLast: 0x16},
	{ip: "84.124.73.14", randByte: 65, expectHex: "1b0321dd1bb1fe518101ceef99462b947a01ff41", expectLast: 0x41},
	{ip: "43.213.53.83", randByte: 90, expectHex: "e56f6cbf5b7c4be0237986d5243b87aa6d51305a", expectLast: 0x5a},
}

func TestSecureNodeID_bep42TestVectors_firstBitsAndLastByteMatch(t *testing.T) {
	t.Parallel()

	for _, v := range bep42Vectors {
		v := v
		t.Run(v.ip, func(t *testing.T) {
			t.Parallel()

			ip, err := netip.ParseAddr(v.ip)
			require.NoError(t, err)

			expect, err := hex.DecodeString(v.expectHex)
			require.NoError(t, err)
			require.Len(t, expect, 20)

			// Reproduce the test vector exactly: feed the published
			// interior bytes back in.
			r := v.randByte & 0x7
			var interior [16]byte
			copy(interior[:], expect[3:19])
			byte2Low := expect[2] & 0x07

			id := secureNodeIDWithEntropy(ip, r, v.randByte, byte2Low, interior[:])

			assert.Equal(t, expect, id[:],
				"BEP-42 vector: id should match the published test vector byte-for-byte")

			// Sanity: the verifier accepts the same id for the same IP.
			assert.True(t, VerifySecureNodeID(id, ip),
				"vector must self-verify")

			// And the last-byte invariant (id[19] ≡ randByte) holds.
			assert.Equal(t, v.expectLast, id[19])
		})
	}
}

func TestSecureNodeID_verifyRejectsWrongIP(t *testing.T) {
	t.Parallel()

	ipA := netip.MustParseAddr("124.31.75.21")
	ipB := netip.MustParseAddr("21.75.31.124")

	id, err := SecureNodeID(ipA)
	require.NoError(t, err)

	assert.True(t, VerifySecureNodeID(id, ipA), "should verify under original IP")
	// CRC32C collisions on 21 bits for two fixed IPs across 8 possible
	// r-values are possible but astronomically unlikely in a unit test.
	// This asserts the expected real-world behaviour.
	assert.False(t, VerifySecureNodeID(id, ipB), "should not verify under different IP")
}

func TestSecureNodeID_roundTripForManyIPs(t *testing.T) {
	t.Parallel()

	// A handful of real-world-shaped IPs covering class boundaries.
	ips := []string{
		"8.8.8.8",
		"1.1.1.1",
		"100.64.0.1",           // a CGNAT address (Tailscale-style)
		"173.194.71.100",       // public IPv4
		"192.0.2.1",            // TEST-NET-1
		"198.51.100.42",        // TEST-NET-2
		"2001:4860:4860::8888", // Google DNS v6
		"2606:4700:4700::1111", // Cloudflare DNS v6
	}

	for _, s := range ips {
		s := s
		t.Run(s, func(t *testing.T) {
			t.Parallel()

			ip := netip.MustParseAddr(s)
			id, err := SecureNodeID(ip)
			require.NoError(t, err)
			assert.True(t, VerifySecureNodeID(id, ip),
				"SecureNodeID(%s) must round-trip through VerifySecureNodeID", s)
		})
	}
}

func TestSecureNodeID_invalidIP(t *testing.T) {
	t.Parallel()

	var zero netip.Addr
	_, err := SecureNodeID(zero)
	assert.ErrorIs(t, err, ErrInvalidExternalIP)

	assert.False(t, VerifySecureNodeID(ID{}, zero))
}

func TestSecureNodeID_distinctRsProduceDistinctIDs(t *testing.T) {
	t.Parallel()

	// Same IP, different r, should produce IDs with differing top-21-bit
	// prefixes almost always. BEP-42 gives us 8 r-values → 8 valid prefixes,
	// so each call to SecureNodeID picks one at random.
	ip := netip.MustParseAddr("173.194.71.100")

	seen := make(map[[3]byte]struct{})
	for i := 0; i < 64; i++ {
		id, err := SecureNodeID(ip)
		require.NoError(t, err)
		require.True(t, VerifySecureNodeID(id, ip))

		var pfx [3]byte
		pfx[0] = id[0]
		pfx[1] = id[1]
		pfx[2] = id[2] & 0xf8

		seen[pfx] = struct{}{}
	}
	// With 8 distinct valid prefixes and 64 draws we expect to see at
	// least a few of them. Probability of seeing exactly 1 is
	// 8 × (1/8)^64 ≈ 10^-56 — safely negligible.
	assert.GreaterOrEqual(t, len(seen), 2,
		"64 draws from 8 prefixes should hit at least 2 distinct ones")
}

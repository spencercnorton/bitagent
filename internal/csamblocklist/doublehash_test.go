package csamblocklist

import (
	"strings"
	"testing"

	"github.com/spencercnorton/bitagent/internal/protocol"
)

func TestDoubleHash_KnownVector(t *testing.T) {
	// 20 zero bytes — well-defined SHA-256 output.
	var zeroHash protocol.ID
	got := DoubleHashHex(zeroHash)
	want := "de47c9b27eb8d300dbb5f2c353e632c393262cf06340c4fa7f1b40c4cbd36f90"
	if got != want {
		t.Fatalf("DoubleHashHex(zero infohash) = %q, want %q", got, want)
	}

	if l := len(got); l != 64 {
		t.Fatalf("DoubleHashHex length = %d, want 64", l)
	}
}

func TestDoubleHash_DifferentInputsDifferOutputs(t *testing.T) {
	var a, b protocol.ID
	for i := range a {
		a[i] = byte(i)
		b[i] = byte(i + 1)
	}
	if DoubleHashHex(a) == DoubleHashHex(b) {
		t.Fatal("expected different infohashes to produce different double-hashes")
	}
}

func TestParseDoubleHashHex_Roundtrip(t *testing.T) {
	var ih protocol.ID
	for i := range ih {
		ih[i] = byte(i*7 + 1)
	}
	hexStr := DoubleHashHex(ih)
	parsed, err := ParseDoubleHashHex(hexStr)
	if err != nil {
		t.Fatalf("ParseDoubleHashHex error: %v", err)
	}
	if got := DoubleHash(ih); parsed != got {
		t.Fatalf("roundtrip mismatch: parsed=%x raw=%x", parsed, got)
	}
}

func TestParseDoubleHashHex_Rejects(t *testing.T) {
	cases := []struct {
		name string
		in   string
	}{
		{"empty", ""},
		{"whitespace_only", "   "},
		{"too_short", strings.Repeat("a", 40)}, // SHA-1 infohash length
		{"too_long", strings.Repeat("a", 65)},
		{"non_hex", strings.Repeat("z", 64)},
		{"uppercase", strings.Repeat("A", 64)},
		{"mixed_case", "A" + strings.Repeat("a", 63)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := ParseDoubleHashHex(tc.in); err == nil {
				t.Errorf("expected error for %q, got nil", tc.in)
			}
		})
	}
}

func TestParseDoubleHashHex_TrimsSurroundingWhitespace(t *testing.T) {
	var ih protocol.ID
	hexStr := DoubleHashHex(ih)
	padded := "  \t" + hexStr + "  \n"
	parsed, err := ParseDoubleHashHex(padded)
	if err != nil {
		t.Fatalf("ParseDoubleHashHex(trimmed) error: %v", err)
	}
	if got := DoubleHash(ih); parsed != got {
		t.Fatal("trimmed parse did not match raw")
	}
}

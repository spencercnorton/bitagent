package dhtcrawler

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// TestNextSampleInterval pins the BEP-51 compliance policy.
//
// Upstream bitmagnet clamped productive peers to 60 s regardless of
// the advertised interval. The GPT-5.4-pro review flagged that as
// spec-hostile and likely abusive: BEP-51 requires clients to honour
// whatever value in [0, 21600] seconds the peer advertises. These
// cases lock in the corrected behaviour — every path returns the
// peer's advertised interval unchanged.
//
// If a future MR wants to add any deviation (rate-limiting-by-class,
// per-peer floors, etc.), it should do so with explicit justification
// and update these cases accordingly — the point of this test is to
// make silent spec regression impossible.
func TestNextSampleInterval_honoursPeerAdvertisedInterval(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name           string
		fromPeer       int
		discoveredNew  int
		expectInterval int
	}{
		{"productive peer at 6h backoff", 21600, 12, 21600},
		{"productive peer at 10min backoff", 600, 1, 600},
		{"productive peer at threshold (300s)", 300, 5, 300},
		{"productive peer at 120s", 120, 5, 120},
		{"unproductive peer at 6h", 21600, 0, 21600},
		{"unproductive peer at 0", 0, 0, 0},
		{"productive peer at 0", 0, 3, 0},
		{"productive peer at 1h", 3600, 8, 3600},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := nextSampleInterval(tt.fromPeer, tt.discoveredNew)
			assert.Equal(t, tt.expectInterval, got,
				"the crawler must honour the peer's advertised BEP-51 interval; "+
					"any clamp here would be a spec violation")
		})
	}
}

package dhtcrawler

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/spencercnorton/bitagent/internal/protocol"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func hashOf(b byte) protocol.ID {
	var h protocol.ID
	h[0] = b
	return h
}

// removedHashes must return exactly the hashes Filter dropped (all minus
// kept), in order, and nothing when nothing was dropped.
func TestRemovedHashes(t *testing.T) {
	all := []protocol.ID{hashOf(1), hashOf(2), hashOf(3), hashOf(4)}

	got := removedHashes(all, []protocol.ID{hashOf(1), hashOf(3)})
	require.Len(t, got, 2)
	assert.Equal(t, hashOf(2), got[0])
	assert.Equal(t, hashOf(4), got[1])

	// Nothing dropped → nil (fast path, no recording).
	assert.Nil(t, removedHashes(all, all))

	// Everything dropped → all returned.
	assert.Len(t, removedHashes(all, nil), 4)
}

// csamEvidence is a redacted label — source + site only, never the hash,
// title, or matched keyword.
func TestCsamEvidence(t *testing.T) {
	var got map[string]string
	require.NoError(t, json.Unmarshal(csamEvidence("triage"), &got))
	assert.Equal(t, "community_feed", got["source"])
	assert.Equal(t, "triage", got["site"])
	assert.Len(t, got, 2, "evidence must carry no other fields (no PII)")
}

// inPlaceFilter replicates csamblocklist.service.Filter EXACTLY: it compacts
// in place (out := in[:0:len(in)]), so it mutates the caller's backing array.
// Anything blocked (first byte in `block`) is dropped.
func inPlaceFilter(in []protocol.ID, block map[byte]struct{}) []protocol.ID {
	out := in[:0:len(in)]
	for _, h := range in {
		if _, blocked := block[h[0]]; !blocked {
			out = append(out, h)
		}
	}
	return out
}

// REGRESSION: the triage site must snapshot allHashes BEFORE Filter mutates
// its backing array, or the reject diff sees the compacted (kept) hashes and
// records nothing. Guards infohash_triage.go's preFilter copy.
func TestCsamRejectDiff_SurvivesInPlaceFilter(t *testing.T) {
	block := map[byte]struct{}{1: {}, 2: {}} // rejects at the FRONT — worst case
	orig := []protocol.ID{hashOf(1), hashOf(2), hashOf(3), hashOf(4)}

	// The FIXED sequence: snapshot with a fresh backing array, then Filter.
	all := append([]protocol.ID(nil), orig...)
	preFilter := append(all[:0:0], all...)
	kept := inPlaceFilter(all, block)
	removed := removedHashes(preFilter, kept)
	assert.ElementsMatch(t, []protocol.ID{hashOf(1), hashOf(2)}, removed,
		"snapshot before Filter must recover the true reject set")

	// Sanity: diffing against the POST-Filter (mutated) slice loses the
	// rejects — proving the snapshot is load-bearing, not decorative.
	all2 := append([]protocol.ID(nil), orig...)
	kept2 := inPlaceFilter(all2, block)
	assert.Empty(t, removedHashes(all2, kept2),
		"without the snapshot the in-place compaction hides every front reject")
}

// The recording helpers are nil-safe: a crawler with no verdicts store must
// not panic (narrow wiring / ledger disabled).
func TestRecordVerdict_NilSafe(t *testing.T) {
	c := &crawler{}
	assert.NotPanics(t, func() {
		c.recordCsamRejects(context.Background(), []protocol.ID{hashOf(1)}, nil, "triage")
		c.recordCsamReject(context.Background(), hashOf(1), "egress")
		c.recordBlockingVerdict(context.Background(), hashOf(1))
	})
}

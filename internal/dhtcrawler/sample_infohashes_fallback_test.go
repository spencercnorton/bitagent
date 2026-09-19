package dhtcrawler

import (
	"context"
	"testing"
	"time"

	ktable_mocks "github.com/spencercnorton/bitagent/internal/protocol/dht/ktable/mocks"
	"github.com/stretchr/testify/assert"
	"go.uber.org/zap"
)

// TestGetNodesForSampleInfoHashes_randomFallbackBlocks pins the
// correctness condition for the BEP-42 gate: when the crawler is
// running with a random-fallback node ID, the outbound
// sample_infohashes feeder MUST NOT call into the ktable at all.
// If it did, we'd spend bandwidth on queries enforcers will refuse.
//
// The test asserts by using a strict ktable mock that would fail
// AssertExpectations if ANY method were called on it. The feeder's
// only ktable call on the happy path is
// `GetNodesForSampleInfoHashes` — if the gate works, it's never
// invoked, so the mock's zero expectations are satisfied.
func TestGetNodesForSampleInfoHashes_randomFallbackBlocks(t *testing.T) {
	t.Parallel()

	table := ktable_mocks.NewTable(t) // zero expectations set

	c := &crawler{
		kTable:         table,
		randomFallback: true,
		logger:         zap.NewNop().Sugar(),
	}

	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan struct{})
	go func() {
		c.getNodesForSampleInfoHashes(ctx)
		close(done)
	}()

	// Give the goroutine a beat to hit its ctx.Done select. If the
	// gate leaked to the main loop, it'd have called
	// GetNodesForSampleInfoHashes on the mock by now and failed
	// the test.
	time.Sleep(20 * time.Millisecond)

	cancel()

	select {
	case <-done:
		// expected: the gate unblocks on ctx.Done
	case <-time.After(200 * time.Millisecond):
		t.Fatal("getNodesForSampleInfoHashes did not return after ctx cancel")
	}

	// t.Cleanup from ktable_mocks.NewTable asserts zero expectations
	// were violated — i.e. no ktable method was called, which is
	// the whole point of the gate.
	_ = assert.True // silence unused import in the assert.assert-less path
}

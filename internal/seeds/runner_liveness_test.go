package seeds

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

type fakeRecorder struct {
	alive   [][]byte
	suspect [][]byte
}

func (f *fakeRecorder) MarkAliveBatch(_ context.Context, hashes [][]byte, _ time.Time, source string) (int64, error) {
	if source != "tracker_scrape" {
		panic("wrong source attribution: " + source)
	}
	f.alive = append(f.alive, hashes...)
	return int64(len(hashes)), nil
}

func (f *fakeRecorder) RecordSuspectBatch(_ context.Context, hashes [][]byte, _ time.Time) (int64, error) {
	f.suspect = append(f.suspect, hashes...)
	return int64(len(hashes)), nil
}

// TestLivenessPartition locks the evidence routing: tracker-positive → alive,
// tracker-zero → suspect, tracker-unknown → nothing. Exercised through the
// partition function RunBatch calls; the batch SQL itself is covered by the
// PG co-test.
func TestLivenessPartition(t *testing.T) {
	t.Parallel()

	h1, h2, h3, h4, h5 := []byte{1}, []byte{2}, []byte{3}, []byte{4}, []byte{5}
	outcomes := map[string]*ScrapeOutcome{
		string(h1): {TrackerKnown: true, Seeders: 12},
		string(h2): {TrackerKnown: true, Seeders: 0},
		string(h3): {TrackerKnown: false},
		// h4 absent — never scraped
		// h5: leecher-active reseed-in-progress — an answering swarm is
		// alive evidence, NOT an authoritative zero (Positive() boundary)
		string(h5): {TrackerKnown: true, Seeders: 0, Leechers: 7},
	}

	positives, zeros := partitionLivenessEvidence([][]byte{h1, h2, h3, h4, h5}, outcomes)

	rec := &fakeRecorder{}
	_, err := rec.MarkAliveBatch(context.Background(), positives, time.Now(), "tracker_scrape")
	assert.NoError(t, err)
	_, err = rec.RecordSuspectBatch(context.Background(), zeros, time.Now())
	assert.NoError(t, err)

	assert.Equal(t, [][]byte{h1, h5}, rec.alive)
	assert.Equal(t, [][]byte{h2}, rec.suspect)
}

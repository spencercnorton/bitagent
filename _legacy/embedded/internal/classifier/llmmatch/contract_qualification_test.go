package llmmatch

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/spencercnorton/bitagent/internal/llmcapture"
	"github.com/spencercnorton/bitagent/internal/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestContractQualificationCrossSourceAndReplay verifies that candidate source
// identity (e.g. local mirror vs TMDB API) is strictly bound into the rerank
// cache, preventing cross-source evidence pollution, while exact retries safely reuse decisions.
func TestContractQualificationCrossSourceAndReplay(t *testing.T) {
	srv, calls := chatServer(t,
		`{"tmdb_id":100,"confidence":0.95}`,
		`{"tmdb_id":100,"confidence":0.95}`,
	)
	c := testClient(srv.URL)
	tor := mediaTorrent("Synthetic.Title.2024.mkv")
	ext := Extraction{Title: "Synthetic Title", Year: 2024, Type: "movie", OK: true}
	cands := []Candidate{
		{ID: 100, Title: "Synthetic Title", Year: 2024},
		{ID: 101, Title: "Alternate Candidate", Year: 2023},
	}

	// 1. Initial local-source rerank call.
	id, conf, err := c.RerankForMediaType(context.Background(), tor, ext, "Synthetic Title", false, cands, llmcapture.CandidateSourceLocal)
	require.NoError(t, err)
	assert.Equal(t, int64(100), id)
	assert.InDelta(t, 0.95, conf, 0.001)
	assert.Equal(t, int32(1), atomic.LoadInt32(calls))

	// 2. Exact retry/replay with local source must hit cache without network dispatch.
	replayedID, replayedConf, err := c.RerankForMediaType(context.Background(), tor, ext, "Synthetic Title", false, cands, llmcapture.CandidateSourceLocal)
	require.NoError(t, err)
	assert.Equal(t, id, replayedID)
	assert.Equal(t, conf, replayedConf)
	assert.Equal(t, int32(1), atomic.LoadInt32(calls), "replay of identical local candidate request must hit cache")

	// 3. Same torrent and candidates under CandidateSourceAPI must NOT reuse local cache entry.
	apiID, apiConf, err := c.RerankForMediaType(context.Background(), tor, ext, "Synthetic Title", false, cands, llmcapture.CandidateSourceAPI)
	require.NoError(t, err)
	assert.Equal(t, int64(100), apiID)
	assert.InDelta(t, 0.95, apiConf, 0.001)
	assert.Equal(t, int32(2), atomic.LoadInt32(calls), "cross-source candidate evidence must not reuse cache across local vs API sources")

	// 4. Replay of API source must hit cache.
	replayedAPIID, replayedAPIConf, err := c.RerankForMediaType(context.Background(), tor, ext, "Synthetic Title", false, cands, llmcapture.CandidateSourceAPI)
	require.NoError(t, err)
	assert.Equal(t, apiID, replayedAPIID)
	assert.Equal(t, apiConf, replayedAPIConf)
	assert.Equal(t, int32(2), atomic.LoadInt32(calls), "replay of identical API candidate request must hit cache")
}

// TestContractQualificationCandidateOrderingAndSeparation verifies candidate list
// changes and candidate ordering produce distinct cache entries.
func TestContractQualificationCandidateOrderingAndSeparation(t *testing.T) {
	srv, calls := chatServer(t,
		`{"tmdb_id":100,"confidence":0.92}`,
		`{"tmdb_id":100,"confidence":0.92}`,
	)
	c := testClient(srv.URL)
	tor := mediaTorrent("Synthetic.Show.S01E01.mkv")
	ext := Extraction{Title: "Synthetic Show", Year: 2024, Type: "tv", OK: true}

	candsA := []Candidate{
		{ID: 100, Title: "Synthetic Show", Year: 2024},
		{ID: 200, Title: "Different Show", Year: 2020},
	}
	candsB := []Candidate{
		{ID: 200, Title: "Different Show", Year: 2020},
		{ID: 100, Title: "Synthetic Show", Year: 2024},
	}

	idA, _, err := c.RerankForMediaType(context.Background(), tor, ext, "Synthetic Show", true, candsA, llmcapture.CandidateSourceLocal)
	require.NoError(t, err)
	assert.Equal(t, int64(100), idA)
	assert.Equal(t, int32(1), atomic.LoadInt32(calls))

	// Reordered candidates must cause a new dispatch, not stale cache hit.
	idB, _, err := c.RerankForMediaType(context.Background(), tor, ext, "Synthetic Show", true, candsB, llmcapture.CandidateSourceLocal)
	require.NoError(t, err)
	assert.Equal(t, int64(100), idB)
	assert.Equal(t, int32(2), atomic.LoadInt32(calls), "reordered candidates must not reuse cache")
}

// TestContractQualificationConservativeNoAttachmentFallback verifies conservative
// fallback to 0/no-attachment when candidate is rejected, absent, or model declines.
func TestContractQualificationConservativeNoAttachmentFallback(t *testing.T) {
	// Case 1: Model hallucinates an ID not in candidate list -> must fallback to 0.
	srvHallucinate, _ := chatServer(t, `{"tmdb_id":9999,"confidence":0.99}`)
	c1 := testClient(srvHallucinate.URL)
	tor := mediaTorrent("Synthetic.Title.2024.mkv")
	ext := Extraction{Title: "Synthetic Title", Year: 2024, Type: "movie", OK: true}
	cands := []Candidate{{ID: 100, Title: "Synthetic Title", Year: 2024}}

	id, conf, err := c1.Rerank(context.Background(), tor, ext, cands)
	require.NoError(t, err)
	assert.Zero(t, id, "unmatched/hallucinated candidate ID must fall back to 0 (no attachment)")
	assert.Zero(t, conf)

	// Case 2: Model explicitly declines (tmdb_id: 0) -> fallback to 0.
	srvDecline, _ := chatServer(t, `{"tmdb_id":0,"confidence":0.0}`)
	c2 := testClient(srvDecline.URL)
	id2, conf2, err := c2.Rerank(context.Background(), tor, ext, cands)
	require.NoError(t, err)
	assert.Zero(t, id2)
	assert.Zero(t, conf2)

	// Case 3: Empty extraction title -> marked not OK.
	srvEmpty, _ := chatServer(t, `{"title":"","year":0,"type":"movie"}`)
	c3 := testClient(srvEmpty.URL)
	extEmpty, err := c3.Extract(context.Background(), tor)
	require.NoError(t, err)
	assert.False(t, extEmpty.OK, "empty title must mark extraction as not OK")
}

// TestContractQualificationRenderedKeyInvariants verifies key namespace prefixes,
// cryptographic digest structure, and that sensitive API keys are never rendered into keys.
func TestContractQualificationRenderedKeyInvariants(t *testing.T) {
	c := testClient("https://api.example.invalid/v1/chat/completions")
	c.cfg.APIKey = "super-secret-api-key-never-leak"

	tor := extractionTorrent("Synthetic.Release.2024.mkv", "video.mkv", "sub.srt")
	extKey := c.extractKey(tor)
	assert.True(t, strings.HasPrefix(extKey, "extract|"))
	parts := strings.Split(extKey, "|")
	require.Len(t, parts, 2)
	assert.Len(t, parts[1], 64, "digest must be 64-hex SHA-256")
	assert.NotContains(t, extKey, "super-secret")

	batchKey := c.batchExtractKey([]model.Torrent{tor})
	assert.True(t, strings.HasPrefix(batchKey, "batch_extract|"))
	batchParts := strings.Split(batchKey, "|")
	require.Len(t, batchParts, 2)
	assert.Len(t, batchParts[1], 64, "batch digest must be 64-hex SHA-256")
	assert.NotContains(t, batchKey, "super-secret")
	assert.NotEqual(t, extKey, batchKey, "single and batch keys must never collide")
}

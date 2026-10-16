package llmmatch

import (
	"context"
	"sync/atomic"
	"testing"

	"github.com/spencercnorton/bitagent/internal/llmprovider"
	"github.com/spencercnorton/bitagent/internal/llmwork"
	"github.com/spencercnorton/bitagent/internal/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func extractionTorrent(name string, files ...string) model.Torrent {
	t := mediaTorrent(name)
	for _, path := range files {
		t.Files = append(t.Files, model.TorrentFile{Path: path})
	}
	return t
}

func TestExtractCacheSeparatesRenderedFileEvidence(t *testing.T) {
	srv, calls := chatServer(t,
		`{"title":"Example Anime","type":"tv","is_anime":true,"is_pack":false,"is_adult":false,"english":"sub"}`,
		`{"title":"Example Anime","type":"tv","is_anime":true,"is_pack":false,"is_adult":false,"english":"none"}`,
	)
	c := testClient(srv.URL)
	english := extractionTorrent("Example.Anime.S01E02.mkv", "episode.mkv", "episode.eng.srt")
	foreign := extractionTorrent(english.Name, "episode.mkv", "episode.fra.srt")

	first, err := c.Extract(context.Background(), english)
	require.NoError(t, err)
	second, err := c.Extract(context.Background(), foreign)
	require.NoError(t, err)
	assert.Equal(t, EnglishSub, first.English)
	assert.Equal(t, EnglishNone, second.English)
	assert.Equal(t, int32(2), atomic.LoadInt32(calls))

	replayed, err := c.Extract(context.Background(), english)
	require.NoError(t, err)
	assert.Equal(t, first, replayed)
	assert.Equal(t, int32(2), atomic.LoadInt32(calls), "identical evidence still reuses its decision")
}

func TestExtractCacheIdentityMatchesRenderedRequest(t *testing.T) {
	c := testClient("https://example.invalid/v1/chat/completions")
	base := extractionTorrent("Example.Movie.2024.mkv", "movie.mkv", "movie.eng.srt")
	key := c.extractKey(base)

	for _, tc := range []struct {
		name string
		t    model.Torrent
	}{
		{"name case", extractionTorrent("example.movie.2024.mkv", "movie.mkv", "movie.eng.srt")},
		{"file path", extractionTorrent(base.Name, "movie.mkv", "movie.fra.srt")},
		{"files absent", extractionTorrent(base.Name)},
	} {
		t.Run(tc.name, func(t *testing.T) { assert.NotEqual(t, key, c.extractKey(tc.t)) })
	}
	assert.Equal(t, key, c.extractKey(extractionTorrent(base.Name, "movie.eng.srt", "movie.mkv")), "preload row order is not rendered request evidence")
	// Neither size nor admission-only source identity is rendered to the model.
	metadataOnly := base
	metadataOnly.Size++
	metadataOnly.Files = append([]model.TorrentFile(nil), base.Files...)
	metadataOnly.Files[0].Size = 123
	assert.Equal(t, key, c.extractKey(metadataOnly))

	// The deployed renderer omits all files when there are more than five.
	large := extractionTorrent(base.Name, "a", "b", "c", "d", "e", "f")
	otherLarge := extractionTorrent(base.Name, "g", "h", "i", "j", "k", "l", "m")
	assert.Equal(t, c.extractKey(extractionTorrent(base.Name)), c.extractKey(large))
	assert.Equal(t, c.extractKey(large), c.extractKey(otherLarge))
}

func TestIndexedExtractionEvidenceIsStableAfterShuffledPreload(t *testing.T) {
	c := testClient("https://example.invalid/v1/chat/completions")
	before := extractionTorrent("Example.Movie.2024.mkv", "movie.mkv", "movie.eng.srt", "notes.txt")
	for i := range before.Files {
		before.Files[i].Index = uint(i)
	}
	shuffled := before
	shuffled.Files = append([]model.TorrentFile(nil), before.Files...)
	shuffled.Files[0], shuffled.Files[2] = shuffled.Files[2], shuffled.Files[0]
	order := append([]model.TorrentFile(nil), shuffled.Files...)
	require.Equal(t, llmwork.SourceDigest(before), llmwork.SourceDigest(shuffled))
	require.Equal(t, ExtractInput(before.Name, extractionModelFiles(before)), ExtractInput(shuffled.Name, extractionModelFiles(shuffled)))
	require.Equal(t, c.extractKey(before), c.extractKey(shuffled))
	require.Equal(t, c.WorkInputDigest(before), c.WorkInputDigest(shuffled))
	require.Equal(t, order, shuffled.Files)
}

func TestExtractCacheIdentityIncludesRequestRoute(t *testing.T) {
	c := testClient("https://example.invalid/v1/chat/completions")
	torrent := extractionTorrent("Example.Movie.2024.mkv")
	initial := c.cfg
	key := c.extractKey(torrent)
	for _, tc := range []struct {
		name string
		edit func(*Config)
	}{
		{"endpoint", func(cfg *Config) { cfg.Endpoint = "https://other.invalid/v1/chat/completions" }},
		{"model", func(cfg *Config) { cfg.Model = "another-model" }},
		{"prompt version", func(cfg *Config) { cfg.PromptVersion = "another-prompt" }},
		{"backend", func(cfg *Config) { cfg.ChatBackend = llmprovider.ChatBackendOllama }},
		{"provider", func(cfg *Config) { cfg.OpenrouterProvider = "example-provider" }},
		{"data sharing", func(cfg *Config) { cfg.OpenaiDataSharing = true }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c.cfg = initial
			tc.edit(&c.cfg)
			assert.NotEqual(t, key, c.extractKey(torrent))
		})
	}
	c.cfg = initial
	// API credentials do not become cache material; each client owns its cache.
	c.cfg.APIKey = "synthetic-credential"
	assert.Equal(t, key, c.extractKey(torrent))
}

func TestExtractManySeparatesFileEvidenceAndKeepsGroupsIsolated(t *testing.T) {
	srv, calls := chatServer(t,
		`{"title":"Example Anime","type":"tv","is_anime":true,"is_pack":false,"is_adult":false,"english":"sub"}`,
		`{"title":"Example Anime","type":"tv","is_anime":true,"is_pack":false,"is_adult":false,"english":"none"}`,
		batchItemsJSON(8),
		`{"title":"Some Movie 0","type":"movie","is_anime":false,"is_pack":false,"is_adult":false}`,
	)
	c := testClient(srv.URL)
	english := extractionTorrent("Example.Anime.S01E02.mkv", "episode.mkv", "episode.eng.srt")
	foreign := extractionTorrent(english.Name, "episode.mkv", "episode.fra.srt")
	torrents := append([]model.Torrent{english, foreign}, batchTorrents(8)...)
	stats := c.ExtractMany(context.Background(), torrents, len(torrents))
	assert.Equal(t, 10, stats.OK)
	assert.Equal(t, 2, stats.Singles)
	assert.Equal(t, 1, stats.Batches)
	assert.Zero(t, stats.Cached+stats.Failed)
	assert.Equal(t, int32(3), atomic.LoadInt32(calls))

	repeated := c.ExtractMany(context.Background(), torrents, len(torrents))
	assert.Equal(t, 10, repeated.Cached)
	assert.Zero(t, repeated.Batches+repeated.Singles)
	assert.Equal(t, int32(3), atomic.LoadInt32(calls), "only exact contracts are reused")
	first, _ := c.Extract(context.Background(), english)
	second, _ := c.Extract(context.Background(), foreign)
	assert.Equal(t, EnglishSub, first.English)
	assert.Equal(t, EnglishNone, second.English)
	assert.Equal(t, int32(3), atomic.LoadInt32(calls))
	_, err := c.Extract(context.Background(), torrents[2])
	require.NoError(t, err)
	assert.Equal(t, int32(4), atomic.LoadInt32(calls), "group evidence cannot warm a single extraction")
}

func TestBatchExtractionCacheBindsWholeRequest(t *testing.T) {
	c := testClient("https://example.invalid/v1/chat/completions")
	torrents := batchTorrents(8)
	key := c.batchExtractKey(torrents)
	assert.NotEqual(t, key, c.extractKey(torrents[0]))
	reordered := append([]model.Torrent(nil), torrents...)
	reordered[0], reordered[1] = reordered[1], reordered[0]
	assert.NotEqual(t, key, c.batchExtractKey(reordered))
	changed := append([]model.Torrent(nil), torrents...)
	changed[7].Name = "Another.Movie.2024.mkv"
	assert.NotEqual(t, key, c.batchExtractKey(changed))
	c.cfg.Endpoint = "https://other.invalid/v1/chat/completions"
	assert.NotEqual(t, key, c.batchExtractKey(torrents))
}

func TestBacklogGroupedWarmingRequiresCompatibleContract(t *testing.T) {
	srv, calls := chatServer(t, batchItemsJSON(8))
	c := testClient(srv.URL)
	budget := &embeddingBudgetProbe{}
	c.budget = budget
	require.Error(t, c.ValidateBacklogExtraction(32))
	assert.Zero(t, atomic.LoadInt32(calls), "preflight refuses before dispatch")
	assert.Zero(t, budget.attempts.Load(), "preflight refuses before budget reservation")
	require.NoError(t, c.ValidateBacklogExtraction(1))
	require.NoError(t, c.ValidateBacklogExtraction(minBatchSplit-1))
	c.capture = &captureProbe{}
	require.NoError(t, c.ValidateBacklogExtraction(32), "capture mode already sends compatible singles")
}

func TestExtractCacheCannotExposeNativePrivateEvidence(t *testing.T) {
	srv, calls := chatServer(t, `{"title":"Example Movie","type":"movie","is_anime":false,"is_pack":false,"is_adult":false}`)
	c := testClient(srv.URL)
	torrent := extractionTorrent("Example.Movie.2024.mkv", "movie.mkv")
	_, err := c.Extract(context.Background(), torrent)
	require.NoError(t, err)
	torrent.Private = true
	got, err := c.Extract(context.Background(), torrent)
	require.NoError(t, err)
	assert.Equal(t, Extraction{}, got)
	assert.Equal(t, int32(1), atomic.LoadInt32(calls))
}

package llmmatch

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/spencercnorton/bitagent/internal/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

// chatEcho stands in for the OpenAI-compatible endpoint. It returns the queued
// message content wrapped in a choices envelope and counts calls.
func chatServer(t *testing.T, contents ...string) (*httptest.Server, *int32) {
	t.Helper()
	var calls int32
	i := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		body, _ := io.ReadAll(r.Body)
		require.Contains(t, string(body), `"response_format"`)
		content := contents[len(contents)-1]
		if i < len(contents) {
			content = contents[i]
		}
		i++
		env := map[string]any{"choices": []map[string]any{{"message": map[string]string{"content": content}}}}
		_ = json.NewEncoder(w).Encode(env)
	}))
	t.Cleanup(srv.Close)
	return srv, &calls
}

func testClient(endpoint string) *Client {
	return testClientWithPrivacy(endpoint, nil)
}

func testClientWithPrivacy(endpoint string, privacy PrivacyStore) *Client {
	cfg := NewDefaultConfig()
	cfg.Enabled = true
	cfg.Endpoint = endpoint
	cfg.MinTotalSizeBytes = 0 // don't gate on size in tests
	return NewClient(cfg, privacy, NewMetrics(), zap.NewNop().Sugar())
}

type privacyProbe struct {
	isPrivate bool
	err       error
	calls     atomic.Int32
}

func (p *privacyProbe) IsPrivateInfoHash(context.Context, []byte) (bool, error) {
	p.calls.Add(1)
	return p.isPrivate, p.err
}

func mediaTorrent(name string) model.Torrent {
	return model.Torrent{
		Name:      name,
		Size:      2_000_000_000,
		Extension: model.NewNullString("mkv"),
	}
}

func TestExtract(t *testing.T) {
	srv, _ := chatServer(t, `{"title":"The Raid: Redemption","year":2011,"type":"movie","season":0,"episode":0,"is_anime":false,"is_pack":false,"is_adult":false}`)
	c := testClient(srv.URL)

	ext, err := c.Extract(context.Background(), mediaTorrent("the-raid-redemption-2011.mkv"))
	require.NoError(t, err)
	assert.True(t, ext.OK)
	assert.Equal(t, "The Raid: Redemption", ext.Title)
	assert.Equal(t, 2011, ext.Year)
	assert.Equal(t, "movie", ext.Type)
}

func TestExtractEmptyTitleNotOK(t *testing.T) {
	srv, _ := chatServer(t, `{"title":"","year":0,"type":"movie"}`)
	c := testClient(srv.URL)
	ext, err := c.Extract(context.Background(), mediaTorrent("random garbage upload"))
	require.NoError(t, err)
	assert.False(t, ext.OK)
}

func TestExtractCached(t *testing.T) {
	srv, calls := chatServer(t, `{"title":"Dune","year":2021,"type":"movie","is_anime":false,"is_pack":false,"is_adult":false}`)
	c := testClient(srv.URL)
	tor := mediaTorrent("Dune.2021.1080p.mkv")
	_, _ = c.Extract(context.Background(), tor)
	_, _ = c.Extract(context.Background(), tor)
	assert.Equal(t, int32(1), atomic.LoadInt32(calls), "second Extract must hit cache")
}

func TestRerankPicksCandidate(t *testing.T) {
	srv, _ := chatServer(t, `{"tmdb_id":429200,"confidence":0.93}`)
	c := testClient(srv.URL)
	cands := []Candidate{
		{ID: 1, Title: "The Raid 2", Year: 2014},
		{ID: 429200, Title: "The Raid: Redemption", Year: 2011},
	}
	id, conf, err := c.Rerank(context.Background(), mediaTorrent("the-raid-redemption-2011"), Extraction{Title: "The Raid: Redemption", Year: 2011}, cands)
	require.NoError(t, err)
	assert.Equal(t, int64(429200), id)
	assert.InDelta(t, 0.93, conf, 0.001)
}

func TestRerankRejectsHallucinatedID(t *testing.T) {
	// Model returns an id that was not among the candidates -> treated as none.
	srv, _ := chatServer(t, `{"tmdb_id":999999,"confidence":0.99}`)
	c := testClient(srv.URL)
	cands := []Candidate{{ID: 1, Title: "A", Year: 2000}}
	id, _, err := c.Rerank(context.Background(), mediaTorrent("x"), Extraction{Title: "A"}, cands)
	require.NoError(t, err)
	assert.Equal(t, int64(0), id, "id not shown to the model must be rejected")
}

func TestRerankNone(t *testing.T) {
	srv, _ := chatServer(t, `{"tmdb_id":0,"confidence":0}`)
	c := testClient(srv.URL)
	id, _, err := c.Rerank(context.Background(), mediaTorrent("x"), Extraction{Title: "A"}, []Candidate{{ID: 5, Title: "A", Year: 1}})
	require.NoError(t, err)
	assert.Equal(t, int64(0), id)
}

func TestRerankRejectsOutOfRangeConfidence(t *testing.T) {
	for _, confidence := range []string{"-0.1", "1.5"} {
		srv, _ := chatServer(
			t,
			`{"tmdb_id":5,"confidence":`+confidence+`}`,
		)
		c := testClient(srv.URL)
		id, gotConfidence, err := c.Rerank(
			context.Background(),
			mediaTorrent("A.2000.mkv"),
			Extraction{Title: "A", Year: 2000},
			[]Candidate{{ID: 5, Title: "A", Year: 2000}},
		)
		require.Error(t, err)
		assert.Equal(t, int64(0), id)
		assert.Equal(t, 0.0, gotConfidence)
		srv.Close()
	}
}

func TestExtractAnime(t *testing.T) {
	srv, _ := chatServer(t, `{"title":"Attack on Titan","year":2013,"type":"tv","season":1,"episode":1,"is_anime":true,"english":"sub","is_pack":false,"is_adult":false}`)
	c := testClient(srv.URL)
	ext, err := c.Extract(context.Background(), mediaTorrent("[SubsPlease] Shingeki no Kyojin - 01 (1080p) [A1B2C3D4].mkv"))
	require.NoError(t, err)
	assert.True(t, ext.IsAnime)
	assert.Equal(t, "Attack on Titan", ext.Title)
	assert.Equal(t, "sub", ext.English)
}

func TestExtractEnglishDefaultsUnknown(t *testing.T) {
	srv, _ := chatServer(t, `{"title":"Dune","year":2021,"type":"movie","is_anime":false,"is_pack":false,"is_adult":false}`)
	c := testClient(srv.URL)
	ext, err := c.Extract(context.Background(), mediaTorrent("Dune.2021.1080p.mkv"))
	require.NoError(t, err)
	assert.Equal(t, "unknown", ext.English, "missing english field normalises to unknown")
}

func TestAnimeEnglishOK(t *testing.T) {
	c := testClient("http://unused") // defaults: require=true, allowSub=true
	cases := []struct {
		anime   bool
		english string
		want    bool
	}{
		{false, "none", true},   // non-anime never gated
		{true, "dub", true},     // dub always ok
		{true, "sub", true},     // subs count (allowSubOnly default true)
		{true, "unknown", true}, // don't drop on uncertainty
		{true, "none", false},   // raw japanese-only -> rejected
	}
	for _, tc := range cases {
		got := c.AnimeEnglishOK(Extraction{IsAnime: tc.anime, English: tc.english})
		assert.Equalf(t, tc.want, got, "anime=%v english=%s", tc.anime, tc.english)
	}

	// With dubs-only policy, subs are rejected.
	c.cfg.AnimeAllowSubOnly = false
	assert.False(t, c.AnimeEnglishOK(Extraction{IsAnime: true, English: "sub"}))
	assert.True(t, c.AnimeEnglishOK(Extraction{IsAnime: true, English: "dub"}))

	// With the whole gate off, everything passes.
	c.cfg.AnimeRequireEnglish = false
	assert.True(t, c.AnimeEnglishOK(Extraction{IsAnime: true, English: "none"}))
}

func TestAllowGates(t *testing.T) {
	c := testClient("http://unused")
	// no media extension -> rejected
	assert.False(t, c.Allow(context.Background(), model.Torrent{Name: "data", Size: 1_000_000_000, Extension: model.NewNullString("iso")}))
	// media file present -> allowed
	assert.True(t, c.Allow(context.Background(), mediaTorrent("Movie.2020.mkv")))
}

func TestNativePrivateBlocksEveryMatcherModelBoundaryIndependentOfEvidence(t *testing.T) {
	tests := []struct {
		name    string
		privacy *privacyProbe
	}{
		{name: "missing evidence store"},
		{name: "stale public evidence", privacy: &privacyProbe{isPrivate: false}},
		{name: "evidence lookup error", privacy: &privacyProbe{err: errors.New("stale connection")}},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			srv, calls := chatServer(t,
				`{"title":"Should Never Leave","year":2026,"type":"movie","is_anime":false,"is_pack":false,"is_adult":false}`,
				`{"tmdb_id":1,"confidence":1}`,
			)
			c := testClientWithPrivacy(srv.URL, tc.privacy)
			tor := mediaTorrent("Private.Release.2026.1080p.mkv")
			tor.Private = true

			assert.False(t, c.Allow(context.Background(), tor))
			ext, err := c.Extract(context.Background(), tor)
			require.NoError(t, err)
			assert.False(t, ext.OK)
			id, conf, err := c.Rerank(context.Background(), tor, Extraction{Title: "Private Release"}, []Candidate{{ID: 1, Title: "Private Release", Year: 2026}})
			require.NoError(t, err)
			assert.Zero(t, id)
			assert.Zero(t, conf)
			assert.Zero(t, atomic.LoadInt32(calls), "native private flag must block extract and rerank HTTP calls")
			if tc.privacy != nil {
				assert.Zero(t, tc.privacy.calls.Load(), "native flag must not depend on evidence lookup")
			}
		})
	}
}

func TestPromptsAreSent(t *testing.T) {
	var gotSystem string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req chatRequest
		_ = json.Unmarshal(body, &req)
		for _, m := range req.Messages {
			if m.Role == "system" {
				gotSystem = m.Content
			}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"choices": []map[string]any{{"message": map[string]string{"content": `{"title":"X","year":0,"type":"movie","is_anime":false,"is_pack":false,"is_adult":false}`}}}})
	}))
	defer srv.Close()
	c := testClient(srv.URL)
	_, _ = c.Extract(context.Background(), mediaTorrent("X.2020.mkv"))
	assert.True(t, strings.Contains(gotSystem, "canonical"), "extract system prompt should be used")
}

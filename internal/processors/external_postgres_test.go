package processors

import (
	"encoding/hex"
	"encoding/json"
	"encoding/xml"
	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/spencercnorton/bitagent/internal/blocking"
	"github.com/spencercnorton/bitagent/internal/classifier"
	"github.com/spencercnorton/bitagent/internal/database/dao"
	"github.com/spencercnorton/bitagent/internal/database/search"
	"github.com/spencercnorton/bitagent/internal/lazy"
	"github.com/spencercnorton/bitagent/internal/processor"
	"github.com/spencercnorton/bitagent/internal/protocol"
	"github.com/spencercnorton/bitagent/internal/serving"
	"github.com/spencercnorton/bitagent/internal/tmdb"
	"github.com/spencercnorton/bitagent/internal/torznab"
	"github.com/spencercnorton/bitagent/internal/torznab/adapter"
	torznabhttp "github.com/spencercnorton/bitagent/internal/torznab/httpserver"
	"go.uber.org/zap"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	v1 "github.com/spencercnorton/bitagent/processorapi/v1"
	"github.com/stretchr/testify/require"
)

// This accepts ANY compatible pair with a common ID and different immutable
// identities. No model implementation or provider library enters this test.
func TestExternalServiceUpgradeRollback(t *testing.T) {
	endpointA, endpointB := os.Getenv("PROCESSOR_TEST_ENDPOINT_A"), os.Getenv("PROCESSOR_TEST_ENDPOINT_B")
	if endpointA == "" || endpointB == "" {
		t.Skip("two independently running compatible services not configured")
	}
	s, p, _ := fixture(t)
	s.cfg.RequestTimeout = 5 * time.Second
	s.transport = NewTransport(5 * time.Second)
	auth := ""
	if path := os.Getenv("PROCESSOR_TEST_TOKEN_FILE"); path != "" {
		token, e := os.ReadFile(path)
		require.NoError(t, e)
		auth = "BITAGENT_PROCESSOR_TOKEN_CONFORMANCE"
		t.Setenv(auth, string(token))
	}
	write := func(url string) v1.RegistryWrite {
		return v1.RegistryWrite{Endpoint: url, State: "active", Mode: "apply", Configuration: json.RawMessage(`{}`), AuthEnv: auth}
	}
	wa, wb := write(endpointA), write(endpointB)
	ma, _, e := s.transport.Inspect(testContext, wa)
	require.NoError(t, e)
	mb, _, e := s.transport.Inspect(testContext, wb)
	require.NoError(t, e)
	require.Equal(t, ma.Processor.ID, mb.Processor.ID)
	require.NotEqual(t, ma.Processor, mb.Processor)
	wa.ExpectedProcessor = &ma.Processor
	wb.ExpectedProcessor = &mb.Processor
	matched := seedTorrent(t, p, 29, "movie")
	_, e = p.Exec(testContext, `INSERT INTO content(type,source,id,title,release_year,created_at,updated_at)VALUES('movie','tmdb','991001','Matched Reference',2024,now(),now());UPDATE torrents SET name='Matched.Reference.2024.mkv' WHERE info_hash=$1;UPDATE torrent_contents SET content_source='tmdb',content_id='991001',tsv=to_tsvector('simple','Matched Reference 2024') WHERE info_hash=$1`, matched)
	require.NoError(t, e)
	hA := seedTorrent(t, p, 30, nil)
	a, e := s.Register(testContext, ma.Processor.ID, wa)
	require.NoError(t, e)
	drain := func() {
		t.Helper()
		for n := 0; n < 100; n++ {
			_, err := s.Scan(testContext)
			require.NoError(t, err)
			worked, err := s.RunOne(testContext)
			require.NoError(t, err)
			if !worked {
				var active int
				require.NoError(t, p.QueryRow(testContext, `SELECT count(*) FROM processor_jobs WHERE state IN('queued','deferred','leased')`).Scan(&active))
				if active > 0 {
					rows, err := p.Query(testContext, `SELECT input->'deterministic',results,outcome,COALESCE((SELECT reason FROM processor_attempts a WHERE a.job_id=processor_jobs.job_id ORDER BY lease_generation DESC LIMIT 1),reason) FROM processor_jobs WHERE state IN('queued','deferred','leased')`)
					require.NoError(t, err)
					for rows.Next() {
						var input, results []byte
						var outcome, reason string
						require.NoError(t, rows.Scan(&input, &results, &outcome, &reason))
						t.Logf("remaining: deterministic=%s results=%s outcome=%s reason=%s", input, results, outcome, reason)
					}
					rows.Close()
				}
				require.Zero(t, active)
				return
			}
		}
		t.Fatal("bounded conformance drain exhausted")
	}
	coreProcess(t, s, p, hA)
	drain()
	var first string
	require.NoError(t, p.QueryRow(testContext, `SELECT content_type FROM torrent_contents WHERE info_hash=$1`, hA).Scan(&first))
	require.Equal(t, "movie", first)
	hB := seedTorrent(t, p, 31, nil)
	coreProcess(t, s, p, hB)
	_, e = s.Scan(testContext)
	require.NoError(t, e)
	_, e = s.Scan(testContext)
	require.NoError(t, e)
	old, e := s.Claim(testContext)
	require.NoError(t, e)
	require.NotNil(t, old)
	job, e := s.Prepare(testContext, *old)
	require.NoError(t, e)
	require.NoError(t, s.startAttempt(testContext, *old))
	oldResult, e := s.transport.Process(testContext, endpointA, auth, job)
	require.NoError(t, e)
	wb.ExpectedGeneration = a.Generation
	b, e := s.Register(testContext, ma.Processor.ID, wb)
	require.NoError(t, e)
	require.ErrorIs(t, s.Finish(testContext, *old, job, oldResult, true), ErrStale)
	drain()
	var second string
	require.NoError(t, p.QueryRow(testContext, `SELECT content_type FROM torrent_contents WHERE info_hash=$1`, hB).Scan(&second))
	require.Equal(t, "tv_show", second)
	wa.ExpectedGeneration = b.Generation
	rolled, e := s.Register(testContext, ma.Processor.ID, wa)
	require.NoError(t, e)
	hC := seedTorrent(t, p, 32, nil)
	coreProcess(t, s, p, hC)
	drain()
	var third string
	require.NoError(t, p.QueryRow(testContext, `SELECT content_type FROM torrent_contents WHERE info_hash=$1`, hC).Scan(&third))
	require.Equal(t, "movie", third)
	require.NoError(t, s.Remove(testContext, ma.Processor.ID, rolled.Generation))
	seedTorrent(t, p, 33, nil)
	n, e := s.Scan(testContext)
	require.NoError(t, e)
	require.Zero(t, n)
	var count int
	require.NoError(t, p.QueryRow(testContext, `SELECT count(*) FROM torrent_contents WHERE content_type IS NOT NULL`).Scan(&count))
	require.Equal(t, 4, count)
	var retainedIdentity string
	require.NoError(t, p.QueryRow(testContext, `SELECT content_id FROM torrent_contents WHERE info_hash=$1`, matched).Scan(&retainedIdentity))
	require.Equal(t, "991001", retainedIdentity)
	assertIndexer(t, p)
	t.Logf("same_store=true same_core_process=true generations=%d,%d,%d stale_rejected=true types=%s,%s,%s removal_preserved=%d source=%s", a.ExecutionGeneration, b.ExecutionGeneration, rolled.ExecutionGeneration, first, second, third, count, hex.EncodeToString(hA))
}
func TestGenerateOperatorConformanceFixtures(t *testing.T) {
	if os.Getenv("PROCESSORS_WRITE_FIXTURES") != "1" {
		t.Skip("explicit fixture generation")
	}
	s, p, _ := fixture(t)
	seedTorrent(t, p, 40, nil)
	service := newService(t, "reference", []v1.Stage{v1.Classify}, func(j v1.Job) v1.Result { return result(j, "movie") })
	register(t, s, service)
	_, e := s.Scan(testContext)
	require.NoError(t, e)
	run(t, s)
	status, e := s.Status(testContext)
	require.NoError(t, e)
	status.Processors[0].Endpoint = "http://reference:8090"
	page, e := s.Jobs(testContext, 50, "", "reference")
	require.NoError(t, e)
	dir := filepath.Join("..", "..", "docs", "processors", "fixtures")
	require.NoError(t, os.MkdirAll(dir, 0755))
	for name, value := range map[string]any{"operator-status.json": status, "operator-jobs.json": page} {
		raw, err := json.MarshalIndent(value, "", "  ")
		require.NoError(t, err)
		require.NoError(t, os.WriteFile(filepath.Join(dir, name), append(raw, '\n'), 0644))
	}
}

// Real database search, CEL classifier, processor persistence and receipt path.
// Remote metadata is explicitly unconfigured. An unknown type has no normal
// match search to run; that remains not_attempted, never a fabricated miss.
func coreProcess(t *testing.T, s *Store, p *pgxpool.Pool, h []byte) {
	t.Helper()
	_, e := p.Exec(testContext, `UPDATE torrents SET name='OpaqueFixture',files_status='no_info' WHERE info_hash=$1`, h)
	require.NoError(t, e)
	db := stdlib.OpenDB(*p.Config().ConnConfig)
	defer db.Close()
	g, e := gorm.Open(postgres.New(postgres.Config{Conn: db}), &gorm.Config{Logger: logger.Discard})
	require.NoError(t, e)
	query := lazy.New(func() (*dao.Query, error) { return dao.Use(g), nil })
	searches := search.New(search.Params{Query: query})
	config := classifier.NewDefaultConfig()
	config.Flags = map[string]any{"tmdb_enabled": false}
	runner := classifier.New(classifier.Params{Config: config, Search: searches.Search, TmdbConfig: tmdb.NewDefaultConfig(), TmdbClient: lazy.New(func() (tmdb.Client, error) { return nil, nil })})
	core, e := processor.New(processor.Params{CoreRecorder: s, ClassifierConfig: config, Search: searches.Search, Workflow: runner.Runner, Dao: query, BlockingManager: lazy.New(func() (blocking.Manager, error) { return nil, nil }), Logger: zap.NewNop().Sugar()}).Processor.Get()
	require.NoError(t, e)
	var id protocol.ID
	copy(id[:], h)
	require.NoError(t, core.Process(testContext, processor.MessageParams{InfoHashes: []protocol.ID{id}}))
	var processing, matching string
	require.NoError(t, p.QueryRow(testContext, `SELECT processing_outcome,outcome FROM processor_core_receipts WHERE info_hash=$1`, h).Scan(&processing, &matching))
	require.Equal(t, "completed", processing)
	require.Equal(t, "not_attempted", matching)
}

func assertIndexer(t *testing.T, p *pgxpool.Pool) {
	t.Helper()
	db := stdlib.OpenDB(*p.Config().ConnConfig)
	defer db.Close()
	g, e := gorm.Open(postgres.New(postgres.Config{Conn: db}), &gorm.Config{Logger: logger.Discard})
	require.NoError(t, e)
	policy, e := serving.NewPolicy(serving.Config{ProcessorExclusions: true}, []string{"syntheticstrongadult"}, []string{"mkv"})
	require.NoError(t, e)
	queries := lazy.New(func() (*dao.Query, error) { return dao.Use(g), nil })
	consumer, e := search.New(search.Params{Query: queries, ServingPolicy: policy}).ServingSearch.Get()
	require.NoError(t, e)
	client := adapter.New(consumer)
	router := gin.New()
	require.NoError(t, torznabhttp.New(lazy.New(func() (torznab.Client, error) { return client, nil }), torznab.NewDefaultConfig(), nil).Apply(router))
	type attr struct {
		Name  string `xml:"name,attr"`
		Value string `xml:"value,attr"`
	}
	type item struct {
		Title string `xml:"title"`
		Attrs []attr `xml:"attr"`
	}
	type rss struct {
		Items []item `xml:"channel>item"`
	}
	request := func(query string) rss {
		w := httptest.NewRecorder()
		router.ServeHTTP(w, httptest.NewRequest("GET", "/torznab/api?t=search&q="+query, nil))
		require.Equal(t, 200, w.Code, w.Body.String())
		var out rss
		require.NoError(t, xml.Unmarshal(w.Body.Bytes(), &out))
		return out
	}
	opaque := request("OpaqueFixture")
	require.Len(t, opaque.Items, 3)
	movies, tv := 0, 0
	for _, row := range opaque.Items {
		for _, a := range row.Attrs {
			if a.Name == "category" && a.Value == "2000" {
				movies++
			}
			if a.Name == "category" && a.Value == "5000" {
				tv++
			}
			require.NotContains(t, []string{"tmdbid", "tvdbid", "imdb"}, a.Name)
		}
	}
	require.Equal(t, 2, movies)
	require.Equal(t, 1, tv)
	matched := request("Matched")
	require.Len(t, matched.Items, 1)
	identity := ""
	for _, a := range matched.Items[0].Attrs {
		if a.Name == "tmdbid" {
			identity = a.Value
		}
	}
	require.Equal(t, "991001", identity)
	t.Log("real Torznab HTTP/XML: 2 movie category2000 + 1 TV category5000, no invented IDs; pre-existing matched tmdbid991001 retained after upgrade/rollback/removal")
}

package llmstage

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/spencercnorton/bitagent/internal/classifier"
	"github.com/spencercnorton/bitagent/internal/classifier/classification"
	"github.com/spencercnorton/bitagent/internal/classifier/llmmatch"
	"github.com/spencercnorton/bitagent/internal/lazy"
	"github.com/spencercnorton/bitagent/internal/llmcapture"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

// The fixture requires an explicitly selected disposable database, uses an
// isolated schema, and never calls a paid provider or touches application data.
func newTypeAdmissionPostgresFixture(t *testing.T) (*pgxpool.Pool, *llmcapture.Recorder) {
	t.Helper()
	dsn := os.Getenv("BITAGENT_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("set BITAGENT_TEST_POSTGRES_DSN to a disposable PostgreSQL")
	}
	ctx := context.Background()
	admin, err := pgxpool.New(ctx, dsn)
	require.NoError(t, err)
	t.Cleanup(admin.Close)
	schema := fmt.Sprintf("bitagent_type_admission_test_%d", time.Now().UnixNano())
	quoted := pgx.Identifier{schema}.Sanitize()
	_, err = admin.Exec(ctx, "CREATE SCHEMA "+quoted)
	require.NoError(t, err)
	t.Cleanup(func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_, cleanupErr := admin.Exec(cleanupCtx, "DROP SCHEMA "+quoted+" CASCADE")
		require.NoError(t, cleanupErr)
	})
	pcfg, err := pgxpool.ParseConfig(dsn)
	require.NoError(t, err)
	pcfg.ConnConfig.RuntimeParams["search_path"] = schema
	pool, err := pgxpool.NewWithConfig(ctx, pcfg)
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	_, err = pool.Exec(ctx, `CREATE TABLE torrents(info_hash bytea PRIMARY KEY, private boolean NOT NULL);
CREATE TABLE label_evidence(info_hash bytea, source text, category text);`)
	require.NoError(t, err)
	for _, path := range []string{
		"../../../migrations/00046_llm_evaluation_capture.sql",
		"../../../migrations/00050_llm_request_budgets.sql",
		"../../../migrations/00051_llm_capture_results.sql",
		"../../../migrations/00052_classifier_type_capture.sql",
	} {
		raw, readErr := os.ReadFile(path)
		require.NoError(t, readErr)
		_, err = pool.Exec(ctx, strings.Split(string(raw), "-- +goose Down")[0])
		require.NoError(t, err)
	}
	store := llmcapture.NewPostgresStore(lazy.New(func() (*pgxpool.Pool, error) { return pool, nil }))
	cfg := llmcapture.NewDefaultConfig()
	cfg.Enabled = true
	// Evidence is public in this fixture. Native privacy remains authoritative
	// inside the production recorder's SQL, including on cached decisions.
	recorder, err := llmcapture.NewRecorder(cfg, fakePrivacy{}, store)
	require.NoError(t, err)
	return pool, recorder
}

func typeAdmissionCounterValue(t *testing.T, collector prometheus.Collector, name string) float64 {
	t.Helper()
	registry := prometheus.NewRegistry()
	require.NoError(t, registry.Register(collector))
	families, err := registry.Gather()
	require.NoError(t, err)
	for _, family := range families {
		if family.GetName() == "bitagent_classifier_llm_"+name {
			require.Len(t, family.Metric, 1)
			return family.Metric[0].GetCounter().GetValue()
		}
	}
	t.Fatalf("primary %s metric is missing", name)
	return 0
}

func TestTypeAdmissionPostgresShadowCaptureCacheAndNativePrivacy(t *testing.T) {
	pool, recorder := newTypeAdmissionPostgresFixture(t)
	ctx := context.Background()
	tor := baseTorrent()
	_, err := pool.Exec(ctx, "INSERT INTO torrents VALUES ($1,false)", tor.InfoHash.Bytes())
	require.NoError(t, err)

	response := httptest.NewRecorder()
	respondWith(response, "movie", .98)
	rawResponse := response.Body.Bytes()
	requests := make(chan []byte, 4)
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		body, readErr := io.ReadAll(r.Body)
		if readErr != nil {
			t.Errorf("read local test request: %v", readErr)
			http.Error(w, "read failed", http.StatusBadRequest)
			return
		}
		select {
		case requests <- body:
		default:
			t.Error("unexpected additional provider request")
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(rawResponse)
	}))
	t.Cleanup(srv.Close)
	lazyPool := lazy.New(func() (*pgxpool.Pool, error) { return pool, nil })
	// Exhaust the matcher first. The type-only stage must still have its own
	// single call available, without replenishing the matcher on cache access.
	matcherBudget := llmmatch.NewPostgresCallBudget(lazyPool)
	ok, err := matcherBudget.Reserve(ctx, 1, 1)
	require.NoError(t, err)
	require.True(t, ok)
	cfg := NewDefaultConfig()
	cfg.Enabled, cfg.EnableLive = true, false
	cfg.APIKey, cfg.Endpoint = "local-test-key", srv.URL
	cfg.DailyCallLimit, cfg.MonthlyCallLimit = 1, 1
	inner := fakeInner{err: classification.ErrUnmatched}
	s := NewStage(cfg, inner, fakePrivacy{}, NewMetrics(), zap.NewNop().Sugar(), Admission{
		Budget: llmmatch.NewPostgresTypeCallBudget(lazyPool), Capture: recorder,
	})
	result, err := s.Run(ctx, "", classifier.Flags{}, tor)
	require.ErrorIs(t, err, classification.ErrUnmatched)
	require.Equal(t, inner.res, result, "shadow mode must preserve the deterministic result")
	require.EqualValues(t, 1, calls.Load())
	requestBody := <-requests
	require.Equal(t, buildBoundedRequestBody(cfg, tor), requestBody)

	var task, contract string
	var modelInput, taskInput, captureKey []byte
	require.NoError(t, pool.QueryRow(ctx, `SELECT task, contract_id, model_input, task_input, capture_key
FROM llm_evaluation_captures`).Scan(&task, &contract, &modelInput, &taskInput, &captureKey))
	require.Equal(t, "classifier_type", task)
	require.Equal(t, "classifier-type-v1", contract)
	// jsonb normalizes formatting; the complete outbound JSON object, not a
	// reconstructed prompt subset, must survive capture without changed values.
	require.JSONEq(t, string(requestBody), string(modelInput))
	require.JSONEq(t, `{"live":false,"min_confidence":0.75}`, string(taskInput))
	var recordedHash []byte
	require.NoError(t, pool.QueryRow(ctx, `SELECT info_hash FROM llm_evaluation_capture_admissions
WHERE capture_key=$1`, captureKey).Scan(&recordedHash))
	require.Equal(t, tor.InfoHash.Bytes(), recordedHash)

	var body, digest, decisionJSON []byte
	var status int
	var errorClass string
	var decidedAt time.Time
	require.NoError(t, pool.QueryRow(ctx, `SELECT response_body, response_sha256, http_status, error_class, decision, decided_at
FROM llm_evaluation_capture_results WHERE capture_key=$1`, captureKey).
		Scan(&body, &digest, &status, &errorClass, &decisionJSON, &decidedAt))
	require.Equal(t, rawResponse, body)
	expectedDigest := sha256.Sum256(rawResponse)
	require.Equal(t, expectedDigest[:], digest)
	require.Equal(t, http.StatusOK, status)
	require.Equal(t, "none", errorClass)
	var decision llmcapture.TypeDecision
	require.NoError(t, json.Unmarshal(decisionJSON, &decision))
	require.Equal(t, llmcapture.TypeDecision{
		Outcome: "classified", Category: "movie", Confidence: .98,
		MinConfidence: .75, WouldApply: true, Live: false,
	}, decision)

	result, err = s.Run(ctx, "", classifier.Flags{}, tor)
	require.ErrorIs(t, err, classification.ErrUnmatched)
	require.Equal(t, inner.res, result)
	require.EqualValues(t, 1, calls.Load(), "cached response must not call HTTP again")
	require.Equal(t, float64(1), typeAdmissionCounterValue(t, s.metrics.cacheHitsTotal, "cache_hits_total"))
	require.Equal(t, float64(2), typeAdmissionCounterValue(t, s.metrics.auditTotal.WithLabelValues("decision_recorded"), "audit_total"))
	require.Zero(t, typeAdmissionCounterValue(t, s.metrics.liveAppliedTotal.WithLabelValues("movie"), "live_applied_total"))
	var sameDecision []byte
	var sameDecidedAt time.Time
	require.NoError(t, pool.QueryRow(ctx, `SELECT decision, decided_at FROM llm_evaluation_capture_results
WHERE capture_key=$1`, captureKey).Scan(&sameDecision, &sameDecidedAt))
	require.JSONEq(t, string(decisionJSON), string(sameDecision))
	require.Equal(t, decidedAt, sameDecidedAt, "cache use must not rewrite the first durable decision")

	// The in-memory torrent and evidence lookup are intentionally stale-public.
	// Reusing the cached result must still fail the current database native flag.
	_, err = pool.Exec(ctx, "UPDATE torrents SET private=true WHERE info_hash=$1", tor.InfoHash.Bytes())
	require.NoError(t, err)
	result, err = s.Run(ctx, "", classifier.Flags{}, tor)
	require.ErrorIs(t, err, classification.ErrUnmatched)
	require.Equal(t, inner.res, result)
	require.EqualValues(t, 1, calls.Load())
	require.Equal(t, float64(2), typeAdmissionCounterValue(t, s.metrics.cacheHitsTotal, "cache_hits_total"))
	require.Equal(t, float64(1), typeAdmissionCounterValue(t, s.metrics.auditTotal.WithLabelValues("decision_error"), "audit_total"))
	require.Equal(t, float64(2), typeAdmissionCounterValue(t, s.metrics.auditTotal.WithLabelValues("decision_recorded"), "audit_total"))
	require.Zero(t, typeAdmissionCounterValue(t, s.metrics.liveAppliedTotal.WithLabelValues("movie"), "live_applied_total"))

	for _, scope := range []string{"matcher", "classifier_type"} {
		var daily, monthly int
		require.NoError(t, pool.QueryRow(ctx, `SELECT daily_calls, monthly_calls
FROM llm_request_budgets WHERE scope=$1`, scope).Scan(&daily, &monthly))
		require.Equal(t, 1, daily)
		require.Equal(t, 1, monthly)
	}
	ok, err = matcherBudget.Reserve(ctx, 1, 1)
	require.NoError(t, err)
	require.False(t, ok, "type-stage use must not reset the matcher allowance")
	var count int
	require.NoError(t, pool.QueryRow(ctx, "SELECT count(*) FROM llm_evaluation_captures").Scan(&count))
	require.Equal(t, 1, count)
	require.NoError(t, pool.QueryRow(ctx, "SELECT count(*) FROM llm_evaluation_capture_results").Scan(&count))
	require.Equal(t, 1, count)
}

package llmstage

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/spencercnorton/bitagent/internal/classifier/llmmatch"
	"github.com/spencercnorton/bitagent/internal/lazy"
	"github.com/spencercnorton/bitagent/internal/llmcapture"
	"github.com/spencercnorton/bitagent/internal/version"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

func TestControlledTypeReplaysAcrossRestartBeforeAllowanceOrCooldown(t *testing.T) {
	previous := version.GitTag
	version.GitTag = "synthetic-build-a"
	defer func() { version.GitTag = previous }()
	pool, recorder := newTypeAdmissionPostgresFixture(t)
	raw, err := os.ReadFile("../../../migrations/00056_llm_work_lifecycle.sql")
	require.NoError(t, err)
	_, err = pool.Exec(context.Background(), strings.Split(string(raw), "-- +goose Down")[0])
	require.NoError(t, err)
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { calls.Add(1); respondWith(w, "movie", .98) }))
	defer server.Close()
	cfg := NewDefaultConfig()
	cfg.Enabled = true
	cfg.APIKey = "synthetic-test-key"
	cfg.Endpoint = server.URL
	cfg.DailyCallLimit = 1
	cfg.MonthlyCallLimit = 1
	pg := lazy.New(func() (*pgxpool.Pool, error) { return pool, nil })
	newStage := func() *Stage {
		return NewStage(cfg, fakeInner{}, fakePrivacy{}, NewMetrics(), zap.NewNop().Sugar(), Admission{
			Budget: llmmatch.NewPostgresTypeCallBudget(pg), Capture: recorder, Dispatch: llmcapture.NewPostgresDispatchController(llmcapture.NewPostgresStore(pg), true),
		})
	}
	tor := baseTorrent()
	_, err = pool.Exec(context.Background(), `INSERT INTO torrents VALUES($1,false)`, tor.InfoHash.Bytes())
	require.NoError(t, err)
	first, err := newStage().classify(context.Background(), tor)
	require.NoError(t, err)
	require.True(t, first.receipt.FirstObservation)
	restarted := newStage()
	restarted.retryAfter.Store(time.Now().Add(24 * time.Hour).UnixNano())
	again, err := restarted.classify(context.Background(), tor)
	require.NoError(t, err)
	require.Equal(t, first.MediaType, again.MediaType)
	require.True(t, again.receipt.FromCache)
	require.Equal(t, int32(1), calls.Load())
	var used int
	require.NoError(t, pool.QueryRow(context.Background(), `SELECT daily_calls FROM llm_request_budgets WHERE scope='classifier_type'`).Scan(&used))
	require.Equal(t, 1, used)
	// Only the binary generation changes. The exact semantic request must
	// replay its original first result and cannot become another observation.
	version.GitTag = "synthetic-build-b"
	nextBuild, err := newStage().classify(context.Background(), tor)
	require.NoError(t, err)
	require.True(t, nextBuild.receipt.FromCache)
	require.Equal(t, first.receipt.CaptureKey, nextBuild.receipt.CaptureKey)
	require.Equal(t, int32(1), calls.Load())
	var responseCount int
	require.NoError(t, pool.QueryRow(context.Background(), `SELECT count(*) FROM llm_evaluation_capture_results`).Scan(&responseCount))
	require.Equal(t, 1, responseCount)
	var originalBuild string
	require.NoError(t, pool.QueryRow(context.Background(), `SELECT build_identity FROM llm_evaluation_captures WHERE capture_key=$1`, nextBuild.receipt.CaptureKey).Scan(&originalBuild))
	require.Equal(t, "synthetic-build-a", originalBuild)
	_, err = pool.Exec(context.Background(), `UPDATE torrents SET private=true WHERE info_hash=$1`, tor.InfoHash.Bytes())
	require.NoError(t, err)
	_, err = newStage().classify(context.Background(), tor)
	require.Error(t, err)
	require.Equal(t, int32(1), calls.Load(), "private replay must not escape the current source gate")
}

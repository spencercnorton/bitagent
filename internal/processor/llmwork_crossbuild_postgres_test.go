package processor

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/spencercnorton/bitagent/internal/classifier"
	"github.com/spencercnorton/bitagent/internal/classifier/llmmatch"
	"github.com/spencercnorton/bitagent/internal/database/search"
	"github.com/spencercnorton/bitagent/internal/lazy"
	"github.com/spencercnorton/bitagent/internal/llmwork"
	"github.com/spencercnorton/bitagent/internal/model"
	"github.com/spencercnorton/bitagent/internal/tmdb"
	"github.com/spencercnorton/bitagent/internal/version"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

func TestPostgresDeferredMatcherCompletesAcrossBuildWithoutBuyingExtractionAgain(t *testing.T) {
	previous := version.GitTag
	version.GitTag = "synthetic-build-a"
	defer func() { version.GitTag = previous }()
	h, work, pool, source, recorder, dispatch := resetOptionalHarness(t)
	ctx := context.Background()
	_, err := pool.Exec(ctx, `INSERT INTO content(type,source,id,title,release_year,release_date,tsv,created_at,updated_at)VALUES('movie','tmdb','42','Amber Signal',2026,make_date(2026,1,1),to_tsvector('simple','Amber Signal'),now(),now());UPDATE torrent_contents SET content_type='movie'`)
	require.NoError(t, err)
	var extracts, reranks atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			Messages []struct {
				Content string `json:"content"`
			} `json:"messages"`
		}
		require.NoError(t, json.NewDecoder(r.Body).Decode(&request))
		require.NotEmpty(t, request.Messages)
		answer := `{"title":"Amber Signal","year":2026,"type":"movie","season":0,"episode":0,"is_anime":false,"is_pack":false,"is_adult":false,"english":"unknown"}`
		if request.Messages[0].Content == llmmatch.ExtractPrompt() {
			extracts.Add(1)
		} else {
			reranks.Add(1)
			answer = `{"tmdb_id":42,"confidence":0.99}`
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{"finish_reason": "stop", "message": map[string]any{"role": "assistant", "content": answer}}}})
	}))
	defer server.Close()
	cfg := llmmatch.NewDefaultConfig()
	cfg.Enabled = true
	cfg.EnableLive = true
	cfg.Endpoint = server.URL
	cfg.APIKey = "synthetic-test-key"
	cfg.DailyCallLimit = 1
	cfg.MonthlyCallLimit = 20
	newMatcher := func() *llmmatch.Client {
		matcher := llmmatch.NewClientWithBudget(cfg, typeEnrichmentPublicPrivacy{}, llmmatch.NewMetrics(), zap.NewNop().Sugar(), recorder, llmmatch.NewPostgresCallBudget(h.Pool)).WithDispatchControl(dispatch)
		matcher.SetWork(work, h.Classifier)
		observer := classifier.NewMatchDecisionObserver(recorder, matcher)
		backend, x := h.Search.Get()
		require.NoError(t, x)
		runner, x := classifier.New(classifier.Params{Config: h.Classifier, Search: lazy.New(func() (search.Search, error) { return backend, nil }), TmdbClient: lazy.New(func() (tmdb.Client, error) { return nil, nil }), LlmMatch: matcher, MatchDecisionObserver: observer}).Runner.Get()
		require.NoError(t, x)
		h.Runner = lazy.New(func() (classifier.Runner, error) { return runner, nil })
		h.Matcher = matcher
		h.Observer = observer
		return matcher
	}
	matcher := newMatcher()
	payload := llmmatch.WorkPayload{Type: model.NewNullContentType(model.ContentTypeMovie)}
	body, err := json.Marshal(payload)
	require.NoError(t, err)
	_, err = work.Enqueue(ctx, llmwork.Draft{Kind: llmwork.Matcher, InfoHash: source.InfoHash.Bytes(), SourceDigest: llmwork.SourceDigest(source), PolicyDigest: llmwork.Digest(matcher.WorkPolicy(payload)), InputDigest: matcher.WorkInputDigest(source), FamilyDigest: llmwork.Digest("synthetic cross-build"), Payload: body, DailyLimit: 1, MonthlyLimit: 20})
	require.NoError(t, err)
	lease, err := work.Claim(ctx, "first-build")
	require.NoError(t, err)
	require.NotNil(t, lease)
	err = h.Handle(llmwork.WithExecution(ctx, work, *lease), lease.Task)
	require.ErrorIs(t, err, llmwork.ErrDeferred)
	var deferred *llmwork.DeferredError
	var value llmwork.DeferredError
	if errors.As(err, &deferred) {
		value = *deferred
	} else {
		require.True(t, errors.As(err, &value))
	}
	require.NoError(t, work.Finish(ctx, *lease, "deferred", value.Reason, value.RetryAfter))
	require.EqualValues(t, 1, extracts.Load())
	require.Zero(t, reranks.Load())
	var priority, monthly int
	require.NoError(t, pool.QueryRow(ctx, `SELECT priority FROM llm_work_tasks`).Scan(&priority))
	require.Equal(t, 100, priority)
	// Fixture UTC rollover makes the existing positive pre-dispatch denial due.
	// It does not refund the paid extraction or reset the monthly allowance.
	_, err = pool.Exec(ctx, `UPDATE llm_request_budgets SET day_start=day_start-1;UPDATE llm_work_tasks SET retry_after=clock_timestamp()-interval '1 second';UPDATE llm_capture_dispatch_attempts SET retry_after=clock_timestamp()-interval '1 second' WHERE state='no_dispatch'`)
	require.NoError(t, err)
	version.GitTag = "synthetic-build-b"
	nextMatcher := newMatcher()
	require.Equal(t, lease.Task.PolicyDigest, llmwork.Digest(nextMatcher.WorkPolicy(payload)))
	next, err := work.Claim(ctx, "next-build")
	require.NoError(t, err)
	require.NotNil(t, next)
	require.Greater(t, next.Generation, lease.Generation)
	require.NoError(t, h.Handle(llmwork.WithExecution(ctx, work, *next), next.Task))
	require.EqualValues(t, 1, extracts.Load())
	require.EqualValues(t, 1, reranks.Load())
	require.NoError(t, pool.QueryRow(ctx, `SELECT monthly_calls FROM llm_request_budgets WHERE scope='matcher'`).Scan(&monthly))
	require.Equal(t, 2, monthly)
	var id, state string
	require.NoError(t, pool.QueryRow(ctx, `SELECT content_id FROM torrent_contents`).Scan(&id))
	require.Equal(t, "42", id)
	require.NoError(t, pool.QueryRow(ctx, `SELECT state FROM llm_work_tasks`).Scan(&state))
	require.Equal(t, "completed", state)
	var originalBuild string
	require.NoError(t, pool.QueryRow(ctx, `SELECT build_identity FROM llm_evaluation_captures c JOIN llm_evaluation_capture_results r USING(capture_key) WHERE c.task='matcher_extract'`).Scan(&originalBuild))
	require.Equal(t, "synthetic-build-a", originalBuild)
}

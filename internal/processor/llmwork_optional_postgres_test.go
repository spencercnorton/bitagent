package processor

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/spencercnorton/bitagent/internal/classifier"
	"github.com/spencercnorton/bitagent/internal/classifier/classification"
	"github.com/spencercnorton/bitagent/internal/classifier/contentfilter"
	"github.com/spencercnorton/bitagent/internal/classifier/llmmatch"
	"github.com/spencercnorton/bitagent/internal/database/search"
	"github.com/spencercnorton/bitagent/internal/lazy"
	"github.com/spencercnorton/bitagent/internal/llmcapture"
	"github.com/spencercnorton/bitagent/internal/llmwork"
	"github.com/spencercnorton/bitagent/internal/model"
	"github.com/spencercnorton/bitagent/internal/tmdb"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

func resetOptionalHarness(t *testing.T) (*DeferredApplyHandler, *llmwork.Store, *pgxpool.Pool, model.Torrent, *llmcapture.Recorder, llmcapture.DispatchControl) {
	t.Helper()
	h, work, _, _, pool, source, _ := deferredTypeHarness(t, nil, false)
	_, err := pool.Exec(context.Background(), `DELETE FROM llm_work_events;DELETE FROM llm_work_tasks`)
	require.NoError(t, err)
	captureStore := llmcapture.NewPostgresStore(h.Pool)
	cfg := llmcapture.NewDefaultConfig()
	cfg.Enabled = true
	recorder, err := llmcapture.NewRecorder(cfg, typeEnrichmentPublicPrivacy{}, captureStore)
	require.NoError(t, err)
	dispatch := llmcapture.NewPostgresDispatchController(captureStore, true)
	work.SetDispatch(dispatch)
	return h, work, pool, source, recorder, dispatch
}
func enqueueOptional(t *testing.T, work *llmwork.Store, source model.Torrent, kind llmwork.Kind, policy, input, payload any) *llmwork.Lease {
	t.Helper()
	body, err := json.Marshal(payload)
	require.NoError(t, err)
	_, err = work.Enqueue(context.Background(), llmwork.Draft{Kind: kind, InfoHash: source.InfoHash.Bytes(), SourceDigest: llmwork.SourceDigest(source), PolicyDigest: llmwork.Digest(policy), InputDigest: input.([]byte), FamilyDigest: llmwork.Digest("synthetic optional"), Payload: body, DailyLimit: 20, MonthlyLimit: 20})
	require.NoError(t, err)
	lease, err := work.Claim(context.Background(), "optional-fixture")
	require.NoError(t, err)
	require.NotNil(t, lease)
	return lease
}

func TestPostgresDeferredMappedLanguageReviewAndChangedMetadata(t *testing.T) {
	for _, change := range []bool{false, true} {
		t.Run(fmt.Sprint(change), func(t *testing.T) {
			h, work, pool, source, recorder, dispatch := resetOptionalHarness(t)
			ctx := context.Background()
			_, err := pool.Exec(ctx, `INSERT INTO content(type,source,id,title,release_year,created_at,updated_at)VALUES('movie','tmdb','42','Amber Signal',2026,now(),now());UPDATE torrent_contents SET content_type='movie',content_source='tmdb',content_id='42'`)
			require.NoError(t, err)
			var calls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				if change {
					_, e := pool.Exec(ctx, `UPDATE content SET original_language='fr' WHERE id='42'`)
					require.NoError(t, e)
				}
				_ = json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{"finish_reason": "stop", "message": map[string]any{"role": "assistant", "content": `{"is_english":false,"confidence":0.99,"reason":"synthetic"}`}}}})
			}))
			defer server.Close()
			cfg := contentfilter.NewDefaultConfig()
			cfg.Enabled = true
			cfg.LLMEnabled = true
			cfg.LLMAction = "review"
			cfg.LLMEnforce = "true"
			cfg.LLMBaseURL = server.URL
			cfg.LLMApiStyle = "chat"
			cfg.LLMDailyBudget = 20
			cfg.LLMMonthlyBudget = 20
			client := contentfilter.NewOpenAIClientWithPolicy("local", cfg.LLMModel, server.URL, cfg.LLMApiStyle, cfg.LLMPromptVersion, "", cfg.LLMMaxOutputTokens, time.Second)
			filter := contentfilter.NewWithLLMAdmission(cfg, client, contentfilter.LLMCallbacks{}, contentfilter.Admission{Budget: llmmatch.NewPostgresContentFilterCallBudget(h.Pool), Capture: recorder, Dispatch: dispatch})
			filter.SetWork(work)
			h.Filter = filter
			cl := classification.Result{ContentAttributes: classification.ContentAttributes{ContentType: model.NewNullContentType(model.ContentTypeMovie)}, Content: &model.Content{Type: model.ContentTypeMovie, Source: "tmdb", ID: "42"}}
			input := torrentToFilterInput(source, cl)
			p := contentfilter.WorkPayload{Input: input, Source: contentfilter.AuditSource{InfoHash: source.InfoHash.Bytes(), GroupKey: []byte("movie\x00tmdb\x0042")}}
			contract, err := filter.EvaluationCapture(input)
			require.NoError(t, err)
			lease := enqueueOptional(t, work, source, llmwork.Language, filter.WorkPolicy(), llmwork.Digest(contract.ModelInputJSON), p)
			err = h.Handle(llmwork.WithExecution(ctx, work, *lease), lease.Task)
			if change {
				require.ErrorIs(t, err, llmwork.ErrObsolete)
				var n int
				require.NoError(t, pool.QueryRow(ctx, `SELECT count(*) FROM llm_work_applications`).Scan(&n))
				require.Zero(t, n)
				return
			}
			require.NoError(t, err)
			require.EqualValues(t, 1, calls.Load())
			var n int
			require.NoError(t, pool.QueryRow(ctx, `SELECT count(*) FROM torrent_tags WHERE name='llm-language-review'`).Scan(&n))
			require.Equal(t, 1, n)
			_, err = pool.Exec(ctx, `DELETE FROM llm_evaluation_captures;UPDATE llm_work_tasks SET payload='{}'::jsonb`)
			require.NoError(t, err)
			d, err := filter.DecideAudited(llmwork.WithSourceTorrent(ctx, source), input, p.Source)
			require.NoError(t, err)
			require.True(t, d.Review)
			require.True(t, d.Allow)
			require.EqualValues(t, 1, calls.Load())
		})
	}
}

func TestPostgresDeferredMatcherUsesFinalPolicyAndPreservesIdentity(t *testing.T) {
	h, work, pool, source, recorder, dispatch := resetOptionalHarness(t)
	ctx := context.Background()
	_, err := pool.Exec(ctx, `INSERT INTO content(type,source,id,title,release_year,release_date,tsv,created_at,updated_at)VALUES('movie','tmdb','42','Amber Signal',2026,make_date(2026,1,1),to_tsvector('simple','Amber Signal'),now(),now());UPDATE torrent_contents SET content_type='movie'`)
	require.NoError(t, err)
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		call := calls.Add(1)
		answer := `{"title":"Amber Signal","year":2026,"type":"movie","season":0,"episode":0,"is_anime":false,"is_pack":false,"is_adult":false,"english":"unknown"}`
		if call > 1 {
			answer = `{"tmdb_id":42,"confidence":0.99}`
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{"finish_reason": "stop", "message": map[string]any{"role": "assistant", "content": answer}}}})
	}))
	defer server.Close()
	cfg := llmmatch.NewDefaultConfig()
	cfg.Enabled = true
	cfg.EnableLive = true
	cfg.Endpoint = server.URL
	cfg.APIKey = "local"
	cfg.DailyCallLimit = 20
	cfg.MonthlyCallLimit = 20
	matcher := llmmatch.NewClientWithBudget(cfg, typeEnrichmentPublicPrivacy{}, llmmatch.NewMetrics(), zap.NewNop().Sugar(), recorder, llmmatch.NewPostgresCallBudget(h.Pool)).WithDispatchControl(dispatch)
	matcher.SetWork(work, h.Classifier)
	observer := classifier.NewMatchDecisionObserver(recorder, matcher)
	backend, err := h.Search.Get()
	require.NoError(t, err)
	runner, err := classifier.New(classifier.Params{Config: h.Classifier, Search: lazy.New(func() (search.Search, error) { return backend, nil }), TmdbClient: lazy.New(func() (tmdb.Client, error) { return nil, nil }), LlmMatch: matcher, MatchDecisionObserver: observer}).Runner.Get()
	require.NoError(t, err)
	h.Runner = lazy.New(func() (classifier.Runner, error) { return runner, nil })
	h.Matcher = matcher
	h.Observer = observer
	p := llmmatch.WorkPayload{Type: model.NewNullContentType(model.ContentTypeMovie)}
	lease := enqueueOptional(t, work, source, llmwork.Matcher, matcher.WorkPolicy(p), matcher.WorkInputDigest(source), p)
	require.NoError(t, h.Handle(llmwork.WithExecution(ctx, work, *lease), lease.Task))
	require.EqualValues(t, 2, calls.Load())
	var id string
	require.NoError(t, pool.QueryRow(ctx, `SELECT content_id FROM torrent_contents`).Scan(&id))
	require.Equal(t, "42", id)
	_, err = pool.Exec(ctx, `DELETE FROM llm_evaluation_captures;UPDATE llm_work_tasks SET payload='{}'::jsonb`)
	require.NoError(t, err)
	a, err := work.PreservePolicy(ctx, llmwork.Matcher, source, func(a llmwork.ApplicationSnapshot) any {
		return matcher.WorkPolicy(llmmatch.WorkPayload{Type: a.ContentType})
	})
	require.NoError(t, err)
	require.NotNil(t, a)
	require.Equal(t, "42", a.Result().Content.ID)
	require.EqualValues(t, 2, calls.Load())
}

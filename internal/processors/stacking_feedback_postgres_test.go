package processors

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/spencercnorton/bitagent/internal/classifier/contentfilter"
	"github.com/spencercnorton/bitagent/internal/evidence"
	"github.com/spencercnorton/bitagent/internal/lazy"
	v1 "github.com/spencercnorton/bitagent/processorapi/v1"
	"github.com/stretchr/testify/require"
)

func TestPostgresMultiCapabilitySinglePassProcessor(t *testing.T) {
	s, p, _ := fixture(t)
	h := seedTorrent(t, p, 101, nil)
	_, err := p.Exec(testContext, `INSERT INTO content(type,source,id,title,release_year,tsv,created_at,updated_at) VALUES('movie','tmdb','884422','Synthetic Amber',2024,to_tsvector('simple','Synthetic Amber 2024'),now(),now())`)
	require.NoError(t, err)

	var calls atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v1/manifest":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"contract":     v1.Contract,
				"processor":    map[string]string{"id": "omni-processor"},
				"capabilities": []string{"classify", "extract", "language", "match", "policy"},
			})
		case "/v1/status":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"contract":    v1.Contract,
				"processor":   map[string]string{"id": "omni-processor"},
				"state":       "ready",
				"instance_id": "omni-1",
				"started_at":  "2026-01-01T00:00:00Z",
			})
		case "/v1/process":
			calls.Add(1)
			var j v1.Job
			_ = json.NewDecoder(r.Body).Decode(&j)
			// Return a minimal stateless webhook response (Binding omitted) proposing all 5 capabilities in 1 step.
			_ = json.NewEncoder(w).Encode(map[string]any{
				"outcome": "completed",
				"proposal": map[string]any{
					"content_type": "movie",
					"extraction": map[string]any{
						"title":            "Synthetic Amber",
						"year":             2024,
						"video_resolution": "1080p",
						"release_group":    "GROUP",
					},
					"language": map[string]any{
						"audio":     []string{"en"},
						"subtitles": []string{"en"},
						"evidence":  "metadata_claim",
					},
					"match": map[string]any{
						"source": "tmdb",
						"id":     "884422",
					},
					"add_tags": []string{"omni-verified"},
					"weights":  map[string]float64{"confidence": 0.98},
				},
			})
		}
	}))
	defer srv.Close()

	_, err = s.Register(testContext, "omni-processor", v1.RegistryWrite{
		Endpoint:      srv.URL,
		Mode:          "apply",
		State:         "active",
		Configuration: json.RawMessage(`{}`),
	})
	require.NoError(t, err)

	admitted, err := s.Scan(testContext)
	require.NoError(t, err)
	require.Equal(t, 1, admitted)

	// Single RunOne executes all 5 capabilities in 1 pass!
	run(t, s)
	require.EqualValues(t, 1, calls.Load())

	var kind, cid string
	require.NoError(t, p.QueryRow(testContext, `SELECT content_type,content_id FROM torrent_contents WHERE info_hash=$1`, h).Scan(&kind, &cid))
	require.Equal(t, "movie", kind)
	require.Equal(t, "884422", cid)

	var tagCount int
	require.NoError(t, p.QueryRow(testContext, `SELECT count(*) FROM torrent_tags WHERE info_hash=$1 AND name='omni-verified'`, h).Scan(&tagCount))
	require.Equal(t, 1, tagCount)
}

func TestPostgresStackingAndDynamicSelectors(t *testing.T) {
	s, p, _ := fixture(t)

	hBlocked := seedTorrent(t, p, 102, nil)
	_, err := p.Exec(testContext, `UPDATE torrents SET name='Blocked.Camrip.2024.CAMRIP.mkv' WHERE info_hash=$1`, hBlocked)
	require.NoError(t, err)

	hConfident := seedTorrent(t, p, 103, nil)
	_, err = p.Exec(testContext, `UPDATE torrents SET name='Confident.Movie.2024.1080p.WEB-DL.H264-SOLID.mkv' WHERE info_hash=$1`, hConfident)
	require.NoError(t, err)

	hAmbiguous := seedTorrent(t, p, 104, nil)
	_, err = p.Exec(testContext, `UPDATE torrents SET name='Ambiguous.Release.2024.1080p.WEB-DL.H264.mkv' WHERE info_hash=$1`, hAmbiguous)
	require.NoError(t, err)

	// Step 1 (Priority 10): Built-in filter list that blocks CAMRIP
	_, err = s.Register(testContext, "fast-filter", v1.RegistryWrite{
		Driver:        v1.DriverBuiltinFilter,
		Priority:      10,
		Mode:          "apply",
		State:         "active",
		Configuration: json.RawMessage(`{"block_patterns":["(?i)\\bCAMRIP\\b"],"reason":"camrip_blocked"}`),
	})
	require.NoError(t, err)

	// Step 2 (Priority 20): Fast classifier (only runs when Excluded == false)
	notExcluded := false
	fastClassifier := newService(t, "fast-classifier", []v1.Stage{v1.Classify}, func(j v1.Job) v1.Result {
		u := 0.05
		if bytes.Contains([]byte(j.Input.Name), []byte("Ambiguous")) {
			u = 0.75
		}
		return v1.Result{
			Binding:     j.Binding,
			Outcome:     v1.Completed,
			Proposal:    &v1.Proposal{ContentType: "movie"},
			Uncertainty: &u,
		}
	})
	wFast := fastClassifier.write()
	wFast.Priority = 20
	wFast.Selector = &v1.Selector{Excluded: &notExcluded}
	_, err = s.Register(testContext, "fast-classifier", wFast)
	require.NoError(t, err)

	// Step 3 (Priority 30): Heavy extractor fallback (only runs when Excluded == false, ContentTypes == ["movie"], MinUncertainty >= 0.50)
	minUnc := 0.50
	heavyFallback := newService(t, "heavy-fallback", []v1.Stage{v1.Extract}, func(j v1.Job) v1.Result {
		return v1.Result{
			Binding:  j.Binding,
			Outcome:  v1.Completed,
			Proposal: &v1.Proposal{Extraction: &v1.Extraction{ReleaseGroup: "HEAVYLLM"}},
		}
	})
	wHeavy := heavyFallback.write()
	wHeavy.Priority = 30
	wHeavy.Selector = &v1.Selector{
		Excluded:       &notExcluded,
		ContentTypes:   []string{"movie"},
		MinUncertainty: &minUnc,
	}
	_, err = s.Register(testContext, "heavy-fallback", wHeavy)
	require.NoError(t, err)

	admitted, err := s.Scan(testContext)
	require.NoError(t, err)
	require.Equal(t, 3, admitted)

	for range 9 {
		run(t, s)
	}

	// Verify fast-classifier was only called 2 times (skipped for hBlocked!)
	require.EqualValues(t, 2, fastClassifier.calls.Load())
	// Verify heavy-fallback was only called 1 time (skipped for hBlocked AND hConfident!)
	require.EqualValues(t, 1, heavyFallback.calls.Load())

	var exclCount int
	require.NoError(t, p.QueryRow(testContext, `SELECT count(*) FROM processor_exclusions WHERE info_hash=$1 AND reason='camrip_blocked'`, hBlocked).Scan(&exclCount))
	require.Equal(t, 1, exclCount)

	var grpConfident, grpAmbiguous string
	require.NoError(t, p.QueryRow(testContext, `SELECT COALESCE(release_group,'') FROM torrent_contents WHERE info_hash=$1`, hConfident).Scan(&grpConfident))
	require.Equal(t, "SOLID", grpConfident)

	require.NoError(t, p.QueryRow(testContext, `SELECT COALESCE(release_group,'') FROM torrent_contents WHERE info_hash=$1`, hAmbiguous).Scan(&grpAmbiguous))
	require.Equal(t, "HEAVYLLM", grpAmbiguous)
}

func TestPostgresBuiltinDrivers(t *testing.T) {
	s, p, _ := fixture(t)

	h := seedTorrent(t, p, 105, nil)
	_, err := p.Exec(testContext, `UPDATE torrents SET name='Anime.Show.2024.1080p.WEB-DL.mkv' WHERE info_hash=$1; UPDATE torrent_contents SET seeders=15,leechers=3 WHERE info_hash=$1`, h)
	require.NoError(t, err)

	llmServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{
				{
					"message": map[string]string{
						"content": `{"outcome":"completed","proposal":{"content_type":"tv_show","extraction":{"title":"Anime Show","season":1,"episodes":[1]}},"uncertainty":0.1,"reasons":["llm_classified"]}`,
					},
				},
			},
		})
	}))
	defer llmServer.Close()

	// 1. builtin/regex-weights (Priority 10)
	_, err = s.Register(testContext, "regex-scorer", v1.RegistryWrite{
		Driver:        v1.DriverBuiltinRegex,
		Priority:      10,
		Mode:          "apply",
		State:         "active",
		Configuration: json.RawMessage(`{"rules":[{"id":"webdl-1080p","pattern":"(?i)1080p\\.WEB-DL","dimension":"quality","weight":2.5,"add_tags":["hd-webdl"],"release_group":"REGEXGRP"}]}`),
	})
	require.NoError(t, err)

	// 2. builtin/tracker-liveness (Priority 20)
	_, err = s.Register(testContext, "swarm-liveness", v1.RegistryWrite{
		Driver:        v1.DriverBuiltinLiveness,
		Priority:      20,
		Mode:          "apply",
		State:         "active",
		Configuration: json.RawMessage(`{"min_seeders":5,"tag_live":"swarm-alive","weight_dimension":"liveness","live_weight":1.0}`),
	})
	require.NoError(t, err)

	// 3. builtin/llm (Priority 30) - gated by score_thresholds from prior steps!
	minQual := 2.0
	_, err = s.Register(testContext, "builtin-llm", v1.RegistryWrite{
		Driver:   v1.DriverBuiltinLLM,
		Endpoint: llmServer.URL,
		Priority: 30,
		Mode:     "apply",
		State:    "active",
		Selector: &v1.Selector{
			RequiredTags:    []string{"hd-webdl", "swarm-alive"},
			ScoreThresholds: []v1.ScoreThreshold{{Dimension: "quality", Min: &minQual}},
		},
		Configuration: json.RawMessage(`{"model":"qwen2.5-7b-instruct","capabilities":["classify","extract"]}`),
	})
	require.NoError(t, err)

	require.NoError(t, s.Refresh(testContext))
	st, err := s.Status(testContext)
	require.NoError(t, err)
	require.Len(t, st.Processors, 3)
	for _, proc := range st.Processors {
		require.Equal(t, "healthy", proc.Health)
		require.NotEmpty(t, proc.Driver)
	}

	admitted, err := s.Scan(testContext)
	require.NoError(t, err)
	require.Equal(t, 1, admitted)

	for range 3 {
		run(t, s)
	}

	var kind, grp string
	require.NoError(t, p.QueryRow(testContext, `SELECT content_type,release_group FROM torrent_contents WHERE info_hash=$1`, h).Scan(&kind, &grp))
	require.Equal(t, "tv_show", kind)
	require.Equal(t, "REGEXGRP", grp)

	var tags []string
	rows, err := p.Query(testContext, `SELECT name FROM torrent_tags WHERE info_hash=$1 ORDER BY name`, h)
	require.NoError(t, err)
	for rows.Next() {
		var tName string
		require.NoError(t, rows.Scan(&tName))
		tags = append(tags, tName)
	}
	rows.Close()
	require.Equal(t, []string{"hd-webdl", "swarm-alive"}, tags)
}

func TestPostgresCoreOperationsAPI(t *testing.T) {
	s, p, _ := fixture(t)
	s.cfg.AdminToken = "core-ops-token"
	s.filter = contentfilter.New(contentfilter.Config{Enabled: true, Enforce: true, BlockedExtensions: []string{"exe"}})

	h := seedTorrent(t, p, 106, "movie")
	_, err := p.Exec(testContext, `UPDATE torrent_contents SET seeders=42,leechers=7 WHERE info_hash=$1;
  INSERT INTO content(type,source,id,title,release_year,tsv,created_at,updated_at) VALUES('movie','tmdb','777888','Synthetic Amber',2024,to_tsvector('simple','Synthetic Amber 2024'),now(),now())`, h)
	require.NoError(t, err)

	gin.SetMode(gin.TestMode)
	router := gin.New()
	require.NoError(t, s.Apply(router))

	post := func(path string, payload any, out any) int {
		t.Helper()
		b, err := json.Marshal(payload)
		require.NoError(t, err)
		req := httptest.NewRequest("POST", "/api/processors/v1"+path, bytes.NewReader(b))
		req.Header.Set("Authorization", "Bearer core-ops-token")
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)
		if w.Code == 200 && out != nil {
			require.NoError(t, v1.Decode(w.Body.Bytes(), out))
		}
		return w.Code
	}

	var parseResp v1.CoreParseResponse
	require.Equal(t, 200, post("/core/parse", v1.CoreParseRequest{
		Name:        "Synthetic.Amber.2024.2160p.BluRay.REMUX.HEVC-GRP.mkv",
		ContentType: "movie",
	}, &parseResp))
	require.Equal(t, "Synthetic Amber", parseResp.Extraction.Title)
	require.Equal(t, 2024, parseResp.Extraction.Year)
	require.Equal(t, "V2160p", parseResp.Extraction.VideoResolution)
	require.Equal(t, "BluRay", parseResp.Extraction.VideoSource)
	require.Equal(t, "REMUX", parseResp.Extraction.VideoModifier)
	require.Equal(t, "HEVC", parseResp.Extraction.VideoCodec)
	require.Equal(t, "GRP", parseResp.Extraction.ReleaseGroup)

	var searchResp v1.CoreSearchResponse
	require.Equal(t, 200, post("/core/search", v1.CoreSearchRequest{
		Title:       "Synthetic Amber",
		Year:        2024,
		ContentType: "movie",
	}, &searchResp))
	require.Len(t, searchResp.Candidates, 1)
	require.Equal(t, "777888", searchResp.Candidates[0].ID)

	var liveResp v1.CoreLivenessResponse
	require.Equal(t, 200, post("/core/liveness", v1.CoreLivenessRequest{
		InfoHash: hex.EncodeToString(h),
	}, &liveResp))
	require.Equal(t, "live", liveResp.Liveness.Observation)
	require.NotNil(t, liveResp.Liveness.Seeders)
	require.Equal(t, 42, *liveResp.Liveness.Seeders)

	var polResp v1.CorePolicyResponse
	require.Equal(t, 200, post("/core/policy", v1.CorePolicyRequest{
		Name:             "Malware.Setup.exe",
		ContentType:      "software",
		PrimaryExtension: "exe",
		AllExtensions:    []string{"exe"},
	}, &polResp))
	require.False(t, polResp.Allowed)
	require.False(t, polResp.FilterAllowed)
}

func TestPostgresBiDirectionalFeedbackAndOnlineLearning(t *testing.T) {
	s, p, _ := fixture(t)
	s.cfg.AdminToken = "feedback-admin-token"
	h := seedTorrent(t, p, 107, nil)

	var pushedBatches atomic.Int64
	teacherSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v1/manifest":
			_ = json.NewEncoder(w).Encode(v1.Manifest{
				Contract:            v1.Contract,
				Processor:           v1.Identity{ID: "teacher-http", Version: "1.0.0", ModelVersion: "v1", PolicyVersion: "p1", ArtifactDigest: "none"},
				Capabilities:        []v1.Stage{v1.Classify},
				ConfigurationSchema: json.RawMessage(`{"type":"object"}`),
				Qualification:       "qualified",
			})
		case "/v1/status":
			_ = json.NewEncoder(w).Encode(v1.ServiceStatus{
				Contract:   v1.Contract,
				Processor:  v1.Identity{ID: "teacher-http", Version: "1.0.0", ModelVersion: "v1", PolicyVersion: "p1", ArtifactDigest: "none"},
				State:      "ready",
				InstanceID: "teacher-1",
				StartedAt:  "2026-01-01T00:00:00Z",
			})
		case "/v1/process":
			var j v1.Job
			_ = json.NewDecoder(r.Body).Decode(&j)
			_ = json.NewEncoder(w).Encode(v1.Result{
				Binding:  j.Binding,
				Outcome:  v1.Completed,
				Proposal: &v1.Proposal{ContentType: "movie"},
			})
		case "/v1/feedback":
			var batch v1.FeedbackBatch
			require.NoError(t, v1.Decode(mustReadAll(t, r), &batch))
			require.NotEmpty(t, batch.Events)
			pushedBatches.Add(int64(len(batch.Events)))
			_ = json.NewEncoder(w).Encode(v1.FeedbackAck{
				Contract:     v1.FeedbackContract,
				Accepted:     len(batch.Events),
				LearnedState: json.RawMessage(`{"teacher_epoch":2}`),
			})
		}
	}))
	defer teacherSrv.Close()

	// Student: builtin/regex-weights at Priority 10 with online_calibrate
	_, err := s.Register(testContext, "regex-student", v1.RegistryWrite{
		Driver:   v1.DriverBuiltinRegex,
		Priority: 10,
		Mode:     "apply",
		State:    "active",
		Learning: &v1.LearningConfig{
			Enabled: true,
			Mode:    "online_calibrate",
			AcceptedTiers:   []string{v1.FeedbackTierOperationalReceipt, v1.FeedbackTierTeacherConsensus, v1.FeedbackTierVerifiedImport, v1.FeedbackTierHumanGold},
		},
		Configuration: json.RawMessage(`{"rules":[{"id":"amber-rule","pattern":"(?i)Amber","dimension":"score","weight":1.0,"content_type":"movie"}]}`),
	})
	require.NoError(t, err)

	// Teacher: HTTP processor at Priority 20 with PushEnabled
	_, err = s.Register(testContext, "teacher-http", v1.RegistryWrite{
		Endpoint: teacherSrv.URL,
		Priority: 20,
		Mode:     "apply",
		State:    "active",
		Learning: &v1.LearningConfig{
			Enabled:     true,
			Mode:        "online_calibrate",
			AcceptedTiers:       []string{v1.FeedbackTierOperationalReceipt, v1.FeedbackTierVerifiedImport, v1.FeedbackTierHumanGold},
			PushEnabled: true,
		},
		Configuration: json.RawMessage(`{}`),
	})
	require.NoError(t, err)

	_, err = s.Scan(testContext)
	require.NoError(t, err)
	run(t, s)
	run(t, s)

	// Check that operational_receipt and teacher_consensus calibrated regex-student's learned_state!
	var studentLearned []byte
	require.NoError(t, p.QueryRow(testContext, `SELECT learned_state FROM processor_registry WHERE id='regex-student'`).Scan(&studentLearned))
	var state regexLearnedState
	require.NoError(t, json.Unmarshal(studentLearned, &state))
	require.GreaterOrEqual(t, state.FeedbackCount, int64(2))
	require.Greater(t, state.RuleAdjustments["amber-rule"], 0.0)

	// Tier 3 (verified_import): Wire evidence.Store post-insert hook and insert a Radarr webhook_import
	evStore := evidence.NewStore(lazy.New(func() (*pgxpool.Pool, error) { return p, nil }))
	evStore.AddPostInsertHook(s.HandleEvidence)
	err = evStore.Insert(testContext, evidence.Evidence{
		InfoHash:       h,
		Source:         evidence.SourceRadarr,
		Kind:           evidence.KindWebhookImport,
		SourceInstance: "radarr-main",
		SourceObjectID: "import-107",
		MediaID:        "tmdb:554433",
		MediaType:      evidence.MediaTypeMovie,
		Title:          "Synthetic Amber",
		Strength:       90,
		ObservedAt:     time.Now(),
		RawPayload:     json.RawMessage(`{}`),
	})
	require.NoError(t, err)

	// Deliver pending feedback to teacher-http over HTTP push (/v1/feedback)
	delivered, err := s.DeliverFeedback(testContext)
	require.NoError(t, err)
	require.GreaterOrEqual(t, delivered, 2)
	require.GreaterOrEqual(t, pushedBatches.Load(), int64(2))

	var teacherLearned string
	require.NoError(t, p.QueryRow(testContext, `SELECT learned_state::text FROM processor_registry WHERE id='teacher-http'`).Scan(&teacherLearned))
	require.JSONEq(t, `{"teacher_epoch":2}`, teacherLearned)

	// Tier 4 (human_gold) + Pull pagination + Cursor Ack via HTTP API
	gin.SetMode(gin.TestMode)
	router := gin.New()
	require.NoError(t, s.Apply(router))

	goldBody, _ := json.Marshal(v1.FeedbackWrite{
		InfoHash:    hex.EncodeToString(h),
		ProcessorID: "regex-student",
		Tier:        v1.FeedbackTierHumanGold,
		GroundTruth: v1.FeedbackGroundTruth{
			Outcome:     "operator_override",
			ContentType: "movie",
			Source:      "operator_audit",
		},
	})
	w := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/api/processors/v1/feedback", bytes.NewReader(goldBody))
	req.Header.Set("Authorization", "Bearer feedback-admin-token")
	router.ServeHTTP(w, req)
	require.Equal(t, 200, w.Code)

	w = httptest.NewRecorder()
	req = httptest.NewRequest("GET", "/api/processors/v1/feedback?processor_id=regex-student&consumer_id=trainer-1&limit=10", nil)
	req.Header.Set("Authorization", "Bearer feedback-admin-token")
	router.ServeHTTP(w, req)
	require.Equal(t, 200, w.Code)
	var fbPage v1.FeedbackPage
	require.NoError(t, v1.Decode(w.Body.Bytes(), &fbPage))
	require.Len(t, fbPage.Items, 4) // operational_receipt, teacher_consensus, verified_import, human_gold
	require.NotZero(t, fbPage.NextCursor)

	// Acknowledge cursor for consumer trainer-1
	ackBody, _ := json.Marshal(v1.FeedbackCursorAck{
		ProcessorID: "regex-student",
		ConsumerID:  "trainer-1",
		LastSeq:     fbPage.NextCursor,
	})
	w = httptest.NewRecorder()
	req = httptest.NewRequest("POST", "/api/processors/v1/feedback/ack", bytes.NewReader(ackBody))
	req.Header.Set("Authorization", "Bearer feedback-admin-token")
	router.ServeHTTP(w, req)
	require.Equal(t, 200, w.Code)

	// Pulling again with consumer_id=trainer-1 and default after_seq=0 resumes from acked cursor and returns 0 items
	w = httptest.NewRecorder()
	req = httptest.NewRequest("GET", "/api/processors/v1/feedback?processor_id=regex-student&consumer_id=trainer-1&limit=10", nil)
	req.Header.Set("Authorization", "Bearer feedback-admin-token")
	router.ServeHTTP(w, req)
	require.Equal(t, 200, w.Code)
	var emptyPage v1.FeedbackPage
	require.NoError(t, v1.Decode(w.Body.Bytes(), &emptyPage))
	require.Empty(t, emptyPage.Items)
}

func TestPostgresDecodeRowNonNullScalarsAndReprocessInterval(t *testing.T) {
	s, p, _ := fixture(t)
	h := seedTorrent(t, p, 108, nil)
	// Populate non-null scalar columns on torrent_contents and torrent_hints that previously failed decodeRow
	_, err := p.Exec(testContext, `UPDATE torrent_contents SET seeders=10,leechers=2,files_count=1,anime_absolute_episode=12,release_date='2024-05-10' WHERE info_hash=$1;
  INSERT INTO torrent_hints(info_hash,content_type,created_at,updated_at) VALUES($1,'movie',now(),now())`, h)
	require.NoError(t, err)
	// Remove hint so classify processor can enrich, while keeping non-null seeders/leechers/files_count/anime_absolute_episode/release_date
	_, err = p.Exec(testContext, `DELETE FROM torrent_hints WHERE info_hash=$1`, h)
	require.NoError(t, err)

	register(t, s, newService(t, "scalar-check", []v1.Stage{v1.Classify}, func(j v1.Job) v1.Result { return result(j, "movie") }))
	_, err = s.Scan(testContext)
	require.NoError(t, err)
	run(t, s)

	var kind string
	require.NoError(t, p.QueryRow(testContext, `SELECT content_type FROM torrent_contents WHERE info_hash=$1`, h).Scan(&kind))
	require.Equal(t, "movie", kind)

	// Verify queue_jobs archival_duration created by Reprocess scans cleanly into model.Duration
	page, err := s.Jobs(testContext, 1, "", "")
	require.NoError(t, err)
	w := reprocessWrite(t, s, page.Items[0].JobID, "9")
	repStatus, err := s.Reprocess(testContext, page.Items[0].JobID, w)
	require.NoError(t, err)
	var archivalText string
	require.NoError(t, p.QueryRow(testContext, `SELECT archival_duration::text FROM queue_jobs WHERE id=$1`, repStatus.CoreJobID).Scan(&archivalText))
	require.NotEmpty(t, archivalText)
}

func mustReadAll(t *testing.T, r *http.Request) []byte {
	t.Helper()
	var buf bytes.Buffer
	_, err := buf.ReadFrom(r.Body)
	require.NoError(t, err)
	return buf.Bytes()
}


func TestPostgresStrictV1MultiStageHTTPProcessorInStack(t *testing.T) {
	s, p, _ := fixture(t)
	hAdult := seedTorrent(t, p, 210, nil)
	_, err := p.Exec(testContext, `UPDATE torrents SET name='Adult.Release.XXX.1080p.MP4' WHERE info_hash=$1`, hAdult)
	require.NoError(t, err)
	_, err = p.Exec(testContext, `INSERT INTO torrent_tracker_seeds(info_hash,tracker_known,seeders,leechers,checked_at) VALUES($1,true,15,2,now())`, hAdult)
	require.NoError(t, err)

	// Priority 10: builtin/tracker-liveness (adds liveness proposal + tracker:alive tag)
	_, err = s.Register(testContext, "tracker-liveness", v1.RegistryWrite{
		Driver:        v1.DriverBuiltinLiveness,
		Priority:      10,
		Mode:          "apply",
		State:         "active",
		Configuration: json.RawMessage(`{"tag_live":"tracker:alive","weight_dimension":"liveness","live_weight":1.5}`),
	})
	require.NoError(t, err)

	// Priority 100: strict v1.0 HTTP service that rejects unknown keys with 400
	// and requires separate classify + policy stage calls.
	strictSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		id := v1.Identity{ID: "strict-v1-classifier", Version: "1.1.1", ModelVersion: "m1", PolicyVersion: "p1", ArtifactDigest: "none"}
		switch r.URL.Path {
		case "/v1/manifest":
			_ = json.NewEncoder(w).Encode(v1.Manifest{
				Contract:            v1.Contract,
				Processor:           id,
				Capabilities:        []v1.Stage{v1.Classify, v1.Policy},
				ConfigurationSchema: json.RawMessage(`{"type":"object","additionalProperties":false}`),
				Qualification:       "unqualified",
			})
		case "/v1/status":
			_ = json.NewEncoder(w).Encode(v1.ServiceStatus{
				Contract:   v1.Contract,
				Processor:  id,
				State:      "ready",
				InstanceID: "strict-1",
				StartedAt:  "2026-01-01T00:00:00Z",
			})
		case "/v1/process":
			var raw map[string]any
			_ = json.NewDecoder(r.Body).Decode(&raw)
			if _, hasCaps := raw["capabilities"]; hasCaps {
				w.WriteHeader(http.StatusBadRequest)
				_ = json.NewEncoder(w).Encode(map[string]string{"error": "invalid_job"})
				return
			}
			inMap, _ := raw["input"].(map[string]any)
			if _, hasScores := inMap["accumulated_scores"]; hasScores {
				w.WriteHeader(http.StatusBadRequest)
				_ = json.NewEncoder(w).Encode(map[string]string{"error": "invalid_job"})
				return
			}
			if _, hasTags := inMap["accumulated_tags"]; hasTags {
				w.WriteHeader(http.StatusBadRequest)
				_ = json.NewEncoder(w).Encode(map[string]string{"error": "invalid_job"})
				return
			}
			rawBytes, _ := json.Marshal(raw)
			var j v1.Job
			_ = json.Unmarshal(rawBytes, &j)
			if j.Stage == v1.Policy {
				_ = json.NewEncoder(w).Encode(v1.Result{
					Binding:  j.Binding,
					Outcome:  v1.Excluded,
					Proposal: &v1.Proposal{Exclusion: &v1.Exclusion{Reason: "adult_content", Scope: "publication"}},
					Reasons:  []string{"adult_content", "proposed_policy_only"},
					Evidence: []v1.Evidence{},
				})
				return
			}
			u := 0.01
			_ = json.NewEncoder(w).Encode(v1.Result{
				Binding:     j.Binding,
				Outcome:     v1.Completed,
				Proposal:    &v1.Proposal{ContentType: "xxx"},
				Uncertainty: &u,
				Reasons:     []string{"calibrated_metadata_classification"},
				Evidence:    []v1.Evidence{},
			})
		}
	}))
	defer strictSrv.Close()

	_, err = s.Register(testContext, "strict-v1-classifier", v1.RegistryWrite{
		Endpoint:      strictSrv.URL,
		Priority:      100,
		Mode:          "apply",
		State:         "active",
		Configuration: json.RawMessage(`{}`),
	})
	require.NoError(t, err)

	admitted, err := s.Scan(testContext)
	require.NoError(t, err)
	require.Equal(t, 1, admitted)

	// Step 0: tracker-liveness
	run(t, s)
	// Step 1: strict-v1-classifier (auto-falls back to strict v1.0 classify + policy and merges both!)
	run(t, s)

	var kind string
	require.NoError(t, p.QueryRow(testContext, `SELECT content_type FROM torrent_contents WHERE info_hash=$1`, hAdult).Scan(&kind))
	require.Equal(t, "xxx", kind)

	var exclReason string
	require.NoError(t, p.QueryRow(testContext, `SELECT reason FROM processor_exclusions WHERE info_hash=$1`, hAdult).Scan(&exclReason))
	require.Equal(t, "adult_content", exclReason)
}

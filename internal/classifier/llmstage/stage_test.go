package llmstage

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/spencercnorton/bitagent/internal/classifier"
	"github.com/spencercnorton/bitagent/internal/classifier/classification"
	"github.com/spencercnorton/bitagent/internal/evidence"
	"github.com/spencercnorton/bitagent/internal/model"
	"github.com/spencercnorton/bitagent/internal/protocol"
	"go.uber.org/zap"
)

// fakeInner is a deterministic classifier.Runner for tests.
type fakeInner struct {
	res classification.Result
	err error
}

func (r fakeInner) Run(_ context.Context, _ string, _ classifier.Flags, _ model.Torrent) (classification.Result, error) {
	return r.res, r.err
}

func (r fakeInner) EvalMatch(_ context.Context, _ model.Torrent, _ model.NullContentType) (classifier.MatchDecision, error) {
	return classifier.MatchDecision{}, nil
}

type fakePrivacy struct {
	isPriv bool
	err    error
	calls  *atomic.Int32
}

func (p fakePrivacy) IsPrivateInfoHash(_ context.Context, _ []byte) (bool, error) {
	if p.calls != nil {
		p.calls.Add(1)
	}
	return p.isPriv, p.err
}

func baseTorrent() model.Torrent {
	var ih protocol.ID
	for i := range ih {
		ih[i] = 0xCD
	}
	return model.Torrent{
		InfoHash:  ih,
		Name:      "Some Thing (2023) 1080p BluRay x265-GROUP",
		Size:      5 * 1024 * 1024 * 1024,
		Extension: model.NullString{String: "mkv", Valid: true},
		Files: []model.TorrentFile{
			{Path: "thing.mkv", Size: 4 * 1024 * 1024 * 1024, Extension: model.NullString{String: "mkv", Valid: true}},
		},
	}
}

func newStageWithServer(t *testing.T, cfg Config, inner classifier.Runner, privacy PrivacyStore, handler http.HandlerFunc) *Stage {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	cfg.Endpoint = srv.URL
	cfg.APIKey = "test-key"
	return NewStage(cfg, inner, privacy, NewMetrics(), zap.NewNop().Sugar(), Admission{Budget: &testBudget{}, Capture: &testAudit{}})
}

// TestDisabledIsInert verifies that the stage returns inner's result
// verbatim when Enabled=false, even on an ErrUnmatched that would
// otherwise trigger the LLM.
func TestDisabledIsInert(t *testing.T) {
	inner := fakeInner{err: classification.ErrUnmatched}
	s := newStageWithServer(t, NewDefaultConfig(), inner, fakePrivacy{},
		func(w http.ResponseWriter, _ *http.Request) {
			t.Fatal("LLM must not be called when stage is disabled")
		})
	_, err := s.Run(context.Background(), "", classifier.Flags{}, baseTorrent())
	if err != classification.ErrUnmatched {
		t.Fatalf("want ErrUnmatched back, got %v", err)
	}
}

// TestRuntimeFlagDisablesStage verifies local-only backlog passes can bypass
// even an enabled shadow-mode llmstage. Shadow mode still calls the endpoint,
// so this flag is load-bearing for no-LLM backfills.
func TestRuntimeFlagDisablesStage(t *testing.T) {
	cfg := NewDefaultConfig()
	cfg.Enabled = true
	inner := fakeInner{err: classification.ErrUnmatched}
	s := newStageWithServer(t, cfg, inner, fakePrivacy{},
		func(w http.ResponseWriter, _ *http.Request) {
			t.Fatal("LLM must not be called when runtime flag disables the stage")
		})
	_, err := s.Run(context.Background(), "", classifier.Flags{"llm_stage_enabled": false}, baseTorrent())
	if err != classification.ErrUnmatched {
		t.Fatalf("want ErrUnmatched back, got %v", err)
	}
}

// TestInnerMatchWinsDoesNotCallLLM: if CEL finds a match, LLM is
// silent — the LLM only fires on unmatched.
func TestInnerMatchWinsDoesNotCallLLM(t *testing.T) {
	cfg := NewDefaultConfig()
	cfg.Enabled = true
	inner := fakeInner{res: classification.Result{
		ContentAttributes: classification.ContentAttributes{
			ContentType: model.NewNullContentType(model.ContentTypeMovie),
		},
	}}
	s := newStageWithServer(t, cfg, inner, fakePrivacy{},
		func(w http.ResponseWriter, _ *http.Request) {
			t.Fatal("LLM must not run when inner matched")
		})
	res, err := s.Run(context.Background(), "", classifier.Flags{}, baseTorrent())
	if err != nil {
		t.Fatal(err)
	}
	if res.ContentType.ContentType != model.ContentTypeMovie {
		t.Fatalf("inner result lost; got %v", res.ContentType)
	}
}

// TestPrivacyGateHardBlocksCall verifies that an infohash flagged
// private via the evidence store never reaches the OpenAI endpoint.
func TestPrivacyGateHardBlocksCall(t *testing.T) {
	cfg := NewDefaultConfig()
	cfg.Enabled = true
	inner := fakeInner{err: classification.ErrUnmatched}
	s := newStageWithServer(t, cfg, inner, fakePrivacy{isPriv: true},
		func(w http.ResponseWriter, _ *http.Request) {
			t.Fatal("LLM must not be called for private-tracker infohash")
		})
	_, _ = s.Run(context.Background(), "", classifier.Flags{}, baseTorrent())
}

// TestPrivacyErrorFailsClosed — if the privacy store itself errors,
// we must not call OpenAI. Failing closed is the point.
func TestPrivacyErrorFailsClosed(t *testing.T) {
	cfg := NewDefaultConfig()
	cfg.Enabled = true
	inner := fakeInner{err: classification.ErrUnmatched}
	s := newStageWithServer(t, cfg, inner, fakePrivacy{err: errFake},
		func(w http.ResponseWriter, _ *http.Request) {
			t.Fatal("LLM must not be called when privacy gate errors")
		})
	_, _ = s.Run(context.Background(), "", classifier.Flags{}, baseTorrent())
}

func TestNativePrivateGateDoesNotDependOnEvidence(t *testing.T) {
	tests := []struct {
		name    string
		privacy PrivacyStore
		calls   *atomic.Int32
	}{
		{name: "missing evidence store"},
		{name: "stale public evidence", calls: &atomic.Int32{}},
		{name: "evidence error", calls: &atomic.Int32{}},
	}
	tests[1].privacy = fakePrivacy{isPriv: false, calls: tests[1].calls}
	tests[2].privacy = fakePrivacy{err: errFake, calls: tests[2].calls}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg := NewDefaultConfig()
			cfg.Enabled = true
			inner := fakeInner{err: classification.ErrUnmatched}
			var modelCalls atomic.Int32
			s := newStageWithServer(t, cfg, inner, tc.privacy,
				func(w http.ResponseWriter, _ *http.Request) {
					modelCalls.Add(1)
					respondWith(w, "movie", 0.99)
				})
			tor := baseTorrent()
			tor.Private = true

			_, err := s.Run(context.Background(), "", classifier.Flags{}, tor)
			if err != classification.ErrUnmatched {
				t.Fatalf("deterministic inner result must be preserved, got %v", err)
			}
			if modelCalls.Load() != 0 {
				t.Fatalf("native private torrent reached model %d times", modelCalls.Load())
			}
			if tc.calls != nil && tc.calls.Load() != 0 {
				t.Fatalf("native gate must precede evidence lookup, got %d calls", tc.calls.Load())
			}
		})
	}
}

// TestPlausibilityRejectsTinyTorrent: a torrent smaller than
// MinTotalSizeBytes is skipped before privacy or OpenAI.
func TestPlausibilityRejectsTinyTorrent(t *testing.T) {
	cfg := NewDefaultConfig()
	cfg.Enabled = true
	inner := fakeInner{err: classification.ErrUnmatched}
	s := newStageWithServer(t, cfg, inner, fakePrivacy{},
		func(w http.ResponseWriter, _ *http.Request) {
			t.Fatal("LLM must not be called for implausibly small torrent")
		})
	tor := baseTorrent()
	tor.Size = 1 * 1024 * 1024 // 1 MB
	_, _ = s.Run(context.Background(), "", classifier.Flags{}, tor)
}

// TestPlausibilityRejectsNonMediaExt: a torrent without a media file
// extension on any row is skipped.
func TestPlausibilityRejectsNonMediaExt(t *testing.T) {
	cfg := NewDefaultConfig()
	cfg.Enabled = true
	inner := fakeInner{err: classification.ErrUnmatched}
	s := newStageWithServer(t, cfg, inner, fakePrivacy{},
		func(w http.ResponseWriter, _ *http.Request) {
			t.Fatal("LLM must not be called for non-media extension set")
		})
	tor := baseTorrent()
	tor.Extension = model.NullString{String: "iso", Valid: true}
	tor.Files = []model.TorrentFile{
		{Path: "x.iso", Size: 4 * 1024 * 1024 * 1024, Extension: model.NullString{String: "iso", Valid: true}},
	}
	_, _ = s.Run(context.Background(), "", classifier.Flags{}, tor)
}

// TestShadowMode: LLM runs, but inner result is returned unchanged.
func TestShadowMode(t *testing.T) {
	cfg := NewDefaultConfig()
	cfg.Enabled = true
	cfg.EnableLive = false

	inner := fakeInner{err: classification.ErrUnmatched}
	called := false
	s := newStageWithServer(t, cfg, inner, fakePrivacy{},
		func(w http.ResponseWriter, _ *http.Request) {
			called = true
			respondWith(w, "movie", 0.9)
		})
	res, err := s.Run(context.Background(), "", classifier.Flags{}, baseTorrent())
	if !called {
		t.Fatal("LLM should have run in shadow mode")
	}
	if err != classification.ErrUnmatched {
		t.Fatalf("shadow mode must return inner err; got %v", err)
	}
	if res.ContentType.Valid {
		t.Fatal("shadow mode must not mutate inner result's ContentType")
	}
}

// TestLiveModeHighConfidenceApplies: live mode + confidence above
// threshold replaces inner with the LLM result.
func TestLiveModeHighConfidenceApplies(t *testing.T) {
	cfg := NewDefaultConfig()
	cfg.Enabled = true
	cfg.EnableLive = true
	cfg.MinConfidence = 0.8

	inner := fakeInner{err: classification.ErrUnmatched}
	s := newStageWithServer(t, cfg, inner, fakePrivacy{},
		func(w http.ResponseWriter, _ *http.Request) {
			respondWith(w, "movie", 0.95)
		})
	res, err := s.Run(context.Background(), "", classifier.Flags{}, baseTorrent())
	if err != nil {
		t.Fatalf("live-mode high-confidence should return nil err; got %v", err)
	}
	if res.ContentType.ContentType != model.ContentTypeMovie {
		t.Fatalf("expected ContentType=movie, got %v", res.ContentType)
	}
}

// TestLiveModeLowConfidenceDoesNotApply: below threshold keeps inner.
func TestLiveModeLowConfidenceDoesNotApply(t *testing.T) {
	cfg := NewDefaultConfig()
	cfg.Enabled = true
	cfg.EnableLive = true
	cfg.MinConfidence = 0.8

	inner := fakeInner{err: classification.ErrUnmatched}
	s := newStageWithServer(t, cfg, inner, fakePrivacy{},
		func(w http.ResponseWriter, _ *http.Request) {
			respondWith(w, "movie", 0.55)
		})
	_, err := s.Run(context.Background(), "", classifier.Flags{}, baseTorrent())
	if err != classification.ErrUnmatched {
		t.Fatalf("expected ErrUnmatched (below threshold); got %v", err)
	}
}

// TestCacheHitAvoidsSecondCall: a second Run for the same torrent
// must not hit the HTTP endpoint again.
func TestCacheHitAvoidsSecondCall(t *testing.T) {
	cfg := NewDefaultConfig()
	cfg.Enabled = true

	inner := fakeInner{err: classification.ErrUnmatched}
	calls := 0
	s := newStageWithServer(t, cfg, inner, fakePrivacy{},
		func(w http.ResponseWriter, _ *http.Request) {
			calls++
			respondWith(w, "movie", 0.9)
		})

	_, _ = s.Run(context.Background(), "", classifier.Flags{}, baseTorrent())
	_, _ = s.Run(context.Background(), "", classifier.Flags{}, baseTorrent())

	if calls != 1 {
		t.Fatalf("expected 1 HTTP call, cache should serve the second; got %d", calls)
	}
}

// TestCacheKeyIncludesPromptVersion: bumping prompt_version must
// produce a different key (cache miss).
func TestCacheKeyIncludesPromptVersion(t *testing.T) {
	cfg := NewDefaultConfig()
	s1 := NewStage(cfg, fakeInner{}, fakePrivacy{}, NewMetrics(), zap.NewNop().Sugar())
	k1 := s1.cacheKey(baseTorrent())

	cfg.PromptVersion = "v2-next"
	s2 := NewStage(cfg, fakeInner{}, fakePrivacy{}, NewMetrics(), zap.NewNop().Sugar())
	k2 := s2.cacheKey(baseTorrent())

	if k1 == k2 {
		t.Fatal("cache key must change when prompt_version changes")
	}
}

// TestResponseParsing verifies strict-JSON round trip.
func TestResponseParsing(t *testing.T) {
	raw := []byte(`{"choices":[{"finish_reason":"stop","message":{"content":"{\"category\":\"tv\",\"confidence\":0.82}"}}]}`)
	d, err := parseResponse(raw)
	if err != nil {
		t.Fatal(err)
	}
	if d.MediaType != evidence.MediaTypeTV {
		t.Fatalf("want tv, got %v", d.MediaType)
	}
	if d.Confidence < 0.8 || d.Confidence > 0.85 {
		t.Fatalf("confidence lost precision: %v", d.Confidence)
	}
}

// TestResponseRejectsOutOfRangeConfidence: belt and braces.
func TestResponseRejectsOutOfRangeConfidence(t *testing.T) {
	raw := []byte(`{"choices":[{"message":{"content":"{\"category\":\"tv\",\"confidence\":1.5}"}}]}`)
	if _, err := parseResponse(raw); err == nil {
		t.Fatal("expected error on confidence > 1")
	}
}

// Helpers

type testError struct{ msg string }

func (e testError) Error() string { return e.msg }

var errFake = testError{"simulated"}

func respondWith(w http.ResponseWriter, category string, confidence float64) {
	inner := map[string]any{"category": category, "confidence": confidence}
	innerB, _ := json.Marshal(inner)
	outer := map[string]any{
		"choices": []map[string]any{{"finish_reason": "stop", "message": map[string]string{"content": string(innerB)}}},
	}
	b, _ := json.Marshal(outer)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(b)
}

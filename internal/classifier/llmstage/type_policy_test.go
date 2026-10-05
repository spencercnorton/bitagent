package llmstage

import (
	"context"
	"encoding/json"
	"net/http"
	"sync/atomic"
	"testing"

	"github.com/go-playground/validator/v10"
	"github.com/spencercnorton/bitagent/internal/config"
	"github.com/spencercnorton/bitagent/internal/config/configresolver"
	"github.com/spencercnorton/bitagent/internal/model"
	"github.com/stretchr/testify/require"
)

func TestTypeLiveAllowlistValidation(t *testing.T) {
	for _, allowed := range [][]string{nil, {}, {"movie", "tv"}, {"music", "audiobook", "book"}} {
		cfg := NewDefaultConfig()
		cfg.LiveAllowedTypes = allowed
		require.NoError(t, cfg.Validate())
	}
	for _, allowed := range [][]string{{"unknown"}, {"tv_show"}, {"Movie"}, {"movie", "movie"}, {""}, {"movie", " tv"}} {
		cfg := NewDefaultConfig()
		cfg.LiveAllowedTypes = allowed
		require.ErrorContains(t, cfg.Validate(), "live_allowed_types")
	}
}

func TestTypeLiveAllowlistEnvironmentBinding(t *testing.T) {
	resolved, err := config.New(config.Params{
		Specs: []config.Spec{{Key: "classifier_llm", DefaultValue: NewDefaultConfig()}},
		Resolvers: []configresolver.Resolver{configresolver.NewEnv(map[string]string{
			"CLASSIFIER_LLM_LIVE_ALLOWED_TYPES": "movie,tv",
		})},
		Validate: validator.New(),
	})
	require.NoError(t, err)
	cfg := resolved.Resolved.NodeMap["classifier_llm"].Value.(Config)
	require.Equal(t, []string{"movie", "tv"}, cfg.LiveAllowedTypes)
	require.NoError(t, cfg.Validate())
}

func TestDefaultWorkflowTypeCanaryCannotInferExcludedTypes(t *testing.T) {
	for _, category := range []string{"music", "book", "audiobook"} {
		t.Run(category, func(t *testing.T) {
			inner := coreWorkflowForTypeStage(t)
			flags := offlineTypeWorkflowFlags()
			flags["delete_content_types"] = []any{"music", "ebook", "audiobook"}
			tor := unknownTypeWorkflowTorrent()
			baseline, err := inner.Run(context.Background(), "default", flags, tor)
			require.NoError(t, err)
			require.False(t, baseline.ContentType.Valid)
			cfg := NewDefaultConfig()
			cfg.Enabled, cfg.EnableLive = true, true
			cfg.LiveAllowedTypes = []string{"tv", "movie"}
			var calls atomic.Int32
			s := newStageWithServer(t, cfg, inner, fakePrivacy{}, func(w http.ResponseWriter, _ *http.Request) {
				calls.Add(1)
				respondWith(w, category, .99)
			})
			for range 2 {
				got, err := s.Run(context.Background(), "default", flags, tor)
				require.NoError(t, err, "a disallowed inferred type cannot trigger the exclusion tail")
				require.Equal(t, baseline, got)
			}
			require.EqualValues(t, 1, calls.Load())
			require.EqualValues(t, 1, s.admission.Budget.(*testBudget).used.Load())
			require.Zero(t, typeAdmissionCounterValue(t, s.metrics.liveAppliedTotal.WithLabelValues(category), "live_applied_total"))
			audit := s.admission.Capture.(*testAudit)
			require.Len(t, audit.requests, 1)
			var input struct {
				LiveAllowedTypes []string `json:"live_allowed_types"`
			}
			require.NoError(t, json.Unmarshal(audit.requests[0].TaskInputJSON, &input))
			require.Equal(t, []string{"movie", "tv"}, input.LiveAllowedTypes)
			for _, d := range audit.decisions {
				require.Equal(t, "policy_declined", d.Outcome)
				require.Equal(t, category, d.Category)
				require.False(t, d.WouldApply)
				require.Equal(t, input.LiveAllowedTypes, d.LiveAllowedTypes)
			}
		})
	}
}

func TestTypeCanaryRechecksAllowlistOnCachedAnswer(t *testing.T) {
	cfg := NewDefaultConfig()
	cfg.Enabled, cfg.EnableLive = true, true
	var calls atomic.Int32
	s := newStageWithServer(t, cfg, coreWorkflowForTypeStage(t), fakePrivacy{}, func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		respondWith(w, "music", .99)
	})
	tor := unknownTypeWorkflowTorrent()
	flags := offlineTypeWorkflowFlags()
	flags["delete_content_types"] = []any{}
	got, err := s.Run(context.Background(), "default", flags, tor)
	require.NoError(t, err)
	require.Equal(t, model.NewNullContentType(model.ContentTypeMusic), got.ContentType)
	oldKey := s.cacheKey(tor)
	cached, ok := s.cache.Get(oldKey)
	require.True(t, ok)
	s.cfg.LiveAllowedTypes = []string{"movie", "tv"}
	require.NotEqual(t, oldKey, s.cacheKey(tor), "ordinary cached receipts belong to the previous policy")
	// Even if a cache entry is copied to a changed policy key, application must
	// inspect the current allowlist. A real recorder also rejects its old receipt.
	s.cache.Put(s.cacheKey(tor), cached)
	flags["delete_content_types"] = []any{"music"}
	got, err = s.Run(context.Background(), "default", flags, tor)
	require.NoError(t, err)
	require.False(t, got.ContentType.Valid)
	require.EqualValues(t, 1, calls.Load())
	require.Equal(t, float64(1), typeAdmissionCounterValue(t, s.metrics.liveAppliedTotal.WithLabelValues("music"), "live_applied_total"))
	require.Equal(t, "policy_declined", s.admission.Capture.(*testAudit).decisions[1].Outcome)
}

func TestTypeAllowlistOwnsItsConfigAndKeepsUnknownAndLowConfidence(t *testing.T) {
	for _, category := range []string{"unknown", "music"} {
		t.Run(category, func(t *testing.T) {
			cfg := NewDefaultConfig()
			cfg.Enabled, cfg.EnableLive = true, true
			cfg.LiveAllowedTypes = []string{"movie", "tv"}
			s := newStageWithServer(t, cfg, coreWorkflowForTypeStage(t), fakePrivacy{}, func(w http.ResponseWriter, _ *http.Request) { respondWith(w, category, .2) })
			cfg.LiveAllowedTypes[0] = "music"
			require.Equal(t, []string{"movie", "tv"}, s.cfg.LiveAllowedTypes)
			got, err := s.Run(context.Background(), "default", offlineTypeWorkflowFlags(), unknownTypeWorkflowTorrent())
			require.NoError(t, err)
			require.False(t, got.ContentType.Valid)
			want := "low_confidence"
			if category == "unknown" {
				want = "unknown"
			}
			require.Equal(t, want, s.admission.Capture.(*testAudit).decisions[0].Outcome)
		})
	}
}

func TestTypeAllowlistAlsoBoundsShadowWouldApply(t *testing.T) {
	cfg := NewDefaultConfig()
	cfg.Enabled = true
	cfg.LiveAllowedTypes = []string{"movie", "tv"}
	s := newStageWithServer(t, cfg, coreWorkflowForTypeStage(t), fakePrivacy{}, func(w http.ResponseWriter, _ *http.Request) { respondWith(w, "music", .99) })
	got, err := s.Run(context.Background(), "default", offlineTypeWorkflowFlags(), unknownTypeWorkflowTorrent())
	require.NoError(t, err)
	require.False(t, got.ContentType.Valid)
	d := s.admission.Capture.(*testAudit).decisions[0]
	require.Equal(t, "policy_declined", d.Outcome)
	require.False(t, d.WouldApply)
	require.False(t, d.Live)
}

func TestInvalidTypeAllowlistCannotDispatch(t *testing.T) {
	cfg := NewDefaultConfig()
	cfg.Enabled, cfg.EnableLive = true, true
	cfg.LiveAllowedTypes = []string{"unknown"}
	var calls atomic.Int32
	s := newStageWithServer(t, cfg, coreWorkflowForTypeStage(t), fakePrivacy{}, func(w http.ResponseWriter, _ *http.Request) { calls.Add(1) })
	got, err := s.Run(context.Background(), "default", offlineTypeWorkflowFlags(), unknownTypeWorkflowTorrent())
	require.NoError(t, err)
	require.False(t, got.ContentType.Valid)
	require.Zero(t, calls.Load())
	require.Zero(t, s.admission.Budget.(*testBudget).used.Load())
	require.Empty(t, s.admission.Capture.(*testAudit).requests)
}

package llmstage

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"sync/atomic"
	"testing"

	"github.com/spencercnorton/bitagent/internal/classifier"
	"github.com/spencercnorton/bitagent/internal/classifier/classification"
	"github.com/spencercnorton/bitagent/internal/database/search"
	"github.com/spencercnorton/bitagent/internal/evidence"
	"github.com/spencercnorton/bitagent/internal/lazy"
	"github.com/spencercnorton/bitagent/internal/model"
	"github.com/spencercnorton/bitagent/internal/tmdb"
	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"
)

// Compile the checked-in default workflow, not a fake Runner that invents a
// top-level ErrUnmatched. Loading the core source explicitly also avoids the
// factory's ambient XDG/CWD operator overrides. Offline flags below ensure the
// nil search/TMDB dependencies cannot be used by these fixtures.
func coreWorkflowForTypeStage(t *testing.T) classifier.Runner {
	t.Helper()
	raw, err := os.ReadFile("../classifier.core.yml")
	require.NoError(t, err)
	var document map[string]any
	require.NoError(t, yaml.Unmarshal(raw, &document))
	encoded, err := json.Marshal(document)
	require.NoError(t, err)
	var source classifier.Source
	require.NoError(t, json.Unmarshal(encoded, &source))
	// This test-only workflow runs the real default workflow before adding a
	// tag, to prove type-only application preserves existing workflow metadata.
	source.Workflows["tagged_default"] = []any{
		map[string]any{"run_workflow": "default"},
		map[string]any{"add_tag": "retained-workflow-tag"},
	}
	factory := classifier.New(classifier.Params{
		Config:     classifier.NewDefaultConfig(),
		Search:     lazy.New(func() (search.Search, error) { return nil, nil }),
		TmdbClient: lazy.New(func() (tmdb.Client, error) { return nil, nil }),
	})
	compiler, err := factory.Compiler.Get()
	require.NoError(t, err)
	runner, err := compiler.Compile(source)
	require.NoError(t, err)
	return runner
}

func offlineTypeWorkflowFlags() classifier.Flags {
	return classifier.Flags{
		"local_search_enabled": false,
		"apis_enabled":         false,
		"tmdb_enabled":         false,
		"llm_match_enabled":    false,
	}
}

func unknownTypeWorkflowTorrent() model.Torrent {
	tor := baseTorrent()
	tor.Name = "Mystery Clip 1080p BluRay x265-GROUP.mkv"
	tor.FilesStatus = model.FilesStatusSingle
	tor.Extension = model.NewNullString("mkv")
	tor.Files = nil
	return tor
}

func TestDefaultWorkflowUnknownTypeReachesAuditedShadow(t *testing.T) {
	ctx := context.Background()
	inner := coreWorkflowForTypeStage(t)
	tor := unknownTypeWorkflowTorrent()
	flags := offlineTypeWorkflowFlags()
	baseline, err := inner.Run(ctx, "default", flags, tor)
	require.NoError(t, err, "the real workflow absorbs unmatched actions")
	require.False(t, baseline.ContentType.Valid)
	require.Nil(t, baseline.Content)
	require.True(t, baseline.VideoResolution.Valid)
	require.True(t, baseline.VideoCodec.Valid)
	cfg := NewDefaultConfig()
	cfg.Enabled = true
	var calls atomic.Int32
	s := newStageWithServer(t, cfg, inner, fakePrivacy{}, func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		respondWith(w, "movie", .98)
	})
	got, err := s.Run(ctx, "default", flags, tor)
	require.NoError(t, err)
	require.Equal(t, baseline, got, "shadow must return the successful unknown result verbatim")
	require.EqualValues(t, 1, calls.Load())
	audit := s.admission.Capture.(*testAudit)
	require.Len(t, audit.requests, 1)
	require.Len(t, audit.results, 1)
	require.Len(t, audit.decisions, 1)
	require.Equal(t, "classified", audit.decisions[0].Outcome)
	require.True(t, audit.decisions[0].WouldApply)
	require.False(t, audit.decisions[0].Live)
}

func TestExplicitUnmatchedLiveTypePreservesAttributesAndTags(t *testing.T) {
	ctx := context.Background()
	inner := coreWorkflowForTypeStage(t)
	tor := unknownTypeWorkflowTorrent()
	flags := offlineTypeWorkflowFlags()
	baseline, err := inner.Run(ctx, "tagged_default", flags, tor)
	require.NoError(t, err)
	require.False(t, baseline.ContentType.Valid)
	require.Contains(t, baseline.Tags, "retained-workflow-tag")
	require.True(t, baseline.VideoResolution.Valid)
	cfg := NewDefaultConfig()
	cfg.Enabled, cfg.EnableLive = true, true
	// The real default produces successful unknowns, now intentionally shadow-
	// only. This synthetic legacy error path uses its non-empty attributes to
	// verify compatibility without pretending the default workflow returns it.
	s := newStageWithServer(t, cfg, fakeInner{res: baseline, err: classification.ErrUnmatched}, fakePrivacy{}, func(w http.ResponseWriter, _ *http.Request) {
		respondWith(w, "movie", .98)
	})
	got, err := s.Run(ctx, "tagged_default", flags, tor)
	require.NoError(t, err)
	baseline.ContentType = model.NewNullContentType(model.ContentTypeMovie)
	require.Equal(t, baseline, got, "the type is the only field the fallback may change")
	require.Nil(t, got.Content, "type-only output cannot invent a catalogue attachment")
}

// The default workflow's deletion tail has already completed on the unknown
// input. There is no approved post-policy live application point, even for a
// seemingly acceptable movie answer. Deny before calling or caching the model.
func TestDefaultWorkflowLiveModeRejectsNaturalUnknownBeforeDispatch(t *testing.T) {
	for _, category := range []string{"movie", "music"} {
		t.Run(category, func(t *testing.T) {
			ctx := context.Background()
			inner := coreWorkflowForTypeStage(t)
			tor := unknownTypeWorkflowTorrent()
			flags := offlineTypeWorkflowFlags()
			flags["delete_content_types"] = []any{"music"}
			baseline, err := inner.Run(ctx, "default", flags, tor)
			require.NoError(t, err)
			require.False(t, baseline.ContentType.Valid)
			cfg := NewDefaultConfig()
			cfg.Enabled, cfg.EnableLive = true, true
			var calls atomic.Int32
			s := newStageWithServer(t, cfg, inner, fakePrivacy{}, func(w http.ResponseWriter, _ *http.Request) {
				calls.Add(1)
				respondWith(w, category, .98)
			})
			got, err := s.Run(ctx, "default", flags, tor)
			require.NoError(t, err)
			require.Equal(t, baseline, got)
			require.Zero(t, calls.Load())
			require.Zero(t, s.admission.Budget.(*testBudget).used.Load())
			require.Empty(t, s.admission.Capture.(*testAudit).requests)
			require.Equal(t, float64(1), typeAdmissionCounterValue(t,
				s.metrics.gateRejectsTotal.WithLabelValues("policy_live_unavailable"), "gate_rejects_total"))
		})
	}
}

type typeWorkflowCanonicalStore struct{ label *evidence.CanonicalLabel }

func (s typeWorkflowCanonicalStore) CanonicalForInfoHash(context.Context, []byte) (*evidence.CanonicalLabel, error) {
	return s.label, nil
}

func TestDefaultWorkflowAuthoritativeOutcomesNeverReachTypeProvider(t *testing.T) {
	for _, kind := range []string{"known_type", "attached_identity", "canonical_type", "canonical_movie", "delete_unknown", "runtime_error", "native_private", "runtime_disabled"} {
		t.Run(kind, func(t *testing.T) {
			ctx := context.Background()
			inner := coreWorkflowForTypeStage(t)
			tor := unknownTypeWorkflowTorrent()
			flags := offlineTypeWorkflowFlags()
			switch kind {
			case "known_type":
				tor.Name = "Some Movie (2023) 1080p BluRay x265-GROUP.mkv"
			case "attached_identity":
				tor.Hint = model.TorrentHint{ContentType: model.ContentTypeMovie,
					ContentSource: model.NewNullString("tmdb"), ContentID: model.NewNullString("123")}
				tor.Contents = []model.TorrentContent{{
					ContentType:   model.NewNullContentType(model.ContentTypeMovie),
					ContentSource: model.NewNullString("tmdb"), ContentID: model.NewNullString("123"),
					Content: model.Content{Type: model.ContentTypeMovie, Source: "tmdb", ID: "123", Title: "Known Movie"},
				}}
			case "canonical_type", "canonical_movie":
				media := evidence.MediaTypeMusic
				if kind == "canonical_movie" {
					media = evidence.MediaTypeMovie
				}
				inner = classifier.NewCanonicalRunner(inner, typeWorkflowCanonicalStore{
					label: &evidence.CanonicalLabel{MediaType: media, ResolvedSource: evidence.SourceRadarr},
				}, classifier.NewPreemptMetrics())
			case "delete_unknown":
				flags["delete_content_types"] = []any{"unknown"}
			case "runtime_error":
				flags["apis_enabled"] = "not-a-boolean"
			case "native_private":
				tor.Private = true
			case "runtime_disabled":
				flags["llm_stage_enabled"] = false
			}
			baseline, baselineErr := inner.Run(ctx, "default", flags, tor)
			switch kind {
			case "delete_unknown":
				require.ErrorIs(t, baselineErr, classification.ErrDeleteTorrent)
			case "runtime_error":
				require.Error(t, baselineErr)
				require.NotErrorIs(t, baselineErr, classification.ErrUnmatched)
			default:
				require.NoError(t, baselineErr)
			}
			if kind == "known_type" || kind == "canonical_type" || kind == "canonical_movie" {
				require.True(t, baseline.ContentType.Valid)
			}
			if kind == "attached_identity" {
				require.NotNil(t, baseline.Content)
			}
			cfg := NewDefaultConfig()
			cfg.Enabled = true
			var calls atomic.Int32
			s := newStageWithServer(t, cfg, inner, fakePrivacy{}, func(w http.ResponseWriter, _ *http.Request) {
				calls.Add(1)
				respondWith(w, "tv", .99)
			})
			got, err := s.Run(ctx, "default", flags, tor)
			require.Equal(t, baselineErr, err)
			require.Equal(t, baseline, got)
			require.Zero(t, calls.Load())
			require.Empty(t, s.admission.Capture.(*testAudit).requests)
		})
	}
}

func TestKnownOrAttachedResultsNeverReachTypeProvider(t *testing.T) {
	for _, innerErr := range []error{nil, classification.ErrUnmatched} {
		for _, result := range []classification.Result{
			{ContentAttributes: classification.ContentAttributes{ContentType: model.NewNullContentType(model.ContentTypeMovie)}},
			{Content: &model.Content{Source: "tmdb", ID: "123"}},
		} {
			cfg := NewDefaultConfig()
			cfg.Enabled = true
			var calls atomic.Int32
			s := newStageWithServer(t, cfg, fakeInner{res: result, err: innerErr}, fakePrivacy{}, func(w http.ResponseWriter, _ *http.Request) {
				calls.Add(1)
				respondWith(w, "tv", .99)
			})
			got, err := s.Run(context.Background(), "default", nil, baseTorrent())
			require.Equal(t, innerErr, err)
			require.Equal(t, result, got)
			require.Zero(t, calls.Load())
		}
	}
}

func TestUnmatchedCompatibilityRejectsMixedErrorsAndCancellation(t *testing.T) {
	for _, tc := range []struct {
		name      string
		innerErr  error
		cancel    bool
		wantCalls int32
	}{
		{"sentinel", classification.ErrUnmatched, false, 1},
		{"linear_wrapper", classification.RuntimeError{Cause: fmt.Errorf("wrapped: %w", classification.ErrUnmatched)}, false, 1},
		{"joined_delete", errors.Join(classification.ErrUnmatched, classification.ErrDeleteTorrent), false, 0},
		{"joined_failure", errors.Join(classification.ErrUnmatched, errFake), false, 0},
		{"wrapped_join", fmt.Errorf("wrapped: %w", errors.Join(classification.ErrUnmatched, classification.ErrDeleteTorrent)), false, 0},
		{"canceled", classification.ErrUnmatched, true, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := NewDefaultConfig()
			cfg.Enabled = true
			var calls atomic.Int32
			s := newStageWithServer(t, cfg, fakeInner{err: tc.innerErr}, fakePrivacy{}, func(w http.ResponseWriter, _ *http.Request) {
				calls.Add(1)
				respondWith(w, "tv", .99)
			})
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if tc.cancel {
				cancel()
			}
			_, err := s.Run(ctx, "default", nil, baseTorrent())
			require.Equal(t, tc.innerErr, err)
			require.Equal(t, tc.wantCalls, calls.Load())
		})
	}
}

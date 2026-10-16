package config

import (
	"testing"

	"github.com/go-playground/validator/v10"
	"github.com/spencercnorton/bitagent/internal/classifier/contentfilter"
	"github.com/spencercnorton/bitagent/internal/config/configresolver"
	"github.com/stretchr/testify/require"
)

func TestEnvBindingContentFilterModelEnforcementIsIndependent(t *testing.T) {
	for _, tc := range []struct {
		name                 string
		env                  map[string]string
		mode                 string
		deterministic, model bool
	}{
		{"default", nil, "inherit", false, false},
		{"model only", map[string]string{"CONTENT_FILTER_LLM_ENFORCE": "true"}, "true", false, true},
		{"deterministic only", map[string]string{"CONTENT_FILTER_ENFORCE": "true", "CONTENT_FILTER_LLM_ENFORCE": "false"}, "false", true, false},
		{"legacy inheritance", map[string]string{"CONTENT_FILTER_ENFORCE": "true"}, "inherit", true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := resolveSectionFromEnv(t, "content_filter", contentfilter.NewDefaultConfig(), tc.env).(contentfilter.Config)
			require.Equal(t, tc.mode, cfg.LLMEnforce)
			require.Equal(t, tc.deterministic, cfg.Enforce)
			require.Equal(t, tc.model, cfg.LLMEnforcementEnabled())
			require.False(t, cfg.Enabled, "application mode must not enable the optional filter")
			require.False(t, cfg.LLMEnabled, "application mode must not enable provider calls")
		})
	}
}

func TestContentFilterModelEnforcementRejectsInvalidConfiguration(t *testing.T) {
	for _, mode := range []string{"yes", "TRUE", "0", "enforce"} {
		_, err := resolveRootNode([]configresolver.Resolver{configresolver.NewEnv(map[string]string{
			"CONTENT_FILTER_LLM_ENFORCE": mode,
		})}, validator.New(), Spec{Key: "content_filter", DefaultValue: contentfilter.NewDefaultConfig()})
		require.Error(t, err, "an invalid model enforcement mode must fail startup")
	}
}

func TestContentFilterModelEnforcementYAMLUsesQuotedStrings(t *testing.T) {
	for _, mode := range []string{"true", "false", "inherit"} {
		node, err := resolveRootNode([]configresolver.Resolver{configresolver.NewMap(map[string]any{
			"content_filter": map[string]any{"enforce": true, "llm_enforce": mode},
		}, validator.New())}, validator.New(), Spec{Key: "content_filter", DefaultValue: contentfilter.NewDefaultConfig()})
		require.NoError(t, err)
		cfg := node.Value.(contentfilter.Config)
		require.Equal(t, mode, cfg.LLMEnforce)
		require.Equal(t, mode != "false", cfg.LLMEnforcementEnabled())
	}
}

func TestEnvBindingContentFilterReviewDispositionDoesNotEnableEnforcement(t *testing.T) {
	cfg := resolveSectionFromEnv(t, "content_filter", contentfilter.NewDefaultConfig(), map[string]string{
		"CONTENT_FILTER_LLM_ACTION": "review",
	}).(contentfilter.Config)
	require.Equal(t, contentfilter.LLMActionReview, cfg.EffectiveLLMAction())
	require.False(t, cfg.Enabled)
	require.False(t, cfg.LLMEnabled)
	require.False(t, cfg.Enforce)
	require.False(t, cfg.LLMEnforcementEnabled())
	_, err := resolveRootNode([]configresolver.Resolver{configresolver.NewEnv(map[string]string{
		"CONTENT_FILTER_LLM_ACTION": "tag_and_delete",
	})}, validator.New(), Spec{Key: "content_filter", DefaultValue: contentfilter.NewDefaultConfig()})
	require.Error(t, err)
}

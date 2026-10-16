package llmwork_test

import (
	"testing"

	"github.com/spencercnorton/bitagent/internal/classifier"
	"github.com/spencercnorton/bitagent/internal/classifier/contentfilter"
	"github.com/spencercnorton/bitagent/internal/classifier/llmmatch"
	"github.com/spencercnorton/bitagent/internal/classifier/llmstage"
	"github.com/spencercnorton/bitagent/internal/llmwork"
	"github.com/spencercnorton/bitagent/internal/version"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

func TestSourceCasePoliciesRemainStableAcrossBuildOnlyRestart(t *testing.T) {
	previous := version.GitTag
	defer func() { version.GitTag = previous }()
	typeCfg := llmstage.NewDefaultConfig()
	stage := llmstage.NewStage(typeCfg, nil, nil, llmstage.NewMetrics(), zap.NewNop().Sugar())
	stage.SetWork(nil, classifier.NewDefaultConfig())
	matcher := llmmatch.NewClient(llmmatch.NewDefaultConfig(), nil, llmmatch.NewMetrics(), zap.NewNop().Sugar())
	matcher.SetWork(nil, classifier.NewDefaultConfig())
	filter := contentfilter.New(contentfilter.NewDefaultConfig())
	for _, tc := range []struct {
		name   string
		policy func() any
	}{
		{"type", func() any { return stage.WorkPolicy(llmstage.WorkPayload{Workflow: "default"}) }},
		{"matcher", func() any { return matcher.WorkPolicy(llmmatch.WorkPayload{}) }},
		{"language", filter.WorkPolicy},
	} {
		t.Run(tc.name, func(t *testing.T) {
			version.GitTag = "synthetic-build-a"
			before := llmwork.Digest(tc.policy())
			version.GitTag = "synthetic-build-b"
			after := llmwork.Digest(tc.policy())
			require.Equal(t, before, after)
		})
	}
	before := llmwork.Digest(stage.WorkPolicy(llmstage.WorkPayload{Workflow: "default"}))
	after := llmwork.Digest(stage.WorkPolicy(llmstage.WorkPayload{Workflow: "default", Flags: classifier.Flags{"local_search_enabled": false}}))
	require.NotEqual(t, before, after, "semantic flags remain part of task authority")
}

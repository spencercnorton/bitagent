package llmworkfx

import (
	"testing"

	"github.com/spencercnorton/bitagent/internal/classifier"
	"github.com/spencercnorton/bitagent/internal/classifier/contentfilter"
	"github.com/spencercnorton/bitagent/internal/classifier/llmmatch"
	"github.com/spencercnorton/bitagent/internal/classifier/llmstage"
	"github.com/spencercnorton/bitagent/internal/llmcapture"
	"github.com/spencercnorton/bitagent/internal/llmwork"
	"github.com/stretchr/testify/require"
)

type enabledControl struct{ llmcapture.DispatchControl }

func (enabledControl) Enabled() bool { return true }

type enabledCapture struct{ llmcapture.Capturer }

func (enabledCapture) Enabled() bool { return true }

func TestQueueActivationRequiresAllStageAndDispatchContracts(t *testing.T) {
	p := storeParams{Config: llmwork.NewDefaultConfig(), Classifier: classifier.NewDefaultConfig(), Type: llmstage.NewDefaultConfig(), Language: contentfilter.NewDefaultConfig(), Matcher: llmmatch.NewDefaultConfig()}
	_, err := provideStore(p)
	require.NoError(t, err, "disabled queue needs no database or capture")
	p.Config.Enabled = true
	_, err = provideStore(p)
	require.Error(t, err)
	p.Capture = enabledCapture{}
	p.Dispatch = enabledControl{}
	_, err = provideStore(p)
	require.Error(t, err, "selected stages must already be explicitly enabled")
	p.Type.Enabled = true
	p.Language.Enabled = true
	p.Language.LLMEnabled = true
	p.Language.LLMAction = contentfilter.LLMActionReview
	p.Matcher.Enabled = true
	_, err = provideStore(p)
	require.NoError(t, err)
	p.Language.LLMAction = contentfilter.LLMActionDrop
	_, err = provideStore(p)
	require.Error(t, err, "background language work cannot delete")
	p.Config.Kinds = []llmwork.Kind{llmwork.Matcher}
	_, err = provideStore(p)
	require.NoError(t, err, "unselected language stage does not gate matcher-only work")
}

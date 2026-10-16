package junkpurge

import (
	"context"
	"github.com/spencercnorton/bitagent/internal/namepolicy"
	"github.com/stretchr/testify/require"
	"testing"
)

func TestRejectedJunkNamesNeverDispatchOrBuildProviderBatch(t *testing.T) {
	p, err := namepolicy.New(namepolicy.Config{Enabled: true})
	require.NoError(t, err)
	j := NewJudge(NewDefaultConfig(), NewMetrics()).(*ollamaJudge)
	j.SetNamePolicy(p)
	for _, name := range []string{"Synthetic.电影.ENG.mkv", "FetishXXX.mkv"} {
		_, err = j.Judge(context.Background(), name)
		require.ErrorIs(t, err, namepolicy.ErrExcluded)
		_, err = j.JudgeBatch(context.Background(), []string{name})
		require.ErrorIs(t, err, namepolicy.ErrExcluded)
		c := newOpenAIBatchClient(NewDefaultConfig())
		c.namePolicy = p
		_, _, _, err = c.BuildInput(batchRun{}, 1, []batchItem{{TorrentName: name}})
		require.ErrorIs(t, err, namepolicy.ErrExcluded)
	}
}

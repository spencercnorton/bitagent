package llmstage

import (
	"fmt"
	"testing"

	"github.com/spencercnorton/bitagent/internal/llmwork"
	"github.com/spencercnorton/bitagent/internal/model"
	"github.com/stretchr/testify/require"
)

func TestTypeRequestIdentityIgnoresPreloadFileOrder(t *testing.T) {
	for _, n := range []int{3, 55} {
		t.Run(fmt.Sprint(n), func(t *testing.T) {
			original := baseTorrent()
			original.Files = nil
			for i := 0; i < n; i++ {
				original.Files = append(original.Files, model.TorrentFile{Index: uint(i), Path: fmt.Sprintf("synthetic-%02d.mkv", i), Size: uint(1000 + i)})
			}
			shuffled := original
			shuffled.Files = append([]model.TorrentFile(nil), original.Files...)
			for i, j := 0, len(shuffled.Files)-1; i < j; i, j = i+1, j-1 {
				shuffled.Files[i], shuffled.Files[j] = shuffled.Files[j], shuffled.Files[i]
			}
			before := append([]model.TorrentFile(nil), shuffled.Files...)
			stage := &Stage{cfg: NewDefaultConfig()}
			require.Equal(t, llmwork.SourceDigest(original), llmwork.SourceDigest(shuffled))
			require.Equal(t, buildBoundedRequestBody(stage.cfg, original), buildBoundedRequestBody(stage.cfg, shuffled))
			require.Equal(t, fileListHash(original), fileListHash(shuffled))
			require.Equal(t, stage.cacheKey(original), stage.cacheKey(shuffled))
			require.Equal(t, stage.WorkInputDigest(original), stage.WorkInputDigest(shuffled))
			require.Equal(t, before, shuffled.Files, "rendering must not mutate caller evidence")
		})
	}
}

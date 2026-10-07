package llmmatch

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/spencercnorton/bitagent/internal/model"
	"github.com/spencercnorton/bitagent/internal/namepolicy"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

func TestOwnerNameRejectionAvoidsExtractionRerankEmbeddingAndGroupedCalls(t *testing.T) {
	var calls atomic.Int32
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls.Add(1); http.Error(w, "unexpected provider", 500) }))
	defer s.Close()
	cfg := NewDefaultConfig()
	cfg.Enabled = true
	cfg.Endpoint = s.URL
	cfg.Embeddings.Endpoint = s.URL
	c := NewClient(cfg, nil, NewMetrics(), zap.NewNop().Sugar())
	p, err := namepolicy.New(namepolicy.Config{Enabled: true})
	require.NoError(t, err)
	c.SetNamePolicy(p)
	for _, name := range []string{"Synthetic.电影.English.mkv", "Synthetic.Фильм.mkv", "FetishXXX.mkv"} {
		torrent := model.Torrent{Name: name, Size: 1 << 30}
		_, err = c.Extract(context.Background(), torrent)
		require.ErrorIs(t, err, namepolicy.ErrExcluded)
		_, _, err = c.Rerank(context.Background(), torrent, Extraction{}, nil)
		require.ErrorIs(t, err, namepolicy.ErrExcluded)
		_, err = c.callEmbeddings(context.Background(), torrent, []byte(`{}`), 1)
		require.ErrorIs(t, err, namepolicy.ErrExcluded)
		st := c.ExtractMany(context.Background(), []model.Torrent{torrent}, 4)
		require.Equal(t, 1, st.Gated)
	}
	require.Zero(t, calls.Load())
}

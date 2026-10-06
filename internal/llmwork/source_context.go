package llmwork

import (
	"context"
	"github.com/spencercnorton/bitagent/internal/model"
)

type sourceKey struct{}

func WithSourceTorrent(ctx context.Context, t model.Torrent) context.Context {
	return context.WithValue(ctx, sourceKey{}, t)
}
func SourceTorrent(ctx context.Context) (model.Torrent, bool) {
	t, ok := ctx.Value(sourceKey{}).(model.Torrent)
	return t, ok
}

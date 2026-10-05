package llmcapture

import (
	"context"
	"crypto/sha256"
	"fmt"
)

// EmbeddingResultRecorder adds a final source-bound admission recheck. It
// grants no match-decision authority: final decisions still bind chat reranks.
type EmbeddingResultRecorder interface {
	RecheckEmbeddingRequest(context.Context, []byte, []byte) error
	RecordHTTPResult(context.Context, []byte, HTTPResult) (ResultReceipt, error)
}

type embeddingRequestStore interface {
	RecheckEmbeddingRequest(context.Context, []byte, []byte) error
}

func (r *Recorder) RecheckEmbeddingRequest(ctx context.Context, key, infoHash []byte) error {
	if !r.Enabled() || len(key) != sha256.Size || len(infoHash) != 20 {
		return fmt.Errorf("%w: embeddings requires a bound public source", ErrCaptureUnavailable)
	}
	store, ok := r.store.(embeddingRequestStore)
	if !ok {
		return fmt.Errorf("%w: embedding admission store is required", ErrCaptureUnavailable)
	}
	if err := store.RecheckEmbeddingRequest(ctx, key, infoHash); err != nil {
		return fmt.Errorf("%w: embedding admission recheck: %v", ErrCaptureUnavailable, err)
	}
	return nil
}

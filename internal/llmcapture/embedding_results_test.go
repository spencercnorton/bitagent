package llmcapture

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
)

type embeddingAdmissionStoreStub struct {
	resultStoreStub
	rechecks int
}

func (s *embeddingAdmissionStoreStub) RecheckEmbeddingRequest(context.Context, []byte, []byte) error {
	s.rechecks++
	return s.err
}

func TestEmbeddingRequestRecheckRequiresEnabledBoundSourceAndStore(t *testing.T) {
	config := NewDefaultConfig()
	config.Enabled = true
	store := &embeddingAdmissionStoreStub{}
	recorder, err := NewRecorder(config, &privacyStub{}, store)
	require.NoError(t, err)
	key, infoHash := make([]byte, 32), make([]byte, 20)
	require.NoError(t, recorder.RecheckEmbeddingRequest(context.Background(), key, infoHash))
	require.Equal(t, 1, store.rechecks)
	require.ErrorIs(t, recorder.RecheckEmbeddingRequest(context.Background(), key[:31], infoHash), ErrCaptureUnavailable)
	store.err = errors.New("unavailable")
	require.ErrorIs(t, recorder.RecheckEmbeddingRequest(context.Background(), key, infoHash), ErrCaptureUnavailable)
	request := validRequest()
	request.Task = TaskMatcherEmbedding
	_, err = recorder.Capture(context.Background(), request)
	require.ErrorIs(t, err, ErrCaptureUnavailable, "embeddings requires local/api source")
	request.CandidateSource = CandidateSourceAPI
	_, err = recorder.Capture(context.Background(), request)
	require.NoError(t, err, "embedding task admits only a local/API candidate source")
	require.ErrorIs(t, recorder.RecheckEmbeddingRequest(context.Background(), key, infoHash), ErrCaptureUnavailable)
}

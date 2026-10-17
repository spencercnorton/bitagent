package processor

import (
	"context"
	"strings"
	"testing"

	"github.com/spencercnorton/bitagent/internal/classifier/classification"
	"github.com/spencercnorton/bitagent/internal/classifier/contentfilter"
	"github.com/spencercnorton/bitagent/internal/model"
	"github.com/spencercnorton/bitagent/internal/processingstate"
	"github.com/spencercnorton/bitagent/internal/protocol"
	"github.com/stretchr/testify/require"
)

type receiptCapture struct{ receipts []processingstate.Receipt }

func (r *receiptCapture) RecordCore(_ context.Context, receipts []processingstate.Receipt) {
	r.receipts = append(r.receipts, receipts...)
}

func TestAuditedReprocessingRetainsCatalogueOnDestructiveVerdicts(t *testing.T) {
	for _, kind := range []string{"classifier", "content_filter"} {
		t.Run(kind, func(t *testing.T) {
			h := processTestHash(2)
			torrent := processTestTorrent(h, "synthetic software image", "iso")
			torrent.Contents = []model.TorrentContent{{ID: "synthetic-existing", InfoHash: h}}
			p, mock, blocker := newProcessTestProcessor(t, []model.Torrent{torrent}, processRunnerStub{run: func(model.Torrent) (classification.Result, error) {
				if kind == "classifier" {
					return classification.Result{}, classification.ErrDeleteTorrent
				}
				return classification.Result{}, nil
			}})
			cfg := contentfilter.NewDefaultConfig()
			cfg.Enabled = true
			cfg.Enforce = true
			cfg.BlockedExtensions = []string{"iso"}
			p.contentFilter = contentfilter.New(cfg)
			recorder := &receiptCapture{}
			p.coreRecorder = recorder
			id := strings.Repeat("a", 64)
			require.NoError(t, p.Process(context.Background(), MessageParams{ReprocessRequestID: id, InfoHashes: []protocol.ID{h}}))
			require.Empty(t, blocker.blockedHashes())
			require.NoError(t, mock.ExpectationsWereMet())
			require.Len(t, recorder.receipts, 1)
			require.Equal(t, id, recorder.receipts[0].ReprocessRequestID)
			require.Equal(t, "deferred", recorder.receipts[0].Processing)
			require.Equal(t, "deferred", recorder.receipts[0].Outcome)
		})
	}
}

func TestAuditedReprocessingRecordsNormalProcessingWithoutInventingAMiss(t *testing.T) {
	h := processTestHash(3)
	p, mock, _ := newProcessTestProcessor(t, []model.Torrent{processTestTorrent(h, "kept movie 2026", "mkv")}, processRunnerStub{run: func(model.Torrent) (classification.Result, error) {
		return classification.Result{ContentAttributes: classification.ContentAttributes{ContentType: model.NewNullContentType(model.ContentTypeMovie)}}, nil
	}})
	expectTorrentContentInsert(mock)
	recorder := &receiptCapture{}
	p.coreRecorder = recorder
	id := strings.Repeat("b", 64)
	require.NoError(t, p.Process(context.Background(), MessageParams{ReprocessRequestID: id, InfoHashes: []protocol.ID{h}}))
	require.NoError(t, mock.ExpectationsWereMet())
	require.Len(t, recorder.receipts, 1)
	require.Equal(t, id, recorder.receipts[0].ReprocessRequestID)
	require.Equal(t, "completed", recorder.receipts[0].Processing)
	require.Equal(t, "not_attempted", recorder.receipts[0].Outcome)
}

func TestAuditedReprocessingRejectsBypassMessages(t *testing.T) {
	p, _, _ := newProcessTestProcessor(t, nil, processRunnerStub{run: func(model.Torrent) (classification.Result, error) {
		t.Fatal("unexpected classifier call")
		return classification.Result{}, nil
	}})
	for _, params := range []MessageParams{
		{ReprocessRequestID: "bad"},
		{ReprocessRequestID: strings.Repeat("c", 64)},
		{ReprocessRequestID: strings.Repeat("c", 64), InfoHashes: []protocol.ID{{1}}, SkipContentFilter: true},
		{ReprocessRequestID: strings.Repeat("c", 64), InfoHashes: []protocol.ID{{1}}, ClassifyMode: ClassifyModeRematch},
	} {
		require.Error(t, p.Process(context.Background(), params))
	}
}

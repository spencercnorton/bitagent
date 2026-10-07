package processor

import (
	"context"
	"github.com/DATA-DOG/go-sqlmock"
	"github.com/spencercnorton/bitagent/internal/classifier/classification"
	"github.com/spencercnorton/bitagent/internal/model"
	"github.com/spencercnorton/bitagent/internal/namepolicy"
	"github.com/spencercnorton/bitagent/internal/protocol"
	"github.com/stretchr/testify/require"
	"testing"
)

func TestStoredNameDenialDoesNotHydrateClassifyBlockOrPersist(t *testing.T) {
	h := protocol.ID{1}
	p, mock, _ := newProcessTestProcessor(t, nil, processRunnerStub{run: func(model.Torrent) (classification.Result, error) {
		t.Fatal("classifier called after denial")
		return classification.Result{}, nil
	}})
	p.namePolicy, _ = namepolicy.New(namepolicy.Config{Enabled: true})
	mock.ExpectQuery(`SELECT .*name.* FROM "torrents"`).WillReturnRows(sqlmock.NewRows([]string{"info_hash", "name"}).AddRow(h.Bytes(), "Synthetic.电影.ENG.mkv"))
	require.NoError(t, p.Process(context.Background(), MessageParams{InfoHashes: []protocol.ID{h}}))
	require.NoError(t, mock.ExpectationsWereMet())
}

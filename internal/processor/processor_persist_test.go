package processor

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/spencercnorton/bitagent/internal/blocking"
	"github.com/spencercnorton/bitagent/internal/cataloguerecovery"
	"github.com/spencercnorton/bitagent/internal/classifier"
	"github.com/spencercnorton/bitagent/internal/classifier/classification"
	"github.com/spencercnorton/bitagent/internal/classifier/contentfilter"
	"github.com/spencercnorton/bitagent/internal/csamblocklist"
	"github.com/spencercnorton/bitagent/internal/database/dao"
	"github.com/spencercnorton/bitagent/internal/database/query"
	dbsearch "github.com/spencercnorton/bitagent/internal/database/search"
	"github.com/spencercnorton/bitagent/internal/llmcapture"
	"github.com/spencercnorton/bitagent/internal/model"
	"github.com/spencercnorton/bitagent/internal/protocol"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

type processSearchStub struct {
	dbsearch.Search
	torrents dbsearch.TorrentsWithMissingInfoHashesResult
	contents dbsearch.TorrentContentResult
}

func (s processSearchStub) TorrentsWithMissingInfoHashes(
	_ context.Context,
	_ []protocol.ID,
	_ ...query.Option,
) (dbsearch.TorrentsWithMissingInfoHashesResult, error) {
	return s.torrents, nil
}

func (s processSearchStub) TorrentContent(
	_ context.Context,
	_ ...query.Option,
) (dbsearch.TorrentContentResult, error) {
	return s.contents, nil
}

type processRunnerStub struct {
	classifier.Runner
	run func(model.Torrent) (classification.Result, error)
}

func (r processRunnerStub) Run(
	_ context.Context,
	_ string,
	_ classifier.Flags,
	torrent model.Torrent,
) (classification.Result, error) {
	return r.run(torrent)
}

var _ blocking.Manager = (*recordingBlockingManager)(nil)
type recordingBlockingManager struct {
	mu      sync.Mutex
	blocked []protocol.ID
	err     error
}

func (m *recordingBlockingManager) Block(
	_ context.Context,
	hashes []protocol.ID,
	_ string,
	_ string,
	_ bool,
) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.err != nil {
		return m.err
	}
	m.blocked = append(m.blocked, hashes...)
	return nil
}

func (m *recordingBlockingManager) Flush(context.Context) error { return nil }

func (m *recordingBlockingManager) blockedHashes() []protocol.ID {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]protocol.ID(nil), m.blocked...)
}

type processExporterStub struct {
	csamblocklist.Exporter
}

func (processExporterStub) Record(context.Context, protocol.ID, string, []string) {}

type unavailableProcessLLM struct{}

func (unavailableProcessLLM) Classify(
	context.Context,
	string,
) (contentfilter.LLMVerdict, error) {
	return contentfilter.LLMVerdict{}, contentfilter.ErrLLMUnavailable
}

func (unavailableProcessLLM) ClassifyWithResult(
	context.Context,
	string,
) (contentfilter.LLMVerdict, llmcapture.HTTPResult, error) {
	return contentfilter.LLMVerdict{}, llmcapture.HTTPResult{ErrorClass: "transport"}, contentfilter.ErrLLMUnavailable
}

func unavailableAuditedFilter(cfg contentfilter.Config) *contentfilter.Filter {
	return contentfilter.NewWithLLMAdmission(
		cfg,
		unavailableProcessLLM{},
		contentfilter.LLMCallbacks{},
		contentfilter.Admission{Budget: processorBudgetProbe{}, Capture: &processorCaptureProbe{}},
	)
}

func processTestHash(prefix byte) protocol.ID {
	return protocol.ID{prefix}
}

func processTestTorrent(hash protocol.ID, name, extension string) model.Torrent {
	return model.Torrent{
		InfoHash:  hash,
		Name:      name,
		Extension: model.NewNullString(extension),
		Size:      1024,
	}
}

func newProcessTestDAO(t *testing.T) (*dao.Query, sqlmock.Sqlmock) {
	t.Helper()

	sqlDB, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	t.Cleanup(func() { _ = sqlDB.Close() })

	db, err := gorm.Open(postgres.New(postgres.Config{Conn: sqlDB}), &gorm.Config{
		Logger:                 logger.Discard,
		SkipDefaultTransaction: true,
	})
	if err != nil {
		t.Fatalf("gorm.Open: %v", err)
	}

	return dao.Use(db), mock
}

func newProcessTestProcessor(
	t *testing.T,
	torrents []model.Torrent,
	runner processRunnerStub,
) (processor, sqlmock.Sqlmock, *recordingBlockingManager) {
	t.Helper()

	d, mock := newProcessTestDAO(t)
	blocker := &recordingBlockingManager{}

	return processor{
		defaultWorkflow: "test",
		search: processSearchStub{
			torrents: dbsearch.TorrentsWithMissingInfoHashesResult{Torrents: torrents},
		},
		runner:          runner,
		dao:             d,
		blockingManager: blocker,
		csamExporter:    processExporterStub{},
	}, mock, blocker
}

func expectTorrentContentInsert(mock sqlmock.Sqlmock) {
	mock.ExpectBegin()
	expectReleaseAttributeReads(mock, processTestHash(3), "kept movie 2026")
	// restoreLLMEnglishAudio: the kept row carries no english_audio signal, so
	// persist first looks for an existing 'llm'-sourced value to preserve.
	mock.ExpectQuery(`SELECT .*english_audio_source.* FROM "torrent_contents"`).
		WillReturnRows(sqlmock.NewRows([]string{"id", "english_audio", "english_audio_source"}))
	mock.ExpectQuery(`INSERT INTO "torrent_contents"`).
		WillReturnRows(sqlmock.NewRows([]string{"published_at"}).AddRow(nil))
	mock.ExpectCommit()
}

func expectQueueJob(mock sqlmock.Sqlmock, id string) {
	mock.ExpectQuery(`INSERT INTO "queue_jobs"`).
		WillReturnRows(sqlmock.NewRows([]string{"id", "status"}).AddRow(id, "pending"))
}

func TestProcessClassifierOnlyDeletePersistsWithoutInsert(t *testing.T) {
	hash := processTestHash(1)
	p, mock, blocker := newProcessTestProcessor(t,
		[]model.Torrent{processTestTorrent(hash, "blocked title", "mkv")},
		processRunnerStub{run: func(model.Torrent) (classification.Result, error) {
			return classification.Result{}, classification.ErrDeleteTorrent
		}},
	)

	if err := p.Process(context.Background(), MessageParams{InfoHashes: []protocol.ID{hash}}); err != nil {
		t.Fatalf("Process: %v", err)
	}
	if got := blocker.blockedHashes(); len(got) != 1 || got[0] != hash {
		t.Fatalf("blocked hashes = %v, want [%s]", got, hash)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("SQL expectations: %v", err)
	}
}

func TestProcessContentFilterOnlyDeletePersistsWithoutInsert(t *testing.T) {
	hash := processTestHash(2)
	p, mock, blocker := newProcessTestProcessor(t,
		[]model.Torrent{processTestTorrent(hash, "software image", "iso")},
		processRunnerStub{run: func(model.Torrent) (classification.Result, error) {
			return classification.Result{}, nil
		}},
	)
	cfg := contentfilter.NewDefaultConfig()
	cfg.Enabled = true
	cfg.Enforce = true
	cfg.BlockedExtensions = []string{"iso"}
	p.contentFilter = contentfilter.New(cfg)

	if err := p.Process(context.Background(), MessageParams{InfoHashes: []protocol.ID{hash}}); err != nil {
		t.Fatalf("Process: %v", err)
	}
	if got := blocker.blockedHashes(); len(got) != 1 || got[0] != hash {
		t.Fatalf("blocked hashes = %v, want [%s]", got, hash)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("SQL expectations: %v", err)
	}
}

func TestProcessMixedKeepAndDeletePersistsBoth(t *testing.T) {
	keepHash := processTestHash(3)
	deleteHash := processTestHash(4)
	p, mock, blocker := newProcessTestProcessor(t,
		[]model.Torrent{
			processTestTorrent(keepHash, "kept movie 2026", "mkv"),
			processTestTorrent(deleteHash, "blocked title", "mkv"),
		},
		processRunnerStub{run: func(torrent model.Torrent) (classification.Result, error) {
			if torrent.InfoHash == deleteHash {
				return classification.Result{}, classification.ErrDeleteTorrent
			}
			return classification.Result{
				ContentAttributes: classification.ContentAttributes{
					ContentType: model.NewNullContentType(model.ContentTypeMovie),
				},
			}, nil
		}},
	)
	expectTorrentContentInsert(mock)

	if err := p.Process(context.Background(), MessageParams{
		InfoHashes: []protocol.ID{keepHash, deleteHash},
	}); err != nil {
		t.Fatalf("Process: %v", err)
	}
	if got := blocker.blockedHashes(); len(got) != 1 || got[0] != deleteHash {
		t.Fatalf("blocked hashes = %v, want [%s]", got, deleteHash)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("SQL expectations: %v", err)
	}
}

func TestProcessDeletePersistsAlongsideFailedAndDeferredRepublishes(t *testing.T) {
	deleteHash := processTestHash(5)
	failedHash := processTestHash(6)
	deferredHash := processTestHash(7)
	p, mock, blocker := newProcessTestProcessor(t,
		[]model.Torrent{
			processTestTorrent(deleteHash, "blocked title", "mkv"),
			processTestTorrent(failedHash, "classifier failure", "mkv"),
			processTestTorrent(deferredHash, "Pelicula 2026", "mkv"),
		},
		processRunnerStub{run: func(torrent model.Torrent) (classification.Result, error) {
			switch torrent.InfoHash {
			case deleteHash:
				return classification.Result{}, classification.ErrDeleteTorrent
			case failedHash:
				return classification.Result{}, errors.New("classifier unavailable")
			default:
				return classification.Result{}, nil
			}
		}},
	)
	cfg := contentfilter.NewDefaultConfig()
	cfg.Enabled = true
	cfg.Enforce = true
	cfg.LLMEnabled = true
	cfg.LLMDeferOnUnavailable = true
	cfg.LLMDailyBudget = 10
	p.contentFilter = unavailableAuditedFilter(cfg)

	expectQueueJob(mock, "failed-republish")
	expectQueueJob(mock, "deferred-republish")

	if err := p.Process(context.Background(), MessageParams{
		InfoHashes: []protocol.ID{deleteHash, failedHash, deferredHash},
	}); err != nil {
		t.Fatalf("Process: %v", err)
	}
	if got := blocker.blockedHashes(); len(got) != 1 || got[0] != deleteHash {
		t.Fatalf("blocked hashes = %v, want [%s]", got, deleteHash)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("SQL expectations: %v", err)
	}
}

func TestProcessOnlyFailureStillReturnsWithoutRepublish(t *testing.T) {
	hash := processTestHash(8)
	p, mock, _ := newProcessTestProcessor(t,
		[]model.Torrent{processTestTorrent(hash, "classifier failure", "mkv")},
		processRunnerStub{run: func(model.Torrent) (classification.Result, error) {
			return classification.Result{}, errors.New("classifier unavailable")
		}},
	)

	if err := p.Process(context.Background(), MessageParams{InfoHashes: []protocol.ID{hash}}); err == nil {
		t.Fatal("Process error = nil, want classifier failure")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unexpected SQL: %v", err)
	}
}

func TestProcessOnlyDeferredRepublishesWithoutTransaction(t *testing.T) {
	hash := processTestHash(9)
	p, mock, blocker := newProcessTestProcessor(t,
		[]model.Torrent{processTestTorrent(hash, "Pelicula 2026", "mkv")},
		processRunnerStub{run: func(model.Torrent) (classification.Result, error) {
			return classification.Result{}, nil
		}},
	)
	cfg := contentfilter.NewDefaultConfig()
	cfg.Enabled = true
	cfg.Enforce = true
	cfg.LLMEnabled = true
	cfg.LLMDeferOnUnavailable = true
	cfg.LLMDailyBudget = 10
	p.contentFilter = unavailableAuditedFilter(cfg)
	expectQueueJob(mock, "deferred-republish")

	if err := p.Process(context.Background(), MessageParams{InfoHashes: []protocol.ID{hash}}); err != nil {
		t.Fatalf("Process: %v", err)
	}
	if got := blocker.blockedHashes(); len(got) != 0 {
		t.Fatalf("blocked hashes = %v, want none", got)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("SQL expectations: %v", err)
	}
}

func TestProcessNoPersistenceWorkAvoidsTransaction(t *testing.T) {
	p, mock, _ := newProcessTestProcessor(t, nil,
		processRunnerStub{run: func(model.Torrent) (classification.Result, error) {
			return classification.Result{}, nil
		}},
	)

	if err := p.Process(context.Background(), MessageParams{}); err != nil {
		t.Fatalf("Process: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unexpected SQL: %v", err)
	}
}

func TestProcessRecoveryOffHoldsLegacyDeleteWithoutSQLOrDeleteMetric(t *testing.T) {
	hash := processTestHash(10)
	p, mock, blocker := newProcessTestProcessor(t,
		[]model.Torrent{processTestTorrent(hash, "synthetic legacy CEL proposal", "mkv")},
		processRunnerStub{run: func(model.Torrent) (classification.Result, error) {
			return classification.Result{}, classification.ErrDeleteTorrent
		}},
	)
	blocker.err = cataloguerecovery.ErrDisabled
	p.deleteMetrics = NewDeleteMetrics()
	err := p.Process(context.Background(), MessageParams{InfoHashes: []protocol.ID{hash}})
	if !errors.Is(err, cataloguerecovery.ErrDisabled) {
		t.Fatalf("Process error = %v, want recovery disabled", err)
	}
	if len(blocker.blockedHashes()) != 0 {
		t.Fatal("held action was admitted to blocking")
	}
	if count := testutil.CollectAndCount(p.deleteMetrics.deleted); count != 0 {
		t.Fatalf("held deletion emitted %d delete counters", count)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("held deletion issued SQL: %v", err)
	}
}

func TestProcessRecoveryOffStillPersistsOrdinaryRowsInMixedBatch(t *testing.T) {
	keepHash, deleteHash := processTestHash(3), processTestHash(11)
	p, mock, blocker := newProcessTestProcessor(t,
		[]model.Torrent{processTestTorrent(keepHash, "kept movie 2026", "mkv"), processTestTorrent(deleteHash, "synthetic legacy proposal", "mkv")},
		processRunnerStub{run: func(torrent model.Torrent) (classification.Result, error) {
			if torrent.InfoHash == deleteHash {
				return classification.Result{}, classification.ErrDeleteTorrent
			}
			return classification.Result{ContentAttributes: classification.ContentAttributes{ContentType: model.NewNullContentType(model.ContentTypeMovie)}}, nil
		}},
	)
	blocker.err = cataloguerecovery.ErrDisabled
	expectTorrentContentInsert(mock)
	if err := p.Process(context.Background(), MessageParams{InfoHashes: []protocol.ID{keepHash, deleteHash}}); !errors.Is(err, cataloguerecovery.ErrDisabled) {
		t.Fatalf("Process error = %v, want recovery disabled", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("ordinary persistence: %v", err)
	}
}

func expectReleaseAttributeReads(mock sqlmock.Sqlmock, hash protocol.ID, name string) {
	mock.ExpectQuery(`SELECT .* FROM "torrents".*FOR UPDATE`).WithArgs(hash.Bytes(), 1).WillReturnRows(sqlmock.NewRows([]string{"info_hash", "name"}).AddRow(hash.Bytes(), name))
	mock.ExpectQuery(`SELECT .* FROM "torrent_contents".*FOR UPDATE`).WithArgs(hash.Bytes()).WillReturnRows(sqlmock.NewRows([]string{"info_hash", "release_attributes"}))
}

func (m *recordingBlockingManager) Filter(ctx context.Context, hashes []protocol.ID) ([]protocol.ID, error) { return hashes, nil }

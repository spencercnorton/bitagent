package importer

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/spencercnorton/bitagent/internal/blocking"
	"github.com/spencercnorton/bitagent/internal/csamblocklist"
	"github.com/spencercnorton/bitagent/internal/database/dao"
	"github.com/spencercnorton/bitagent/internal/model"
	"github.com/spencercnorton/bitagent/internal/processor"
	"github.com/spencercnorton/bitagent/internal/protocol"
	"gorm.io/gorm/clause"
)

type Importer interface {
	New(ctx context.Context, info Info) ActiveImport
}

type Item struct {
	Source          string
	InfoHash        protocol.ID
	Name            string
	Size            uint
	Private         bool
	ContentType     model.NullContentType
	ContentSource   model.NullString
	ContentID       model.NullString
	Title           model.NullString
	ReleaseDate     model.Date
	ReleaseYear     model.Year
	Episodes        model.Episodes
	VideoResolution model.NullVideoResolution
	VideoSource     model.NullVideoSource
	VideoCodec      model.NullVideoCodec
	Video3D         model.NullVideo3D
	VideoModifier   model.NullVideoModifier
	ReleaseGroup    model.NullString
	PublishedAt     time.Time
}

type Info struct {
	ID string
}

type importer struct {
	dao         *dao.Query
	bufferSize  uint
	maxWaitTime time.Duration
	// csamBlocklist + blockingManager close the /import bypass (design
	// §2.3, exam P2): before this, persistItems inserted any infohash
	// straight into the DB, skipping the CSAM feed bloom and the
	// self-observed blocklist bloom that the dhtcrawler enforces at
	// triage. Both are always non-nil in the fx graph (csam has a NoOp
	// fallback); nil only in narrow direct-construction tests, where the
	// gate degrades to pass-through.
	csamBlocklist   csamblocklist.Manager
	blockingManager blocking.Manager
	metrics         *Metrics
}

var ErrImportClosed = errors.New("import closed")

func (i importer) New(ctx context.Context, info Info) ActiveImport {
	ai := &activeImport{
		importer:        i,
		wg:              &sync.WaitGroup{},
		mutex:           &sync.RWMutex{},
		info:            info,
		itemChan:        make(chan Item),
		importedSources: make(map[string]struct{}),
	}
	ai.run(ctx)

	return ai
}

type ActiveImport interface {
	Import(items ...Item) error
	Drain()
	Closed() bool
	Close() error
	Err() error
}

type ImportItemsError struct {
	Items []Item
	Err   error
}

type ImportErrors []ImportItemsError

func (ImportErrors) Error() string {
	return "one or more items failed to import"
}

func (e ImportErrors) IsNil() bool {
	return len(e) == 0
}

func (e ImportErrors) OrNil() error {
	if e.IsNil() {
		return nil
	}

	return e
}

func (e ImportItemsError) Error() string {
	return e.Err.Error()
}

type activeImport struct {
	importer
	wg              *sync.WaitGroup
	stopped         bool
	mutex           *sync.RWMutex
	ctx             context.Context
	stop            context.CancelFunc
	info            Info
	itemChan        chan Item
	itemBuffer      []Item
	importedSources map[string]struct{}
	errors          ImportErrors
}

func (i *activeImport) run(ctx context.Context) {
	i.mutex.Lock()
	go (func() {
		iCtx, cancel := context.WithCancel(ctx)
		defer cancel()
		i.ctx = iCtx
		i.stop = cancel
		i.mutex.Unlock()
		for {
			select {
			case <-iCtx.Done():
				_ = i.Close()
				return
			case item, ok := <-i.itemChan:
				if !ok {
					return
				}
				go i.buffer(item)
			case <-time.After(i.maxWaitTime):
				go i.flush()
			}
		}
	})()
}

func (i *activeImport) buffer(item Item) {
	defer i.wg.Done()
	i.mutex.Lock()
	defer i.mutex.Unlock()

	i.itemBuffer = append(i.itemBuffer, item)
	if len(i.itemBuffer) >= int(i.bufferSize) {
		i.flushLocked()
	}
}

func (i *activeImport) flush() {
	i.mutex.Lock()
	defer i.mutex.Unlock()
	i.flushLocked()
}

func (i *activeImport) flushLocked() {
	if len(i.itemBuffer) == 0 {
		return
	}

	err := i.persistItems(i.itemBuffer...)
	if err != nil {
		i.errors = append(i.errors, ImportItemsError{
			Items: i.itemBuffer,
			Err:   err,
		})
	}

	i.itemBuffer = make([]Item, 0, i.bufferSize)
}

// gateItems runs the imported infohashes through the SAME ordered filter the
// dhtcrawler enforces at triage, closing the /import bloom bypass (design
// §2.3, mechanisms 6+7). CSAM first, unconditional and pure (a bloom test
// that cannot error). Blocking second, and it CAN error (a lazy bloom flush):
// on error we FAIL the whole batch rather than import unfiltered — a blocking
// lookup failure must never fall through to inserting a possibly-blocked
// hash. Returns the surviving items; a fully-filtered batch returns an empty
// slice and no error.
func (i *activeImport) gateItems(items []Item) ([]Item, error) {
	if len(items) == 0 {
		return items, nil
	}
	hashes := make([]protocol.ID, len(items))
	for idx, it := range items {
		hashes[idx] = it.InfoHash
	}

	before := len(hashes)
	if i.csamBlocklist != nil {
		hashes = i.csamBlocklist.Filter(hashes)
		if dropped := before - len(hashes); dropped > 0 {
			i.metrics.Gated("csam", dropped)
		}
	}

	if i.blockingManager != nil {
		afterCsam := len(hashes)
		kept, err := i.blockingManager.Filter(i.ctx, hashes)
		if err != nil {
			// Fail closed: refuse the batch, never import unfiltered.
			return nil, fmt.Errorf("import blocking gate: %w", err)
		}
		hashes = kept
		if dropped := afterCsam - len(hashes); dropped > 0 {
			i.metrics.Gated("blocking", dropped)
		}
	}

	if len(hashes) == len(items) {
		return items, nil // nothing dropped — avoid the rebuild
	}
	keep := make(map[protocol.ID]struct{}, len(hashes))
	for _, h := range hashes {
		keep[h] = struct{}{}
	}
	out := make([]Item, 0, len(hashes))
	for _, it := range items {
		if _, ok := keep[it.InfoHash]; ok {
			out = append(out, it)
		}
	}
	return out, nil
}

func (i *activeImport) persistItems(items ...Item) error {
	items, gateErr := i.gateItems(items)
	if gateErr != nil {
		return gateErr
	}
	if len(items) == 0 {
		return nil // whole batch filtered out — nothing to persist
	}

	var sources []*model.TorrentSource

	sourcesMap := make(map[string]struct{})
	torrents := make([]*model.Torrent, 0, len(items))
	torrentsTorrentSources := make([]*model.TorrentsTorrentSource, 0, len(items))
	torrentHints := make([]*model.TorrentHint, 0, len(items))
	infoHashes := make([]protocol.ID, 0, len(items))

	for _, item := range items {
		if _, ok1 := i.importedSources[item.Source]; !ok1 {
			if _, ok2 := sourcesMap[item.Source]; !ok2 {
				sources = append(sources, &model.TorrentSource{
					Key:  item.Source,
					Name: item.Source,
				})
				sourcesMap[item.Source] = struct{}{}
			}
		}

		torrent := createTorrentModel(i.info, item)
		if !torrent.Hint.IsNil() {
			hint := torrent.Hint
			torrentHints = append(torrentHints, &hint)
			torrent.Hint = model.TorrentHint{}
		}

		for _, s := range torrent.Sources {
			src := s
			torrentsTorrentSources = append(torrentsTorrentSources, &src)
		}

		torrent.Sources = nil
		torrents = append(torrents, &torrent)
		infoHashes = append(infoHashes, item.InfoHash)
	}

	job, jobErr := processor.NewQueueJob(processor.MessageParams{
		InfoHashes: infoHashes,
	}, model.QueueJobPriority(20))
	if jobErr != nil {
		return jobErr
	}

	return i.dao.Transaction(func(tx *dao.Query) error {
		if len(sources) > 0 {
			if createSourcesErr := tx.TorrentSource.WithContext(i.ctx).Clauses(clause.OnConflict{
				DoNothing: true,
			}).CreateInBatches(sources, 100); createSourcesErr != nil {
				return createSourcesErr
			}

			for _, s := range sources {
				i.importedSources[s.Key] = struct{}{}
			}
		}

		if createTorrentsErr := tx.Torrent.WithContext(i.ctx).Clauses(clause.OnConflict{
			DoNothing: true,
		}).CreateInBatches(torrents, 100); createTorrentsErr != nil {
			return createTorrentsErr
		}

		if len(torrentHints) > 0 {
			if createTorrentHintsErr := tx.TorrentHint.WithContext(i.ctx).Clauses(clause.OnConflict{
				UpdateAll: true,
			}).CreateInBatches(torrentHints, 100); createTorrentHintsErr != nil {
				return createTorrentHintsErr
			}
		}

		if createTorrentsTorrentSourcesErr := tx.TorrentsTorrentSource.WithContext(i.ctx).Clauses(clause.OnConflict{
			UpdateAll: true,
		}).CreateInBatches(torrentsTorrentSources, 100); createTorrentsTorrentSourcesErr != nil {
			return createTorrentsTorrentSourcesErr
		}

		return tx.QueueJob.Create(&job)
	})
}

func createTorrentModel(info Info, item Item) model.Torrent {
	t := model.Torrent{
		InfoHash:    item.InfoHash,
		Name:        item.Name,
		Size:        item.Size,
		Private:     item.Private,
		FilesStatus: model.FilesStatusNoInfo,
		Sources: []model.TorrentsTorrentSource{
			{
				InfoHash: item.InfoHash,
				Source:   item.Source,
				ImportID: model.NewNullString(info.ID),
				PublishedAt: sql.NullTime{
					Time:  item.PublishedAt,
					Valid: !item.PublishedAt.IsZero(),
				},
			},
		},
	}
	if item.ContentType.Valid {
		t.Hint = model.TorrentHint{
			InfoHash:        item.InfoHash,
			ContentType:     item.ContentType.ContentType,
			ContentSource:   item.ContentSource,
			ContentID:       item.ContentID,
			Title:           item.Title,
			ReleaseYear:     item.ReleaseYear,
			Episodes:        item.Episodes,
			VideoResolution: item.VideoResolution,
			VideoSource:     item.VideoSource,
			VideoCodec:      item.VideoCodec,
			Video3D:         item.Video3D,
			VideoModifier:   item.VideoModifier,
			ReleaseGroup:    item.ReleaseGroup,
		}
	}

	return t
}

func (i *activeImport) Import(items ...Item) error {
	i.mutex.Lock()
	defer i.mutex.Unlock()

	if i.stopped {
		return ErrImportClosed
	}

	i.wg.Add(len(items))

	for _, item := range items {
		i.itemChan <- item
	}

	return nil
}

func (i *activeImport) Drain() {
	i.wg.Wait()
}

func (i *activeImport) Err() error {
	i.mutex.RLock()
	defer i.mutex.RUnlock()

	return i.errors.OrNil()
}

func (i *activeImport) ImportErrors() ImportErrors {
	i.mutex.RLock()
	defer i.mutex.RUnlock()

	return i.errors
}

func (i *activeImport) Closed() bool {
	i.mutex.RLock()
	defer i.mutex.RUnlock()

	return i.stopped
}

func (i *activeImport) Close() error {
	i.mutex.Lock()
	defer i.mutex.Unlock()
	i.flushLocked()

	if !i.stopped {
		i.stopped = true
		i.stop()
		close(i.itemChan)
	}

	return i.errors.OrNil()
}

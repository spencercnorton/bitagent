// Package arrpoller polls Sonarr/Radarr/Readarr/Lidarr history
// endpoints for grab and import events as a reconciliation backstop
// to the webhook path. If a webhook delivery is lost (service down,
// network blip, misconfigured Connection), this poller fills the gap
// on the next interval.
//
// The poller is deliberately simple: it fetches the most recent page
// of history per eventType per instance, converts each entry to
// evidence, and lets the store's dedupe unique index absorb repeats.
// There is no cursor or paging — the next poll picks up anything new
// that landed since.
package arrpoller

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/spencercnorton/bitagent/internal/evidence"
	"github.com/spencercnorton/bitagent/internal/worker"
	"go.uber.org/fx"
	"go.uber.org/zap"
)

const (
	workerKey       = "evidence_arr_poller"
	httpTimeout     = 20 * time.Second
	historyPageSize = 100
)

type Params struct {
	fx.In
	Config    evidence.Config
	Store     *evidence.Store
	Metrics   *evidence.Metrics
	Freshness *evidence.Freshness
	Logger    *zap.SugaredLogger
}

type Result struct {
	fx.Out
	Worker worker.Worker `group:"workers"`
}

func New(p Params) Result {
	pl := &poller{
		instances: p.Config.ArrSlots(),
		interval:  p.Config.ArrPollInterval,
		store:     p.Store,
		metrics:   p.Metrics,
		freshness: p.Freshness,
		logger:    p.Logger.Named("arrpoller"),
	}
	return Result{Worker: worker.NewWorker(workerKey, fx.Hook{
		OnStart: pl.start,
		OnStop:  pl.stop,
	})}
}

type poller struct {
	instances []evidence.ConfiguredArrInstance
	interval  time.Duration
	store     *evidence.Store
	metrics   *evidence.Metrics
	freshness *evidence.Freshness
	logger    *zap.SugaredLogger

	cancel context.CancelFunc
	wg     sync.WaitGroup
}

func (p *poller) start(context.Context) error {
	if len(p.instances) == 0 {
		p.logger.Info("no arr instances configured; arrpoller disabled")
		return nil
	}
	if p.interval <= 0 {
		return errors.New("arrpoller: arr_poll_interval must be > 0 when instances are configured")
	}
	ctx, cancel := context.WithCancel(context.Background())
	p.cancel = cancel
	for _, inst := range p.instances {
		p.freshness.Track(
			inst.Kind,
			inst.Name,
			evidence.FreshnessWindow(p.interval),
		)
		p.wg.Add(1)
		go p.run(ctx, inst)
	}
	return nil
}

func (p *poller) stop(context.Context) error {
	if p.cancel != nil {
		p.cancel()
	}
	p.wg.Wait()
	return nil
}

func (p *poller) run(ctx context.Context, inst evidence.ConfiguredArrInstance) {
	defer p.wg.Done()
	log := p.logger.With("instance", inst.Name, "kind", inst.Kind)

	select {
	case <-ctx.Done():
		return
	case <-time.After(time.Duration(len(inst.Name)%30) * time.Second):
	}

	ticker := time.NewTicker(p.interval)
	defer ticker.Stop()

	p.pollOnce(ctx, inst, log)
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			p.pollOnce(ctx, inst, log)
		}
	}
}

func (p *poller) pollOnce(ctx context.Context, inst evidence.ConfiguredArrInstance, log *zap.SugaredLogger) {
	start := time.Now()
	defer func() {
		p.metrics.ObservePoll(inst.Kind, inst.Name, time.Since(start).Seconds())
	}()

	// eventType in *arr /api/v3/history is an integer enum:
	//   1 = grabbed
	//   3 = downloadFolderImported
	// Passing the string name returns HTTP 400 on Sonarr/Radarr 4.x.
	// We carry the canonical string form into evidence.Kind; only
	// the query parameter is numeric.
	// Every event type must succeed before the instance counts as fresh.
	// Marking success per fetch would let one permanently failing event type
	// hide behind a sibling that keeps working — the instance would read
	// healthy forever while half its feed was dark.
	cycleOK := true
	for _, eventType := range []eventTypeSpec{
		{code: 1, name: "grabbed"},
		{code: 3, name: "downloadFolderImported"},
	} {
		entries, err := fetchHistory(ctx, inst, eventType.code)
		if err != nil {
			cycleOK = false
			p.metrics.SourceError(inst.Kind, inst.Name, "fetch")
			log.Warnw("arrpoller fetch history", "eventType", eventType.name, "err", err)
			continue
		}
		for _, entry := range entries {
			ev, ok := entryToEvidence(inst, eventType.name, entry)
			if !ok {
				continue
			}
			p.metrics.Received(ev.Source, ev.Kind)
			cctx, cancel := context.WithTimeout(ctx, 3*time.Second)
			err := p.store.Insert(cctx, ev)
			cancel()
			switch {
			case err == nil:
				p.metrics.Persisted(ev.Source, ev.Kind)
				if len(ev.InfoHash) > 0 {
					p.metrics.CanonicalUpsert(ev.Source, ev.MediaType)
				}
			case errors.Is(err, evidence.ErrDuplicate):
				p.metrics.Duplicated(ev.Source, ev.Kind)
			case errors.Is(err, evidence.ErrNotUsable):
				p.metrics.Rejected(ev.Source, ev.Kind, "no_join_key")
			default:
				p.metrics.SourceError(ev.Source, inst.Name, "store")
				log.Warnw("arrpoller store insert", "err", err)
			}
		}
	}
	if cycleOK {
		p.freshness.MarkSuccess(inst.Kind, inst.Name)
	}
}

func entryToEvidence(inst evidence.ConfiguredArrInstance, eventType string, e arrHistoryEntry) (evidence.Evidence, bool) {
	// Map to our Kind + strength
	var kind evidence.Kind
	var strength uint8
	switch eventType {
	case "grabbed":
		kind = evidence.KindPollHistory
		strength = evidence.StrengthArrPollGrab
	case "downloadFolderImported":
		kind = evidence.KindPollHistory
		strength = evidence.StrengthArrPollImport
	default:
		return evidence.Evidence{}, false
	}
	// Media type follows the *arr kind.
	var mt evidence.MediaType
	switch inst.Kind {
	case evidence.SourceSonarr:
		mt = evidence.MediaTypeTV
	case evidence.SourceRadarr:
		mt = evidence.MediaTypeMovie
	case evidence.SourceReadarr:
		mt = evidence.MediaTypeBook
	case evidence.SourceLidarr:
		mt = evidence.MediaTypeMusic
	}

	var ih []byte
	if e.DownloadID != "" && len(e.DownloadID) == 40 {
		if b, err := hex.DecodeString(strings.ToLower(e.DownloadID)); err == nil && len(b) == 20 {
			ih = b
		}
	}
	raw, _ := json.Marshal(e)
	return evidence.Evidence{
		Source:         inst.Kind,
		Kind:           kind,
		SourceInstance: inst.Name,
		SourceObjectID: fmt.Sprintf("%s:%d", eventType, e.ID),
		DownloadID:     strings.ToLower(e.DownloadID),
		InfoHash:       ih,
		Title:          e.SourceTitle,
		MediaType:      mt,
		MediaID:        e.mediaID(inst.Kind),
		ObservedAt:     e.Date,
		Strength:       strength,
		RawPayload:     raw,
	}, true
}

type arrHistoryEntry struct {
	ID             int       `json:"id"`
	EventType      string    `json:"eventType"`
	Date           time.Time `json:"date"`
	DownloadID     string    `json:"downloadId"`
	DownloadClient string    `json:"downloadClient"`
	SourceTitle    string    `json:"sourceTitle"`
	MovieID        int       `json:"movieId"`
	SeriesID       int       `json:"seriesId"`
	EpisodeID      int       `json:"episodeId"`
	ArtistID       int       `json:"artistId"`
	AuthorID       int       `json:"authorId"`
}

func (e arrHistoryEntry) mediaID(src evidence.Source) string {
	switch src {
	case evidence.SourceRadarr:
		if e.MovieID != 0 {
			return fmt.Sprintf("radarr:%d", e.MovieID)
		}
	case evidence.SourceSonarr:
		if e.SeriesID != 0 {
			return fmt.Sprintf("sonarr:%d", e.SeriesID)
		}
	case evidence.SourceReadarr:
		if e.AuthorID != 0 {
			return fmt.Sprintf("readarr:%d", e.AuthorID)
		}
	case evidence.SourceLidarr:
		if e.ArtistID != 0 {
			return fmt.Sprintf("lidarr:%d", e.ArtistID)
		}
	}
	return ""
}

// eventTypeSpec pairs the *arr numeric enum code with the canonical
// string name we persist into label_evidence for downstream legibility.
type eventTypeSpec struct {
	code int
	name string
}

func fetchHistory(ctx context.Context, inst evidence.ConfiguredArrInstance, eventTypeCode int) ([]arrHistoryEntry, error) {
	u, err := url.Parse(strings.TrimRight(inst.BaseURL, "/") + "/api/v3/history")
	if err != nil {
		return nil, err
	}
	q := u.Query()
	q.Set("pageSize", fmt.Sprintf("%d", historyPageSize))
	q.Set("sortKey", "date")
	q.Set("sortDirection", "descending")
	q.Set("eventType", fmt.Sprintf("%d", eventTypeCode))
	u.RawQuery = q.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("X-Api-Key", inst.APIKey)
	resp, err := (&http.Client{Timeout: httpTimeout}).Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("arr history event=%d: http %d", eventTypeCode, resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if err != nil {
		return nil, err
	}
	var envelope struct {
		Records []arrHistoryEntry `json:"records"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil {
		return nil, fmt.Errorf("arr history event=%d decode: %w", eventTypeCode, err)
	}
	return envelope.Records, nil
}

// Package qbpoller polls one or more qBittorrent WebUI instances on a
// schedule, emitting evidence.Evidence records for every torrent whose
// category or tags convey a category signal the canonical label
// resolver cares about.
//
// qB categories are operator-assigned (e.g. "private", "public",
// "bitgrab" — see feedback_hades_scope memory). They are not ground
// truth for media type, but they are ground truth for "did this
// torrent come from a private tracker?" which is load-bearing for
// later-stage gates (e.g. the LLM classifier must never see private
// content).
package qbpoller

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
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
	workerKey    = "evidence_qb_poller"
	httpTimeout  = 20 * time.Second
	perCallLimit = 5000 // max torrents fetched per poll cycle per instance
)

// Params are the fx dependencies.
type Params struct {
	fx.In
	Config    evidence.Config
	Store     *evidence.Store
	Metrics   *evidence.Metrics
	Freshness *evidence.Freshness
	Logger    *zap.SugaredLogger
}

// Result exports the worker into the workers group and the poller
// value itself for tests that want to invoke a single cycle directly.
type Result struct {
	fx.Out
	Worker worker.Worker `group:"workers"`
}

// New wires the poller into the worker registry.
func New(p Params) Result {
	pl := &poller{
		instances:       p.Config.QBSlots(),
		interval:        p.Config.QBPollInterval,
		emitStateEvents: p.Config.Liveness.Enabled,
		store:           p.Store,
		metrics:         p.Metrics,
		freshness:       p.Freshness,
		logger:          p.Logger.Named("qbpoller"),
	}
	return Result{Worker: worker.NewWorker(workerKey, fx.Hook{
		OnStart: pl.start,
		OnStop:  pl.stop,
	})}
}

type poller struct {
	instances []evidence.ConfiguredQBInstance
	interval  time.Duration
	// emitStateEvents gates the per-torrent state observations the
	// liveness module consumes. Off by default so deploys that have
	// not opted in to liveness see exactly the previous evidence
	// flow (categories only).
	emitStateEvents bool
	store           *evidence.Store
	metrics         *evidence.Metrics
	freshness       *evidence.Freshness
	logger          *zap.SugaredLogger

	cancel context.CancelFunc
	wg     sync.WaitGroup
}

func (p *poller) start(context.Context) error {
	if len(p.instances) == 0 {
		p.logger.Info("no qb instances configured; qbpoller disabled")
		return nil
	}
	if p.interval <= 0 {
		return errors.New("qbpoller: qb_poll_interval must be > 0 when instances are configured")
	}

	ctx, cancel := context.WithCancel(context.Background())
	p.cancel = cancel

	for _, inst := range p.instances {
		p.freshness.Track(
			evidence.SourceQBittorrent,
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

func (p *poller) run(ctx context.Context, inst evidence.ConfiguredQBInstance) {
	defer p.wg.Done()
	log := p.logger.With("instance", inst.Name)

	// Stagger the first poll by up to 30s so N instances do not all
	// hammer the network at the same tick.
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

func (p *poller) pollOnce(ctx context.Context, inst evidence.ConfiguredQBInstance, log *zap.SugaredLogger) {
	start := time.Now()
	defer func() {
		p.metrics.ObservePoll(evidence.SourceQBittorrent, inst.Name, time.Since(start).Seconds())
	}()

	client, err := newClient(inst)
	if err != nil {
		p.metrics.SourceError(evidence.SourceQBittorrent, inst.Name, "auth")
		log.Warnw("qbpoller new client", "err", err)
		return
	}

	if err := client.login(ctx); err != nil {
		p.metrics.SourceError(evidence.SourceQBittorrent, inst.Name, "auth")
		log.Warnw("qbpoller login failed", "err", err)
		return
	}

	torrents, err := client.listTorrents(ctx)
	if err != nil {
		p.metrics.SourceError(evidence.SourceQBittorrent, inst.Name, "fetch")
		log.Warnw("qbpoller fetch torrents", "err", err)
		return
	}

	p.freshness.MarkSuccess(evidence.SourceQBittorrent, inst.Name)
	now := time.Now().UTC()

	for _, t := range torrents {
		// Category evidence — operator-assigned label. Only torrents
		// with a category produce a row.
		if ev, ok := torrentToEvidence(inst.Name, t); ok {
			p.persist(ctx, inst.Name, ev, log)
		}
		// State observation — separate evidence row, gated by the
		// state-class mapping AND the liveness master switch.
		// Carries the qB category alongside so the liveness
		// resolver can apply the private-tracker filter without
		// re-querying the store. When liveness is disabled we skip
		// the row entirely so disabled-by-default genuinely means
		// no DB writes.
		if p.emitStateEvents {
			if ev, ok := torrentToStateEvidence(inst.Name, t, now); ok {
				p.persist(ctx, inst.Name, ev, log)
			}
		}
	}
}

// persist runs Insert with the standard 3s per-row timeout and
// translates the result into metric increments. Extracted from
// pollOnce so the categories and state code paths can share it.
func (p *poller) persist(ctx context.Context, instance string, ev evidence.Evidence, log *zap.SugaredLogger) {
	p.metrics.Received(ev.Source, ev.Kind)

	cctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	err := p.store.Insert(cctx, ev)
	cancel()

	switch {
	case err == nil:
		p.metrics.Persisted(ev.Source, ev.Kind)
		if len(ev.InfoHash) > 0 && ev.Kind == evidence.KindPollCategories {
			p.metrics.CanonicalUpsert(ev.Source, ev.MediaType)
		}
	case errors.Is(err, evidence.ErrDuplicate):
		p.metrics.Duplicated(ev.Source, ev.Kind)
	case errors.Is(err, evidence.ErrNotUsable):
		p.metrics.Rejected(ev.Source, ev.Kind, "no_join_key")
	default:
		p.metrics.SourceError(ev.Source, instance, "store")
		log.Warnw("qbpoller store insert", "err", err, "hash", hex.EncodeToString(ev.InfoHash))
	}
}

// torrentToEvidence emits evidence only for torrents whose category
// or tags we can interpret. We deliberately do NOT emit evidence for
// every torrent — pre-categorization qB rows are noise and would bloat
// label_evidence without adding signal.
func torrentToEvidence(instance string, t qbTorrent) (evidence.Evidence, bool) {
	cat := strings.TrimSpace(strings.ToLower(t.Category))
	if cat == "" {
		return evidence.Evidence{}, false
	}
	ih, err := hex.DecodeString(t.Hash)
	if err != nil || len(ih) != 20 {
		return evidence.Evidence{}, false
	}
	strength := evidence.StrengthQBCategoryPublic
	if cat == "private" || cat == "bitgrab" {
		strength = evidence.StrengthQBCategoryPrivate
	}
	raw, mErr := json.Marshal(t)
	if mErr != nil {
		raw = nil
	}
	return evidence.Evidence{
		Source:         evidence.SourceQBittorrent,
		Kind:           evidence.KindPollCategories,
		SourceInstance: instance,
		SourceObjectID: t.Hash,
		InfoHash:       ih,
		Title:          t.Name,
		Category:       cat,
		ObservedAt:     time.Now().UTC(),
		Strength:       strength,
		RawPayload:     raw,
	}, true
}

// torrentToStateEvidence emits one Evidence row per torrent that
// carries a state in either the alive or suspect class. Ignored
// states produce no row, keeping label_evidence free of pure
// transient noise.
//
// The dedupe key is intentionally minute-bucketed: re-emitting the
// same state on every poll cycle within a minute is duplicate work,
// but a state that survives multiple minutes is a real observation
// the resolver should accumulate.
func torrentToStateEvidence(instance string, t qbTorrent, now time.Time) (evidence.Evidence, bool) {
	class := evidence.ClassifyQBState(t.State)
	if class == evidence.QBStateClassIgnore {
		return evidence.Evidence{}, false
	}
	ih, err := hex.DecodeString(t.Hash)
	if err != nil || len(ih) != 20 {
		return evidence.Evidence{}, false
	}
	bucket := now.Truncate(time.Minute).Format(time.RFC3339)
	objectID := "state:" + t.Hash + ":" + bucket

	strength := evidence.StrengthQBStateAlive
	if class == evidence.QBStateClassSuspect {
		strength = evidence.StrengthQBStateSuspect
	}

	cat := strings.TrimSpace(strings.ToLower(t.Category))
	raw, mErr := json.Marshal(t)
	if mErr != nil {
		raw = nil
	}
	return evidence.Evidence{
		Source:         evidence.SourceQBittorrent,
		Kind:           evidence.KindQBStateObservation,
		SourceInstance: instance,
		SourceObjectID: objectID,
		InfoHash:       ih,
		Title:          t.Name,
		Category:       cat,
		ObservedAt:     now,
		Strength:       strength,
		QBState:        t.State,
		RawPayload:     raw,
	}, true
}

// qbTorrent is the permissive subset of the qB /api/v2/torrents/info
// payload we consume. Fields not present in older qB versions are
// zero-valued, which we tolerate.
type qbTorrent struct {
	Hash     string `json:"hash"`
	Name     string `json:"name"`
	Category string `json:"category"`
	Tags     string `json:"tags"`
	State    string `json:"state"`
}

type client struct {
	inst    evidence.ConfiguredQBInstance
	http    *http.Client
	baseURL *url.URL
}

func newClient(inst evidence.ConfiguredQBInstance) (*client, error) {
	u, err := url.Parse(strings.TrimRight(inst.BaseURL, "/"))
	if err != nil {
		return nil, fmt.Errorf("parse base url: %w", err)
	}
	jar, err := cookiejar.New(nil)
	if err != nil {
		return nil, err
	}
	return &client{
		inst: inst,
		http: &http.Client{
			Timeout: httpTimeout,
			Jar:     jar,
		},
		baseURL: u,
	}, nil
}

func (c *client) login(ctx context.Context) error {
	form := url.Values{}
	form.Set("username", c.inst.Username)
	form.Set("password", c.inst.Password)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		c.baseURL.String()+"/api/v2/auth/login",
		strings.NewReader(form.Encode()))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Referer", c.baseURL.String())
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<10))
	return checkLoginResponse(resp.StatusCode, b)
}

// checkLoginResponse interprets a qB /api/v2/auth/login reply.
//
// qB answers this endpoint three different ways:
//
//	200 "Ok."      — credentials accepted, cookie issued
//	200 "Fails."   — credentials rejected
//	403            — client IP temporarily banned for failed attempts
//	204 (no body)  — auth is bypassed for this client, so there was
//	                 nothing to log in to (WebUI\AuthSubnetWhitelist or
//	                 WebUI\LocalHostAuth). Observed on qB 5.2.3 /
//	                 WebAPI 2.15.1.
//
// Requiring exactly `200 "Ok."` treated the fourth case as a hard
// failure, which took the production qB evidence feed dark from
// 2026-07-15 to 2026-07-26: the poller aborted at login on every
// cycle even though torrents/info was returning 200 with data.
//
// Any 2xx with an empty body is therefore accepted. This does not
// weaken the check — a genuinely unauthenticated session still fails
// loudly at the next call, because torrents/info answers 403.
func checkLoginResponse(status int, body []byte) error {
	if status/100 != 2 {
		return fmt.Errorf("qb login: http %d", status)
	}
	if b := strings.TrimSpace(string(body)); b != "" && b != "Ok." {
		return fmt.Errorf("qb login: unexpected body %q", b)
	}
	return nil
}

func (c *client) listTorrents(ctx context.Context) ([]qbTorrent, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		fmt.Sprintf("%s/api/v2/torrents/info?limit=%d", c.baseURL.String(), perCallLimit),
		nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("qb torrents/info: http %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20)) // 32 MB hard cap
	if err != nil {
		return nil, err
	}
	var list []qbTorrent
	if err := json.Unmarshal(body, &list); err != nil {
		return nil, fmt.Errorf("qb torrents/info decode: %w", err)
	}
	return list, nil
}

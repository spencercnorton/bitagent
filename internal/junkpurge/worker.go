package junkpurge

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/spencercnorton/bitagent/internal/lazy"
	"github.com/spencercnorton/bitagent/internal/llmcapture"
	"github.com/spencercnorton/bitagent/internal/llmprovider"
	"github.com/spencercnorton/bitagent/internal/verdicts"
	"github.com/spencercnorton/bitagent/internal/worker"
	"go.uber.org/fx"
	"go.uber.org/zap"
	"gorm.io/gorm"
)

const workerKey = "junkpurge"

const (
	cycleOutcomeInternalError      = "internal_error"
	cycleOutcomeQueryError         = "query_error"
	cycleOutcomeNoCandidates       = "no_candidates"
	cycleOutcomeCanceled           = "canceled"
	cycleOutcomeCaptureUnavailable = "capture_unavailable"
	cycleOutcomeCaptureOnly        = "capture_only"
	cycleOutcomeLLMUnavailable     = "llm_unavailable"
	cycleOutcomeJunkRateAnomaly    = "junk_rate_anomaly"
	cycleOutcomeNoJunk             = "no_confident_junk"
	cycleOutcomeDryRun             = "dry_run"
	cycleOutcomeQuarantineError    = "quarantine_error"
	cycleOutcomeQuarantined        = "quarantined"
	cycleOutcomeBatchDryRun        = "batch_dry_run"
	cycleOutcomeBatchBreaker       = "batch_junk_rate_anomaly"
	cycleOutcomeBatchCompleted     = "batch_completed"
	judgmentReasonSyncPending      = "sync_cycle:pending"
	judgmentReasonBatchRun         = "batch_run"
)

type Params struct {
	fx.In
	Config  Config
	Pool    lazy.Lazy[*pgxpool.Pool]
	GormDB  lazy.Lazy[*gorm.DB]
	Judge   Judge
	Metrics *Metrics
	Logger  *zap.SugaredLogger
	// Capture is disabled by default. When enabled, hosted junk work fails
	// closed unless its exact per-item request is durably admitted. If both
	// paid paths are fused off, it also enables an observation-only capture
	// loop that never calls the judge.
	Capture llmcapture.Capturer `optional:"true"`
	// Verdicts is the T3 phase-A ledger (nil-safe optional).
	Verdicts *verdicts.Store `optional:"true"`
}

type Result struct {
	fx.Out
	Worker worker.Worker `group:"workers"`
}

// New wires the junk-purge worker. Config.Enabled=false admits no new work and
// never purges; a drain-only reconciler may still collect already-paid Batch
// output left by an earlier enabled process.
func New(p Params) Result {
	w := &purgeWorker{
		cfg:             p.Config,
		pool:            p.Pool,
		gormDB:          p.GormDB,
		judge:           p.Judge,
		metrics:         p.Metrics,
		logger:          p.Logger.Named("junkpurge"),
		capture:         p.Capture,
		verdicts:        p.Verdicts,
		batchLeaseOwner: newBatchLeaseOwner(),
	}
	return Result{Worker: worker.NewWorker(workerKey, fx.Hook{
		OnStart: w.start,
		OnStop:  w.stop,
	})}
}

type purgeWorker struct {
	cfg     Config
	pool    lazy.Lazy[*pgxpool.Pool]
	gormDB  lazy.Lazy[*gorm.DB]
	judge   Judge
	metrics *Metrics
	logger  *zap.SugaredLogger
	capture llmcapture.Capturer
	// verdicts is the T3 phase-A dual-write hook (nil-safe): quarantine,
	// expiry-blacklist and operator actions are mirrored into the verdict
	// ledger alongside the existing bookkeeping. Best-effort — a ledger
	// error is logged and never breaks the purge path.
	verdicts *verdicts.Store

	// batchLeaseOwner makes provider POST boundaries single-owner across
	// multiple BitAgent replicas while allowing recovery after lease expiry.
	batchLeaseOwner string

	cancel context.CancelFunc
	wg     sync.WaitGroup
}

type workerProcessingPlan struct {
	name             string
	startSyncLoop    bool
	startBatchLoop   bool
	admitNewBatch    bool
	allowBatchPurge  bool
	requireBatchIdle bool
}

func processingPlan(
	cfg Config,
	capture llmcapture.Capturer,
) workerProcessingPlan {
	switch {
	case cfg.Enabled && cfg.LLMBatchEnabled:
		return workerProcessingPlan{
			name:            "batch",
			startBatchLoop:  true,
			admitNewBatch:   true,
			allowBatchPurge: cfg.EnablePurge,
		}
	case cfg.Enabled && allowStandardLLM(cfg):
		return workerProcessingPlan{
			name:           "standard+batch-drain",
			startSyncLoop:  true,
			startBatchLoop: true,
		}
	case allowCaptureOnly(cfg, capture):
		return workerProcessingPlan{
			name:             "capture-only",
			startSyncLoop:    true,
			requireBatchIdle: true,
		}
	default:
		return workerProcessingPlan{
			name:           "batch-drain-only",
			startBatchLoop: true,
		}
	}
}

func (w *purgeWorker) start(context.Context) error {
	if err := validateWorkerConfig(w.cfg); err != nil {
		return err
	}
	// The parent-scoped GORM decorator applies migrations lazily on Get.
	// Initialize it before launching the raw-pgx Batch reconciler so selected
	// junkpurge-only worker runs cannot wait forever on a schema that nothing
	// else will create.
	if _, err := w.gormDB.Get(); err != nil {
		return fmt.Errorf("junkpurge initialize migrated database: %w", err)
	}
	plan := processingPlan(w.cfg, w.capture)
	if plan.requireBatchIdle {
		pool, err := w.pool.Get()
		if err != nil {
			return fmt.Errorf(
				"junkpurge capture-only acquire pool: %w",
				err,
			)
		}
		if err := requireCaptureOnlyBatchIdle(
			context.Background(),
			pool,
		); err != nil {
			return err
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	w.cancel = cancel
	if plan.startSyncLoop {
		w.wg.Add(1)
		go w.loop(ctx)
	}
	if plan.startBatchLoop {
		client := newOpenAIBatchClient(w.cfg)
		w.wg.Add(1)
		go w.batchLoop(
			ctx,
			client,
			plan.admitNewBatch,
			plan.allowBatchPurge,
		)
	}
	if w.cfg.Enabled && w.cfg.LLMApiStyle == "chat" &&
		!w.cfg.LLMBatchEnabled && !w.cfg.LLMAllowPaidSync {
		if allowCaptureOnly(w.cfg, w.capture) {
			w.logger.Warn(
				"junkpurge capture-only collection active; paid synchronous and new Batch calls remain disabled",
			)
		} else {
			w.logger.Warn(
				"junkpurge paid synchronous calls are disabled; draining existing Batch work only",
			)
		}
	}
	mode := workerMode(w.cfg)
	if allowCaptureOnly(w.cfg, w.capture) {
		mode = "CAPTURE-ONLY"
	}
	w.logger.Infow("junkpurge started", "mode", mode,
		"llm_processing", plan.name,
		"min_age", w.cfg.MinAge, "min_confidence", w.cfg.MinConfidence,
		"batch_size", w.cfg.BatchSize, "interval", w.cfg.Interval)
	return nil
}

func validateWorkerConfig(cfg Config) error {
	if !cfg.Enabled {
		return nil
	}
	if cfg.Interval <= 0 || cfg.MinAge <= 0 ||
		cfg.BatchSize <= 0 || cfg.RejudgeInterval <= 0 {
		return errors.New(
			"junkpurge: interval, min_age, batch_size and rejudge_interval must be positive when enabled",
		)
	}
	if math.IsNaN(cfg.MinConfidence) || math.IsInf(cfg.MinConfidence, 0) ||
		cfg.MinConfidence <= 0 || cfg.MinConfidence > 1 {
		return errors.New("junkpurge: min_confidence must be in (0,1]")
	}
	if math.IsNaN(cfg.MaxJunkRate) || math.IsInf(cfg.MaxJunkRate, 0) ||
		cfg.MaxJunkRate <= 0 || cfg.MaxJunkRate > 1 {
		return errors.New("junkpurge: max_junk_rate must be in (0,1]")
	}
	if cfg.LLMApiStyle != "ollama" && cfg.LLMApiStyle != "chat" {
		return errors.New("junkpurge: llm_api_style must be ollama or chat")
	}
	if err := llmprovider.ValidateDataSharingChatBaseURL(
		cfg.LLMOpenaiDataSharing, cfg.LLMBaseURL, cfg.LLMApiStyle,
		cfg.LLMModel, cfg.LLMApiKey, "",
	); err != nil {
		return fmt.Errorf("junkpurge: %w", err)
	}
	if cfg.LLMOpenaiDataSharing && cfg.LLMBatchEnabled {
		return errors.New("junkpurge: OpenAI data-sharing route requires synchronous chat; Batch incentive eligibility is not asserted")
	}
	if cfg.LLMApiStyle == "chat" && cfg.LLMAllowPaidSync &&
		(cfg.LLMDailyCallLimit <= 0 || cfg.LLMMonthlyCallLimit <= 0) {
		return errors.New("junkpurge: paid synchronous chat requires positive daily and monthly call limits")
	}
	// A non-positive cooldown would restore the unbounded retry this exists to
	// stop, so it fails startup rather than silently degrading.
	if cfg.LLMFailureCooldown <= 0 {
		return errors.New("junkpurge: llm_failure_cooldown must be positive")
	}
	if cfg.LLMNamesPerCall < 1 || cfg.LLMNamesPerCall > 50 {
		return errors.New("junkpurge: llm_names_per_call must be in 1..50")
	}
	if cfg.LLMUnavailableConsecutiveLimit < 1 ||
		cfg.LLMUnavailableConsecutiveLimit > 10 {
		return errors.New(
			"junkpurge: llm_unavailable_consecutive_limit must be in 1..10",
		)
	}
	return validateBatchConfig(cfg)
}

// judgeGroupWithFallback judges a group of names with one grouped request,
// falling back to per-name calls when the grouped reply is unusable. The
// returned slices are aligned with names; only the first `processed` entries
// are meaningful, and for each of those exactly one of judgments[i]
// (itemErrs[i]==nil) or itemErrs[i] holds. unavailableErr preserves an
// ErrLLMUnavailable from either path — the caller records the processed
// prefix FIRST and then trips the cycle breaker, so a mid-group outage never
// discards (and later re-buys) judgments that already succeeded, matching
// the classic strictly-sequential loop's behaviour.
func judgeGroupWithFallback(
	ctx context.Context, judge Judge, names []string, metrics *Metrics,
) (judgments []Judgment, itemErrs []error, processed int, unavailableErr error) {
	if len(names) > 1 {
		js, err := judge.JudgeBatch(ctx, names)
		if err == nil {
			return js, make([]error, len(names)), len(names), nil
		}
		if errors.Is(err, ErrLLMUnavailable) {
			return nil, nil, 0, err
		}
		// The grouped reply was unusable (wrong count, bad index set, an
		// invalid item). Buy the group back at the classic per-name price —
		// a format regression must cost money, never coverage.
		if metrics != nil {
			metrics.cycleErrorsTotal.WithLabelValues("judge_group").Inc()
		}
	}
	judgments = make([]Judgment, len(names))
	itemErrs = make([]error, len(names))
	for i, name := range names {
		if ctx.Err() != nil {
			return judgments, itemErrs, i, ctx.Err()
		}
		jm, err := judge.Judge(ctx, name)
		if errors.Is(err, ErrLLMUnavailable) {
			return judgments, itemErrs, i, err
		}
		judgments[i], itemErrs[i] = jm, err
	}
	return judgments, itemErrs, len(names), nil
}

type unavailableGroup struct {
	err         error
	size        int
	processed   int
	consecutive int
}

type syncJudgingSummary struct {
	hadUnavailable    bool
	unavailableGroups []unavailableGroup
	deferred          int
	halted            bool
	contextErr        error
}

// evaluateCandidateGroups isolates a transient failure to its group and keeps
// collecting durable evaluation judgments from later groups. After
// consecutiveUnavailableLimit failures it stops probing so a real provider
// outage cannot turn into a full-cycle request storm.
//
// The caller MUST still block every destructive action when hadUnavailable is
// true. Continuing here improves observation coverage only; it never makes a
// partial cycle eligible for quarantine.
func evaluateCandidateGroups(
	ctx context.Context,
	judge Judge,
	candidates []candidate,
	groupSize int,
	consecutiveUnavailableLimit int,
	metrics *Metrics,
	consume func(context.Context, candidate, Judgment, error),
) syncJudgingSummary {
	if groupSize < 1 {
		groupSize = 1
	}
	if consecutiveUnavailableLimit < 1 {
		consecutiveUnavailableLimit = 1
	}

	var summary syncJudgingSummary
	consecutiveUnavailable := 0
	for start := 0; start < len(candidates); start += groupSize {
		if ctx.Err() != nil {
			summary.contextErr = ctx.Err()
			return summary
		}
		group := candidates[start:min(start+groupSize, len(candidates))]
		names := make([]string, len(group))
		for i, item := range group {
			names[i] = item.name
		}
		judgments, itemErrs, processed, unavailableErr := judgeGroupWithFallback(
			ctx, judge, names, metrics,
		)
		consumeProcessed := func(consumeCtx context.Context) {
			for i := 0; i < processed; i++ {
				consume(consumeCtx, group[i], judgments[i], itemErrs[i])
			}
		}
		if ctx.Err() != nil {
			// A grouped-format fallback can finish one or more paid single-name
			// judgments before cancellation is observed at the next item. Preserve
			// that completed prefix with one bounded detached recording context;
			// the caller still returns canceled and cannot reach quarantine.
			recordCtx, cancel := context.WithTimeout(
				context.WithoutCancel(ctx), 10*time.Second,
			)
			consumeProcessed(recordCtx)
			cancel()
			summary.contextErr = ctx.Err()
			return summary
		}
		consumeProcessed(ctx)
		if unavailableErr == nil {
			consecutiveUnavailable = 0
			continue
		}

		summary.hadUnavailable = true
		consecutiveUnavailable++
		summary.deferred += len(group) - processed
		summary.unavailableGroups = append(summary.unavailableGroups, unavailableGroup{
			err:         unavailableErr,
			size:        len(group),
			processed:   processed,
			consecutive: consecutiveUnavailable,
		})
		if consecutiveUnavailable >= consecutiveUnavailableLimit {
			summary.halted = true
			summary.deferred += len(candidates) - (start + len(group))
			return summary
		}
	}
	return summary
}

func allowStandardLLM(cfg Config) bool {
	return cfg.LLMApiStyle == "ollama" || cfg.LLMAllowPaidSync
}

func allowCaptureOnly(cfg Config, capture llmcapture.Capturer) bool {
	return cfg.Enabled &&
		cfg.LLMApiStyle == "chat" &&
		!cfg.LLMBatchEnabled &&
		!cfg.LLMAllowPaidSync &&
		capture != nil &&
		capture.Enabled()
}

func allowDestructiveExpiry(
	cfg Config,
	capture llmcapture.Capturer,
) bool {
	return cfg.EnablePurge && !allowCaptureOnly(cfg, capture)
}

func workerMode(cfg Config) string {
	if !cfg.Enabled ||
		(!cfg.LLMBatchEnabled && !allowStandardLLM(cfg)) {
		return "DRAIN-ONLY"
	}
	if cfg.EnablePurge {
		return "LIVE-DELETE"
	}
	return "DRY-RUN"
}

func (w *purgeWorker) stop(context.Context) error {
	if w.cancel != nil {
		w.cancel()
	}
	w.wg.Wait()
	return nil
}

func (w *purgeWorker) loop(ctx context.Context) {
	defer w.wg.Done()
	// Offset the first cycle so we don't slam the DB + LLM at startup.
	select {
	case <-ctx.Done():
		return
	case <-time.After(5 * time.Minute):
	}
	t := time.NewTicker(w.cfg.Interval)
	defer t.Stop()
	w.runCycle(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			w.runCycle(ctx)
		}
	}
}

type candidate struct {
	infoHash []byte
	name     string
}

func (w *purgeWorker) runCycle(ctx context.Context) {
	start := time.Now()
	cycleOutcome := cycleOutcomeInternalError
	var junk [][]byte
	judged := 0
	defer func() {
		w.metrics.cyclesTotal.Inc()
		w.metrics.observeCycleOutcome(cycleOutcome, judged, len(junk))
		w.metrics.cycleDuration.Observe(time.Since(start).Seconds())
		w.metrics.lastCycleUnix.Set(float64(time.Now().Unix()))
	}()

	pool, err := w.pool.Get()
	if err != nil {
		cycleOutcome = cycleOutcomeQueryError
		w.metrics.cycleErrorsTotal.WithLabelValues("query").Inc()
		w.logger.Warnw("junkpurge acquire pool", "err", err)
		return
	}

	// Capture-only is categorically observation-only, even if a stale or
	// contradictory deployment still has EnablePurge=true. Compute the mode
	// before any quarantine lifecycle action so enabling prospective capture
	// cannot turn the otherwise drain-only hosted configuration into a
	// destructive worker.
	captureOnly := allowCaptureOnly(w.cfg, w.capture)

	// Expire quarantine entries past the review window: blacklist + hard-delete.
	// Gated on EnablePurge (T3 phase 0) and the absence of capture-only mode:
	// both dry-run and prospective-capture modes must be observation-only.
	// Before this gate a dry-run evaluation still permanently deleted
	// previously-quarantined rows once their window lapsed — verified live
	// (exam P2; worker.go:134 ran unconditionally while :200 gated only the
	// new-candidate path).
	if allowDestructiveExpiry(w.cfg, w.capture) {
		w.expireQuarantine(ctx, pool)
	} else {
		w.logWouldExpire(ctx, pool)
	}

	var candidates []candidate
	var captureTopUps []candidate
	if captureOnly {
		candidates, err = findCaptureCandidates(ctx, pool, w.cfg)
		if err == nil {
			captureTopUps, err = findCaptureSafetyTopUpCandidates(
				ctx,
				pool,
				w.cfg,
			)
		}
	} else {
		candidates, err = findCandidates(ctx, pool, w.cfg)
	}
	if err != nil {
		cycleOutcome = cycleOutcomeQueryError
		w.metrics.cycleErrorsTotal.WithLabelValues("query").Inc()
		w.logger.Warnw("junkpurge find candidates", "err", err)
		return
	}
	if len(candidates)+len(captureTopUps) == 0 {
		cycleOutcome = cycleOutcomeNoCandidates
		return
	}
	w.metrics.candidatesTotal.Add(
		float64(len(candidates) + len(captureTopUps)),
	)

	claimTTL := time.Duration(w.cfg.BatchSize)*w.cfg.timeout() + 30*time.Minute
	if claimTTL < 2*time.Hour {
		claimTTL = 2 * time.Hour
	}
	var claimed [][]byte
	var recordedClaims [][]byte
	var cooledClaims [][]byte
	defer func() {
		if len(claimed) == 0 {
			return
		}
		releaseCtx, cancel := context.WithTimeout(
			context.WithoutCancel(ctx), 10*time.Second,
		)
		defer cancel()
		if err := settleSyncClaims(
			releaseCtx, pool, w.batchLeaseOwner, claimed, recordedClaims,
			cooledClaims, w.cfg.RejudgeInterval, w.cfg.LLMFailureCooldown,
		); err != nil {
			w.logger.Warnw("junkpurge settle sync claims", "err", err)
		}
	}()

	captured := 0
	captureUnavailable := false
	// Phase 1: claim + capture. Judging is deferred to phase 2 so names can
	// be grouped into shared LLM calls (LLMNamesPerCall). Claims are held for
	// the whole cycle either way (claimTTL >> cycle); an item claimed here
	// but never judged (breaker trip mid-phase-2) is released at settle,
	// exactly as an unclaimed item would have been.
	var toJudge []candidate
	for _, c := range candidates {
		if ctx.Err() != nil {
			cycleOutcome = cycleOutcomeCanceled
			return
		}
		owned, claimErr := claimSyncCandidate(
			ctx, pool, c.infoHash, c.name, w.batchLeaseOwner, claimTTL, w.cfg,
		)
		if claimErr != nil {
			w.metrics.cycleErrorsTotal.WithLabelValues("claim").Inc()
			continue
		}
		if !owned {
			continue
		}
		claimed = append(claimed, c.infoHash)
		if captureErr := w.captureSyncCandidate(ctx, c); captureErr != nil {
			if errors.Is(captureErr, llmcapture.ErrPrivacyBlocked) {
				continue
			}
			w.metrics.cycleErrorsTotal.WithLabelValues("capture").Inc()
			captureUnavailable = true
			break
		}
		if captureOnly {
			captured++
			continue
		}
		toJudge = append(toJudge, c)
	}
	if captureUnavailable && !captureOnly {
		cycleOutcome = cycleOutcomeCaptureUnavailable
		w.metrics.circuitBreaks.WithLabelValues("capture_unavailable").Inc()
		w.logger.Warnw(
			"junkpurge evaluation capture unavailable; no model calls were made",
			"captured", len(toJudge),
			"deferred", len(candidates)-len(toJudge),
		)
		return
	}

	// Phase 2: judge in groups. A transient provider failure blocks all
	// destructive application for this cycle, but is isolated to its group so
	// later successfully-evaluated groups still become durable evaluation data.
	// A bounded consecutive-failure limit stops probes during a real outage.
	judging := evaluateCandidateGroups(
		ctx,
		w.judge,
		toJudge,
		w.cfg.LLMNamesPerCall,
		w.cfg.LLMUnavailableConsecutiveLimit,
		w.metrics,
		func(recordCtx context.Context, c candidate, j Judgment, jerr error) {
			if jerr != nil {
				// Per-item failure (4xx / unparseable). Skip the item; do not
				// trip the breaker — the endpoint is up, this one reply was bad.
				//
				// Log it. This counter was previously the ONLY trace of the
				// failure, and a metric alone cannot say WHICH title or WHY: one
				// torrent whose reply carried a one-character typo in the verdict
				// enum went unnoticed for ~967 cycles because nothing named it.
				w.metrics.cycleErrorsTotal.WithLabelValues("judge").Inc()
				w.logger.Warnw("junkpurge: judge rejected the reply, cooling down",
					"info_hash", hex.EncodeToString(c.infoHash),
					"name", c.name,
					"cooldown", w.cfg.LLMFailureCooldown,
					"err", jerr,
				)
				// Cool it rather than releasing it. Without this the claim is
				// deleted at settle time and the item is eligible again on the
				// very next cycle — so a deterministic bad reply is re-bought
				// every hour, forever.
				//
				// ponytail: a flat cooldown, not an N-strikes counter. This turns
				// an unbounded loop into one call per cooldown (24h default), with
				// no schema change. If a persistent poison pill still costs too
				// much, the upgrade is a failure count on junkpurge_sync_claims
				// and a give-up after N — that needs a migration, so it is not
				// bought until the cooldown proves insufficient.
				cooledClaims = append(cooledClaims, c.infoHash)
				return
			}
			w.metrics.judgedTotal.WithLabelValues(j.Verdict).Inc()
			recorded, rerr := recordClaimedJudgment(
				recordCtx, pool, c.infoHash, j, c.name, w.batchLeaseOwner, w.cfg,
			)
			if rerr != nil {
				w.metrics.cycleErrorsTotal.WithLabelValues("record").Inc()
				return
			}
			if !recorded {
				return
			}
			judged++
			recordedClaims = append(recordedClaims, c.infoHash)
			if j.IsJunk(w.cfg.MinConfidence) {
				junk = append(junk, c.infoHash)
			}
		},
	)
	if judging.contextErr != nil {
		cycleOutcome = cycleOutcomeCanceled
		// The judgments from completed groups are already durable. Finalize their
		// cohort marker even though the caller canceled, just as claim settlement
		// below uses a bounded detached context. This is observational only and
		// cannot reach quarantine.
		outcomeCtx, cancel := context.WithTimeout(
			context.WithoutCancel(ctx), 10*time.Second,
		)
		w.markSyncJudgmentOutcome(
			outcomeCtx, pool, recordedClaims, cycleOutcomeCanceled,
		)
		cancel()
		return
	}
	for _, failure := range judging.unavailableGroups {
		w.logger.Warnw(
			"junkpurge: provider unavailable for one group; cycle application blocked while evaluation continues",
			"reason", llmFailureReason(failure.err),
			"group_size", failure.size,
			"processed", failure.processed,
			"consecutive", failure.consecutive,
			"consecutive_limit", w.cfg.LLMUnavailableConsecutiveLimit,
			"err", failure.err,
		)
	}
	topUpsCaptured := 0
	if captureOnly && !captureUnavailable {
		for _, topUp := range captureTopUps {
			if ctx.Err() != nil {
				cycleOutcome = cycleOutcomeCanceled
				return
			}
			captureErr := w.captureSyncCandidateWithOrigin(
				ctx,
				topUp,
				llmcapture.SamplingOriginSafetyTopUpCapture,
			)
			if errors.Is(captureErr, llmcapture.ErrPrivacyBlocked) {
				continue
			}
			if captureErr != nil {
				w.metrics.cycleErrorsTotal.WithLabelValues("capture").Inc()
				captureUnavailable = true
				break
			}
			topUpsCaptured++
		}
	}
	if captureOnly {
		if captureUnavailable {
			cycleOutcome = cycleOutcomeCaptureUnavailable
			w.metrics.circuitBreaks.WithLabelValues(
				"capture_unavailable",
			).Inc()
			w.logger.Warnw(
				"junkpurge capture-only collection unavailable; no model calls were made",
				"captured",
				captured,
				"safety_topups_captured",
				topUpsCaptured,
				"deferred",
				len(candidates)+len(captureTopUps)-
					captured-topUpsCaptured,
			)
			return
		}
		cycleOutcome = cycleOutcomeCaptureOnly
		w.logger.Infow(
			"junkpurge capture-only cycle complete; no model calls or judgments",
			"captured",
			captured,
			"safety_topups_captured",
			topUpsCaptured,
		)
		return
	}

	// Circuit breaker 1: one or more provider calls were unavailable. Valid
	// judgments from other groups are already durable and explicitly marked as
	// breaker evidence, but a partial/uncertain cycle can never delete.
	if judging.hadUnavailable {
		cycleOutcome = cycleOutcomeLLMUnavailable
		w.metrics.llmDeferredTotal.Add(float64(judging.deferred))
		w.metrics.circuitBreaks.WithLabelValues("llm_unavailable").Inc()
		w.markSyncJudgmentOutcome(
			ctx, pool, recordedClaims, cycleOutcomeLLMUnavailable,
		)
		w.logger.Warnw(
			"junkpurge: provider unavailable in cycle — preserved valid judgments, skipping deletion",
			"judged", judged,
			"confident_junk", len(junk),
			"deferred", judging.deferred,
			"unavailable_groups", len(judging.unavailableGroups),
			"halted_after_consecutive_limit", judging.halted,
		)
		return
	}

	// Circuit breaker 2: genuine junk is a small minority. An implausibly
	// high junk-rate means the LLM/prompt is degraded, not that the library
	// is suddenly junk — abort without deleting.
	if judged > 0 && float64(len(junk))/float64(judged) > w.cfg.MaxJunkRate {
		cycleOutcome = cycleOutcomeJunkRateAnomaly
		w.metrics.circuitBreaks.WithLabelValues("junk_rate_anomaly").Inc()
		w.markSyncJudgmentOutcome(
			ctx, pool, recordedClaims, cycleOutcomeJunkRateAnomaly,
		)
		w.logger.Errorw("junkpurge: junk-rate exceeds max — LLM likely degraded, skipping deletion",
			"junk", len(junk), "judged", judged,
			"max_junk_rate", w.cfg.MaxJunkRate,
			"evaluation_evidence", judged,
		)
		return
	}

	if len(junk) == 0 {
		cycleOutcome = cycleOutcomeNoJunk
		w.markSyncJudgmentOutcome(ctx, pool, recordedClaims, cycleOutcomeNoJunk)
		w.logger.Infow("junkpurge cycle complete", "judged", judged, "junk", 0)
		return
	}

	if !w.cfg.EnablePurge {
		cycleOutcome = cycleOutcomeDryRun
		w.markSyncJudgmentOutcome(ctx, pool, recordedClaims, cycleOutcomeDryRun)
		w.metrics.wouldDeleteTotal.Add(float64(len(junk)))
		w.logger.Infow("junkpurge DRY-RUN — review junkpurge_judgments WHERE verdict='junk' before enabling",
			"judged", judged, "would_delete", len(junk))
		return
	}

	quarantinedHashes, err := quarantineJunk(
		ctx, pool, junk, w.cfg.MinConfidence, w.cfg.MinAge,
		w.batchLeaseOwner,
	)
	if err == nil && w.verdicts != nil {
		days := w.cfg.QuarantineDays
		if days <= 0 {
			days = 30
		}
		exp := time.Now().Add(time.Duration(days) * 24 * time.Hour)
		for _, h := range quarantinedHashes {
			if verr := w.verdicts.Record(ctx, verdicts.Event{
				InfoHash: h, Verdict: verdicts.VerdictQuarantined,
				Mechanism: verdicts.MechanismJunkpurge,
				Reason:    "LLM junk judgment past min-age; snapshot retained",
				ExpiresAt: &exp,
			}); verr != nil {
				w.logger.Warnw("junkpurge verdict record", "err", verr)
			}
		}
	}
	if err != nil {
		cycleOutcome = cycleOutcomeQuarantineError
		w.markSyncJudgmentOutcome(
			ctx, pool, recordedClaims, cycleOutcomeQuarantineError,
		)
		w.metrics.cycleErrorsTotal.WithLabelValues("quarantine").Inc()
		w.logger.Errorw("junkpurge quarantine", "err", err)
		return
	}
	cycleOutcome = cycleOutcomeQuarantined
	w.markSyncJudgmentOutcome(ctx, pool, recordedClaims, cycleOutcomeQuarantined)
	w.metrics.quarantinedTotal.Add(float64(len(quarantinedHashes)))
	w.logger.Infow("junkpurge complete (quarantined for review, not hard-deleted)",
		"judged", judged, "quarantined", len(quarantinedHashes))
}

// candidateQuery selects unmatched movie/tv torrents that are old enough, carry
// no external evidence, have NO matched content at all, and have not been
// judged within rejudge_interval. Driven from torrents (one row per torrent)
// so the LLM sees the torrent name.
const candidateQuery = `
SELECT t.info_hash, t.name
FROM torrents t
WHERE t.private = false
  AND EXISTS (
    SELECT 1 FROM torrent_contents tc
    WHERE tc.info_hash = t.info_hash
      AND tc.content_type IN ('movie','tv_show')
      AND tc.content_id IS NULL
      AND tc.created_at < $1
  )
  AND NOT EXISTS (
    SELECT 1 FROM torrent_contents m
    WHERE m.info_hash = t.info_hash AND m.content_id IS NOT NULL
  )
  AND NOT EXISTS (SELECT 1 FROM torrent_canonical_labels l WHERE l.info_hash = t.info_hash)
  AND NOT EXISTS (SELECT 1 FROM label_evidence e        WHERE e.info_hash = t.info_hash)
  AND NOT EXISTS (
    SELECT 1 FROM junkpurge_judgments j
    WHERE j.info_hash = t.info_hash AND j.judged_at >= $2
  )
  AND NOT EXISTS (
    SELECT 1 FROM junkpurge_batch_items bi
    WHERE bi.info_hash = t.info_hash
      AND (
        bi.state IN ('pending','submitted','succeeded','retryable_error')
        OR (bi.state='abandoned' AND bi.retry_after > now())
      )
  )
  AND NOT EXISTS (
    SELECT 1 FROM junkpurge_sync_claims sc
    WHERE sc.info_hash=t.info_hash AND sc.lease_until > now()
  )
  AND NOT EXISTS (
    SELECT 1 FROM junkpurge_student_releases sr
    WHERE sr.info_hash = t.info_hash
  )
  /* llm-evaluation-capture-exclusion */
ORDER BY t.created_at ASC
LIMIT $3
FOR UPDATE OF t SKIP LOCKED`

func findCandidates(ctx context.Context, pool *pgxpool.Pool, cfg Config) ([]candidate, error) {
	return findCandidatesQuery(ctx, pool, cfg)
}

const captureOnlyCandidateExclusion = `
AND NOT EXISTS (
  SELECT 1
  FROM llm_evaluation_capture_admissions capture_admission
  JOIN llm_evaluation_captures capture
    ON capture.capture_key = capture_admission.capture_key
  WHERE capture_admission.info_hash = t.info_hash
    AND capture.task = 'junkpurge'
    AND capture.expires_at > now()
)`

const captureOnlyAnyJudgmentExclusion = `
  AND NOT EXISTS (
    SELECT 1 FROM junkpurge_judgments j
    WHERE j.info_hash = t.info_hash
  )`

// captureSafetyTopUpQuery is the exact current worker-eligibility query with
// only the 90-day rejudge-spend suppression replaced by a historical-teacher
// stratum. It makes no model call. row_number balances junk and real
// hypotheses deterministically so a bounded capture window cannot evict the
// rare junk stratum behind hundreds of thousands of natural rows.
const captureSafetyTopUpQuery = `
WITH eligible AS (
  SELECT
    t.info_hash,
    t.name,
    t.created_at,
    CASE
      WHEN j.verdict = 'junk' THEN 'junk'
      ELSE 'real'
    END AS teacher_stratum
  FROM torrents t
  JOIN junkpurge_judgments j ON j.info_hash = t.info_hash
  WHERE t.private = false
    AND j.verdict IN ('junk','real_mangled','real_absent')
    AND j.torrent_name = t.name
    AND EXISTS (
      SELECT 1 FROM torrent_contents tc
      WHERE tc.info_hash = t.info_hash
        AND tc.content_type IN ('movie','tv_show')
        AND tc.content_id IS NULL
        AND tc.created_at < $1
    )
    AND NOT EXISTS (
      SELECT 1 FROM torrent_contents m
      WHERE m.info_hash = t.info_hash AND m.content_id IS NOT NULL
    )
    AND NOT EXISTS (
      SELECT 1 FROM torrent_canonical_labels l
      WHERE l.info_hash = t.info_hash
    )
    AND NOT EXISTS (
      SELECT 1 FROM label_evidence e
      WHERE e.info_hash = t.info_hash
    )
    AND NOT EXISTS (
      SELECT 1 FROM junkpurge_batch_items bi
      WHERE bi.info_hash = t.info_hash
        AND (
          bi.state IN ('pending','submitted','succeeded','retryable_error')
          OR (bi.state='abandoned' AND bi.retry_after > now())
        )
    )
    AND NOT EXISTS (
      SELECT 1 FROM junkpurge_sync_claims sc
      WHERE sc.info_hash=t.info_hash AND sc.lease_until > now()
    )
    AND NOT EXISTS (
      SELECT 1
      FROM llm_evaluation_capture_admissions capture_admission
      JOIN llm_evaluation_captures capture
        ON capture.capture_key = capture_admission.capture_key
      WHERE capture_admission.info_hash = t.info_hash
        AND capture.task = 'junkpurge'
        AND capture.expires_at > now()
    )
),
ranked AS (
  SELECT
    info_hash,
    name,
    teacher_stratum,
    row_number() OVER (
      PARTITION BY teacher_stratum
      ORDER BY created_at, info_hash
    ) AS stratum_ordinal
  FROM eligible
)
SELECT info_hash, name
FROM ranked
WHERE stratum_ordinal <= greatest(1, ($2 + 1) / 2)
ORDER BY stratum_ordinal, teacher_stratum, info_hash
LIMIT $2`

func findCaptureCandidates(
	ctx context.Context,
	pool *pgxpool.Pool,
	cfg Config,
) ([]candidate, error) {
	query, err := captureOnlyQuery()
	if err != nil {
		return nil, err
	}
	return queryCandidates(
		ctx,
		pool,
		query,
		time.Now().Add(-cfg.MinAge),
		cfg.BatchSize,
	)
}

func captureOnlyQuery() (string, error) {
	const marker = "/* llm-evaluation-capture-exclusion */"
	const recentJudgmentPredicate = `  AND NOT EXISTS (
    SELECT 1 FROM junkpurge_judgments j
    WHERE j.info_hash = t.info_hash AND j.judged_at >= $2
  )`
	query := strings.Replace(
		candidateQuery,
		recentJudgmentPredicate,
		captureOnlyAnyJudgmentExclusion,
		1,
	)
	if query == candidateQuery {
		return "", fmt.Errorf(
			"junkpurge capture-only judgment predicate is missing",
		)
	}
	withExclusion := strings.Replace(
		query,
		marker,
		captureOnlyCandidateExclusion,
		1,
	)
	if withExclusion == query {
		return "", fmt.Errorf("junkpurge capture-only query marker is missing")
	}
	renumbered := strings.Replace(withExclusion, "LIMIT $3", "LIMIT $2", 1)
	if renumbered == withExclusion {
		return "", fmt.Errorf(
			"junkpurge capture-only query limit placeholder is missing",
		)
	}
	return renumbered, nil
}

func findCaptureSafetyTopUpCandidates(
	ctx context.Context,
	pool *pgxpool.Pool,
	cfg Config,
) ([]candidate, error) {
	rows, err := pool.Query(
		ctx,
		captureSafetyTopUpQuery,
		time.Now().Add(-cfg.MinAge),
		cfg.BatchSize,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []candidate
	for rows.Next() {
		var value candidate
		if err := rows.Scan(&value.infoHash, &value.name); err != nil {
			return nil, err
		}
		out = append(out, value)
	}
	return out, rows.Err()
}

func recordJudgment(ctx context.Context, pool *pgxpool.Pool, infoHash []byte, j Judgment, name string) error {
	_, err := pool.Exec(ctx, `
INSERT INTO junkpurge_judgments (info_hash, verdict, confidence, torrent_name, judged_at, purged)
VALUES ($1, $2, $3, $4, now(), false)
ON CONFLICT (info_hash) DO UPDATE SET
  verdict = excluded.verdict,
  confidence = excluded.confidence,
  torrent_name = excluded.torrent_name,
  judged_at = now()`,
		infoHash, j.Verdict, j.Confidence, name)
	return err
}

// markSyncJudgmentOutcome turns the existing judgment.reason column into an
// explicit sampling seam for synchronous cycles. In particular,
// sync_cycle:junk_rate_anomaly identifies the safety cohort the breaker kept
// out of quarantine without discarding the paid model outputs.
//
// This annotation is observational only. Failure never enables an action and
// never changes verdict/confidence/purged; it is surfaced as record_outcome so
// a missing cohort marker cannot pass silently.
func (w *purgeWorker) markSyncJudgmentOutcome(
	ctx context.Context,
	pool *pgxpool.Pool,
	infoHashes [][]byte,
	outcome string,
) {
	if len(infoHashes) == 0 {
		return
	}
	tag, err := pool.Exec(ctx, `
UPDATE junkpurge_judgments
SET reason=$2
WHERE info_hash=ANY($1)
  AND reason=$3`,
		infoHashes,
		"sync_cycle:"+outcome,
		judgmentReasonSyncPending,
	)
	if err == nil && tag.RowsAffected() == int64(len(infoHashes)) {
		return
	}
	w.metrics.cycleErrorsTotal.WithLabelValues("record_outcome").Inc()
	if err != nil {
		w.logger.Warnw(
			"junkpurge: mark sync judgment outcome",
			"outcome", outcome,
			"expected", len(infoHashes),
			"err", err,
		)
		return
	}
	w.logger.Warnw(
		"junkpurge: mark sync judgment outcome incomplete",
		"outcome", outcome,
		"expected", len(infoHashes),
		"updated", tag.RowsAffected(),
	)
}

// quarantineJunk MOVES the confident-junk torrents out of the main DB into
// junkpurge_quarantine — a restorable snapshot of the raw torrent row + its file
// list — then deletes them from torrents (cascading them out of torrent_contents,
// search and Torznab). The snapshot makes the removal reversible during the
// review window; verdict + confidence come from the judgment row recorded earlier
// this cycle. One transaction. Returns the number quarantined.
// quarantineJunk returns the info-hashes ACTUALLY quarantined — eligibility
// filtering and mid-cycle torrent deletion mean the input list can over-count,
// and the verdict ledger must only record real transitions (review-demonstrated:
// recording the full junk list fabricates 'quarantined' states and skews
// expires_at). A row returned via ON CONFLICT DO UPDATE is a real transition too
// — it is a tombstoned hash the crawler re-acquired and the model re-judged as
// junk, moving tombstoned → quarantined.
func quarantineJunk(
	ctx context.Context,
	pool *pgxpool.Pool,
	infoHashes [][]byte,
	minConfidence float64,
	minAge time.Duration,
	claimOwner string,
) ([][]byte, error) {
	if len(infoHashes) == 0 {
		return nil, nil
	}
	tx, err := pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	eligible, err := eligibleSyncJunkTx(
		ctx, tx, infoHashes, minConfidence, minAge, claimOwner,
	)
	if err != nil {
		return nil, err
	}
	inserted, err := quarantineJunkTx(ctx, tx, eligible, minConfidence)
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return inserted, nil
}

func quarantineJunkTx(
	ctx context.Context,
	tx pgx.Tx,
	infoHashes [][]byte,
	minConfidence float64,
) ([][]byte, error) {
	if len(infoHashes) == 0 {
		return nil, nil
	}
	// ON CONFLICT DO UPDATE, not DO NOTHING: since expiry became a tombstone,
	// a quarantine row outlives the torrent's deletion, so a hash the crawler
	// re-acquires and the model re-judges as junk WILL collide with its own
	// tombstone. Under DO NOTHING that returns no rows, the DELETE FROM torrents
	// below is skipped, and the re-crawled junk torrent stays in search forever,
	// re-judged every RejudgeInterval and never removed. Re-quarantining resets
	// the review window and refreshes the snapshot against the row we are about
	// to delete — which is what keeps the restore path correct.
	//
	// A currently-live quarantine row cannot reach this branch: its torrent is
	// already gone from `torrents`, so the SELECT below produces nothing for it.
	rows, err := tx.Query(ctx, quarantineUpsertSQL, infoHashes, minConfidence)
	if err != nil {
		return nil, err
	}
	var inserted [][]byte
	for rows.Next() {
		var h []byte
		if scanErr := rows.Scan(&h); scanErr != nil {
			rows.Close()
			return nil, scanErr
		}
		inserted = append(inserted, h)
	}
	rows.Close()
	if rowsErr := rows.Err(); rowsErr != nil {
		return nil, rowsErr
	}
	if len(inserted) == 0 {
		return nil, nil
	}
	if _, err := tx.Exec(ctx, `DELETE FROM torrents WHERE info_hash = ANY($1)`, inserted); err != nil {
		return nil, err
	}
	if _, err := tx.Exec(ctx,
		`UPDATE junkpurge_judgments SET purged = true WHERE info_hash = ANY($1)`,
		inserted,
	); err != nil {
		return nil, err
	}
	return inserted, nil
}

const quarantineUpsertSQL = `
INSERT INTO junkpurge_quarantine (info_hash, torrent_name, verdict, confidence, torrent_snapshot, files_snapshot)
SELECT t.info_hash, t.name, j.verdict, j.confidence, to_jsonb(t),
       (SELECT jsonb_agg(to_jsonb(f)) FROM torrent_files f WHERE f.info_hash = t.info_hash)
FROM torrents t
JOIN junkpurge_judgments j ON j.info_hash = t.info_hash
WHERE t.info_hash = ANY($1)
  AND t.private = false
  AND j.verdict = 'junk'
  AND j.confidence >= $2
  AND j.torrent_name = t.name
ON CONFLICT (info_hash) DO UPDATE SET
  torrent_name     = excluded.torrent_name,
  verdict          = excluded.verdict,
  confidence       = excluded.confidence,
  torrent_snapshot = excluded.torrent_snapshot,
  files_snapshot   = excluded.files_snapshot,
  quarantined_at   = now(),
  expired_at       = NULL
RETURNING info_hash`

// The quarantine expiry SQL lives in constants so TestQuarantineExpiryIsNotDestructive
// can assert on it without a database. CI has no PostgreSQL service, so the
// lifecycle integration test skips there and these assertions are the only thing
// standing between a careless edit and a re-armed destructive path.
const (
	quarantineWouldExpireCountSQL = `
SELECT count(*) FROM junkpurge_quarantine
WHERE quarantined_at < now() - make_interval(days => $1) AND expired_at IS NULL`

	// expired_at IS NULL keeps this idempotent: without it every cycle would
	// re-stamp the whole tombstone archive and re-emit a verdict event per row.
	quarantineExpireTombstoneSQL = `
UPDATE junkpurge_quarantine SET expired_at = now()
WHERE quarantined_at < now() - make_interval(days => $1) AND expired_at IS NULL
RETURNING info_hash`
)

// logWouldExpire reports (without tombstoning) how many quarantine entries have
// lapsed their review window — the dry-run counterpart of expireQuarantine.
func (w *purgeWorker) logWouldExpire(ctx context.Context, pool *pgxpool.Pool) {
	days := w.cfg.QuarantineDays
	if days <= 0 {
		days = 30
	}
	var n int64
	if err := pool.QueryRow(ctx, quarantineWouldExpireCountSQL, days).Scan(&n); err != nil {
		w.logger.Warnw("junkpurge would-expire count", "err", err)
		return
	}
	if n > 0 {
		w.logger.Infow("junkpurge DRY-RUN — quarantine entries past review window retained",
			"would_expire", n, "quarantine_days", days)
	}
}

// expireQuarantine tombstones quarantine entries older than the review window.
//
// It is deliberately NON-DESTRUCTIVE. quarantineJunkTx already deleted the
// torrent from `torrents`, so the torrent left search and Torznab at quarantine
// time — expiry never performed any of the operational work. What it used to add
// on top was exactly two irreversible things: it freed ~840 B/row of snapshot,
// and it wrote a torrent_liveness blacklist row that foreclosed re-crawl
// recovery. Neither is worth an unrecoverable action whose error rate cannot be
// bounded (docs/design/junkpurge-safety-program.md).
//
// Now it only stamps expired_at, which drops the row out of the operator review
// list while leaving RestoreQuarantined (store.go) able to rebuild the torrent
// from snapshot indefinitely. The ledger verdict moves quarantined → tombstoned;
// both are in verdicts.blockingVerdicts, so serving and BEP-9 exclusion are
// unchanged. Best-effort; logs on error.
//
// ponytail: no second "really delete after N days" horizon. expired_at is the
// marker one would key on if owner decision D4 elects to re-arm a destructive
// purge — add it then, not speculatively.
func (w *purgeWorker) expireQuarantine(ctx context.Context, pool *pgxpool.Pool) {
	days := w.cfg.QuarantineDays
	if days <= 0 {
		days = 30
	}
	rows, err := pool.Query(ctx, quarantineExpireTombstoneSQL, days)
	if err != nil {
		w.metrics.cycleErrorsTotal.WithLabelValues("expire").Inc()
		w.logger.Warnw("junkpurge expire tombstone", "err", err)
		return
	}
	var expired [][]byte
	scanFailed := false
	for rows.Next() {
		var h []byte
		if scanErr := rows.Scan(&h); scanErr != nil {
			w.logger.Warnw("junkpurge expire scan", "err", scanErr)
			scanFailed = true
			break
		}
		expired = append(expired, h)
	}
	rows.Close()
	// pgx defers execution errors to rows.Err(): a nil pool.Query error does
	// NOT mean the UPDATE ran. Without this check a failed expiry is silent
	// (review-demonstrated). A scan break is also partial: the UPDATE already
	// stamped ALL matching rows server-side, so unscanned hashes get no verdict
	// event — count it as an error. Unlike the previous destructive form this is
	// now fully self-healing: the rows are still there and still restorable.
	if rowsErr := rows.Err(); rowsErr != nil || scanFailed {
		w.metrics.cycleErrorsTotal.WithLabelValues("expire").Inc()
		if rowsErr != nil {
			w.logger.Warnw("junkpurge expire tombstone", "err", rowsErr)
		}
	}
	if w.verdicts != nil {
		for _, h := range expired {
			if verr := w.verdicts.Record(ctx, verdicts.Event{
				InfoHash: h, Verdict: verdicts.VerdictTombstoned,
				Mechanism: verdicts.MechanismJunkpurge,
				Reason:    "quarantine review window lapsed; tombstoned, snapshot retained and restorable",
			}); verr != nil {
				w.logger.Warnw("junkpurge verdict record", "err", verr)
			}
		}
	}
	if n := len(expired); n > 0 {
		w.metrics.expiredTotal.Add(float64(n))
		w.logger.Infow("junkpurge expired — tombstoned, snapshot retained",
			"count", n, "window_days", days)
	}
}

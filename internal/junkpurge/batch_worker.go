package junkpurge

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/spencercnorton/bitagent/internal/verdicts"
)

// batchWorkerClient is deliberately narrower than the HTTP client. It keeps
// all spend-changing calls visible to the durable coordinator while allowing
// an in-memory fake to exercise restart and ambiguous-POST recovery.
type batchWorkerClient interface {
	BaseURL() string
	BuildInput(batchRun, int, []batchItem) ([]byte, string, string, error)
	FindInputFile(context.Context, string, string, int64) (batchWorkerFile, bool, error)
	UploadInputFile(context.Context, string, []byte) (batchWorkerFile, error)
	FindBatch(context.Context, string, string, map[string]string) (batchWorkerJob, bool, error)
	CreateBatch(context.Context, batchWorkerCreate) (batchWorkerJob, error)
	RetrieveBatch(context.Context, string) (batchWorkerJob, error)
	DownloadFile(context.Context, string) ([]byte, error)
	ParseResults([]byte, []byte) ([]batchItemResult, error)
	DeleteFile(context.Context, string) error
}

type batchWorkerFile struct {
	ID       string
	Filename string
	Bytes    int64
}

type batchWorkerCreate struct {
	InputFileID      string
	Endpoint         string
	CompletionWindow string
	Metadata         map[string]string
}

type batchWorkerJob struct {
	ID               string
	Status           string
	InputFileID      string
	OutputFileID     string
	ErrorFileID      string
	RequestTotal     int
	RequestCompleted int
	RequestFailed    int
	CreatedAt        time.Time
	Metadata         map[string]string
}

var batchLeaseSequence atomic.Uint64

func newBatchLeaseOwner() string {
	return fmt.Sprintf(
		"bitagent-%d-%d-%d",
		os.Getpid(), time.Now().UnixNano(), batchLeaseSequence.Add(1),
	)
}

func validateBatchConfig(cfg Config) error {
	if cfg.LLMBatchFallbackSync {
		return errors.New("junkpurge: synchronous fallback is not supported")
	}
	if !cfg.LLMBatchEnabled {
		return nil
	}
	if cfg.LLMAllowPaidSync {
		return errors.New(
			"junkpurge: llm_allow_paid_sync must be false in Batch mode",
		)
	}
	if cfg.LLMApiStyle != "chat" {
		return errors.New("junkpurge: provider Batch requires llm_api_style=chat")
	}
	if strings.TrimSpace(cfg.LLMBaseURL) == "" {
		return errors.New("junkpurge: provider Batch requires llm_base_url")
	}
	parsed, err := url.Parse(cfg.LLMBaseURL)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") ||
		parsed.Host == "" {
		return errors.New("junkpurge: provider Batch requires a valid http(s) llm_base_url")
	}
	if parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return errors.New("junkpurge: provider Batch llm_base_url cannot contain userinfo, query, or fragment")
	}
	if strings.HasSuffix(strings.TrimRight(parsed.Path, "/"), "/chat/completions") {
		return errors.New("junkpurge: provider Batch llm_base_url must be an API root, not a chat endpoint")
	}
	hostname := strings.TrimSuffix(strings.ToLower(parsed.Hostname()), ".")
	if hostname == "api.openai.com" {
		if parsed.Scheme != "https" {
			return errors.New("junkpurge: api.openai.com Batch traffic requires https")
		}
		if strings.TrimRight(parsed.Path, "/") != "/v1" {
			return errors.New("junkpurge: api.openai.com llm_base_url must end at /v1")
		}
		if strings.TrimSpace(cfg.LLMApiKey) == "" {
			return errors.New("junkpurge: provider Batch requires llm_api_key for api.openai.com")
		}
	}
	if strings.TrimSpace(cfg.LLMModel) == "" {
		return errors.New("junkpurge: provider Batch requires llm_model")
	}
	if cfg.LLMBatchPollInterval <= 0 {
		return errors.New("junkpurge: llm_batch_poll_interval must be positive")
	}
	if cfg.LLMBatchMaxInFlight <= 0 {
		return errors.New("junkpurge: llm_batch_max_in_flight must be positive")
	}
	if cfg.LLMBatchMaxAttempts <= 0 {
		return errors.New("junkpurge: llm_batch_max_attempts must be positive")
	}
	if cfg.LLMBatchCompletionWindow != "24h" {
		return errors.New("junkpurge: llm_batch_completion_window must be 24h")
	}
	if cfg.LLMBatchFailureCooldown <= 0 {
		return errors.New("junkpurge: llm_batch_failure_cooldown must be positive")
	}
	if cfg.LLMBatchAmbiguityGrace <= 0 {
		return errors.New("junkpurge: llm_batch_ambiguity_grace must be positive")
	}
	if cfg.BatchSize > 50_000 {
		return errors.New("junkpurge: batch_size exceeds provider Batch limit of 50000")
	}
	return nil
}

// batchLoop uses two clocks: Interval admits at most one new logical safety
// cohort, while the shorter poll interval settles/retries existing work.
// Reconciliation happens before either ticker starts, so restarts resume
// durable attempts without waiting and without falling back to synchronous
// chat completions.
func (w *purgeWorker) batchLoop(
	ctx context.Context,
	client batchWorkerClient,
	admitNew bool,
	allowPurge bool,
) {
	defer w.wg.Done()

	w.runBatchReconcile(ctx, client, allowPurge, admitNew)
	if !admitNew && w.batchDrainComplete(ctx) {
		return
	}

	// Preserve the synchronous worker's five-minute startup offset for new
	// spend while still polling existing durable jobs immediately.
	var submitTimer *time.Timer
	var submitC <-chan time.Time
	if admitNew {
		submitTimer = time.NewTimer(5 * time.Minute)
		submitC = submitTimer.C
		defer submitTimer.Stop()
	}
	pollInterval := w.cfg.LLMBatchPollInterval
	if pollInterval <= 0 {
		pollInterval = time.Minute
	}
	pollTicker := time.NewTicker(pollInterval)
	defer pollTicker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-pollTicker.C:
			w.runBatchReconcile(ctx, client, allowPurge, admitNew)
			if !admitNew && w.batchDrainComplete(ctx) {
				return
			}
		case <-submitC:
			w.runBatchSubmission(ctx, client)
			submitTimer.Reset(w.cfg.Interval)
		}
	}
}

func (w *purgeWorker) runBatchSubmission(ctx context.Context, client batchWorkerClient) {
	pool, err := w.pool.Get()
	if err != nil {
		w.batchError("query", err)
		return
	}

	// Preserve the existing quarantine lifecycle in Batch mode. New provider
	// work and destructive expiration remain separately controlled by
	// EnablePurge.
	if w.cfg.EnablePurge {
		w.expireQuarantine(ctx, pool)
	} else {
		w.logWouldExpire(ctx, pool)
	}

	active, err := countActiveBatchRuns(ctx, pool)
	if err != nil {
		w.batchError("query", err)
		return
	}
	w.metrics.batchInFlight.Set(float64(active))
	if active >= w.cfg.LLMBatchMaxInFlight {
		w.logger.Debugw("junkpurge Batch capacity reached",
			"active", active, "max", w.cfg.LLMBatchMaxInFlight)
		return
	}

	run, err := createBatchRun(ctx, pool, w.cfg)
	if err != nil {
		w.batchError("reserve", err)
		return
	}
	if run == nil {
		return
	}
	w.metrics.candidatesTotal.Add(float64(run.ItemCount))
	w.metrics.batchInFlight.Set(float64(active + 1))

	if err := w.prepareAndSubmitBatchRuns(ctx, pool, client, 1); err != nil {
		w.batchError("submit", err)
	}
}

func (w *purgeWorker) runBatchReconcile(
	ctx context.Context,
	client batchWorkerClient,
	allowPurge bool,
	allowRetrySubmissions bool,
) {
	pool, err := w.pool.Get()
	if err != nil {
		w.batchError("query", err)
		return
	}

	// A reduced max-in-flight setting must not strand work created under an
	// older configuration, so reconciliation is intentionally not limited by
	// the current admission ceiling.
	attempts, err := listUningestedAttempts(ctx, pool, 50_000)
	if err != nil {
		w.batchError("query", err)
		return
	}
	for _, attempt := range attempts {
		if ctx.Err() != nil {
			return
		}
		if err := w.reconcileBatchAttempt(
			ctx, pool, client, attempt, allowRetrySubmissions,
		); err != nil {
			w.batchError("reconcile", err)
		}
	}

	if allowRetrySubmissions {
		// Terminal expiry may leave unfinished items. Build the next attempt
		// after ingestion commits instead of waiting for hourly admission.
		if err := w.prepareAndSubmitBatchRuns(ctx, pool, client, 50_000); err != nil {
			w.batchError("retry", err)
		}
	} else if err := failBatchRunsNeedingAttempt(
		ctx, pool, 50_000, "batch_disabled",
		"Batch admission was disabled before the logical run completed",
	); err != nil {
		w.batchError("disable", err)
	}
	if err := w.finalizeBatchRuns(ctx, pool, allowPurge); err != nil {
		w.batchError("finalize", err)
	}
	w.cleanupSettledBatchInputFiles(ctx, pool, client)
	w.refreshBatchInFlight(ctx, pool)
}

func (w *purgeWorker) batchDrainComplete(ctx context.Context) bool {
	pool, err := w.pool.Get()
	if err != nil {
		w.batchError("query", err)
		return false
	}
	active, err := countActiveBatchRuns(ctx, pool)
	if err != nil {
		w.batchError("query", err)
		return false
	}
	return active == 0
}

func (w *purgeWorker) prepareAndSubmitBatchRuns(
	ctx context.Context,
	pool *pgxpool.Pool,
	client batchWorkerClient,
	limit int,
) error {
	runs, err := listRunsNeedingAttempt(ctx, pool, limit)
	if err != nil {
		return err
	}
	var errs []error
	for _, run := range runs {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		attempt, err := prepareBatchAttempt(
			ctx, pool, run, client.BuildInput,
		)
		if err != nil {
			errs = append(errs, fmt.Errorf("run %d prepare: %w", run.ID, err))
			continue
		}
		if attempt == nil {
			continue
		}
		if err := w.reconcileBatchAttempt(
			ctx, pool, client, *attempt, true,
		); err != nil {
			errs = append(errs, fmt.Errorf("run %d attempt %d submit: %w",
				run.ID, attempt.AttemptNo, err))
		}
	}
	return errors.Join(errs...)
}

func (w *purgeWorker) reconcileBatchAttempt(
	ctx context.Context,
	pool *pgxpool.Pool,
	client batchWorkerClient,
	attempt batchAttempt,
	allowProviderPOST bool,
) (retErr error) {
	lockConn, locked, err := acquireBatchAttemptAdvisoryLock(ctx, pool, attempt.ID)
	if err != nil {
		return fmt.Errorf("acquire attempt advisory lock: %w", err)
	}
	if !locked {
		return nil
	}
	defer func() {
		unlockCtx, cancel := context.WithTimeout(
			context.WithoutCancel(ctx), 5*time.Second,
		)
		defer cancel()
		if err := releaseBatchAttemptAdvisoryLock(
			unlockCtx, lockConn, attempt.ID,
		); err != nil {
			w.logger.Warnw("junkpurge Batch release advisory lock",
				"attempt_id", attempt.ID, "err", err)
		}
	}()

	leaseTTL := 10 * time.Minute
	if requestTTL := 3 * w.cfg.timeout(); requestTTL > leaseTTL {
		leaseTTL = requestTTL
	}
	claimed, err := claimBatchAttempt(
		ctx, pool, attempt.ID, w.batchLeaseOwner, leaseTTL,
	)
	if err != nil {
		return fmt.Errorf("claim attempt lease: %w", err)
	}
	if !claimed {
		return nil
	}
	defer func() {
		releaseCtx, cancel := context.WithTimeout(
			context.WithoutCancel(ctx), 5*time.Second,
		)
		defer cancel()
		if err := releaseBatchAttempt(
			releaseCtx, pool, attempt.ID, w.batchLeaseOwner,
		); err != nil {
			w.logger.Warnw("junkpurge Batch release lease",
				"attempt_id", attempt.ID, "err", err)
		}
	}()
	defer func() {
		if retErr == nil {
			return
		}
		errorCtx, cancel := context.WithTimeout(
			context.WithoutCancel(ctx), 5*time.Second,
		)
		defer cancel()
		if err := setAttemptError(
			errorCtx, pool, attempt.ID, truncate(retErr.Error(), 2_000),
			w.batchLeaseOwner,
		); err != nil {
			w.logger.Warnw("junkpurge Batch persist reconcile error",
				"attempt_id", attempt.ID, "err", err)
		}
	}()

	attempt, err = loadBatchAttempt(ctx, pool, attempt.ID)
	if err != nil {
		return fmt.Errorf("reload claimed attempt: %w", err)
	}
	if attempt.IngestedAt != nil {
		return nil
	}
	if strings.TrimRight(client.BaseURL(), "/") != attempt.ProviderBaseURL {
		return fmt.Errorf(
			"provider base URL changed for active run: configured %q, persisted %q",
			client.BaseURL(), attempt.ProviderBaseURL,
		)
	}
	originalState := attempt.State

	if int64(len(attempt.Payload)) != attempt.InputBytes {
		return fmt.Errorf(
			"persisted input length changed: got %d, want %d",
			len(attempt.Payload), attempt.InputBytes,
		)
	}
	sum := sha256.Sum256(attempt.Payload)
	if digest := hex.EncodeToString(sum[:]); digest != attempt.InputSHA256 {
		return fmt.Errorf(
			"persisted input sha256 changed: got %s, want %s",
			digest, attempt.InputSHA256,
		)
	}

	if attempt.InputFileID == "" {
		file, recovered, err := client.FindInputFile(
			ctx, attempt.InputFilename, attempt.InputSHA256, attempt.InputBytes,
		)
		if err != nil {
			// Listing must succeed before upload. Otherwise a timed-out prior
			// upload could be duplicated on every restart.
			return fmt.Errorf("reconcile input file: %w", err)
		}
		if !recovered {
			if !allowProviderPOST {
				if ambiguityGraceActive(
					attempt, "uploading",
					maxDuration(attempt.AmbiguityGrace, w.cfg.LLMBatchAmbiguityGrace),
					time.Now(),
				) {
					return nil
				}
				return failClaimedBatchAttempt(
					ctx, pool, attempt.ID, w.batchLeaseOwner,
					"batch_disabled_before_upload",
					"Batch admission was disabled before input upload completed",
				)
			}
			if ambiguityGraceActive(
				attempt, "uploading",
				maxDuration(attempt.AmbiguityGrace, w.cfg.LLMBatchAmbiguityGrace),
				time.Now(),
			) {
				return nil
			}
			safe, err := w.enforceBatchAttemptPrivacy(
				ctx, pool, client, attempt,
			)
			if err != nil {
				return fmt.Errorf("pre-upload privacy gate: %w", err)
			}
			if !safe {
				return nil
			}
			if err := w.captureBatchAttempt(ctx, pool, attempt); err != nil {
				if errors.Is(err, errBatchCapturePrivacyBlocked) {
					return failClaimedBatchAttempt(
						ctx, pool, attempt.ID, w.batchLeaseOwner,
						"evaluation_capture_privacy_blocked",
						"evaluation capture observed private-tracker material before upload",
					)
				}
				return fmt.Errorf("pre-upload evaluation capture: %w", err)
			}
			if err := setAttemptUploading(
				ctx, pool, attempt.ID, w.batchLeaseOwner,
			); err != nil {
				return fmt.Errorf("mark uploading: %w", err)
			}
			file, err = client.UploadInputFile(
				ctx, attempt.InputFilename, attempt.Payload,
			)
			if err != nil {
				return fmt.Errorf("upload input file: %w", err)
			}
		}
		if file.ID == "" {
			return errors.New("provider returned an empty input file id")
		}
		if err := setAttemptInputFile(
			ctx, pool, attempt.ID, file.ID, w.batchLeaseOwner,
		); err != nil {
			return fmt.Errorf("persist input file: %w", err)
		}
		attempt.InputFileID = file.ID
		attempt.State = "uploaded"
	}

	// A normal uploaded/prepared attempt has never issued the create POST.
	// Disabled drain mode must fail it immediately instead of mistaking the
	// upload timestamp for an ambiguous Batch-create timestamp.
	if !allowProviderPOST &&
		attempt.ProviderBatchID == "" &&
		originalState != "submitting" {
		err := failClaimedBatchAttempt(
			ctx, pool, attempt.ID, w.batchLeaseOwner,
			"batch_disabled_before_submit",
			"Batch admission was disabled before provider job creation",
		)
		if err == nil {
			w.cleanupBatchFiles(ctx, client, attempt.InputFileID)
		}
		return err
	}

	metadata := batchAttemptMetadata(attempt)
	if attempt.ProviderBatchID == "" {
		job, recovered, err := client.FindBatch(
			ctx, attempt.InputFileID, attempt.Endpoint, metadata,
		)
		if err != nil {
			// As with files, a successful list is the precondition for a new
			// POST after an ambiguous create timeout.
			return fmt.Errorf("reconcile provider batch: %w", err)
		}
		if !recovered {
			if !allowProviderPOST {
				visibilityDeadline := time.Time{}
				if attempt.SubmissionAttemptedAt != nil {
					visibilityDeadline = attempt.SubmissionAttemptedAt.Add(
						24*time.Hour +
							maxDuration(
								attempt.AmbiguityGrace,
								w.cfg.LLMBatchAmbiguityGrace,
							),
					)
				}
				if visibilityDeadline.IsZero() ||
					time.Now().Before(visibilityDeadline) {
					return nil
				}
				err := failClaimedBatchAttempt(
					ctx, pool, attempt.ID, w.batchLeaseOwner,
					"batch_disabled_create_not_found",
					"disabled drain could not recover the attempted provider Batch",
				)
				if err == nil {
					w.cleanupBatchFiles(ctx, client, attempt.InputFileID)
				}
				return err
			}
			if ambiguityGraceActive(
				attempt, "submitting",
				maxDuration(attempt.AmbiguityGrace, w.cfg.LLMBatchAmbiguityGrace),
				time.Now(),
			) {
				return nil
			}
			safe, err := w.enforceBatchAttemptPrivacy(
				ctx, pool, client, attempt,
			)
			if err != nil {
				return fmt.Errorf("pre-create privacy gate: %w", err)
			}
			if !safe {
				return nil
			}
			if err := w.captureBatchAttempt(ctx, pool, attempt); err != nil {
				if errors.Is(err, errBatchCapturePrivacyBlocked) {
					failErr := failClaimedBatchAttempt(
						ctx, pool, attempt.ID, w.batchLeaseOwner,
						"evaluation_capture_privacy_blocked",
						"evaluation capture observed private-tracker material before Batch creation",
					)
					if failErr == nil {
						w.cleanupBatchFiles(ctx, client, attempt.InputFileID)
					}
					return failErr
				}
				return fmt.Errorf("pre-create evaluation capture: %w", err)
			}
			if err := setAttemptSubmitting(
				ctx, pool, attempt.ID, w.batchLeaseOwner,
			); err != nil {
				return fmt.Errorf("mark submitting: %w", err)
			}
			job, err = client.CreateBatch(ctx, batchWorkerCreate{
				InputFileID:      attempt.InputFileID,
				Endpoint:         attempt.Endpoint,
				CompletionWindow: attempt.CompletionWindow,
				Metadata:         metadata,
			})
			if err != nil {
				return fmt.Errorf("create provider batch: %w", err)
			}
		}
		if job.ID == "" || job.Status == "" {
			return errors.New("provider returned an incomplete Batch job")
		}
		submittedAt := job.CreatedAt
		if submittedAt.IsZero() {
			submittedAt = time.Now()
		}
		if err := setAttemptProviderBatch(
			ctx, pool, attempt.ID, job.ID, job.Status, submittedAt,
			w.batchLeaseOwner,
		); err != nil {
			return fmt.Errorf("persist provider batch: %w", err)
		}
		attempt.ProviderBatchID = job.ID
		if recovered {
			w.metrics.batchJobs.WithLabelValues("recovered").Inc()
		} else {
			w.metrics.batchJobs.WithLabelValues("submitted").Inc()
		}
	}

	job, err := client.RetrieveBatch(ctx, attempt.ProviderBatchID)
	if err != nil {
		return fmt.Errorf("retrieve provider batch %s: %w",
			attempt.ProviderBatchID, err)
	}
	if job.ID != attempt.ProviderBatchID {
		return fmt.Errorf("provider returned Batch id %q for %q",
			job.ID, attempt.ProviderBatchID)
	}
	if job.InputFileID != "" && job.InputFileID != attempt.InputFileID {
		return fmt.Errorf("provider Batch %s input file changed from %s to %s",
			job.ID, attempt.InputFileID, job.InputFileID)
	}
	terminal := isTerminalBatchStatus(job.Status)
	if err := updateAttemptProvider(
		ctx, pool, attempt.ID, job.Status, job.OutputFileID, job.ErrorFileID,
		job.RequestTotal, job.RequestCompleted, job.RequestFailed, terminal,
		w.batchLeaseOwner,
	); err != nil {
		return fmt.Errorf("persist provider status: %w", err)
	}
	if job.OutputFileID != "" {
		attempt.OutputFileID = job.OutputFileID
	}
	if job.ErrorFileID != "" {
		attempt.ErrorFileID = job.ErrorFileID
	}
	if !terminal {
		return nil
	}

	output, err := downloadBatchFile(ctx, client, attempt.OutputFileID)
	if err != nil {
		return fmt.Errorf("download output file: %w", err)
	}
	errorOutput, err := downloadBatchFile(ctx, client, attempt.ErrorFileID)
	if err != nil {
		return fmt.Errorf("download error file: %w", err)
	}
	results, err := client.ParseResults(output, errorOutput)
	if err != nil {
		return fmt.Errorf("parse provider results: %w", err)
	}
	summary, err := ingestBatchAttempt(
		ctx, pool, attempt, job.Status, results, w.batchLeaseOwner,
	)
	if err != nil {
		return fmt.Errorf("ingest provider results: %w", err)
	}
	w.observeBatchIngest(attempt, job.Status, results, summary)
	w.cleanupBatchFiles(ctx, client, attempt.InputFileID,
		attempt.OutputFileID, attempt.ErrorFileID)
	return nil
}

// TRIPWIRE — KNOWN RESIDUAL TOCTOU, GATE THE BATCH CANARY ON IT.
//
// batchAttemptPrivacySafe commits its transaction before the upload and the
// Batch-create POST, so a concurrent transaction can still set torrents.private
// or insert qBittorrent private/bitgrab evidence in the window between the
// check and provider egress. Re-checking at both provider boundaries narrows
// that window but does not close it; only a lock/lease protocol honoured by
// every native-private and qB evidence writer, or holding the relevant locks
// across the POST under bounded timeouts, actually closes it.
//
// This is currently unreachable in production: JUNKPURGE_LLM_BATCH_ENABLED is
// false and the deployment has never executed a provider Batch run. It must be
// closed BEFORE the purge-disabled Batch canary enables admission, not before
// the surrounding change merges. Do not enable Batch admission while this
// comment is still here.
func (w *purgeWorker) enforceBatchAttemptPrivacy(
	ctx context.Context,
	pool *pgxpool.Pool,
	client batchWorkerClient,
	attempt batchAttempt,
) (bool, error) {
	safe, err := batchAttemptPrivacySafe(
		ctx,
		pool,
		attempt.ID,
		attempt.ItemCount,
		w.batchLeaseOwner,
	)
	if err != nil {
		// DB uncertainty is a hard egress stop. The attempt remains durable and
		// will be rechecked on a later reconcile; no provider POST follows.
		return false, err
	}
	if safe {
		return true, nil
	}

	const message = "current native-private or qBittorrent private/bitgrab evidence blocked provider egress"
	if err := failClaimedBatchAttempt(
		ctx,
		pool,
		attempt.ID,
		w.batchLeaseOwner,
		"pre_provider_privacy_gate",
		message,
	); err != nil {
		return false, err
	}
	// An uploaded-but-not-created legacy attempt has already stored its input
	// remotely. Delete that file after the terminal local transition; never
	// create a provider Batch from it. The id stays on the row until the
	// provider confirms deletion, so a transient failure here is retried by
	// cleanupSettledBatchInputFiles instead of stranding newly-private data on
	// the provider forever.
	w.deleteBatchInputFileDurably(ctx, pool, client, attempt)
	return false, nil
}

// deleteBatchInputFileDurably deletes a settled attempt's provider-hosted input
// and only then clears the id. Leaving the id set on failure is what keeps the
// attempt eligible for a later cleanup-only pass.
func (w *purgeWorker) deleteBatchInputFileDurably(
	ctx context.Context,
	pool *pgxpool.Pool,
	client batchWorkerClient,
	attempt batchAttempt,
) {
	if attempt.InputFileID == "" {
		return
	}
	// DeleteFile already maps provider 404 to success, so a file the provider
	// no longer has is treated as deleted rather than retried forever.
	if err := client.DeleteFile(ctx, attempt.InputFileID); err != nil {
		w.logger.Warnw("junkpurge Batch input cleanup deferred",
			"attempt", attempt.ID,
			"file_id", attempt.InputFileID,
			"err", err)
		return
	}
	if err := clearBatchAttemptInputFile(ctx, pool, attempt.ID); err != nil {
		// The file is gone but the id survived. The next pass re-deletes and
		// gets 404, which is success, so this is safe to retry.
		w.logger.Warnw("junkpurge Batch input cleanup journal",
			"attempt", attempt.ID,
			"file_id", attempt.InputFileID,
			"err", err)
	}
}

// cleanupSettledBatchInputFiles retries provider deletion for settled attempts
// whose input file outlived their terminal transition. It runs on every
// reconcile cycle, including drain-only mode, because privacy-blocked input
// must not wait on Batch admission being re-enabled.
func (w *purgeWorker) cleanupSettledBatchInputFiles(
	ctx context.Context,
	pool *pgxpool.Pool,
	client batchWorkerClient,
) {
	attempts, err := listBatchAttemptsPendingInputCleanup(ctx, pool, 10_000)
	if err != nil {
		w.batchError("cleanup", err)
		return
	}
	for _, attempt := range attempts {
		if ctx.Err() != nil {
			return
		}
		w.deleteBatchInputFileDurably(ctx, pool, client, attempt)
	}
}

func maxDuration(left, right time.Duration) time.Duration {
	if left > right {
		return left
	}
	return right
}

func ambiguityGraceActive(
	attempt batchAttempt,
	postState string,
	grace time.Duration,
	now time.Time,
) bool {
	if attempt.State != postState || attempt.SubmissionAttemptedAt == nil {
		return false
	}
	return now.Before(attempt.SubmissionAttemptedAt.Add(grace))
}

func batchAttemptMetadata(attempt batchAttempt) map[string]string {
	return map[string]string{
		"bitagent_component": "junkpurge",
		"run_id":             strconv.FormatInt(attempt.RunID, 10),
		"attempt_id":         strconv.FormatInt(attempt.ID, 10),
		"attempt_no":         strconv.Itoa(attempt.AttemptNo),
		"input_sha256":       attempt.InputSHA256,
	}
}

func isTerminalBatchStatus(status string) bool {
	switch status {
	case "completed", "failed", "expired", "cancelled":
		return true
	default:
		return false
	}
}

func downloadBatchFile(
	ctx context.Context,
	client batchWorkerClient,
	fileID string,
) ([]byte, error) {
	if fileID == "" {
		return nil, nil
	}
	return client.DownloadFile(ctx, fileID)
}

func (w *purgeWorker) observeBatchIngest(
	attempt batchAttempt,
	providerStatus string,
	results []batchItemResult,
	summary batchIngestSummary,
) {
	// A replay after a committed ingest returns an empty summary. Suppressing
	// metrics here makes reconciliation idempotent within one process.
	settled := summary.Succeeded + summary.RetryableFailures + summary.TerminalFailures
	if settled == 0 {
		return
	}
	w.metrics.batchJobs.WithLabelValues(providerStatus).Inc()
	if summary.Succeeded > 0 {
		w.metrics.batchItems.WithLabelValues("succeeded").Add(float64(summary.Succeeded))
	}
	if summary.RetryableFailures > 0 {
		w.metrics.batchItems.WithLabelValues("retryable_error").Add(float64(summary.RetryableFailures))
	}
	if summary.TerminalFailures > 0 {
		w.metrics.batchItems.WithLabelValues("terminal_error").Add(float64(summary.TerminalFailures))
	}
	for _, result := range results {
		outcome := "error"
		if result.Succeeded {
			outcome = "ok"
			w.metrics.judgedTotal.WithLabelValues(result.Judgment.Verdict).Inc()
		}
		w.metrics.llmRequests.WithLabelValues("batch", attempt.Model, outcome).Inc()
	}
	w.metrics.observeProviderUsage("batch", attempt.Model, summary.Usage)
	w.logger.Infow("junkpurge Batch attempt settled",
		"run_id", summary.RunID,
		"attempt_id", summary.AttemptID,
		"attempt_no", summary.AttemptNo,
		"provider_status", providerStatus,
		"succeeded", summary.Succeeded,
		"retryable", summary.RetryableFailures,
		"terminal", summary.TerminalFailures,
		"missing", summary.Missing,
		"run_state", summary.RunState,
		"input_file", attempt.InputFileID,
		"provider_batch", attempt.ProviderBatchID)
}

func (w *purgeWorker) cleanupBatchFiles(
	ctx context.Context,
	client batchWorkerClient,
	fileIDs ...string,
) {
	seen := make(map[string]struct{}, len(fileIDs))
	for _, id := range fileIDs {
		if id == "" {
			continue
		}
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		if err := client.DeleteFile(ctx, id); err != nil {
			// Cleanup is deliberately post-ingest and best effort: provider
			// retention must never undo an exactly-once DB settlement.
			w.logger.Warnw("junkpurge Batch file cleanup", "file_id", id, "err", err)
		}
	}
}

func (w *purgeWorker) finalizeBatchRuns(
	ctx context.Context,
	pool *pgxpool.Pool,
	allowPurge bool,
) error {
	runs, err := listFinalizingRuns(ctx, pool, 50_000)
	if err != nil {
		return err
	}
	var errs []error
	for _, run := range runs {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		summary, err := finalizeBatchRun(
			ctx, pool, run, allowPurge,
		)
		if err != nil {
			errs = append(errs, fmt.Errorf("run %d: %w", run.ID, err))
			continue
		}
		if summary.State == "" {
			continue
		}
		w.observeBatchFinalization(ctx, summary)
	}
	return errors.Join(errs...)
}

func (w *purgeWorker) observeBatchFinalization(
	ctx context.Context,
	summary batchFinalizeSummary,
) {
	w.metrics.cyclesTotal.Inc()
	w.metrics.lastCycleUnix.Set(float64(time.Now().Unix()))
	switch summary.State {
	case runStateDryRun:
		w.metrics.observeCycleOutcome(
			cycleOutcomeBatchDryRun, summary.Judged, summary.Junk,
		)
		w.metrics.wouldDeleteTotal.Add(float64(summary.WouldDelete))
	case runStateBreakerBlocked:
		w.metrics.observeCycleOutcome(
			cycleOutcomeBatchBreaker, summary.Judged, summary.Junk,
		)
		w.metrics.circuitBreaks.WithLabelValues("junk_rate_anomaly").Inc()
	case runStateCompleted:
		w.metrics.observeCycleOutcome(
			cycleOutcomeBatchCompleted, summary.Judged, summary.Junk,
		)
		w.metrics.quarantinedTotal.Add(float64(len(summary.QuarantinedHashes)))
	}

	if len(summary.QuarantinedHashes) > 0 && w.verdicts != nil {
		days := summary.QuarantineDays
		if days <= 0 {
			days = 30
		}
		expiresAt := time.Now().Add(time.Duration(days) * 24 * time.Hour)
		for _, hash := range summary.QuarantinedHashes {
			if err := w.verdicts.Record(ctx, verdicts.Event{
				InfoHash:  hash,
				Verdict:   verdicts.VerdictQuarantined,
				Mechanism: verdicts.MechanismJunkpurge,
				Reason:    "LLM Batch junk judgment past min-age; snapshot retained",
				ExpiresAt: &expiresAt,
			}); err != nil {
				w.logger.Warnw("junkpurge Batch verdict record", "err", err)
			}
		}
	}
	w.logger.Infow("junkpurge Batch run finalized",
		"run_id", summary.RunID,
		"state", summary.State,
		"judged", summary.Judged,
		"junk", summary.Junk,
		"would_delete", summary.WouldDelete,
		"quarantined", len(summary.QuarantinedHashes))
}

func (w *purgeWorker) refreshBatchInFlight(
	ctx context.Context,
	pool *pgxpool.Pool,
) {
	active, err := countActiveBatchRuns(ctx, pool)
	if err != nil {
		w.batchError("query", err)
		return
	}
	w.metrics.batchInFlight.Set(float64(active))
}

func (w *purgeWorker) batchError(stage string, err error) {
	w.metrics.cycleErrorsTotal.WithLabelValues("batch_" + stage).Inc()
	w.logger.Warnw("junkpurge Batch "+stage, "err", err)
}

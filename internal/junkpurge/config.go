// Package junkpurge runs a scheduled worker that asks its configured LLM to
// judge persistently-unmatched movie/tv torrents ("real-but-mangled" vs.
// "genuinely junk") and deletes ONLY the confident-junk ones.
//
// It is the deliberately-guarded answer to "if even the LLM can't map it,
// delete it": a naive "no TMDB match -> delete" would nuke ~800k torrents
// that are real content with garbage or foreign-language names (verified
// against the live DB). The LLM judges the NAME (independent of TMDB
// reachability, so a TMDB/DNS outage can't corrupt the verdict), spares
// everything it can't confidently call junk, and a circuit breaker pauses
// the whole cycle if the LLM is unavailable or starts flagging an
// implausible fraction of a batch as junk.
//
// Dry-run is the default. Real deletion is opt-in (EnablePurge) so the
// first operational window populates the junkpurge_judgments drop-list for
// review before any row is removed.
package junkpurge

import "time"

// Config controls the junk-purge worker. Registered as "junkpurge" on the
// configfx module; env prefix JUNKPURGE_*.
type Config struct {
	// Enabled admits new judgment cycles. When false, no new work or purge is
	// allowed; a drain-only reconciler may still ingest already-paid durable
	// Batch output. Explicit opt-in.
	Enabled bool `yaml:"enabled"`

	// EnablePurge gates actual deletion. Default false — the worker
	// records LLM verdicts into junkpurge_judgments and emits
	// bitagent_junkpurge_would_delete_total, but never deletes. Flip
	// only after reviewing the dry-run drop-list.
	EnablePurge bool `yaml:"enable_purge"`

	// Interval between cycles.
	Interval time.Duration `yaml:"interval"`

	// MinAge — a torrent is a candidate only if torrents_contents.created_at
	// is older than this. Gives the classifier + reprocess pipeline a fair
	// chance to match before we ever consider it junk.
	MinAge time.Duration `yaml:"min_age"`

	// MinConfidence — the LLM's self-rated confidence must be at least this
	// for a 'junk' verdict to be actioned. Below it, the row is kept.
	MinConfidence float64 `yaml:"min_confidence"`

	// BatchSize caps how many candidates one cycle judges (and at most
	// deletes). Bounds LLM load and delete-statement size per cycle.
	BatchSize int `yaml:"batch_size"`

	// RejudgeInterval — once an info_hash has a judgment row, it is skipped
	// as a candidate until this long has passed. Stops the worker from
	// re-spending LLM calls on the large persistently-unmatched-but-real
	// backlog every cycle.
	RejudgeInterval time.Duration `yaml:"rejudge_interval"`

	// MaxJunkRate is the circuit breaker: if the fraction of a cycle's
	// judged batch that comes back confident-junk exceeds this, the cycle
	// aborts WITHOUT deleting and logs an alert. Genuine junk is a small
	// minority; an implausibly high rate means the LLM/prompt is degraded,
	// not that the library is suddenly all junk.
	MaxJunkRate float64 `yaml:"max_junk_rate"`

	// QuarantineDays is the operator review window. When EnablePurge is on,
	// confident-junk is MOVED to junkpurge_quarantine (out of the main DB)
	// rather than hard-deleted, so it leaves search and Torznab at that moment.
	//
	// After this many days the entry is TOMBSTONED, not deleted: expireQuarantine
	// (worker.go) only stamps expired_at, which drops the row out of the operator
	// review list at /api/quarantine. RestoreQuarantined can still rebuild the
	// torrent from its snapshot indefinitely, and no info_hash is blacklisted.
	// Nothing on this path is irreversible — see the expireQuarantine doc comment
	// and docs/design/junkpurge-safety-program.md.
	QuarantineDays int `yaml:"quarantine_days"`

	// ---- LLM endpoint (the repository default is an inert local Qwen
	// reference; it is not the live contentfilter route). Override per
	// JUNKPURGE_LLM_*. For hosted OpenAI
	// (api.openai.com) set JUNKPURGE_LLM_API_STYLE=chat + JUNKPURGE_LLM_API_KEY
	// (Bearer auth); the key stays empty for a local no-auth endpoint. ----
	LLMBaseURL  string `yaml:"llm_base_url"`
	LLMModel    string `yaml:"llm_model"`
	LLMApiStyle string `yaml:"llm_api_style"`
	LLMApiKey   string `yaml:"llm_api_key"`
	LLMTimeout  string `yaml:"llm_timeout"`
	// LLMOpenaiDataSharing selects the strict direct-OpenAI incentive route.
	// Provider usage receipts, not this local assertion, prove complimentary use.
	LLMOpenaiDataSharing bool `yaml:"llm_openai_data_sharing"`

	// LLMAllowPaidSync explicitly permits per-title synchronous calls when a
	// chat-compatible (potentially metered) provider is configured and Batch
	// mode is off. It defaults false so disabling Batch cannot silently
	// restore the expensive request pattern this feature replaces. Native
	// Ollama remains available without this opt-in.
	LLMAllowPaidSync bool `yaml:"llm_allow_paid_sync"`

	// LLMDailyCallLimit and LLMMonthlyCallLimit are durable provider-request
	// allowances. A grouped request consumes one; each per-title fallback
	// consumes another. Reservations happen immediately before HTTP dispatch,
	// are never refunded, and share the PostgreSQL junkpurge scope across
	// replicas and restarts.
	LLMDailyCallLimit   int `yaml:"llm_daily_call_limit"`
	LLMMonthlyCallLimit int `yaml:"llm_monthly_call_limit"`

	// LLMNamesPerCall groups this many candidate names into ONE synchronous
	// judge request, amortising the ~470-token policy prompt across the
	// group (input tokens per name: 1 → ~600, 10 → ~177; the prompt is 78%
	// of junkpurge's input spend at 1). 1 keeps the classic one-name-per-call
	// request shape byte-identical to prior releases. A grouped reply that
	// fails validation falls back to per-name calls for that group
	// (cycle_errors{stage="judge_group"}), so a format regression degrades
	// cost, never coverage. Bounded 1..50 — beyond that the reply outgrows
	// the completion cap and positional discipline decays.
	// Env: JUNKPURGE_LLM_NAMES_PER_CALL.
	LLMNamesPerCall int `yaml:"llm_names_per_call"`

	// LLMUnavailableConsecutiveLimit bounds how many consecutive synchronous
	// groups may report a transient provider outage before the worker stops
	// making calls for the cycle. A successful group resets the streak.
	//
	// Any unavailable group still blocks ALL quarantine/delete application for
	// that cycle. This knob only lets isolated failures stop wasting the rest of
	// an already-captured evaluation cohort while bounding the call storm during
	// a real outage. Env: JUNKPURGE_LLM_UNAVAILABLE_CONSECUTIVE_LIMIT.
	LLMUnavailableConsecutiveLimit int `yaml:"llm_unavailable_consecutive_limit"`

	// LLMBatchEnabled routes hosted chat-completions judgments through the
	// provider Batch API. It is deliberately independent from EnablePurge:
	// operators can validate submission/result ingestion in dry-run mode
	// before any completed batch is allowed to quarantine content.
	LLMBatchEnabled bool `yaml:"llm_batch_enabled"`

	// LLMBatchPollInterval controls how often non-terminal provider batches
	// are reconciled. New submissions still follow Interval.
	LLMBatchPollInterval time.Duration `yaml:"llm_batch_poll_interval"`

	// LLMBatchMaxInFlight bounds submitted-but-unprocessed provider batches.
	// This is a safety valve, not a request batch size; BatchSize remains the
	// number of titles placed in each provider batch.
	LLMBatchMaxInFlight int `yaml:"llm_batch_max_in_flight"`

	// LLMBatchMaxAttempts bounds provider retries for one logical safety
	// cohort. Only unfinished/transient items are retried; successful items
	// are never resubmitted.
	LLMBatchMaxAttempts int `yaml:"llm_batch_max_attempts"`

	// LLMBatchCompletionWindow is persisted with each job. OpenAI currently
	// supports only "24h"; startup rejects any other value rather than
	// silently changing delivery semantics.
	LLMBatchCompletionWindow string `yaml:"llm_batch_completion_window"`

	// LLMBatchFailureCooldown prevents a permanently malformed/provider-
	// rejected title from being selected and re-billed every hourly cycle.
	// It is intentionally much shorter than RejudgeInterval because failed
	// items never received a valid judgment.
	LLMBatchFailureCooldown time.Duration `yaml:"llm_batch_failure_cooldown"`

	// LLMFailureCooldown is the synchronous-path sibling of
	// LLMBatchFailureCooldown, and exists for exactly the same reason: a title
	// the model reliably answers badly must not be re-selected and re-billed
	// every cycle. The batch path has had this since it was written; the sync
	// path did not, and one torrent whose reply carries a one-character typo
	// in the verdict enum ("real_manged") was re-judged on every cycle for
	// ~967 cycles — pinning cycle_errors{stage="judge"} at 1 per cycle and
	// making that counter useless as an alert threshold.
	//
	// Applied as a lease on the item's junkpurge_sync_claims row, which the
	// candidate query already excludes while lease_until > now(). Much shorter
	// than RejudgeInterval on purpose: a failed item never got a valid
	// judgment, so this is a retry delay, not a re-judge suppression.
	LLMFailureCooldown time.Duration `yaml:"llm_failure_cooldown"`

	// LLMBatchAmbiguityGrace is the minimum list-only reconciliation window
	// after an upload/create POST may have timed out. A successful empty list
	// during this window is not treated as proof that the provider rejected
	// the POST, preventing duplicate paid jobs under eventual consistency.
	LLMBatchAmbiguityGrace time.Duration `yaml:"llm_batch_ambiguity_grace"`

	// LLMBatchFallbackSync is an explicit spend-safety assertion. Synchronous
	// fallback is intentionally unsupported: when Batch is enabled, provider
	// failures stay durable for reconciliation instead of silently returning
	// to full-price per-item requests.
	LLMBatchFallbackSync bool `yaml:"llm_batch_fallback_sync"`
}

// NewDefaultConfig returns conservative defaults. Enabled + EnablePurge are
// both false: including the module is a no-op until JUNKPURGE_ENABLED=true,
// and even then it is dry-run until JUNKPURGE_ENABLE_PURGE=true.
func NewDefaultConfig() Config {
	return Config{
		Enabled:         false, // explicit opt-in
		EnablePurge:     false, // dry-run until operator flips it
		Interval:        1 * time.Hour,
		MinAge:          7 * 24 * time.Hour, // 7 days
		MinConfidence:   0.8,
		BatchSize:       500,
		RejudgeInterval: 90 * 24 * time.Hour, // re-judge a kept item at most every 90d
		MaxJunkRate:     0.5,
		QuarantineDays:  30, // review window before permanent delete + blacklist
		// Conservative local reference for an explicitly enabled development
		// deployment. Production routes are separate operational inventory;
		// never infer them from this disabled code default. The native
		// /api/chat + think:false path gets a terse verdict from Ollama.
		LLMBaseURL:  "http://127.0.0.1:11434/v1",
		LLMModel:    "qwen3.6:35b",
		LLMApiStyle: "ollama",
		// 60s (was 20s): a local Qwen may be shared with other sessions, so
		// junkpurge's burst of calls queues. At 20s, queued calls
		// timed out -> classified ErrLLMUnavailable -> the breaker tripped after
		// ~26-200 items every cycle (only ~6k judged in 17h). 60s lets queued
		// calls finish instead of false-tripping.
		LLMTimeout:          "60s",
		LLMAllowPaidSync:    false,
		LLMDailyCallLimit:   50,
		LLMMonthlyCallLimit: 1500,
		LLMNamesPerCall:     1, // classic shape; operators raise it deliberately
		// Three probes cost at most three grouped requests during a real outage,
		// while allowing one isolated 429/5xx/transport failure to leave the rest
		// of the cycle useful for evaluation. Destructive application remains
		// blocked after even one unavailable group.
		LLMUnavailableConsecutiveLimit: 3,
		LLMBatchEnabled:                false,
		LLMBatchPollInterval:           1 * time.Minute,
		LLMBatchMaxInFlight:            1,
		LLMBatchMaxAttempts:            3,
		LLMBatchCompletionWindow:       "24h",
		LLMBatchFailureCooldown:        24 * time.Hour,
		LLMFailureCooldown:             24 * time.Hour,
		LLMBatchAmbiguityGrace:         1 * time.Hour,
		LLMBatchFallbackSync:           false,
	}
}

// timeout parses LLMTimeout, falling back to 60s on empty/invalid input.
func (c Config) timeout() time.Duration {
	if c.LLMTimeout == "" {
		return 60 * time.Second
	}
	d, err := time.ParseDuration(c.LLMTimeout)
	if err != nil || d <= 0 {
		return 60 * time.Second
	}
	return d
}

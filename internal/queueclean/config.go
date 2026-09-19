// Package queueclean runs a scheduled worker that purges stale
// queue_jobs rows. The DHT crawl pipeline writes process_torrent jobs
// to a Postgres-backed queue; once a job moves to status='processed'
// or 'failed' it can be safely deleted to keep the table + its 825 MB
// queue_payload index lean.
//
// Today's symptom (2026-04-26 audit): 13317 processed + 1393 failed
// rows accumulated since boot, never purged. Index footprint
// (queue_jobs_queue_payload_idx) grew to be larger than the data
// itself.
//
// Two-stage opt-in mirrors retention:
//
//	QUEUECLEAN_ENABLED=false          (default) — pure no-op
//	QUEUECLEAN_ENABLED=true,
//	  QUEUECLEAN_ENABLE_PURGE=false   shadow mode — emits would_purge counts
//	QUEUECLEAN_ENABLED=true,
//	  QUEUECLEAN_ENABLE_PURGE=true    deletes per cycle
//
// See AGENTS/SPEC_db_hygiene_2026-04-26.md §1.
package queueclean

import "time"

// Config controls queueclean behaviour. Registered as the
// "queueclean" configfx section; env prefix QUEUECLEAN_*.
type Config struct {
	// Enabled turns the worker on. When false, no scheduled cycles run
	// and no metrics are emitted.
	Enabled bool `yaml:"enabled"`

	// EnablePurge gates whether we actually delete. Default false —
	// the worker emits queueclean_would_purge_total but doesn't touch
	// the DB. Flip after dry-run counts have been reviewed.
	EnablePurge bool `yaml:"enable_purge"`

	// Interval between purge cycles. Default 1h is fine: queue_jobs
	// is bounded by the BEP-9 fetch rate, so it grows at most a few
	// hundred rows per cycle.
	Interval time.Duration `yaml:"interval"`

	// RetentionAge — purge candidates must be at least this old.
	// Conservative default: 7d. Operator can drop to 24h once steady
	// state is observed.
	RetentionAge time.Duration `yaml:"retention_age"`

	// BatchSize caps how many rows one cycle deletes. Protects against
	// long-held locks if RetentionAge gets cut and a backlog clears
	// at once.
	BatchSize int `yaml:"batch_size"`

	// PurgeStatuses lists which queue_jobs.status values are eligible
	// for purge. Default ["processed", "failed"]. Pending jobs are
	// never purged regardless of age.
	PurgeStatuses []string `yaml:"purge_statuses"`
}

// NewDefaultConfig — explicit opt-in, dry-run by default. Same shape
// as retention.NewDefaultConfig.
func NewDefaultConfig() Config {
	return Config{
		Enabled:       false,
		EnablePurge:   false,
		Interval:      1 * time.Hour,
		RetentionAge:  7 * 24 * time.Hour,
		BatchSize:     5000,
		PurgeStatuses: []string{"processed", "failed"},
	}
}

// Package evidence ingests authoritative category/media signals from
// qBittorrent instances and *arr services (Sonarr, Radarr, Readarr,
// Lidarr), persists them to label_evidence as an append-only audit
// trail, and derives a per-infohash winning label in
// torrent_canonical_labels via explicit precedence rules.
//
// Canonical labels are consumed before the classifier runs; see
// ARCHITECTURE.md §1 for the pre-emption contract.
package evidence

import "time"

// Config drives which external sources the evidence ingestor attaches
// to. The shape is deliberately flat (named slots per *arr kind,
// named slots per qB instance) so every field resolves from a single
// environment variable — upstream's env resolver does not walk
// slice-of-struct types. YAML deploys can set the same fields.
//
// Registered as "evidence" on the configfx module — environment-variable
// overrides follow the EVIDENCE_* prefix (e.g.
// EVIDENCE_SONARR_BASE_URL=http://host:8989).
type Config struct {
	// WebhookSecret is a shared secret required in the X-Evidence-Token
	// header on every incoming *arr webhook. Empty disables auth (dev only).
	// In production set this via Infisical and mirror the value in each
	// Sonarr/Radarr Connection → Webhook configuration as a Custom Header.
	WebhookSecret string `yaml:"webhook_secret"`

	// ArrPollInterval is how often the *arr history poller runs.
	// Backfill cadence; the webhook is the primary low-latency path.
	ArrPollInterval time.Duration `yaml:"arr_poll_interval"`

	// QBPollInterval is how often each qB instance is polled for
	// torrent categories and tags.
	QBPollInterval time.Duration `yaml:"qb_poll_interval"`

	// Named *arr slots. A slot is considered configured when
	// BaseURL is non-empty; otherwise the corresponding worker
	// skips it silently.
	Sonarr  ArrInstance `yaml:"sonarr"`
	Radarr  ArrInstance `yaml:"radarr"`
	Readarr ArrInstance `yaml:"readarr"`
	Lidarr  ArrInstance `yaml:"lidarr"`

	// Named qB slots. Most deploys have one qB; add more here in
	// sequence (QBBeta, QBCharlie) as the fleet grows.
	QBAlpha QBInstance `yaml:"qb_alpha"`
	QBBeta  QBInstance `yaml:"qb_beta"`

	// Liveness controls the qB-state-driven blacklist.
	Liveness LivenessConfig `yaml:"liveness"`

	// OutcomePriors controls the *arr-grab outcome learner that
	// re-ranks Torznab search results by historical success rate.
	OutcomePriors OutcomePriorsConfig `yaml:"outcome_priors"`
}

// OutcomePriorsConfig drives the priors module. Disabled by default;
// turning it on starts collecting per-feature Beta(α, β) priors from
// observed *arr Grab/Download events but does not change Torznab
// ranking until OutcomePriorsConfig.Apply is also true.
//
// All fields are flat scalars so the env resolver picks them up via
// the EVIDENCE_OUTCOME_PRIORS_* prefix (e.g.
// EVIDENCE_OUTCOME_PRIORS_ENABLED=true).
type OutcomePriorsConfig struct {
	// Enabled toggles the entire module. When false the resolver
	// short-circuits, the expirer worker is dormant, and the Torznab
	// adapter performs no ranking lookup.
	Enabled bool `yaml:"enabled"`

	// Apply gates the actual re-ranking of Torznab search results.
	// When false the priors are still collected and exposed in
	// metrics, but the result order returned to *arrs is unchanged.
	// This is the safe-rollout knob — collect priors for a window,
	// inspect the metrics, only then flip Apply=true.
	Apply bool `yaml:"apply"`

	// ResolutionWindow is how long we wait after a Grab before
	// declaring it a failure if no matching Download arrived. Tune
	// to the slowest catalog content typically takes to import: too
	// short and we mark in-progress imports as failures; too long
	// and the priors take forever to learn from missing imports.
	ResolutionWindow time.Duration `yaml:"resolution_window"`

	// ExpirerInterval is the cadence of the expirer worker. The
	// worker runs every interval, expiring any pending grabs older
	// than ResolutionWindow. Setting this much shorter than
	// ResolutionWindow gives near-real-time failure resolution; it
	// must not exceed ResolutionWindow.
	ExpirerInterval time.Duration `yaml:"expirer_interval"`

	// MinObservations is the floor on (α + β − 2) below which a
	// per-key prior is ignored and the global average is used
	// instead. Prevents a single observation from biasing rankings
	// against a brand-new release group / source.
	MinObservations int `yaml:"min_observations"`

	// ExcludePrivateTrackerGrabs mirrors the liveness flag of the
	// same name. When true (the default), import events tagged with
	// a private-tracker category do not contribute α — those grabs
	// prove nothing about whether bitagent's catalog can serve the
	// same hash from the public swarm.
	ExcludePrivateTrackerGrabs bool `yaml:"exclude_private_tracker_grabs"`

	// PurgeResolvedAfterDays controls how long resolved
	// torrent_grab_attempts rows survive before the expirer purges
	// them. The priors table itself never shrinks; this housekeeping
	// applies only to the per-grab projection.
	PurgeResolvedAfterDays int `yaml:"purge_resolved_after_days"`
}

// NewDefaultOutcomePriorsConfig returns the default priors configuration —
// disabled, with a 48h resolution window and a 30m expirer cadence.
func NewDefaultOutcomePriorsConfig() OutcomePriorsConfig {
	return OutcomePriorsConfig{
		Enabled:                    false,
		Apply:                      false,
		ResolutionWindow:           48 * time.Hour,
		ExpirerInterval:            30 * time.Minute,
		MinObservations:            10,
		ExcludePrivateTrackerGrabs: true,
		PurgeResolvedAfterDays:     30,
	}
}

// PurgeResolvedAfter returns PurgeResolvedAfterDays as a duration.
func (c OutcomePriorsConfig) PurgeResolvedAfter() time.Duration {
	return time.Duration(c.PurgeResolvedAfterDays) * 24 * time.Hour
}

// LivenessConfig drives the liveness module. Disabled by default —
// turning it on requires an explicit opt-in so existing deploys see
// no behaviour change until the operator decides to engage it.
//
// All fields are flat scalars so the env resolver picks them up via
// the EVIDENCE_LIVENESS_* prefix (e.g.
// EVIDENCE_LIVENESS_STALL_THRESHOLD_HOURS=4).
type LivenessConfig struct {
	// Enabled toggles the entire module. When false the resolver
	// short-circuits, the revalidator worker is dormant, and the
	// Torznab adapter performs no exclusion lookup.
	Enabled bool `yaml:"enabled"`

	// StallThresholdHours is how long a torrent must remain in a
	// suspect state class before it can be flipped to dead. A
	// shorter threshold catches dead torrents faster but risks
	// flipping torrents that are merely waiting for a tracker
	// announce window. 4h is conservative.
	StallThresholdHours int `yaml:"stall_threshold_hours"`

	// MinObservations is the minimum number of suspect observations
	// required to corroborate a stall. Combined with the threshold
	// this prevents a single transient observation (e.g. peer just
	// disconnected mid-poll) from burying an otherwise-healthy
	// infohash.
	MinObservations int `yaml:"min_observations"`

	// BlacklistTTLDays controls how long a dead infohash remains
	// dead before the revalidator is allowed to retry. Re-trying
	// every dead hash on every cycle would defeat the purpose;
	// re-trying never would prevent recovery from transient swarm
	// failures.
	BlacklistTTLDays int `yaml:"blacklist_ttl_days"`

	// RevalidateInterval is the cadence of the revalidator worker.
	RevalidateInterval time.Duration `yaml:"revalidate_interval"`

	// RevalidateBatchSize caps how many dead infohashes the worker
	// considers per cycle. The DHT layer is bounded; oversubscribing
	// it would degrade the crawler.
	RevalidateBatchSize int `yaml:"revalidate_batch_size"`

	// RevalidateMinPeers is the floor on peers a get_peers query
	// must return before an infohash is restored to alive. 1 is
	// noisy (any responder counts), 2 is the smallest number that
	// rules out a single misbehaving peer.
	RevalidateMinPeers int `yaml:"revalidate_min_peers"`

	// RevalidateTimeout is the per-infohash budget for the DHT
	// query fan-out during a revalidation cycle.
	RevalidateTimeout time.Duration `yaml:"revalidate_timeout"`

	// ExcludePrivateTrackerGrabs decides whether *arr import
	// webhooks tagged with a private-tracker category should mark
	// the infohash alive. Default true: bitagent's catalog is
	// public-DHT-derived and a successful import that came from a
	// private tracker proves nothing about whether the same hash is
	// fetchable from the public swarm.
	ExcludePrivateTrackerGrabs bool `yaml:"exclude_private_tracker_grabs"`
}

// NewDefaultLivenessConfig returns the default liveness configuration —
// disabled, with thresholds tuned for the production catalog.
func NewDefaultLivenessConfig() LivenessConfig {
	return LivenessConfig{
		Enabled:                    false,
		StallThresholdHours:        4,
		MinObservations:            2,
		BlacklistTTLDays:           30,
		RevalidateInterval:         6 * time.Hour,
		RevalidateBatchSize:        50,
		RevalidateMinPeers:         2,
		RevalidateTimeout:          30 * time.Second,
		ExcludePrivateTrackerGrabs: true,
	}
}

// StallThreshold returns StallThresholdHours as a duration.
func (c LivenessConfig) StallThreshold() time.Duration {
	return time.Duration(c.StallThresholdHours) * time.Hour
}

// BlacklistTTL returns BlacklistTTLDays as a duration.
func (c LivenessConfig) BlacklistTTL() time.Duration {
	return time.Duration(c.BlacklistTTLDays) * 24 * time.Hour
}

// ArrInstance identifies a single *arr service. Kind is derived from
// the slot name (sonarr -> sonarr) by the consumer; the struct
// carries only the connection parameters.
type ArrInstance struct {
	BaseURL string `yaml:"base_url"`
	APIKey  string `yaml:"api_key"`
}

// QBInstance identifies a single qBittorrent WebUI endpoint.
type QBInstance struct {
	BaseURL  string `yaml:"base_url"`
	Username string `yaml:"username"`
	Password string `yaml:"password"`
}

// NewDefaultConfig returns a config with no sources attached and
// sensible poll intervals. Attaching sources is a deploy concern.
func NewDefaultConfig() Config {
	return Config{
		ArrPollInterval: 15 * time.Minute,
		QBPollInterval:  15 * time.Minute,
		Liveness:        NewDefaultLivenessConfig(),
		OutcomePriors:   NewDefaultOutcomePriorsConfig(),
	}
}

// ArrSlots returns the configured *arr instances as a stable list
// (name, kind, ArrInstance) suitable for iteration by workers. Order
// is fixed: Sonarr, Radarr, Readarr, Lidarr. Slots with a blank
// BaseURL are omitted.
func (c Config) ArrSlots() []ConfiguredArrInstance {
	slots := []struct {
		name string
		src  Source
		inst ArrInstance
	}{
		{"sonarr", SourceSonarr, c.Sonarr},
		{"radarr", SourceRadarr, c.Radarr},
		{"readarr", SourceReadarr, c.Readarr},
		{"lidarr", SourceLidarr, c.Lidarr},
	}
	out := make([]ConfiguredArrInstance, 0, len(slots))
	for _, s := range slots {
		if s.inst.BaseURL == "" {
			continue
		}
		out = append(out, ConfiguredArrInstance{
			Name:    s.name,
			Kind:    s.src,
			BaseURL: s.inst.BaseURL,
			APIKey:  s.inst.APIKey,
		})
	}
	return out
}

// QBSlots returns the configured qB instances. Slots with a blank
// BaseURL are omitted.
func (c Config) QBSlots() []ConfiguredQBInstance {
	slots := []struct {
		name string
		inst QBInstance
	}{
		{"qb-alpha", c.QBAlpha},
		{"qb-beta", c.QBBeta},
	}
	out := make([]ConfiguredQBInstance, 0, len(slots))
	for _, s := range slots {
		if s.inst.BaseURL == "" {
			continue
		}
		out = append(out, ConfiguredQBInstance{
			Name:     s.name,
			BaseURL:  s.inst.BaseURL,
			Username: s.inst.Username,
			Password: s.inst.Password,
		})
	}
	return out
}

// ConfiguredArrInstance is the flat view a worker consumes.
type ConfiguredArrInstance struct {
	Name    string
	Kind    Source
	BaseURL string
	APIKey  string
}

// ConfiguredQBInstance is the flat view a worker consumes.
type ConfiguredQBInstance struct {
	Name     string
	BaseURL  string
	Username string
	Password string
}

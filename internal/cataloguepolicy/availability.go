package cataloguepolicy

import (
	"sort"
	"time"
)

const AvailabilityVersion = "fresh-availability-shadow-v1"

// Observation is a qualified public availability signal. ID identifies the
// original source observation, so replaying a scrape cannot multiply zeros.
type Observation struct {
	ID         string    `json:"id"`
	Source     string    `json:"source"`
	Class      string    `json:"class"` // positive, zero, stall or unknown
	ObservedAt time.Time `json:"observed_at"`
	ExpiresAt  time.Time `json:"expires_at"`
	Qualified  bool      `json:"qualified"`
	Private    bool      `json:"private"`
}

// AvailabilityConfig contains experimental thresholds, not an accepted risk
// budget. They can produce suspicion only, never a confirmed exclusion.
type AvailabilityConfig struct {
	FreshFor            time.Duration `json:"fresh_for"`
	RecentPositiveFor   time.Duration `json:"recent_positive_for"`
	MinimumSpan         time.Duration `json:"minimum_span"`
	MinimumGap          time.Duration `json:"minimum_gap"`
	MinimumObservations int           `json:"minimum_observations"`
}

func DefaultAvailabilityConfig() AvailabilityConfig {
	return AvailabilityConfig{24 * time.Hour, 7 * 24 * time.Hour, 4 * time.Hour, time.Hour, 2}
}

// AvailabilityDecision has no effect on the legacy torrent_liveness table.
type AvailabilityDecision struct {
	State          string    `json:"state"`
	Reason         string    `json:"reason"`
	Version        string    `json:"version"`
	EvaluatedAt    time.Time `json:"evaluated_at"`
	ExpiresAt      time.Time `json:"expires_at"`
	ObservationIDs []string  `json:"observation_ids"`
}

// EvaluateAvailability recomputes from source timestamps on every evaluation.
// New positives revive immediately; expired negative evidence becomes unknown.
// Tracker outages and client IO/paused/unknown states are unqualified unknowns.
// Policy-confirmed-unavailable is deliberately unreachable without separately
// accepted criteria and independent adjudication, which this contract lacks.
func EvaluateAvailability(input []Observation, now time.Time, cfg AvailabilityConfig) AvailabilityDecision {
	d := AvailabilityDecision{State: "unknown_stale", Reason: "no_fresh_qualified_observation", Version: AvailabilityVersion, EvaluatedAt: now, ExpiresAt: now, ObservationIDs: []string{}}
	if cfg.FreshFor <= 0 || cfg.RecentPositiveFor <= 0 || cfg.MinimumSpan <= 0 || cfg.MinimumGap <= 0 || cfg.MinimumObservations < 2 {
		d.Reason = "invalid_shadow_thresholds"
		return d
	}
	observations := append([]Observation(nil), input...)
	sort.Slice(observations, func(i, j int) bool {
		if observations[i].ObservedAt.Equal(observations[j].ObservedAt) {
			return observations[i].ID < observations[j].ID
		}
		return observations[i].ObservedAt.Before(observations[j].ObservedAt)
	})
	seen := map[string]bool{}
	var negatives []Observation
	var lastPositive time.Time
	var latestUnknown time.Time
	for _, o := range observations {
		if o.Private || o.ID == "" || o.Source == "" || o.ObservedAt.IsZero() || o.ObservedAt.After(now) || !o.ExpiresAt.After(o.ObservedAt) || seen[o.ID] {
			continue
		}
		seen[o.ID] = true
		if !o.Qualified || o.Class == "unknown" {
			if o.ObservedAt.After(latestUnknown) {
				latestUnknown = o.ObservedAt
			}
			continue
		}
		if o.Class == "positive" {
			if o.ObservedAt.After(lastPositive) {
				lastPositive = o.ObservedAt
			}
			if now.Before(o.ExpiresAt) && now.Sub(o.ObservedAt) < cfg.FreshFor {
				d.State = "fresh_positive"
				d.Reason = "fresh_public_positive"
				d.ExpiresAt = earlier(o.ExpiresAt, o.ObservedAt.Add(cfg.FreshFor))
				d.ObservationIDs = []string{o.ID}
			}
			continue
		}
		if (o.Class == "zero" || o.Class == "stall") && now.Before(o.ExpiresAt) && now.Sub(o.ObservedAt) < cfg.FreshFor {
			negatives = append(negatives, o)
		}
	}
	if d.State == "fresh_positive" {
		return d
	}
	var qualified []Observation
	for _, o := range negatives {
		if !o.ObservedAt.After(latestUnknown) || !o.ObservedAt.After(lastPositive) {
			continue
		}
		if len(qualified) == 0 || o.ObservedAt.Sub(qualified[len(qualified)-1].ObservedAt) >= cfg.MinimumGap {
			qualified = append(qualified, o)
		}
	}
	if len(qualified) == 0 {
		return d
	}
	latest := qualified[len(qualified)-1]
	d.ExpiresAt = earlier(latest.ExpiresAt, latest.ObservedAt.Add(cfg.FreshFor))
	d.State = "fresh_known_zero"
	d.Reason = "single_qualified_zero"
	if latest.Class == "stall" {
		d.State = "unknown_stale"
		d.Reason = "client_stall_is_not_swarm_zero"
	}
	for _, o := range qualified {
		d.ObservationIDs = append(d.ObservationIDs, o.ID)
		d.ExpiresAt = earlier(d.ExpiresAt, earlier(o.ExpiresAt, o.ObservedAt.Add(cfg.FreshFor)))
	}
	if !lastPositive.IsZero() && now.Sub(lastPositive) < cfg.RecentPositiveFor {
		d.Reason = "recent_positive_history_protects"
		return d
	}
	if len(qualified) >= cfg.MinimumObservations && latest.ObservedAt.Sub(qualified[0].ObservedAt) >= cfg.MinimumSpan {
		d.State = "suspected_unavailable"
		d.Reason = "repeated_qualified_negative_shadow_only"
	}
	return d
}

func earlier(a, b time.Time) time.Time {
	if a.Before(b) {
		return a
	}
	return b
}

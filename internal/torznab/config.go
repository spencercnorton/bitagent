package torznab

import (
	"crypto/subtle"
	"strings"

	"github.com/spencercnorton/bitagent/internal/slice"
)

type Config struct {
	Profiles []Profile

	// APIKey gates every /torznab request when non-empty. Backwards
	// compatible: an empty value preserves the original open behaviour
	// for operators running bitagent on a trusted private network.
	//
	// Set via env TORZNAB_API_KEY. Required for any public-internet
	// deployment; the README documents private-tracker setup on first
	// run.
	//
	// Clients (Prowlarr, hand-curl, etc.) send the key as the standard
	// Torznab query parameter `apikey=...`, OR via X-Api-Key header for
	// convenience. Compared in constant time at request boundary.
	APIKey string `yaml:"api_key"`

	// APIKeys is a comma-separated list of named keys for per-consumer
	// access. Format: `name1:value1,name2:value2,...`. Names appear in
	// `bitagent_torznab_requests_total{key_name=...}` so the operator
	// can audit which consumer hit the indexer and revoke a single key
	// without rotating every consumer's key.
	//
	// Coexists with APIKey: a request matching EITHER the legacy single
	// APIKey (logged as key_name="default") OR any value in APIKeys is
	// accepted. Set via env TORZNAB_API_KEYS.
	//
	// Names must be `[a-z0-9_-]` (kept lowercase for stable metric
	// labels). Values must be non-empty. Malformed entries are skipped
	// silently rather than failing config-load — typos in one entry
	// shouldn't take down the indexer for everyone else.
	APIKeys string `yaml:"api_keys"`

	// HideZeroSeeders drops items from the response when every tracker
	// source has scraped Seeders=0. *arrs treat seeders=0 as "queue it
	// anyway" and the resulting grab sits in metaDL forever before
	// hades reaps it. Default true.
	//
	// Set via env TORZNAB_HIDE_ZERO_SEEDERS.
	HideZeroSeeders bool `yaml:"hide_zero_seeders"`

	// HideUnknownSeedersAgeDays drops items where we have NO valid
	// scraped seeder data anywhere AND the most-recent source row is
	// older than this many days. Catches stale DHT-only discoveries
	// that haven't been re-seen recently. Default 7. Zero disables.
	//
	// Set via env TORZNAB_HIDE_UNKNOWN_SEEDERS_AGE_DAYS.
	HideUnknownSeedersAgeDays int `yaml:"hide_unknown_seeders_age_days"`

	// ZeroSeedersAuthoritativeOnly changes how the filters above read a
	// zero: only the 'tracker' source row (mirrored from the BEP-15
	// scrape ledger) counts as a real 0. The DHT BEP-33 bloom
	// approximation frequently reads 0 for a young, alive swarm, and
	// treating that as a real zero hides our own fresh content from the
	// *arrs — a self-inflicted loss. When set, a bloom-only zero is
	// served as honest-unknown (no seeders attr, so *arr minimumSeeders
	// treats it as unknown) and ages out via HideUnknownSeedersAgeDays
	// instead. Default true; set false for the pre-v0.41 behavior.
	//
	// Set via env TORZNAB_ZERO_SEEDERS_AUTHORITATIVE_ONLY.
	ZeroSeedersAuthoritativeOnly bool `yaml:"zero_seeders_authoritative_only"`
}

// NamedAPIKey is a parsed entry from Config.APIKeys.
type NamedAPIKey struct {
	Name  string
	Value string
}

// ParseNamedAPIKeys returns the parsed list of (name, value) pairs from
// Config.APIKeys plus, when APIKey is non-empty, a synthetic entry with
// name "default" so the legacy single-key path lands the same metric
// label as the rest.
//
// Malformed entries are silently dropped — see APIKeys doc.
func (c Config) ParseNamedAPIKeys() []NamedAPIKey {
	out := make([]NamedAPIKey, 0)
	if c.APIKey != "" {
		out = append(out, NamedAPIKey{Name: "default", Value: c.APIKey})
	}
	for _, raw := range strings.Split(c.APIKeys, ",") {
		raw = strings.TrimSpace(raw)
		if raw == "" {
			continue
		}
		idx := strings.Index(raw, ":")
		if idx <= 0 || idx == len(raw)-1 {
			// missing colon, leading colon, or trailing colon → malformed
			continue
		}
		name := strings.TrimSpace(strings.ToLower(raw[:idx]))
		value := strings.TrimSpace(raw[idx+1:])
		if name == "" || value == "" || !validKeyName(name) {
			continue
		}
		out = append(out, NamedAPIKey{Name: name, Value: value})
	}
	return out
}

// LookupAPIKey returns the name of the matching key when `presented`
// matches any configured key. Comparison is constant-time per entry
// to avoid leaking length / prefix info via timing.
//
// Returns ("", false) when no key is configured (open mode) or no
// match is found. Callers distinguish via Config.AnyAPIKeyConfigured.
func (c Config) LookupAPIKey(presented string) (string, bool) {
	if presented == "" {
		return "", false
	}
	for _, nk := range c.ParseNamedAPIKeys() {
		if subtle.ConstantTimeCompare([]byte(presented), []byte(nk.Value)) == 1 {
			return nk.Name, true
		}
	}
	return "", false
}

// AnyAPIKeyConfigured reports whether the operator has set ANY key
// (legacy APIKey or APIKeys). Used to gate "open mode" — when no key
// is configured, all requests pass for backward compat.
func (c Config) AnyAPIKeyConfigured() bool {
	if c.APIKey != "" {
		return true
	}
	for _, raw := range strings.Split(c.APIKeys, ",") {
		if strings.TrimSpace(raw) != "" {
			return true
		}
	}
	return false
}

// validKeyName allows only [a-z0-9_-]. Names are used as Prometheus
// label values (bitagent_torznab_requests_total{key_name=...}); strict
// charset keeps cardinality bounded and avoids label-injection
// surprises.
func validKeyName(name string) bool {
	if name == "" {
		return false
	}
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z':
		case r >= '0' && r <= '9':
		case r == '_' || r == '-':
		default:
			return false
		}
	}
	return true
}

func (c Config) MergeDefaults() Config {
	c.Profiles = slice.Map(c.Profiles, func(profile Profile) Profile {
		return profile.MergeDefaults()
	})

	return c
}

func NewDefaultConfig() Config {
	return Config{
		HideZeroSeeders:              true,
		HideUnknownSeedersAgeDays:    7,
		ZeroSeedersAuthoritativeOnly: true,
	}
}

func (c Config) GetProfile(name string) (Profile, bool) {
	for _, p := range c.Profiles {
		if strings.EqualFold(p.ID, name) {
			return p, true
		}
	}

	return Profile{}, false
}

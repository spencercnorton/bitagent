package config

import (
	"github.com/go-playground/validator/v10"
	"github.com/spencercnorton/bitagent/internal/classifier/contentfilter"
	"github.com/spencercnorton/bitagent/internal/config/configresolver"
	"github.com/spencercnorton/bitagent/internal/csamblocklist"
	"github.com/spencercnorton/bitagent/internal/evidence"
	"github.com/spencercnorton/bitagent/internal/seeds"
	"testing"
	"time"
)

func TestEnvBinding_CsamBlocklistFeedUrls(t *testing.T) {
	const wantURL = "https://feed.example.invalid/list.txt"
	env := map[string]string{
		"CSAM_BLOCKLIST_FEED_URLS": wantURL,
	}
	resolved := resolveSectionFromEnv(t, "csam_blocklist", csamblocklist.NewDefaultConfig(), env)
	cfg, ok := resolved.(csamblocklist.Config)
	if !ok {
		t.Fatalf("resolved value is %T, want csamblocklist.Config", resolved)
	}
	if len(cfg.FeedUrls) != 1 || cfg.FeedUrls[0] != wantURL {
		t.Fatalf("CSAM_BLOCKLIST_FEED_URLS did not bind: FeedUrls=%v, want [%q]",
			cfg.FeedUrls, wantURL)
	}
}

// TestEnvBinding_ContentFilterDropLossyAudioOnly proves the documented env var
// CONTENT_FILTER_DROP_LOSSY_AUDIO_ONLY binds the renamed DropLossyAudioOnly
// field (the rename that dodged the DropMP3Only digit-split footgun). It also
// pins that the legacy CONTENT_FILTER_DROP_MP_3_ONLY no longer binds the field
// through the resolver — its honoring now lives in the provideFilter back-compat
// shim, asserted by TestApplyDropLossyAudioOnlyBackCompat in contentfilterfx.

func TestEnvBinding_ContentFilterDropLossyAudioOnly(t *testing.T) {
	// Default is true; assert the working env var can flip it to false.
	env := map[string]string{
		"CONTENT_FILTER_DROP_LOSSY_AUDIO_ONLY": "false",
	}
	resolved := resolveSectionFromEnv(t, "content_filter", contentfilter.NewDefaultConfig(), env)
	cfg, ok := resolved.(contentfilter.Config)
	if !ok {
		t.Fatalf("resolved value is %T, want contentfilter.Config", resolved)
	}
	if cfg.DropLossyAudioOnly {
		t.Fatalf("CONTENT_FILTER_DROP_LOSSY_AUDIO_ONLY=false did not bind: DropLossyAudioOnly=true")
	}

	// The legacy env var must NOT bind the renamed field through the resolver
	// (the default true survives) — back-compat is handled in provideFilter, not here.
	envLegacy := map[string]string{
		"CONTENT_FILTER_DROP_MP_3_ONLY": "false",
	}
	resolvedLegacy := resolveSectionFromEnv(t, "content_filter", contentfilter.NewDefaultConfig(), envLegacy)
	cfgLegacy, ok := resolvedLegacy.(contentfilter.Config)
	if !ok {
		t.Fatalf("resolved value is %T, want contentfilter.Config", resolvedLegacy)
	}
	if !cfgLegacy.DropLossyAudioOnly {
		t.Fatalf("legacy CONTENT_FILTER_DROP_MP_3_ONLY unexpectedly bound the renamed " +
			"DropLossyAudioOnly field through the resolver; back-compat must be shim-only")
	}
}

// TestEnvBinding_ContentFilterLLMApiStyle asserts the documented env var
// CONTENT_FILTER_LLM_API_STYLE binds to LLMApiStyle (the rename that dodged the
// stacked-acronym footgun; the old LLMAPIStyle would have bound LLMAPI_STYLE).

func TestEnvBinding_SeedsTrackerUrls(t *testing.T) {
	env := map[string]string{
		"SEEDS_ENABLED":          "true",
		"SEEDS_ENABLE_WRITE":     "true",
		"SEEDS_TRACKER_URLS":     "udp://a.example:1337/announce,udp://b.example:6969/announce",
		"SEEDS_BATCH_SIZE":       "123",
		"SEEDS_MIN_RESCRAPE_AGE": "6h",
	}
	resolved := resolveSectionFromEnv(t, "seeds", seeds.NewDefaultConfig(), env)
	cfg, ok := resolved.(seeds.Config)
	if !ok {
		t.Fatalf("resolved value is %T, want seeds.Config", resolved)
	}
	if !cfg.Enabled || !cfg.EnableWrite {
		t.Fatalf("SEEDS_ENABLED/SEEDS_ENABLE_WRITE did not bind: %+v", cfg)
	}
	if len(cfg.TrackerUrls) != 2 ||
		cfg.TrackerUrls[0] != "udp://a.example:1337/announce" ||
		cfg.TrackerUrls[1] != "udp://b.example:6969/announce" {
		t.Fatalf("SEEDS_TRACKER_URLS did not bind: TrackerUrls=%v", cfg.TrackerUrls)
	}
	if cfg.BatchSize != 123 {
		t.Fatalf("SEEDS_BATCH_SIZE did not bind: BatchSize=%d, want 123", cfg.BatchSize)
	}
	if cfg.MinRescrapeAge.Hours() != 6 {
		t.Fatalf("SEEDS_MIN_RESCRAPE_AGE did not bind: MinRescrapeAge=%s, want 6h", cfg.MinRescrapeAge)
	}
}

// TestEnvBinding_ComposeKeys pins the documented backend Compose keys and
// optional processing integrations against the section each configures.
//
// Both files shipped keys carrying a `BITMAGNET_` prefix that binds to
// nothing: the resolver joins the section key with the ToSnake'd Go field
// names and uppercases (see resolveStructNode and
// configresolver.envResolver.Resolve) — there is no prefix in the scheme at
// all. An unmatched key is not an error, so anyone deploying from those files
// got the default value silently. Production (Galactic-Torrent, 105 env vars)
// has always used the unprefixed spelling; the compose files were the
// outliers.
//
// Each case is asserted in both directions: the documented key must bind
// every field it names, and the legacy `BITMAGNET_`-prefixed spelling must
// bind nothing, so a well-meaning "restore the prefix" cannot pass.
//
// The `evidence` section carried the same defect in a shape of its own (an
// indexed INSTANCES_0 list against named struct slots); its keys are pinned by
// TestEnvBinding_EvidenceSources rather than duplicated here.

func TestEnvBinding_EvidenceSources(t *testing.T) {
	env := map[string]string{
		"EVIDENCE_QB_POLL_INTERVAL":  "7m",
		"EVIDENCE_QB_ALPHA_BASE_URL": "http://qb:8080",
		"EVIDENCE_QB_ALPHA_USERNAME": "user",
		"EVIDENCE_QB_ALPHA_PASSWORD": "pass",
		"EVIDENCE_SONARR_BASE_URL":   "http://sonarr:8989",
		"EVIDENCE_SONARR_API_KEY":    "key",
		"EVIDENCE_LIVENESS_ENABLED":  "true",
	}
	resolved := resolveSectionFromEnv(t, "evidence", evidence.NewDefaultConfig(), env)
	cfg, ok := resolved.(evidence.Config)
	if !ok {
		t.Fatalf("resolved value is %T, want evidence.Config", resolved)
	}
	if cfg.QBPollInterval != 7*time.Minute {
		t.Errorf("QBPollInterval = %v, want 7m", cfg.QBPollInterval)
	}
	qb := cfg.QBSlots()
	if len(qb) != 1 || qb[0].Name != "qb-alpha" || qb[0].BaseURL != "http://qb:8080" || qb[0].Username != "user" || qb[0].Password != "pass" {
		t.Fatalf("qb slots did not bind: %+v", qb)
	}
	arr := cfg.ArrSlots()
	if len(arr) != 1 || arr[0].Name != "sonarr" || arr[0].BaseURL != "http://sonarr:8989" || arr[0].APIKey != "key" {
		t.Fatalf("arr slots did not bind: %+v", arr)
	}
	if !cfg.Liveness.Enabled {
		t.Error("EVIDENCE_LIVENESS_ENABLED did not bind")
	}
}

func resolveSectionFromEnv(t *testing.T, sectionKey string, defaultValue interface{}, env map[string]string) interface{} {
	t.Helper()
	val := validator.New()
	resolvers := []configresolver.Resolver{configresolver.NewEnv(env)}
	node, err := resolveRootNode(resolvers, val, Spec{Key: sectionKey, DefaultValue: defaultValue})
	if err != nil {
		t.Fatalf("resolveRootNode(%q): %v", sectionKey, err)
	}
	return node.Value
}

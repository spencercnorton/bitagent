package config

import (
	"testing"
	"time"

	"github.com/go-playground/validator/v10"
	"github.com/iancoleman/strcase"

	"github.com/spencercnorton/bitagent/internal/classifier/contentfilter"
	"github.com/spencercnorton/bitagent/internal/classifier/llmmatch"
	"github.com/spencercnorton/bitagent/internal/classifier/llmstage"
	"github.com/spencercnorton/bitagent/internal/config/configresolver"
	"github.com/spencercnorton/bitagent/internal/csamblocklist"
	"github.com/spencercnorton/bitagent/internal/database/postgres"
	"github.com/spencercnorton/bitagent/internal/dhtcrawler"
	"github.com/spencercnorton/bitagent/internal/evidence"
	"github.com/spencercnorton/bitagent/internal/junkpurge"
	"github.com/spencercnorton/bitagent/internal/llmcapture"
	"github.com/spencercnorton/bitagent/internal/logging"
	"github.com/spencercnorton/bitagent/internal/retention"
	"github.com/spencercnorton/bitagent/internal/seeds"
	"github.com/spencercnorton/bitagent/internal/tmdb"
	"github.com/spencercnorton/bitagent/internal/torznab"
	"github.com/spencercnorton/bitagent/internal/ui"
)

// The config env resolver derives env keys from the Go STRUCT FIELD NAME via
// strcase.ToSnake (see resolveStructNode -> fieldKey), NOT from the yaml tag.
// Stacked acronyms tokenize in surprising ways:
//
//	ToSnake("LLMOpenAIAPIKey") == "llm_open_aiapi_key"  (Open + AIAPI)
//	ToSnake("FeedURLs")        == "feed_ur_ls"           (UR + Ls)
//
// Both of those silently fail to match their documented env vars
// (CONTENT_FILTER_LLM_OPENAI_API_KEY / CSAM_BLOCKLIST_FEED_URLS), which
// disabled the contentfilter LLM tier and the CSAM feed opt-in respectively.
// The fix renames the fields to LLMOpenaiApiKey / FeedUrls so ToSnake yields
// the documented snake_case. These tests pin that invariant so the regression
// cannot recur (e.g. someone "correcting" the acronym casing back).

// TestToSnake_StackedAcronymsResolveToDocumentedKeys guards the raw tokenizer
// behavior that the env resolver depends on.
func TestToSnake_StackedAcronymsResolveToDocumentedKeys(t *testing.T) {
	cases := []struct {
		field     string
		wantSnake string
	}{
		{"LLMOpenaiApiKey", "llm_openai_api_key"},
		{"FeedUrls", "feed_urls"},
		// Sanity: the broken spellings must NOT produce the documented key, so
		// this test fails loudly if the fields are ever renamed back.
		{"LLMOpenAIAPIKey", "llm_open_aiapi_key"},
		{"OpenRouterProvider", "open_router_provider"},
		{"OpenrouterProvider", "openrouter_provider"},
		{"FeedURLs", "feed_ur_ls"},
		// DropLossyAudioOnly (renamed from DropMP3Only): the OLD field name had
		// an embedded digit that strcase ALWAYS isolates, so it bound the awkward
		// CONTENT_FILTER_DROP_MP_3_ONLY. The new digit-free name resolves to the
		// clean, intuitive drop_lossy_audio_only. Both pinned: the old spelling so
		// nobody assumes a rename can recover "drop_mp3_only", the new one so the
		// clean binding can't silently regress.
		{"DropMP3Only", "drop_mp_3_only"},
		{"DropLossyAudioOnly", "drop_lossy_audio_only"},
		// LLMApiStyle: the stacked "APIStyle" acronym mis-tokenizes — spelling it
		// LLMAPIStyle yields "llmapi_style" (wrong). LLMApiStyle yields the
		// documented "llm_api_style". Pinned both ways so nobody reverts.
		{"LLMApiStyle", "llm_api_style"},
		{"LLMAPIStyle", "llmapi_style"},
		{"LLMBatchEnabled", "llm_batch_enabled"},
		{"LLMBatchPollInterval", "llm_batch_poll_interval"},
		{"LLMBatchMaxInFlight", "llm_batch_max_in_flight"},
		{"LLMBatchMaxAttempts", "llm_batch_max_attempts"},
		{"LLMBatchCompletionWindow", "llm_batch_completion_window"},
		{"LLMBatchFailureCooldown", "llm_batch_failure_cooldown"},
		{"LLMBatchAmbiguityGrace", "llm_batch_ambiguity_grace"},
		{"LLMBatchFallbackSync", "llm_batch_fallback_sync"},
		{"LLMAllowPaidSync", "llm_allow_paid_sync"},
		// seeds.Config.TrackerUrls: spelled TrackerUrls (not TrackerURLs) so the
		// pool binds from SEEDS_TRACKER_URLS. TrackerURLs would mis-tokenize to
		// tracker_ur_ls (same footgun as FeedURLs) and silently ignore the env.
		{"TrackerUrls", "tracker_urls"},
		{"TrackerURLs", "tracker_ur_ls"},
		{"MinRescrapeAge", "min_rescrape_age"},
		{"MaxHashesPerPacket", "max_hashes_per_packet"},
		{"MaxInputBytes", "max_input_bytes"},
	}
	for _, c := range cases {
		if got := strcase.ToSnake(c.field); got != c.wantSnake {
			t.Errorf("strcase.ToSnake(%q) = %q, want %q", c.field, got, c.wantSnake)
		}
	}
}

func TestEnvBinding_LLMEvaluationCapture(t *testing.T) {
	env := map[string]string{
		"LLM_EVALUATION_CAPTURE_ENABLED":          "true",
		"LLM_EVALUATION_CAPTURE_RETENTION":        "72h",
		"LLM_EVALUATION_CAPTURE_CLEANUP_INTERVAL": "17m",
		"LLM_EVALUATION_CAPTURE_MAX_ROWS":         "1234",
		"LLM_EVALUATION_CAPTURE_MAX_INPUT_BYTES":  "65536",
	}
	resolved := resolveSectionFromEnv(
		t,
		"llm_evaluation_capture",
		llmcapture.NewDefaultConfig(),
		env,
	)
	cfg, ok := resolved.(llmcapture.Config)
	if !ok {
		t.Fatalf("resolved value is %T, want llmcapture.Config", resolved)
	}
	if !cfg.Enabled ||
		cfg.Retention != 72*time.Hour ||
		cfg.CleanupInterval != 17*time.Minute ||
		cfg.MaxRows != 1234 ||
		cfg.MaxInputBytes != 65536 {
		t.Fatalf("LLM evaluation capture env vars did not bind: %+v", cfg)
	}
}

func TestEnvBinding_BoundedMatcherCanary(t *testing.T) {
	env := map[string]string{
		"CLASSIFIER_LLM_MATCH_ENABLED":              "true",
		"CLASSIFIER_LLM_MATCH_ENABLE_LIVE":          "true",
		"CLASSIFIER_LLM_MATCH_ENDPOINT":             "https://openrouter.ai/api/v1/chat/completions",
		"CLASSIFIER_LLM_MATCH_MODEL":                "openai/gpt-5.4-nano-20260317",
		"CLASSIFIER_LLM_MATCH_OPENROUTER_PROVIDER":  "azure/us",
		"CLASSIFIER_LLM_MATCH_DAILY_CALL_LIMIT":     "50",
		"CLASSIFIER_LLM_MATCH_MONTHLY_CALL_LIMIT":   "1500",
		"CLASSIFIER_LLM_MATCH_MAX_REQUEST_BYTES":    "8192",
		"CLASSIFIER_LLM_MATCH_MAX_OUTPUT_TOKENS":    "256",
		"CLASSIFIER_LLM_MATCH_MAX_CONCURRENT_CALLS": "2",
		"CLASSIFIER_LLM_MATCH_REQUIRE_SOURCE_TITLE": "true",
		"CLASSIFIER_LLM_MATCH_MIN_CONFIDENCE":       "0.75",
		"CLASSIFIER_LLM_MATCH_TIMEOUT":              "20s",
	}
	resolved := resolveSectionFromEnv(t, "classifier_llm_match", llmmatch.NewDefaultConfig(), env)
	cfg, ok := resolved.(llmmatch.Config)
	if !ok {
		t.Fatalf("resolved value is %T, want llmmatch.Config", resolved)
	}
	if !cfg.Enabled || !cfg.EnableLive || !cfg.RequireSourceTitle ||
		cfg.Endpoint != env["CLASSIFIER_LLM_MATCH_ENDPOINT"] ||
		cfg.Model != env["CLASSIFIER_LLM_MATCH_MODEL"] || cfg.OpenrouterProvider != "azure/us" ||
		cfg.DailyCallLimit != 50 || cfg.MonthlyCallLimit != 1500 ||
		cfg.MaxRequestBytes != 8192 || cfg.MaxOutputTokens != 256 ||
		cfg.MaxConcurrentCalls != 2 || cfg.MinConfidence != 0.75 || cfg.Timeout != 20*time.Second {
		t.Fatalf("bounded matcher canary env vars did not bind")
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("resolved canary must validate: %v", err)
	}
}

func TestEnvBinding_BoundedTypeShadow(t *testing.T) {
	env := map[string]string{
		"CLASSIFIER_LLM_ENABLED":              "true",
		"CLASSIFIER_LLM_ENABLE_LIVE":          "false",
		"CLASSIFIER_LLM_API_KEY":              "test",
		"CLASSIFIER_LLM_ENDPOINT":             "https://openrouter.ai/api/v1/chat/completions",
		"CLASSIFIER_LLM_MODEL":                "openai/gpt-5.4-nano-20260317",
		"CLASSIFIER_LLM_OPENROUTER_PROVIDER":  "azure/us",
		"CLASSIFIER_LLM_DAILY_CALL_LIMIT":     "50",
		"CLASSIFIER_LLM_MONTHLY_CALL_LIMIT":   "1500",
		"CLASSIFIER_LLM_MAX_REQUEST_BYTES":    "8192",
		"CLASSIFIER_LLM_MAX_OUTPUT_TOKENS":    "64",
		"CLASSIFIER_LLM_MAX_CONCURRENT_CALLS": "1",
		"CLASSIFIER_LLM_MIN_CONFIDENCE":       "0.75",
		"CLASSIFIER_LLM_TIMEOUT":              "20s",
	}
	resolved := resolveSectionFromEnv(t, "classifier_llm", llmstage.NewDefaultConfig(), env)
	cfg, ok := resolved.(llmstage.Config)
	if !ok {
		t.Fatalf("resolved value is %T, want llmstage.Config", resolved)
	}
	if !cfg.Enabled || cfg.EnableLive ||
		cfg.Endpoint != env["CLASSIFIER_LLM_ENDPOINT"] ||
		cfg.Model != env["CLASSIFIER_LLM_MODEL"] || cfg.OpenrouterProvider != "azure/us" ||
		cfg.DailyCallLimit != 50 || cfg.MonthlyCallLimit != 1500 ||
		cfg.MaxRequestBytes != 8192 || cfg.MaxOutputTokens != 64 ||
		cfg.MaxConcurrentCalls != 1 || cfg.MinConfidence != 0.75 || cfg.Timeout != 20*time.Second {
		t.Fatalf("bounded type shadow env vars did not bind: %+v", cfg)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("resolved type shadow must validate: %v", err)
	}
}

// TestEnvBinding_JunkpurgeLLMBatch proves every provider-Batch rollout flag
// binds through the real resolver. These names combine two acronyms and are
// easy to silently mis-tokenize.
func TestEnvBinding_JunkpurgeLLMBatch(t *testing.T) {
	env := map[string]string{
		"JUNKPURGE_LLM_BATCH_ENABLED":                 "true",
		"JUNKPURGE_LLM_BATCH_POLL_INTERVAL":           "2m",
		"JUNKPURGE_LLM_BATCH_MAX_IN_FLIGHT":           "7",
		"JUNKPURGE_LLM_BATCH_MAX_ATTEMPTS":            "4",
		"JUNKPURGE_LLM_BATCH_COMPLETION_WINDOW":       "24h",
		"JUNKPURGE_LLM_BATCH_FAILURE_COOLDOWN":        "6h",
		"JUNKPURGE_LLM_BATCH_AMBIGUITY_GRACE":         "30m",
		"JUNKPURGE_LLM_BATCH_FALLBACK_SYNC":           "false",
		"JUNKPURGE_LLM_ALLOW_PAID_SYNC":               "true",
		"JUNKPURGE_LLM_DAILY_CALL_LIMIT":              "15",
		"JUNKPURGE_LLM_MONTHLY_CALL_LIMIT":            "450",
		"JUNKPURGE_LLM_NAMES_PER_CALL":                "10",
		"JUNKPURGE_LLM_UNAVAILABLE_CONSECUTIVE_LIMIT": "4",
	}
	resolved := resolveSectionFromEnv(t, "junkpurge", junkpurge.NewDefaultConfig(), env)
	cfg, ok := resolved.(junkpurge.Config)
	if !ok {
		t.Fatalf("resolved value is %T, want junkpurge.Config", resolved)
	}
	if !cfg.LLMBatchEnabled || cfg.LLMBatchPollInterval != 2*time.Minute ||
		cfg.LLMBatchMaxInFlight != 7 || cfg.LLMBatchMaxAttempts != 4 ||
		cfg.LLMBatchCompletionWindow != "24h" ||
		cfg.LLMBatchFailureCooldown != 6*time.Hour ||
		cfg.LLMBatchAmbiguityGrace != 30*time.Minute ||
		cfg.LLMBatchFallbackSync || !cfg.LLMAllowPaidSync ||
		cfg.LLMDailyCallLimit != 15 || cfg.LLMMonthlyCallLimit != 450 ||
		cfg.LLMNamesPerCall != 10 ||
		cfg.LLMUnavailableConsecutiveLimit != 4 {
		t.Fatalf("junkpurge Batch env vars did not bind: %+v", cfg)
	}
}

// TestEnvBinding_ContentFilterLLMOpenAIKey resolves the real contentfilter
// Config through the real env resolver and asserts the documented env var
// CONTENT_FILTER_LLM_OPENAI_API_KEY binds to the field. This is the
// end-to-end guard for the original bug (LLM tier silently disabled).
func TestEnvBinding_ContentFilterLLMOpenAIKey(t *testing.T) {
	const wantKey = "sk-test-contentfilter-key"
	env := map[string]string{
		"CONTENT_FILTER_LLM_OPENAI_API_KEY": wantKey,
	}
	resolved := resolveSectionFromEnv(t, "content_filter", contentfilter.NewDefaultConfig(), env)
	cfg, ok := resolved.(contentfilter.Config)
	if !ok {
		t.Fatalf("resolved value is %T, want contentfilter.Config", resolved)
	}
	if cfg.LLMOpenaiApiKey != wantKey {
		t.Fatalf("CONTENT_FILTER_LLM_OPENAI_API_KEY did not bind: "+
			"LLMOpenaiApiKey=%q, want %q", cfg.LLMOpenaiApiKey, wantKey)
	}
}

// TestEnvBinding_CsamBlocklistFeedUrls resolves the real csamblocklist Config
// through the env resolver and asserts CSAM_BLOCKLIST_FEED_URLS binds (the
// documented var used by docs/csam-defense.md and the public docker-compose).
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
func TestEnvBinding_ContentFilterLLMApiStyle(t *testing.T) {
	env := map[string]string{"CONTENT_FILTER_LLM_API_STYLE": "ollama"}
	resolved := resolveSectionFromEnv(t, "content_filter", contentfilter.NewDefaultConfig(), env)
	cfg, ok := resolved.(contentfilter.Config)
	if !ok {
		t.Fatalf("resolved value is %T, want contentfilter.Config", resolved)
	}
	if cfg.LLMApiStyle != "ollama" {
		t.Fatalf("CONTENT_FILTER_LLM_API_STYLE did not bind: LLMApiStyle=%q, want ollama", cfg.LLMApiStyle)
	}
}

func TestEnvBinding_ContentFilterLLMProductionBounds(t *testing.T) {
	env := map[string]string{
		"CONTENT_FILTER_LLM_OPENROUTER_PROVIDER":  "azure/us",
		"CONTENT_FILTER_LLM_DAILY_BUDGET":         "50",
		"CONTENT_FILTER_LLM_MONTHLY_BUDGET":       "1500",
		"CONTENT_FILTER_LLM_MAX_CONCURRENT_CALLS": "1",
		"CONTENT_FILTER_LLM_MAX_REQUEST_BYTES":    "8192",
		"CONTENT_FILTER_LLM_MAX_OUTPUT_TOKENS":    "64",
	}
	resolved := resolveSectionFromEnv(t, "content_filter", contentfilter.NewDefaultConfig(), env)
	cfg, ok := resolved.(contentfilter.Config)
	if !ok {
		t.Fatalf("resolved value is %T, want contentfilter.Config", resolved)
	}
	if cfg.LLMOpenrouterProvider != "azure/us" || cfg.LLMDailyBudget != 50 ||
		cfg.LLMMonthlyBudget != 1500 || cfg.LLMMaxConcurrentCalls != 1 ||
		cfg.LLMMaxRequestBytes != 8192 || cfg.LLMMaxOutputTokens != 64 {
		t.Fatalf("contentfilter production bounds did not bind: %+v", cfg)
	}
}

func TestEnvBinding_OpenAIDataSharingAssertions(t *testing.T) {
	matcher := resolveSectionFromEnv(t, "classifier_llm_match", llmmatch.NewDefaultConfig(), map[string]string{
		"CLASSIFIER_LLM_MATCH_OPENAI_DATA_SHARING": "true",
	}).(llmmatch.Config)
	typeStage := resolveSectionFromEnv(t, "classifier_llm", llmstage.NewDefaultConfig(), map[string]string{
		"CLASSIFIER_LLM_OPENAI_DATA_SHARING": "true",
	}).(llmstage.Config)
	filter := resolveSectionFromEnv(t, "content_filter", contentfilter.NewDefaultConfig(), map[string]string{
		"CONTENT_FILTER_LLM_OPENAI_DATA_SHARING": "true",
	}).(contentfilter.Config)
	junk := resolveSectionFromEnv(t, "junkpurge", junkpurge.NewDefaultConfig(), map[string]string{
		"JUNKPURGE_LLM_OPENAI_DATA_SHARING": "true",
	}).(junkpurge.Config)

	if !matcher.OpenaiDataSharing || !typeStage.OpenaiDataSharing ||
		!filter.LLMOpenaiDataSharing || !junk.LLMOpenaiDataSharing {
		t.Fatalf("OpenAI data-sharing env assertions did not bind: matcher=%v type=%v filter=%v junk=%v",
			matcher.OpenaiDataSharing, typeStage.OpenaiDataSharing,
			filter.LLMOpenaiDataSharing, junk.LLMOpenaiDataSharing)
	}
}

// TestEnvBinding_SeedsTrackerUrls proves the seeds worker's tracker pool binds
// from SEEDS_TRACKER_URLS (comma-separated slice-of-string) and that the scalar
// gates bind too — the whole point of naming the field TrackerUrls rather than
// TrackerURLs. If someone "corrects" the casing, the pool silently falls back to
// the built-in default and this test fails.
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

// TestEnvBinding_ComposeKeys pins every env key that the two shipped compose
// files set on the bitagent service — deploy/docker-compose.yml (the Kleos
// Portainer stack) and examples/docker-compose.public.yml (the public
// quickstart) — exactly as spelled there, against the section each is meant
// to configure.
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
func TestEnvBinding_ComposeKeys(t *testing.T) {
	cases := []struct {
		section  string
		defaults interface{}
		// env holds the key/value pairs verbatim from the compose file
		// (variable references replaced by a representative literal).
		env map[string]string
	}{
		{"postgres", postgres.NewDefaultConfig(), map[string]string{
			"POSTGRES_HOST":     "postgres",
			"POSTGRES_PORT":     "5432",
			"POSTGRES_NAME":     "bitmagnet",
			"POSTGRES_USER":     "bitmagnet",
			"POSTGRES_PASSWORD": "hunter2",
		}},
		{"dht_crawler", dhtcrawler.NewDefaultConfig(), map[string]string{
			"DHT_CRAWLER_SCALING_FACTOR": "10",
		}},
		{"tmdb", tmdb.NewDefaultConfig(), map[string]string{
			"TMDB_API_KEY": "tmdb-key",
		}},
		{"ui", ui.NewDefaultConfig(), map[string]string{
			"UI_ENABLED": "true",
		}},
		{"classifier_llm", llmstage.NewDefaultConfig(), map[string]string{
			"CLASSIFIER_LLM_ENABLED":     "false",
			"CLASSIFIER_LLM_ENABLE_LIVE": "false",
			"CLASSIFIER_LLM_API_KEY":     "sk-test",
			"CLASSIFIER_LLM_MODEL":       "gpt-5.4-nano",
		}},
		{"classifier_llm_match", llmmatch.NewDefaultConfig(), map[string]string{
			"CLASSIFIER_LLM_MATCH_ENABLED":               "false",
			"CLASSIFIER_LLM_MATCH_ENABLE_LIVE":           "false",
			"CLASSIFIER_LLM_MATCH_ENDPOINT":              "http://localhost:11436/v1/chat/completions",
			"CLASSIFIER_LLM_MATCH_MODEL":                 "qwen3.6:35b",
			"CLASSIFIER_LLM_MATCH_API_KEY":               "sk-test",
			"CLASSIFIER_LLM_MATCH_ANIME_REQUIRE_ENGLISH": "true",
			"CLASSIFIER_LLM_MATCH_ANIME_ALLOW_SUB_ONLY":  "true",
		}},
		// Field-level assertions for these live in
		// TestEnvBinding_EvidenceSources; listed here so the no-prefix
		// direction is covered for this section too.
		{"evidence", evidence.NewDefaultConfig(), map[string]string{
			"EVIDENCE_WEBHOOK_SECRET":    "secret",
			"EVIDENCE_ARR_POLL_INTERVAL": "15m",
			"EVIDENCE_QB_POLL_INTERVAL":  "15m",
			"EVIDENCE_SONARR_BASE_URL":   "http://sonarr:8989",
			"EVIDENCE_SONARR_API_KEY":    "key",
			"EVIDENCE_RADARR_BASE_URL":   "http://radarr:7878",
			"EVIDENCE_RADARR_API_KEY":    "key",
			"EVIDENCE_READARR_BASE_URL":  "http://readarr:8787",
			"EVIDENCE_READARR_API_KEY":   "key",
			"EVIDENCE_LIDARR_BASE_URL":   "http://lidarr:8686",
			"EVIDENCE_LIDARR_API_KEY":    "key",
			"EVIDENCE_QB_ALPHA_BASE_URL": "http://qb:8080",
			"EVIDENCE_QB_ALPHA_USERNAME": "user",
			"EVIDENCE_QB_ALPHA_PASSWORD": "pass",
		}},
		{"retention", retention.NewDefaultConfig(), map[string]string{
			"RETENTION_ENABLED":      "false",
			"RETENTION_ENABLE_PURGE": "false",
		}},
		{"log", logging.NewDefaultConfig(), map[string]string{
			"LOG_LEVEL": "info",
		}},
		// Set only by examples/docker-compose.public.yml.
		{"torznab", torznab.NewDefaultConfig(), map[string]string{
			"TORZNAB_API_KEY": "torznab-key",
		}},
		{"csam_blocklist", csamblocklist.NewDefaultConfig(), map[string]string{
			"CSAM_BLOCKLIST_ENABLED":             "true",
			"CSAM_BLOCKLIST_FEED_URLS":           "https://feed.example.invalid/list.txt",
			"CSAM_BLOCKLIST_EXPORT_ENABLED":      "true",
			"CSAM_BLOCKLIST_EXPORT_UPSTREAM_URL": "https://upstream.example.invalid/report",
		}},
		{"content_filter", contentfilter.NewDefaultConfig(), map[string]string{
			"CONTENT_FILTER_ENABLED": "false",
			"CONTENT_FILTER_ENFORCE": "false",
		}},
	}

	for _, c := range cases {
		t.Run(c.section, func(t *testing.T) {
			if got := countEnvBoundLeaves(t, c.section, c.defaults, c.env); got != len(c.env) {
				t.Errorf("section %q: %d of %d documented keys bound; "+
					"a key that binds nothing is silently ignored, not an error",
					c.section, got, len(c.env))
			}

			prefixed := make(map[string]string, len(c.env))
			for k, v := range c.env {
				prefixed["BITMAGNET_"+k] = v
			}

			if got := countEnvBoundLeaves(t, c.section, c.defaults, prefixed); got != 0 {
				t.Errorf("section %q: %d BITMAGNET_-prefixed keys bound, want 0 — "+
					"the resolver applies no prefix", c.section, got)
			}
		})
	}
}

// countEnvBoundLeaves resolves a section and counts the leaf fields whose
// value actually came from the env resolver, rather than from the default.
func countEnvBoundLeaves(t *testing.T, sectionKey string, defaultValue interface{}, env map[string]string) int {
	t.Helper()

	val := validator.New()
	resolvers := []configresolver.Resolver{configresolver.NewEnv(env)}

	node, err := resolveRootNode(resolvers, val, Spec{Key: sectionKey, DefaultValue: defaultValue})
	if err != nil {
		t.Fatalf("resolveRootNode(%q): %v", sectionKey, err)
	}

	var count func(ResolvedNode) int
	count = func(n ResolvedNode) int {
		total := 0
		if !n.IsStruct && n.ResolverKey == "env" {
			total++
		}

		for _, child := range n.ChildMap {
			total += count(child)
		}

		return total
	}

	return count(node)
}

// resolveSectionFromEnv runs the production env resolution path for a single
// config section and returns the decoded section value.
// TestEnvBinding_EvidenceSources pins the env keys the evidence ingestor
// actually reads. deploy/docker-compose.yml shipped an INSTANCES_0-style
// name for these ("BITMAGNET_EVIDENCE_QB_INSTANCES_0_BASE_URL"), which
// binds to nothing: the resolver derives keys from Go field names and does
// not walk slice-of-struct types, so a deploy using those names gets a
// silently disabled poller. Same failure family as the LLM acronym keys
// above — the config loads, the worker starts, and no evidence ever
// arrives.
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

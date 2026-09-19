package contentfilter

// Config controls the deterministic content-filter layer. Registered
// as the "content_filter" configfx section; env prefix
// CONTENT_FILTER_*.
//
// Default `Enabled=false` is the operationally-safe starting point:
// the filter compiles and is wired into the pipeline but every
// torrent passes through unchanged. Operator flips Enabled+Enforce
// in two stages once the shadow-mode metrics confirm the drop
// counts are sane.
type Config struct {
	// Enabled — turn the filter on. When false, Decide() returns
	// Allow for every torrent without inspecting it; no metrics
	// are emitted. Default false so a fresh deploy is a pure
	// no-op until the operator opts in.
	Enabled bool `yaml:"enabled"`

	// Enforce — flip from shadow to live. With Enforce=false but
	// Enabled=true, the filter still examines every torrent and
	// emits `bitagent_contentfilter_would_drop_total{reason}` so
	// the operator can see the counterfactual; nothing is actually
	// dropped. With Enforce=true, drops take effect.
	Enforce bool `yaml:"enforce"`

	// RequireEnglishLanguage — when true, torrents whose
	// classifier-set language tag is anything other than `en` (or
	// empty/unset, see DropOnUnknownScript for the fallback) are
	// dropped. Default true per the operator's 2026-04-25 spec.
	RequireEnglishLanguage bool `yaml:"require_english_language"`

	// DropNonLatinScript — when true, torrents whose title
	// contains characters from a non-Latin script (Cyrillic,
	// CJK, Arabic, Hebrew, Devanagari, Thai) are dropped. This
	// catches the ~40% of torrents the TMDB classifier didn't
	// language-tag — the classifier's confidence on a Russian
	// title with no TMDB match is low, so the language column
	// stays empty; script detection handles those.
	DropNonLatinScript bool `yaml:"drop_non_latin_script"`

	// DropLossyAudioOnly — when true, music torrents whose audio files
	// are exclusively lossy mp3 are dropped. Lossless formats (flac,
	// alac, m4a, wav, ape, opus) and mixed torrents (mp3 +
	// lossless) are kept. Default true.
	//
	// Renamed from DropMP3Only (env CONTENT_FILTER_DROP_MP_3_ONLY) to fix
	// the strcase.ToSnake digit-split footgun: ToSnake ALWAYS isolates an
	// embedded digit, so "DropMP3Only" bound to the awkward
	// CONTENT_FILTER_DROP_MP_3_ONLY (note MP_3), and the intuitive
	// CONTENT_FILTER_DROP_MP3_ONLY silently did NOT bind. The current field
	// name has no embedded digit — ToSnake("DropLossyAudioOnly") ==
	// "drop_lossy_audio_only" — so the clean, intuitive env var
	// CONTENT_FILTER_DROP_LOSSY_AUDIO_ONLY binds it.
	//
	// Backward-compat: provideFilter (contentfilterfx) still reads the
	// legacy CONTENT_FILTER_DROP_MP_3_ONLY env var when the new one is
	// unset, logging a one-time deprecation WARN, so a running deploy that
	// only sets the old name keeps working until the compose is updated.
	DropLossyAudioOnly bool `yaml:"drop_lossy_audio_only"`

	// DropNSFW — drop torrents whose title matches the NSFW
	// keyword list OR whose CEL-classified content_type indicates
	// adult. Default true.
	DropNSFW bool `yaml:"drop_nsfw"`

	// DropForeignAudio — when true, drop a release whose name advertises a
	// non-English audio or subtitle track and no English one
	// ("...WEBRip.Dublado.mkv", "...MULTi.TRUEFRENCH...", "[Lektor PL]").
	//
	// This is the only rule that reads the language of the RELEASE.
	// DropNonLatinScript reads the title's alphabet and RequireEnglishLanguage
	// reads TMDB's tag, which is the language of the WORK — so a Portuguese
	// dub of an English film arrives tagged ["en"] and clears both. A census
	// of live production found 18,363 matched movie/TV rows in exactly that
	// state, 1,502 of them above 10 seeders and therefore at the top of the
	// Torznab result the arr stack grabs. Env: CONTENT_FILTER_DROP_FOREIGN_AUDIO.
	DropForeignAudio bool `yaml:"drop_foreign_audio"`

	// AnimeAware — when true, releases the deterministic anime detector
	// recognises (known fansub-group bracket, romaji season markers,
	// the "[Group] Title - NNN" absolute-episode shape, English-track
	// markers) are carved OUT of the destructive drops that otherwise
	// delete watchable anime: the non-Latin-script drop (a single CJK
	// glyph in a native-title anime), the non-English-language drop (TMDB
	// tags anime `ja`), and — for a KNOWN fansub group only, which is
	// disjoint from adult studios so porn is never rescued — the NSFW
	// keyword drop (rating tokens like "18+"/"r18" collide with ecchi/
	// seinen). Detected anime also bypasses the LLM English-language tier
	// (anime is kept, not classified). Default true: the drops it guards
	// were the single largest source of anime data-loss at ingest. Set
	// CONTENT_FILTER_ANIME_AWARE=false to restore the pre-carve behaviour.
	// The explicit NSFW content_type tier and the blocked-extension/
	// mp3-only checks are never carved.
	AnimeAware bool `yaml:"anime_aware"`

	// BlockedExtensions — torrents whose primary file extension
	// matches anything in this list are dropped. Default covers
	// software / archive containers per the operator's spec.
	// Lowercase, no leading dot.
	BlockedExtensions []string `yaml:"blocked_extensions"`

	// BlockedContentTypes — torrents whose classifier-set
	// content_type matches anything here are dropped. Default
	// empty; operator can add e.g. "ebook" or "audiobook" if
	// they decide they don't want those.
	BlockedContentTypes []string `yaml:"blocked_content_types"`

	// AllowedLanguages — when RequireEnglishLanguage=true, the
	// classifier-set language tag must be in this set OR empty/
	// unset. Default `["en"]`. Empty/unset is handled separately
	// by DropNonLatinScript.
	AllowedLanguages []string `yaml:"allowed_languages"`

	// LLMEnabled — turn on the LLM tier (Phase 2). When false,
	// the deterministic ladder is the entire decision; the
	// residual (Latin script, no language tag) flows through to
	// the persist queue. Default false because the LLM path
	// requires a configured API key + a thoughtful budget cap;
	// enabling cold could surprise the operator with cost.
	LLMEnabled bool `yaml:"llm_enabled"`

	// LLMOpenaiApiKey — bearer token for api.openai.com. Empty
	// disables the LLM tier even if LLMEnabled=true (safe-default).
	//
	// NOTE on the field-name spelling (LLMOpenaiApiKey, not
	// LLMOpenAIAPIKey): the config env resolver derives the env key
	// from the Go FIELD NAME via strcase.ToSnake (see
	// internal/config/config.go), NOT from the yaml tag. The triple
	// acronym "OpenAIAPI" tokenizes as Open+AIAPI, yielding
	// "llm_open_aiapi_key" → CONTENT_FILTER_LLM_OPEN_AIAPI_KEY, which
	// does NOT match the documented CONTENT_FILTER_LLM_OPENAI_API_KEY.
	// "LLMOpenaiApiKey" tokenizes as LLM+Openai+Api+Key →
	// "llm_openai_api_key" → CONTENT_FILTER_LLM_OPENAI_API_KEY (the
	// documented name). Do not "correct" the casing back to
	// LLMOpenAIAPIKey — it silently breaks the env binding.
	LLMOpenaiApiKey string `yaml:"llm_openai_api_key"`

	// LLMModel — model identifier. Default "gpt-5.4-nano" per the
	// operator's spec ("small and highly accurate"). Self-hosted
	// model is configured via LLMBaseURL.
	LLMModel string `yaml:"llm_model"`

	// LLMBaseURL — override for OpenAI-compatible endpoints
	// (self-hosted, Ollama, vLLM). Empty defaults to OpenAI.
	// The path /responses is appended automatically.
	LLMBaseURL string `yaml:"llm_base_url"`

	// LLMOpenrouterProvider pins OpenRouter to one exact provider. When set,
	// the runtime accepts only the canonical OpenRouter base URL and sends a
	// fail-closed provider policy (no fallback, no data collection, ZDR).
	// The field spelling intentionally binds CONTENT_FILTER_LLM_OPENROUTER_PROVIDER.
	LLMOpenrouterProvider string `yaml:"llm_openrouter_provider"`

	// LLMOpenaiDataSharing selects the strict direct-OpenAI incentive route.
	// The env spelling is CONTENT_FILTER_LLM_OPENAI_DATA_SHARING. It is an
	// operator assertion; provider usage receipts remain the billing proof.
	LLMOpenaiDataSharing bool `yaml:"llm_openai_data_sharing"`

	// LLMPromptVersion — bumped when the prompt changes. Cache
	// keys include this so a prompt rewrite atomically invalidates
	// every cached entry. Default "v1".
	LLMPromptVersion string `yaml:"llm_prompt_version"`

	// LLMTimeout — per-call timeout. Anything slower than this
	// just gets a "keep" decision and counts toward the budget.
	// Default 8 seconds.
	LLMTimeout string `yaml:"llm_timeout"`

	// LLMDailyBudget / LLMMonthlyBudget cap billed dispatches in PostgreSQL,
	// so restarts and replicas cannot reset or race the allowance. When the
	// budget is exhausted, residual cases default to "keep" and
	// `bitagent_contentfilter_llm_budget_exhausted_total` increments.
	// Zero disables calls. Production does not support an unlimited sentinel:
	// every provider dispatch must have both a daily and monthly ceiling.
	LLMDailyBudget   int `yaml:"llm_daily_budget"`
	LLMMonthlyBudget int `yaml:"llm_monthly_budget"`

	// LLMMaxConcurrentCalls bounds simultaneous provider work. Request and
	// output limits cap both accidental payload growth and worst-case spend.
	LLMMaxConcurrentCalls int `yaml:"llm_max_concurrent_calls"`
	LLMMaxRequestBytes    int `yaml:"llm_max_request_bytes"`
	LLMMaxOutputTokens    int `yaml:"llm_max_output_tokens"`

	// LLMCacheMaxEntries / LLMCacheTTL — sha256-keyed LRU bounds.
	LLMCacheMaxEntries int    `yaml:"llm_cache_max_entries"`
	LLMCacheTTL        string `yaml:"llm_cache_ttl"`

	// LLMMinConfidenceForDrop — only drop a torrent when the LLM
	// reports IsEnglish=false AND confidence >= this threshold.
	// Default 0.85 — conservative; we'd rather keep an ambiguous
	// title than wrongly drop one. Below this confidence, the
	// torrent is kept regardless of the LLM's verdict.
	LLMMinConfidenceForDrop float64 `yaml:"llm_min_confidence_for_drop"`

	// LLMRuleMinerThreshold — number of LLM verdicts with the
	// same (reason, is_english) tuple within the sliding window
	// before the miner emits a "candidate rule" notification.
	// Default 100 (per the operator's spec: "implement rules
	// instead of running hundreds of thousands through an llm").
	LLMRuleMinerThreshold int    `yaml:"llm_rule_miner_threshold"`
	LLMRuleMinerWindow    string `yaml:"llm_rule_miner_window"`

	// LLMApiStyle selects the HTTP API the client speaks:
	//   "responses" — OpenAI Responses API (POST /responses); default
	//                 for api.openai.com.
	//   "chat"      — OpenAI chat-completions (POST /chat/completions);
	//                 generic shape for self-hosted vLLM / LocalAI.
	//   "ollama"    — Ollama NATIVE chat (POST <host>/api/chat) with
	//                 think:false. REQUIRED for an Ollama-hosted
	//                 reasoning model (qwen3): the OpenAI-compat endpoint
	//                 cannot disable its chain-of-thought, so it burns
	//                 ~2min/call there. "/v1" is stripped from the base
	//                 URL for the native path.
	// Empty = auto: "responses" for the OpenAI default base URL, "chat"
	// for any overridden base URL. "ollama" must be set explicitly.
	//
	// NOTE on the field name (LLMApiStyle, not LLMAPIStyle): the env
	// resolver derives the env key from the field name via strcase.ToSnake,
	// NOT the yaml tag. ToSnake("LLMAPIStyle")="llmapi_style" (the stacked
	// "APIStyle" acronym mis-tokenizes) → would have bound the WRONG var.
	// "LLMApiStyle" → "llm_api_style" → the documented
	// CONTENT_FILTER_LLM_API_STYLE. Do not "correct" the casing back.
	LLMApiStyle string `yaml:"llm_api_style"`

	// LLMDeferOnUnavailable — when true, a residual torrent whose LLM
	// classification can't complete because the endpoint is UNREACHABLE
	// (connection refused / timeout / HTTP 5xx / 429) is DEFERRED —
	// re-queued for a later retry — instead of kept. Prevents
	// foreign-language leakage when a self-hosted LLM is offline, at
	// the cost of queue growth during the outage (the backlog drains
	// when the endpoint returns). Non-availability errors (a 4xx, or an
	// unparseable response) still fall through to "keep" so a
	// permanently-bad response can't defer-loop forever. Default false
	// (preserve fail-open-keep).
	LLMDeferOnUnavailable bool `yaml:"llm_defer_on_unavailable"`
}

// NewDefaultConfig returns a config that's safe to ship enabled in
// shadow mode (every drop is counted but nothing is actually
// dropped). The defaults match the operator's 2026-04-25 spec.
func NewDefaultConfig() Config {
	return Config{
		Enabled: false,
		Enforce: false,

		RequireEnglishLanguage: true,
		DropNonLatinScript:     true,
		DropForeignAudio:       true,
		DropLossyAudioOnly:     true,
		DropNSFW:               true,
		AnimeAware:             true,

		BlockedExtensions: []string{
			// "ts" removed: MPEG Transport Stream is a legitimate video
			// container (TV rips, HDR BluRay) and appears as a primary
			// extension on real content — its 2,150+ drops were FPs.
			"iso", "zip", "rar", "exe", "dmg", "msi", "pkg", "deb",
		},
		BlockedContentTypes: []string{},
		AllowedLanguages:    []string{"en"},

		// LLM tier — off by default; needs an API key + an explicit
		// flip from the operator. Defaults exist so a "just turn it
		// on" deploy is sensible.
		LLMEnabled:              false,
		LLMOpenaiApiKey:         "",
		LLMModel:                "gpt-5.4-nano",
		LLMBaseURL:              "",
		LLMPromptVersion:        "v2", // v2: anime-romaji KEEP rule (bump invalidates stale drops)
		LLMTimeout:              "8s",
		LLMDailyBudget:          50,
		LLMMonthlyBudget:        1500,
		LLMMaxConcurrentCalls:   1,
		LLMMaxRequestBytes:      8192,
		LLMMaxOutputTokens:      64,
		LLMCacheMaxEntries:      50000,
		LLMCacheTTL:             "168h", // 7 days
		LLMMinConfidenceForDrop: 0.85,
		LLMRuleMinerThreshold:   100,
		LLMRuleMinerWindow:      "168h", // 7 days
	}
}

// DropReason enumerates why a torrent was dropped. Stable strings
// for `bitagent_contentfilter_drop_total{reason="..."}` labels.
// Adding a new reason: extend this enum + map in String() + bump
// the test that pins the label set.
type DropReason int

const (
	ReasonNone DropReason = iota
	ReasonBlockedExtension
	ReasonBlockedContentType
	ReasonNonEnglishLanguage
	ReasonNonLatinScript
	ReasonMP3Only
	ReasonNSFWKeyword
	ReasonNSFWContentType
	ReasonLLMNonEnglish // verdict: IsEnglish=false above MinConfidence
	ReasonForeignAudio  // release names a non-English track, no English one
)

func (r DropReason) String() string {
	switch r {
	case ReasonBlockedExtension:
		return "blocked_extension"
	case ReasonBlockedContentType:
		return "blocked_content_type"
	case ReasonNonEnglishLanguage:
		return "non_english_language"
	case ReasonNonLatinScript:
		return "non_latin_script"
	case ReasonMP3Only:
		return "mp3_only"
	case ReasonNSFWKeyword:
		return "nsfw_keyword"
	case ReasonNSFWContentType:
		return "nsfw_content_type"
	case ReasonLLMNonEnglish:
		return "llm_non_english"
	case ReasonForeignAudio:
		return "foreign_audio"
	default:
		return "none"
	}
}

package classifier

type Config struct {
	Workflow   string
	Keywords   map[string][]string
	Extensions map[string][]string
	Flags      map[string]any
	DeleteXxx  bool
	// DeleteContentTypes lists content types the classifier deletes at
	// classify time instead of persisting (feeds the workflow flag
	// delete_content_types; deleted hashes are also blocked so the crawler
	// skips them on re-announce). BitAgent's catalog is TV + movies only:
	// set to the out-of-scope types (music, ebook, audiobook, comic, game,
	// software). Invalid type names fail workflow compilation at startup.
	// Comma-separated env: CLASSIFIER_DELETE_CONTENT_TYPES. Empty (the
	// default) leaves behavior unchanged.
	DeleteContentTypes []string
	Concurrency        int
	// AltTitleMatch lets local content search match torrents against stored
	// alternative/translated titles (content_attributes alt_title:* rows),
	// not just Title/OriginalTitle. Off by default until precision is
	// measured (env CLASSIFIER_ALT_TITLE_MATCH).
	AltTitleMatch bool
	// FuzzyMatchEnabled activates the deterministic fuzzy-matching overhaul:
	//   (A) title normalisation (roman→arabic, article stripping, &→and, punct),
	//   (B) token-set-ratio ≥ 0.90 OR length-scaled Levenshtein with a
	//       first-token precision gate,
	//   (C) year ±1 retrieval window (local DB) / dropped TMDB year param with
	//       year-proximity penalty.
	// When false (the default) behavior is byte-identical to before. Flip to
	// true via env CLASSIFIER_FUZZY_MATCH_ENABLED for A/B shadow measurement.
	FuzzyMatchEnabled bool
	// SingleEpisodeMaxBytes is the size ceiling used by the season-pack
	// backstop in parse_video_content. When a torrent's total size is at or
	// below this value AND the parsed episode map is season-only (a season key
	// with no episode numbers), the release is treated as a single episode
	// rather than a complete-season pack and its episode map is cleared.
	// Genuine season packs are well above 4 GiB; single episodes rarely
	// exceed 2–3 GiB.  Set to 0 to disable the backstop.
	// Configured via env CLASSIFIER_SINGLE_EPISODE_MAX_BYTES (default 4 GiB).
	SingleEpisodeMaxBytes int64
	// ParseNoiseV2 enables second-generation name-noise stripping and the
	// leading-year rescue in the video parser (roadmap WI2.3): fullwidth CJK
	// site-tag spans, www.-prefixed site prefixes on any TLD, extra torrent
	// TLDs, and adopting a leading bare year as the release year. Off by
	// default — byte-identical parsing until the eval harness gates it
	// (shadow n>=500, binomial LB>=0.99). Env: CLASSIFIER_PARSE_NOISE_V_2
	// (NOT ...V2 — strcase.ToSnake splits the trailing digit; verify with
	// `bitagent config show`).
	ParseNoiseV2 bool
	// DeleteAuditSample records the torrent name in the verdict ledger's
	// classifier_delete evidence for a deterministic 1-in-128 sample of
	// deletes, excluding CSAM banned-keyword matches and private torrents.
	//
	// Off by default, and the default is the point: the ledger's evidence
	// builder was written to carry NO title or file path, and this flag
	// deliberately narrows that invariant. Turning it on is an operator
	// privacy decision, not a code decision.
	//
	// Why it exists: classifier_delete is the largest destructive surface in
	// the estate (~160k/day, every hash blacklisted so it cannot be
	// re-acquired), and the ledger currently records that a rule fired but
	// never what it destroyed — so its false-delete rate is unmeasurable in
	// principle. One sampled name per 128 deletes (~1,250/day) is enough to
	// adjudicate a few hundred rows and put a Wilson bound on it.
	//
	// File paths are never recorded, on or off. Env:
	// CLASSIFIER_DELETE_AUDIT_SAMPLE.
	DeleteAuditSample bool
	// DeleteAuditSampleMax caps how many names one process records before
	// capture stops on its own. It is the enforceable half of the flag
	// above: without it the stop condition is a future manual config edit,
	// and this estate has already shipped one safety argument that rested on
	// a review nobody was staffed to perform (junkpurge's 30-day quarantine
	// window, which recorded zero operator reviews).
	//
	// 5,000 is ~4 days at the measured ~1,250 names/day and roughly 16x the
	// n=300 an adjudication needs. A non-positive value disables capture
	// rather than meaning unlimited, so a mis-set zero fails closed.
	// Env: CLASSIFIER_DELETE_AUDIT_SAMPLE_MAX.
	DeleteAuditSampleMax int
}

func NewDefaultConfig() Config {
	return Config{
		Workflow:              "default",
		Concurrency:           10,
		DeleteAuditSampleMax:  5000,
		SingleEpisodeMaxBytes: 4 * 1024 * 1024 * 1024, // 4 GiB
	}
}

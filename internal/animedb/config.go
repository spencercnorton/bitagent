// Package animedb is the deterministic anime-metadata backbone. It turns messy
// anime release-name titles (romaji, kanji, English, synonyms, fan
// abbreviations) into a canonical TMDB id + content type WITHOUT relying on the
// LLM's world knowledge or hardcoded per-title switches.
//
// It joins two public, offline-cacheable data sources:
//
//   - Anime-Lists anime-list-full.xml — per AniDB entry, the direct TMDB id
//     (tmdbtv for series, tmdbid for films) plus the TVDB/absolute-episode
//     mapping. This is the anidbid -> TMDB spine.
//   - AniDB anime-titles.dat — per AniDB entry, every alias (romaji `x-jat`,
//     kanji `ja`, English `en`, synonyms, short titles). This is the alias set.
//
// Joined on the AniDB id, the result is "every alias -> TMDB id + type", which
// is persisted to the anime_titles table (a pure derived cache) and served from
// an in-memory Resolver. The classifier consults the Resolver on its local
// candidate path: a resolved alias attaches its vetted TMDB id directly,
// bypassing the romaji-hostile TMDB search entirely.
//
// A small baked-in seed set (see seed.go) guarantees the highest-value aliases
// resolve even before the first refresh or fully offline, so the Resolver is a
// like-for-like (and then some) replacement for the retired matcher switches.
package animedb

import "time"

// Config controls the anime-titles refresh worker and the data sources it
// pulls. Registered under section key "anime_titles"; env prefix ANIME_TITLES_*.
//
// Field-name footgun (shared with seeds.Config): the env resolver derives keys
// via strcase.ToSnake(field.Name) and ignores yaml tags, so stacked acronyms
// mis-tokenize. Fields here are named so ToSnake yields the intended key —
// AnimeListUrl (not AnimeListURL, which becomes anime_list_ur_l), HttpTimeout
// (not HTTPTimeout). See config_test.go, which pins every binding.
type Config struct {
	// Enabled turns the scheduled refresh worker on. When false no cycles run,
	// but a resolver still serves the baked seed set plus whatever the last
	// refresh persisted to anime_titles. Env: ANIME_TITLES_ENABLED.
	Enabled bool `yaml:"enabled"`

	// EnableWrite gates persistence. When false the worker downloads, parses and
	// builds the alias set and reports counts but writes nothing (dry-run) — flip
	// only after reviewing the coverage counts. Env: ANIME_TITLES_ENABLE_WRITE.
	EnableWrite bool `yaml:"enable_write"`

	// Interval between refresh cycles. The upstream files change at most daily
	// and AniDB asks that its dump be fetched no more than once per day, so keep
	// this at 24h or longer. Env: ANIME_TITLES_INTERVAL.
	Interval time.Duration `yaml:"interval"`

	// MinRefreshAge skips a cycle whose most recent persisted row is younger than
	// this, so a restart storm cannot hammer the upstream hosts. Env:
	// ANIME_TITLES_MIN_REFRESH_AGE.
	MinRefreshAge time.Duration `yaml:"min_refresh_age"`

	// AnimeListUrl is the Anime-Lists anime-list-full.xml (anidbid -> TMDB id +
	// mapping). Env: ANIME_TITLES_ANIME_LIST_URL.
	AnimeListUrl string `yaml:"anime_list_url"`

	// AnidbTitlesUrl is the AniDB anime-titles.dat[.gz] alias dump. A .gz suffix
	// is transparently decompressed. Env: ANIME_TITLES_ANIDB_TITLES_URL.
	AnidbTitlesUrl string `yaml:"anidb_titles_url"`

	// CacheDir is where downloaded source files are cached for conditional GET
	// (If-Modified-Since) and offline fallback. Empty uses a bitagent-animedb
	// subdirectory of the OS temp dir. Env: ANIME_TITLES_CACHE_DIR.
	CacheDir string `yaml:"cache_dir"`

	// UserAgent identifies this client to the data hosts (AniDB requires a
	// descriptive UA). Env: ANIME_TITLES_USER_AGENT.
	UserAgent string `yaml:"user_agent"`

	// HttpTimeout bounds a single source download. Env: ANIME_TITLES_HTTP_TIMEOUT.
	HttpTimeout time.Duration `yaml:"http_timeout"`

	// Languages is the AniDB language whitelist for aliases kept in the table.
	// Empty keeps all languages (noisy — dozens of localizations per title).
	// Default keeps romaji (x-jat), English (en) and Japanese (ja). Env:
	// ANIME_TITLES_LANGUAGES.
	Languages []string `yaml:"languages"`
}

// NewDefaultConfig returns conservative, opt-in defaults. The worker is off and
// in dry-run until an operator flips Enabled + EnableWrite after reviewing the
// build/coverage counts. The Resolver still serves the seed set regardless.
func NewDefaultConfig() Config {
	return Config{
		Enabled:        false,
		EnableWrite:    false,
		Interval:       24 * time.Hour,
		MinRefreshAge:  20 * time.Hour,
		AnimeListUrl:   "https://raw.githubusercontent.com/Anime-Lists/anime-lists/master/anime-list-full.xml",
		AnidbTitlesUrl: "https://anidb.net/api/anime-titles.dat.gz",
		CacheDir:       "",
		UserAgent:      "bitagent-anime-backbone/1.0 (+https://github.com/spencercnorton/bitagent)",
		HttpTimeout:    2 * time.Minute,
		Languages:      []string{"x-jat", "en", "ja"},
	}
}

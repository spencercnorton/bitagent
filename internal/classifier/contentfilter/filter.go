package contentfilter

import (
	"github.com/spencercnorton/bitagent/internal/anime"
	"github.com/spencercnorton/bitagent/internal/namepolicy"
	"strings"
)

type Decision struct {
	Allow, WouldDrop, WouldReview, Review, Defer bool
	Reason                                       DropReason
	BlockedExt                                   string
}
type Input struct {
	// Private is the authoritative private flag from the torrent's metainfo.
	// Deterministic checks still run; optional-service privacy is gated separately.
	Private bool

	// Title is the human-readable torrent name. Used for NSFW
	// keyword matching and non-Latin script detection.
	Title string

	// PrimaryExtension is the lowercase file extension of the
	// torrent's "main" file (largest media-ish file by size, or
	// the only file for single-file torrents). No leading dot.
	// Empty string is allowed — it just means "unknown ext."
	PrimaryExtension string

	// AllExtensions is the set of file extensions in the torrent
	// (lowercase, no leading dot, deduped). For
	// content_type="music", this lets us differentiate "mp3 only"
	// from "lossless" or "mixed."
	AllExtensions []string

	// ContentType is the CEL classifier's emitted type, e.g.
	// "movie", "tv_show", "music", "ebook", "audiobook", "xxx".
	// Empty string when the classifier didn't match.
	ContentType string

	// Languages is the classifier-set language tag set for the
	// torrent's content. Examples: ["en"], ["ru"], [], nil. The
	// classifier populates these from TMDB lookup; ~40% of
	// torrents have empty/nil languages here.
	Languages []string
}

type Filter struct {
	cfg        Config
	namePolicy *namepolicy.Policy
}

func New(cfg Config) *Filter                         { return &Filter{cfg: cfg} }
func (f *Filter) Enabled() bool                      { return f.cfg.Enabled }
func (f *Filter) SetNamePolicy(p *namepolicy.Policy) { f.namePolicy = p }
func (f *Filter) Decide(in Input) Decision           { return f.DecideDeterministic(in) }
func (f *Filter) DecideDeterministic(in Input) Decision {
	if !f.cfg.Enabled {
		return Decision{Allow: true}
	}
	reason, ext := f.evaluate(in)
	return Decision{Allow: reason == ReasonNone || !f.cfg.Enforce, WouldDrop: reason != ReasonNone, Reason: reason, BlockedExt: ext}
}
func (f *Filter) evaluate(in Input) (DropReason, string) {
	// Anime carve-out (computed lazily, at most once, and only when a
	// destructive step below would otherwise fire). When AnimeAware is on,
	// a recognised anime release is exempted from the script / language /
	// NSFW-keyword drops that were deleting watchable anime — the single
	// largest anime data-loss source at ingest. See Config.AnimeAware.
	var (
		animeOnce bool
		animeSig  anime.Signals
	)
	detectAnime := func() anime.Signals {
		if !animeOnce {
			animeSig = anime.Detect(in.Title)
			animeOnce = true
		}
		return animeSig
	}

	// 1. Blocked extension. Cheapest check; lots of zero-content
	// torrents (iso, exe) get caught here so the rest of the
	// ladder doesn't run. Normalise the returned ext so Prometheus
	// label cardinality stays bounded — "ISO" and "iso" must
	// produce the same series, not two.
	if in.PrimaryExtension != "" && extInBlocklist(in.PrimaryExtension, f.cfg.BlockedExtensions) {
		return ReasonBlockedExtension, strings.ToLower(strings.TrimSpace(in.PrimaryExtension))
	}

	// 2. Blocked content_type from operator config (e.g. "ebook").
	if in.ContentType != "" {
		ct := strings.ToLower(strings.TrimSpace(in.ContentType))
		for _, blocked := range f.cfg.BlockedContentTypes {
			if strings.EqualFold(blocked, ct) {
				return ReasonBlockedContentType, ""
			}
		}
	}

	// 3. NSFW content_type. Separate from BlockedContentTypes
	// because NSFW has its own toggle (DropNSFW) and the operator
	// might want to block it without listing every variant.
	if f.cfg.DropNSFW && isNSFWContentType(in.ContentType) {
		return ReasonNSFWContentType, ""
	}

	// 4. NSFW keyword in title. Catches studio-tag-style entries
	// the classifier might miss (e.g. "[brazzers] ..."). A KNOWN anime
	// fansub group carves this out — rating tokens like "18+"/"r18"
	// collide with mainstream ecchi/seinen anime, and the known-group
	// allowlist is disjoint from adult studios so no porn is rescued.
	// Non-anime "[18+]" releases (no fansub group) still drop.
	if f.cfg.DropNSFW && titleMatchesNSFW(in.Title) {
		if !(f.cfg.AnimeAware && detectAnime().IsKnownFansub()) {
			return ReasonNSFWKeyword, ""
		}
	}

	// 5. Non-Latin script in title. Catches the ~40% of torrents
	// with empty languages tag that are obviously non-English by
	// script alone (Cyrillic, CJK, etc.). A recognised anime release
	// (a native-title anime with a fansub-group bracket) is carved out —
	// a single CJK glyph must not delete an English-fansubbed anime.
	if f.cfg.DropNonLatinScript && titleContainsNonLatinScript(in.Title) {
		if !(f.cfg.AnimeAware && detectAnime().IsAnime()) {
			return ReasonNonLatinScript, ""
		}
	}

	// 7. Foreign-audio release. Placed after the script check and BEFORE the
	// language check because the language check is what lets this class
	// through: TMDB reports the language of the work, so a dub of an English
	// film is tagged "en" and cleared. Deliberately NOT anime-carved — a
	// Chinese-subtitled [LoliHouse] release is precisely what the carve-out
	// must not rescue, and a curated English subbing group already counts as
	// English evidence inside ForeignAudioOnly.
	if f.cfg.DropForeignAudio && anime.ForeignAudioOnly(in.Title) {
		return ReasonForeignAudio, ""
	}

	// 6. Language tag check. Only when the classifier actually
	// set a language; an empty/nil set is "unknown, defer to
	// script check above." A recognised anime
	// release is carved out — TMDB tags anime with its ORIGINAL language
	// ("ja"), which is the language of the work, not of this (subbed/
	// dubbed) release; dropping on it deletes exactly the anime matched
	// best.
	if f.cfg.RequireEnglishLanguage && len(in.Languages) > 0 {
		if !anyAllowedLanguage(in.Languages, f.cfg.AllowedLanguages) {
			if !(f.cfg.AnimeAware && detectAnime().IsAnime()) {
				return ReasonNonEnglishLanguage, ""
			}
		}
	}

	// 7. mp3-only music. Music is kept generally, but not the
	// pure-mp3 subset. Mixed (mp3 + lossless) is fine because
	// most operators want lossless tagged with the mp3 also
	// available for mobile playback.
	if f.cfg.DropLossyAudioOnly && strings.EqualFold(in.ContentType, "music") {
		if isPureMP3(in.AllExtensions) {
			return ReasonMP3Only, ""
		}
	}

	return ReasonNone, ""
}

// anyAllowedLanguage returns true iff at least one tag in `langs`
// is in `allowed` (case-insensitive). Empty `allowed` is treated as
// "permit nothing"; the caller's contract is that AllowedLanguages
// is non-empty when RequireEnglishLanguage=true (defaults handle
// this).
func anyAllowedLanguage(langs []string, allowed []string) bool {
	if len(allowed) == 0 {
		return false
	}
	for _, lang := range langs {
		l := strings.ToLower(strings.TrimSpace(lang))
		for _, a := range allowed {
			if strings.EqualFold(a, l) {
				return true
			}
		}
	}
	return false
}

// isPureMP3 reports true iff every extension in `exts` is mp3 OR
// the torrent contains mp3 audio AND no other audio formats. The
// "OR" branch handles single-file torrents (one .mp3); the AND
// branch handles album-style torrents that include cue/log/jpg
// alongside the audio — those non-audio extensions don't count
// against the "pure mp3" determination.
//
// Empty `exts` returns false — we don't have enough info to call
// it pure mp3.
func isPureMP3(exts []string) bool {
	if len(exts) == 0 {
		return false
	}
	hasAnyMP3 := false
	hasOtherAudio := false
	for _, e := range exts {
		if isMP3(e) {
			hasAnyMP3 = true
			continue
		}
		if _, ok := nonMP3MusicExtensions[e]; ok {
			hasOtherAudio = true
		}
	}
	return hasAnyMP3 && !hasOtherAudio
}

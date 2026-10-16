package contentfilter

// Config controls the deterministic content-filter layer. Registered
// as the "content_filter" configfx section; env prefix
// CONTENT_FILTER_*.
//
// Default `Enabled=false` is the operationally-safe starting point:
// the filter compiles and is wired into the pipeline but every
// torrent passes through unchanged. Operator flips Enabled+Enforce
// in two stages once independently reviewed shadow decisions are acceptable.
type Config struct {
	// Enabled — turn the filter on. When false, Decide() returns
	// Allow for every torrent without inspecting it; no metrics
	// are emitted. Default false so a fresh deploy is a pure
	// no-op until the operator opts in.
	Enabled bool `yaml:"enabled"`

	// Enforce applies deterministic drops when Enabled=true.
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
	// seinen). Default true: the drops it guards
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
	}
}

const LLMReviewTag = "llm-language-review" // historical catalogue provenance

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

package contentfilter

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func enforcing() Config {
	c := NewDefaultConfig()
	c.Enabled = true
	c.Enforce = true
	return c
}

func shadowing() Config {
	c := NewDefaultConfig()
	c.Enabled = true
	c.Enforce = false
	return c
}

// --- the disabled / shadow contracts ------------------------------

func TestFilter_DisabledAllowsEverything(t *testing.T) {
	cfg := NewDefaultConfig()
	cfg.Enabled = false
	f := New(cfg)

	for _, in := range []Input{
		{Title: "Some Random Movie 2023", PrimaryExtension: "mkv"},
		{Title: "[XXX] Brazzers studio thing", PrimaryExtension: "mp4"}, // would normally NSFW
		{Title: "Какой-то фильм 2022", PrimaryExtension: "mkv"},         // Cyrillic
		{Title: "movie.iso", PrimaryExtension: "iso"},                   // blocked ext
	} {
		d := f.Decide(in)
		assert.True(t, d.Allow, "disabled filter must allow %q", in.Title)
		assert.False(t, d.WouldDrop)
		assert.Equal(t, ReasonNone, d.Reason)
	}
}

func TestFilter_ShadowModeKeepsButFlagsWouldDrop(t *testing.T) {
	f := New(shadowing())
	d := f.Decide(Input{Title: "Какой-то фильм 2022", PrimaryExtension: "mkv"})

	assert.True(t, d.Allow, "shadow mode must keep allowing")
	assert.True(t, d.WouldDrop, "shadow mode must surface counterfactual")
	assert.Equal(t, ReasonNonLatinScript, d.Reason)
}

func TestFilter_EnabledReportsConfigState(t *testing.T) {
	// Regression guard for Jeeves's medium-confidence finding on
	// callers MUST be able to gate metrics emission on
	// the operator's Enabled flag so a disabled filter is a real
	// no-op (no examined_total / keep_total leakage).
	disabled := New(NewDefaultConfig()) // default Enabled=false
	assert.False(t, disabled.Enabled(), "default config should report Enabled=false")

	enabled := New(enforcing())
	assert.True(t, enabled.Enabled(), "enforcing config should report Enabled=true")

	shadow := New(shadowing())
	assert.True(t, shadow.Enabled(), "shadow config should report Enabled=true")
}

// --- per-reason coverage ------------------------------------------

func TestFilter_BlockedExtensionsAreDropped(t *testing.T) {
	f := New(enforcing())
	// "ts" is intentionally absent — MPEG Transport Stream is a legitimate video
	// container; see TestFilter_TSExtensionIsNotBlocked.
	for _, ext := range []string{"iso", "zip", "rar", "exe", "dmg", "msi", "pkg", "deb"} {
		ext := ext
		t.Run(ext, func(t *testing.T) {
			d := f.Decide(Input{Title: "anything 2024", PrimaryExtension: ext})
			assert.False(t, d.Allow, "ext %q must be dropped", ext)
			assert.Equal(t, ReasonBlockedExtension, d.Reason)
			assert.Equal(t, ext, d.BlockedExt, "BlockedExt must carry the matched extension")
		})
	}
}

func TestFilter_BlockedExtIsLowerCased(t *testing.T) {
	// Prometheus cardinality guard: BlockedExt must be normalised so "ISO"
	// and "iso" don't create two parallel time-series. The matcher already
	// normalises for comparison; this pins that the returned label does too.
	f := New(enforcing())
	for _, raw := range []string{"ISO", "Iso", "iSo", " iso "} {
		raw := raw
		t.Run(raw, func(t *testing.T) {
			d := f.Decide(Input{Title: "x", PrimaryExtension: raw})
			assert.Equal(t, ReasonBlockedExtension, d.Reason)
			assert.Equal(t, "iso", d.BlockedExt, "BlockedExt must be lowercase + trimmed")
		})
	}
}

func TestFilter_TSExtensionIsNotBlocked(t *testing.T) {
	// Regression guard: .ts = MPEG Transport Stream (TV rips, HDR BluRay),
	// not TypeScript source. It was removed from the default blocklist
	// because it caused ~14% false-positive drops of legitimate TV content.
	f := New(enforcing())
	d := f.Decide(Input{Title: "Show.S03E01.2160p.BluRay.ts", PrimaryExtension: "ts"})
	assert.True(t, d.Allow)
	assert.False(t, d.WouldDrop)
	assert.Equal(t, ReasonNone, d.Reason)
	assert.Empty(t, d.BlockedExt)
}

func TestFilter_BlockedExtPopulatedOnDrop(t *testing.T) {
	f := New(enforcing())
	for _, ext := range []string{"iso", "rar", "exe"} {
		ext := ext
		t.Run(ext, func(t *testing.T) {
			d := f.Decide(Input{Title: "file 2024", PrimaryExtension: ext})
			assert.Equal(t, ReasonBlockedExtension, d.Reason)
			assert.Equal(t, ext, d.BlockedExt, "BlockedExt must equal the triggering extension")
		})
	}
}

func TestFilter_BlockedExtEmptyOnOtherReasons(t *testing.T) {
	// BlockedExt is only set for ReasonBlockedExtension; all other reasons leave it empty.
	f := New(enforcing())
	d := f.Decide(Input{Title: "[Brazzers] Some Title 2024", PrimaryExtension: "mp4", ContentType: "movie"})
	assert.Equal(t, ReasonNSFWKeyword, d.Reason)
	assert.Empty(t, d.BlockedExt, "BlockedExt must be empty for non-extension drop reasons")
}

func TestFilter_BlockedExtensionMatchIsCaseInsensitive(t *testing.T) {
	f := New(enforcing())
	d := f.Decide(Input{Title: "x", PrimaryExtension: "ISO"})
	assert.False(t, d.Allow)
	assert.Equal(t, ReasonBlockedExtension, d.Reason)
}

func TestFilter_BlockedContentTypeFromConfig(t *testing.T) {
	cfg := enforcing()
	cfg.BlockedContentTypes = []string{"ebook"}
	f := New(cfg)

	d := f.Decide(Input{Title: "A Programming Book", PrimaryExtension: "epub", ContentType: "ebook"})
	assert.False(t, d.Allow)
	assert.Equal(t, ReasonBlockedContentType, d.Reason)
}

func TestFilter_NSFWContentTypeDropped(t *testing.T) {
	f := New(enforcing())
	for _, ct := range []string{"xxx", "adult", "porn", "XXX", "Adult"} {
		ct := ct
		t.Run(ct, func(t *testing.T) {
			d := f.Decide(Input{Title: "x", PrimaryExtension: "mp4", ContentType: ct})
			assert.False(t, d.Allow)
			assert.Equal(t, ReasonNSFWContentType, d.Reason)
		})
	}
}

func TestFilter_NSFWKeywordTitleDropped(t *testing.T) {
	f := New(enforcing())
	for _, title := range []string{
		"[Brazzers] Some Title 2024",
		"My Favorite Film XXX",
		"OnlyFans leaks 2023 1080p",
		"hentai vol 7",
		"Movie [18+]",
	} {
		title := title
		t.Run(title, func(t *testing.T) {
			d := f.Decide(Input{Title: title, PrimaryExtension: "mp4", ContentType: "movie"})
			assert.False(t, d.Allow, "title %q should match NSFW keyword filter", title)
			assert.Equal(t, ReasonNSFWKeyword, d.Reason)
		})
	}
}

func TestFilter_NSFWKeywordRespectsWordBoundary(t *testing.T) {
	// "Aviato" must NOT match "jav" because of word boundary
	// requirements. This is the key false-positive guard for the
	// short keyword list.
	f := New(enforcing())
	for _, title := range []string{
		"Aviato Inc. 2024", // contains "viato" not "jav"
		"Jurassic Park 1993",
		"Naked Gun 1988",
	} {
		title := title
		t.Run(title, func(t *testing.T) {
			d := f.Decide(Input{Title: title, PrimaryExtension: "mkv", ContentType: "movie"})
			assert.True(t, d.Allow, "title %q should NOT match — false positive on substring", title)
		})
	}
}

func TestFilter_NonLatinScriptDropped(t *testing.T) {
	f := New(enforcing())
	cases := []struct {
		title  string
		script string
	}{
		{"Какой-то фильм 2022", "Cyrillic"},
		{"電影 2023", "Han"},
		{"映画 2024", "Hiragana/Katakana mix"},
		{"영화 2022", "Hangul"},
		{"فيلم 2023", "Arabic"},
		{"סרט 2024", "Hebrew"},
		{"फिल्म 2022", "Devanagari"},
		{"หนัง 2024", "Thai"},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.script, func(t *testing.T) {
			d := f.Decide(Input{Title: tc.title, PrimaryExtension: "mkv", ContentType: "movie"})
			assert.False(t, d.Allow, "title %q (%s) should be dropped", tc.title, tc.script)
			assert.Equal(t, ReasonNonLatinScript, d.Reason)
		})
	}
}

func TestFilter_LatinAccentsAreNotDroppedAsScript(t *testing.T) {
	// French/Spanish/German titles use Latin extensions (é, ü,
	// ñ). The script filter must NOT drop these — only the
	// language-tag filter (a separate check) should.
	f := New(enforcing())
	for _, title := range []string{
		"Amélie 2001",
		"Pan's Labyrinth (El Laberinto del Fauno) 2006",
		"Das Boot 1981",
		"Crøssing 2023", // Norwegian-style ø
	} {
		title := title
		t.Run(title, func(t *testing.T) {
			// Must NOT be dropped by SCRIPT. (May still be dropped
			// by language tag if classifier set non-en, but that's
			// a different check.)
			d := f.Decide(Input{
				Title:            title,
				PrimaryExtension: "mkv",
				ContentType:      "movie",
				// language empty so the script filter is the only
				// gate that could fire here
			})
			assert.True(t, d.Allow, "Latin-extension title %q must not be dropped by script filter", title)
		})
	}
}

func TestFilter_NonEnglishLanguageDropped(t *testing.T) {
	f := New(enforcing())
	cases := []struct {
		langs []string
	}{
		{[]string{"fr"}},
		{[]string{"es"}},
		{[]string{"de"}},
		{[]string{"ru", "en"}}, // mixed but no language equals "en" exactly? both must match — read AnyAllowed.
	}
	// First three cases plain non-English: drop expected. Last
	// case should ALLOW because "en" is in the list.
	for _, tc := range cases[:3] {
		tc := tc
		t.Run(tc.langs[0], func(t *testing.T) {
			d := f.Decide(Input{
				Title:            "Generic Movie 2024",
				PrimaryExtension: "mkv",
				ContentType:      "movie",
				Languages:        tc.langs,
			})
			assert.False(t, d.Allow, "lang %v must be dropped", tc.langs)
			assert.Equal(t, ReasonNonEnglishLanguage, d.Reason)
		})
	}
	t.Run("multilingual-with-en-allowed", func(t *testing.T) {
		d := f.Decide(Input{
			Title: "Generic Movie 2024", PrimaryExtension: "mkv",
			ContentType: "movie", Languages: []string{"ru", "en"},
		})
		assert.True(t, d.Allow, "torrent tagged with multiple languages incl. English must be kept")
	})
}

func TestFilter_EmptyLanguageDoesNotTriggerLanguageDrop(t *testing.T) {
	// A torrent with no language tag and a Latin-script title
	// should pass the language gate (defers to Phase 2 LLM).
	f := New(enforcing())
	d := f.Decide(Input{
		Title:            "Some Title 2024",
		PrimaryExtension: "mkv",
		ContentType:      "movie",
		Languages:        []string{},
	})
	assert.True(t, d.Allow, "empty lang + Latin title must pass — Phase 2 will decide")
}

func TestFilter_MP3OnlyMusicDropped(t *testing.T) {
	f := New(enforcing())
	d := f.Decide(Input{
		Title:            "Some Album 2024",
		PrimaryExtension: "mp3",
		AllExtensions:    []string{"mp3", "jpg", "txt"}, // album art + nfo, no other audio
		ContentType:      "music",
	})
	assert.False(t, d.Allow, "pure-mp3 music must be dropped")
	assert.Equal(t, ReasonMP3Only, d.Reason)
}

func TestFilter_MixedFormatMusicKept(t *testing.T) {
	f := New(enforcing())
	d := f.Decide(Input{
		Title:            "Some Album 2024",
		PrimaryExtension: "flac",
		AllExtensions:    []string{"flac", "mp3", "log", "cue"}, // both formats
		ContentType:      "music",
	})
	assert.True(t, d.Allow, "mixed flac+mp3 music must be kept")
}

func TestFilter_LosslessOnlyMusicKept(t *testing.T) {
	f := New(enforcing())
	d := f.Decide(Input{
		Title:            "Some Album 2024",
		PrimaryExtension: "flac",
		AllExtensions:    []string{"flac", "log", "cue"},
		ContentType:      "music",
	})
	assert.True(t, d.Allow)
}

func TestFilter_MP3OnlyOnlyAppliesToMusic(t *testing.T) {
	// An audiobook chapter that's all mp3 should NOT be dropped
	// by the mp3-only check (which only fires for content_type=
	// "music"). It might be dropped by something else (operator
	// adding "audiobook" to BlockedContentTypes), but not this
	// rule.
	f := New(enforcing())
	d := f.Decide(Input{
		Title:            "Audiobook 2024",
		PrimaryExtension: "mp3",
		AllExtensions:    []string{"mp3"},
		ContentType:      "audiobook",
	})
	assert.True(t, d.Allow, "mp3-only filter must scope to content_type=music")
}

// --- decision precedence -----------------------------------------

func TestFilter_BlockedExtensionFiresBeforeNSFWKeyword(t *testing.T) {
	// Catch ladder ordering: a torrent that's BOTH NSFW-named AND
	// .iso should report ReasonBlockedExtension (the cheaper
	// check) — NOT the NSFW reason.
	f := New(enforcing())
	d := f.Decide(Input{
		Title: "[Brazzers] thing 2024", PrimaryExtension: "iso",
		ContentType: "movie",
	})
	assert.Equal(t, ReasonBlockedExtension, d.Reason,
		"ladder must report blocked-ext before NSFW")
}

// --- defaults guard ----------------------------------------------

func TestNewDefaultConfig_ShipSafe(t *testing.T) {
	c := NewDefaultConfig()
	assert.False(t, c.Enabled, "default Enabled must be false (safe ship)")
	assert.False(t, c.Enforce, "default Enforce must be false (shadow first)")
	assert.True(t, c.RequireEnglishLanguage)
	assert.True(t, c.DropNonLatinScript)
	assert.True(t, c.DropLossyAudioOnly)
	assert.True(t, c.DropNSFW)
	assert.True(t, c.AnimeAware, "default AnimeAware must be true (stop anime data-loss)")
	assert.NotEmpty(t, c.BlockedExtensions)
	assert.Contains(t, c.AllowedLanguages, "en")
}

// --- anime carve-out ---------------------------------------------

// A native-title (CJK) anime carrying a known fansub-group bracket must
// survive the non-Latin-script drop that was deleting it at ingest, while
// a bare CJK title with no anime signal still drops.
func TestFilter_AnimeCarveOut_NonLatinScript(t *testing.T) {
	f := New(enforcing())

	kept := f.Decide(Input{Title: "[SubsPlease] 葬送のフリーレン - 01 (1080p)", PrimaryExtension: "mkv"})
	assert.True(t, kept.Allow, "fansub-bracketed CJK anime must be kept")
	assert.Equal(t, ReasonNone, kept.Reason)

	dropped := f.Decide(Input{Title: "電影 2023", PrimaryExtension: "mkv"})
	assert.False(t, dropped.Allow, "bare CJK title (no anime signal) still drops")
	assert.Equal(t, ReasonNonLatinScript, dropped.Reason)
}

// TMDB tags anime with its original language ("ja"); the language drop
// must not delete a matched, English-subbed anime.
func TestFilter_AnimeCarveOut_JapaneseLanguageTag(t *testing.T) {
	f := New(enforcing())

	kept := f.Decide(Input{
		Title:            "[Erai-raws] Shingeki no Kyojin - 1090 [1080p][Multiple Subtitle]",
		PrimaryExtension: "mkv",
		ContentType:      "tv_show",
		Languages:        []string{"ja"},
	})
	assert.True(t, kept.Allow, "ja-tagged fansub anime must be kept")

	dropped := f.Decide(Input{Title: "Nekaya Film", PrimaryExtension: "mkv", Languages: []string{"ru"}})
	assert.False(t, dropped.Allow, "ru-tagged non-anime still drops")
	assert.Equal(t, ReasonNonEnglishLanguage, dropped.Reason)
}

// The NSFW-keyword carve-out is porn-safe: it fires ONLY for a KNOWN
// fansub group (disjoint from adult studios). A rating token on a
// non-anime release, or on an unlisted group, still drops.
func TestFilter_AnimeCarveOut_NSFWKeywordPornSafe(t *testing.T) {
	f := New(enforcing())

	kept := f.Decide(Input{Title: "[SubsPlease] Some Ecchi Show [18+] (1080p)", PrimaryExtension: "mkv"})
	assert.True(t, kept.Allow, "known-fansub anime with a rating token must be kept")

	genericAdult := f.Decide(Input{Title: "Some Movie [18+]", PrimaryExtension: "mp4"})
	assert.False(t, genericAdult.Allow, "non-anime [18+] must still drop")
	assert.Equal(t, ReasonNSFWKeyword, genericAdult.Reason)

	studio := f.Decide(Input{Title: "[Brazzers] scene 12", PrimaryExtension: "mp4"})
	assert.False(t, studio.Allow, "adult studio in brackets must never be carved")
	assert.Equal(t, ReasonNSFWKeyword, studio.Reason)
}

// AnimeAware=false restores the pre-carve behaviour exactly.
func TestFilter_AnimeAwareDisabled_RestoresDrops(t *testing.T) {
	cfg := enforcing()
	cfg.AnimeAware = false
	f := New(cfg)

	d := f.Decide(Input{Title: "[SubsPlease] 葬送のフリーレン - 01 (1080p)", PrimaryExtension: "mkv"})
	assert.False(t, d.Allow, "with AnimeAware off, the CJK anime drops again")
	assert.Equal(t, ReasonNonLatinScript, d.Reason)
}

// --- DropReason.String pinned for metric labels ------------------

func TestDropReason_Strings(t *testing.T) {
	assert.Equal(t, "none", ReasonNone.String())
	assert.Equal(t, "blocked_extension", ReasonBlockedExtension.String())
	assert.Equal(t, "blocked_content_type", ReasonBlockedContentType.String())
	assert.Equal(t, "non_english_language", ReasonNonEnglishLanguage.String())
	assert.Equal(t, "non_latin_script", ReasonNonLatinScript.String())
	assert.Equal(t, "mp3_only", ReasonMP3Only.String())
	assert.Equal(t, "nsfw_keyword", ReasonNSFWKeyword.String())
	assert.Equal(t, "nsfw_content_type", ReasonNSFWContentType.String())
	assert.Equal(t, "llm_non_english", ReasonLLMNonEnglish.String())
	assert.Equal(t, "foreign_audio", ReasonForeignAudio.String())
}

// The ladder must decide a foreign-audio release BEFORE the language check —
// TMDB tags a Portuguese dub of an English film "en", so reaching step 8 keeps
// it. Pins the ordering, not just the rule.
func TestForeignAudioBeatsEnglishLanguageTag(t *testing.T) {
	cfg := NewDefaultConfig()
	cfg.Enabled = true
	cfg.Enforce = true
	f := New(cfg)
	d := f.DecideDeterministic(Input{
		Title:       "Evil.Dead.Burn.2026.1080p.WEBRip.Dublado.mkv",
		ContentType: "movie",
		Languages:   []string{"en"},
	})
	assert.False(t, d.Allow)
	assert.Equal(t, ReasonForeignAudio, d.Reason)
}

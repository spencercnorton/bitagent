package anime

import "regexp"

var (
	// foreignAudioRe lists release-scene tokens that name the language of this
	// release's AUDIO. Subtitle markers are deliberately absent: "Napisy PL",
	// "SweSub", "NLsubs", "Legendado" and "VOSTFR" all leave the original
	// audio intact, so an English film carrying them is perfectly watchable.
	// The 457-case frontier-adjudicated gold corpus caught exactly that
	// over-reach — a Polish-subtitled Death of a Unicorn was being dropped.
	//
	// Every token here is a scene marker with no English-title meaning, so it
	// cannot fire on "The Italian Job" or "The French Connection". Bare
	// language names (italian, french, german, spanish, latino) are also
	// absent: a live probe found they appear almost exclusively inside
	// multi-audio packs that also carry ENG ("...ENG.LATINO.CASTELLANO..."),
	// so as standalone evidence they are noise.
	foreignAudioRe = regexp.MustCompile(`(?i)(?:^|[^a-z])(` +
		`dublado|` + // pt-BR dub
		`lektor|dubbing\.pl|pl-?dub|` + // Polish voice-over / dub
		`truefrench|vff|vfq|vfi|vf2|` + // French audio
		`castellano|` + // Spanish (Spain) audio
		`dublaj` + // Turkish dub — bare `turkce` is NOT audio-specific
		// ("Movie.2024.TURKCE.Altyazili" advertises Turkish SUBTITLES)
		`)(?:[^a-z]|$)`)

	// foreignDubCompoundRe is the "<language> Dub" form. Only the dub half of
	// anime.nonEnglishTrackRe: "Hindi Dubbed" replaces the audio, "Spanish
	// Subbed" does not.
	foreignDubCompoundRe = regexp.MustCompile(`(?i)(hindi|tamil|telugu|kannada|` +
		`malayalam|bengali|french|german|spanish|castellano|italian|portuguese|` +
		`russian|polish|arabic|latino|turkish)[ ._-]{0,2}dub(?:bed)?s?\b`)

	// foreignSubtitleRe lists standalone scene tokens naming a non-English
	// SUBTITLE track. These must NEVER feed ForeignAudioOnly: foreign
	// subtitles leave the audio untouched, and dropping on them is the exact
	// over-reach the 457-case gold corpus caught. They exist solely to stop
	// the fansub-group fallback manufacturing an English-subtitle verdict
	// over a name that explicitly names a different subtitle language.
	foreignSubtitleRe = regexp.MustCompile(`(?i)(?:^|[^a-z])(` +
		`vostfr|legendado|napisy|nlsubbed|nlsubs|swesub|dksub|norsub|finsub|` +
		`italiansubs|subspedia|altyazili` +
		`)(?:[^a-z]|$)`)

	// englishEvidenceRe is the widest English-track evidence, and it is
	// deliberately wide: this gate makes a destructive rule fail OPEN, so a
	// loose match costs one kept foreign release while a missed one deletes a
	// watchable English release.
	//
	//   eng/english/en — multi-audio packs list tracks positionally
	//     ("ENG.LATINO.HINDI", "[NAPISY.PL.EN]") rather than saying "Dual
	//     Audio", and the bare EN spelling is common in subtitle tags.
	//   multi — the scene's MULTi tag means several audio tracks INCLUDING
	//     the original, so "MULTi.TRUEFRENCH" is a French dub added alongside
	//     English, not a replacement for it. A live probe of the top-seeded
	//     matched corpus found MULTi releases were the dominant
	//     false-positive class before this term was added.
	englishEvidenceRe = regexp.MustCompile(`(?i)(?:^|[^a-z])(eng|english|en|multi)(?:[^a-z]|$)`)
)

// foreignAudioMarker reports whether the name carries an explicit non-English
// AUDIO marker — either a standalone scene token or a "<language> Dub" compound.
//
// Pure regex, and deliberately NOT a call back into ForeignAudioOnly, so Detect
// can consult it while building the Signals that ForeignAudioOnly later reads.
//
// Note this is narrower than nonEnglishTrackRe, which also matches subtitle
// forms ("Spanish Subbed"). ForeignAudioOnly must not fire on those; Detect's
// group fallback must be suppressed by both, so the two call sites combine
// these predicates differently on purpose.
func foreignAudioMarker(name string) bool {
	return foreignAudioRe.MatchString(name) || foreignDubCompoundRe.MatchString(name)
}

// explicitForeignTrack reports ANY explicit non-English track advertisement the
// package can recognise — audio tokens, "<language> Dub/Sub" compounds, and
// standalone subtitle tokens.
//
// Only the fansub-group fallback in Detect uses this. ForeignAudioOnly
// deliberately uses the audio half alone, because a foreign SUBTITLE says
// nothing about whether the release is watchable in English. Keeping the two
// predicates separate is the whole point; collapsing them reintroduces the
// over-reach the gold corpus caught.
func explicitForeignTrack(name string) bool {
	return foreignAudioMarker(name) ||
		foreignSubtitleRe.MatchString(name) ||
		nonEnglishTrackRe.MatchString(name)
}

// ForeignAudioOnly reports true when a release name names a non-English AUDIO
// track and carries no English-track evidence of any kind — i.e. the release
// cannot be watched in English.
//
// It answers the question the content filter's other rules cannot: what
// language is THIS RELEASE in. The script check reads the title's alphabet and
// the language check reads TMDB's tag — which is the language of the WORK, so
// a Brazilian dub of an American film arrives tagged ["en"] and passes. A
// live census found 9,306 such rows: Toy Story, Zootopia and Evil Dead in
// Portuguese, matched, ranked by seeders and served to Radarr as English.
//
// English evidence is taken from Detect (which now counts a curated subbing
// fansub group as an English-subtitle advertisement) plus a wide bare-token
// test, so a multi-audio pack listing several dubs alongside English is kept.
func ForeignAudioOnly(name string) bool {
	if !foreignAudioMarker(name) {
		return false
	}
	if englishEvidenceRe.MatchString(name) {
		return false
	}
	s := Detect(name)
	return !s.EnglishAudioSignal && !s.EnglishSubSignal
}

// Package cataloguepolicy derives source-bound catalogue policy receipts.
// Receipts are shadow observations only: they never authorize serving changes,
// crawler blocking or deletion.
package cataloguepolicy

import (
	"regexp"
	"strings"
	"time"
)

const ParserVersion = "advertised-english-v1"

// Track describes an advertised track, never an inspected media stream.
type Track string

const (
	TrackUnknown  Track = "unknown"
	TrackYes      Track = "yes"
	TrackNo       Track = "no"
	TrackConflict Track = "conflicting"
)

// EnglishEvidence keeps audio, subtitles and work/title language separate.
type EnglishEvidence struct {
	Source        string    `json:"source"`
	ObservedAt    time.Time `json:"observed_at"`
	ParserVersion string    `json:"parser_version"`
	Audio         Track     `json:"audio"`
	Subtitles     Track     `json:"subtitles"`
	Claims        []string  `json:"claims"`
}

// EnglishDecision preserves uncertainty and mixed packs for review.
type EnglishDecision struct {
	State                  string            `json:"state"`
	Reason                 string            `json:"reason"`
	AnimeSubtitleAllowance bool              `json:"anime_subtitle_allowance"`
	Evidence               []EnglishEvidence `json:"evidence"`
}

var (
	technicalBoundary = regexp.MustCompile(`(?i)(?:^|[ ._\-(])(?:19\d{2}|20\d{2}|s\d{1,2}e?\d{0,3}|\d{3,4}p|web[ ._-]?(?:dl|rip)|blu[ ._-]?ray)\b`)
	brackets          = regexp.MustCompile(`\[([^\]]+)\]`)
	englishAudio      = regexp.MustCompile(`(?i)\b(?:eng(?:lish)?[ ._-]+(?:audio|dub(?:bed)?)|(?:audio|dub(?:bed)?)[ ._-]+eng(?:lish)?)\b`)
	englishSubs       = regexp.MustCompile(`(?i)\b(?:eng(?:lish)?[ ._-]*(?:subs?|subtitles?)|(?:subs?|subtitles?)[ ._-]+eng(?:lish)?)\b`)
	trackList         = regexp.MustCompile(`(?i)\b(audio|subs?|subtitles?)([ ._-]+only)?\s*:\s*([^;\]\)]+)`)
	englishWord       = regexp.MustCompile(`(?i)\b(?:english|eng|en)\b`)
	negatedEnglish    = regexp.MustCompile(`(?i)\b(?:(?:no|not|without|except|excluding)[ ._-]+(?:english|eng|en)|non[ ._-]?(?:english|eng|en)|(?:english|eng|en)[ ._-]+(?:absent|unavailable|excluded|not[ ._-]+(?:available|included|present)))\b`)
	noAudio           = regexp.MustCompile(`(?i)\b(?:no[ ._-]+eng(?:lish)?[ ._-]+(?:audio|dub(?:bed)?)|(?:japanese|jpn|french|german|spanish|hindi)[ ._-]+audio[ ._-]+only)\b`)
	noSubs            = regexp.MustCompile(`(?i)\b(?:no[ ._-]+eng(?:lish)?[ ._-]+(?:subs?|subtitles?)|no[ ._-]+(?:subs?|subtitles?))\b`)
	foreignWord       = regexp.MustCompile(`(?i)\b(?:japanese|jpn|ja|french|fra|fr|german|deu|de|spanish|spa|es|hindi|hin|hi|italian|ita|it|portuguese|por|pt|russian|rus|ru|chinese|zho|zh|korean|kor|ko)\b`)
)

// ParseEnglishClaims reads explicit track claims in technical tails or track
// brackets. Bare language words, translated titles, original_language, generic
// dual/multi audio, dubbed and group reputation do not prove an English track.
func ParseEnglishClaims(name, source string, at time.Time) EnglishEvidence {
	e := EnglishEvidence{Source: source, ObservedAt: at, ParserVersion: ParserVersion, Audio: TrackUnknown, Subtitles: TrackUnknown, Claims: []string{}}
	texts := []string{}
	if loc := technicalBoundary.FindStringIndex(name); loc != nil {
		texts = append(texts, name[loc[0]:])
	}
	for _, m := range brackets.FindAllStringSubmatch(name, -1) {
		if trackList.MatchString(m[1]) || englishAudio.MatchString(m[1]) || englishSubs.MatchString(m[1]) || noAudio.MatchString(m[1]) || noSubs.MatchString(m[1]) {
			texts = append(texts, m[1])
		}
	}
	for _, text := range texts {
		add := func(kind string, value Track, claim string) {
			if kind == "audio" {
				e.Audio = combine(e.Audio, value)
			} else {
				e.Subtitles = combine(e.Subtitles, value)
			}
			e.Claims = append(e.Claims, claim)
		}
		positiveText := noAudio.ReplaceAllString(text, "")
		positiveText = noSubs.ReplaceAllString(positiveText, "")
		positiveText = negatedEnglish.ReplaceAllString(positiveText, "")
		for _, m := range englishAudio.FindAllString(positiveText, -1) {
			add("audio", TrackYes, m)
		}
		for _, m := range englishSubs.FindAllString(positiveText, -1) {
			add("subtitles", TrackYes, m)
		}
		for _, m := range noAudio.FindAllString(text, -1) {
			add("audio", TrackNo, m)
		}
		for _, m := range noSubs.FindAllString(text, -1) {
			add("subtitles", TrackNo, m)
		}
		for _, m := range trackList.FindAllStringSubmatch(text, -1) {
			kind := strings.ToLower(m[1])
			if kind != "audio" {
				kind = "subtitles"
			}
			if negatedEnglish.MatchString(m[3]) {
				add(kind, TrackNo, m[0])
			}
			if englishWord.MatchString(negatedEnglish.ReplaceAllString(m[3], "")) {
				add(kind, TrackYes, m[0])
			} else if m[2] != "" && foreignWord.MatchString(m[3]) {
				add(kind, TrackNo, m[0])
			}
		}
	}
	return e
}

func combine(a, b Track) Track {
	if a == TrackUnknown {
		return b
	}
	if b == TrackUnknown || a == b {
		return a
	}
	return TrackConflict
}

// EvaluateEnglish applies the draft subtitle preference explicitly. A caller
// must supply all media members of a pack; incomplete file evidence abstains.
// Positive release claims do not conceal members with missing/conflicting data.
func EvaluateEnglish(evidence []EnglishEvidence, anime, allowAnimeSubs, incomplete bool) EnglishDecision {
	d := EnglishDecision{State: "unknown", Reason: "missing_track_evidence", AnimeSubtitleAllowance: allowAnimeSubs, Evidence: evidence}
	if incomplete {
		d.Reason = "incomplete_file_evidence"
		return d
	}
	if len(evidence) == 0 {
		return d
	}
	states := map[string]bool{}
	for _, e := range evidence {
		state := "unknown"
		switch {
		case e.Audio == TrackConflict || e.Subtitles == TrackConflict:
			state = "conflicting"
		case e.Audio == TrackYes:
			state = "eligible_audio"
		case anime && allowAnimeSubs && e.Subtitles == TrackYes:
			state = "eligible_anime_subtitles"
		case e.Audio == TrackNo && (!anime || !allowAnimeSubs || e.Subtitles == TrackNo):
			state = "ineligible_review"
		}
		states[state] = true
	}
	if states["conflicting"] {
		d.State = "conflicting"
		d.Reason = "contradictory_advertisements"
		return d
	}
	if len(states) > 1 {
		d.State = "mixed_review"
		d.Reason = "member_evidence_differs"
		return d
	}
	for state := range states {
		d.State = state
	}
	if d.State != "unknown" {
		d.Reason = "explicit_advertised_tracks"
	}
	return d
}

package model

// EnglishAudio classifies an ANIME release's English availability, derived
// from release-name conventions (and, in a later phase, the LLM extraction):
// an English audio dub, English subtitles over Japanese audio, or a raw with
// neither. NULL/invalid = unknown, or not an anime release — Western content
// is English-audio by convention and the distinction carries no signal.
// ENUM(dub, sub, none)
type EnglishAudio string

func (e EnglishAudio) Label() string {
	return e.String()
}

func (e EnglishAudio) IsNil() bool {
	return e == ""
}

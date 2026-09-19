package model

// DeriveEnglishAudio computes the english_audio column from the deterministic
// anime signals: dub when an English AUDIO track is advertised (a Dual-Audio
// release usually also carries subs, so audio wins), sub when only subtitles
// are advertised, NULL otherwise — including non-anime releases (Western
// content is English-audio by convention; the distinction carries no signal)
// and raws ('none' is deliberately reserved for the LLM phase: "\braw\b"
// collides with fansub group names).
//
// TRIPWIRE (updated for the T4 LLM phase): english_audio_source records
// which layer wrote the value ('name' = this derivation, 'llm' = the
// llmmatch extraction). BOTH writers are provenance-aware in lockstep — the
// processor merges in newTorrentContent + persist's restoreLLMEnglishAudio
// (its OnConflict UpdateAll would otherwise erase 'llm' rows on cycles with
// no signal), and derived-backfill's englishAudioChanges never downgrades an
// 'llm' row without a fresh deterministic signal. Any NEW writer of
// english_audio MUST keep the pair in sync and honour that precedence:
// deterministic-explicit > llm > absent.
//
// Known cohort limit: the anchored Dual/Multi-Audio regex fires without
// reading adjacent language lists, so a non-English dual-audio anime name
// ("[Grp] Show - 500 [Dual Audio] [Tamil+Telugu]") derives 'dub' here and —
// by the precedence above — outranks the LLM's correct 'none' read, while
// AnimeEnglishOK simultaneously rejects the release as a raw. Fixing this
// means suppressing the audio signal when an adjacent language list excludes
// English (a detect.go change, tracked for T4 phase 3).
func DeriveEnglishAudio(isAnime, audioSignal, subSignal bool) NullEnglishAudio {
	if !isAnime {
		return NullEnglishAudio{}
	}
	switch {
	case audioSignal:
		return NewNullEnglishAudio(EnglishAudioDub)
	case subSignal:
		return NewNullEnglishAudio(EnglishAudioSub)
	default:
		return NullEnglishAudio{}
	}
}

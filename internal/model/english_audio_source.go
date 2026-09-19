package model

// english_audio provenance markers (torrent_contents.english_audio_source).
// The column is NULL iff english_audio is NULL; both writers (processor
// persist, derived-backfill) set the pair together. Precedence between the
// layers: a fresh deterministic 'name' value always wins; an absent
// deterministic signal never downgrades an 'llm' row.
//
// ponytail: plain constants + NullString field, not a go-enum type — the
// source is internal provenance with two values; generate the enum when a
// GQL/torznab surface needs it.
const (
	EnglishAudioSourceName = "name"
	EnglishAudioSourceLLM  = "llm"
)

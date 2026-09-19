package model

const (
	SourceTmdb = "tmdb"
	SourceImdb = "imdb"
	SourceTvdb = "tvdb"
)

// AltTitleAttributePrefix prefixes content_attributes keys that hold
// alternative / translated titles (key shape: alt_title:<iso3166>:<hash>).
// The hash suffix keeps rows unique under the (…, source, key) primary key
// and deterministic across re-fetches regardless of TMDB response ordering.
const AltTitleAttributePrefix = "alt_title:"

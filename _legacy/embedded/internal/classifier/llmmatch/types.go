package llmmatch

// Extraction is stage-1 output: the canonical identity the model reads out of
// a messy release name.
type Extraction struct {
	Title   string `json:"title"`
	Year    int    `json:"year"`
	Type    string `json:"type"` // "movie" | "tv"
	Season  int    `json:"season"`
	Episode int    `json:"episode"`

	// IsAnime marks Japanese animation. Anime release names are their own
	// dialect (fansub group brackets, romaji/alt titles, absolute episode
	// numbering) so the matcher treats them specially.
	IsAnime bool `json:"is_anime"`
	// IsPack marks a bundle of MULTIPLE different films (trilogy, collection,
	// filmography). torrent_contents attaches ONE content row, so matching a
	// pack to its first film is a wrong match — packs stay unattached.
	// Multi-season TV packs are NOT packs: they still belong to one show.
	IsPack bool `json:"is_pack"`
	// IsAdult marks pornographic releases. These sometimes slip past the
	// deterministic xxx typing (site-prefixed names read as plausible titles)
	// and must never be attached to a mainstream movie/tv entry.
	IsAdult bool `json:"is_adult"`
	// English is the model's read of the release's English availability:
	//   "dub"     — has an English audio dub (Dual Audio / Multi-Audio / ENG DUB)
	//   "sub"     — Japanese audio but an English subtitle track (typical fansub)
	//   "none"    — Japanese-only, no English audio or subs (a "raw")
	//   "unknown" — cannot tell from the available signals
	English string `json:"english"`

	// OK is false when the model declined to name a title it recognised.
	OK bool `json:"-"`
}

// English track values.
const (
	EnglishDub     = "dub"
	EnglishSub     = "sub"
	EnglishNone    = "none"
	EnglishUnknown = "unknown"
)

// Candidate is one TMDB search hit presented to the stage-2 rerank call. The
// action builds these from tmdb search results so this package stays free of a
// tmdb import.
type Candidate struct {
	ID       int64  `json:"tmdb_id"`
	Title    string `json:"title"`
	Year     int    `json:"year"`
	Overview string `json:"overview,omitempty"`
	// AltTitles are stored alternative/translated titles for this candidate,
	// used ONLY by the post-rerank identity gate as authoritative same-work
	// evidence. Deliberately `json:"-"`: this struct is serialized verbatim
	// into the model prompt, and adding titles there would both inflate the
	// 737-token prompt and hand the model new material to rationalize from.
	// The gate is a check on the model, so its evidence must not be an input
	// to the model.
	AltTitles []string `json:"-"`
}

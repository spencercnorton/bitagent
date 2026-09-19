package animedb

import "github.com/spencercnorton/bitagent/internal/model"

// Alias is one resolved anime identity: a normalized lookup key mapped to a
// canonical TMDB entry. It is both the persisted row shape (anime_titles) and
// the in-memory resolver value.
type Alias struct {
	Normalized string            // the lookup key (see Normalize)
	Display    string            // the alias' human-readable form
	TMDBType   model.ContentType // model.ContentTypeMovie | model.ContentTypeTvShow
	TMDBID     int64
	AniDBID    int
	Source     string // "primary" | "official" | "synonym" | "short" | "seed"
	// DirectAttach is in-memory trust provenance, never loaded from the
	// generated anime_titles table. Only a baked, human-curated seed may set
	// it; external mapping data can guide search but must still be reranked.
	DirectAttach bool
	// Adult marks a gate-only entry: a known adult title that must be rejected,
	// never attached. Only the baked seed set produces these; data-built rows
	// always carry a real TMDBID and Adult=false.
	Adult bool
}

// titleSourceName maps an AniDB title type to its provenance label.
func titleSourceName(t int) string {
	switch t {
	case titleTypePrimary:
		return "primary"
	case titleTypeOfficial:
		return "official"
	case titleTypeShort:
		return "short"
	default:
		return "synonym"
	}
}

// sourceRank orders alias provenance for display/tie-breaking: a primary title
// is preferred over an official translation, over a synonym, over a short
// title. Lower is better.
var sourceRank = map[string]int{"primary": 0, "official": 1, "synonym": 2, "short": 3}

// BuildAliases joins the anidbid->TMDB mappings with the anidbid->aliases dump
// into the deduplicated alias table. For every mapped AniDB entry it emits the
// entry's own primary romaji name plus each alias whose AniDB id is in the
// mapping. Aliases whose AniDB id has no TMDB mapping are dropped (nothing to
// attach).
//
// Deduplication key is the normalized form. When several source aliases
// normalize to the same key AND agree on the TMDB target, they collapse to one
// row (keeping the best-ranked source's display form). When they DISAGREE on
// the TMDB target the key is AMBIGUOUS and dropped entirely — a lookup must
// never have to guess between two different shows.
func BuildAliases(mappings []Mapping, titles []Title) []Alias {
	byAID := make(map[int]Mapping, len(mappings))
	for _, m := range mappings {
		byAID[m.AniDBID] = m
	}

	type agg struct {
		alias    Alias
		rank     int
		conflict bool
	}
	byNorm := make(map[string]*agg)

	add := func(norm, display, source string, m Mapping) {
		if len(norm) < minNormalizedLen {
			return
		}
		a, ok := byNorm[norm]
		if !ok {
			byNorm[norm] = &agg{
				alias: Alias{
					Normalized: norm,
					Display:    display,
					TMDBType:   m.TMDBType,
					TMDBID:     m.TMDBID,
					AniDBID:    m.AniDBID,
					Source:     source,
				},
				rank: sourceRank[source],
			}
			return
		}
		if a.conflict {
			return
		}
		if a.alias.TMDBType != m.TMDBType || a.alias.TMDBID != m.TMDBID {
			// Same normalized alias points at two different TMDB entries — drop it.
			a.conflict = true
			return
		}
		// Same target: keep the better-ranked source's display form.
		if r := sourceRank[source]; r < a.rank {
			a.rank = r
			a.alias.Display = display
			a.alias.Source = source
			a.alias.AniDBID = m.AniDBID
		}
	}

	// The mapping's own primary romaji name is a first-class alias.
	for _, m := range mappings {
		add(Normalize(m.PrimaryName), m.PrimaryName, "primary", m)
	}
	// Then every dump alias whose anidbid is mapped.
	for _, t := range titles {
		m, ok := byAID[t.AID]
		if !ok {
			continue
		}
		add(Normalize(t.Value), t.Value, titleSourceName(t.Type), m)
	}

	out := make([]Alias, 0, len(byNorm))
	for _, a := range byNorm {
		if a.conflict {
			continue
		}
		out = append(out, a.alias)
	}
	return out
}

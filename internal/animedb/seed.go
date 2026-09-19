package animedb

import "github.com/spencercnorton/bitagent/internal/model"

// seedEntry is a curated, always-present alias. The seed set is the small
// collection of high-value mappings — fan abbreviations that aren't in the
// AniDB dump, plus adult gates — that must resolve even before the first data
// refresh and fully offline. It directly replaces the former
// applyLLMMatchEdgeOverrides / forcedLLMMatchID switches in matcher.go.
//
// Alias/contains strings are written in readable form and normalized at load,
// so they can never drift from what Normalize produces for a release name.
type seedEntry struct {
	// aliases are matched exactly (after normalization) against a parsed or
	// extracted title.
	aliases []string
	// contains are normalized substrings scanned against the whole normalized
	// release name — the safety net for abbreviations buried in a noisy name
	// ("[Judas] KiseKoi - S02E12.mkv") that the parsed title might miss. Kept
	// tiny (seed set only) so the O(n) scan stays cheap.
	contains []string
	title    string
	ct       model.ContentType
	tmdbID   int64
	adult    bool
}

func (s seedEntry) alias() Alias {
	return Alias{
		Display:      s.title,
		TMDBType:     s.ct,
		TMDBID:       s.tmdbID,
		Source:       "seed",
		DirectAttach: !s.adult && s.tmdbID != 0,
		Adult:        s.adult,
	}
}

// seedEntries is the baked-in alias set. Keep it disjoint and high-precision;
// broad new coverage belongs in the data-built table, not here.
var seedEntries = []seedEntry{
	{
		aliases:  []string{"KiseKoi", "Sono Bisque Doll wa Koi wo Suru", "My Dress-Up Darling"},
		contains: []string{"KiseKoi"},
		title:    "My Dress-Up Darling",
		ct:       model.ContentTypeTvShow,
		tmdbID:   123249,
	},
	{
		aliases:  []string{"Jidou Hanbaiki ni Umarekawatta Ore wa Meikyuu wo Samayou", "Reborn as a Vending Machine, I Now Wander the Dungeon"},
		contains: []string{"Jidou Hanbaiki ni Umarekawatta Ore wa Meikyuu wo Samayou"},
		title:    "Reborn as a Vending Machine, I Now Wander the Dungeon",
		ct:       model.ContentTypeTvShow,
		tmdbID:   207564,
	},
	{
		aliases:  []string{"Shibou Yuugi de Meshi wo Kuu", "Shiboyugi: Playing Death Games to Put Food on the Table"},
		contains: []string{"Shibou Yuugi de Meshi wo Kuu"},
		title:    "Shiboyugi: Playing Death Games to Put Food on the Table",
		ct:       model.ContentTypeTvShow,
		tmdbID:   263330,
	},
	{
		// Adult: gate-only, no TMDB attach. Matches either the romaji or the
		// English title so a mainstream-looking name can never slip an attach.
		aliases:  []string{"Ishuzoku Reviewers", "Interspecies Reviewers"},
		contains: []string{"Ishuzoku Reviewers", "Interspecies Reviewers"},
		title:    "Interspecies Reviewers",
		ct:       model.ContentTypeTvShow,
		adult:    true,
	},
}

// seedExactMap builds the exact-lookup entries contributed by the seed set,
// keyed by normalized alias.
func seedExactMap() map[string]Alias {
	m := make(map[string]Alias, len(seedEntries)*2)
	for _, s := range seedEntries {
		base := s.alias()
		for _, raw := range s.aliases {
			key := Normalize(raw)
			if len(key) < minNormalizedLen {
				continue
			}
			a := base
			a.Normalized = key
			m[key] = a
		}
	}
	return m
}

// seedContains is a seed entry pre-normalized for the substring safety net.
type seedContains struct {
	subs  []string
	alias Alias
}

// seedContainsList returns the seed entries' normalized contains-substrings.
func seedContainsList() []seedContains {
	out := make([]seedContains, 0, len(seedEntries))
	for _, s := range seedEntries {
		subs := make([]string, 0, len(s.contains))
		for _, raw := range s.contains {
			n := Normalize(raw)
			if len(n) >= minNormalizedLen {
				subs = append(subs, n)
			}
		}
		if len(subs) > 0 {
			out = append(out, seedContains{subs: subs, alias: s.alias()})
		}
	}
	return out
}

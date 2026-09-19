package animedb

import (
	"strings"
	"testing"

	"github.com/spencercnorton/bitagent/internal/model"
)

const sampleAnimeList = `<?xml version="1.0" encoding="utf-8"?>
<anime-list>
  <anime anidbid="1" tvdbid="72025" defaulttvdbseason="1" tmdbtv="26209" tmdbseason="1">
    <name>Seikai no Monshou</name>
  </anime>
  <anime anidbid="7" tvdbid="movie" imdbid="tt0119698">
    <name>Mononoke-hime</name>
  </anime>
  <anime anidbid="11" tvdbid="70900" defaulttvdbseason="0" episodeoffset="2" tmdbid="1390599" imdbid="tt7941838">
    <name>Initial D Battle Stage</name>
  </anime>
  <anime anidbid="4521" tvdbid="unknown">
    <name>Some Unmapped Show</name>
  </anime>
</anime-list>`

func TestParseAnimeList(t *testing.T) {
	t.Parallel()
	got, err := ParseAnimeList(strings.NewReader(sampleAnimeList))
	if err != nil {
		t.Fatalf("ParseAnimeList: %v", err)
	}
	// Only the two TMDB-mapped entries survive (Mononoke-hime has no tmdb id,
	// "Some Unmapped Show" has neither tvdb nor tmdb).
	if len(got) != 2 {
		t.Fatalf("got %d mappings, want 2: %+v", len(got), got)
	}
	byAID := map[int]Mapping{}
	for _, m := range got {
		byAID[m.AniDBID] = m
	}
	tv := byAID[1]
	if tv.TMDBType != model.ContentTypeTvShow || tv.TMDBID != 26209 || tv.PrimaryName != "Seikai no Monshou" {
		t.Errorf("aid 1 = %+v, want tv/26209/Seikai no Monshou", tv)
	}
	mv := byAID[11]
	if mv.TMDBType != model.ContentTypeMovie || mv.TMDBID != 1390599 {
		t.Errorf("aid 11 = %+v, want movie/1390599", mv)
	}
}

const sampleTitles = `# created: whenever
# <aid>|<type>|<language>|<title>
1|1|x-jat|Seikai no Monshou
1|4|en|Crest of the Stars
1|2|ja|星界の紋章
1|2|fr|Banniere des etoiles
11|1|x-jat|Initial D Battle Stage
11|2|en|Initial D: Battle Stage
malformed line without pipes
`

func TestParseAnimeTitles(t *testing.T) {
	t.Parallel()
	langs := map[string]bool{"x-jat": true, "en": true, "ja": true}
	got, err := ParseAnimeTitles(strings.NewReader(sampleTitles), langs)
	if err != nil {
		t.Fatalf("ParseAnimeTitles: %v", err)
	}
	// The French synonym is filtered out; the comment/malformed lines are
	// skipped; 5 lines remain.
	if len(got) != 5 {
		t.Fatalf("got %d titles, want 5: %+v", len(got), got)
	}
	for _, tt := range got {
		if tt.Lang == "fr" {
			t.Errorf("french synonym leaked through the language filter: %+v", tt)
		}
	}
}

func TestParseAnimeTitlesEmptyLangKeepsAll(t *testing.T) {
	t.Parallel()
	got, err := ParseAnimeTitles(strings.NewReader(sampleTitles), nil)
	if err != nil {
		t.Fatalf("ParseAnimeTitles: %v", err)
	}
	// nil filter keeps every language incl. French: 6 title lines.
	if len(got) != 6 {
		t.Fatalf("got %d titles, want 6 (all langs): %+v", len(got), got)
	}
}

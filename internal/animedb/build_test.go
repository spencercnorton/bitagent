package animedb

import (
	"testing"

	"github.com/spencercnorton/bitagent/internal/model"
)

func TestBuildAliasesJoinsAndNormalizes(t *testing.T) {
	t.Parallel()
	mappings := []Mapping{
		{AniDBID: 1, TMDBType: model.ContentTypeTvShow, TMDBID: 1429, PrimaryName: "Shingeki no Kyojin"},
	}
	titles := []Title{
		{AID: 1, Type: titleTypeOfficial, Lang: "en", Value: "Attack on Titan"},
		{AID: 1, Type: titleTypeSynonym, Lang: "x-jat", Value: "Shingeki no Kyojin"},
		{AID: 99, Type: titleTypePrimary, Lang: "x-jat", Value: "Unmapped Show"}, // no mapping -> dropped
	}

	got := BuildAliases(mappings, titles)
	idx := map[string]Alias{}
	for _, a := range got {
		idx[a.Normalized] = a
	}

	if a, ok := idx["shingekinokyojin"]; !ok || a.TMDBID != 1429 || a.TMDBType != model.ContentTypeTvShow {
		t.Errorf("romaji alias missing/wrong: %+v (ok=%v)", a, ok)
	}
	if a, ok := idx["attackontitan"]; !ok || a.TMDBID != 1429 {
		t.Errorf("english alias missing/wrong: %+v (ok=%v)", a, ok)
	}
	if _, ok := idx["unmappedshow"]; ok {
		t.Errorf("alias for an unmapped anidb id must be dropped")
	}
	// The primary name provenance wins the display of the shared key.
	if a := idx["shingekinokyojin"]; a.Source != "primary" {
		t.Errorf("expected primary source for the mapping name, got %q", a.Source)
	}
}

func TestBuildAliasesDropsAmbiguous(t *testing.T) {
	t.Parallel()
	// Two different shows share a colliding alias -> the alias is ambiguous and
	// must not be persisted (a lookup can't guess which one).
	mappings := []Mapping{
		{AniDBID: 1, TMDBType: model.ContentTypeTvShow, TMDBID: 100, PrimaryName: "First Show"},
		{AniDBID: 2, TMDBType: model.ContentTypeTvShow, TMDBID: 200, PrimaryName: "Second Show"},
	}
	titles := []Title{
		{AID: 1, Type: titleTypeSynonym, Lang: "en", Value: "Clash"},
		{AID: 2, Type: titleTypeSynonym, Lang: "en", Value: "Clash"},
	}
	got := BuildAliases(mappings, titles)
	for _, a := range got {
		if a.Normalized == "clash" {
			t.Fatalf("ambiguous alias 'clash' should have been dropped, got %+v", a)
		}
	}
}

func TestBuildAliasesSameTargetMultipleSeasonsIsNotAmbiguous(t *testing.T) {
	t.Parallel()
	// Multiple AniDB seasons of one show map to the same TMDB series id; a shared
	// alias across them is NOT ambiguous and must survive.
	mappings := []Mapping{
		{AniDBID: 1, TMDBType: model.ContentTypeTvShow, TMDBID: 26209, PrimaryName: "Seikai no Monshou"},
		{AniDBID: 4, TMDBType: model.ContentTypeTvShow, TMDBID: 26209, PrimaryName: "Seikai no Senki"},
	}
	titles := []Title{
		{AID: 1, Type: titleTypeSynonym, Lang: "en", Value: "Crest of the Stars"},
		{AID: 4, Type: titleTypeSynonym, Lang: "en", Value: "Crest of the Stars"},
	}
	got := BuildAliases(mappings, titles)
	var found bool
	for _, a := range got {
		if a.Normalized == "crestofthestars" {
			found = true
			if a.TMDBID != 26209 {
				t.Errorf("crestofthestars -> %d, want 26209", a.TMDBID)
			}
		}
	}
	if !found {
		t.Fatalf("shared-target alias 'crestofthestars' should survive")
	}
}

func TestBuildAliasesDropsTooShortKeys(t *testing.T) {
	t.Parallel()
	mappings := []Mapping{
		{AniDBID: 1, TMDBType: model.ContentTypeTvShow, TMDBID: 100, PrimaryName: "K"},
	}
	got := BuildAliases(mappings, nil)
	for _, a := range got {
		if a.Normalized == "k" {
			t.Fatalf("single-character key must be dropped, got %+v", a)
		}
	}
}

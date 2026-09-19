package adapter

import (
	"strconv"
	"testing"

	"github.com/spencercnorton/bitagent/internal/database/search"
	"github.com/spencercnorton/bitagent/internal/model"
	"github.com/spencercnorton/bitagent/internal/torznab"
)

// categoryValues returns every torznab "category" attribute value on the
// mapped result item.
func categoryValues(item torznab.SearchResultItem) []string {
	var out []string
	for _, a := range item.TorznabAttrs {
		if a.AttrName == torznab.AttrCategory {
			out = append(out, a.AttrValue)
		}
	}
	return out
}

// animeItem builds a result item with the stored is_anime flag set. The
// adapter emits 5070 from this persisted flag (computed once at classify time),
// NOT from re-detecting the release name at serve time — so the flag is passed
// explicitly rather than derived from the name here.
func animeItem(name string, ct model.ContentType, isAnime bool) search.TorrentContentResultItem {
	hash := make([]byte, 20)
	hash[0] = 1
	tor := model.Torrent{Name: name}
	_ = tor.InfoHash.UnmarshalBinary(hash)
	return search.TorrentContentResultItem{
		TorrentContent: model.TorrentContent{
			Torrent:     tor,
			ContentType: model.NullContentType{Valid: true, ContentType: ct},
			IsAnime:     isAnime,
		},
	}
}

func contains(vals []string, want string) bool {
	for _, v := range vals {
		if v == want {
			return true
		}
	}
	return false
}

// An anime TV release must be served under BOTH 5000 (generic TV) and
// 5070 (TV/Anime) so Sonarr anime-type series can find it.
func TestTorznabResult_AnimeTVEmits5070(t *testing.T) {
	item := torrentContentResultItemToTorznabResultItem(
		animeItem("[SubsPlease] Sousou no Frieren - 12 (1080p) [ABCD].mkv", model.ContentTypeTvShow, true), false,
	)
	cats := categoryValues(item)
	if !contains(cats, strconv.Itoa(torznab.CategoryTV.ID)) {
		t.Errorf("anime TV missing generic TV category %d; got %v", torznab.CategoryTV.ID, cats)
	}
	if !contains(cats, strconv.Itoa(torznab.CategoryTVAnime.ID)) {
		t.Errorf("anime TV missing TV/Anime category %d; got %v", torznab.CategoryTVAnime.ID, cats)
	}
	if item.Category != torznab.CategoryTVAnime.Name {
		t.Errorf("anime TV human label = %q, want %q", item.Category, torznab.CategoryTVAnime.Name)
	}
}

// A non-anime TV release stays TV-only — no 5070.
func TestTorznabResult_NonAnimeTVNo5070(t *testing.T) {
	item := torrentContentResultItemToTorznabResultItem(
		animeItem("The Wire S01E01 1080p BluRay x264-GROUP", model.ContentTypeTvShow, false), false,
	)
	cats := categoryValues(item)
	if !contains(cats, strconv.Itoa(torznab.CategoryTV.ID)) {
		t.Errorf("TV release missing TV category; got %v", cats)
	}
	if contains(cats, strconv.Itoa(torznab.CategoryTVAnime.ID)) {
		t.Errorf("non-anime TV must NOT carry 5070; got %v", cats)
	}
}

// An anime-shaped MOVIE stays under Movies — the Newznab set has no
// anime-movie subcategory, so 5070 (a TV subcat) must not be emitted.
func TestTorznabResult_AnimeMovieNo5070(t *testing.T) {
	item := torrentContentResultItemToTorznabResultItem(
		animeItem("[Judas] Kimetsu no Yaiba Mugen Train - Movie (1080p)", model.ContentTypeMovie, true), false,
	)
	cats := categoryValues(item)
	if !contains(cats, strconv.Itoa(torznab.CategoryMovies.ID)) {
		t.Errorf("anime movie missing Movies category; got %v", cats)
	}
	if contains(cats, strconv.Itoa(torznab.CategoryTVAnime.ID)) {
		t.Errorf("anime movie must NOT carry TV/Anime 5070; got %v", cats)
	}
}

// The TV/Anime category is advertised in caps (nested under TV) so Prowlarr
// maps the indexer as anime-capable.
func TestTorznab_5070AdvertisedInTVCaps(t *testing.T) {
	if !torznab.CategoryTV.Has(torznab.CategoryTVAnime.ID) {
		t.Fatalf("CategoryTV must advertise 5070 as a subcategory for caps + cat=5070 queries")
	}
}

// The adapter emits 5070 from the persisted is_anime flag, NOT by re-detecting
// the name. An anime-looking name whose stored flag is false must stay TV-only:
// this proves the switch from serve-time anime.Detect to the stored column.
func TestTorznabResult_ReadsStoredFlagNotName(t *testing.T) {
	item := torrentContentResultItemToTorznabResultItem(
		animeItem("[SubsPlease] Sousou no Frieren - 12 (1080p)", model.ContentTypeTvShow, false), false,
	)
	cats := categoryValues(item)
	if contains(cats, strconv.Itoa(torznab.CategoryTVAnime.ID)) {
		t.Errorf("stored is_anime=false must NOT carry 5070 despite an anime-shaped name; got %v", cats)
	}
	if item.Category == torznab.CategoryTVAnime.Name {
		t.Errorf("human label must not be TV/Anime when is_anime=false; got %q", item.Category)
	}
}

// Conversely, a plain-looking name flagged is_anime=true (e.g. a raw whose
// anime-ness was resolved at classify time) still gets 5070 — the flag, not
// the name, decides.
func TestTorznabResult_StoredFlagTrueEmits5070(t *testing.T) {
	item := torrentContentResultItemToTorznabResultItem(
		animeItem("Some Plain Title 2024 1080p", model.ContentTypeTvShow, true), false,
	)
	cats := categoryValues(item)
	if !contains(cats, strconv.Itoa(torznab.CategoryTVAnime.ID)) {
		t.Errorf("stored is_anime=true TV must carry 5070 regardless of name; got %v", cats)
	}
}

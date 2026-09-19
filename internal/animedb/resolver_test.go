package animedb

import (
	"testing"

	"github.com/spencercnorton/bitagent/internal/model"
)

func TestSeedResolverExactAliases(t *testing.T) {
	t.Parallel()
	r := NewSeedResolver()
	cases := []struct {
		title   string
		wantID  int64
		wantDsp string
	}{
		{"KiseKoi", 123249, "My Dress-Up Darling"},
		{"Sono Bisque Doll wa Koi wo Suru", 123249, "My Dress-Up Darling"},
		{"Shibou Yuugi de Meshi wo Kuu", 263330, "Shiboyugi: Playing Death Games to Put Food on the Table"},
	}
	for _, c := range cases {
		// Exact match on the extracted title (2nd arg).
		alias, ok := r.Lookup("", c.title, "")
		if !ok {
			t.Errorf("Lookup(%q) miss, want hit", c.title)
			continue
		}
		if alias.TMDBID != c.wantID || alias.Display != c.wantDsp {
			t.Errorf("Lookup(%q) = %+v, want id=%d display=%q", c.title, alias, c.wantID, c.wantDsp)
		}
		if alias.TMDBType != model.ContentTypeTvShow {
			t.Errorf("Lookup(%q) type = %q, want tv_show", c.title, alias.TMDBType)
		}
		if !alias.DirectAttach {
			t.Errorf("curated seed %q must retain direct-attach provenance", c.title)
		}
	}
}

func TestSeedResolverContainsNet(t *testing.T) {
	t.Parallel()
	r := NewSeedResolver()
	// Abbreviation buried in a noisy release name, only reachable via the
	// contains safety net (parsed/extract titles unhelpful).
	alias, ok := r.Lookup("", "Kiss Him, Not Me", "[Judas] KiseKoi - S02E12.mkv")
	if !ok {
		t.Fatal("contains-net miss for KiseKoi")
	}
	if alias.TMDBID != 123249 {
		t.Errorf("contains-net id = %d, want 123249", alias.TMDBID)
	}
}

func TestSeedResolverAdultGate(t *testing.T) {
	t.Parallel()
	r := NewSeedResolver()
	alias, ok := r.Lookup("", "Interspecies Reviewers", "")
	if !ok {
		t.Fatal("adult seed miss")
	}
	if !alias.Adult {
		t.Errorf("expected Adult=true for Interspecies Reviewers, got %+v", alias)
	}
	if alias.TMDBID != 0 {
		t.Errorf("adult gate must carry no TMDB id, got %d", alias.TMDBID)
	}
	if alias.DirectAttach {
		t.Error("adult gate must never be direct-attachable")
	}
}

func TestSeedResolverMiss(t *testing.T) {
	t.Parallel()
	r := NewSeedResolver()
	if _, ok := r.Lookup("Some Random Movie", "Some Random Movie", "Some.Random.Movie.2020.mkv"); ok {
		t.Error("expected a miss for an unknown title")
	}
	// Too-short keys never match.
	if _, ok := r.Lookup("K", "K", ""); ok {
		t.Error("single-char key must not match")
	}
}

func TestResolverSwapMergesDataAndSeedsWins(t *testing.T) {
	t.Parallel()
	r := NewSeedResolver()
	r.Swap([]Alias{
		{Normalized: "shingekinokyojin", Display: "Attack on Titan", TMDBType: model.ContentTypeTvShow, TMDBID: 1429},
		// A data row colliding with a seed key must lose to the seed.
		{Normalized: "kisekoi", Display: "WRONG", TMDBType: model.ContentTypeMovie, TMDBID: 999},
	})

	if a, ok := r.Lookup("", "Shingeki no Kyojin", ""); !ok || a.TMDBID != 1429 {
		t.Errorf("data alias not resolvable after Swap: %+v ok=%v", a, ok)
	} else if a.DirectAttach {
		t.Errorf("data alias must not gain direct-attach provenance: %+v", a)
	}
	if a, ok := r.Lookup("", "KiseKoi", ""); !ok || a.TMDBID != 123249 {
		t.Errorf("seed must win over a colliding data row: got %+v", a)
	}
}

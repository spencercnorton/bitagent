package wantbridge

import (
	"reflect"
	"testing"
)

func TestCanonicalise_TVStandard(t *testing.T) {
	tests := []struct {
		in            string
		wantTitle     string
		wantSeason    int
		wantEpisodes  []int
		wantYear      int
		wantKindIsTV  bool
	}{
		{"The.Wire.S01E03.Lessons.1080p.BluRay.x264-MIHD",
			"the wire", 1, []int{3}, 0, true},
		{"Breaking.Bad.S05.Complete.720p.WEB-DL",
			"breaking bad", 5, nil, 0, true},
		{"Game.of.Thrones.S08E06.iNTERNAL.720p.HDTV.x264",
			"game of thrones", 8, []int{6}, 0, true},
		{"some.long.show.name.s10e123.hevc",
			"some long show name", 10, []int{123}, 0, true},
		{"BoJack.Horseman.S04E10.x265.HEVC",
			"bojack horseman", 4, []int{10}, 0, true},
		{"The.X-Files.S01E03",
			"the x files", 1, []int{3}, 0, true},
	}
	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			c := Canonicalise(tt.in)
			if (c.Kind == KindTV) != tt.wantKindIsTV {
				t.Errorf("Kind: got %v, want TV=%v", c.Kind, tt.wantKindIsTV)
			}
			if c.Title != tt.wantTitle {
				t.Errorf("Title: got %q want %q", c.Title, tt.wantTitle)
			}
			if c.Season != tt.wantSeason {
				t.Errorf("Season: got %d want %d", c.Season, tt.wantSeason)
			}
			if !reflect.DeepEqual(c.EpisodeSet, tt.wantEpisodes) {
				t.Errorf("EpisodeSet: got %v want %v", c.EpisodeSet, tt.wantEpisodes)
			}
		})
	}
}

func TestCanonicalise_MovieStandard(t *testing.T) {
	tests := []struct {
		in        string
		wantTitle string
		wantYear  int
		wantKind  Kind
	}{
		{"Inception.2010.1080p.BluRay.x264", "inception", 2010, KindMovie},
		{"Inception (2010) [1080p]", "inception", 2010, KindMovie},
		{"Some.Movie.2024.WEB-DL.HEVC", "some movie", 2024, KindMovie},
		{"Mad.Max.Fury.Road.2015.IMAX.UHD.HDR", "mad max fury road", 2015, KindMovie},
		{"The.Lord.of.the.Rings.2001.Extended.1080p", "the lord of the rings", 2001, KindMovie},
	}
	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			c := Canonicalise(tt.in)
			if c.Title != tt.wantTitle {
				t.Errorf("Title: got %q want %q", c.Title, tt.wantTitle)
			}
			if c.Year != tt.wantYear {
				t.Errorf("Year: got %d want %d", c.Year, tt.wantYear)
			}
			if tt.wantTitle != "" && c.Kind != tt.wantKind {
				t.Errorf("Kind: got %v want %v", c.Kind, tt.wantKind)
			}
		})
	}
}

func TestCanonicalise_DefersTitleAtFirstNoiseToken(t *testing.T) {
	// Title tokens stop at the FIRST cut-point so codec/quality
	// tokens that come BEFORE a year don't get pulled into the title.
	c := Canonicalise("Movie.Name.UHD.2024.x264")
	if c.Title != "movie name" {
		t.Errorf("got Title=%q, want \"movie name\" (uhd should cut before year)", c.Title)
	}
}

func TestCanonicalise_AllNoiseReturnsUnknown(t *testing.T) {
	tests := []string{
		"1080p.x264.WEB-DL.2024",
		"720p.HDTV.x265.HEVC",
		"",
		"   ",
	}
	for _, in := range tests {
		c := Canonicalise(in)
		if c.Kind != KindUnknown {
			t.Errorf("input %q: got Kind=%v, want KindUnknown", in, c.Kind)
		}
	}
}

func TestCanonicalise_MusicWithMarker(t *testing.T) {
	tests := []struct {
		in            string
		wantArtist    string
		wantAlbum     string
		wantYear      int
		wantKindMusic bool
	}{
		{"Pink Floyd - The Wall (1979) [FLAC]", "pink floyd", "the wall", 1979, true},
		{"Radiohead - OK Computer [FLAC]", "radiohead", "ok computer", 0, true},
		// Without a music marker, can't tell music from movie:
		{"Some Artist - Some Album (2020)", "some artist", "some album", 2020, false},
	}
	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			c := Canonicalise(tt.in)
			if (c.Kind == KindMusic) != tt.wantKindMusic {
				t.Errorf("Kind: got %v, want music=%v", c.Kind, tt.wantKindMusic)
			}
			if c.Title != tt.wantArtist {
				t.Errorf("Title (artist): got %q want %q", c.Title, tt.wantArtist)
			}
			if tt.wantKindMusic && c.AlbumHint != tt.wantAlbum {
				t.Errorf("AlbumHint: got %q want %q", c.AlbumHint, tt.wantAlbum)
			}
		})
	}
}

func TestCanonicalKey_StableFormatting(t *testing.T) {
	tests := []struct {
		c    Canonical
		want string
	}{
		{Canonical{Kind: KindTV, Title: "the wire", Season: 1}, "tv:the wire:s01"},
		{Canonical{Kind: KindTV, Title: "the wire", Season: 12}, "tv:the wire:s12"},
		{Canonical{Kind: KindTV, Title: "the wire", Season: -1}, "tv:the wire"},
		{Canonical{Kind: KindMovie, Title: "inception", Year: 2010}, "mv:inception:2010"},
		{Canonical{Kind: KindMovie, Title: "inception"}, "mv:inception"},
		{Canonical{Kind: KindMusic, Title: "pink floyd", AlbumHint: "the wall"}, "ms:pink floyd:the wall"},
		{Canonical{Kind: KindMusic, Title: "pink floyd"}, "ms:pink floyd"},
		{Canonical{Kind: KindUnknown}, ""},
	}
	for _, tt := range tests {
		if got := canonicalKey(tt.c); got != tt.want {
			t.Errorf("canonicalKey(%+v): got %q want %q", tt.c, got, tt.want)
		}
	}
}

func TestCanonicalKeys_TVHasFallback(t *testing.T) {
	// TV with a season number gets indexed under BOTH the season-
	// specific key AND the title-only key so a torrent without a
	// season marker can still flag.
	c := Canonical{Kind: KindTV, Title: "the wire", Season: 1}
	keys := canonicalKeys(c)
	want := map[string]bool{
		"tv:the wire:s01": false,
		"tv:the wire":     false,
	}
	for _, k := range keys {
		if _, ok := want[k]; !ok {
			t.Errorf("unexpected key %q", k)
			continue
		}
		want[k] = true
	}
	for k, v := range want {
		if !v {
			t.Errorf("missing expected key %q", k)
		}
	}
}

func TestCanonicalKeys_MovieAndMusicHaveSingleKey(t *testing.T) {
	mov := Canonical{Kind: KindMovie, Title: "inception", Year: 2010}
	if got := len(canonicalKeys(mov)); got != 1 {
		t.Errorf("movie canonicalKeys count: got %d want 1", got)
	}
	mus := Canonical{Kind: KindMusic, Title: "pink floyd", AlbumHint: "the wall"}
	if got := len(canonicalKeys(mus)); got != 1 {
		t.Errorf("music canonicalKeys count: got %d want 1", got)
	}
	un := Canonical{Kind: KindUnknown}
	if got := len(canonicalKeys(un)); got != 0 {
		t.Errorf("unknown canonicalKeys count: got %d want 0", got)
	}
}

func TestExtractSeasonEpisode_MultiEpisode(t *testing.T) {
	season, eps, idx := extractSeasonEpisode([]string{"the", "show", "s02e01e02e03", "extra"})
	if season != 2 {
		t.Errorf("season: got %d want 2", season)
	}
	if !reflect.DeepEqual(eps, []int{1, 2, 3}) {
		t.Errorf("eps: got %v want [1 2 3]", eps)
	}
	if idx != 2 {
		t.Errorf("idx: got %d want 2", idx)
	}
}

func TestExtractYear_BoundsAndDigits(t *testing.T) {
	tests := []struct {
		tokens []string
		want   int
	}{
		{[]string{"my", "movie", "2024"}, 2024},
		{[]string{"my", "movie", "1899"}, 0}, // out of range
		{[]string{"my", "movie", "2099"}, 2099},
		{[]string{"my", "movie", "2100"}, 0}, // out of range
		{[]string{"my", "12345", "movie"}, 0},
		{[]string{"my", "12a4", "movie"}, 0},
		{[]string{"no", "year"}, 0},
	}
	for _, tt := range tests {
		got, _ := extractYear(tt.tokens)
		if got != tt.want {
			t.Errorf("tokens %v: got year %d want %d", tt.tokens, got, tt.want)
		}
	}
}

package parsers

import (
	"testing"

	"github.com/spencercnorton/bitagent/internal/classifier/classification"
	"github.com/spencercnorton/bitagent/internal/model"
	"github.com/stretchr/testify/assert"
)

// TestParseVideoContentBaseTitle locks in the title/year that ParseVideoContent
// hands to the TMDB search. The messy cases below are real unmatched movies
// pulled from the live index (2026-07-01) that are present in TMDB but were
// failing to match because BaseTitle carried site-prefix or dash-slug noise.
func TestParseVideoContentBaseTitle(t *testing.T) {
	t.Parallel()

	movie := model.NullContentType{Valid: true, ContentType: model.ContentTypeMovie}

	cases := []struct {
		name      string
		torrent   string
		ct        model.NullContentType
		wantTitle string
		wantYear  model.Year
	}{
		// ── the regressions we must not break ──
		{"plain movie", "The Regular Movie (2000).mkv", movie, "The Regular Movie", 2000},
		{"dotted movie", "Batman.Begins.2005.1080p.BluRay.x264", movie, "Batman Begins", 2005},
		{"hyphenated real title stays matchable", "Spider-Man.2002.1080p.BluRay", movie, "Spider Man", 2002},
		{"live false positive normalises hyphen", "Shang-Chi.and.the.Legend.of.the.Ten.Rings.2021.UHD.BluRay.2160p", movie, "Shang Chi and the Legend of the Ten Rings", 2021},

		// ── site / tracker prefix junk ──
		{"www dash spaces prefix", "www.UIndex.org    -    The Awakening 2011 1080p AMZN WEB-DL H264-GPRS", movie, "The Awakening", 2011},
		{"www single-space prefix", "www.Torrenting.com - Fast.Times.at.Ridgemont.High.1982.REMASTERED.720p.BluRay", movie, "Fast Times at Ridgemont High", 1982},
		{"scenetime prefix", "www.SceneTime.com - Batman Mask of the Phantasm 1993 1080p BluRay x264", movie, "Batman Mask of the Phantasm", 1993},

		// ── dash-slug names ──
		{"dash slug with year", "the-raid-redemption-2011", movie, "the raid redemption", 2011},

		// ── a domain that is NOT a prefix (no trailing dash) must be preserved ──
		{"domain-like title kept", "Something.com.2020.1080p.WEBRip", movie, "Something com", 2020},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			attrs, err := ParseVideoContent(
				model.Torrent{Name: tc.torrent},
				classification.Result{ContentAttributes: classification.ContentAttributes{ContentType: tc.ct}},
			)
			assert.NoError(t, err)
			assert.Equal(t, tc.wantTitle, attrs.BaseTitle.String, "BaseTitle")
			assert.Equal(t, tc.wantYear, attrs.Date.Year, "Year")
		})
	}
}

// TestParseVideoContentXFormatEpisodes locks in correct parsing of the
// NNxNN / S?NNxNN episode notation and guards against known regressions
// (standard SxxExx, season ranges, resolution strings).
func TestParseVideoContentXFormatEpisodes(t *testing.T) {
	t.Parallel()

	tv := model.NullContentType{Valid: true, ContentType: model.ContentTypeTvShow}

	cases := []struct {
		name         string
		torrent      string
		ct           model.NullContentType
		wantEpisodes model.Episodes
		wantTitle    string
	}{
		// ── confirmed bug fixes: S-prefix x-separator ──
		{
			"S02x20 scooby",
			"Scooby-Doo et Compagnie - S02x20 - Les mines",
			tv,
			model.Episodes{2: {20: {}}},
			"Scooby Doo et Compagnie",
		},
		{
			"S17X02 doctor who uppercase X",
			"Doctor Who 04 S17X02 Shada",
			tv,
			model.Episodes{17: {2: {}}},
			"Doctor Who 04",
		},

		// ── existing formats that must not regress ──
		{
			"standard SxxExx",
			"Show.S01E05.Title",
			tv,
			model.Episodes{1: {5: {}}},
			"Show",
		},
		{
			"bare NNxNN Ted Lasso",
			"Ted.Lasso.2x09.AFC.Wimbledon",
			tv,
			model.Episodes{2: {9: {}}},
			"Ted Lasso",
		},
		{
			"x-format range",
			"Star.Trek.3x01-6.Episode",
			tv,
			model.Episodes{3: {1: {}, 2: {}, 3: {}, 4: {}, 5: {}, 6: {}}},
			"Star Trek",
		},

		// ── resolution strings must NOT be treated as episode notation ──
		{
			"1920x1080 not episode",
			"Show.S01E01.1920x1080.BluRay.mkv",
			tv,
			model.Episodes{1: {1: {}}}, // only S01E01 should be captured
			"Show",
		},
		{
			"1280x720 not episode",
			"Movie.2020.1280x720.BluRay.mkv",
			model.NullContentType{Valid: true, ContentType: model.ContentTypeMovie},
			nil,
			"Movie",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			attrs, err := ParseVideoContent(
				model.Torrent{Name: tc.torrent},
				classification.Result{ContentAttributes: classification.ContentAttributes{ContentType: tc.ct}},
			)
			assert.NoError(t, err)
			assert.Equal(t, tc.wantEpisodes, attrs.Episodes, "Episodes")
			assert.Equal(t, tc.wantTitle, attrs.BaseTitle.String, "BaseTitle")
		})
	}
}

// TestParseVideoContentReleaseGroup locks in that the anime fansub group —
// named in a LEADING [Group] bracket that cleanTitle strips before the
// trailing "-GROUP" scene extractor runs — is recovered into ReleaseGroup,
// and that ordinary scene "-GROUP" releases are unaffected.
func TestParseVideoContentReleaseGroup(t *testing.T) {
	t.Parallel()

	tv := model.NullContentType{Valid: true, ContentType: model.ContentTypeTvShow}
	movie := model.NullContentType{Valid: true, ContentType: model.ContentTypeMovie}

	cases := []struct {
		name         string
		torrent      string
		ct           model.NullContentType
		wantValid    bool
		wantReleaseG string
	}{
		// ── anime: known fansub bracket becomes the release group ──
		{"subsplease canonical", "[SubsPlease] Sousou no Frieren - 12 (1080p) [ABCD1234].mkv", tv, true, "SubsPlease"},
		{"erai-raws canonical", "[Erai-raws] Shingeki no Kyojin - 1090 [1080p][Multiple Subtitle]", tv, true, "Erai-raws"},
		// canonicalisation reaches attrs.ReleaseGroup end-to-end (not just lower-cased)
		{"lowercase spaced known group canonicalised", "[erai raws] Some Show - 05 [1080p]", tv, true, "Erai-raws"},
		// site prefix before the fansub bracket is stripped first, group still recovered
		{"site prefix then fansub", "www.Torrenting.com - [SubsPlease] Show - 05 (1080p)", tv, true, "SubsPlease"},

		// ── the guard: a genuine trailing scene "-GROUP" is never overridden ──
		// (movie/year shape retains the tail so InferVideoAttributes captures SPARKS
		// first; the anime fill must then be skipped)
		{"leading fansub + trailing scene group keeps trailing", "[SubsPlease] Some Movie 2020 1080p x264-SPARKS", movie, true, "SPARKS"},

		// ── non-anime scene "-GROUP" releases keep their trailing group ──
		{"scene trailing group preserved", "Batman.Begins.2005.1080p.BluRay.x264-SPARKS", movie, true, "SPARKS"},

		// ── precision: generic / unlisted / adult leading brackets are NOT groups ──
		{"unlisted latin group not extracted", "[NewGroup] Some Show - 137 [1080p]", tv, false, ""},
		{"western encoder dual-audio not a group", "[Tigole] Movie 2019 Dual-Audio 1080p", movie, false, ""},
		{"adult studio scene-numbered not a group", "[Brazzers] Scene Title - 12", tv, false, ""},
		{"cjk leading bracket not a group", "[某组] Show - 25 [Multiple Subtitle]", tv, false, ""},

		// ── non-anime, no group at all ──
		{"plain movie no group", "The Regular Movie (2000).mkv", movie, false, ""},
		{"leading req tag not a group", "[REQ] The Regular Movie (2000) 1080p", movie, false, ""},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			attrs, err := ParseVideoContent(
				model.Torrent{Name: tc.torrent},
				classification.Result{ContentAttributes: classification.ContentAttributes{ContentType: tc.ct}},
			)
			assert.NoError(t, err)
			assert.Equal(t, tc.wantValid, attrs.ReleaseGroup.Valid, "ReleaseGroup.Valid")
			assert.Equal(t, tc.wantReleaseG, attrs.ReleaseGroup.String, "ReleaseGroup")
		})
	}
}

// TestYearRangeCollapse verifies that year ranges in release names are
// correctly collapsed to the anchor (first) year, and that the movie-pack
// guard prevents single-movie attachment for film collections.
func TestYearRangeCollapse(t *testing.T) {
	t.Parallel()

	tv := model.NullContentType{Valid: true, ContentType: model.ContentTypeTvShow}
	movie := model.NullContentType{Valid: true, ContentType: model.ContentTypeMovie}

	cases := []struct {
		name        string
		torrent     string
		ct          model.NullContentType
		wantTitle   string
		wantYear    model.Year
		wantEpisode bool // true = expect at least one episode/season parsed
	}{
		// ── year-range TV series packs must resolve to anchor year ──
		{
			"peep show hyphen range",
			"Peep Show (2003-2015) S01-S09 720p WEB-DL H265 BONE",
			tv, "Peep Show", 2003, true,
		},
		{
			"peep show en-dash range",
			"Peep Show (2003–2015) S01-S09 720p WEB-DL H265 BONE",
			tv, "Peep Show", 2003, true,
		},
		{
			"open-ended range present",
			"The Office (2005-Present) S01-S09 HDTV x264",
			tv, "The Office", 2005, true,
		},

		// ── single-year-in-parens must be UNTOUCHED ──
		{
			"single year parens untouched",
			"Firefly (2002) S01 720p BluRay",
			tv, "Firefly", 2002, true,
		},

		// ── plain dotted movie must be UNTOUCHED ──
		{
			"dotted movie untouched",
			"Batman.Begins.2005.1080p.BluRay.x264",
			movie, "Batman Begins", 2005, false,
		},

		// ── numeric title with single year must not be treated as a range ──
		{
			"numeric title not a range",
			"1917 (2019) 1080p BluRay x264",
			movie, "1917", 2019, false,
		},

		// ── movie-pack guard: collection with year range → empty BaseTitle ──
		{
			"movie collection pack guard",
			"The Bourne Collection 2002-2007 1080p BluRay x264",
			movie, "", 0, false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			attrs, _ := ParseVideoContent(
				model.Torrent{Name: tc.torrent},
				classification.Result{ContentAttributes: classification.ContentAttributes{ContentType: tc.ct}},
			)
			assert.Equal(t, tc.wantTitle, attrs.BaseTitle.String, "BaseTitle")
			assert.Equal(t, tc.wantYear, attrs.Date.Year, "Year")
			if tc.wantEpisode {
				assert.NotEmpty(t, attrs.Episodes, "expected episode data")
			}
		})
	}
}

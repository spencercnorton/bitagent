package parsers

import (
	"testing"

	"github.com/spencercnorton/bitagent/internal/classifier/classification"
	"github.com/spencercnorton/bitagent/internal/model"
	"github.com/stretchr/testify/require"
)

func parseV2(t *testing.T, name string, v2 bool) (classification.ContentAttributes, error) {
	t.Helper()

	return ParseVideoContentWithOptions(
		model.Torrent{Name: name},
		classification.Result{},
		ParseOptions{NoiseV2: v2},
	)
}

func TestNoiseV2SitePrefixes(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name      string
		wantTitle string
		wantYear  model.Year
	}{
		// v1 already handles the classic www + allowlisted TLD; regression.
		{"www.Torrenting.com - The.Serpent.2020.720p.WEBRip", "The Serpent", 2020},
		// UIndex spaces around the dash; v1 regression.
		{"www.UIndex.org    -    Zombie.Creeping.Flesh.1980.720p.BluRay", "Zombie Creeping Flesh", 1980},
		// NEW: www + non-allowlisted TLD (.rsvp).
		{"www.1TamilMV.rsvp - Gandhi Talks (2026) 1080p WEB-DL", "Gandhi Talks", 2026},
		// NEW: non-www on an extended TLD.
		{"tamilblasters.la - Vettaiyan (2024) 1080p HQ", "Vettaiyan", 2024},
		// www host, no dash, a run of spaces.
		{"www.Torrenting.org       For All Mankind S01E09 MULTi 1080p WEB H264-CiELOS", "For All Mankind", 0},
		{"www.Torrenting.org       Ancient Aliens S14E10 1080p WEB h264-NiXON", "Ancient Aliens", 0},
	}

	for _, tc := range cases {
		attrs, err := parseV2(t, tc.name, true)
		require.NoError(t, err, tc.name)
		require.Equal(t, tc.wantTitle, attrs.BaseTitle.String, tc.name)
		require.Equal(t, tc.wantYear, attrs.Date.Year, tc.name)
	}
}

func TestNoiseV2CJKSpans(t *testing.T) {
	t.Parallel()

	attrs, err := parseV2(t, "【高清影视之家发布 www.BBQDDQ.com】龙卷风末日 The.Twisters.2024.1080p.WEB-DL", true)
	require.NoError(t, err)
	// The fullwidth span (incl. the embedded site tag) is gone; the embedded
	// Latin scene segment parses. The CJK alt-title remains ahead of the
	// Latin title — the year still anchors the parse.
	require.Equal(t, model.Year(2024), attrs.Date.Year)
	require.Contains(t, attrs.BaseTitle.String, "The Twisters")
}

func TestNoiseV2LeadingYearRescue(t *testing.T) {
	t.Parallel()

	// Movie: leading year + tech tail, no year elsewhere.
	attrs, err := parseV2(t, "2019.The.Irishman.1080p.WEB-DL.x264", true)
	require.NoError(t, err)
	require.Equal(t, "The Irishman", attrs.BaseTitle.String)
	require.Equal(t, model.Year(2019), attrs.Date.Year)
	require.Equal(t, model.ContentTypeMovie, attrs.ContentType.ContentType)

	// TV: leading year with an episode anchor — year moves out of the title.
	attrs, err = parseV2(t, "2019.Some.Series.S01E01.720p.HDTV", true)
	require.NoError(t, err)
	require.Equal(t, "Some Series", attrs.BaseTitle.String)
	require.Equal(t, model.Year(2019), attrs.Date.Year)
	require.Len(t, attrs.Episodes, 1)

	// Year-titled film WITH its own year keeps legacy behavior (2046 is the
	// title, 2004 the year) — the rescue must not fire.
	attrs, err = parseV2(t, "2046.2004.1080p.BluRay.x264", true)
	require.NoError(t, err)
	require.Equal(t, "2046", attrs.BaseTitle.String)
	require.Equal(t, model.Year(2004), attrs.Date.Year)
}

func TestNoiseV2OffIsByteIdentical(t *testing.T) {
	t.Parallel()

	names := []string{
		"www.Torrenting.com - The.Serpent.2020.720p.WEBRip",
		"www.UIndex.org    -    Zombie.Creeping.Flesh.1980.720p.BluRay",
		"www.1TamilMV.rsvp - Gandhi Talks (2026) 1080p WEB-DL",
		"tamilblasters.la - Vettaiyan (2024) 1080p HQ",
		"【高清影视之家发布 www.BBQDDQ.com】龙卷风末日 The.Twisters.2024.1080p.WEB-DL",
		"2019.The.Irishman.1080p.WEB-DL.x264",
		"2019.Some.Series.S01E01.720p.HDTV",
		"2046.2004.1080p.BluRay.x264",
		"Peep Show (2003-2015) S01-S09 720p WEB-DL",
		"[SubsPlease] Frieren - 28 (1080p) [ABCD1234].mkv",
		"The.Matrix.1999.1080p.BluRay.x264",
	}

	for _, name := range names {
		legacy, legacyErr := ParseVideoContent(model.Torrent{Name: name}, classification.Result{})
		off, offErr := parseV2(t, name, false)
		require.Equal(t, legacyErr, offErr, name)
		require.Equal(t, legacy, off, name)
	}
}

func TestNoiseV2DoesNotDisturbCleanNames(t *testing.T) {
	t.Parallel()

	// Flag ON must leave already-clean names' parses unchanged.
	for _, name := range []string{
		"The.Matrix.1999.1080p.BluRay.x264",
		"Peep Show (2003-2015) S01-S09 720p WEB-DL",
		"[SubsPlease] Frieren - 28 (1080p) [ABCD1234].mkv",
	} {
		legacy, legacyErr := ParseVideoContent(model.Torrent{Name: name}, classification.Result{})
		v2, v2Err := parseV2(t, name, true)
		require.Equal(t, legacyErr, v2Err, name)
		require.Equal(t, legacy, v2, name)
	}
}

// A www host needs a dash or a run of spaces after it. One space or a dot is
// not enough evidence of a site tag, and the name is left as it was.
func TestNoiseV2WWWPrefixNeedsASeparator(t *testing.T) {
	t.Parallel()

	for _, name := range []string{
		"www.Example.com Great Movie 2020 1080p WEB",
		"www.Example.com.Great.Movie.2020.1080p.WEB",
	} {
		require.Equal(t, name, stripSiteNoisePrefixV2(name), name)
	}
}

// An EP-numbered episode is TV, titled by what precedes the EP token. The
// digits must touch "EP", and an EP inside a bracketed tag is not an episode.
func TestNoiseV2AbsoluteEpisode(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name, title string
		year        int
	}{
		{"Some.Anime.EP1168.Episode.1168.1080p.CR.WEB-DL.JPN.AAC2.0.H.264-GRP.mkv", "Some Anime", 0},
		{"Some.Drama.2024.EP12.1080p.WEB-DL.x264-GRP", "Some Drama", 2024},
		{"[Group] Some Show EP07 [WEBDL] [1080p]", "Some Show", 0},
	} {
		attrs, err := ParseVideoContentWithOptions(model.Torrent{Name: tc.name}, classification.Result{}, ParseOptions{NoiseV2: true})
		require.NoError(t, err, tc.name)
		require.Equal(t, model.ContentTypeTvShow, attrs.ContentType.ContentType, tc.name)
		require.Equal(t, tc.title, attrs.BaseTitle.String, tc.name)
		require.Equal(t, model.Year(tc.year), attrs.Date.Year, tc.name)
		require.Empty(t, attrs.Episodes, tc.name) // absolute numbering has no season
	}

	for _, name := range []string{
		"Some Artist - Great Songs EP 2019",                 // a music EP and its year
		"Some Show Special [1920x1080p.EP001-151.END.hevc]", // EP inside a tag
	} {
		attrs, _ := ParseVideoContentWithOptions(model.Torrent{Name: name}, classification.Result{}, ParseOptions{NoiseV2: true})
		require.NotEqual(t, model.ContentTypeTvShow, attrs.ContentType.ContentType, name)
	}
}

func TestLatinTitleAfterCJK(t *testing.T) {
	t.Parallel()

	for in, want := range map[string]string{
		"范海辛 Van Helsing":                             "Van Helsing",
		"南方公园 South Park":                             "South Park",
		"美国队长3 Captain America Civil War":             "Captain America Civil War", // sequel digit stays with the CJK title
		"黑炮事件 [国语音轨 +简英字幕 ]The Black Cannon Incident": "The Black Cannon Incident",
		"国语中字La fille de d'Artagnan":                  "La fille de d'Artagnan", // Latin glued to CJK starts the title
		"노랑 머리 (Yellow Hair)":                         "Yellow Hair",
		// unchanged
		"进击的巨人":                           "进击的巨人",                           // CJK only
		"Plastic Tree スロウ (Nihon Ongaku)": "Plastic Tree スロウ (Nihon Ongaku)", // Latin first
		"临床13区 [4KHDR CN ]":               "临床13区 [4KHDR CN ]",               // bracketed tag, not a title
		"本命年 Snow":                        "本命年 Snow",                        // one Latin word is not enough
		"中文 13 14":                        "中文 13 14",                        // digits alone are not a title
		"双龙出手 2 Guns":                     "2 Guns",                          // one Latin word plus a number is
	} {
		require.Equal(t, want, latinTitleAfterCJK(in), in)
	}

	attrs, err := ParseVideoContentWithOptions(model.Torrent{Name: "南方公园.South.Park.S22E06.中英字幕.HDTVrip.720p"}, classification.Result{}, ParseOptions{NoiseV2: true})
	require.NoError(t, err)
	require.Equal(t, "South Park", attrs.BaseTitle.String)
}

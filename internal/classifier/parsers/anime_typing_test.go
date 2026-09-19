package parsers

import (
	"strings"
	"testing"

	"github.com/spencercnorton/bitagent/internal/classifier/classification"
	"github.com/spencercnorton/bitagent/internal/model"
)

func parseName(t *testing.T, name string) classification.ContentAttributes {
	t.Helper()
	attrs, err := ParseVideoContentWithOptions(
		model.Torrent{Name: name}, classification.Result{}, ParseOptions{NoiseV2: true})
	if err != nil {
		t.Fatalf("parse %q: %v", name, err)
	}
	return attrs
}

// TestAnimeFansubTypesAsVideo is the whole point of the change: these are real
// production release names that the classifier typed as `unknown`, which the
// operator's norvi[1] rule then deleted. Every one must now type as video.
func TestAnimeFansubTypesAsVideo(t *testing.T) {
	for _, tc := range []struct {
		name      string
		wantType  model.ContentType
		wantTitle string
	}{
		{"[SubsPlease] One Piece - 1077 (480p) [3FC90F00].mkv", model.ContentTypeTvShow, "One Piece"},
		{"[Erai-raws] Towa no Yugure - 09 [720p HIDIVE WEB-DL AVC AAC][D482EC01].mkv", model.ContentTypeTvShow, "Towa no Yugure"},
		{"[Ohys-Raws] Kusuriya no Hitorigoto - 17 (NTV 1280x720 x264 AAC).mp4", model.ContentTypeTvShow, "Kusuriya no Hitorigoto"},
		{"[HorribleSubs] Naruto Shippuuden - 141 [720p].mkv", model.ContentTypeTvShow, "Naruto Shippuuden"},
		{"[Erai-raws] Oshi no Ko 2nd Season - 11 [1080p AMZN WEBRip HEVC EAC3][MultiSub][9C9E5A43].mkv", model.ContentTypeTvShow, "Oshi no Ko 2nd Season"},
		// AMBIGUOUS: no episode, no batch, no film marker. Typed tv_show purely
		// so norvi[1] does not delete them, and deliberately given NO title so
		// they are never searched — Kotonoha no Niwa (The Garden of Words) is a
		// film and Hinamatsuri is a TV series, and nothing in either name says
		// which. Jeeves round 4. See
		// TestAnimeAmbiguousReleasesAreKeptButNotSearched.
		{"[Beatrice-Raws] Kotonoha no Niwa [BDRip 1920x1080 HEVC DTSHD].mkv", model.ContentTypeTvShow, ""},
		{"[HorribleSubs] Hinamatsuri [720p]", model.ContentTypeTvShow, ""},
		// batches are TV, never films
		{"[Coalgirls]_Soul_Eater_27-39_(1280x720_Blu-ray_FLAC)", model.ContentTypeTvShow, "Soul Eater"},
		{"[SubsPlease] Kizoku Tensei - Megumareta Umare kara Saikyou no Chikara wo Eru (01-12) (480p) [Batch]", model.ContentTypeTvShow, "Kizoku Tensei - Megumareta Umare kara Saikyou no Chikara wo Eru"},
		// an EXPLICIT film marker is required to say movie
		{"[Erai-raws] Top o Nerae! - Movie [720p][4F7C0410].mkv", model.ContentTypeMovie, "Top o Nerae! - Movie"},
	} {
		got := parseName(t, tc.name)
		if !got.ContentType.Valid || got.ContentType.ContentType != tc.wantType {
			t.Errorf("%q\n  contentType = %v (valid=%v), want %v",
				tc.name, got.ContentType.ContentType, got.ContentType.Valid, tc.wantType)
		}
		if got.BaseTitle.String != tc.wantTitle {
			t.Errorf("%q\n  baseTitle = %q, want %q", tc.name, got.BaseTitle.String, tc.wantTitle)
		}
	}
}

// TestAnimeRescueNeverEmitsJunkSearchTitle is the cost-and-accuracy guard.
// A rescued row with a mangled BaseTitle fires a live TMDB query per release
// (action_attach_tmdb_content_by_search) that cannot match and, with fuzzy and
// alt-title matching enabled, can MIS-match. Empty is acceptable; junk is not.
func TestAnimeRescueNeverEmitsJunkSearchTitle(t *testing.T) {
	names := []string{
		"[SubsPlease] One Piece - 1077 (480p) [3FC90F00].mkv",
		"[Erai-raws] Digimon Beatbreak - 13 (ITA) [1080p AMZN WEB-DL HEVC EAC3][075A5C4F].mkv",
		"[Ohys-Raws] Eighty-Six Part 2 - 03 (BS11 1280x720 x264 AAC).mp4",
		"[ASW] Kamonohashi Ron no Kindan Suiri - 02 [1080p HEVC][2B43980A].mkv",
		"[SubsPlease] Kizoku Tensei - Megumareta Umare kara Saikyou no Chikara wo Eru (01-12) (480p) [Batch]",
		"[Coalgirls]_Soul_Eater_27-39_(1280x720_Blu-ray_FLAC)",
		"[Erai-raws] Boku no Hero Academia Final Season - More [1080p CR WEBRip HEVC AAC][MultiSub][8FF555D4].mkv",
	}
	banned := []string{"[", "]", "(", ")", ".mkv", ".mp4", "1080p", "720p", "480p", "x264", "x265", "hevc", "web-dl", "webrip", "bdrip", "multisub", "aac", "eac3", "flac"}
	for _, n := range names {
		got := parseName(t, n)
		title := strings.ToLower(got.BaseTitle.String)
		if title == "" {
			continue // explicitly allowed: no search beats a bad search
		}
		for _, b := range banned {
			if strings.Contains(title, b) {
				t.Errorf("%q\n  baseTitle %q still contains technical noise %q — this becomes a live TMDB query",
					n, got.BaseTitle.String, b)
			}
		}
	}
}

// TestAnimeRescueDoesNotRescueAdult locks the porn-safety property. These are
// real adult releases that carry a leading Latin bracket, so IsAnime() would
// rescue them; IsKnownFansub() must not.
func TestAnimeRescueDoesNotRescueAdult(t *testing.T) {
	for _, n := range []string{
		"[PKF Studios] Ivy Wolfe - Hard Bargain (2019) 1080p.mp4",
		"[CzechGav] Scene 12 [1080p].mp4",
		"[ero-teca.blogspot.com] Some Release [720p].mkv",
		"[Digital Playground] Another Robby D",
		"[AluraJensonXXX] Alura Jenson Threesome with Karen Fisher (08.09.2016) rq.mp4",
	} {
		got := parseName(t, n)
		if got.ContentType.Valid && got.ContentType.ContentType == model.ContentTypeTvShow {
			t.Errorf("adult release rescued as tv_show by the anime path: %q", n)
		}
	}
}

// TestNonAnimeTypingUnchanged proves the new switch case is additive: names
// that already typed correctly must be byte-identical afterwards.
func TestNonAnimeTypingUnchanged(t *testing.T) {
	for _, tc := range []struct {
		name string
		want model.ContentType
	}{
		{"American.Dad.S17E13.WEBRip.x264-BAE.mkv", model.ContentTypeTvShow},
		{"The.Matrix.1999.1080p.BluRay.x264.mkv", model.ContentTypeMovie},
		{"Breaking.Bad.S05E14.1080p.WEB-DL.mkv", model.ContentTypeTvShow},
		{"Blade.Runner.2049.2017.2160p.UHD.mkv", model.ContentTypeMovie},
	} {
		got := parseName(t, tc.name)
		if !got.ContentType.Valid || got.ContentType.ContentType != tc.want {
			t.Errorf("%q typed %v, want %v (regression in existing behaviour)",
				tc.name, got.ContentType.ContentType, tc.want)
		}
	}
}

// TestKnownFansubConventionalNameKeepsItsTitle is the Jeeves round-2 HIGH
// finding: a known fansub group also publishes conventional SxxExx names. Those
// are typed by an EARLIER switch branch and their title is already correct —
// overriding it with CleanTitle (which only strips the anime absolute-episode
// shape) would leave "Show S01E02" as the live TMDB query.
func TestKnownFansubConventionalNameKeepsItsTitle(t *testing.T) {
	for _, tc := range []struct {
		name      string
		wantTitle string
	}{
		{"[SubsPlease] Frieren S01E02 [1080p][MultiSub][ABCD1234].mkv", "Frieren"},
		{"[Erai-raws] Vinland Saga S02E11 [1080p][Multiple Subtitle].mkv", "Vinland Saga"},
		{"[Ohys-Raws] Bocchi the Rock S01E05 (BS11 1280x720 x264 AAC).mp4", "Bocchi the Rock"},
	} {
		got := parseName(t, tc.name)
		if got.BaseTitle.String != tc.wantTitle {
			t.Errorf("%q\n  baseTitle = %q, want %q (conventional SxxExx title was overwritten)",
				tc.name, got.BaseTitle.String, tc.wantTitle)
		}
		if !got.ContentType.Valid || got.ContentType.ContentType != model.ContentTypeTvShow {
			t.Errorf("%q typed %v, want tv_show", tc.name, got.ContentType.ContentType)
		}
		if len(got.Episodes) == 0 {
			t.Errorf("%q lost its parsed episodes", tc.name)
		}
	}
}

// TestAnimeAmbiguousReleasesAreKeptButNotSearched covers a review
// finding: a fansub release with no episodic evidence and no film marker is
// typed tv_show only so it survives the operator's contentType delete rule.
// That type is a survival default, not a finding, so it must NOT be paired with
// a BaseTitle — a valid BaseTitle admits the torrent to the TMDB search, and
// the search path is chosen by ContentType. Measured on the survivor corpus:
// 48 of 5,075 known-fansub rows, roughly 40% of them genuinely films.
func TestAnimeAmbiguousReleasesAreKeptButNotSearched(t *testing.T) {
	// Genuinely films and genuinely TV, indistinguishable from the name alone.
	ambiguous := []string{
		"[Anime Time] The Boy and the Heron (2023) [1080p][Dual Audio][HEVC 10bit x265][AAC][Multiple Sub].mkv",
		"[EMBER] Drifting Home (2022) [BDRip] [1080p Dual Audio HEVC 10 bits DDP].mkv",
		"[Yameii] Fate stay night - Heaven's Feel III. Spring Song (2020) [English Dub] [CR WEB-DL 1080p] [2C3C5409].mkv",
		"[Commie] Ghost in the Shell SAC_2045 [BD 1080p TrueHD]",
		"[VARYG] Dorohedoro (2026) [WEB-DL 1080p x264 E-AC-3]",
		"[Erai-raws] Night Head 2041 [720p]",
	}
	for _, name := range ambiguous {
		t.Run(name, func(t *testing.T) {
			a, err := ParseVideoContentWithOptions(
				model.Torrent{Name: name}, classification.Result{}, ParseOptions{NoiseV2: true})
			if err != nil {
				t.Fatalf("parse: %v", err)
			}
			// Kept: still typed, so norvi[1] does not delete it.
			if !a.ContentType.Valid || a.ContentType.ContentType != model.ContentTypeTvShow {
				t.Errorf("want tv_show so it survives the delete rule, got %v", a.ContentType)
			}
			// Not searched: no BaseTitle, clean OR mangled.
			if a.BaseTitle.Valid && a.BaseTitle.String != "" {
				t.Errorf("ambiguous release must carry NO BaseTitle, got %q", a.BaseTitle.String)
			}
		})
	}

	// Contrast: evidence present => title IS supplied, so the search still runs.
	evidenced := map[string]string{
		"[SubsPlease] One Piece - 1077 (480p) [3FC90F00].mkv":  "One Piece",
		"[Coalgirls]_Soul_Eater_27-39_(1280x720_Blu-ray_FLAC)": "Soul Eater",
		"[Beatrice-Raws] Some Film [10 bit].mkv":               "Some Film",
	}
	for name, want := range evidenced {
		t.Run("evidenced/"+name, func(t *testing.T) {
			a, err := ParseVideoContentWithOptions(
				model.Torrent{Name: name}, classification.Result{}, ParseOptions{NoiseV2: true})
			if err != nil {
				t.Fatalf("parse: %v", err)
			}
			if a.BaseTitle.String != want {
				t.Errorf("BaseTitle = %q, want %q", a.BaseTitle.String, want)
			}
		})
	}
}

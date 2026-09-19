package anime

import (
	"strings"
	"testing"
)

// TestCleanTitle locks the contract: a confident title, or nothing. The wrong
// outcome is not "no title" — it is a mangled title reaching the TMDB search.
func TestCleanTitle(t *testing.T) {
	for _, tc := range []struct {
		name string
		want string
	}{
		// canonical fansub shape
		{"[SubsPlease] One Piece - 1077 (480p) [3FC90F00].mkv", "One Piece"},
		{"[Erai-raws] Towa no Yugure - 09 [720p HIDIVE WEB-DL AVC AAC][D482EC01].mkv", "Towa no Yugure"},
		{"[Ohys-Raws] Kusuriya no Hitorigoto - 17 (NTV 1280x720 x264 AAC).mp4", "Kusuriya no Hitorigoto"},
		{"[ASW] Tenmaku no Jaadugar - 02 [1080p HEVC][DA3ACF2B].mkv", "Tenmaku no Jaadugar"},
		// version suffix on the episode
		{"[SubsPlease] Saigo ni Hitotsu dake Onegai Shitemo Yoroshii Deshou ka - 01v2 (720p) [344234E4].mkv",
			"Saigo ni Hitotsu dake Onegai Shitemo Yoroshii Deshou ka"},
		// title containing its own " - " before the episode marker
		{"[Erai-raws] Grow Up Show - Himawari no Circus-dan - 03 [1080p CR WEB-DL AVC AAC][MultiSub][EDB2C88A].mkv",
			"Grow Up Show - Himawari no Circus-dan"},
		// season marker retained (it is part of how the series is listed)
		{"[Erai-raws] Oshi no Ko 2nd Season - 11 [1080p AMZN WEBRip HEVC EAC3][MultiSub][9C9E5A43].mkv",
			"Oshi no Ko 2nd Season"},
		// no episode: film / batch — cut at the first bracket instead
		{"[Beatrice-Raws] Kotonoha no Niwa [BDRip 1920x1080 HEVC DTSHD].mkv", "Kotonoha no Niwa"},
		{"[HorribleSubs] Hinamatsuri [720p]", "Hinamatsuri"},
		{"[Erai-raws] Top o Nerae! - Movie [720p][4F7C0410].mkv", "Top o Nerae! - Movie"},
		{"[SubsPlease] Kizoku Tensei - Megumareta Umare kara Saikyou no Chikara wo Eru (01-12) (480p) [Batch]",
			"Kizoku Tensei - Megumareta Umare kara Saikyou no Chikara wo Eru"},
		// underscore-separated; the batch range is not part of the title
		{"[Coalgirls]_Soul_Eater_27-39_(1280x720_Blu-ray_FLAC)", "Soul Eater"},
		// stacked leading brackets (only stripped while technical/known-group —
		// CleanTitle is reachable only when a known group is present)
		{"[Ohys-Raws][Raws] Some Title - 05 [1080p].mkv", "Some Title"},

		// TITLE QUALIFIERS MUST SURVIVE. Truncating at the first bracket turns
		// these into plausible-but-WRONG search titles, which is precisely the
		// misattachment this cleaner exists to prevent.
		{"[Ohys-Raws] Fate stay night [Unlimited Blade Works] - 12 (BS11 1280x720 x264 AAC).mp4",
			"Fate stay night [Unlimited Blade Works]"},
		{"[SubsPlease] [Oshi no Ko] - 11 (1080p) [F00DF00D].mkv", "[Oshi no Ko]"},
		{"[Erai-raws] Re Zero kara Hajimeru Isekai Seikatsu (Director's Cut) - 05 [1080p].mkv",
			"Re Zero kara Hajimeru Isekai Seikatsu (Director's Cut)"},

		// Review finding: short technical alternatives must not match
		// ordinary words by PREFIX. Each of these was destroyed or truncated
		// while technicalTokenRe was anchored only at the start.
		{"[SubsPlease] [Engage Kiss] - 01 (1080p) [ABCD1234].mkv", "[Engage Kiss]"}, // eng? vs "Engage"
		{"[SubsPlease] Pokemon [Indigo League] - 05 (720p) [F00D1234].mkv",
			"Pokemon [Indigo League]"}, // ind vs "Indigo"
		{"[Ohys-Raws] Haruhi (Endless Eight) - 03 (BS11 1280x720 x264 AAC).mp4",
			"Haruhi (Endless Eight)"}, // en vs "Endless"
		{"[SubsPlease] Show [Crunchyroll Collection] - 02 (1080p) [11112222].mkv",
			"Show [Crunchyroll Collection]"}, // cr vs "Crunchyroll"
		{"[Erai-raws] Rawhide Riders - 07 [1080p][MultiSub][AABBCCDD].mkv", "Rawhide Riders"}, // raw vs "Rawhide"

		// Review finding: MULTI-WORD technical tags. Splitting on spaces
		// destroyed these before they could match, so "[10 bit]" scored 0/2 and
		// was kept as title — putting the noise into the live TMDB search.
		{"[Beatrice-Raws] Some Film [10 bit].mkv", "Some Film"},
		{"[SubsPlease] Some Show [Dual Audio].mkv", "Some Show"},
		{"[Erai-raws] Another Show [Blu Ray].mkv", "Another Show"},
		{"[Ohys-Raws] Third Show [WEB DL].mp4", "Third Show"},
		{"[Beatrice-Raws] Fourth Show [1080p 10 bit HEVC].mkv", "Fourth Show"},

		// Review finding: FULL-language + role tags. `eng?` is anchored, so
		// "English" never matched and "[English Dub]" scored 1/2 — under the
		// majority, kept as title. Production proof: CleanTitle was returning
		// "OVERLORD - The Sacred Kingdom (2024) [English Dub]" and searching
		// TMDB on it.
		{"[Yameii] OVERLORD - The Sacred Kingdom (2024) [English Dub] [CR WEB-DL 720p] [18859BC1].mkv",
			"OVERLORD - The Sacred Kingdom (2024)"},
		// explicit-movie and batch shapes, which Jeeves noted are the ones that
		// reach the trailing tags (absolute-episode names are cut before them)
		{"[Yameii] Some Film - Movie [English Dub] [1080p].mkv", "Some Film - Movie"},
		{"[SubsPlease] Some Show (01-12) [English Sub] [1080p] [Batch]", "Some Show"},
		{"[Erai-raws] Another Show [English Audio] [1080p]", "Another Show"},

		// MUST return "" — anything less than a confident title
		{"", ""},
		{"[SubsPlease] [1080p].mkv", ""},
		{"[SubsPlease] 12 - 05 [x264].mkv", ""},
		{"[SubsPlease] ab - 03 [720p].mkv", ""},
	} {
		if got := CleanTitle(tc.name, Detect(tc.name)); got != tc.want {
			t.Errorf("CleanTitle(%q)\n  got  %q\n  want %q", tc.name, got, tc.want)
		}
	}
}

// TestCleanTitleNeverReturnsBracketNoise is the load-bearing property: whatever
// comes back must never contain the technical noise that made the raw parse
// dangerous. A regression here means junk reaches the live TMDB search.
func TestCleanTitleNeverReturnsBracketNoise(t *testing.T) {
	names := []string{
		"[SubsPlease] One Piece - 1077 (480p) [3FC90F00].mkv",
		"[Erai-raws] Digimon Beatbreak - 13 (ITA) [1080p AMZN WEB-DL HEVC EAC3][075A5C4F].mkv",
		"[Ohys-Raws] Eighty-Six Part 2 - 03 (BS11 1280x720 x264 AAC).mp4",
		"[Beatrice-Raws] Kotonoha no Niwa [BDRip 1920x1080 HEVC DTSHD].mkv",
		"[Coalgirls]_Soul_Eater_27-39_(1280x720_Blu-ray_FLAC)",
	}
	for _, n := range names {
		got := CleanTitle(n, Detect(n))
		if got == "" {
			continue
		}
		for _, bad := range []string{"[", "]", "(", ")", ".mkv", ".mp4", "1080p", "720p", "480p", "x264", "HEVC", "WEB-DL"} {
			if containsFold(got, bad) {
				t.Errorf("CleanTitle(%q) = %q — still contains %q", n, got, bad)
			}
		}
	}
}

func containsFold(s, sub string) bool {
	return len(sub) > 0 && len(s) >= len(sub) && indexFold(s, sub) >= 0
}

func indexFold(s, sub string) int {
	ls, lsub := lower(s), lower(sub)
	for i := 0; i+len(lsub) <= len(ls); i++ {
		if ls[i:i+len(lsub)] == lsub {
			return i
		}
	}
	return -1
}

func lower(s string) string {
	b := []byte(s)
	for i := range b {
		if b[i] >= 'A' && b[i] <= 'Z' {
			b[i] += 32
		}
	}
	return string(b)
}

// TestIsBatchAndExplicitMovie locks the typing inputs. A batch classified as a
// film routes TMDB matching down the movie path, where it can attach an
// unrelated film — the failure mode review flagged.
func TestIsBatchAndExplicitMovie(t *testing.T) {
	for _, tc := range []struct {
		name         string
		wantBatch    bool
		wantExplicit bool
	}{
		{"[Coalgirls]_Soul_Eater_27-39_(1280x720_Blu-ray_FLAC)", true, false},
		{"[SubsPlease] Kizoku Tensei - X (01-12) (480p) [Batch]", true, false},
		{"[Group] Series Complete Series [1080p]", true, false},
		{"[Erai-raws] Top o Nerae! - Movie [720p][4F7C0410].mkv", false, true},
		{"[Group] Gekijouban Something [BDRip]", false, true},
		{"[SubsPlease] One Piece - 1077 (480p) [3FC90F00].mkv", false, false},
		{"[HorribleSubs] Hinamatsuri [720p]", false, false},
	} {
		s := Detect(tc.name)
		if got := IsBatch(tc.name, s); got != tc.wantBatch {
			t.Errorf("IsBatch(%q) = %v, want %v", tc.name, got, tc.wantBatch)
		}
		if got := IsExplicitMovie(tc.name); got != tc.wantExplicit {
			t.Errorf("IsExplicitMovie(%q) = %v, want %v", tc.name, got, tc.wantExplicit)
		}
	}
}

// TestCleanTitleLeavesNoBrackets is the blunt invariant behind the round-5 fix:
// whatever CleanTitle returns for a release carrying trailing metadata tags, it
// must not still contain a bracket. A bracket in the output means a technical
// tag was scored as title and is about to be sent to the live TMDB search.
func TestCleanTitleLeavesNoBrackets(t *testing.T) {
	for _, name := range []string{
		"[Yameii] OVERLORD - The Sacred Kingdom (2024) [English Dub] [CR WEB-DL 720p] [18859BC1].mkv",
		"[Yameii] Some Film - Movie [English Dub] [1080p].mkv",
		"[SubsPlease] Some Show (01-12) [English Sub] [1080p] [Batch]",
		"[Erai-raws] Another Show [English Audio] [1080p]",
		"[Beatrice-Raws] Some Film [10 bit].mkv",
		"[Ohys-Raws] Third Show [WEB DL].mp4",
		"[SubsPlease] One Piece - 1077 (480p) [3FC90F00].mkv",
	} {
		got := CleanTitle(name, Detect(name))
		if got == "" {
			continue // "" is the safe answer and is always allowed
		}
		if strings.ContainsAny(got, "[]") {
			t.Errorf("%q\n  CleanTitle = %q — a bracket survived into the TMDB query", name, got)
		}
	}
}

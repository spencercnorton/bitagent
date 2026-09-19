package anime

import "testing"

func TestDetect_KnownFansubGroup(t *testing.T) {
	cases := []string{
		"[SubsPlease] Sousou no Frieren - 12 (1080p) [ABCD1234].mkv",
		"[Erai-raws] Shingeki no Kyojin - 1090 [1080p][Multiple Subtitle]",
		"[HorribleSubs] One Piece - 900 [720p].mkv",
		"[Judas] Kimetsu no Yaiba S03 [BD]",
	}
	for _, name := range cases {
		s := Detect(name)
		if !s.IsKnownFansub() {
			t.Errorf("%q: IsKnownFansub=false, want true (FansubGroup=%q)", name, s.FansubGroup)
		}
		if !s.IsAnime() {
			t.Errorf("%q: IsAnime=false, want true", name)
		}
	}
}

func TestDetect_NativeScriptWithFansubGroupIsAnime(t *testing.T) {
	// The exact shape that the non-Latin-script filter was deleting.
	s := Detect("[SubsPlease] 葬送のフリーレン - 01 (1080p)")
	if !s.IsAnime() {
		t.Fatalf("CJK title with known fansub group must be detected as anime")
	}
	if !s.IsKnownFansub() {
		t.Errorf("known fansub group not detected in CJK title")
	}
}

func TestDetect_UnlistedLatinGroupPlusAbsoluteIsAnime(t *testing.T) {
	s := Detect("[NewGroup] Some Show - 137 [1080p]")
	if s.FansubGroup != "" {
		t.Errorf("NewGroup should not be a KNOWN group")
	}
	if s.LeadingLatinGroup == "" {
		t.Errorf("leading Latin group not captured")
	}
	if s.AbsoluteEpisode != 137 {
		t.Errorf("AbsoluteEpisode=%d, want 137", s.AbsoluteEpisode)
	}
	if !s.IsAnime() {
		t.Errorf("unlisted Latin group + absolute episode should be anime")
	}
	// But it is NOT a known fansub, so the porn-safe predicate is false.
	if s.IsKnownFansub() {
		t.Errorf("unlisted group must not report IsKnownFansub")
	}
}

func TestDetect_CJKRawGroupNotRescuedByGenericShape(t *testing.T) {
	// A raw with a CJK group bracket must NOT be treated as anime via the
	// generic-shape branch — LeadingLatinGroup stays empty for CJK brackets.
	s := Detect("[某组] 進撃の巨人 - 25")
	if s.LeadingLatinGroup != "" {
		t.Errorf("CJK bracket must not be a Latin group, got %q", s.LeadingLatinGroup)
	}
	if s.IsAnime() {
		t.Errorf("CJK raw with no English signal must not be rescued as anime")
	}
}

func TestDetect_AbsoluteEpisodeRejectsYearsAndResolutions(t *testing.T) {
	if got := Detect("Detective Conan - 2011 [1080p]").AbsoluteEpisode; got != 0 {
		t.Errorf("year 2011 wrongly parsed as episode %d", got)
	}
	if got := Detect("Some Movie - 1080 [x265]").AbsoluteEpisode; got != 0 {
		t.Errorf("resolution 1080 wrongly parsed as episode %d", got)
	}
	if got := Detect("[Erai-raws] Show - 12v2 [1080p]").AbsoluteEpisode; got != 12 {
		t.Errorf("versioned episode 12v2 -> %d, want 12", got)
	}
}

func TestDetect_EnglishTrackMarkers(t *testing.T) {
	for _, name := range []string{
		"Show S01 [Dual Audio]",
		"Show 2024 Multi-Audio 1080p",
		"[Group] Show - 05 [Multiple Subtitle]",
		"Show.2024.Eng.Sub.1080p",
	} {
		if !Detect(name).EnglishTrack {
			t.Errorf("%q: EnglishTrack=false, want true", name)
		}
	}
}

func TestDetect_NonAnimeIsNotAnime(t *testing.T) {
	for _, name := range []string{
		"The Batman 2022 1080p BluRay x264",
		"Movie [18+] 1080p",
		"Война и мир 2024",      // Cyrillic, no anime signal
		"Some.Documentary.2023", // plain
	} {
		if Detect(name).IsAnime() {
			t.Errorf("%q wrongly detected as anime", name)
		}
	}
}

func TestFansubGroupNames_SingleSourceConsistency(t *testing.T) {
	seen := map[string]string{}
	for _, g := range fansubGroupNames {
		key := normGroup(g)
		if key == "" {
			t.Errorf("allowlist entry %q normalises to empty", g)
			continue
		}
		if prev, dup := seen[key]; dup {
			t.Errorf("%q and %q normalise to the same key %q", prev, g, key)
			continue
		}
		seen[key] = g
		// Every entry must round-trip through bracket detection to its
		// canonical casing, so the allowlist, the detector and KnownGroup
		// can never drift.
		name := "[" + g + "] Some Show - 01 (1080p)"
		if !Detect(name).IsKnownFansub() {
			t.Errorf("Detect did not flag %q as a known fansub", g)
		}
		if got := KnownGroup(name); got != g {
			t.Errorf("KnownGroup(%q bracket) = %q, want %q", g, got, g)
		}
	}
	// The inference buckets must stay subsets of the allowlist, and disjoint —
	// a key in neither bucket gets the default English-sub fallback.
	for k := range rawFansubGroups {
		if _, ok := knownFansubGroups[k]; !ok {
			t.Errorf("rawFansubGroups key %q is not on the allowlist", k)
		}
		if _, dub := dubFansubGroups[k]; dub {
			t.Errorf("%q is in both the raw and dub buckets", k)
		}
	}
	for k := range dubFansubGroups {
		if _, ok := knownFansubGroups[k]; !ok {
			t.Errorf("dubFansubGroups key %q is not on the allowlist", k)
		}
	}
}

// TestNonEnglishSubGroupsStayOffTheAllowlist pins the deliberate EXCLUSIONS
// from the 2026-08 allowlist census. These groups lead thousands of real
// is_anime rows, so they look like allowlist gaps — but they ship Chinese
// (CHS/CHT), Spanish or Hungarian subtitles, not English. Listing a group
// makes the fallback in Detect stamp english_audio=sub over releases with no
// English track, and makes IsKnownFansub/IsAnime rescue even its markerless
// releases from the content filter's script drop — the exact rescue the
// foreign-audio rule's design comment forbids. They stay recognisable as
// anime through the unlisted-group path (leading Latin bracket + episode
// marker): kept, correctly unverdicted.
func TestNonEnglishSubGroupsStayOffTheAllowlist(t *testing.T) {
	cases := []struct{ label, name string }{
		{"Lilith-Raws (Baha CHT)", "[Lilith-Raws] Majutsushi Orphen Hagure Tabi S02 Kimluck Hen - 08 [Baha][WEB-DL][1080p][AVC AAC][CHT][MP4].mp4"},
		{"LoliHouse (CHS+CHT SRTx2)", "[LoliHouse] Seirei Gensouki S2 - 02 [WebRip 1080p HEVC-10bit AAC SRTx2].mkv"},
		{"Prejudice-Studio (CHS/CHT)", "[Prejudice-Studio] Bocchi the Rock! - 05 [WebRip 1080p HEVC-10bit AAC].mkv"},
		{"ANi (Baha CHT)", "[ANi] SPY×FAMILY 間諜家家酒 Season 3 - 49 [1080P][Baha][WEB-DL][AAC AVC][CHT].mp4"},
		{"Skymoon-Raws (ViuTV CHT)", "[Skymoon-Raws] Tensei Shitara Slime Datta Ken S04 - 77 [ViuTV][WEB-DL][CHT][SRT][1080p][AVC AAC].mkv"},
		{"jibaketa (Cantonese CHT)", "[jibaketa]Dandadan S2 - 02 (WEB 1920x1080 AVC AACx2 SRT MUSE CHT).mkv"},
		{"Kamigami (Chs,Cht,Jap)", "[Kamigami] Samurai Flamenco - 04 [1920×1080 x264 AAC Sub(Chs,Cht,Jap)].mkv"},
		{"CameEsp (Spanish-first)", "[CameEsp] Spy x Family (2023) - 09 [720p][ESP-ENG][mkv].mkv"},
		{"PuyaSubs! (Spanish)", "[PuyaSubs!] Digimon Adventure (2020) - 05 [1080p][C2C40040].mkv"},
		{"Naruto-Kun.Hu (Hungarian)", "[Naruto-Kun.Hu] Vinland Saga S2 - 13 [1080p].mkv"},
	}
	for _, tc := range cases {
		t.Run(tc.label, func(t *testing.T) {
			s := Detect(tc.name)
			if s.FansubGroup != "" {
				t.Errorf("non-English-sub group wrongly on the allowlist: FansubGroup=%q", s.FansubGroup)
			}
			if s.EnglishSubSignal || s.EnglishAudioSignal {
				t.Errorf("manufactured English evidence for a non-English release: %+v", s)
			}
			if !s.IsAnime() {
				t.Errorf("still anime via the unlisted-group path, got IsAnime=false")
			}
		})
	}
}

func TestKnownGroup(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		// ── known fansub groups: canonical display casing, wherever they sit ──
		{"known leading", "[SubsPlease] Sousou no Frieren - 12 (1080p) [ABCD1234].mkv", "SubsPlease"},
		{"known hyphenated canonicalised", "[Erai-raws] Shingeki no Kyojin - 1090 [1080p][Multiple Subtitle]", "Erai-raws"},
		{"known normalised spacing", "[erai raws] Some Show - 05 [1080p]", "Erai-raws"},
		{"known lowercase", "[subsplease] Show - 07 (720p)", "SubsPlease"},
		{"known in trailing bracket", "Some Show - 12 [SubsPlease]", "SubsPlease"},
		{"known with CJK title", "[SubsPlease] 葬送のフリーレン - 01 (1080p)", "SubsPlease"},

		// ── unlisted / generic leading brackets are deliberately NOT groups ──
		// (this is the porn-safe / precision boundary — see KnownGroup doc)
		{"unlisted latin + absolute ep", "[NewGroup] Some Show - 137 [1080p]", ""},
		{"unlisted latin + season marker", "[FreshSubs] Overlord 2nd Season - 05", ""},
		{"western encoder + dual audio", "[Tigole] Movie 2019 Dual-Audio 1080p", ""},
		{"western encoder + multi audio", "[MkvCage] Movie 2020 Multi Audio", ""},
		{"adult studio + scene number", "[Brazzers] Scene Title - 12", ""},
		{"out-of-list adult studio", "[TeamSkeet] Some Scene - 08 [1080p]", ""},
		{"format descriptor + number", "[Multi] Some Release - 12", ""},

		// ── rejected: no group signal ──
		{"unlisted latin no anime marker", "[REQ] Random Movie 1080p", ""},
		{"leading resolution tag no marker", "[1080p] Random Movie", ""},
		{"cjk leading bracket", "[某组] 進撃の巨人 - 25", ""},
		{"no leading bracket scene", "Batman.Begins.2005.1080p.BluRay.x264-SPARKS", ""},
		{"plain movie", "The Batman 2022 1080p BluRay x264", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := KnownGroup(tc.in); got != tc.want {
				t.Errorf("KnownGroup(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestDetect_SeasonMarkers(t *testing.T) {
	for _, name := range []string{
		"[Group] Attack on Titan The Final Season - 12",
		"[Group] Overlord 2nd Season - 05",
		"[Group] Re:Zero Cour 2 - 03",
	} {
		if !Detect(name).SeasonMarker {
			t.Errorf("%q: SeasonMarker=false, want true", name)
		}
	}
}

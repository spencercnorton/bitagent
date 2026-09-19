package anime

import "testing"

func TestForeignAudioOnly(t *testing.T) {
	drop := []string{
		"Evil.Dead.Burn.2026.1080p.WEBRip.Dublado.mkv",
		"Toy.Story.5.2026.1080p.WEBRip.Dublado.mkv",
		"'Allo 'Allo! 1982-1992 [720p.AC3.HDTV.x264-sy5ka][Lektor PL][Alusia]",
		"Abduction 2011 m1080p x264 AC3 TURKCE DUBLAJ.mkv",
		"Some.Show.S01.2020.1080p.Hindi.Dubbed.x264.mkv",
		"[LoliHouse] DanMachi S5 - 08 [WebRip 1080p HEVC-10bit AAC][CHT].mkv Castellano",
		"10.Lives.2024.PLDUB.1080p.AMZN.WEB-DL.DD5.1.H264-RX",
		// A curated subbing group must NOT manufacture an English signal over
		// an explicit foreign dub — the group inference is a fallback, and an
		// explicit advertisement wins in EITHER direction.
		"[SubsPlease] Some Show - 07 French Dubbed [1080p].mkv",
		"[Erai-raws] Some Show - 07 [1080p] Hindi Dubbed.mkv",
		// Standalone scene tokens nonEnglishTrackRe does NOT know — the group
		// fallback must be suppressed by these too.
		"[SubsPlease] Some Movie Dublado.mkv",
		"[SubsPlease] Some Movie TURKCE DUBLAJ.mkv",
		"[Erai-raws] Some Movie [Lektor PL].mkv",
		"[Judas] Some Movie TRUEFRENCH.mkv",
	}
	for _, n := range drop {
		if !ForeignAudioOnly(n) {
			t.Errorf("expected foreign-only: %q", n)
		}
	}

	keep := []string{
		// Multi-audio packs that DO carry English — the whole reason the
		// bare language names are not treated as evidence.
		"127.Hours.2010.1080p.WEB-DL.ENG.LATINO.CASTELLANO.POR.DDP.5.1.H264-BEN.THE.MEN",
		"10 Cloverfield Lane 2016 Bonus BR EAC3 VFF ENG 1080p x265 10Bits T0M",
		"Castle.Rock.S01.SweSub-EngSub.1080p.x264-Justiso",
		// MULTi means several audio tracks INCLUDING the original, so a
		// French dub is added alongside English, not instead of it. This was
		// the dominant false positive before MULTi counted as evidence.
		"The.Mandalorian.and.Grogu.2026.MULTi.TRUEFRENCH.iMAX.1080p.WEB.H264-SUPPLY.mkv",
		// English subs spelled EN rather than ENG.
		"Bullet (1996) [1080p] [WEB-DL] [AC3] [LEKTOR.PL] [NAPISY.PL.EN].mkv",
		// Foreign SUBTITLE markers leave the audio alone, so an English work
		// carrying them is watchable. The gold corpus caught this class.
		"Death of a Unicorn (2025) [BRRip.XviD-NN] [Napisy_PL].avi",
		"Oceans.Eight.2018.SweSub.1080p.x264-Justiso",
		"Bamboozled (2000)(NLsubs) TBS",
		"Le.Samourai.1967.VOSTFR.1080p.BluRay.x264.mkv",
		// A foreign SUBTITLE compound must still not make a release
		// foreign-AUDIO — the two predicates are combined differently by
		// design, and conflating them would drop watchable releases.
		"Some.English.Film.2024.1080p.Spanish.Subbed.x264.mkv",
		// Bare `turkce` means Turkish, not a Turkish DUB — this one
		// advertises Turkish SUBTITLES over untouched audio.
		"Movie.2024.1080p.WEB-DL.TURKCE.Altyazili.x264.mkv",
		"Digimon Tamers Movie 2 (2002) [VOSTFR] [ENG SubsDubs] (BD 720p X264 AAC).mkv",
		// English titles that merely contain a language word.
		"The Italian Job 2003 1080p BluRay HEVC x265 5.1 BONE.mkv",
		"The.French.Connection.1971.1080p.BluRay.x264.mkv",
		"Plain.Movie.2024.1080p.WEB-DL.x264.mkv",
		// A curated English subbing group is English evidence.
		"[SubsPlease] Shangri-La Frontier - 40 (1080p) [59400145].mkv",
	}
	for _, n := range keep {
		if ForeignAudioOnly(n) {
			t.Errorf("expected keep: %q", n)
		}
	}
}

// A curated group must not manufacture an English-SUBTITLE verdict over a name
// that explicitly advertises subtitles in another language. These are KEPT by
// ForeignAudioOnly (a foreign subtitle is not foreign audio) — the defect is
// the english_audio column recording "sub" as if it meant English.
func TestGroupFallbackYieldsToExplicitForeignSubtitles(t *testing.T) {
	for _, n := range []string{
		"[SubsPlease] Some Movie VOSTFR.mkv",
		"[SubsPlease] Some Movie SweSub.mkv",
		"[Erai-raws] Some Movie [Napisy PL].mkv",
		"[Judas] Some Movie Legendado.mkv",
		"[ASW] Some Movie NLsubs.mkv",
	} {
		d := Detect(n)
		if d.EnglishSubSignal || d.EnglishAudioSignal {
			t.Errorf("explicit foreign subtitle must suppress the group fallback: %q -> %+v", n, d)
		}
		if ForeignAudioOnly(n) {
			t.Errorf("a foreign SUBTITLE is not foreign audio; must be kept: %q", n)
		}
	}
}

func TestKnownSubbingGroupImpliesEnglishSubs(t *testing.T) {
	s := Detect("[SubsPlease] Tensei Shitara Ken Deshita - 08 (1080p) [0CFD4914].mkv")
	if !s.EnglishSubSignal || s.EnglishAudioSignal {
		t.Errorf("subbing group should imply subs, not dub: %+v", s)
	}
	if d := Detect("[Yameii] Some Show - 03 [1080p].mkv"); !d.EnglishAudioSignal {
		t.Errorf("Yameii is a dub group: %+v", d)
	}
	if r := Detect("[Ohys-Raws] Some Show - 03 (1080p).mkv"); r.EnglishSubSignal || r.EnglishAudioSignal {
		t.Errorf("a raws group advertises nothing: %+v", r)
	}
	// 2026-08 census additions, one per bucket. Prod-shaped names with no
	// explicit track markers, so the verdict comes from the fallback alone.
	if s := Detect("[Dynamis One] Grand Blue Season 3 - 07 (CR 1920x1080 AVC AAC MKV) [D81FF7AE].mkv"); !s.EnglishSubSignal || s.EnglishAudioSignal {
		t.Errorf("Dynamis One is a subbing group: %+v", s)
	}
	if s := Detect("[SubsMix] The Ogre's Bride - 03 (S01E03) - (WEB 1080p AVC x264 AAC 2.0).mkv"); !s.EnglishSubSignal || s.EnglishAudioSignal {
		t.Errorf("SubsMix is a subbing group: %+v", s)
	}
	if d := Detect("[Golumpa] My Hero Academia S4 - 04 (Boku no Hero Academia) [FuniDub 720p x264 AAC] [7B2D56CA].mkv"); !d.EnglishAudioSignal {
		t.Errorf("Golumpa rips English dubs (FuniDub carries no bare token the regexes see): %+v", d)
	}
	if d := Detect("[TRC] Some Show - S01 [CR WEB-RIP 1080p HEVC-10 AAC]"); !d.EnglishAudioSignal {
		t.Errorf("TRC rips English dubs: %+v", d)
	}
	if r := Detect("[NanakoRaws] Boku no Hero Academia S8 - 03 (NTV 1920x1080 x265 AAC).mkv"); r.EnglishSubSignal || r.EnglishAudioSignal {
		t.Errorf("NanakoRaws is a raw group, advertises nothing: %+v", r)
	}
	if r := Detect("[New-raws] Jigokuraku S2 - 11 [1080p] [AMZN].mkv"); r.EnglishSubSignal || r.EnglishAudioSignal {
		t.Errorf("New-raws is a raw group, advertises nothing: %+v", r)
	}
	// The fallback must stay out of the way of an explicit FOREIGN signal too,
	// or it manufactures English evidence that ForeignAudioOnly then trusts.
	if f := Detect("[SubsPlease] Some Show - 07 French Dubbed [1080p].mkv"); f.EnglishSubSignal || f.EnglishAudioSignal {
		t.Errorf("explicit foreign dub must suppress the group fallback: %+v", f)
	}
}

func TestDateTailIsNotAnAbsoluteEpisode(t *testing.T) {
	s := Detect("[IKnowThatGirl] Alyssa Cole (A Hard Fuck in Torn Stockings - 28.11.2016) rq.mp4")
	if s.AbsoluteEpisode != 0 {
		t.Errorf("date tail parsed as episode %d", s.AbsoluteEpisode)
	}
	if s.IsAnime() {
		t.Error("adult release must not read as anime")
	}
	if e := Detect("[SubsPlease] Show - 12 (1080p).mkv"); e.AbsoluteEpisode != 12 {
		t.Errorf("real absolute episode lost: %d", e.AbsoluteEpisode)
	}
}

// TestSceneDatedStudioReleasesAreNotAnime pins the prod shapes that survived
// dateTailRe (which only guards a date DIRECTLY after the absolute number):
// "Part N" scene titles reaching IsAnime via the season marker, and scene
// numbers before a parenthesized date. All names are real 2026-08-24 prod
// census rows. A dotted scene date marks the leading bracket as a studio
// tag, so no unlisted-group signal may be emitted.
func TestSceneDatedStudioReleasesAreNotAnime(t *testing.T) {
	adult := []string{
		"[BrazzersExxtra] Abella Danger - The Trip Part 1 (19.01.2020).mp4",
		"[RealWifeStories] Romi Rain - Hungry For Spring Breakers Part 1 (07.03.2019) rq",
		"[FirstAnalQuest] Helena Miles aka Carolina Star - 433 (13.02.2017)",
		"[WoodmanCastingX] Diana Rius - Casting X 194 - 2 (30.04.2023) rq.mp4",
		"[ExploitedCollegeGirls] Kai West - Anal - 20 Years Old (15.10.2020).mp4",
		"[ATKGirlFriends]Chloe Temple - Hawaii Part 2 [07.29.19].mp4",
		"[HotAndMean] Cassidy Banks, Jelena Jensen (Like A Mother - Part 3 - 22.03.2017) rq (1k).mp4",
	}
	for _, name := range adult {
		s := Detect(name)
		if s.LeadingLatinGroup != "" {
			t.Errorf("scene-dated name emitted unlisted-group signal %q: %s", s.LeadingLatinGroup, name)
		}
		if s.IsAnime() {
			t.Errorf("adult release must not read as anime: %s", name)
		}
	}

	// Real anime with the same markers and no dotted date keeps its signal.
	anime := []string{
		"[LoliHouse] One-Punch Man S3 - 02(26) [WebRip 1080p HEVC-10bit AAC SRTx2].mkv",
		"[DragsterPS] Rilakkuma and Kaoru S01 [1080p] [HEVC] [Multi-Audio] [Multi-Subs]",
		"[Feibanyama] Attack on Titan The Final Season Part 2 - 04 [WebRip 1080p].mkv",
	}
	for _, name := range anime {
		if !Detect(name).IsAnime() {
			t.Errorf("undated anime lost its signal: %s", name)
		}
	}
	// A JP-raw YYYY.MM.DD airdate is not a scene date — \b cannot sit
	// between two digits, so the order distinguishes the conventions.
	if s := Detect("[Zenkai] Show 2023.01.05 - 03 (1080p).mkv"); !s.IsAnime() {
		t.Errorf("YYYY.MM.DD airdate wrongly suppressed the group: %+v", s)
	}
	// The allowlist path is date-immune: known groups are disjoint from
	// adult studios by construction.
	if s := Detect("[SubsPlease] Show - 12 (13.02.2017).mkv"); !s.IsAnime() {
		t.Errorf("known fansub group must survive a dotted date: %+v", s)
	}
}

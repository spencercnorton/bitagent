package classifier

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/spencercnorton/bitagent/internal/model"
)

// The gate is worthless unless it runs BEFORE the model. Asserting only on the
// predicate would pass even if the call were never wired into decide(), so this
// fails the test server outright if the matcher endpoint is reached at all —
// which also proves the gate costs zero API spend on the 16,007 matching rows.
func TestAdultShapeIsGatedBeforeModel(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Fatal("an adult scene release must not reach the matcher endpoint")
	}))
	t.Cleanup(srv.Close)

	tor := model.Torrent{
		Name:        "spyfam.17.05.01.aubrey.sinclair.mp4",
		Size:        2_000_000_000,
		FilesStatus: model.FilesStatusSingle,
		Extension:   model.NewNullString("mp4"),
	}
	dec, err := (matchRunner{lm: newLLMMatchClient(t, srv.URL, true)}).decide(
		context.Background(), tor, model.NewNullContentType(model.ContentTypeTvShow),
	)
	require.NoError(t, err)
	assert.Equal(t, OutcomeAdult, dec.Outcome,
		"this exact release attached to SPY x FAMILY (2022) in production")
}

// The adult-shape gate exists because the MODEL cannot police this class. It
// misreads an adult scene release as a mainstream title and clears its own
// IsAdult flag in the same breath, so ext.IsAdult never fires and the post-rerank
// identity gate then agrees with itself — extraction and candidate wrong together.
// The post-resolve year guard cannot see them either: it abstains for TV.
//
// Every "adult" name below is a REAL release that reached production with a wrong
// TMDB attachment; the comment names what it was attached to.
func TestAdultReleaseShape(t *testing.T) {
	adult := []struct{ name, attachedTo string }{
		{"spyfam.17.05.01.aubrey.sinclair.mp4", "SPY x FAMILY (2022)"},
		{"spyfam.18.11.19.melody.marks.4k", "SPY x FAMILY (2022)"},
		{"spyfam.24.04.06.ellie.nova.buff.stepdad.4k.mp4", "SPY x FAMILY (2022)"},
		{"frs.17.02.20.anie.darling[et].MP4.mp4", "DARLING in the FRANXX (2018)"},
		{"pml.16.05.20.charlotte.angel.hot.angel[et].mp4", "Charlotte (2015)"},
		{"ecg.14.07.03.charlotte.mp4", "Charlotte (2015)"},
		{"nvg.22.11.03.charlotte.4k.mp4", "Charlotte (2015)"},
		{"btra.16.12.08.anna.bell.peaks.mp4", "Twin Peaks (1990)"},
		{"pf.15.10.30.jodi.taylor.nerd.girls.6.mp4", "The Boys (2019)"},
		{"prdi.16.07.13.india.summer.the.hitchhiker[et].mp4", "The Hitchhiker's Guide to the Galaxy (1981)"},
		{"lcd.21.12.24.agatha.vega.caprice.divas.mp4", "Agatha All Along (2024)"},
		{"tto.16.01.08.dallas.black.mp4", "Dallas (2012)"},
		{"hegre.20.08.11.a.day.in.the.life.of.alya.extended.version.", "Alya Sometimes Hides Her Feelings in Russian (2024)"},
		// 'tds' looks like a talk-show abbreviation and is not one — sampled from
		// the same corpus, this is adult too. Prefix allowlists would get this wrong.
		{"tds.15.12.07.yhivi.mp4", "(sampled from the corpus)"},
	}
	for _, c := range adult {
		if !adultReleaseShape(c.name) {
			t.Errorf("adultReleaseShape(%q) = false, want true — this release attached to %s", c.name, c.attachedTo)
		}
	}

	// Legitimate releases the gate must never touch. The dated-TV cases are the
	// whole reason the SxxExx escape hatch exists: measured on the production
	// corpus, ZERO of the 16,007 shape matches carry episode notation, so keying
	// the exemption on it costs nothing today and keeps a real collision matchable.
	legit := []string{
		"Black.S01.2017.1080p.NF.WEBRip.DDP2.0.x265-RL",
		"Schitts.Creek.S06.WEBRip.720p.Idea Film",
		"RuPauls.Drag.Race.Untucked.S14E07.720p.HEVC.x265-MeGusta[eztv.re]",
		"Love.Simon.2018.1080p.BluRay.x264-GECKOS[EtHD]",
		"Hobbit.Pustosh.Smauga.2013.D.BDRip.1.45GB_New-team_by_Yarmak23",
		"L Armee des 12 Singes 1995 1080p FR EN X264 AC3-mHDgz.mkv",
		"2005 - El método [MPG2].mkv",
		"Frontier Marshal  (Western 1939)  Randolph Scott  720p",
		// A dated-TV release in the adult shape is EXEMPTED by episode notation —
		// the one collision the gate is deliberately built to survive.
		"tds.16.01.08.s01e02.jon.stewart.mp4",
		// Four-digit year is the normal dated-TV convention and never matches.
		"The.Daily.Show.2016.01.08.720p.WEB.mp4",
		// Uppercase is deliberately NOT matched, and this is not fastidiousness:
		// case-insensitive matching swallows live sports, which uses the identical
		// convention and carries no SxxExx to exempt it.
		"SPYFAM.17.05.01.aubrey.sinclair.mp4",
		"NHL.15.10.13.San Jose Sharks vs St. Louis Blues.540p.mkv",
		"NHL.19-05-08.West.Final.G6.DAL-DET.DivX.avi",
		// A purely numeric leading token is an upload date, not a studio — these are
		// real films whose TITLE starts with a number. All ten were in the corpus and
		// the shape alone would have discarded every one.
		"2011.01.03.12.Monkeys.1995.Blu-ray.x264.720P.DTS.MySilu",
		"09.02.04.21.Grams.2003.BDRE.1080p.x264.DTSHDC-MYSiLU",
		"07.04.23.50.First.Dates.Blue-ray.Remux.MPEG2.1080p.LPCM.DD51.Fanxy@Silu",
		"08.04.21.27.Dresses.Blu-ray.REMUX.H264.1080p.DTSHD-MA51.Silu",
		"2013.02.09.96.Minutes.2011.BluRay.1080p.x264.DTS-MySilu",
	}
	for _, name := range legit {
		if adultReleaseShape(name) {
			t.Errorf("adultReleaseShape(%q) = true, want false — gate must not swallow legitimate releases", name)
		}
	}
}

func TestAdultReleaseShapeEdges(t *testing.T) {
	cases := []struct {
		name string
		want bool
	}{
		{"", false},
		{"abbw.18.04.07.alba.and.kate.g.lesbian.mp4", true},
		// Studio tag longer than 8 chars is outside the measured convention.
		{"verylongstudio.18.04.07.someone.mp4", false},
		// Numeric-only leading token is an upload date, never a studio.
		{"2012.04.01.99.Francs.2007.BluRay.1080p.x264.DTS-MySilu", false},
		// A single letter in the tag is enough to make it a studio again.
		{"a1.18.04.07.someone.mp4", true},
		// Date triple must be exactly three 2-digit groups.
		{"ps.15.08.kimmy.granger.mp4", false},
		{"ps.2015.08.07.kimmy.granger.mp4", false},
		// Needs a trailing segment after the date.
		{"ps.15.08.07", false},
	}
	for _, c := range cases {
		if got := adultReleaseShape(c.name); got != c.want {
			t.Errorf("adultReleaseShape(%q) = %v, want %v", c.name, got, c.want)
		}
	}
}

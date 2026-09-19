package parsers

import (
	"encoding/csv"
	"os"
	"testing"

	"github.com/spencercnorton/bitagent/internal/anime"
	"github.com/spencercnorton/bitagent/internal/classifier/classification"
	"github.com/spencercnorton/bitagent/internal/model"
)

// TestZZAmbiguousCohort sizes the branch review flagged: a
// known-fansub release that reaches the inner switch's DEFAULT case — no
// episodic evidence and no explicit film marker — yet is typed tv_show and
// handed a clean BaseTitle, so it reaches the TV TMDB search.
//
// Measured through the REAL parser. Rows carrying SxxExx never reach the
// fansub branch at all: the outer `len(episodes) > 0` case types them first.
func TestZZAmbiguousCohort(t *testing.T) {
	p := os.Getenv("SURVIVORS")
	if p == "" {
		t.Skip("set SURVIVORS")
	}
	f, err := os.Open(p)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	cr := csv.NewReader(f)
	cr.FieldsPerRecord = 2
	cr.LazyQuotes = true

	var fansub, ambiguous, ambiguousWithTitle int
	var ex []string
	for {
		parts, rerr := cr.Read()
		if rerr != nil {
			break
		}
		name := parts[0]
		s := anime.Detect(name)
		if !s.IsKnownFansub() {
			continue
		}
		fansub++
		// Positive evidence => not the default case.
		if s.AbsoluteEpisode > 0 || s.SeasonMarker || anime.IsBatch(name, s) || anime.IsExplicitMovie(name) {
			continue
		}
		// PRODUCTION ORDERING. default[2] runs parse_date before default[4]
		// runs parse_video_content, so result.Date is already populated by the
		// time the fansub branch is reached. Without this the probe sends rows
		// to the fansub branch that production types via the earlier
		// `result.Date.IsValid()` case instead.
		prior := classification.Result{}
		if d := ParseDate(name); !d.IsNil() {
			prior.Date = d
		}
		a, perr := ParseVideoContentWithOptions(
			model.Torrent{Name: name}, prior, ParseOptions{NoiseV2: true})
		if perr != nil {
			continue
		}
		// An SxxExx name is typed by the OUTER episodes branch, not ours.
		if len(a.Episodes) > 0 {
			continue
		}
		ambiguous++
		if a.BaseTitle.Valid && a.BaseTitle.String != "" {
			ambiguousWithTitle++
			if len(ex) < 20 {
				ex = append(ex, a.BaseTitle.String+"   <<<   "+name)
			}
		}
	}
	t.Logf("known-fansub n=%d", fansub)
	t.Logf("  reach the DEFAULT (ambiguous) case: %d (%.2f%% of fansub)",
		ambiguous, 100*float64(ambiguous)/float64(fansub))
	t.Logf("  ...of those, given a BaseTitle -> hit the TV TMDB search: %d", ambiguousWithTitle)
	for _, e := range ex {
		t.Logf("   %s", e)
	}
}

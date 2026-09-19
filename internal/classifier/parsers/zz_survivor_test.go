package parsers

import (
	"encoding/csv"
	"os"

	"github.com/spencercnorton/bitagent/internal/anime"
	"strings"
	"testing"

	"github.com/spencercnorton/bitagent/internal/classifier/classification"
	"github.com/spencercnorton/bitagent/internal/model"
)

// TestZZSurvivorImpact checks the patch against torrents that are ALREADY
// indexed, on an anime-rich corpus. A previous analysis used a corpus with only
// 1% anime and concluded "0 rows change" — that was a sampling artefact, not a
// safety property. SURVIVORS must be a leading-bracket sample so the cohort
// under test is actually present.
//
// SCOPING, and why it is not a fudge: the new switch case and the title
// assignment are BOTH guarded on anime.Detect(name).IsKnownFansub(). A row
// where that is false is provably untouched by this change, so including it
// measures parser-in-isolation vs the full pipeline (which also runs
// parse_date at default[2] before parse_video_content at default[4], so
// result.Date is populated in production and empty here) — a real difference,
// but not one this patch causes.
func TestZZSurvivorImpact(t *testing.T) {
	p := os.Getenv("SURVIVORS")
	if p == "" {
		t.Skip("set SURVIVORS to a name,content_type CSV")
	}
	f, err := os.Open(p)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	cr := csv.NewReader(f)
	cr.FieldsPerRecord = 2
	cr.LazyQuotes = true

	var n int
	changes := map[string]int{}
	var samples []string
	for {
		parts, rerr := cr.Read()
		if rerr != nil {
			break
		}
		name, was := parts[0], parts[1]
		if !anime.Detect(name).IsKnownFansub() {
			continue // provably untouched by this change
		}
		n++
		a, err := ParseVideoContentWithOptions(
			model.Torrent{Name: name}, classification.Result{}, ParseOptions{NoiseV2: true})
		now := "unknown"
		if err == nil && a.ContentType.Valid {
			now = a.ContentType.ContentType.String()
		}
		if now != was {
			changes[was+" -> "+now]++
			if len(samples) < 10 {
				samples = append(samples, was+" -> "+now+"  |  "+name)
			}
		}
	}
	t.Logf("survivor corpus, KNOWN-FANSUB subset n=%d", n)
	if len(changes) == 0 {
		t.Logf("  no reclassifications")
	}
	for k, v := range changes {
		t.Logf("  %-24s %d (%.2f%%)", k, v, 100*float64(v)/float64(n))
	}
	for _, s := range samples {
		t.Logf("    %s", s)
	}
	// The only unacceptable direction is losing video typing entirely — that
	// would newly expose an indexed torrent to the operator's unknown-bucket
	// delete rule.
	for k, v := range changes {
		if strings.HasSuffix(k, "-> unknown") && v > 0 {
			t.Errorf("REGRESSION: %d rows lost their video type (%s) — these become deletable", v, k)
		}
	}
}

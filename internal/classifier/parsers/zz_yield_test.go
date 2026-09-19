package parsers

import (
	"bufio"
	"os"
	"testing"

	"github.com/spencercnorton/bitagent/internal/anime"
	"github.com/spencercnorton/bitagent/internal/classifier/classification"
	"github.com/spencercnorton/bitagent/internal/model"
)

// TestZZCorpusYield measures the patch against the real norvi[1] delete corpus.
// Skipped unless CORPUS points at one name per line.
func TestZZCorpusYield(t *testing.T) {
	p := os.Getenv("CORPUS")
	if p == "" {
		t.Skip("set CORPUS")
	}
	f, err := os.Open(p)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 1<<24)

	var n, typed, tv, mv, withTitle, emptyTitle int
	// Scoped strictly to the cohort THIS change touches. Junk titles elsewhere
	// in the corpus are pre-existing (music albums typed `movie` by the bare-year
	// branch) and are reported separately rather than blamed on this patch.
	var mine, mineTyped, mineJunk int
	var junk, preexisting []string
	banned := []string{"[", "]", "(", ")", ".mkv", ".mp4", "1080p", "720p", "480p"}
	for sc.Scan() {
		name := sc.Text()
		if name == "" {
			continue
		}
		n++
		isMine := anime.Detect(name).IsKnownFansub()
		if isMine {
			mine++
		}
		a, err := ParseVideoContentWithOptions(
			model.Torrent{Name: name}, classification.Result{}, ParseOptions{NoiseV2: true})
		if err != nil || !a.ContentType.Valid {
			continue
		}
		typed++
		if isMine {
			mineTyped++
		}
		switch a.ContentType.ContentType {
		case model.ContentTypeTvShow:
			tv++
		case model.ContentTypeMovie:
			mv++
		}
		if a.BaseTitle.String == "" {
			emptyTitle++
			continue
		}
		withTitle++
		for _, b := range banned {
			if contains(a.BaseTitle.String, b) {
				if isMine {
					mineJunk++
					junk = append(junk, a.BaseTitle.String)
				} else {
					preexisting = append(preexisting, a.BaseTitle.String)
				}
				break
			}
		}
	}
	t.Logf("corpus n=%d  typed=%d (%.2f%%)  tv=%d movie=%d", n, typed, 100*float64(typed)/float64(n), tv, mv)
	t.Logf("  of typed: withTitle=%d  emptyTitle=%d", withTitle, emptyTitle)
	t.Logf("THIS CHANGE's cohort (known fansub): present=%d typed=%d junkTitles=%d", mine, mineTyped, mineJunk)
	t.Logf("PRE-EXISTING junk titles elsewhere in the corpus (NOT this change): %d", len(preexisting))
	for i, j := range preexisting {
		if i >= 5 {
			break
		}
		t.Logf("    pre-existing: %q  <- music album typed `movie` by the bare-year branch", j)
	}
	if mineJunk > 0 {
		for _, j := range junk {
			t.Errorf("rescued row carries a junk BaseTitle (a live TMDB query): %q", j)
		}
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

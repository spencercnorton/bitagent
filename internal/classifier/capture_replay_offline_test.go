package classifier

import (
	"bufio"
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/spencercnorton/bitagent/internal/classifier/llmmatch"
)

// TestCaptureReplayIdentityGate replays REAL captured production rerank inputs
// through the live identity gate, canonical-only versus canonical+alias, and
// reports how far the alias evidence added in v0.72.0 widens it.
//
// Why this exists: the counter-based before/after could not reach significance,
// because the pre-deploy baseline arm is fixed and small — growing the after arm
// alone has a hard power ceiling. The title gate is a PURE function, so
// replaying frozen captures answers the same question deterministically at
// arbitrary N, with zero LLM cost and zero sampling noise.
//
// It also gives llm_evaluation_captures a reader reachable from the classifier
// package: tools/llmeval can read the ledger, but it is a separate main package
// that is not built into the shipped image.
//
// Skipped unless CAPTURE_REPLAY_FILE points at a JSONL export of
// llm_evaluation_captures (task='matcher_rerank'), one object per line:
//
//	{"extracted":"<extraction.title>",
//	 "candidates":[{"tmdb_id":"123","title":"...","alts":["..."],"original":"..."}]}
//
// Set CAPTURE_REPLAY_RECOVERED_OUT to dump every alias-only recovery for hand
// adjudication. The recoveries inherit TMDB's alt-title data quality and are NOT
// all correct — regional spinoffs and franchise entries leak through (measured
// 2026-08-05: roughly 1 in 5 of a 30-case sample).
func TestCaptureReplayIdentityGate(t *testing.T) {
	path := os.Getenv("CAPTURE_REPLAY_FILE")
	if path == "" {
		t.Skip("set CAPTURE_REPLAY_FILE to a captured-rerank JSONL export")
	}
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open %s: %v", path, err)
	}
	defer f.Close()

	type candidate struct {
		TMDBID   string   `json:"tmdb_id"`
		Title    string   `json:"title"`
		Alts     []string `json:"alts"`
		Original *string  `json:"original"`
	}
	type row struct {
		Extracted  string      `json:"extracted"`
		Candidates []candidate `json:"candidates"`
	}

	var sets, setsCanonical, setsAlias int
	var pairs, pairsCanonical, pairsAlias int
	var recovered []string
	// Dropped rows are counted and reported, never silently ignored. A replay
	// that quietly discards part of its input still prints a confident
	// percentage, which is worse than failing: the number looks like it came
	// from the whole corpus.
	var skippedNoTitle, skippedNoCandidates int

	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 1<<24)
	lineNo := 0
	for sc.Scan() {
		lineNo++
		line := sc.Bytes()
		if len(strings.TrimSpace(string(line))) == 0 {
			continue
		}
		var r row
		// Malformed JSON is a broken export, not a row to skip. Fail loudly
		// with the line number rather than analysing a partial corpus.
		if err := json.Unmarshal(line, &r); err != nil {
			t.Fatalf("%s:%d: malformed JSON: %v", path, lineNo, err)
		}
		if r.Extracted == "" {
			skippedNoTitle++
			continue
		}
		// A row with a title but no candidates contributes no pairs. Counting
		// it as a set would deflate the set-level percentages against a
		// denominator that never had a chance to match.
		if len(r.Candidates) == 0 {
			skippedNoCandidates++
			continue
		}
		sets++
		ext := llmmatch.Extraction{Title: r.Extracted}
		anyCanonical, anyAlias := false, false
		for _, c := range r.Candidates {
			pairs++
			okCanonical := llmMatchTitleCompatible(ext, llmmatch.Candidate{Title: c.Title})
			alts := append([]string{}, c.Alts...)
			if c.Original != nil && *c.Original != "" {
				alts = append(alts, *c.Original)
			}
			okAlias := llmMatchTitleCompatible(ext,
				llmmatch.Candidate{Title: c.Title, AltTitles: alts})

			if okCanonical {
				pairsCanonical++
				anyCanonical = true
			}
			if okAlias {
				pairsAlias++
				anyAlias = true
			}
			if okAlias && !okCanonical {
				recovered = append(recovered, r.Extracted+"  ->  "+c.Title)
			}
			// MONOTONICITY, not precision. This fires only when the alias path
			// REJECTS something the canonical comparison accepted — i.e. when
			// the canonical comparison has been weakened rather than
			// supplemented. Zero such cases means the change is additive.
			//
			// It says nothing about whether the pairs alias evidence NEWLY
			// accepts are correct, and that is the direction precision
			// actually moves: a 30-case hand read of the recoveries found
			// roughly 1 in 5 wrong (Suicide Squad -> Crazy Famous, Top Gear ->
			// Top Gear Australia, Beverly Hills 90210 -> BH90210). Regional
			// spinoffs and franchise entries leak through TMDB's alt-title
			// data, and the year gate cannot catch the TV ones because it
			// exempts TV.
			//
			// Measuring that properly needs adjudicated labels on the
			// alias-only recoveries, reported as a false-positive rate. This
			// assertion is not a substitute and must not be cited as one.
			if okCanonical && !okAlias {
				t.Errorf("REGRESSION: alias evidence rejected a pair canonical accepted: %q -> %q",
					r.Extracted, c.Title)
			}
		}
		if anyCanonical {
			setsCanonical++
		}
		if anyAlias {
			setsAlias++
		}
	}
	if err := sc.Err(); err != nil {
		t.Fatalf("scan: %v", err)
	}
	if sets == 0 {
		t.Fatalf("no usable rows in %s (%d lines, %d without a title, %d without candidates)",
			path, lineNo, skippedNoTitle, skippedNoCandidates)
	}
	// Guard the denominators before any percentage is printed. Without this a
	// candidate-less export yields NaN%% and reads like a real measurement.
	if pairs == 0 {
		t.Fatalf("%d usable rows but zero candidate pairs in %s — nothing was compared",
			sets, path)
	}
	if skippedNoTitle > 0 || skippedNoCandidates > 0 {
		t.Logf("DROPPED %d of %d lines: %d without a title, %d without candidates — "+
			"percentages below cover the remaining %d rows only",
			skippedNoTitle+skippedNoCandidates, lineNo,
			skippedNoTitle, skippedNoCandidates, sets)
	}

	if out := os.Getenv("CAPTURE_REPLAY_RECOVERED_OUT"); out != "" {
		if err := os.WriteFile(out, []byte(strings.Join(recovered, "\n")), 0o644); err != nil {
			t.Logf("could not write recovered dump: %v", err)
		}
	}

	pct := func(a, b int) float64 { return 100 * float64(a) / float64(b) }
	t.Logf("captured rerank sets: %d   (extraction, candidate) pairs: %d", sets, pairs)
	t.Logf("PAIRS accepted   canonical-only %d (%.2f%%)  ->  +alias %d (%.2f%%)   %+d (%+.2f pp)",
		pairsCanonical, pct(pairsCanonical, pairs), pairsAlias, pct(pairsAlias, pairs),
		pairsAlias-pairsCanonical, pct(pairsAlias, pairs)-pct(pairsCanonical, pairs))
	t.Logf("SETS with >=1 acceptable   canonical-only %d (%.2f%%)  ->  +alias %d (%.2f%%)   %+d (%+.2f pp, %+.1f%% relative)",
		setsCanonical, pct(setsCanonical, sets), setsAlias, pct(setsAlias, sets),
		setsAlias-setsCanonical, pct(setsAlias, sets)-pct(setsCanonical, sets),
		100*float64(setsAlias-setsCanonical)/float64(setsCanonical))
	t.Logf("NOTE: this measures gate OPPORTUNITY, not realised recovery — the model must also")
	t.Logf("      have chosen the alias-matching candidate. Set-level gain is an upper bound.")
	for i, s := range recovered {
		if i >= 12 {
			t.Logf("   ... %d more (set CAPTURE_REPLAY_RECOVERED_OUT to dump all)", len(recovered)-i)
			break
		}
		t.Logf("   recovered: %s", s)
	}
}

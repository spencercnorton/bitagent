package processor

import (
	"encoding/json"
	"sync"
	"testing"

	"github.com/spencercnorton/bitagent/internal/classifier/classification"
	"github.com/spencercnorton/bitagent/internal/model"
	"github.com/spencercnorton/bitagent/internal/protocol"
)

// TestDeleteAuditBudgetStopsAtCap is the enforceable-stop-condition guarantee:
// capture must halt on its own rather than depending on a future config edit.
func TestDeleteAuditBudgetStopsAtCap(t *testing.T) {
	b := newDeleteAuditBudget(true, 3, nil)
	got := 0
	for i := 0; i < 50; i++ {
		if b.consume() {
			got++
		}
	}
	if got != 3 {
		t.Fatalf("recorded %d names, want exactly the cap of 3", got)
	}
	if b.Recorded() != 3 {
		t.Fatalf("Recorded() = %d, want 3", b.Recorded())
	}
}

// TestDeleteAuditBudgetConcurrentNeverExceedsCap guards the reason consume()
// adds before comparing. A check-then-add would let two goroutines both see
// recorded == max-1 and both record, overshooting a privacy bound under
// exactly the concurrency the classify loop runs at (one goroutine/torrent).
func TestDeleteAuditBudgetConcurrentNeverExceedsCap(t *testing.T) {
	const cap, workers, each = 100, 32, 50
	b := newDeleteAuditBudget(true, cap, nil)

	var mu sync.Mutex
	total := 0
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			local := 0
			for i := 0; i < each; i++ {
				if b.consume() {
					local++
				}
			}
			mu.Lock()
			total += local
			mu.Unlock()
		}()
	}
	wg.Wait()
	if total != cap {
		t.Fatalf("recorded %d across %d goroutines, want exactly %d", total, workers, cap)
	}
}

// TestDeleteAuditBudgetFailsClosed pins that a misconfigured cap disables
// capture instead of meaning "unlimited" — the failure mode that would turn a
// typo into an unbounded privacy-sensitive collection.
func TestDeleteAuditBudgetFailsClosed(t *testing.T) {
	for _, tc := range []struct {
		name string
		on   bool
		max  int
	}{
		{"flag off", false, 5000},
		{"zero cap", true, 0},
		{"negative cap", true, -1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b := newDeleteAuditBudget(tc.on, tc.max, nil)
			if b.enabled() {
				t.Fatal("budget reports enabled")
			}
			if b.consume() {
				t.Fatal("budget granted a name")
			}
			// A disabled budget must report zero recorded, not a negative
			// count leaked from a negative cap.
			if got := b.Recorded(); got != 0 {
				t.Fatalf("Recorded() = %d on a disabled budget, want 0", got)
			}
		})
	}
	var nilBudget *deleteAuditBudget
	if nilBudget.enabled() || nilBudget.consume() || nilBudget.Recorded() != 0 {
		t.Fatal("nil budget must be inert, not permissive")
	}
}

// TestExcludedDeletesDoNotBurnBudget pins the ordering in sampledDeleteName:
// budget is consumed only after every exclusion and the sample test pass.
// If an excluded torrent spent quota, the cap would stop meaning "names
// recorded" and a run of private or CSAM deletes could silently exhaust the
// sample without producing a single reviewable row.
func TestExcludedDeletesDoNotBurnBudget(t *testing.T) {
	b := newDeleteAuditBudget(true, 2, nil)
	inSample := protocol.ID([20]byte{0})

	// Excluded: private, banned keyword in name, banned keyword in a path.
	excluded := []struct {
		torrent model.Torrent
		paths   []string
	}{
		{model.Torrent{InfoHash: inSample, Name: "x", Private: true}, nil},
		{model.Torrent{InfoHash: inSample, Name: "something pthc something"}, nil},
		{model.Torrent{InfoHash: inSample, Name: "clean name"}, []string{"a/preteen b.mkv"}},
		// Out of sample entirely.
		{model.Torrent{InfoHash: protocol.ID([20]byte{9}), Name: "clean"}, nil},
	}
	for _, e := range excluded {
		if _, ok := sampledDeleteName(e.torrent, e.paths, b); ok {
			t.Fatalf("excluded torrent %q recorded a name", e.torrent.Name)
		}
	}
	if b.Recorded() != 0 {
		t.Fatalf("excluded deletes burned %d budget, want 0", b.Recorded())
	}

	// The full cap is still available to real, eligible deletes.
	for i := 0; i < 2; i++ {
		if _, ok := sampledDeleteName(
			model.Torrent{InfoHash: inSample, Name: "Some.Movie.2024.GERMAN.1080p"}, nil, b,
		); !ok {
			t.Fatalf("eligible delete %d was refused despite available budget", i)
		}
	}
	if _, ok := sampledDeleteName(
		model.Torrent{InfoHash: inSample, Name: "Some.Movie.2024.GERMAN.1080p"}, nil, b,
	); ok {
		t.Fatal("recorded past the cap")
	}
}

// TestEvidenceStopsCarryingNamesOnceSpent is the end-to-end form: the emitted
// evidence JSON silently loses the name field once the budget is spent, while
// the rule_path the ledger depends on keeps flowing.
func TestEvidenceStopsCarryingNamesOnceSpent(t *testing.T) {
	b := newDeleteAuditBudget(true, 1, nil)
	tor := model.Torrent{
		InfoHash: protocol.ID([20]byte{0}),
		Name:     "Some.Movie.2024.GERMAN.1080p",
	}
	err := classification.RuntimeError{
		Cause: classification.ErrDeleteTorrent,
		Path:  []string{"workflows", "norvi", "[2]"},
	}

	hasName := func() bool {
		var ev map[string]any
		if uerr := json.Unmarshal(
			classifierDeleteEvidence("norvi", err, tor, nil, b), &ev,
		); uerr != nil {
			t.Fatalf("evidence is not valid JSON: %v", uerr)
		}
		if _, ok := ev["rule_path"]; !ok {
			t.Fatal("rule_path disappeared — the ledger contract broke")
		}
		_, ok := ev["name"]
		return ok
	}

	if !hasName() {
		t.Fatal("first delete recorded no name despite available budget")
	}
	if hasName() {
		t.Fatal("second delete recorded a name past the cap of 1")
	}
}

package evidence

import (
	"strings"
	"testing"
)

// TestIndexerStatsQueryScope pins the privacy and correctness contract:
// only webhook_grab rows count, grouping is by the *arr-chosen indexer,
// and no title/release column is selected — aggregate counts only.
func TestIndexerStatsQueryScope(t *testing.T) {
	for _, q := range []string{indexerBreakdownQuery, indexerTrendQuery} {
		if !strings.Contains(q, "source_kind = 'webhook_grab'") {
			t.Error("query must restrict to webhook_grab evidence")
		}
		if !strings.Contains(q, "raw_payload->'release'->>'indexer'") {
			t.Error("query must read the indexer from the grab payload")
		}
		if strings.Contains(q, "title") {
			t.Error("query must not select release titles (aggregate counts only)")
		}
	}
	if !strings.Contains(indexerBreakdownQuery, "'(unknown)'") {
		t.Error("breakdown must fold missing indexer names into (unknown)")
	}
	if !strings.Contains(indexerTrendQuery, "date_trunc('day'") {
		t.Error("trend must bucket by day")
	}
}

// TestBitagentIndexerPattern pins the substring match: the current *arr
// indexer name and a plausible rename must both count as BitAgent wins.
func TestBitagentIndexerPattern(t *testing.T) {
	if bitagentIndexerPattern != "%bitagent%" {
		t.Errorf("pattern must be a case-insensitive substring match, got %q", bitagentIndexerPattern)
	}
	for _, name := range []string{"BitAgent (Local DHT)", "bitagent (Prowlarr)"} {
		if !strings.Contains(strings.ToLower(name), strings.Trim(bitagentIndexerPattern, "%")) {
			t.Errorf("%q must match the BitAgent pattern", name)
		}
	}
}

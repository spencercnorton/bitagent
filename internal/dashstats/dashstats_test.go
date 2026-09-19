package dashstats

import (
	"strings"
	"testing"
)

func TestDefaultConfig(t *testing.T) {
	c := NewDefaultConfig()
	if !c.Enabled {
		t.Error("dashstats should default Enabled=true (read-only dashboard gauges)")
	}
	if c.Interval <= 0 {
		t.Errorf("Interval must be positive, got %v", c.Interval)
	}
}

// TestMatchQueryScope pins the operator decision (2026-06-26): the match figure
// counts movie/tv only (music/ebook/audiobook have no metadata source) over a
// 30-day window.
func TestMatchQueryScope(t *testing.T) {
	if !strings.Contains(matchQuery, "content_type IN ('movie', 'tv_show')") {
		t.Error("match query must restrict to movie/tv content types")
	}
	if !strings.Contains(matchQuery, "30 days") {
		t.Error("match query must use the 30-day window")
	}
	if !strings.Contains(matchQuery, "content_id IS NOT NULL") {
		t.Error("match query must count matched as content_id present")
	}
}

func TestGrabQueryOutcomes(t *testing.T) {
	for _, want := range []string{"outcome = 'success'", "outcome = 'failure'", "resolved_at IS NULL"} {
		if !strings.Contains(grabQuery, want) {
			t.Errorf("grab query missing %q", want)
		}
	}
}

// TestAltTitleQueryScope pins the coverage semantics: the universe is the same
// rows refresh-alt-titles targets (tmdb movie/tv_show/xxx), "checked" includes
// the zero-alt-titles marker, and the LIKE underscore is escaped so the prefix
// matches literally.
func TestAltTitleQueryScope(t *testing.T) {
	for _, want := range []string{
		"c.type IN ('movie', 'tv_show', 'xxx')",
		"c.source = 'tmdb'",
		"ca.key = 'alt_titles_checked'",
		`ca.key LIKE 'alt\_title:%'`,
	} {
		if !strings.Contains(altTitleQuery, want) {
			t.Errorf("alt-title query missing %q", want)
		}
	}
}

func TestCollectorsCount(t *testing.T) {
	if got := len(NewMetrics().Collectors()); got != 10 {
		t.Errorf("expected 10 gauges, got %d", got)
	}
}

// TestIndexerGrabQueryScope pins the north-star KPI framing: webhook_grab
// evidence over a 30-day window, BitAgent wins matched by indexer substring,
// raw counts only (the dashboard computes the ratio).
func TestIndexerGrabQueryScope(t *testing.T) {
	for _, want := range []string{
		"source_kind = 'webhook_grab'",
		"30 days",
		`raw_payload->'release'->>'indexer' ILIKE '%bitagent%'`,
	} {
		if !strings.Contains(indexerGrabQuery, want) {
			t.Errorf("indexer grab query missing %q", want)
		}
	}
}

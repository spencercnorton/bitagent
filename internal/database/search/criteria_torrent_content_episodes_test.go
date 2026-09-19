package search

import (
	"testing"

	"github.com/spencercnorton/bitagent/internal/database/query"
	"github.com/spencercnorton/bitagent/internal/model"
)

// fragments renders TorrentContentEpisodesCriteria to the per-season SQL
// fragments it emits. The criteria's inner GenCriteria closure ignores its
// DBContext, so a nil context is sufficient to obtain the AndCriteria without a
// database — matching the SQL-fragment assertion style used elsewhere in this
// package (see criteria_torrent_created_at_query_test.go).
func fragments(t *testing.T, episodes model.Episodes) []string {
	t.Helper()

	gen, ok := TorrentContentEpisodesCriteria(episodes).(query.GenCriteria)
	if !ok {
		t.Fatalf("expected GenCriteria, got %T", TorrentContentEpisodesCriteria(episodes))
	}

	crit, err := gen(nil)
	if err != nil {
		t.Fatalf("GenCriteria: %v", err)
	}

	and, ok := crit.(query.AndCriteria)
	if !ok {
		t.Fatalf("expected AndCriteria, got %T", crit)
	}

	out := make([]string, 0, len(and.Criteria))
	for _, c := range and.Criteria {
		db, ok := c.(query.DBCriteria)
		if !ok {
			t.Fatalf("expected DBCriteria, got %T", c)
		}
		out = append(out, db.SQL)
	}
	return out
}

// TestEpisodesCriteriaSQL locks the generated SQL to the HasEpisode semantics
// verified against Postgres: season-only queries match any row carrying the
// season key (whole packs, partial packs, single episodes); episode queries
// match whole-season packs OR rows enumerating the requested episode(s).
func TestEpisodesCriteriaSQL(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		episodes model.Episodes
		want     []string
	}{
		{
			name:     "season only matches any row carrying the season",
			episodes: make(model.Episodes).AddSeason(3),
			want:     []string{"jsonb_exists(torrent_contents.episodes, '3')"},
		},
		{
			name:     "single episode matches whole-season pack or the episode",
			episodes: make(model.Episodes).AddEpisode(2, 5),
			want: []string{
				"(torrent_contents.episodes #> '{2}' = '{}'::jsonb " +
					"OR torrent_contents.episodes #> '{2}' @> '{\"5\":{}}'::jsonb)",
			},
		},
		{
			name: "multiple episodes require all requested (or whole-season pack)",
			episodes: func() model.Episodes {
				e := make(model.Episodes)
				e.AddEpisode(1, 1)
				e.AddEpisode(1, 2)
				return e
			}(),
			want: []string{
				"(torrent_contents.episodes #> '{1}' = '{}'::jsonb " +
					"OR torrent_contents.episodes #> '{1}' @> '{\"1\":{},\"2\":{}}'::jsonb)",
			},
		},
		{
			name: "multiple whole seasons AND together",
			episodes: func() model.Episodes {
				e := make(model.Episodes)
				e.AddSeason(1)
				e.AddSeason(2)
				return e
			}(),
			want: []string{
				"jsonb_exists(torrent_contents.episodes, '1')",
				"jsonb_exists(torrent_contents.episodes, '2')",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := fragments(t, tt.episodes)
			if len(got) != len(tt.want) {
				t.Fatalf("fragment count = %d %v, want %d %v", len(got), got, len(tt.want), tt.want)
			}
			for i := range got {
				if got[i] != tt.want[i] {
					t.Errorf("fragment[%d]:\n got  %s\n want %s", i, got[i], tt.want[i])
				}
			}
		})
	}
}

// TestEpisodesCriteriaMirrorsHasEpisode is the spec anchor: the SQL above is a
// direct translation of model.Episodes.HasEpisode (a whole-season pack contains
// every episode). This table is the same matrix executed against Postgres
// during review; keeping it in Go guards against HasEpisode drifting away from
// the SQL semantics without either being updated together.
func TestEpisodesCriteriaMirrorsHasEpisode(t *testing.T) {
	t.Parallel()

	pack := model.Episodes{1: {}}                       // whole season 1
	partial := model.Episodes{1: {1: {}, 2: {}, 3: {}}} // S01E01-E03
	single := model.Episodes{1: {5: {}}}                // S01E05
	other := model.Episodes{2: {}}                      // whole season 2

	cases := []struct {
		row        model.Episodes
		season, ep int
		wantMatch  bool
	}{
		{pack, 1, 5, true},     // whole-season pack contains any episode
		{partial, 1, 2, true},  // partial pack contains an enumerated episode
		{partial, 1, 9, false}, // partial pack excludes a non-enumerated episode
		{single, 1, 5, true},   // single episode matches itself
		{single, 1, 6, false},  // single episode excludes a different episode
		{other, 1, 5, false},   // wrong season never matches
	}

	for _, c := range cases {
		if got := c.row.HasEpisode(c.season, c.ep); got != c.wantMatch {
			t.Errorf("HasEpisode(%v, S%dE%d) = %v, want %v", c.row, c.season, c.ep, got, c.wantMatch)
		}
	}
}

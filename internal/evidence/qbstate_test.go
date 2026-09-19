package evidence

import "testing"

// TestClassifyQBStateMapping locks down the alive/suspect/ignore
// partition. Adding a new qB state requires updating this table —
// it is the contract the liveness resolver and the qBittorrent
// poller share.
func TestClassifyQBStateMapping(t *testing.T) {
	cases := []struct {
		state string
		want  QBStateClass
	}{
		// alive
		{"seeding", QBStateClassAlive},
		{"uploading", QBStateClassAlive},
		{"forcedUP", QBStateClassAlive},
		{"forcedDL", QBStateClassAlive},
		{"downloading", QBStateClassAlive},
		{"stalledUP", QBStateClassAlive},
		// suspect
		{"stalledDL", QBStateClassSuspect},
		{"metaDL", QBStateClassSuspect},
		{"error", QBStateClassSuspect},
		{"missingFiles", QBStateClassSuspect},
		{"pausedDL", QBStateClassSuspect},
		{"unknown", QBStateClassSuspect},
		{"someFutureUnknownState", QBStateClassSuspect},
		// ignore
		{"queuedDL", QBStateClassIgnore},
		{"queuedUP", QBStateClassIgnore},
		{"checkingDL", QBStateClassIgnore},
		{"checkingUP", QBStateClassIgnore},
		{"moving", QBStateClassIgnore},
		{"allocating", QBStateClassIgnore},
		{"", QBStateClassIgnore},
		// case + whitespace insensitivity
		{"  Seeding  ", QBStateClassAlive},
		{"STALLEDDL", QBStateClassSuspect},
	}
	for _, tc := range cases {
		t.Run(tc.state, func(t *testing.T) {
			got := ClassifyQBState(tc.state)
			if got != tc.want {
				t.Errorf("ClassifyQBState(%q) = %q, want %q", tc.state, got, tc.want)
			}
		})
	}
}

package qbpoller

import (
	"strings"
	"testing"
	"time"

	"github.com/spencercnorton/bitagent/internal/evidence"
)

// TestTorrentToEvidencePrivateCategory verifies that torrents with
// "private" or "bitgrab" category get StrengthQBCategoryPrivate. This
// is load-bearing — later LLM gates read this to decide whether
// content is safe to send externally.
func TestTorrentToEvidencePrivateCategory(t *testing.T) {
	cases := []struct {
		name         string
		cat          string
		wantStrength uint8
	}{
		{"private", "private", evidence.StrengthQBCategoryPrivate},
		{"bitgrab", "bitgrab", evidence.StrengthQBCategoryPrivate},
		{"Private uppercase (normalized)", "Private", evidence.StrengthQBCategoryPrivate},
		{"public", "public", evidence.StrengthQBCategoryPublic},
		{"other", "linux-iso", evidence.StrengthQBCategoryPublic},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tor := qbTorrent{
				Hash:     "0123456789abcdef0123456789abcdef01234567",
				Name:     "test torrent",
				Category: tc.cat,
			}
			ev, ok := torrentToEvidence("qb-main", tor)
			if !ok {
				t.Fatal("expected evidence produced")
			}
			if ev.Strength != tc.wantStrength {
				t.Errorf("strength=%d want %d", ev.Strength, tc.wantStrength)
			}
			if ev.Source != evidence.SourceQBittorrent {
				t.Errorf("source=%q want qbittorrent", ev.Source)
			}
		})
	}
}

// TestTorrentToEvidenceSkipsUncategorized ensures we do NOT emit
// evidence for every torrent in qB — only those with an explicit
// operator-assigned category. Otherwise label_evidence bloats with
// signal-free rows.
func TestTorrentToEvidenceSkipsUncategorized(t *testing.T) {
	tor := qbTorrent{
		Hash: "0123456789abcdef0123456789abcdef01234567",
		Name: "uncategorized torrent",
	}
	if _, ok := torrentToEvidence("qb-main", tor); ok {
		t.Fatal("expected uncategorized torrent to be skipped")
	}
}

// TestTorrentToStateEvidenceClassMapping verifies the state-class
// gate — alive/suspect produce evidence rows, ignore states are
// dropped to keep label_evidence free of pure transient noise.
func TestTorrentToStateEvidenceClassMapping(t *testing.T) {
	now := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		state   string
		wantOk  bool
		wantStr uint8
	}{
		{"downloading", true, evidence.StrengthQBStateAlive},
		{"seeding", true, evidence.StrengthQBStateAlive},
		{"stalledDL", true, evidence.StrengthQBStateSuspect},
		{"metaDL", true, evidence.StrengthQBStateSuspect},
		{"queuedDL", false, 0},
		{"checkingDL", false, 0},
		{"", false, 0},
	}
	for _, tc := range cases {
		t.Run(tc.state, func(t *testing.T) {
			tor := qbTorrent{
				Hash:  "0123456789abcdef0123456789abcdef01234567",
				Name:  "test",
				State: tc.state,
			}
			ev, ok := torrentToStateEvidence("qb-main", tor, now)
			if ok != tc.wantOk {
				t.Fatalf("ok=%v want %v", ok, tc.wantOk)
			}
			if !ok {
				return
			}
			if ev.Strength != tc.wantStr {
				t.Errorf("strength=%d want %d", ev.Strength, tc.wantStr)
			}
			if ev.Kind != evidence.KindQBStateObservation {
				t.Errorf("kind=%q want %q", ev.Kind, evidence.KindQBStateObservation)
			}
			if ev.QBState != tc.state {
				t.Errorf("QBState=%q want %q", ev.QBState, tc.state)
			}
		})
	}
}

// TestTorrentToStateEvidenceDedupeKey ensures the dedupe key buckets
// per-minute so repeat polls inside the same minute collide
// (absorbed by the unique index) but distinct minutes produce
// distinct rows.
func TestTorrentToStateEvidenceDedupeKey(t *testing.T) {
	tor := qbTorrent{
		Hash:  "0123456789abcdef0123456789abcdef01234567",
		Name:  "test",
		State: "downloading",
	}
	t1 := time.Date(2026, 5, 1, 12, 0, 30, 0, time.UTC)
	t2 := time.Date(2026, 5, 1, 12, 0, 59, 0, time.UTC)
	t3 := time.Date(2026, 5, 1, 12, 1, 0, 0, time.UTC)

	ev1, _ := torrentToStateEvidence("qb-main", tor, t1)
	ev2, _ := torrentToStateEvidence("qb-main", tor, t2)
	ev3, _ := torrentToStateEvidence("qb-main", tor, t3)

	if ev1.SourceObjectID != ev2.SourceObjectID {
		t.Fatalf("expected same-minute dedupe key, got %q vs %q", ev1.SourceObjectID, ev2.SourceObjectID)
	}
	if ev1.SourceObjectID == ev3.SourceObjectID {
		t.Fatalf("expected different-minute keys to differ, both %q", ev1.SourceObjectID)
	}
	if !strings.HasPrefix(ev1.SourceObjectID, "state:") {
		t.Errorf("dedupe key should be prefixed with state:, got %q", ev1.SourceObjectID)
	}
}

// TestTorrentToEvidenceRejectsBadHash rejects torrents whose hash
// cannot be parsed. Protects the store from receiving garbage infohash
// bytes that would never match a crawler row.
func TestTorrentToEvidenceRejectsBadHash(t *testing.T) {
	tor := qbTorrent{
		Hash:     "nothex",
		Name:     "test",
		Category: "public",
	}
	if _, ok := torrentToEvidence("qb-main", tor); ok {
		t.Fatal("expected bad hash to be rejected")
	}
}

// TestCheckLoginResponse is the regression guard for the 2026-07-15
// dark-feed outage: qB answers /api/v2/auth/login with 204 and no body
// when auth is bypassed for the client subnet, and the poller used to
// treat that as a hard failure and abort every cycle.
func TestCheckLoginResponse(t *testing.T) {
	cases := []struct {
		name    string
		status  int
		body    string
		wantErr bool
	}{
		{"classic ok", 200, "Ok.", false},
		{"auth bypassed, no content", 204, "", false},
		{"auth bypassed with whitespace", 204, "\n", false},
		{"bad credentials", 200, "Fails.", true},
		{"banned", 403, "", true},
		{"server error", 500, "", true},
		{"redirect to login page", 302, "", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := checkLoginResponse(tc.status, []byte(tc.body))
			if (err != nil) != tc.wantErr {
				t.Fatalf("err=%v wantErr=%v", err, tc.wantErr)
			}
		})
	}
}

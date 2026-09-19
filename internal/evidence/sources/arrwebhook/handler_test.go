package arrwebhook

import (
	"testing"

	"github.com/spencercnorton/bitagent/internal/evidence"
)

// TestClassifyEvent verifies the event-to-precedence mapping.
// If this test changes, the evidence precedence constants have
// changed and the DOC (ARCHITECTURE.md §1) must change with it.
func TestClassifyEvent(t *testing.T) {
	cases := []struct {
		name     string
		app      string
		event    string
		wantSrc  evidence.Source
		wantKind evidence.Kind
		wantMT   evidence.MediaType
		wantStr  uint8
		wantOK   bool
	}{
		{"sonarr grab", "Sonarr", "Grab", evidence.SourceSonarr, evidence.KindWebhookGrab, evidence.MediaTypeTV, evidence.StrengthArrWebhookGrab, true},
		{"sonarr download", "Sonarr", "Download", evidence.SourceSonarr, evidence.KindWebhookImport, evidence.MediaTypeTV, evidence.StrengthArrWebhookImport, true},
		{"sonarr folder import", "Sonarr", "DownloadFolderImported", evidence.SourceSonarr, evidence.KindWebhookImport, evidence.MediaTypeTV, evidence.StrengthArrWebhookImport, true},
		{"radarr grab", "Radarr", "Grab", evidence.SourceRadarr, evidence.KindWebhookGrab, evidence.MediaTypeMovie, evidence.StrengthArrWebhookGrab, true},
		{"readarr grab", "Readarr", "Grab", evidence.SourceReadarr, evidence.KindWebhookGrab, evidence.MediaTypeBook, evidence.StrengthArrWebhookGrab, true},
		{"lidarr import", "Lidarr", "Download", evidence.SourceLidarr, evidence.KindWebhookImport, evidence.MediaTypeMusic, evidence.StrengthArrWebhookImport, true},
		{"ignored event", "Sonarr", "Test", "", "", "", 0, false},
		{"unknown app", "Prowlarr", "Grab", "", "", "", 0, false},
		{"empty event", "Sonarr", "", "", "", "", 0, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gotSrc, gotKind, gotMT, gotStr, gotOK := classifyEvent(tc.app, tc.event)
			if gotOK != tc.wantOK {
				t.Fatalf("ok=%v want %v", gotOK, tc.wantOK)
			}
			if !tc.wantOK {
				return
			}
			if gotSrc != tc.wantSrc {
				t.Errorf("src=%q want %q", gotSrc, tc.wantSrc)
			}
			if gotKind != tc.wantKind {
				t.Errorf("kind=%q want %q", gotKind, tc.wantKind)
			}
			if gotMT != tc.wantMT {
				t.Errorf("mediaType=%q want %q", gotMT, tc.wantMT)
			}
			if gotStr != tc.wantStr {
				t.Errorf("strength=%d want %d", gotStr, tc.wantStr)
			}
		})
	}
}

// TestPrecedenceOrder locks in the global strength ordering. The
// resolver's correctness depends on: qB public < qB private < *arr
// grab < *arr import, with webhook and poll tied within each *arr
// kind. If someone reorders these constants for cosmetic reasons they
// will fail this test and be forced to justify the change.
func TestPrecedenceOrder(t *testing.T) {
	order := []uint8{
		evidence.StrengthQBCategoryPublic,
		evidence.StrengthQBCategoryPrivate,
		evidence.StrengthArrPollGrab,
		evidence.StrengthArrPollImport,
		evidence.StrengthArrWebhookImport,
	}
	for i := 1; i < len(order); i++ {
		if order[i] <= order[i-1] {
			t.Fatalf("strength ordering broken at index %d: %d <= %d", i, order[i], order[i-1])
		}
	}
	if evidence.StrengthArrWebhookGrab != evidence.StrengthArrPollGrab {
		t.Errorf("webhook grab and poll grab should tie; got %d and %d",
			evidence.StrengthArrWebhookGrab, evidence.StrengthArrPollGrab)
	}
}

// TestDeriveApp verifies the precedence URL :instance > payload.InstanceName
// > payload.ApplicationName. This is the regression test for the long-running
// silent-drop bug where Sonarr v4 / Radarr v6 webhooks were rejected because
// they do not populate the `applicationName` JSON field. The URL :instance
// path parameter is operator-configured (URL .../evidence/arr/sonarr) and is
// always present, so it is the authoritative routing key.
func TestDeriveApp(t *testing.T) {
	cases := []struct {
		name            string
		instance        string
		payloadInstance string
		payloadAppName  string
		want            string
	}{
		{"url only — sonarr v4 / radarr v6 happy path", "sonarr", "", "", "sonarr"},
		{"url wins over payload instanceName", "sonarr", "Sonarr-Custom", "", "sonarr"},
		{"url wins over payload applicationName", "sonarr", "", "Sonarr-Legacy", "sonarr"},
		{"url wins over both payload fields", "radarr", "Radarr-4K", "Radarr-Legacy", "radarr"},
		{"instanceName fallback when url empty", "", "Sonarr", "", "Sonarr"},
		{"applicationName fallback when both above empty", "", "", "Lidarr", "Lidarr"},
		{"instanceName beats applicationName when url empty", "", "Sonarr", "Stale", "Sonarr"},
		{"whitespace trimmed", "  sonarr  ", "", "", "sonarr"},
		{"whitespace-only treated as empty, falls through", "   ", "Sonarr", "", "Sonarr"},
		{"all empty", "", "", "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := deriveApp(tc.instance, arrPayload{
				InstanceName:    tc.payloadInstance,
				ApplicationName: tc.payloadAppName,
			})
			if got != tc.want {
				t.Errorf("deriveApp(%q, {instanceName=%q, applicationName=%q}) = %q, want %q",
					tc.instance, tc.payloadInstance, tc.payloadAppName, got, tc.want)
			}
		})
	}
}

// TestDecodeHex verifies infohash detection from the *arr downloadId
// (qB uses the lowercased hex-encoded hash).
func TestDecodeHex(t *testing.T) {
	cases := []struct {
		in      string
		wantLen int
	}{
		{"", 0},
		{"not-a-hash", 0},
		{"0123456789abcdef0123456789abcdef0123456789ab", 0}, // 44 chars, wrong length
		{"0123456789abcdef0123456789abcdef01234567", 20},    // 40 chars, valid
		{"0123456789ABCDEF0123456789ABCDEF01234567", 20},    // uppercase tolerated
	}
	for _, tc := range cases {
		got := decodeHexOrNil(tc.in)
		if len(got) != tc.wantLen {
			t.Errorf("decodeHexOrNil(%q) len=%d want %d", tc.in, len(got), tc.wantLen)
		}
	}
}

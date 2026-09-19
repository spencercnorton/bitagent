package processor

import (
	"encoding/json"
	"testing"

	"github.com/spencercnorton/bitagent/internal/classifier/classification"
	"github.com/spencercnorton/bitagent/internal/model"
	"github.com/spencercnorton/bitagent/internal/protocol"
)

// hashWithLeadingByte builds an info-hash whose first byte decides sample
// membership, so the sampling branch is exercised deterministically.
func hashWithLeadingByte(b byte) protocol.ID {
	var arr [20]byte
	arr[0] = b
	return protocol.ID(arr)
}

func evidenceName(t *testing.T, tor model.Torrent, paths []string) (string, bool) {
	t.Helper()
	raw := classifierDeleteEvidence("norvi", classification.ErrDeleteTorrent, tor, paths,
		newDeleteAuditBudget(true, 1000, nil))
	var ev map[string]any
	if err := json.Unmarshal(raw, &ev); err != nil {
		t.Fatalf("evidence is not valid JSON: %v", err)
	}
	n, ok := ev["name"]
	if !ok {
		return "", false
	}
	s, isStr := n.(string)
	if !isStr {
		t.Fatalf("evidence name is %T, want string", n)
	}
	return s, true
}

// TestClassifierDeleteEvidenceExclusions pins the two exclusions that make
// name capture safe. A regression in either is a privacy incident, not a
// failed metric — an in-sample banned-keyword or private torrent must never
// have its name written to the ledger.
func TestClassifierDeleteEvidenceExclusions(t *testing.T) {
	const inSample = byte(0)  // < deleteNameSampleShare
	const outSample = byte(9) // >= deleteNameSampleShare

	cases := []struct {
		name     string
		torrent  model.Torrent
		paths    []string
		wantName string
		wantOK   bool
	}{
		{
			name: "in-sample public non-banned records the name",
			torrent: model.Torrent{
				InfoHash: hashWithLeadingByte(inSample),
				Name:     "Some.Movie.2024.GERMAN.1080p.BluRay.x264",
			},
			wantName: "Some.Movie.2024.GERMAN.1080p.BluRay.x264",
			wantOK:   true,
		},
		{
			name: "out-of-sample records nothing",
			torrent: model.Torrent{
				InfoHash: hashWithLeadingByte(outSample),
				Name:     "Some.Movie.2024.GERMAN.1080p.BluRay.x264",
			},
			wantOK: false,
		},
		{
			name: "private torrent is excluded even in sample",
			torrent: model.Torrent{
				InfoHash: hashWithLeadingByte(inSample),
				Name:     "Some.Movie.2024.GERMAN.1080p",
				Private:  true,
			},
			wantOK: false,
		},
		{
			name: "banned keyword in the name is excluded even in sample",
			torrent: model.Torrent{
				InfoHash: hashWithLeadingByte(inSample),
				Name:     "something pthc something",
			},
			wantOK: false,
		},
		{
			name: "banned keyword in a FILE PATH is excluded even when the name is clean",
			torrent: model.Torrent{
				InfoHash: hashWithLeadingByte(inSample),
				Name:     "Innocuous.Release.Name.2024.1080p",
			},
			paths:  []string{"folder/preteen material.mkv"},
			wantOK: false,
		},
	}

	// The flag itself: an in-sample, public, clean torrent still records
	// nothing while the operator has not opted in. This is the default path
	// in production and it must stay byte-identical to the old behaviour.
	t.Run("audit sample disabled records nothing", func(t *testing.T) {
		raw := classifierDeleteEvidence("norvi", classification.ErrDeleteTorrent,
			model.Torrent{
				InfoHash: hashWithLeadingByte(inSample),
				Name:     "Some.Movie.2024.GERMAN.1080p",
			}, nil, newDeleteAuditBudget(false, 1000, nil))
		var ev map[string]any
		if err := json.Unmarshal(raw, &ev); err != nil {
			t.Fatalf("evidence is not valid JSON: %v", err)
		}
		if _, ok := ev["name"]; ok {
			t.Fatal("name recorded with the audit sample disabled")
		}
	})

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := evidenceName(t, tc.torrent, tc.paths)
			if ok != tc.wantOK {
				t.Fatalf("name recorded = %v, want %v (got %q)", ok, tc.wantOK, got)
			}
			if tc.wantOK && got != tc.wantName {
				t.Fatalf("name = %q, want %q", got, tc.wantName)
			}
		})
	}
}

// TestClassifierDeleteEvidenceKeepsRulePath guards the pre-existing contract:
// adding name capture must not disturb the rule_path the ledger already
// depends on for the CSAM-vs-flag distinction.
func TestClassifierDeleteEvidenceKeepsRulePath(t *testing.T) {
	err := classification.RuntimeError{
		Cause: classification.ErrDeleteTorrent,
		Path:  []string{"workflows", "norvi", "[1]", "if_else"},
	}
	raw := classifierDeleteEvidence("norvi", err, model.Torrent{
		InfoHash: hashWithLeadingByte(9),
		Name:     "whatever",
	}, nil, newDeleteAuditBudget(true, 1000, nil))

	var ev map[string]any
	if uerr := json.Unmarshal(raw, &ev); uerr != nil {
		t.Fatalf("evidence is not valid JSON: %v", uerr)
	}
	if ev["workflow"] != "norvi" {
		t.Fatalf("workflow = %v, want norvi", ev["workflow"])
	}
	path, ok := ev["rule_path"].([]any)
	if !ok || len(path) != 4 {
		t.Fatalf("rule_path = %v, want the 4-element path", ev["rule_path"])
	}
}

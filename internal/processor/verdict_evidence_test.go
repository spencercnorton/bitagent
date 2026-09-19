package processor

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/spencercnorton/bitagent/internal/classifier/classification"
	"github.com/spencercnorton/bitagent/internal/classifier/contentfilter"
	"github.com/spencercnorton/bitagent/internal/model"
	"github.com/spencercnorton/bitagent/internal/protocol"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// classifierDeleteEvidence records the workflow and, when the error carries a
// CEL rule path, that path — so a reviewer can tell a CSAM-keyword delete from
// a flag delete WITHOUT any title/file-path (PII) reaching the ledger.
func TestClassifierDeleteEvidence(t *testing.T) {
	// RuntimeError case: rule_path is extracted.
	err := classification.RuntimeError{
		Path:  []string{"keywords", "banned"},
		Cause: classification.ErrDeleteTorrent,
	}
	var got map[string]any
	require.NoError(t, json.Unmarshal(classifierDeleteEvidence("default", err, model.Torrent{}, nil, newDeleteAuditBudget(false, 1000, nil)), &got))
	assert.Equal(t, "default", got["workflow"])
	assert.Equal(t, []any{"keywords", "banned"}, got["rule_path"])

	// Bare ErrDeleteTorrent (no RuntimeError wrapper): no rule_path key.
	got = nil
	require.NoError(t, json.Unmarshal(classifierDeleteEvidence("wf2", classification.ErrDeleteTorrent, model.Torrent{}, nil, newDeleteAuditBudget(false, 1000, nil)), &got))
	assert.Equal(t, "wf2", got["workflow"])
	_, hasPath := got["rule_path"]
	assert.False(t, hasPath, "bare error must not synthesize a rule_path")
}

func TestContentFilterEvidence(t *testing.T) {
	// blocked_extension carries the ext; other reasons omit it.
	var got map[string]any
	require.NoError(t, json.Unmarshal(contentFilterEvidence(contentfilter.Decision{
		Reason: contentfilter.ReasonBlockedExtension, BlockedExt: "exe",
	}), &got))
	assert.Equal(t, "blocked_extension", got["reason"])
	assert.Equal(t, "exe", got["ext"])

	got = nil
	require.NoError(t, json.Unmarshal(contentFilterEvidence(contentfilter.Decision{
		Reason: contentfilter.ReasonNSFWKeyword,
	}), &got))
	assert.Equal(t, "nsfw_keyword", got["reason"])
	_, hasExt := got["ext"]
	assert.False(t, hasExt, "non-extension drops must omit ext")
}

// No evidence field may leak a torrent name or file path while the audit
// sample is off — which is the default, and the state production runs in
// until an operator opts in (classifier.Config.DeleteAuditSample).
func TestVerdictEvidenceNoPII(t *testing.T) {
	workflowLabel := "Some.Private.Movie.2024.1080p"
	torrentName := "Totally.Distinct.Torrent.Name.2024.1080p"
	filePath := "/secret/path/file.mkv"
	ev := string(classifierDeleteEvidence(workflowLabel, classification.RuntimeError{
		Path: []string{"flags", "delete_xxx"}, Cause: classification.ErrDeleteTorrent,
	}, model.Torrent{
		// Leading byte 0 puts this squarely INSIDE the 1-in-128 audit
		// sample, so only the disabled flag can be keeping the name out.
		InfoHash: protocol.ID([20]byte{0}),
		Name:     torrentName,
	}, []string{filePath}, newDeleteAuditBudget(false, 1000, nil)))
	assert.False(t, strings.Contains(ev, torrentName),
		"audit sample off must keep the torrent name out of the ledger")
	// workflow is a caller-supplied label, not PII — but assert the file path
	// (never recorded, on or off) can't appear, and that evidence stays a
	// compact label.
	assert.False(t, strings.Contains(ev, filePath))
	assert.NotContains(t, string(contentFilterEvidence(contentfilter.Decision{
		Reason: contentfilter.ReasonBlockedExtension, BlockedExt: "iso",
	})), filePath)
}

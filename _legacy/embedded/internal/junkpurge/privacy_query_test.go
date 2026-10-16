package junkpurge

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestCandidateQueryExcludesNativePrivateTorrents(t *testing.T) {
	normalized := strings.Join(strings.Fields(candidateQuery), " ")
	require.Contains(t, normalized, "FROM torrents t WHERE t.private = false")
}

func TestCaptureOnlyQueryAdvancesPastUnexpiredJunkCaptures(t *testing.T) {
	query, err := captureOnlyQuery()
	require.NoError(t, err)
	require.NotContains(t, query, "llm-evaluation-capture-exclusion")
	normalized := strings.Join(strings.Fields(query), " ")
	require.Contains(
		t,
		normalized,
		"llm_evaluation_capture_admissions capture_admission",
	)
	require.Contains(
		t,
		normalized,
		"capture.capture_key = capture_admission.capture_key",
	)
	require.Contains(t, normalized, "capture.task = 'junkpurge'")
	require.Contains(t, normalized, "capture.expires_at > now()")
	require.Contains(
		t,
		normalized,
		"NOT EXISTS ( SELECT 1 FROM junkpurge_judgments j WHERE j.info_hash = t.info_hash )",
	)
	require.NotContains(t, normalized, "j.judged_at >= $2")
	require.Contains(t, normalized, "LIMIT $2")
	require.NotContains(t, normalized, "LIMIT $3")
}

func TestCaptureSafetyTopUpQueryIsBalancedDisjointAndPrivacySafe(
	t *testing.T,
) {
	normalized := strings.Join(
		strings.Fields(captureSafetyTopUpQuery),
		" ",
	)
	for _, required := range []string{
		"t.private = false",
		"j.verdict IN ('junk','real_mangled','real_absent')",
		"j.torrent_name = t.name",
		"PARTITION BY teacher_stratum",
		"stratum_ordinal <= greatest(1, ($2 + 1) / 2)",
		"llm_evaluation_capture_admissions capture_admission",
		"NOT EXISTS ( SELECT 1 FROM label_evidence e",
		"NOT EXISTS ( SELECT 1 FROM torrent_canonical_labels l",
	} {
		require.Contains(t, normalized, required)
	}
	require.NotContains(t, normalized, "judged_at >=")
}

func TestBatchAttemptEligibilityRechecksEveryLocalPrivacyGate(t *testing.T) {
	normalized := strings.Join(
		strings.Fields(batchAttemptEligibilityQuery),
		" ",
	)
	require.Contains(t, normalized, "AND t.private = false")
	require.Contains(
		t,
		normalized,
		"private_evidence.source='qbittorrent'",
	)
	require.Contains(
		t,
		normalized,
		"lower(private_evidence.category) IN ('private','bitgrab')",
	)
	require.Contains(
		t,
		normalized,
		"SELECT 1 FROM label_evidence e WHERE e.info_hash=t.info_hash",
	)
	require.Contains(
		t,
		normalized,
		"SELECT 1 FROM torrent_canonical_labels l WHERE l.info_hash=t.info_hash",
	)
	require.Contains(t, normalized, "AND t.name=i.torrent_name")
}

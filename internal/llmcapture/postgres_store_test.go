package llmcapture

import (
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCaptureInsertPinsBothDatabasePrivacyPredicates(t *testing.T) {
	normalized := strings.ToLower(captureInsertSQL)
	assert.Contains(t, normalized, "t.private = false")
	assert.Contains(t, normalized, "e.source = 'qbittorrent'")
	assert.Contains(t, normalized, "lower(e.category) in ('private', 'bitgrab')")
	assert.Contains(
		t,
		strings.ToLower(captureAdmissionIdentitySQL),
		"llm_evaluation_capture_admissions",
	)
	assert.NotContains(
		t,
		normalized,
		"insert into llm_evaluation_captures (\n    info_hash",
	)
}

func TestPostgresStorePinsSerializedRetentionAndHardCap(t *testing.T) {
	assert.Contains(
		t,
		strings.ToLower(captureAdmissionLockSQL),
		"pg_advisory_xact_lock",
	)
	raw, err := os.ReadFile("postgres_store.go")
	require.NoError(t, err)
	source := strings.ToLower(string(raw))
	assert.Contains(
		t,
		source,
		"delete from llm_evaluation_captures where expires_at <= $1",
	)
	assert.Contains(
		t,
		source,
		"where expires_at <= transaction_timestamp()",
	)
	assert.Contains(t, source, "order by captured_at desc, id desc")
	assert.Contains(t, source, "offset $1")
}

func TestCaptureMigrationKeepsRawAdmissionIdentitySeparateAndCascaded(
	t *testing.T,
) {
	raw, err := os.ReadFile("../../migrations/00046_llm_evaluation_capture.sql")
	require.NoError(t, err)
	sql := strings.ToLower(string(raw))
	captureStart := strings.Index(sql, "create table llm_evaluation_captures")
	admissionStart := strings.Index(
		sql,
		"create table llm_evaluation_capture_admissions",
	)
	require.Greater(t, captureStart, -1)
	require.Greater(t, admissionStart, captureStart)
	captureDefinition := sql[captureStart:admissionStart]
	assert.NotContains(t, captureDefinition, "info_hash")
	assert.Contains(t, sql[admissionStart:], "info_hash")
	assert.Contains(t, sql[admissionStart:], "on delete cascade")
	assert.Contains(t, sql, "'natural_capture','safety_topup_capture'")
}

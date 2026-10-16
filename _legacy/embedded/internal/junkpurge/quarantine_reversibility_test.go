package junkpurge

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/spencercnorton/bitagent/internal/cataloguerecovery"
	migrationssql "github.com/spencercnorton/bitagent/migrations"
	"github.com/stretchr/testify/require"
)

func TestRuntimeQuarantineRequiresCompleteRecoveryBeforeAnyMutation(t *testing.T) {
	hashes, err := quarantineJunk(context.Background(), nil, nil, [][]byte{bytes20(1)}, 0.99, 24*time.Hour, "synthetic-owner")
	require.ErrorIs(t, err, cataloguerecovery.ErrDisabled)
	require.Empty(t, hashes)
	hashes, err = quarantineJunk(context.Background(), nil, nil, nil, 0.99, 24*time.Hour, "synthetic-owner")
	require.NoError(t, err)
	require.Empty(t, hashes)
}

func TestLegacySnapshotPurgeFailsClosedBeforeDatabaseAccess(t *testing.T) {
	err := DeleteQuarantinedNow(context.Background(), nil, nil, nil, "0000000000000000000000000000000000000000")
	require.ErrorIs(t, err, cataloguerecovery.ErrDisabled)
}

// These assertions run everywhere, including CI, which has no PostgreSQL service
// and therefore skips TestQuarantineTombstoneLifecycle. They are deliberately
// crude — they read the SQL, not the behaviour — because the property being
// protected is crude: the quarantine expiry path must never again destroy
// anything. Reintroducing either the row DELETE or the torrent_liveness
// blacklist fails a test in CI rather than in production.

func TestQuarantineExpiryIsNotDestructive(t *testing.T) {
	sql := strings.Join(strings.Fields(quarantineExpireTombstoneSQL), " ")

	require.Contains(t, sql, "UPDATE junkpurge_quarantine SET expired_at = now()",
		"expiry must tombstone")
	require.Contains(t, sql, "expired_at IS NULL",
		"expiry must skip rows already tombstoned, or it re-stamps the archive "+
			"and re-emits a ledger event every cycle, forever")
	require.NotContains(t, sql, "DELETE",
		"expiry must not delete the quarantine row — the snapshot is the only "+
			"thing that makes a false-junk error recoverable")
	require.NotContains(t, sql, "torrent_liveness",
		"expiry must not blacklist — that is what forecloses re-crawl recovery")
}

func TestQuarantineUpsertReclaimsTombstones(t *testing.T) {
	sql := strings.Join(strings.Fields(quarantineUpsertSQL), " ")

	require.Contains(t, sql, "ON CONFLICT (info_hash) DO UPDATE SET")
	require.NotContains(t, sql, "DO NOTHING",
		"since expiry became a tombstone the quarantine row outlives the torrent, "+
			"so a re-crawled hash collides with its own tombstone; under DO NOTHING "+
			"the INSERT returns no rows, DELETE FROM torrents is skipped, and the "+
			"junk torrent stays in search permanently")
	require.Contains(t, sql, "expired_at = NULL",
		"re-quarantine must clear the tombstone and restart the review window")
	require.Contains(t, sql, "quarantined_at = now()")
	require.Contains(t, sql, "torrent_snapshot = excluded.torrent_snapshot",
		"the snapshot must be refreshed against the row about to be deleted, or "+
			"restore rebuilds a stale torrent")
}

func TestCandidateQueryExcludesStudentReleases(t *testing.T) {
	normalized := strings.Join(strings.Fields(candidateQuery), " ")
	require.Contains(
		t,
		normalized,
		"NOT EXISTS ( SELECT 1 FROM junkpurge_student_releases sr "+
			"WHERE sr.info_hash = t.info_hash )",
	)
}

// The student's release arm is contract-invariant precisely because it can only
// keep things. That property is enforced structurally: junkpurge_student_releases
// has nowhere to put a deletion. If someone adds a verdict/action/disposition
// column, the action arm — which IS gated on the gold-contract rewrite — becomes
// expressible through a table that no destructive review covered.
func TestStudentReleaseTableCannotExpressAnAction(t *testing.T) {
	migration, err := migrationssql.FS.ReadFile(
		"00048_junkpurge_student_releases.sql",
	)
	require.NoError(t, err)
	ddl := strings.SplitN(string(migration), "-- +goose Down", 2)[0]
	start := strings.Index(ddl, "create table junkpurge_student_releases")
	require.GreaterOrEqual(t, start, 0, "table definition not found")
	body := ddl[start:]
	end := strings.Index(body, ");")
	require.GreaterOrEqual(t, end, 0, "unterminated table definition")
	body = strings.ToLower(body[:end])

	for _, forbidden := range []string{"verdict", "action", "disposition"} {
		require.NotContains(t, body, forbidden,
			"junkpurge_student_releases must carry no %q column: a release keeps, "+
				"it never deletes", forbidden)
	}
	require.Contains(t, body, "p_junk", "the score itself is fine — it is not a decision")
}

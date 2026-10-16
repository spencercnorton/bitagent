package processors

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/spencercnorton/bitagent/internal/cataloguerecovery"
	"github.com/spencercnorton/bitagent/internal/processingstate"
	"github.com/spencercnorton/bitagent/internal/serving"
	v1 "github.com/spencercnorton/bitagent/processorapi/v1"
	"github.com/stretchr/testify/require"
)

func TestMigrationFreezesLegacyAndRestoresPreMigrationSnapshot(t *testing.T) {
	_, p, m := fixtureAt(t, 60)
	h := seedTorrent(t, p, 20, "movie")
	cfg := cataloguerecovery.Config{Enabled: true, Retention: 24 * time.Hour, MaxPayloadBytes: 64 << 20, MaxSnapshotBytes: 8 << 20, MaxSnapshots: 128, MaxStorageBytes: 128 << 20, MaxRowsPerSnapshot: 1024}
	recovery, e := cataloguerecovery.NewStore(p, cfg)
	require.NoError(t, e)
	snapshot, e := recovery.Remove(testContext, h, "blocking", "synthetic", time.Now())
	require.NoError(t, e)
	_, e = p.Exec(testContext, `INSERT INTO llm_work_tasks(task_key,kind,info_hash,source_digest,policy_digest,input_digest,family_digest,payload,priority,time_bucket,daily_limit,monthly_limit,state,expires_at,lease_owner,lease_until)
 SELECT decode(repeat(n::text,64),'hex'),'classifier_type',decode(repeat('0',40),'hex'),decode(repeat('a',64),'hex'),decode(repeat('b',64),'hex'),decode(repeat('c',64),'hex'),decode(repeat('d',64),'hex'),'{}',0,0,0,0,state,now()+interval '1 day',CASE WHEN state='leased' THEN 'old-worker' END,CASE WHEN state='leased' THEN now()+interval '1 hour' END FROM (VALUES(1,'queued'),(2,'deferred'),(3,'leased'),(4,'completed')) t(n,state);
 INSERT INTO llm_work_applications(task_key,info_hash,source_digest,policy_digest,applied_snapshot) SELECT task_key,info_hash,source_digest,policy_digest,'{"schema":"historical-test"}' FROM llm_work_tasks WHERE state='completed'`)
	require.NoError(t, e)
	_, e = m.Up(testContext)
	require.NoError(t, e)
	var count int
	require.NoError(t, p.QueryRow(testContext, `SELECT count(*) FROM processor_legacy_freeze`).Scan(&count))
	require.Equal(t, 3, count)
	require.NoError(t, p.QueryRow(testContext, `SELECT count(*) FROM llm_work_tasks WHERE state='held' AND reason='embedded_runtime_retired' AND lease_owner IS NULL`).Scan(&count))
	require.Equal(t, 3, count)
	require.NoError(t, p.QueryRow(testContext, `SELECT count(*) FROM llm_work_applications WHERE applied_snapshot='{"schema":"historical-test"}'`).Scan(&count))
	require.Equal(t, 1, count)
	restored, e := recovery.Restore(testContext, snapshot.ID, time.Now())
	require.NoError(t, e)
	require.True(t, restored)
	// Later additive migrations may be reversible. The evidence-bearing v61
	// boundary must still refuse a downgrade into the embedded runtime schema.
	_, e = m.DownTo(testContext, 60)
	require.ErrorContains(t, e, "evidence prevents destructive downgrade")
}
func TestNormalSearchReceiptAndSourceRevision(t *testing.T) {
	s, p, _ := fixture(t)
	h := seedTorrent(t, p, 21, "movie")
	a := newService(t, "receipt", []v1.Stage{v1.Match}, func(j v1.Job) v1.Result { return v1.Result{Binding: j.Binding, Outcome: v1.Unmatched} })
	register(t, s, a)
	tx, e := s.begin(testContext)
	require.NoError(t, e)
	src, _, e := readSource(testContext, tx, h, false)
	require.NoError(t, e)
	rollback(tx)
	tnt := src.torrent("1500000000000000000000000000000000000000")
	s.RecordCore(testContext, []processingstate.Receipt{{Torrent: tnt, ContentID: tnt.InfoHash.String() + ":movie:?:?", Outcome: "completed_unmatched", Processing: "completed"}})
	_, e = s.Scan(testContext)
	require.NoError(t, e)
	l, e := s.Claim(testContext)
	require.NoError(t, e)
	require.Equal(t, "completed_unmatched", l.Input.Deterministic.MatchingOutcome)
	j, e := s.Prepare(testContext, *l)
	require.NoError(t, e)
	require.NoError(t, s.Finish(testContext, *l, j, v1.Result{Binding: j.Binding, Outcome: v1.Unmatched}, false))
	_, e = p.Exec(testContext, `UPDATE torrents SET name='Synthetic.Amber.2020.mkv' WHERE info_hash=$1`, h)
	require.NoError(t, e)
	tx, e = s.begin(testContext)
	require.NoError(t, e)
	defer rollback(tx)
	src, _, e = readSource(testContext, tx, h, false)
	require.NoError(t, e)
	require.Equal(t, "not_attempted", src.input(h, s.policy, false).Deterministic.MatchingOutcome)
}
func TestPolicyRestrictionSurvivesRemovalAndDoesNotDelete(t *testing.T) {
	s, p, _ := fixture(t)
	h := seedTorrent(t, p, 22, "movie")
	a := newService(t, "policy", []v1.Stage{v1.Policy}, func(j v1.Job) v1.Result {
		return v1.Result{Binding: j.Binding, Outcome: v1.Excluded, Proposal: &v1.Proposal{Exclusion: &v1.Exclusion{Scope: "publication", Reason: "synthetic"}}}
	})
	entry := register(t, s, a)
	_, e := s.Scan(testContext)
	require.NoError(t, e)
	run(t, s)
	require.NoError(t, s.Remove(testContext, entry.ID, entry.Generation))
	policy, e := serving.NewPolicy(serving.Config{ProcessorExclusions: true}, []string{"syntheticstrongadult"}, []string{"mkv"})
	require.NoError(t, e)
	expr := policy.TorrentCondition("torrents")
	require.Contains(t, expr.SQL, "processor_exclusions")
	query := expr.SQL
	for i := range expr.Vars {
		query = strings.Replace(query, "?", fmt.Sprintf("$%d", i+1), 1)
	}
	var visible int
	require.NoError(t, p.QueryRow(testContext, "SELECT count(*) FROM torrents WHERE "+query, expr.Vars...).Scan(&visible))
	require.Zero(t, visible, "removed policy service must not republish excluded rows")

	var count int
	require.NoError(t, p.QueryRow(testContext, `SELECT count(*) FROM torrents WHERE info_hash=$1`, h).Scan(&count))
	require.Equal(t, 1, count)
	require.NoError(t, p.QueryRow(testContext, `SELECT count(*) FROM processor_exclusions WHERE info_hash=$1`, h).Scan(&count))
	require.Equal(t, 1, count)
}

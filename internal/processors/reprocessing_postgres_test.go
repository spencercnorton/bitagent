package processors

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"github.com/gin-gonic/gin"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/spencercnorton/bitagent/internal/processingstate"
	"github.com/spencercnorton/bitagent/internal/processor"
	v1 "github.com/spencercnorton/bitagent/processorapi/v1"
	"github.com/stretchr/testify/require"
)

func reprocessOrigin(t *testing.T, s *Store, p *pgxpool.Pool) (string, []byte) {
	t.Helper()
	register(t, s, newService(t, "revisit", []v1.Stage{v1.Classify}, func(j v1.Job) v1.Result { return result(j, "movie") }))
	h := seedTorrent(t, p, 90, nil)
	_, err := s.Scan(testContext)
	require.NoError(t, err)
	run(t, s)
	page, err := s.Jobs(testContext, 1, "", "")
	require.NoError(t, err)
	return page.Items[0].JobID, h
}
func reprocessWrite(t *testing.T, s *Store, origin, digit string) v1.ReprocessWrite {
	t.Helper()
	export, err := s.ExportJob(testContext, origin)
	require.NoError(t, err)
	status, err := s.Status(testContext)
	require.NoError(t, err)
	return v1.ReprocessWrite{RequestID: strings.Repeat(digit, 64), ExpectedInputRevision: export.Job.InputRevision, ExpectedPipelineRevision: status.PipelineRevision, Reason: "synthetic_review"}
}
func recordReprocessCore(t *testing.T, s *Store, p *pgxpool.Pool, h []byte, id, outcome string) {
	t.Helper()
	tx, err := s.begin(testContext)
	require.NoError(t, err)
	src, _, err := readSource(testContext, tx, h, false)
	require.NoError(t, err)
	rollback(tx)
	var contentID string
	require.NoError(t, p.QueryRow(testContext, `SELECT id FROM torrent_contents WHERE info_hash=$1`, h).Scan(&contentID))
	s.RecordCore(testContext, []processingstate.Receipt{{ReprocessRequestID: id, Torrent: src.torrent(hex.EncodeToString(h)), ContentID: contentID, Outcome: "completed_unmatched", Processing: outcome}})
}

func TestPostgresAuditedReprocessingRequiresCorrelatedCoreReceipt(t *testing.T) {
	s, p, m := fixture(t)
	origin, h := reprocessOrigin(t, s, p)
	_, err := p.Exec(testContext, `INSERT INTO processor_exclusions(info_hash,job_id,processor_id,binding,reason) VALUES($1,$2,'retained-policy','{}','synthetic_restriction')`, h, origin)
	require.NoError(t, err)
	w := reprocessWrite(t, s, origin, "a")
	r, err := s.Reprocess(testContext, origin, w)
	require.NoError(t, err)
	require.Equal(t, "waiting_core", r.State)
	var raw []byte
	require.NoError(t, p.QueryRow(testContext, `SELECT payload FROM queue_jobs WHERE id=$1`, r.CoreJobID).Scan(&raw))
	var msg processor.MessageParams
	require.NoError(t, json.Unmarshal(raw, &msg))
	require.Equal(t, w.RequestID, msg.ReprocessRequestID)
	require.Equal(t, processor.ClassifyModeDefault, msg.ClassifyMode)
	require.False(t, msg.SkipContentFilter)
	require.Empty(t, msg.ClassifierFlags)
	require.Len(t, msg.InfoHashes, 1)
	duplicate, err := s.Reprocess(testContext, origin, w)
	require.NoError(t, err)
	require.Equal(t, r, duplicate)
	changed := w
	changed.Reason = "another_reason"
	_, err = s.Reprocess(testContext, origin, changed)
	require.ErrorIs(t, err, ErrConflict)
	var n int
	require.NoError(t, p.QueryRow(testContext, `SELECT count(*) FROM queue_jobs WHERE queue='process_torrent'`).Scan(&n))
	require.Equal(t, 1, n)
	n, err = s.Scan(testContext)
	require.NoError(t, err)
	require.Zero(t, n)
	recordReprocessCore(t, s, p, h, w.RequestID, "completed")
	_, err = p.Exec(testContext, `UPDATE queue_jobs SET status='processed',ran_at=clock_timestamp() WHERE id=$1`, r.CoreJobID)
	require.NoError(t, err)
	n, err = s.Scan(testContext)
	require.NoError(t, err)
	require.Equal(t, 1, n)
	r, err = s.Reprocessing(testContext, w.RequestID)
	require.NoError(t, err)
	require.Equal(t, "queued", r.State)
	require.NotNil(t, r.ResultJobID)
	require.NotEqual(t, origin, *r.ResultJobID)
	newExport, err := s.ExportJob(testContext, *r.ResultJobID)
	require.NoError(t, err)
	require.Equal(t, "completed_unmatched", newExport.Input.Deterministic.MatchingOutcome)
	run(t, s)
	r, err = s.Reprocessing(testContext, w.RequestID)
	require.NoError(t, err)
	require.Equal(t, "settled", r.State)
	var before, archive []byte
	require.NoError(t, p.QueryRow(testContext, `SELECT before_snapshot,origin_export FROM processor_reprocess_requests WHERE request_id=$1`, w.RequestID).Scan(&before, &archive))
	require.Contains(t, string(before), origin)
	require.Contains(t, string(archive), origin)
	var typeName, application string
	require.NoError(t, p.QueryRow(testContext, `SELECT content_type FROM torrent_contents WHERE info_hash=$1`, h).Scan(&typeName))
	require.Equal(t, "movie", typeName)
	require.NoError(t, p.QueryRow(testContext, `SELECT job_id FROM processor_applications WHERE info_hash=$1`, h).Scan(&application))
	require.Equal(t, *r.ResultJobID, application)
	var restriction string
	require.NoError(t, p.QueryRow(testContext, `SELECT job_id FROM processor_exclusions WHERE info_hash=$1`, h).Scan(&restriction))
	require.Equal(t, origin, restriction)
	// Terminal retention cannot erase this explicit before-image or break
	// reconciliation of the same already-accepted request.
	_, err = p.Exec(testContext, `DELETE FROM processor_jobs WHERE job_id=$1`, origin)
	require.NoError(t, err)
	duplicate, err = s.Reprocess(testContext, origin, w)
	require.NoError(t, err)
	require.Equal(t, r, duplicate)
	audit, err := s.ExportReprocessing(testContext, w.RequestID)
	require.NoError(t, err)
	require.Equal(t, origin, audit.OriginExport.Job.JobID)
	require.Contains(t, string(audit.BeforeSnapshot), origin)
	require.NotEmpty(t, audit.CoreReceipt)
	encoded, err := json.Marshal(audit)
	require.NoError(t, err)
	var decoded v1.ReprocessingAudit
	require.NoError(t, v1.DecodeReprocessingAudit(encoded, &decoded))
	require.Equal(t, r, decoded.Request)
	_, err = m.DownTo(testContext, 63)
	require.ErrorContains(t, err, "reprocessing audit evidence prevents destructive downgrade")
}

func TestPostgresReprocessingAPIAndLateResultCancellation(t *testing.T) {
	s, p, _ := fixture(t)
	origin, h := reprocessOrigin(t, s, p)
	w := reprocessWrite(t, s, origin, "f")
	s.cfg.AdminToken = "synthetic-admin"
	gin.SetMode(gin.TestMode)
	router := gin.New()
	require.NoError(t, s.Apply(router))
	request := func(method, path string, body []byte, auth string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, "/api/processors/v1/"+path, bytes.NewReader(body))
		r.Header.Set("Authorization", auth)
		x := httptest.NewRecorder()
		router.ServeHTTP(x, r)
		return x
	}
	body, err := json.Marshal(w)
	require.NoError(t, err)
	require.Equal(t, 401, request("POST", "jobs/"+origin+"/reprocess", body, "").Code)
	require.Equal(t, 400, request("POST", "jobs/"+origin+"/reprocess", []byte(`{"unexpected":true}`), "Bearer synthetic-admin").Code)
	x := request("POST", "jobs/"+origin+"/reprocess", body, "Bearer synthetic-admin")
	require.Equal(t, 200, x.Code)
	var status v1.ReprocessStatus
	require.NoError(t, v1.Decode(x.Body.Bytes(), &status))
	recordReprocessCore(t, s, p, h, w.RequestID, "completed")
	_, err = p.Exec(testContext, `UPDATE queue_jobs SET status='processed' WHERE id=$1`, status.CoreJobID)
	require.NoError(t, err)
	_, err = s.Scan(testContext)
	require.NoError(t, err)
	l, err := s.Claim(testContext)
	require.NoError(t, err)
	require.NotNil(t, l)
	j, err := s.Prepare(testContext, *l)
	require.NoError(t, err)
	require.NoError(t, s.startAttempt(testContext, *l))
	require.Equal(t, 200, request("POST", "reprocessing/"+w.RequestID+"/cancel", nil, "Bearer synthetic-admin").Code)
	require.ErrorIs(t, s.Finish(testContext, *l, j, result(j, "movie"), true), ErrStale)
	x = request("GET", "reprocessing/"+w.RequestID+"/export", nil, "Bearer synthetic-admin")
	require.Equal(t, 200, x.Code)
	require.Equal(t, "no-store", x.Header().Get("Cache-Control"))
	var audit v1.ReprocessingAudit
	require.NoError(t, v1.DecodeReprocessingAudit(x.Body.Bytes(), &audit))
	require.Equal(t, "canceled", audit.Request.State)
}

func TestPostgresReprocessingFailsClosedAndPreservesFacts(t *testing.T) {
	for _, scenario := range []string{"no_receipt", "wrong_request", "deferred", "private", "pipeline_changed", "core_failure", "expired", "canceled"} {
		t.Run(scenario, func(t *testing.T) {
			s, p, _ := fixture(t)
			origin, h := reprocessOrigin(t, s, p)
			w := reprocessWrite(t, s, origin, "b")
			r, err := s.Reprocess(testContext, origin, w)
			require.NoError(t, err)
			want := "held"
			switch scenario {
			case "wrong_request":
				recordReprocessCore(t, s, p, h, strings.Repeat("c", 64), "completed")
			case "deferred":
				recordReprocessCore(t, s, p, h, w.RequestID, "deferred")
				want = "deferred"
			case "private":
				recordReprocessCore(t, s, p, h, w.RequestID, "completed")
				_, err = p.Exec(testContext, `UPDATE torrents SET private=true WHERE info_hash=$1`, h)
				require.NoError(t, err)
			case "pipeline_changed":
				recordReprocessCore(t, s, p, h, w.RequestID, "completed")
				a, err := s.Admission(testContext)
				require.NoError(t, err)
				_, err = s.SetAdmission(testContext, v1.AdmissionWrite{ExpectedGeneration: a.Generation, Seed: a.Seed, BasisPoints: 100, PolicyVersion: "new-cohort", Reason: "change"})
				require.NoError(t, err)
				want = "stale"
			case "core_failure":
				want = "failed"
			case "expired":
				recordReprocessCore(t, s, p, h, w.RequestID, "completed")
				_, err = p.Exec(testContext, `UPDATE processor_reprocess_requests SET created_at=clock_timestamp()-interval '2 hours' WHERE request_id=$1`, w.RequestID)
				require.NoError(t, err)
				want = "deferred"
			case "canceled":
				_, err = s.CancelReprocessing(testContext, w.RequestID)
				require.NoError(t, err)
				recordReprocessCore(t, s, p, h, w.RequestID, "completed")
				want = "canceled"
			}
			queueState := "processed"
			if scenario == "core_failure" {
				queueState = "failed"
			}
			_, err = p.Exec(testContext, `UPDATE queue_jobs SET status=$2::queue_job_status WHERE id=$1`, r.CoreJobID, queueState)
			require.NoError(t, err)
			_, err = s.Scan(testContext)
			require.NoError(t, err)
			r, err = s.Reprocessing(testContext, w.RequestID)
			require.NoError(t, err)
			require.Equal(t, want, r.State)
			require.Nil(t, r.ResultJobID)
			var count int
			require.NoError(t, p.QueryRow(testContext, `SELECT count(*) FROM torrents WHERE info_hash=$1`, h).Scan(&count))
			require.Equal(t, 1, count)
			var old string
			require.NoError(t, p.QueryRow(testContext, `SELECT job_id FROM processor_applications WHERE info_hash=$1`, h).Scan(&old))
			require.Equal(t, origin, old)
		})
	}
}

func TestPostgresReprocessingWorksWithoutPluginsAndCannotBypassCapacity(t *testing.T) {
	s, p, _ := fixture(t)
	origin, h := reprocessOrigin(t, s, p)
	es, err := s.Entries(testContext)
	require.NoError(t, err)
	require.NoError(t, s.Remove(testContext, es[0].ID, es[0].Generation))
	fresh, err := NewStore(s.cfg, s.pool, s.policy)
	require.NoError(t, err)
	w := reprocessWrite(t, fresh, origin, "d")
	r, err := fresh.Reprocess(testContext, origin, w)
	require.NoError(t, err)
	fresh.cfg.RetainedJobs = 1
	other := w
	other.RequestID = strings.Repeat("e", 64)
	_, err = fresh.Reprocess(testContext, origin, other)
	require.ErrorIs(t, err, ErrCapacity)
	recordReprocessCore(t, fresh, p, h, w.RequestID, "completed")
	_, err = p.Exec(testContext, `UPDATE queue_jobs SET status='processed' WHERE id=$1`, r.CoreJobID)
	require.NoError(t, err)
	n, err := fresh.Scan(testContext)
	require.NoError(t, err)
	require.Zero(t, n)
	r, err = fresh.Reprocessing(testContext, w.RequestID)
	require.NoError(t, err)
	require.Equal(t, "core_only", r.State)
	_, err = fresh.Reprocess(testContext, origin, w)
	require.NoError(t, err)
}

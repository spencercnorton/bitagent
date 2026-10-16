package processors

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5"
	v1 "github.com/spencercnorton/bitagent/processorapi/v1"
	"github.com/stretchr/testify/require"
)

func TestPostgresExportRetainsVersionedEvidenceAndExplicitGaps(t *testing.T) {
	s, p, _ := fixture(t)
	a := newService(t, "audit", []v1.Stage{v1.Classify}, func(j v1.Job) v1.Result { return result(j, "movie") })
	entry := register(t, s, a)
	seedTorrent(t, p, 80, nil)
	_, err := s.Scan(testContext)
	require.NoError(t, err)
	page, err := s.Jobs(testContext, 1, "", "")
	require.NoError(t, err)
	id := page.Items[0].JobID
	run(t, s)
	require.NoError(t, s.Remove(testContext, entry.ID, entry.Generation))
	export, err := s.ExportJob(testContext, id)
	require.NoError(t, err)
	require.Equal(t, v1.JobExportContract, export.Contract)
	require.Equal(t, "completed", export.Job.State)
	require.Equal(t, entry.Manifest.Processor, export.Job.Processor)
	require.Len(t, export.Results, 1)
	require.Len(t, export.Attempts, 1)
	require.Equal(t, "completed", export.Attempts[0].Outcome)
	require.NotNil(t, export.Attempts[0].FinishedAt)
	require.Contains(t, string(export.LatestApplication), id)
	require.Nil(t, export.PreparedJob)
	require.Equal(t, "not_retained_by_core", export.Coverage["raw_service_request_response"])
	require.Equal(t, "not_received_by_core", export.Coverage["raw_provider_payloads"])
	var source source
	require.NoError(t, json.Unmarshal(export.SourceSnapshot, &source))
	require.Equal(t, export.Job.InputRevision, digest(canonical(export.SourceSnapshot)))
	require.Equal(t, export.Input.Name, source.Name)
	raw, err := json.Marshal(export)
	require.NoError(t, err)
	var decoded v1.JobExport
	require.NoError(t, v1.DecodeExport(raw, &decoded))
	require.Equal(t, export.Job, decoded.Job)
	require.NotContains(t, string(raw), "synthetic-admin")
}

func TestPostgresExportAuthorizationAndBounds(t *testing.T) {
	s, p, _ := fixture(t)
	s.cfg.AdminToken = "synthetic-admin"
	gin.SetMode(gin.TestMode)
	router := gin.New()
	require.NoError(t, s.Apply(router))
	request := func(id, auth string) *httptest.ResponseRecorder {
		r := httptest.NewRequest("GET", "/api/processors/v1/jobs/"+id+"/export", nil)
		r.Header.Set("Authorization", auth)
		w := httptest.NewRecorder()
		router.ServeHTTP(w, r)
		return w
	}
	require.Equal(t, 401, request(strings.Repeat("a", 64), "").Code)
	require.Equal(t, 400, request("bad", "Bearer synthetic-admin").Code)
	require.Equal(t, 404, request(strings.Repeat("a", 64), "Bearer synthetic-admin").Code)
	_, err := s.ExportJob(testContext, strings.Repeat("a", 64))
	require.ErrorIs(t, err, pgx.ErrNoRows)
	// The export cap refuses an invalid oversized retained history rather than
	// truncating it and implying completeness.
	id := windowJob(t, p, "b")
	_, err = p.Exec(testContext, `INSERT INTO processor_attempts(job_id,lease_generation,processor_id,generation,stage,queue_wait_ms)
 SELECT $1,i,'synthetic',1,'classify',0 FROM generate_series(1,1001) i`, id)
	require.NoError(t, err)
	require.Equal(t, 413, request(id, "Bearer synthetic-admin").Code)
}

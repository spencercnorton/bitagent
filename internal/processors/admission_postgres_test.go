package processors

import (
	"bytes"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	v1 "github.com/spencercnorton/bitagent/processorapi/v1"
	"github.com/stretchr/testify/require"
)

func TestPostgresAdmissionCASAndRestart(t *testing.T) {
	s, pool, migrations := fixture(t)
	a := newService(t, "sampled", []v1.Stage{v1.Classify}, func(j v1.Job) v1.Result { return result(j, "movie") })
	entry := register(t, s, a)
	h := seedTorrent(t, pool, 60, nil)
	p, err := s.Admission(testContext)
	require.NoError(t, err)
	require.Equal(t, 10000, p.BasisPoints)
	_, err = s.Scan(testContext)
	require.NoError(t, err)
	l, err := s.Claim(testContext)
	require.NoError(t, err)
	require.NotNil(t, l)
	j, err := s.Prepare(testContext, *l)
	require.NoError(t, err)
	require.NoError(t, s.startAttempt(testContext, *l))
	w := v1.AdmissionWrite{ExpectedGeneration: p.Generation, Seed: strings.Repeat("42", 32), BasisPoints: 0, PolicyVersion: "sample-v1", Reason: "stop"}
	stopped, err := s.SetAdmission(testContext, w)
	require.NoError(t, err)
	require.Greater(t, stopped.Generation, p.Generation)
	_, err = s.SetAdmission(testContext, w)
	require.ErrorIs(t, err, ErrConflict)
	require.ErrorIs(t, s.Finish(testContext, *l, j, result(j, "movie"), true), ErrStale)
	var outcome string
	require.NoError(t, pool.QueryRow(testContext, `SELECT outcome FROM processor_attempts WHERE job_id=$1`, l.ID).Scan(&outcome))
	require.Equal(t, "unknown", outcome)
	var kind *string
	require.NoError(t, pool.QueryRow(testContext, `SELECT content_type FROM torrent_contents WHERE info_hash=$1`, h).Scan(&kind))
	require.Nil(t, kind)
	status, err := s.Status(testContext)
	require.NoError(t, err)
	require.False(t, status.Processors[0].Enabled)
	require.Greater(t, status.Processors[0].ExecutionGeneration, entry.ExecutionGeneration)
	w.ExpectedGeneration = stopped.Generation
	noOp, err := s.SetAdmission(testContext, w)
	require.NoError(t, err)
	require.Equal(t, stopped, noOp)
	// Replacement/registration cannot silently reset the global gate.
	replace := a.write()
	replace.ExpectedGeneration = status.Processors[0].Generation
	_, err = s.Register(testContext, entry.ID, replace)
	require.NoError(t, err)
	fresh, err := NewStore(s.cfg, s.pool, s.policy)
	require.NoError(t, err)
	restored, err := fresh.Admission(testContext)
	require.NoError(t, err)
	require.Equal(t, stopped, restored)
	n, err := fresh.Scan(testContext)
	require.NoError(t, err)
	require.Zero(t, n)

	w.BasisPoints, w.Reason = 10000, "resume"
	_, err = fresh.SetAdmission(testContext, w)
	require.NoError(t, err)
	n, err = fresh.Scan(testContext)
	require.NoError(t, err)
	require.Equal(t, 1, n)
	run(t, fresh)
	require.NoError(t, pool.QueryRow(testContext, `SELECT content_type FROM torrent_contents WHERE info_hash=$1`, h).Scan(&kind))
	require.Equal(t, "movie", *kind)
	history, err := fresh.AdmissionHistory(testContext, 0, 1)
	require.NoError(t, err)
	require.Len(t, history.Items, 1)
	require.Equal(t, "resume", history.Items[0].Reason)
	require.NotZero(t, history.NextBefore)
	older, err := fresh.AdmissionHistory(testContext, history.NextBefore, 100)
	require.NoError(t, err)
	require.Len(t, older.Items, 2)
	require.Equal(t, "stop", older.Items[0].Reason)
	_, err = migrations.Down(testContext)
	require.ErrorContains(t, err, "admission audit evidence prevents destructive downgrade")
}

func TestPostgresAdmissionUsesStableWholePipelineCohort(t *testing.T) {
	s, pool, _ := fixture(t)
	s.cfg.ScanBatch = 256
	for _, id := range []string{"first", "second"} {
		register(t, s, newService(t, id, []v1.Stage{v1.Classify}, func(j v1.Job) v1.Result { return result(j, "movie") }))
	}
	p, err := s.Admission(testContext)
	require.NoError(t, err)
	p, err = s.SetAdmission(testContext, v1.AdmissionWrite{ExpectedGeneration: p.Generation, Seed: strings.Repeat("42", 32), BasisPoints: 1000, PolicyVersion: "ten-percent-v1", Reason: "cohort"})
	require.NoError(t, err)
	want := 0
	for i := 1; i <= 200; i++ {
		h := seedTorrent(t, pool, byte(i), nil)
		if admittedByPolicy(h, &p) {
			want++
		}
	}
	n, err := s.Scan(testContext)
	require.NoError(t, err)
	require.Equal(t, want, n)
	require.Greater(t, n, 0)
	require.Less(t, n, 200)
	var count int
	require.NoError(t, pool.QueryRow(testContext, `SELECT count(*) FROM processor_jobs WHERE jsonb_array_length(plan->'steps')=2 AND (plan->'admission'->>'generation')::bigint=$1`, p.Generation).Scan(&count))
	require.Equal(t, want, count)
}

func TestPostgresAdmissionAPIRejectsUnnegotiatedOrMalformedWrites(t *testing.T) {
	s, _, _ := fixture(t)
	s.cfg.AdminToken = "synthetic-admin"
	gin.SetMode(gin.TestMode)
	router := gin.New()
	require.NoError(t, s.Apply(router))
	request := func(method, path, body, auth string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, "/api/processors/v1/"+path, bytes.NewBufferString(body))
		r.Header.Set("Authorization", auth)
		w := httptest.NewRecorder()
		router.ServeHTTP(w, r)
		return w
	}
	require.Equal(t, 401, request("GET", "capabilities", "", "").Code)
	w := request("GET", "capabilities", "", "Bearer synthetic-admin")
	require.Equal(t, 200, w.Code)
	var caps v1.OperationsCapabilities
	require.NoError(t, v1.Decode(w.Body.Bytes(), &caps))
	require.Equal(t, v1.AdmissionContract, caps.Features["admission"])
	for _, body := range []string{`{}`, `{"expected_generation":1,"basis_points":10001}`, `{"expected_generation":1,"expected_generation":2}`, `{"unexpected":true}`} {
		require.Equal(t, 400, request("PUT", "admission", body, "Bearer synthetic-admin").Code)
	}
	p, err := s.Admission(testContext)
	require.NoError(t, err)
	b, err := json.Marshal(v1.AdmissionWrite{ExpectedGeneration: p.Generation, Seed: p.Seed, BasisPoints: 100, PolicyVersion: "one-percent-v1", Reason: "canary"})
	require.NoError(t, err)
	require.Equal(t, 200, request("PUT", "admission", string(b), "Bearer synthetic-admin").Code)
	require.Equal(t, 409, request("PUT", "admission", string(b), "Bearer synthetic-admin").Code)
	require.Equal(t, 400, request("GET", "admission/history?limit=101", "", "Bearer synthetic-admin").Code)
}

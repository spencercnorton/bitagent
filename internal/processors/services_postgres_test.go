package processors

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	v1 "github.com/spencercnorton/bitagent/processorapi/v1"
	"github.com/stretchr/testify/require"
)

func unusedPort(t *testing.T) string {
	l, e := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, e)
	addr := l.Addr().String()
	require.NoError(t, l.Close())
	return addr
}
func launch(t *testing.T, cmd *exec.Cmd, endpoint string) {
	t.Helper()
	require.NoError(t, cmd.Start())
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
	require.Eventually(t, func() bool {
		ctx, c := context.WithTimeout(testContext, 100*time.Millisecond)
		defer c()
		_, e := NewTransport(100*time.Millisecond).Status(ctx, endpoint, "")
		return e == nil
	}, 10*time.Second, 50*time.Millisecond)
}
func TestPostgresIndependentProcessesAndOperatorAPI(t *testing.T) {
	s, p, _ := fixture(t)
	s.cfg.RequestTimeout = 2 * time.Second
	s.transport = NewTransport(2 * time.Second)
	binary := filepath.Join(t.TempDir(), "reference")
	build := exec.Command("go", "build", "-o", binary, "../../examples/processors/reference")
	output, e := build.CombinedOutput()
	require.NoError(t, e, string(output))
	address := unusedPort(t)
	cmd := exec.Command(binary)
	cmd.Env = append(os.Environ(), "PROCESSOR_LISTEN="+address)
	launch(t, cmd, "http://"+address)
	py, e := exec.LookPath("python3")
	require.NoError(t, e)
	pyAddress := unusedPort(t)
	_, port, e := net.SplitHostPort(pyAddress)
	require.NoError(t, e)
	independent := exec.Command(py, "../../examples/processors/independent/service.py")
	independent.Env = append(os.Environ(), "PROCESSOR_HOST=127.0.0.1", "PROCESSOR_PORT="+port)
	launch(t, independent, "http://"+pyAddress)
	h := seedTorrent(t, p, 70, nil)
	exclude := seedTorrent(t, p, 71, "movie")
	_, e = p.Exec(testContext, `UPDATE torrents SET name='Synthetic.Exclude.2024.mkv' WHERE info_hash=$1`, exclude)
	require.NoError(t, e)
	for id, url := range map[string]string{"reference": "http://" + address, "independent": "http://" + pyAddress} {
		_, e = s.Register(testContext, id, v1.RegistryWrite{Endpoint: url, State: "active", Mode: "apply", Configuration: json.RawMessage(`{}`)})
		require.NoError(t, e)
	}
	_, e = s.Scan(testContext)
	require.NoError(t, e)
	for range 4 {
		run(t, s)
	}
	var kind string
	require.NoError(t, p.QueryRow(testContext, `SELECT content_type FROM torrent_contents WHERE info_hash=$1`, h).Scan(&kind))
	require.Equal(t, "movie", kind)
	var n int
	require.NoError(t, p.QueryRow(testContext, `SELECT count(*) FROM processor_exclusions WHERE info_hash=$1`, exclude).Scan(&n))
	require.Equal(t, 1, n)
	s.cfg.AdminToken = "synthetic-administration-credential"
	gin.SetMode(gin.TestMode)
	router := gin.New()
	require.NoError(t, s.Apply(router))
	request := func(path, token string) *httptest.ResponseRecorder {
		t.Helper()
		w := httptest.NewRecorder()
		r := httptest.NewRequest("GET", path, nil)
		if token != "" {
			r.Header.Set("Authorization", "Bearer "+token)
		}
		router.ServeHTTP(w, r)
		return w
	}
	require.Equal(t, 401, request("/api/processors/v1/status", "").Code)
	response := request("/api/processors/v1/status", string(s.cfg.AdminToken))
	require.Equal(t, 200, response.Code)
	var status v1.Status
	require.NoError(t, json.Unmarshal(response.Body.Bytes(), &status))
	require.Len(t, status.Processors, 2)
	require.False(t, status.OptionalProcessorsRequired)
	for _, entry := range status.Processors {
		require.Equal(t, float64(2), *entry.Metrics["attempts"].Value)
		require.Nil(t, entry.Evaluation)
	}
	pageResponse := request("/api/processors/v1/jobs?limit=1&processor_id=reference", string(s.cfg.AdminToken))
	require.Equal(t, 200, pageResponse.Code)
	var page v1.JobPage
	require.NoError(t, json.Unmarshal(pageResponse.Body.Bytes(), &page))
	require.Len(t, page.Items, 1)
	require.NotEmpty(t, page.NextCursor)
	second := request("/api/processors/v1/jobs?limit=1&processor_id=reference&cursor="+page.NextCursor, string(s.cfg.AdminToken))
	require.Equal(t, 200, second.Code)
	var next v1.JobPage
	require.NoError(t, json.Unmarshal(second.Body.Bytes(), &next))
	require.Len(t, next.Items, 1)
	require.NotEqual(t, page.Items[0].JobID, next.Items[0].JobID)
	require.NotContains(t, response.Body.String(), string(s.cfg.AdminToken))
	require.NotContains(t, response.Body.String(), "Synthetic.Amber")
	t.Log("independent Go and Python processes: classify + policy, PostgreSQL application, authenticated status, counters and filtered cursor pagination passed")

	// Kill an actual independent process after registration. A successful type
	// proposal in the same pipeline must not authorize new enrichment when its
	// policy participant crashes; already eligible facts remain available.
	require.NoError(t, independent.Process.Kill())
	_ = independent.Wait()
	crashed := seedTorrent(t, p, 72, nil)
	_, e = s.Scan(testContext)
	require.NoError(t, e)
	run(t, s) // reference classification
	run(t, s) // first failed policy attempt
	_, e = p.Exec(testContext, `UPDATE processor_jobs SET available_at=now() WHERE state='deferred'`)
	require.NoError(t, e)
	run(t, s) // bounded final failure
	var newType *string
	require.NoError(t, p.QueryRow(testContext, `SELECT content_type FROM torrent_contents WHERE info_hash=$1`, crashed).Scan(&newType))
	require.Nil(t, newType)
	require.NoError(t, p.QueryRow(testContext, `SELECT content_type FROM torrent_contents WHERE info_hash=$1`, h).Scan(&kind))
	require.Equal(t, "movie", kind)
	require.NoError(t, p.QueryRow(testContext, `SELECT count(*) FROM processor_attempts WHERE processor_id='independent' AND outcome='failed'`).Scan(&n))
	require.Equal(t, 2, n)
	status, e = s.Status(testContext)
	require.NoError(t, e)
	require.Equal(t, "ready", status.CoreReadiness)
	t.Log("SIGKILL: two bounded technical failures, no new publication authority, existing movie retained, core ready")
}
func TestTransportRejectsHostileResponsesAndContract(t *testing.T) {
	j := v1.Job{Binding: v1.Binding{Contract: v1.Contract, JobID: strings.Repeat("1", 64), InputRevision: strings.Repeat("2", 64), PipelineRevision: strings.Repeat("3", 64), ConfigurationDigest: strings.Repeat("4", 64), Processor: v1.Identity{ID: "test", Version: "1", ModelVersion: "none", PolicyVersion: "1", ArtifactDigest: "none"}, Stage: v1.Classify}, Configuration: json.RawMessage(`{}`), Input: v1.Input{InfoHash: strings.Repeat("1", 40), Name: "synthetic", Deterministic: v1.Deterministic{MatchingOutcome: "not_attempted", ProcessingOutcome: "completed"}}, Deadline: time.Now().Add(time.Second).UTC().Format(time.RFC3339Nano)}
	for _, kind := range []string{"duplicate", "oversize", "unknown", "wrong_version", "redirect", "text"} {
		t.Run(kind, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				raw, _ := json.Marshal(result(j, "movie"))
				switch kind {
				case "duplicate":
					raw = append([]byte(`{"outcome":"abstained",`), raw[1:]...)
				case "oversize":
					raw = []byte(strings.Repeat("x", v1.MaxBody+1))
				case "unknown":
					raw = append([]byte(`{"surprise":true,`), raw[1:]...)
				case "wrong_version":
					raw = []byte(strings.Replace(string(raw), v1.Contract, "bitagent.processor.v2", 1))
				case "redirect":
					w.Header().Set("Location", "http://invalid.invalid")
					w.WriteHeader(302)
					return
				case "text":
					w.Header().Set("Content-Type", "text/plain")
				}
				_, _ = w.Write(raw)
			}))
			defer server.Close()
			_, e := NewTransport(time.Second).Process(testContext, server.URL, "", j)
			require.Error(t, e)
		})
	}
}
func TestCredentialFileRotationWithoutCoreRestart(t *testing.T) {
	dir := t.TempDir()
	name := "BITAGENT_PROCESSOR_TOKEN_FIXTURE"
	path := filepath.Join(dir, name)
	require.NoError(t, os.WriteFile(path, []byte("one"), 0600))
	t.Setenv(name, "")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{"received": r.Header.Get("Authorization")})
	}))
	defer server.Close()
	transport := NewTransport(time.Second)
	transport.CredentialDirectory = dir
	for _, value := range []string{"one", "two"} {
		require.NoError(t, os.WriteFile(path, []byte(value), 0600))
		var response map[string]string
		require.NoError(t, transport.call(testContext, server.URL, name, "/v1/test", nil, &response))
		require.Equal(t, "Bearer "+value, response["received"])
	}
	require.Error(t, validateEndpoint(server.URL, "../../elsewhere"))
	require.Error(t, validateEndpoint(fmt.Sprintf("%s?token=x", server.URL), ""))
}

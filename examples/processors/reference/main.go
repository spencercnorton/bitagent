// This standalone, synthetic reference service imports only the public contract.
// It is not linked into BitAgent and does not read torrent files or databases.
package main

import (
	"crypto/subtle"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
	"sync/atomic"
	"time"

	v1 "github.com/spencercnorton/bitagent/processorapi/v1"
)

func main() {
	id := v1.Identity{ID: "reference", Version: "1.0.0", ModelVersion: "none", PolicyVersion: "synthetic-v1", ArtifactDigest: "none"}
	start := time.Now().UTC().Format(time.RFC3339Nano)
	var attempts atomic.Int64
	slot := make(chan struct{}, 1)
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if secret := os.Getenv("PROCESSOR_TOKEN"); secret != "" && subtle.ConstantTimeCompare([]byte(r.Header.Get("Authorization")), []byte("Bearer "+secret)) != 1 {
			w.WriteHeader(401)
			return
		}
		switch r.URL.Path {
		case "/v1/manifest":
			_ = json.NewEncoder(w).Encode(v1.Manifest{Contract: v1.Contract, Processor: id, Capabilities: []v1.Stage{v1.Classify}, ConfigurationSchema: json.RawMessage(`{"type":"object","additionalProperties":false}`), Qualification: "synthetic"})
		case "/v1/status":
			n := attempts.Load()
			_ = json.NewEncoder(w).Encode(v1.ServiceStatus{Contract: v1.Contract, Processor: id, State: "ready", InstanceID: start, StartedAt: start, Attempts: &n})
		case "/v1/process":
			if r.Method != "POST" {
				w.WriteHeader(405)
				return
			}
			select {
			case slot <- struct{}{}:
				defer func() { <-slot }()
			default:
				w.WriteHeader(429)
				return
			}
			raw, e := io.ReadAll(io.LimitReader(r.Body, v1.MaxBody+1))
			var job v1.Job
			if e != nil || v1.Decode(raw, &job) != nil || job.Validate() != nil || job.Processor != id || job.Stage != v1.Classify {
				w.WriteHeader(422)
				return
			}
			deadline, _ := time.Parse(time.RFC3339Nano, job.Deadline)
			if time.Now().After(deadline) {
				w.WriteHeader(408)
				return
			}
			attempts.Add(1)
			result := v1.Result{Binding: job.Binding, Outcome: v1.Abstained, Reasons: []string{"outside_synthetic_fixture"}, Evidence: []v1.Evidence{}}
			if strings.HasPrefix(job.Input.Name, "Synthetic.Amber.") {
				result.Outcome = v1.Completed
				result.Proposal = &v1.Proposal{ContentType: "movie"}
				result.Reasons = []string{"synthetic_fixture"}
			}
			// A pure function has no repeatable side effects; retries return the same
			// proposal without depending on a volatile cache or worker lifetime.
			_ = json.NewEncoder(w).Encode(result)
		default:
			w.WriteHeader(404)
		}
	})
	addr := os.Getenv("PROCESSOR_LISTEN")
	if addr == "" {
		addr = "127.0.0.1:8090"
	}
	server := http.Server{Addr: addr, Handler: handler, ReadHeaderTimeout: 2 * time.Second, ReadTimeout: 3 * time.Second, WriteTimeout: 3 * time.Second, IdleTimeout: 10 * time.Second, MaxHeaderBytes: 8192}
	log.Fatal(server.ListenAndServe())
}

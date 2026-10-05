package llmcapture

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/spencercnorton/bitagent/internal/lazy"
	"github.com/stretchr/testify/require"
)

func TestJunkGroupLedgerAtomicPrivacyAndDecisionCAS(t *testing.T) {
	dsn := os.Getenv("BITAGENT_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("set BITAGENT_TEST_POSTGRES_DSN to a disposable PostgreSQL")
	}
	ctx := context.Background()
	admin, err := pgxpool.New(ctx, dsn)
	require.NoError(t, err)
	defer admin.Close()
	schema := pgx.Identifier{fmt.Sprintf("junk_capture_test_%d", time.Now().UnixNano())}.Sanitize()
	_, err = admin.Exec(ctx, "CREATE SCHEMA "+schema)
	require.NoError(t, err)
	defer func() { _, e := admin.Exec(ctx, "DROP SCHEMA "+schema+" CASCADE"); require.NoError(t, e) }()
	pcfg, err := pgxpool.ParseConfig(dsn)
	require.NoError(t, err)
	pcfg.ConnConfig.RuntimeParams["search_path"] = schema
	pool, err := pgxpool.NewWithConfig(ctx, pcfg)
	require.NoError(t, err)
	defer pool.Close()
	_, err = pool.Exec(ctx, `CREATE TABLE torrents(info_hash bytea PRIMARY KEY,name text NOT NULL,private boolean NOT NULL);
CREATE TABLE label_evidence(info_hash bytea,source text,category text);`)
	require.NoError(t, err)
	for _, path := range []string{"../../migrations/00046_llm_evaluation_capture.sql", "../../migrations/00051_llm_capture_results.sql"} {
		raw, e := os.ReadFile(path)
		require.NoError(t, e)
		_, e = pool.Exec(ctx, strings.Split(string(raw), "-- +goose Down")[0])
		require.NoError(t, e)
	}
	store := NewPostgresStore(lazy.New(func() (*pgxpool.Pool, error) { return pool, nil }))
	cfg := NewDefaultConfig()
	cfg.Enabled = true
	r, err := NewRecorder(cfg, &privacyStub{}, store)
	require.NoError(t, err)
	requests := make([]Request, 2)
	keys := make([][]byte, 2)
	hashes := make([][]byte, 2)
	for i := range requests {
		q := validRequest()
		q.Task = TaskJunkPurge
		q.CandidateSource = CandidateSourceNone
		q.InfoHash = bytes.Repeat([]byte{byte(i + 1)}, 20)
		q.ContractID = "junk-audited-synthetic-v1"
		q.ModelInputJSON = []byte(`{"model":"synthetic","messages":[{"role":"system","content":"synthetic policy"},{"role":"user","content":"1. Synthetic Film\n2. Synthetic Software"}]}`)
		name := []string{"Synthetic Film", "Synthetic Software"}[i]
		q.TaskInputJSON, err = json.Marshal(map[string]any{"torrent_name": name, "request_index": i + 1, "request_size": 2, "min_confidence": .8, "live": false})
		require.NoError(t, err)
		requests[i] = q
		hashes[i] = q.InfoHash
		keys[i], err = KeyForRequest(q)
		require.NoError(t, err)
		_, err = pool.Exec(ctx, "INSERT INTO torrents VALUES($1,$2,false)", q.InfoHash, name)
		require.NoError(t, err)
	}
	count := func(table string, column string) int {
		var n int
		require.NoError(t, pool.QueryRow(ctx, "SELECT count(*) FROM "+table+column).Scan(&n))
		return n
	}
	_, err = pool.Exec(ctx, "INSERT INTO label_evidence VALUES($1,'qbittorrent','BiTgRaB')", hashes[1])
	require.NoError(t, err)
	require.ErrorIs(t, r.CaptureJunkGroup(ctx, requests), ErrPrivacyBlocked)
	require.Zero(t, count("llm_evaluation_captures", ""), "public source must not retain the private member's shared body")
	_, err = pool.Exec(ctx, "DELETE FROM label_evidence")
	require.NoError(t, err)
	_, err = pool.Exec(ctx, "UPDATE torrents SET private=true WHERE info_hash=$1", hashes[1])
	require.NoError(t, err)
	require.ErrorIs(t, r.CaptureJunkGroup(ctx, requests), ErrPrivacyBlocked)
	require.Zero(t, count("llm_evaluation_captures", ""))
	_, err = pool.Exec(ctx, "UPDATE torrents SET private=false")
	require.NoError(t, err)
	require.NoError(t, r.CaptureJunkGroup(ctx, requests))
	require.Equal(t, 2, count("llm_evaluation_captures", ""))
	require.NoError(t, r.RecheckJunkGroup(ctx, keys))
	require.Error(t, r.RecheckJunkGroup(ctx, keys[:1]), "one member cannot borrow authority for the complete wire envelope")
	_, err = pool.Exec(ctx, "UPDATE torrents SET name='changed' WHERE info_hash=$1", hashes[1])
	require.NoError(t, err)
	require.Error(t, r.RecheckJunkGroup(ctx, keys))
	result := HTTPResult{Body: []byte(`{"choices":[{"message":{"content":"[{\"i\":1,\"verdict\":\"real_mangled\",\"confidence\":0.9},{\"i\":2,\"verdict\":\"junk\",\"confidence\":0.99}]"}}]}`), StatusCode: 200, ErrorClass: "none"}
	_, err = r.RecordJunkHTTPResult(ctx, keys, result)
	require.Error(t, err)
	require.Zero(t, count("llm_evaluation_capture_results", ""))
	_, err = pool.Exec(ctx, "UPDATE torrents SET name='Synthetic Software' WHERE info_hash=$1", hashes[1])
	require.NoError(t, err)
	receipts, err := r.RecordJunkHTTPResult(ctx, keys, result)
	require.NoError(t, err)
	require.Equal(t, 2, count("llm_evaluation_capture_results", ""))
	_, err = r.RecordJunkHTTPResult(ctx, keys, HTTPResult{Body: []byte(`{"different":true}`), StatusCode: 200, ErrorClass: "none"})
	require.Error(t, err, "first results are immutable and cannot be rebought")
	decisions := []JunkDecision{{Outcome: "judged", Verdict: "real_mangled", Confidence: .9, MinConfidence: .8, RequestIndex: 1, RequestSize: 2}, {Outcome: "judged", Verdict: "junk", Confidence: .99, MinConfidence: .8, WouldQuarantine: true, RequestIndex: 2, RequestSize: 2}}
	badHashes := [][]byte{hashes[0], bytes.Repeat([]byte{9}, 20)}
	require.Error(t, r.RecordJunkDecisions(ctx, receipts, badHashes, decisions))
	require.Zero(t, count("llm_evaluation_capture_results", " WHERE decision IS NOT NULL"), "second-member CAS failure rolls back the first member")
	bad := append([]JunkDecision(nil), decisions...)
	bad[1].Live = true
	require.Error(t, r.RecordJunkDecisions(ctx, receipts, hashes, bad))
	require.Zero(t, count("llm_evaluation_capture_results", " WHERE decision IS NOT NULL"))
	duplicate := append([]ResultReceipt(nil), receipts...)
	duplicate[0].FirstObservation = false
	require.Error(t, r.RecordJunkDecisions(ctx, duplicate, hashes, decisions))
	require.Zero(t, count("llm_evaluation_capture_results", " WHERE decision IS NOT NULL"))
	require.NoError(t, r.RecordJunkDecisions(ctx, receipts, hashes, decisions))
	require.NoError(t, r.RecordJunkDecisions(ctx, duplicate, hashes, decisions))
	bad = append([]JunkDecision(nil), decisions...)
	bad[1].Confidence = .95
	require.Error(t, r.RecordJunkDecisions(ctx, receipts, hashes, bad))
	require.Equal(t, 2, count("llm_evaluation_capture_results", " WHERE decision IS NOT NULL"))
	require.Error(t, r.CaptureJunkGroup(ctx, requests))
	require.Equal(t, 2, count("llm_evaluation_captures", ""))
	_, err = pool.Exec(ctx, "INSERT INTO label_evidence VALUES($1,'qbittorrent','private')", hashes[1])
	require.NoError(t, err)
	require.Error(t, r.RecordJunkDecisions(ctx, receipts, hashes, decisions), "privacy is rechecked on idempotent decision retries too")
}

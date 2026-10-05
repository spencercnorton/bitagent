package llmcapture

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

func (s *PostgresStore) junkTransaction(ctx context.Context) (pgx.Tx, error) {
	if s == nil || s.pool == nil {
		return nil, fmt.Errorf("junk capture store has no database pool")
	}
	pool, err := s.pool.Get()
	if err != nil {
		return nil, err
	}
	tx, err := pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	if _, err = tx.Exec(ctx, captureAdmissionLockSQL); err == nil {
		_, err = tx.Exec(ctx, `LOCK TABLE label_evidence IN SHARE MODE`)
	}
	if err != nil {
		_ = tx.Rollback(ctx)
		return nil, err
	}
	return tx, nil
}

// Whole-group admission prevents a public member's record from retaining text
// belonging to a second, private member. Locks cover the short database write;
// no database transaction is held over a network/provider call.
func (s *PostgresStore) PersistJunkGroup(ctx context.Context, records []storedCapture, hashes [][]byte, maxRows int) error {
	if len(records) == 0 || len(records) != len(hashes) || maxRows < len(records) {
		return fmt.Errorf("incomplete junk capture group or row capacity")
	}
	names := make([]string, len(records))
	for i, c := range records {
		var input struct {
			Name string `json:"torrent_name"`
		}
		if err := json.Unmarshal(c.TaskInputJSON, &input); err != nil || input.Name == "" {
			return fmt.Errorf("junk request needs exact source name")
		}
		names[i] = input.Name
	}
	tx, err := s.junkTransaction(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	rows, err := tx.Query(ctx, `SELECT t.info_hash
FROM unnest($1::bytea[], $2::text[]) AS sources(info_hash, name)
JOIN torrents t USING (info_hash)
WHERE t.private=false AND t.name=sources.name
AND NOT EXISTS (SELECT 1 FROM label_evidence e WHERE e.info_hash=t.info_hash
 AND e.source='qbittorrent' AND lower(e.category) IN ('private','bitgrab'))
FOR SHARE OF t`, hashes, names)
	if err != nil {
		return err
	}
	n := 0
	for rows.Next() {
		var hash []byte
		if err = rows.Scan(&hash); err != nil {
			rows.Close()
			return err
		}
		n++
	}
	rows.Close()
	if err = rows.Err(); err != nil {
		return err
	}
	if n != len(records) {
		return ErrPrivacyBlocked
	}
	if _, err = tx.Exec(ctx, `DELETE FROM llm_evaluation_captures WHERE expires_at <= $1`, records[0].CapturedAt); err != nil {
		return err
	}
	for i, c := range records {
		var public, inserted bool
		err = tx.QueryRow(ctx, captureInsertSQL, c.CaptureKey, string(c.Task), string(c.CandidateSource), c.SourceSHA256, c.GroupSHA256, c.InputSHA256, c.ContractSHA256, c.PromptSHA256, c.EndpointSHA256, c.Model, c.PromptVersion, c.BuildIdentity, c.ContractID, c.SystemPrompt, c.ModelInputJSON, c.TaskInputJSON, CaptureSchemaVersion, c.SamplingOrigin, c.CapturedAt, c.ExpiresAt, c.PrivacyCheckedAt, hashes[i]).Scan(&public, &inserted)
		if err != nil {
			return err
		}
		if !public {
			return ErrPrivacyBlocked
		}
		if !inserted {
			return fmt.Errorf("junk request already captured; do not rebuy or replace its first response")
		}
		if _, err = tx.Exec(ctx, captureAdmissionIdentitySQL, c.CaptureKey, hashes[i], c.ExpiresAt); err != nil {
			return err
		}
	}
	if _, err = tx.Exec(ctx, `DELETE FROM llm_evaluation_captures WHERE id IN (
 SELECT id FROM llm_evaluation_captures ORDER BY captured_at DESC,id DESC OFFSET $1)`, maxRows); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// checkJunkGroupTx locks and rechecks every member's admitted source and its
// exact name. A caller cannot substitute, omit or repeat a member of the actual
// provider request. Evidence writers wait only during this local transaction.
func checkJunkGroupTx(ctx context.Context, tx pgx.Tx, keys [][]byte) error {
	rows, err := tx.Query(ctx, `SELECT c.model_input::text,c.contract_sha256,
 (c.task_input->>'request_index')::int,(c.task_input->>'request_size')::int
FROM llm_evaluation_captures c
JOIN llm_evaluation_capture_admissions a USING(capture_key)
JOIN torrents t ON t.info_hash=a.info_hash
WHERE c.capture_key=ANY($1) AND c.task='junkpurge'
 AND c.expires_at>transaction_timestamp() AND a.expires_at=c.expires_at
 AND t.private=false AND t.name=c.task_input->>'torrent_name'
 AND NOT EXISTS(SELECT 1 FROM label_evidence e WHERE e.info_hash=t.info_hash
  AND e.source='qbittorrent' AND lower(e.category) IN('private','bitgrab'))
FOR SHARE OF c,t`, keys)
	if err != nil {
		return err
	}
	defer rows.Close()
	var contract, body []byte
	seen := map[int]bool{}
	count := 0
	for rows.Next() {
		var b string
		var c []byte
		var index, size int
		if err = rows.Scan(&b, &c, &index, &size); err != nil {
			return err
		}
		if size != len(keys) || index < 1 || index > size || seen[index] {
			return fmt.Errorf("junk group position or size mismatch")
		}
		if count > 0 && (!bytes.Equal(contract, c) || !bytes.Equal(body, []byte(b))) {
			return fmt.Errorf("junk group HTTP contract mismatch")
		}
		contract, body = c, []byte(b)
		seen[index] = true
		count++
	}
	if err = rows.Err(); err != nil {
		return err
	}
	if count != len(keys) {
		return fmt.Errorf("junk group is absent, changed, expired or no longer public")
	}
	return nil
}

func (s *PostgresStore) RecheckJunkGroup(ctx context.Context, keys [][]byte) error {
	tx, err := s.junkTransaction(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err = checkJunkGroupTx(ctx, tx, keys); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (s *PostgresStore) PersistJunkHTTPResults(ctx context.Context, results []storedHTTPResult) error {
	tx, err := s.junkTransaction(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	keys := make([][]byte, len(results))
	for i, r := range results {
		keys[i] = r.CaptureKey
	}
	if err = checkJunkGroupTx(ctx, tx, keys); err != nil {
		return err
	}
	for _, r := range results {
		tag, e := tx.Exec(ctx, `INSERT INTO llm_evaluation_capture_results
 (capture_key,response_body,response_sha256,http_status,error_class,observed_at)
 VALUES($1,$2,$3,$4,$5,$6) ON CONFLICT(capture_key) DO NOTHING`, r.CaptureKey, r.Body, r.ResponseSHA256, r.StatusCode, r.ErrorClass, r.ObservedAt)
		if e != nil {
			return e
		}
		if tag.RowsAffected() != 1 {
			return fmt.Errorf("junk first response already exists")
		}
	}
	return tx.Commit(ctx)
}

func (s *PostgresStore) PersistJunkDecisions(ctx context.Context, receipts []ResultReceipt, hashes, bodies [][]byte, at time.Time) error {
	tx, err := s.junkTransaction(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	keys := make([][]byte, len(receipts))
	for i, r := range receipts {
		keys[i] = r.CaptureKey
	}
	if err = checkJunkGroupTx(ctx, tx, keys); err != nil {
		return err
	}
	for i, r := range receipts {
		tag, e := tx.Exec(ctx, `UPDATE llm_evaluation_capture_results r
SET decision=COALESCE(r.decision,$4::jsonb),decided_at=COALESCE(r.decided_at,$5)
FROM llm_evaluation_captures c JOIN llm_evaluation_capture_admissions a USING(capture_key)
WHERE r.capture_key=$1 AND r.response_sha256=$2 AND c.capture_key=r.capture_key
 AND a.info_hash=$3 AND c.task='junkpurge'
 AND c.task_input->'min_confidence'=$4::jsonb->'min_confidence'
 AND c.task_input->'live'=$4::jsonb->'live'
 AND c.task_input->'request_index'=$4::jsonb->'request_index'
 AND c.task_input->'request_size'=$4::jsonb->'request_size'
 AND (($4::jsonb->>'outcome'='invalid_response') OR(r.http_status=200 AND r.error_class='none'))
 AND ((r.decision IS NULL AND $6) OR r.decision=$4::jsonb)`, r.CaptureKey, r.ResponseSHA256, hashes[i], bodies[i], at, r.FirstObservation)
		if e != nil {
			return e
		}
		if tag.RowsAffected() != 1 {
			return fmt.Errorf("junk first decision is changed, mismatched or already decided")
		}
	}
	return tx.Commit(ctx)
}

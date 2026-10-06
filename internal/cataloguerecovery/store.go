// Package cataloguerecovery implements an explicitly enabled, bounded recovery
// transition. It is not wired into the application and authorizes no policy.
package cataloguerecovery

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/spencercnorton/bitagent/internal/verdicts"
)

const ContractVersion = "full-catalogue-snapshot-v2"
const MaxBatch = 32

var (
	ErrDisabled  = errors.New("catalogue recovery: foundation disabled")
	ErrCapacity  = errors.New("catalogue recovery: configured recovery budget exhausted")
	ErrExpired   = errors.New("catalogue recovery: retention window expired; snapshot retained")
	ErrConflict  = errors.New("catalogue recovery: current source or policy conflicts with transition")
	ErrProtected = errors.New("catalogue recovery: protected torrent")
)

// Config has no enabled defaults. MaxPayloadBytes bounds retained canonical JSON;
// MaxStorageBytes checks the recovery and shared verdict relations including indexes
// and TOAST. WAL, the rest of the database and backup storage need separate owner
// budgets. All snapshots, including expired/restored ones, consume these caps.
type Config struct {
	Enabled            bool
	Retention          time.Duration
	MaxPayloadBytes    int64
	MaxSnapshotBytes   int64
	MaxSnapshots       int64
	MaxStorageBytes    int64
	MaxRowsPerSnapshot int64
}

func (c Config) Validate() error {
	if !c.Enabled {
		return nil
	}
	if c.Retention <= 0 || c.Retention > 365*24*time.Hour || c.MaxPayloadBytes <= 0 || c.MaxSnapshotBytes <= 0 || c.MaxSnapshotBytes > 64<<20 || c.MaxSnapshotBytes > c.MaxPayloadBytes || c.MaxSnapshots <= 0 || c.MaxStorageBytes <= 0 || c.MaxRowsPerSnapshot <= 0 || c.MaxRowsPerSnapshot > 65536 {
		return fmt.Errorf("catalogue recovery: enabling requires explicit bounded retention, payload, storage, snapshot and row caps")
	}
	return nil
}

type Store struct {
	pool *pgxpool.Pool
	cfg  Config
}

func NewStore(pool *pgxpool.Pool, cfg Config) (*Store, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	if cfg.Enabled && pool == nil {
		return nil, fmt.Errorf("catalogue recovery: enabled store requires pool")
	}
	return &Store{pool, cfg}, nil
}
func (s *Store) Enabled() bool { return s.cfg.Enabled }

// UsesPool prevents attaching removal and bloom persistence to different databases.
func (s *Store) UsesPool(pool *pgxpool.Pool) bool { return pool != nil && s.pool == pool }

type Snapshot struct {
	ID           int64
	InfoHash     []byte
	SourceDigest string
	State        string
	CreatedAt    time.Time
	ExpiresAt    time.Time
	PayloadBytes int64
}

// Include every raw-torrent cascading relation. Retained external evidence,
// canonical labels, metadata, quarantines, liveness and audit rows are untouched.
var tables = []string{"torrents", "torrent_sources", "torrents_torrent_sources", "torrent_files", "torrent_pieces", "torrent_hints", "torrent_tags", "torrent_contents", "torrent_tracker_seeds", "junkpurge_sync_claims"}

type payload map[string]json.RawMessage

func validateNow(now time.Time) error {
	if now.IsZero() {
		return fmt.Errorf("catalogue recovery: observation time required")
	}
	return nil
}
func validateHash(hash []byte) error {
	if len(hash) != 20 {
		return fmt.Errorf("catalogue recovery: 20-byte hash required")
	}
	return nil
}
func lockHash(ctx context.Context, tx pgx.Tx, hash []byte) error {
	// The same lock order is used by remove and restore, including raw absence.
	_, err := tx.Exec(ctx, `select pg_advisory_xact_lock(hashtextextended(encode($1::bytea,'hex'),73159))`, hash)
	return err
}
func (s *Store) storageCheck(ctx context.Context, tx pgx.Tx) error {
	var used int64
	err := tx.QueryRow(ctx, `select sum(pg_total_relation_size(rel)) from unnest(array['catalogue_recovery_budget'::regclass,'catalogue_recovery_snapshots'::regclass,'catalogue_recovery_events'::regclass,'torrent_verdict_events'::regclass,'torrent_verdict_state'::regclass]) rel`).Scan(&used)
	if err != nil {
		return err
	}
	if used > s.cfg.MaxStorageBytes {
		return ErrCapacity
	}
	return nil
}

// RemoveBatch is the sole removal entry point. Snapshot, raw cascade, bound
// blocking verdict and audit events commit together, or all roll back. It never
// truncates a snapshot or refunds a retained budget. A missing unsnapshotted raw
// row is an error; historical losses cannot be fabricated into recovery receipts.
func (s *Store) RemoveBatch(ctx context.Context, hashes [][]byte, reason string, now time.Time) ([]Snapshot, error) {
	if !s.cfg.Enabled {
		return nil, ErrDisabled
	}
	if len(hashes) == 0 || len(hashes) > MaxBatch || strings.TrimSpace(reason) == "" || len(reason) > 1024 {
		return nil, fmt.Errorf("catalogue recovery: 1..32 hashes and bounded reason required")
	}
	if err := validateNow(now); err != nil {
		return nil, err
	}
	ordered := make([][]byte, len(hashes))
	for i, h := range hashes {
		if err := validateHash(h); err != nil {
			return nil, err
		}
		ordered[i] = append([]byte(nil), h...)
	}
	sort.Slice(ordered, func(i, j int) bool { return bytes.Compare(ordered[i], ordered[j]) < 0 })
	for i := 1; i < len(ordered); i++ {
		if bytes.Equal(ordered[i-1], ordered[i]) {
			return nil, fmt.Errorf("catalogue recovery: duplicate hash")
		}
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	// Serialize budgets before hash locks; competing removers share this order.
	var used, count int64
	if err = tx.QueryRow(ctx, `select payload_bytes,snapshots from catalogue_recovery_budget where singleton=true for update`).Scan(&used, &count); err != nil {
		return nil, err
	}
	if err = s.storageCheck(ctx, tx); err != nil {
		return nil, err
	}
	out := make([]Snapshot, 0, len(ordered))
	for _, hash := range ordered {
		if err = lockHash(ctx, tx, hash); err != nil {
			return nil, err
		}
		snap, e := s.removeTx(ctx, tx, hash, reason, now, &used, &count)
		if e != nil {
			return nil, e
		}
		out = append(out, snap)
	}
	if err = s.storageCheck(ctx, tx); err != nil {
		return nil, err
	}
	if err = tx.Commit(ctx); err != nil {
		return nil, err
	}
	return out, nil
}
func (s *Store) Remove(ctx context.Context, hash []byte, reason string, now time.Time) (Snapshot, error) {
	r, e := s.RemoveBatch(ctx, [][]byte{hash}, reason, now)
	if e != nil {
		return Snapshot{}, e
	}
	return r[0], nil
}
func (s *Store) removeTx(ctx context.Context, tx pgx.Tx, hash []byte, reason string, now time.Time, used, count *int64) (Snapshot, error) {
	var out Snapshot
	var locked []byte
	err := tx.QueryRow(ctx, `select info_hash from torrents where info_hash=$1 for update`, hash).Scan(&locked)
	if errors.Is(err, pgx.ErrNoRows) {
		// Retries of the same committed removal do not duplicate snapshots/events.
		err = tx.QueryRow(ctx, `select id,info_hash,source_digest,state,created_at,expires_at,payload_bytes from catalogue_recovery_snapshots s where info_hash=$1 and state='removed' and reason=$2 and removed_verdict_event_id=(select max(id) from torrent_verdict_events where info_hash=$1) order by id desc limit 1`, hash, reason).Scan(&out.ID, &out.InfoHash, &out.SourceDigest, &out.State, &out.CreatedAt, &out.ExpiresAt, &out.PayloadBytes)
		if err != nil {
			return out, ErrConflict
		}
		if !now.Before(out.ExpiresAt) {
			return Snapshot{}, ErrExpired
		}
		if err = currentReceipt(ctx, tx, out.ID, hash, "removed"); err != nil {
			return Snapshot{}, err
		}
		return out, nil
	}
	if err != nil {
		return out, err
	}
	if err = protected(ctx, tx, hash, true); err != nil {
		return out, err
	}
	if err = checkCascade(ctx, tx); err != nil {
		return out, err
	}
	p := payload{}
	var size, rowCount int64
	for _, table := range tables {
		where := `info_hash=$1`
		if table == "torrent_sources" {
			where = `key in(select source from torrents_torrent_sources where info_hash=$1)`
		}
		name := pgx.Identifier{table}.Sanitize()
		// Locks existing children and shared source definitions before reading; the
		// raw FOR UPDATE also blocks new FK children until this transition ends.
		rows, e := tx.Query(ctx, `select 1 from `+name+` where `+where+` for share`, hash)
		if e != nil {
			return out, e
		}
		for rows.Next() {
		}
		e = rows.Err()
		rows.Close()
		if e != nil {
			return out, e
		}
		var n, bytesEstimate int64
		if e = tx.QueryRow(ctx, `select count(*),coalesce(sum(octet_length(to_jsonb(r)::text)),0) from `+name+` r where `+where, hash).Scan(&n, &bytesEstimate); e != nil {
			return out, e
		}
		rowCount += n
		size += bytesEstimate
		if rowCount > s.cfg.MaxRowsPerSnapshot || size > s.cfg.MaxSnapshotBytes {
			return out, ErrCapacity
		}
		var b []byte
		if e = tx.QueryRow(ctx, `select coalesce(jsonb_agg(to_jsonb(r) order by to_jsonb(r)::text),'[]'::jsonb) from `+name+` r where `+where, hash).Scan(&b); e != nil {
			return out, e
		}
		p[table] = json.RawMessage(b)
	}
	var schema []byte
	if err = tx.QueryRow(ctx, schemaQuery, tables).Scan(&schema); err != nil {
		return out, err
	}
	p["_schema"] = schema
	body, err := json.Marshal(p)
	if err != nil {
		return out, err
	}
	size = int64(len(body))
	if size > s.cfg.MaxSnapshotBytes || size > s.cfg.MaxPayloadBytes-*used || *count >= s.cfg.MaxSnapshots {
		return out, ErrCapacity
	}
	digest := sha256.Sum256(body)
	out.SourceDigest = hex.EncodeToString(digest[:])
	err = tx.QueryRow(ctx, `insert into catalogue_recovery_snapshots(info_hash,source_digest,contract_version,state,reason,snapshot,payload_bytes,created_at,expires_at) values($1,$2,$3,'prepared',$4,$5::jsonb,$6,$7,$8) returning id`, hash, out.SourceDigest, ContractVersion, reason, string(body), size, now, now.Add(s.cfg.Retention)).Scan(&out.ID)
	if err != nil {
		return out, err
	}
	if _, err = tx.Exec(ctx, `update catalogue_recovery_budget set payload_bytes=payload_bytes+$1,snapshots=snapshots+1 where singleton=true`, size); err != nil {
		return out, err
	}
	if err = event(ctx, tx, out.ID, "prepared", now, reason); err != nil {
		return out, err
	}
	if _, err = tx.Exec(ctx, `delete from torrents where info_hash=$1`, hash); err != nil {
		return out, err
	}
	evID, err := recordVerdict(ctx, tx, out.ID, hash, "removed", reason)
	if err != nil {
		return out, err
	}
	if _, err = tx.Exec(ctx, `update catalogue_recovery_snapshots set state='removed',removed_verdict_event_id=$2 where id=$1`, out.ID, evID); err != nil {
		return out, err
	}
	if err = event(ctx, tx, out.ID, "removed", now, reason); err != nil {
		return out, err
	}
	out.InfoHash = append([]byte(nil), hash...)
	out.State = "removed"
	out.CreatedAt = now
	out.ExpiresAt = now.Add(s.cfg.Retention)
	out.PayloadBytes = size
	*used += size
	*count++
	return out, nil
}

// Metadata changes invalidate restoration even when names remain compatible.
const schemaQuery = `select coalesce(jsonb_agg(jsonb_build_object('table',c.relname,'column',a.attname,'type',format_type(a.atttypid,a.atttypmod),'notnull',a.attnotnull,'generated',a.attgenerated,'identity',a.attidentity,'default',pg_get_expr(d.adbin,d.adrelid),'constraints',(select coalesce(jsonb_agg(pg_get_constraintdef(k.oid) order by pg_get_constraintdef(k.oid)), '[]'::jsonb) from pg_constraint k where k.conrelid=c.oid)) order by c.relname,a.attnum),'[]'::jsonb) from pg_class c join pg_namespace n on n.oid=c.relnamespace join pg_attribute a on a.attrelid=c.oid and a.attnum>0 and not a.attisdropped left join pg_attrdef d on d.adrelid=c.oid and d.adnum=a.attnum where n.nspname=current_schema() and c.relname=any($1)`

func checkCascade(ctx context.Context, tx pgx.Tx) error {
	var unsupported bool
	err := tx.QueryRow(ctx, `select exists(select 1 from pg_constraint where contype='f' and confrelid='torrents'::regclass and conrelid::regclass::text<>all($1))`, tables).Scan(&unsupported)
	if err != nil {
		return err
	}
	if unsupported {
		return fmt.Errorf("%w: unsupported raw dependency", ErrConflict)
	}
	return nil
}
func protected(ctx context.Context, tx pgx.Tx, hash []byte, removing bool) error {
	var active bool
	err := tx.QueryRow(ctx, `select exists(select 1 from junkpurge_quarantine where info_hash=$1 and expired_at is null) or exists(select 1 from torrent_canonical_labels where info_hash=$1)`, hash).Scan(&active)
	if err != nil {
		return err
	}
	if active {
		return ErrProtected
	}
	if !removing {
		return nil
	}
	err = tx.QueryRow(ctx, `select exists(select 1 from torrents where info_hash=$1 and private) or exists(select 1 from torrent_canonical_labels where info_hash=$1) or exists(select 1 from torrent_hints where info_hash=$1 and content_id is not null) or exists(select 1 from torrent_tags where info_hash=$1 and name in('wanted','manual','reference','bitgrab')) or exists(select 1 from junkpurge_sync_claims where info_hash=$1 and lease_until>now()) or exists(select 1 from torrent_verdict_state where info_hash=$1 and verdict in('quarantined','blacklisted','tombstoned'))`, hash).Scan(&active)
	if err != nil {
		return err
	}
	if active {
		return ErrProtected
	}
	return nil
}
func recordVerdict(ctx context.Context, tx pgx.Tx, id int64, hash []byte, state, reason string) (int64, error) {
	verdict, mechanism := verdicts.VerdictTombstoned, verdicts.MechanismBlocking
	if state == "restored" {
		verdict, mechanism = verdicts.VerdictRestored, verdicts.MechanismOperator
	}
	evidence, _ := json.Marshal(map[string]any{"catalogue_recovery_snapshot": id, "contract": ContractVersion, "transition": state})
	if _, err := tx.Exec(ctx, `insert into torrent_verdict_state(info_hash,verdict,mechanism,since) values($1,$2,$3,now()) on conflict(info_hash) do update set verdict=excluded.verdict,mechanism=excluded.mechanism,since=now(),expires_at=null,updated_at=now()`, hash, verdict, mechanism); err != nil {
		return 0, err
	}
	var evID int64
	err := tx.QueryRow(ctx, `insert into torrent_verdict_events(info_hash,verdict,mechanism,reason,evidence,actor) values($1,$2,$3,$4,$5::jsonb,'system') returning id`, hash, verdict, mechanism, reason, string(evidence)).Scan(&evID)
	return evID, err
}
func currentReceipt(ctx context.Context, tx pgx.Tx, id int64, hash []byte, state string) error {
	var matches bool
	err := tx.QueryRow(ctx, `select exists(select 1 from catalogue_recovery_snapshots s join torrent_verdict_state v on v.info_hash=s.info_hash join torrent_verdict_events e on e.id=case when $3='removed' then s.removed_verdict_event_id else s.restored_verdict_event_id end where s.id=$1 and s.info_hash=$2 and s.state=$3 and e.id=(select max(id) from torrent_verdict_events where info_hash=$2) and e.info_hash=$2 and e.verdict=v.verdict and e.mechanism=v.mechanism and e.evidence=jsonb_build_object('catalogue_recovery_snapshot',s.id,'contract',s.contract_version,'transition',$3::text) and v.verdict=case when $3='removed' then 'tombstoned' else 'restored' end and v.mechanism=case when $3='removed' then 'blocking' else 'operator' end)`, id, hash, state).Scan(&matches)
	if err != nil {
		return err
	}
	if !matches {
		return ErrConflict
	}
	return nil
}

// Restore commits the exact complete source rows and its bound unblock receipt
// together. Recrawl, schema drift, newer verdicts and active quarantine fail closed.
// The retained snapshot permits investigation after expiry; expiry does not purge.
func (s *Store) Restore(ctx context.Context, id int64, now time.Time) (bool, error) {
	if !s.cfg.Enabled {
		return false, ErrDisabled
	}
	if id <= 0 {
		return false, fmt.Errorf("catalogue recovery: positive snapshot ID required")
	}
	if err := validateNow(now); err != nil {
		return false, err
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead})
	if err != nil {
		return false, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	var budgetLock bool
	if err = tx.QueryRow(ctx, `select singleton from catalogue_recovery_budget where singleton=true for update`).Scan(&budgetLock); err != nil {
		return false, err
	}
	var hash []byte
	if err = tx.QueryRow(ctx, `select info_hash from catalogue_recovery_snapshots where id=$1`, id).Scan(&hash); err != nil {
		return false, err
	}
	if err = lockHash(ctx, tx, hash); err != nil {
		return false, err
	}
	var state, reason, version, digest string
	var expiry time.Time
	var body []byte
	err = tx.QueryRow(ctx, `select state,reason,contract_version,expires_at,snapshot,source_digest from catalogue_recovery_snapshots where id=$1 for update`, id).Scan(&state, &reason, &version, &expiry, &body, &digest)
	if err != nil {
		return false, err
	}
	if state == "restored" {
		return false, nil
	}
	if state != "removed" || version != ContractVersion {
		return false, ErrConflict
	}
	if !now.Before(expiry) {
		return false, ErrExpired
	}
	if err = checkCascade(ctx, tx); err != nil {
		return false, err
	}
	if err = protected(ctx, tx, hash, false); err != nil {
		return false, err
	}
	if err = currentReceipt(ctx, tx, id, hash, "removed"); err != nil {
		return false, err
	}
	var exists bool
	if err = tx.QueryRow(ctx, `select exists(select 1 from torrents where info_hash=$1)`, hash).Scan(&exists); err != nil {
		return false, err
	}
	if exists {
		return false, ErrConflict
	}
	var p payload
	if err = json.Unmarshal(body, &p); err != nil {
		return false, err
	}
	canonical, err := json.Marshal(p)
	if err != nil {
		return false, err
	}
	sum := sha256.Sum256(canonical)
	if hex.EncodeToString(sum[:]) != digest {
		return false, ErrConflict
	}
	var schema []byte
	if err = tx.QueryRow(ctx, schemaQuery, tables).Scan(&schema); err != nil {
		return false, err
	}
	if !bytes.Equal(p["_schema"], schema) {
		return false, ErrConflict
	}
	if len(p) != len(tables)+1 {
		return false, ErrConflict
	}
	for _, table := range tables {
		b, ok := p[table]
		if !ok {
			return false, ErrConflict
		}
		if err = restoreTable(ctx, tx, table, b); err != nil {
			return false, fmt.Errorf("catalogue recovery: restore %s: %w", table, err)
		}
	}
	evID, err := recordVerdict(ctx, tx, id, hash, "restored", reason)
	if err != nil {
		return false, err
	}
	if _, err = tx.Exec(ctx, `update catalogue_recovery_snapshots set state='restored',restored_at=$2,restored_verdict_event_id=$3 where id=$1`, id, now, evID); err != nil {
		return false, err
	}
	if err = event(ctx, tx, id, "restored", now, reason); err != nil {
		return false, err
	}
	if err = s.storageCheck(ctx, tx); err != nil {
		return false, err
	}
	if err = tx.Commit(ctx); err != nil {
		return false, err
	}
	return true, nil
}

// Filter combines the caller's bloom result with exact, current recovery blocks
// and restores. Only the latest bound restoration may override a bloom hit.
// CSAM and other independent crawler gates must still run separately.
func (s *Store) Filter(ctx context.Context, all, bloomKept [][]byte) ([][]byte, error) {
	if !s.cfg.Enabled {
		return nil, ErrDisabled
	}
	if len(all) > 1024 {
		return nil, fmt.Errorf("catalogue recovery: filter batch exceeds 1024")
	}
	kept := map[string]bool{}
	for _, h := range bloomKept {
		kept[string(h)] = true
	}
	rows, err := s.pool.Query(ctx, `select distinct on(s.info_hash) s.info_hash,s.state,exists(select 1 from torrent_verdict_state v join torrent_verdict_events e on e.info_hash=v.info_hash where v.info_hash=s.info_hash and e.id=s.restored_verdict_event_id and e.id=(select max(id) from torrent_verdict_events where info_hash=s.info_hash) and v.verdict='restored' and v.mechanism='operator' and e.verdict=v.verdict and e.mechanism=v.mechanism and e.evidence=jsonb_build_object('catalogue_recovery_snapshot',s.id,'contract',s.contract_version,'transition','restored') and not exists(select 1 from junkpurge_quarantine q where q.info_hash=s.info_hash and q.expired_at is null)) from catalogue_recovery_snapshots s where info_hash=any($1) order by s.info_hash,s.id desc`, all)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var hash []byte
		var state string
		var valid bool
		if err = rows.Scan(&hash, &state, &valid); err != nil {
			return nil, err
		}
		if state == "removed" {
			kept[string(hash)] = false
		} else if state == "restored" {
			kept[string(hash)] = valid
		}
	}
	if err = rows.Err(); err != nil {
		return nil, err
	}
	out := make([][]byte, 0, len(all))
	for _, h := range all {
		if kept[string(h)] {
			out = append(out, h)
		}
	}
	return out, nil
}
func event(ctx context.Context, tx pgx.Tx, id int64, transition string, now time.Time, reason string) error {
	_, err := tx.Exec(ctx, `insert into catalogue_recovery_events(snapshot_id,transition,observed_at,reason) values($1,$2,$3,$4)`, id, transition, now, reason)
	return err
}
func restoreTable(ctx context.Context, tx pgx.Tx, table string, b json.RawMessage) error {
	var records []map[string]json.RawMessage
	if err := json.Unmarshal(b, &records); err != nil {
		return err
	}
	if len(records) == 0 {
		return nil
	}
	rows, err := tx.Query(ctx, `select column_name,is_generated,is_identity from information_schema.columns where table_schema=current_schema() and table_name=$1 order by ordinal_position`, table)
	if err != nil {
		return err
	}
	all := map[string]bool{}
	columns := []string{}
	for rows.Next() {
		var name, gen, identity string
		if err = rows.Scan(&name, &gen, &identity); err != nil {
			rows.Close()
			return err
		}
		all[name] = true
		if gen == "NEVER" && identity == "NO" {
			columns = append(columns, pgx.Identifier{name}.Sanitize())
		}
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	if len(columns) == 0 {
		return ErrConflict
	}
	for _, r := range records {
		if len(r) != len(all) {
			return ErrConflict
		}
		for key := range r {
			if !all[key] {
				return ErrConflict
			}
		}
	}
	name := pgx.Identifier{table}.Sanitize()
	col := strings.Join(columns, ",")
	q := `insert into ` + name + ` (` + col + `) select ` + col + ` from jsonb_populate_recordset(null::` + name + `,$1::jsonb)`
	if table == "torrent_sources" {
		q += ` on conflict(key) do nothing`
	}
	if _, err = tx.Exec(ctx, q, string(b)); err != nil {
		return err
	}
	// Verify generated columns, NULLs, bytes and complete shared-source rows too.
	where := `info_hash=(select info_hash from jsonb_populate_recordset(null::` + name + `,$1::jsonb) limit 1)`
	if table == "torrent_sources" {
		where = `key in(select key from jsonb_populate_recordset(null::torrent_sources,$1::jsonb))`
	}
	var matches bool
	err = tx.QueryRow(ctx, `select coalesce(jsonb_agg(to_jsonb(r) order by to_jsonb(r)::text),'[]'::jsonb)=$1::jsonb from `+name+` r where `+where, string(b)).Scan(&matches)
	if err != nil {
		return err
	}
	if !matches {
		return ErrConflict
	}
	return nil
}

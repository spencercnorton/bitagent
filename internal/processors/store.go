package processors

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/spencercnorton/bitagent/internal/classifier/contentfilter"
	"github.com/spencercnorton/bitagent/internal/lazy"
	"github.com/spencercnorton/bitagent/internal/namepolicy"
	v1 "github.com/spencercnorton/bitagent/processorapi/v1"
	"sort"
	"sync/atomic"
	"time"
)

var ErrConflict = errors.New("generation_conflict")
var ErrAbsent = errors.New("not_found")
var ErrStale = errors.New("stale_result")
var ErrCapacity = errors.New("queue_capacity")
var ErrRegistration = errors.New("registration_rejected")
var ErrControl = errors.New("invalid_control")

type Entry struct {
	ID                  string          `json:"id"`
	Generation          int64           `json:"generation"`
	ExecutionGeneration int64           `json:"execution_generation"`
	Endpoint            string          `json:"endpoint"`
	AuthEnv             string          `json:"auth_env"`
	Mode                string          `json:"mode"`
	State               string          `json:"state"`
	Priority            int             `json:"priority"`
	Manifest            v1.Manifest     `json:"manifest"`
	Configuration       json.RawMessage `json:"configuration"`
}
type Step struct {
	Entry Entry    `json:"entry"`
	Stage v1.Stage `json:"stage"`
}
type Plan struct {
	Epoch      int64               `json:"epoch"`
	CorePolicy string              `json:"core_policy"`
	Steps      []Step              `json:"steps"`
	Admission  *v1.AdmissionPolicy `json:"admission,omitempty"`
}
type Store struct {
	filter           *contentfilter.Filter
	forbiddenTypes   map[string]bool
	noiseV2          bool
	singleEpisodeMax int64
	names            *namepolicy.Policy
	active           atomic.Bool
	pool             lazy.Lazy[*pgxpool.Pool]
	cfg              Config
	policy           string
	transport        *Transport
	metrics          *Metrics
}

func NewStore(c Config, p lazy.Lazy[*pgxpool.Pool], policy string) (*Store, error) {
	if e := c.Validate(); e != nil {
		return nil, e
	}
	transport := NewTransport(c.RequestTimeout)
	transport.CredentialDirectory = c.CredentialDirectory
	return &Store{pool: p, cfg: c, policy: policy, transport: transport, metrics: NewMetrics()}, nil
}
func digest(v any) string {
	b, e := json.Marshal(v)
	if e != nil {
		panic(e)
	}
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}
func canonical(raw []byte) json.RawMessage {
	var v any
	if json.Unmarshal(raw, &v) != nil {
		return nil
	}
	b, _ := json.Marshal(v)
	return b
}
func (s *Store) begin(ctx context.Context) (pgx.Tx, error) {
	p, e := s.pool.Get()
	if e != nil {
		return nil, e
	}
	return p.Begin(ctx)
}
func rollback(tx pgx.Tx) {
	ctx, c := context.WithTimeout(context.Background(), time.Second)
	defer c()
	_ = tx.Rollback(ctx)
}

func entries(ctx context.Context, q interface {
	Query(context.Context, string, ...any) (pgx.Rows, error)
}) ([]Entry, error) {
	rows, e := q.Query(ctx, `SELECT id,generation,execution_generation,endpoint,auth_env,mode,state,priority,manifest,configuration FROM processor_registry ORDER BY priority,id`)
	if e != nil {
		return nil, e
	}
	defer rows.Close()
	out := []Entry{}
	for rows.Next() {
		var x Entry
		var m []byte
		if e = rows.Scan(&x.ID, &x.Generation, &x.ExecutionGeneration, &x.Endpoint, &x.AuthEnv, &x.Mode, &x.State, &x.Priority, &m, &x.Configuration); e != nil {
			return nil, e
		}
		if e = json.Unmarshal(m, &x.Manifest); e != nil {
			return nil, e
		}
		out = append(out, x)
	}
	return out, rows.Err()
}
func readPlan(ctx context.Context, tx pgx.Tx, policy string) (Plan, bool, error) {
	var p Plan
	p.CorePolicy = policy
	p.Steps = []Step{}
	if e := tx.QueryRow(ctx, `SELECT epoch FROM processor_control WHERE singleton FOR SHARE`).Scan(&p.Epoch); e != nil {
		return p, false, e
	}
	admission, e := readAdmission(ctx, tx)
	if e != nil {
		return p, false, e
	}
	p.Admission = &admission
	es, e := entries(ctx, tx)
	if e != nil {
		return p, false, e
	}
	draining := false
	for _, stage := range v1.Stages {
		for _, entry := range es {
			if entry.State == "paused" {
				continue
			}
			draining = draining || entry.State == "draining"
			for _, cap := range entry.Manifest.Capabilities {
				if stage == cap {
					entry.Generation = entry.ExecutionGeneration
					entry.State = "active"
					p.Steps = append(p.Steps, Step{entry, stage})
				}
			}
		}
	}
	return p, draining, nil
}
func (s *Store) Entries(ctx context.Context) ([]Entry, error) {
	p, e := s.pool.Get()
	if e != nil {
		return nil, e
	}
	return entries(ctx, p)
}
func validateWrite(w v1.RegistryWrite) error {
	if w.ExpectedGeneration < 0 || w.Priority < 0 || w.Priority > 1000 || (w.Mode != "shadow" && w.Mode != "apply") || (w.State != "active" && w.State != "paused" && w.State != "draining") {
		return errors.New("invalid_registration")
	}
	return validateEndpoint(w.Endpoint, w.AuthEnv)
}
func (s *Store) Register(ctx context.Context, id string, w v1.RegistryWrite) (Entry, error) {
	if e := validateWrite(w); e != nil {
		return Entry{}, fmt.Errorf("%w: %v", ErrRegistration, e)
	}
	bounded, cancel := context.WithTimeout(ctx, s.cfg.RequestTimeout)
	defer cancel()
	m, status, e := s.transport.Inspect(bounded, w)
	if e != nil {
		return Entry{}, fmt.Errorf("%w: %v", ErrRegistration, e)
	}
	if w.ExpectedProcessor != nil && *w.ExpectedProcessor != m.Processor {
		return Entry{}, fmt.Errorf("%w: expected_processor_mismatch", ErrRegistration)
	}
	if m.Processor.ID != id {
		return Entry{}, fmt.Errorf("%w: processor_id_mismatch", ErrRegistration)
	}
	tx, e := s.begin(ctx)
	if e != nil {
		return Entry{}, e
	}
	defer rollback(tx)
	if _, e = tx.Exec(ctx, `SELECT epoch FROM processor_control WHERE singleton FOR UPDATE`); e != nil {
		return Entry{}, e
	}
	var current int64
	e = tx.QueryRow(ctx, `SELECT generation FROM processor_registry WHERE id=$1 FOR UPDATE`, id).Scan(&current)
	if e != nil && !errors.Is(e, pgx.ErrNoRows) {
		return Entry{}, e
	}
	if current != w.ExpectedGeneration {
		return Entry{}, ErrConflict
	}
	var n int
	if e = tx.QueryRow(ctx, `SELECT count(*) FROM processor_registry`).Scan(&n); e != nil {
		return Entry{}, e
	}
	if current == 0 && n >= 16 {
		return Entry{}, ErrCapacity
	}
	var gen int64
	if e = tx.QueryRow(ctx, `SELECT nextval('processor_registry_generation')`).Scan(&gen); e != nil {
		return Entry{}, e
	}
	mb, _ := json.Marshal(m)
	ob, _ := json.Marshal(status)
	config := canonical(w.Configuration)
	_, e = tx.Exec(ctx, `INSERT INTO processor_registry(id,generation,execution_generation,endpoint,auth_env,mode,state,priority,manifest,configuration,observed,observed_at) VALUES($1,$2,$2,$3,$4,$5,$6,$7,$8,$9,$10,clock_timestamp())
 ON CONFLICT(id) DO UPDATE SET generation=$2,execution_generation=$2,endpoint=$3,auth_env=$4,mode=$5,state=$6,priority=$7,manifest=$8,configuration=$9,observed=$10,observed_at=clock_timestamp(),last_error='',updated_at=clock_timestamp()`, id, gen, w.Endpoint, w.AuthEnv, w.Mode, w.State, w.Priority, string(mb), string(config), string(ob))
	if e != nil {
		return Entry{}, e
	}
	if e = invalidate(ctx, tx); e != nil {
		return Entry{}, e
	}
	if e = tx.Commit(ctx); e != nil {
		return Entry{}, e
	}
	s.active.Store(true)
	return Entry{id, gen, gen, w.Endpoint, w.AuthEnv, w.Mode, w.State, w.Priority, m, config}, nil
}
func invalidate(ctx context.Context, tx pgx.Tx) error {
	return invalidateFor(ctx, tx, "registry_changed")
}
func invalidateFor(ctx context.Context, tx pgx.Tx, reason string) error {
	if _, e := tx.Exec(ctx, `UPDATE processor_control SET epoch=epoch+1,scan_after=''::bytea,updated_at=clock_timestamp() WHERE singleton`); e != nil {
		return e
	}
	_, e := tx.Exec(ctx, `WITH invalidated AS (
 UPDATE processor_jobs SET state='stale',reason=$1,lease_until=NULL,finished_at=clock_timestamp()
 WHERE state IN('queued','deferred','leased') RETURNING job_id,lease_generation)
 UPDATE processor_attempts a SET finished_at=clock_timestamp(),outcome='unknown',reason=$1
 FROM invalidated j WHERE a.job_id=j.job_id AND a.lease_generation=j.lease_generation AND a.finished_at IS NULL`, reason)
	return e
}
func (s *Store) Control(ctx context.Context, id string, c v1.RegistryControl) error {
	tx, e := s.begin(ctx)
	if e != nil {
		return e
	}
	defer rollback(tx)
	if _, e = tx.Exec(ctx, `SELECT epoch FROM processor_control WHERE singleton FOR UPDATE`); e != nil {
		return e
	}
	var gen, execGen int64
	var state, mode string
	e = tx.QueryRow(ctx, `SELECT generation,execution_generation,state,mode FROM processor_registry WHERE id=$1 FOR UPDATE`, id).Scan(&gen, &execGen, &state, &mode)
	if errors.Is(e, pgx.ErrNoRows) {
		return ErrAbsent
	}
	if e != nil {
		return e
	}
	if c.ExpectedGeneration != gen {
		return ErrConflict
	}
	newState, newMode := state, mode
	if c.State != nil {
		newState = *c.State
	}
	if c.Mode != nil {
		newMode = *c.Mode
	}
	if (newState != "active" && newState != "paused" && newState != "draining") || (newMode != "shadow" && newMode != "apply") {
		return ErrControl
	}
	if e = tx.QueryRow(ctx, `SELECT nextval('processor_registry_generation')`).Scan(&gen); e != nil {
		return e
	}
	// Drain/resume changes admission only. A mode or pause transition cancels
	// execution and creates a fresh immutable execution generation.
	changed := mode != newMode || state == "paused" && newState != "paused" || state != "paused" && newState == "paused"
	if changed {
		execGen = gen
		if e = invalidate(ctx, tx); e != nil {
			return e
		}
	}
	_, e = tx.Exec(ctx, `UPDATE processor_registry SET generation=$2,execution_generation=$3,state=$4,mode=$5,updated_at=clock_timestamp() WHERE id=$1`, id, gen, execGen, newState, newMode)
	if e != nil {
		return e
	}
	return tx.Commit(ctx)
}
func (s *Store) Remove(ctx context.Context, id string, gen int64) error {
	tx, e := s.begin(ctx)
	if e != nil {
		return e
	}
	defer rollback(tx)
	if _, e = tx.Exec(ctx, `SELECT epoch FROM processor_control WHERE singleton FOR UPDATE`); e != nil {
		return e
	}
	tag, e := tx.Exec(ctx, `DELETE FROM processor_registry WHERE id=$1 AND generation=$2`, id, gen)
	if e != nil {
		return e
	}
	if tag.RowsAffected() != 1 {
		return ErrConflict
	}
	if e = invalidate(ctx, tx); e != nil {
		return e
	}
	return tx.Commit(ctx)
}
func (s *Store) Cancel(ctx context.Context, id string) (string, error) {
	p, e := s.pool.Get()
	if e != nil {
		return "", e
	}
	_, e = p.Exec(ctx, `WITH canceled AS (
 UPDATE processor_jobs SET state='canceled',reason='operator_canceled',lease_until=NULL,finished_at=clock_timestamp()
 WHERE job_id=$1 AND state IN('queued','deferred','leased') RETURNING job_id,lease_generation)
 UPDATE processor_attempts a SET finished_at=clock_timestamp(),outcome='unknown',reason='operator_canceled'
 FROM canceled j WHERE a.job_id=j.job_id AND a.lease_generation=j.lease_generation AND a.finished_at IS NULL`, id)
	if e != nil {
		return "", e
	}
	var state string
	e = p.QueryRow(ctx, `SELECT state FROM processor_jobs WHERE job_id=$1`, id).Scan(&state)
	return state, e
}
func sortFiles(files []v1.File) {
	sort.Slice(files, func(i, j int) bool {
		if files[i].Path == files[j].Path {
			return files[i].Size < files[j].Size
		}
		return files[i].Path < files[j].Path
	})
}
func (s *Store) String() string {
	return fmt.Sprintf("optional processors (capacity %d)", s.cfg.QueueCapacity)
}

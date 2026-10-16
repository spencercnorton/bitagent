package processors

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"regexp"
	"time"

	"github.com/jackc/pgx/v5"
	v1 "github.com/spencercnorton/bitagent/processorapi/v1"
)

var admissionVersion = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._-]{0,95}$`)

func readAdmission(ctx context.Context, tx pgx.Tx) (v1.AdmissionPolicy, error) {
	p := v1.AdmissionPolicy{Contract: v1.AdmissionContract, Algorithm: v1.AdmissionAlgorithm}
	err := tx.QueryRow(ctx, `SELECT admission_generation,admission_basis_points,admission_seed,admission_policy_version
 FROM processor_control WHERE singleton`).Scan(&p.Generation, &p.BasisPoints, &p.Seed, &p.PolicyVersion)
	return p, err
}

func (s *Store) Admission(ctx context.Context) (v1.AdmissionPolicy, error) {
	tx, err := s.begin(ctx)
	if err != nil {
		return v1.AdmissionPolicy{}, err
	}
	defer rollback(tx)
	return readAdmission(ctx, tx)
}

func (s *Store) AdmissionHistory(ctx context.Context, before int64, limit int) (v1.AdmissionHistory, error) {
	out := v1.AdmissionHistory{Items: []v1.AdmissionChange{}}
	if before < 0 || limit < 1 || limit > 100 {
		return out, ErrControl
	}
	p, err := s.pool.Get()
	if err != nil {
		return out, err
	}
	rows, err := p.Query(ctx, `SELECT policy,reason,recorded_at FROM processor_admission_history
 WHERE ($1=0 OR generation<$1) ORDER BY generation DESC LIMIT $2`, before, limit+1)
	if err != nil {
		return out, err
	}
	defer rows.Close()
	for rows.Next() {
		var change v1.AdmissionChange
		var raw []byte
		var at time.Time
		if err = rows.Scan(&raw, &change.Reason, &at); err != nil {
			return out, err
		}
		if err = json.Unmarshal(raw, &change.Policy); err != nil {
			return out, err
		}
		change.RecordedAt = stamp(at)
		if len(out.Items) == limit {
			out.NextBefore = out.Items[limit-1].Policy.Generation
			break
		}
		out.Items = append(out.Items, change)
	}
	return out, rows.Err()
}

// admissionBucket is a published, reproducible cohort rule. Hash inputs are
// fixed length, domain-separated bytes; neither model output nor catalogue
// matches can steer selection. A fixed seed gives nested percentage cohorts.
func admissionBucket(hash []byte, seed string) (uint64, error) {
	b, err := hex.DecodeString(seed)
	if err != nil || len(b) != 32 || len(hash) != 20 || hex.EncodeToString(b) != seed {
		return 0, ErrControl
	}
	h := sha256.New()
	h.Write([]byte(v1.AdmissionContract + "\n"))
	h.Write(b)
	h.Write(hash)
	return binary.BigEndian.Uint64(h.Sum(nil)[:8]) % 10000, nil
}

func admittedByPolicy(hash []byte, p *v1.AdmissionPolicy) bool {
	if p == nil || p.Contract != v1.AdmissionContract || p.Algorithm != v1.AdmissionAlgorithm {
		return false
	}
	bucket, err := admissionBucket(hash, p.Seed)
	return err == nil && p.BasisPoints >= 0 && p.BasisPoints <= 10000 && bucket < uint64(p.BasisPoints)
}

func (s *Store) SetAdmission(ctx context.Context, w v1.AdmissionWrite) (v1.AdmissionPolicy, error) {
	if _, err := admissionBucket(make([]byte, 20), w.Seed); err != nil || w.ExpectedGeneration < 1 ||
		w.BasisPoints < 0 || w.BasisPoints > 10000 || !admissionVersion.MatchString(w.PolicyVersion) || !admissionVersion.MatchString(w.Reason) {
		return v1.AdmissionPolicy{}, ErrControl
	}
	tx, err := s.begin(ctx)
	if err != nil {
		return v1.AdmissionPolicy{}, err
	}
	defer rollback(tx)
	if _, err = tx.Exec(ctx, `SELECT epoch FROM processor_control WHERE singleton FOR UPDATE`); err != nil {
		return v1.AdmissionPolicy{}, err
	}
	p, err := readAdmission(ctx, tx)
	if err != nil {
		return p, err
	}
	if w.ExpectedGeneration != p.Generation {
		return p, ErrConflict
	}
	if w.Seed == p.Seed && w.BasisPoints == p.BasisPoints && w.PolicyVersion == p.PolicyVersion {
		return p, nil
	}
	p.Generation++
	p.Seed, p.BasisPoints, p.PolicyVersion = w.Seed, w.BasisPoints, w.PolicyVersion
	raw, _ := json.Marshal(p)
	_, err = tx.Exec(ctx, `UPDATE processor_control SET admission_generation=$1,admission_basis_points=$2,
 admission_seed=$3,admission_policy_version=$4 WHERE singleton`, p.Generation, p.BasisPoints, p.Seed, p.PolicyVersion)
	if err != nil {
		return p, err
	}
	if err = invalidateFor(ctx, tx, "admission_changed"); err != nil {
		return p, err
	}
	// Rollout populations must not mix across admission policies. Also advance
	// the administrative CAS so a client cannot unknowingly control an old view.
	_, err = tx.Exec(ctx, `WITH generations AS (SELECT id,nextval('processor_registry_generation') AS generation FROM processor_registry)
 UPDATE processor_registry r SET generation=g.generation,execution_generation=g.generation,updated_at=clock_timestamp()
 FROM generations g WHERE g.id=r.id`)
	if err != nil {
		return p, err
	}
	_, err = tx.Exec(ctx, `INSERT INTO processor_admission_history(generation,policy,reason) VALUES($1,$2,$3)`, p.Generation, string(raw), w.Reason)
	if err != nil {
		return p, err
	}
	return p, tx.Commit(ctx)
}

package llmcapture

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"

	"github.com/jackc/pgx/v5"
)

// admittedSemanticIdentity reads only immutable capture facts. The caller's
// digest is a consistency check, never the source of equivalence authority.
func admittedSemanticIdentity(ctx context.Context, tx pgx.Tx, req DispatchRequest) (SemanticCaptureIdentity, []byte, error) {
	var identity SemanticCaptureIdentity
	err := tx.QueryRow(ctx, `SELECT c.task,COALESCE(c.candidate_source,''),c.sampling_origin,
 c.source_sha256,c.group_sha256,c.input_sha256,c.prompt_sha256,c.endpoint_sha256,
 c.model,c.prompt_version,c.contract_id
 FROM llm_evaluation_captures c WHERE c.capture_key=$1 FOR SHARE`, req.CaptureKey).
		Scan(&identity.Task, &identity.CandidateSource, &identity.SamplingOrigin, &identity.SourceSHA256,
			&identity.GroupSHA256, &identity.InputSHA256, &identity.PromptSHA256, &identity.EndpointSHA256,
			&identity.Model, &identity.PromptVersion, &identity.ContractID)
	if err != nil {
		return identity, nil, ErrCaptureUnavailable
	}
	source := namespacedDigest("bitagent-llm-evaluation-source-v1", req.InfoHash)
	if identity.Task != req.Task || identity.CandidateSource != req.CandidateSource || !bytes.Equal(identity.SourceSHA256, source[:]) {
		return identity, nil, ErrCaptureUnavailable
	}
	semantic, err := identity.Key()
	if err != nil {
		return identity, nil, err
	}
	if len(req.SemanticKey) > 0 && !bytes.Equal(req.SemanticKey, semantic) {
		return identity, nil, ErrCaptureUnavailable
	}
	return identity, semantic, nil
}

// resolveSemanticFence is called under the semantic advisory lock. Existing
// semantic authority wins, including when all its raw capture data expired.
// Older retained equal captures with no dispatch proof conservatively hold a
// new build: recording a new build cannot prove the older request undispatched.
func resolveSemanticFence(ctx context.Context, tx pgx.Tx, req DispatchRequest, identity SemanticCaptureIdentity, semantic []byte) ([]byte, bool, error) {
	var fence []byte
	err := tx.QueryRow(ctx, `SELECT capture_key FROM llm_capture_dispatch_attempts WHERE semantic_key=$1`, semantic).Scan(&fence)
	if err == nil {
		return fence, false, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return nil, false, err
	}
	var priorSemantic []byte
	err = tx.QueryRow(ctx, `SELECT capture_key,semantic_key FROM llm_capture_dispatch_attempts WHERE capture_key=$1`, req.CaptureKey).Scan(&fence, &priorSemantic)
	if err == nil {
		if len(priorSemantic) > 0 && !bytes.Equal(priorSemantic, semantic) {
			return nil, false, ErrCaptureUnavailable
		}
		return fence, false, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return nil, false, err
	}
	var proven bool
	err = tx.QueryRow(ctx, `SELECT COALESCE(d.capture_key,c.capture_key),d.capture_key IS NOT NULL
 FROM llm_evaluation_captures c LEFT JOIN llm_capture_dispatch_attempts d
 ON COALESCE(d.active_capture_key,d.capture_key)=c.capture_key
 WHERE c.capture_key<>$1 AND c.task=$2 AND COALESCE(c.candidate_source,'')=$3 AND c.sampling_origin=$4
 AND c.source_sha256=$5 AND c.group_sha256=$6 AND c.input_sha256=$7 AND c.prompt_sha256=$8
 AND c.endpoint_sha256=$9 AND c.model=$10 AND c.prompt_version=$11 AND c.contract_id=$12
 ORDER BY c.captured_at,c.capture_key LIMIT 1`, req.CaptureKey, string(identity.Task), string(identity.CandidateSource),
		string(identity.SamplingOrigin), identity.SourceSHA256, identity.GroupSHA256, identity.InputSHA256, identity.PromptSHA256,
		identity.EndpointSHA256, identity.Model, identity.PromptVersion, identity.ContractID).Scan(&fence, &proven)
	if errors.Is(err, pgx.ErrNoRows) {
		return append([]byte(nil), req.CaptureKey...), false, nil
	}
	if err != nil {
		return nil, false, err
	}
	return fence, !proven, nil
}

func linkDispatchAlias(ctx context.Context, tx pgx.Tx, capture, fence []byte) error {
	var existing []byte
	err := tx.QueryRow(ctx, `INSERT INTO llm_capture_dispatch_aliases(capture_key,fence_key) VALUES($1,$2)
 ON CONFLICT(capture_key) DO UPDATE SET fence_key=llm_capture_dispatch_aliases.fence_key RETURNING fence_key`, capture, fence).Scan(&existing)
	if err != nil {
		return err
	}
	if !bytes.Equal(existing, fence) {
		return ErrCaptureUnavailable
	}
	return nil
}

func dispatchFenceKey(lease DispatchLease) []byte {
	if len(lease.FenceKey) == sha256.Size {
		return lease.FenceKey
	}
	return lease.CaptureKey
}

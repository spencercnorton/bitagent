package llmwork

import (
	"bytes"
	"context"
	"encoding/json"

	"github.com/spencercnorton/bitagent/internal/model"
)

// Submit returns nil only to an owned worker for this exact source/policy.
// Ingestion retains its ordinary result and never waits for provider capacity.
func (s *Store) Submit(ctx context.Context, kind Kind, t model.Torrent, policy, input, payload, family any, daily, monthly int) error {
	if !s.Enabled() || !s.cfg.Accepts(kind) {
		return nil
	}
	source, policyHash := SourceDigest(t), Digest(policy)
	if e := ExecutionFrom(ctx); e != nil {
		if e.Store == s && e.Matches(kind, t.InfoHash.Bytes(), source, policyHash) && bytes.Equal(e.Lease.Task.InputDigest, Digest(input)) {
			return nil
		}
		return ErrObsolete
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	outcome, err := s.Enqueue(ctx, Draft{Kind: kind, InfoHash: t.InfoHash.Bytes(), SourceDigest: source, PolicyDigest: policyHash,
		InputDigest: Digest(input), FamilyDigest: Digest(family), Payload: body, DailyLimit: daily, MonthlyLimit: monthly})
	if err != nil {
		return err
	}
	if outcome == "completed" {
		return ErrReplayOnly
	}
	return ErrDeferred
}

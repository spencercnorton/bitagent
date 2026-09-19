package dhtcrawler

import (
	"context"
	"encoding/json"

	"github.com/spencercnorton/bitagent/internal/protocol"
	"github.com/spencercnorton/bitagent/internal/verdicts"
)

// Phase-C verdict dual-writes for the crawler's two currently-unrecorded
// mechanisms (design §2.3): csam (community-feed pre-fetch rejects) and
// blocking (the ban-check auto-block). All are recording-only,
// blacklisted, best-effort log-and-continue — the ledger must never break
// the crawl path — and OUTSIDE any bloom/DB transaction (Record opens its
// own). Volume is low and bounded: csam-feed hits and ban failures are rare,
// and ignoreHashes suppresses re-processing a hash before it reaches these
// sites, so each hash is recorded roughly once.

// recordVerdict best-effort writes one blacklisted verdict. Nil-safe.
func (c *crawler) recordVerdict(ctx context.Context, hash protocol.ID, mechanism, reason string, evidence []byte) {
	if c.verdicts == nil {
		return
	}
	if err := c.verdicts.Record(ctx, verdicts.Event{
		InfoHash:  hash.Bytes(),
		Verdict:   verdicts.VerdictBlacklisted,
		Mechanism: mechanism,
		Reason:    reason,
		Evidence:  evidence,
	}); err != nil {
		c.logger.Warnw("crawler verdict record", "mechanism", mechanism, "err", err)
	}
}

func csamEvidence(site string) []byte {
	b, err := json.Marshal(map[string]string{"source": "community_feed", "site": site})
	if err != nil {
		return nil
	}
	return b
}

// removedHashes returns the hashes in all that are NOT in kept — the subset
// the Filter dropped. Filter only ever removes, so this is the reject set.
func removedHashes(all, kept []protocol.ID) []protocol.ID {
	if len(all) == len(kept) {
		return nil
	}
	keptSet := make(map[protocol.ID]struct{}, len(kept))
	for _, h := range kept {
		keptSet[h] = struct{}{}
	}
	var out []protocol.ID
	for _, h := range all {
		if _, ok := keptSet[h]; !ok {
			out = append(out, h)
		}
	}
	return out
}

// recordCsamRejects records a csam blacklist for each hash the community-feed
// Filter removed (all minus kept). site distinguishes the triage batch from
// the BEP-9 egress re-check. Evidence is a label only — NEVER the matched
// keyword, title, or raw hash (design §3 redaction posture).
//
// csam verdicts are NON-operator-restorable (design §3/§6-Q3): any future
// ledger-based restore path MUST refuse to restore a mechanism=csam verdict.
func (c *crawler) recordCsamRejects(ctx context.Context, all, kept []protocol.ID, site string) {
	if c.verdicts == nil {
		return
	}
	removed := removedHashes(all, kept)
	if len(removed) == 0 {
		return // common path: no CSAM hit — skip the evidence marshal
	}
	ev := csamEvidence(site)
	for _, h := range removed {
		c.recordVerdict(ctx, h, verdicts.MechanismCsam, "community-feed CSAM reject ("+site+")", ev)
	}
}

// recordCsamReject records a single csam blacklist (the per-hash egress
// re-check, where the batch diff is unavailable).
func (c *crawler) recordCsamReject(ctx context.Context, hash protocol.ID, site string) {
	c.recordVerdict(ctx, hash, verdicts.MechanismCsam, "community-feed CSAM reject ("+site+")", csamEvidence(site))
}

// recordBlockingVerdict records a blocking blacklist for a ban-check
// auto-block (the blocking manager's own choke, driven by banningChecker).
func (c *crawler) recordBlockingVerdict(ctx context.Context, hash protocol.ID) {
	c.recordVerdict(ctx, hash, verdicts.MechanismBlocking, "banning check failed; blocked", nil)
}

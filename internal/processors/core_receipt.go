package processors

import (
	"context"
	llmwork "github.com/spencercnorton/bitagent/internal/legacyresults"
	"github.com/spencercnorton/bitagent/internal/processingstate"
	"time"
)

// Receipt writes are best effort and bounded AFTER ordinary core persistence.
// Missing evidence is explicitly not_attempted; it never blocks the core or
// certifies a miss. No optional registrations means no database work here.
func (s *Store) RecordCore(ctx context.Context, rs []processingstate.Receipt) {
	if !s.active.Load() || len(rs) == 0 {
		return
	}
	ctx, c := context.WithTimeout(ctx, 100*time.Millisecond)
	defer c()
	p, e := s.pool.Get()
	if e != nil {
		return
	}
	for _, r := range rs {
		_, e = p.Exec(ctx, `INSERT INTO processor_core_receipts(info_hash,source_digest,content_id,outcome,core_policy,processing_outcome)
 SELECT $1,$2,$3,$4,$5,$6 WHERE EXISTS(SELECT 1 FROM torrent_contents WHERE info_hash=$1 AND id=$3)
 ON CONFLICT(info_hash) DO UPDATE SET source_digest=$2,content_id=$3,outcome=$4,core_policy=$5,processing_outcome=$6,recorded_at=clock_timestamp()`, r.Torrent.InfoHash.Bytes(), llmwork.SourceDigest(r.Torrent), r.ContentID, r.Outcome, s.policy, r.Processing)
		if e != nil {
			s.metrics.admission.WithLabelValues("core_receipt_unavailable").Inc()
			return
		}
	}
}

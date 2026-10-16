package processors

import (
	"context"
	llmwork "github.com/spencercnorton/bitagent/internal/legacyresults"
	"github.com/spencercnorton/bitagent/internal/processingstate"
	"time"
)

// Receipt writes are best effort and bounded AFTER ordinary core persistence.
// Missing evidence is explicitly not_attempted; it never blocks the core or
// certifies a miss. Without registrations it writes only for an explicitly
// requested, correlated core reprocessing operation.
func (s *Store) RecordCore(ctx context.Context, rs []processingstate.Receipt) {
	requested := false
	for _, r := range rs {
		requested = requested || r.ReprocessRequestID != ""
	}
	if (!s.active.Load() && !requested) || len(rs) == 0 {
		return
	}
	ctx, c := context.WithTimeout(ctx, 100*time.Millisecond)
	defer c()
	p, e := s.pool.Get()
	if e != nil {
		return
	}
	for _, r := range rs {
		_, e = p.Exec(ctx, `WITH saved AS (INSERT INTO processor_core_receipts(info_hash,source_digest,content_id,outcome,core_policy,processing_outcome)
 SELECT $1,$2,$3,$4,$5,$6 WHERE EXISTS(SELECT 1 FROM torrent_contents WHERE info_hash=$1 AND id=$3)
 ON CONFLICT(info_hash) DO UPDATE SET source_digest=$2,content_id=$3,outcome=$4,core_policy=$5,processing_outcome=$6,recorded_at=clock_timestamp() RETURNING *)
 UPDATE processor_reprocess_requests SET core_receipt=(SELECT to_jsonb(saved) FROM saved),updated_at=clock_timestamp()
 WHERE request_id=$7 AND info_hash=$1 AND state='waiting_core' AND EXISTS(SELECT 1 FROM saved)`, r.Torrent.InfoHash.Bytes(), llmwork.SourceDigest(r.Torrent), r.ContentID, r.Outcome, s.policy, r.Processing, r.ReprocessRequestID)
		if e != nil {
			s.metrics.admission.WithLabelValues("core_receipt_unavailable").Inc()
			return
		}
	}
}

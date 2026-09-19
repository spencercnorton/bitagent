// Package peerrep tracks per-peer history of BEP-9 metadata-fetch
// attempts so the crawler can stop hammering peers that consistently
// fail. The 2026-04-24 audit identified BEP-9 as the dominant cost
// center: 8.68M errors against 207k successes (~2.3% success rate),
// with most errors being transport timeouts to dead or NAT-bound
// peers that will never reply. Deduplicating those repeated futile
// attempts is the GPT-5.5-pro review's #1 architectural recommendation
// (D3 in the original audit report).
//
// Design constraints:
//
//  1. **Cheap.** O(1) lookup per attempt. The hot path is the BEP-9
//     fetcher's per-peer decision; adding a slow check here would
//     just trade timeout-cost for lookup-cost.
//
//  2. **Shadow-mode first.** The first deploy stage observes only —
//     decisions are logged + emitted as metrics but the fetcher
//     still attempts every peer. After a window of data, an operator
//     flips `ENFORCE=true` and skipped attempts actually skip.
//
//  3. **Staged backoff by error class.** A peer that timed out once
//     might be fine in 10 minutes. A peer that responded with KRPC
//     "method unknown" probably never will. The penalty schedule is
//     keyed on the *kind* of failure, not just the count.
//
//  4. **Success clears.** A successful BEP-9 handshake resets the
//     peer's penalty so we don't accumulate poison from a flaky-then-
//     recovered tracker.
//
//  5. **No assumption that errors are independent of infohash.** A
//     peer might lack `ut_metadata` for one swarm but have it for
//     another. The store keys on (peer-only) for now; per-infohash
//     state would be a v2 if we ever see evidence it matters.
//
// Wiring is deliberately deferred to a follow-up MR — this MR ships
// the data structures + decision logic + tests, in shadow mode. The
// actual call-site wrap into metainforequester.Requester comes after
// the operator confirms the metric numbers from a shadow run match
// expectations.
package peerrep

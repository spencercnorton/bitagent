// Package priors learns per-feature Beta(α, β) success priors from
// observed *arr grab outcomes and re-ranks Torznab search results
// according to the posterior expected success rate of each candidate.
//
// Pipeline
//
//  1. Grab webhook arrives  → resolver extracts features from the
//     release title + source list, persists a torrent_grab_attempts
//     row with the frozen feature set, returns.
//  2. Import webhook arrives → resolver finds pending grab attempts
//     for the same infohash, increments α on every feature key, marks
//     them resolved with outcome='success'.
//  3. Expirer cron fires every ExpirerInterval → for every pending
//     grab attempt older than ResolutionWindow, increments β on every
//     feature key, marks them resolved with outcome='failure'.
//  4. Torznab search → ranker reads priors for the candidate result
//     features, computes posterior expected success per item, sorts
//     items by score×log(1+seeders), and returns the re-ranked list.
//
// Why this works cheaply
//
// Updates are O(1) UPSERTs against a small fixed-size table (~5K
// rows steady state — one row per (key_type, key_value) pair).
// Ranking is O(R·F) with R≤50 and F≤6, executed entirely in-memory
// after a single batched key lookup. No LLM calls, no external API
// traffic beyond the webhooks already received.
//
// Shadow mode
//
// OutcomePriorsConfig.Apply gates the actual re-ranking; the resolver
// and expirer run regardless. Operators can collect priors for one
// observation window, inspect the histogram, and only then flip
// Apply=true to engage the ranking effect.
//
// Why a separate table for grab attempts
//
// label_evidence is append-only audit. Driving the resolution state
// machine from there would require either a second projection or a
// scan-the-tail pattern that grows with the audit log. The
// torrent_grab_attempts table is the short-lived projection: rows
// land on Grab and are evicted on Import (or on expiry). Resolved
// rows are kept long enough for the expirer to mark them; a periodic
// vacuum job (separate concern) prunes resolved rows older than 30d
// to keep the table small.
package priors

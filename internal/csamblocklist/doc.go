// Package csamblocklist is the pre-fetch CSAM-defense layer for the
// DHT crawler. It maintains an in-memory bloom filter of double-hashed
// infohashes loaded from configured external feeds, and is consulted
// before any BEP-9 metadata fetch is initiated.
//
// # Why a separate layer from BlockingManager
//
// BlockingManager (internal/blocking) tracks infohashes this instance
// has personally observed and post-classified. Its filter is populated
// reactively — a CSAM hash only enters it after bitagent has already
// fetched metadata, classified the title, and been told to delete.
//
// The exposure window between "first observed in the swarm" and
// "post-fetch classification rejects" is the core problem raised in
// upstream issue #494. A peer-tracking service watching the swarm
// (e.g. "I know what you download") sees the operator's IP transiently
// connected to known-bad swarms even when the post-fetch defense
// works correctly.
//
// This package closes that window for *community-known* hashes:
//
//   - Operator subscribes to one or more feed URLs
//   - Each feed is a list of double-hashes — SHA-256(20-byte SHA-1
//     infohash). The hashes are one-way; the public list is not
//     itself a directory of CSAM.
//   - On every DHT-discovered infohash, we compute the same
//     double-hash and reject if matched, BEFORE any BEP-9 fetch.
//
// # Honest limits
//
// First observation across the network still incurs one BEP-9 fetch
// per network — the post-fetch CEL classifier (`keywords.banned`) +
// BlockingManager + this package's self-export pipeline together
// turn that single first-observation into a community signal that
// closes the window for everyone else. The defense converges, it does
// not eliminate.
//
// # Layered defense (in order of operation)
//
//  1. csamblocklist.Filter — pre-fetch reject for community-known
//     double-hashes. This package.
//  2. BlockingManager.Filter — pre-fetch reject for self-observed
//     hashes. internal/blocking.
//  3. CEL classifier `keywords.banned` workflow — post-fetch delete
//     action when title or file paths match. internal/classifier.
//  4. processor → BlockingManager.Block — when (3) fires, the hash
//     enters this instance's BlockingManager so future DHT
//     announcements are caught at (2).
//  5. csamblocklist export — when (3) fires AND the title matches
//     `keywords.banned`, the double-hash is appended to a local
//     JSONL log; an opt-in upstream POST contributes to community
//     feeds so other instances catch it at (1).
//
// # Defaults
//
//   - Enabled by default (CSAM_BLOCKLIST_ENABLED=true)
//   - No feeds configured by default → Manager is effectively NoOp
//     until the operator opts into a feed
//   - Self-export ENABLED writing locally; outbound POST OFF until
//     operator opts in
//
// See docs/csam-defense.md for the full architecture and operator
// guidance.
package csamblocklist

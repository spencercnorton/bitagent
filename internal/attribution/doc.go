// Package attribution builds an end-to-end correlation report of
// what BitAgent's torznab indexer served vs what the *arr stack
// actually grabbed, downloaded, and imported.
//
// # Why this exists
//
// gpt-5.5-pro's senior review (2026-04-25) flagged a load-bearing
// gap in the previous claims: every "8.4% grab rate" / "18% liveness
// miss" / "38.3% drop rate" assertion lacked an attribution chain
// to verify it. Without one, those numbers are folklore.
//
// The chain we need to reconstruct, per single info_hash:
//
//	bitagent torznab result   "I told Prowlarr X seeders, Y leechers"
//	    ↓
//	Prowlarr grab event       "Sonarr/Radarr/Lidarr selected this release"
//	    ↓
//	qBittorrent state         "qB picked it up, current state is downloading
//	                           / stalledDL / stalledUP / errored / completed"
//	    ↓
//	*arr import outcome       "Sonarr imported it / failed / replaced / blocked"
//
// Joining all three lets us answer questions the previous session
// couldn't:
//
//   - When BitAgent says "5 seeders" and qB lands on stalledDL, was
//     it BitAgent's seed estimate that lied, OR is qB stalled for an
//     unrelated reason (disk pressure / tracker auth / NAT)?
//   - What's the actual end-to-end grab→import success rate for
//     bitagent-served torrents vs other indexers?
//   - Are content-filter ENFORCE drops costing us legitimate grabs
//     elsewhere in the *arr stack?
//
// # Scope of the first cut
//
// One-shot CLI: `bitagent attribution recon`. Pulls from:
//
//	Prowlarr  /api/v1/history?eventType=releaseGrabbed
//	          → grab events with indexer name, title, info_hash, *arr
//	            source
//	qBittorrent /api/v2/torrents/info?hashes=...
//	          → state, category, last activity, tracker errors
//	*arr      /api/v3/history (Sonarr/Radarr) or /api/v1/history (Lidarr)
//	          → import outcome per info_hash
//
// Joins on info_hash. Prints a pretty table. NO database writes,
// NO worker. Operator runs on demand to spot-check the chain or
// produce ad-hoc reports.
//
// A future MR can promote this to a periodic worker that persists
// the joined rows so we can build dashboards and false-positive
// audits, but that's scope creep for now.
//
// # Failure modes (all degrade gracefully)
//
//   - Prowlarr unreachable → return empty grab list, log warn,
//     exit cleanly. The downstream joins have nothing to do.
//   - qB unreachable → fill qB columns with "?", continue.
//   - One *arr unreachable → fill that source's import column
//     with "?".
//
// All credential failures are reported once at startup; the CLI
// proceeds with whatever subset of sources is reachable, so a
// partially-configured operator can still get value.
//
// # Status
//
// **First cut as of 2026-04-25** — package + CLI surface + Prowlarr
// poller + qB client + *arr history adapter + a join function.
// Reuses wantbridge's *arr base-URL/key env vars
// (WANTBRIDGE_SONARR_BASE_URL, etc.) when present so the operator
// doesn't double-configure. New env required only for Prowlarr +
// qB.
package attribution

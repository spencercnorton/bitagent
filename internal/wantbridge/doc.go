// Package wantbridge bridges the DHT firehose to *arr (Sonarr /
// Radarr / Lidarr) wantlists. It's the centerpiece of the operator's
// 2026-04-25 north-star request: "an ultra lean highly effective
// system at bridging DHT items to requests in sonarr and radarr."
//
// STATUS: observe-only / experimental (as of 2026-06-02). Match() is
// wired into the dhtcrawler hot path and emits the bitagent_wantbridge_*
// metrics, but its result is DISCARDED — there is no fetch
// prioritisation yet. Enabling wantbridge (WANTBRIDGE_ENABLED=true)
// does NOT change BEP-9 fetch order, does NOT skip Tier-2 discoveries,
// and does NOT change Torznab results. The priority-queue / persist-
// annotation wiring described below (D4/D5) is deferred to a follow-up
// MR. Treat the metrics as a shadow/counterfactual measurement, not a
// live hit-rate feature.
//
// # Why this exists
//
// The DHT throws ~3M torrents/year at us; the BEP-9 fetcher succeeds
// ~2.3% of the time; most of the persisted corpus is content nothing
// in the *arr stack wants. Wantbridge inverts the polarity: instead
// of crawling everything and letting *arr poll a torznab indexer
// for matches, we pull the *arr wantlists upstream, fingerprint
// every DHT discovery against them, and prioritise BEP-9 fetches
// for hashes that are likely to satisfy a known want.
//
// # The wantlist source surface
//
// Each *arr exposes its wantlist via REST. We only need a small
// projection:
//
//	Sonarr:  /api/v3/series          ->  monitored, status, title, year, tvdbId,
//	         /api/v3/episode?seriesId=…   monitored episodes per season
//	Radarr:  /api/v3/movie            ->  monitored, status, title, year, tmdbId
//	Lidarr:  /api/v1/artist           ->  monitored, name
//
// Wantbridge polls each source on a configurable cadence (default
// 5 min). One missed cycle is harmless; the previous fingerprint
// set stays live. A persistently-unreachable *arr logs a warn but
// does not block the rest.
//
// # Fingerprinting
//
// A torrent name like "The.Wire.S01E03.Lessons.1080p.BluRay.x264"
// must collapse to "the wire s01" or "the wire" so the wantlist
// match works. We reuse the existing release-tag normalizer from
// internal/classifier/contentfilter (normalizeTitle) plus a few
// season/episode extractors:
//
//	normalised  = the wire s01
//	tokens      = [the, wire, s01]
//	canonical   = (kind=tv, title="the wire", season=1)
//
// The wantlist becomes the same shape:
//
//	canonical   = (kind=tv, title="the wire", season=1, episode_set=[3])
//
// Match is "wantlist canonical encompasses torrent canonical." A
// season pack matches; an episode in a wanted season matches; an
// episode in an unwanted season does not.
//
// # Fast-path: bloom filter for "definitely no match"
//
// The DHT firehose is high-volume. Every discovery hits Match() on
// the hot path. Naive linear search across N*arr_entries is too
// slow at high N. The implementation maintains a SipHash-keyed
// bloom filter sized for the union of all wantlists; on Match,
// the bloom is the first gate. False positives fall through to
// the exact-match table; negatives short-circuit instantly.
//
// # Three-tier prioritisation
//
// The DHT crawler's BEP-9 fetch queue grows a priority shim that
// consults Match() before enqueueing. Three tiers (see Tier const):
//
//	Tier0  high-confidence wantlist match -> jump the queue
//	Tier1  no match but the hash is otherwise unremarkable -> normal queue
//	Tier2  contentfilter would drop pre-fetch -> skip entirely (no enqueue)
//
// Tier 2 is where the ultra-lean win lives: hashes that the
// deterministic content filter would drop after metadata fetch are
// recognised by NAME alone (script / extension hint / NSFW keyword)
// and never enter the fetcher. This compounds across the corpus —
// at today's stats, ~16% of new discoveries are Tier 2 candidates,
// each worth one BEP-9 round trip we don't have to make.
//
// # Push (D5) vs poll (legacy)
//
// Once a Tier-0 fetch completes successfully and the torrent is
// classified, wantbridge pushes the release directly to the
// matching *arr via its release-add API. Sonarr/Radarr's poll
// cadence becomes a backstop — drop the cadence to hourly without
// losing latency. See `pkg push` (sibling package, D5).
//
// # Two-stage opt-in (mirrors contentfilter / peerrep / retention)
//
//	WANTBRIDGE_ENABLED=false (default) — pure no-op. Wantbridge
//	  does not poll *arr, does not maintain fingerprints, does not
//	  consult Match(). Memory + CPU cost = zero.
//	WANTBRIDGE_ENABLED=true, ENFORCE=false (shadow) — wantbridge
//	  polls *arrs, builds fingerprints, every DHT discovery hits
//	  Match() and the result feeds metrics
//	  (`wantbridge_match_total{tier,source}`) but the BEP-9
//	  fetcher's queue order is unchanged.
//	WANTBRIDGE_ENABLED=true, ENFORCE=true — Match() drives queue
//	  priority. Tier 2 hashes are dropped pre-fetch.
//
// # Failure modes (all fail-open)
//
//   - *arr unreachable -> previous fingerprint set stays live;
//     metric `wantbridge_arr_poll_errors_total{source}` increments.
//   - Empty wantlist -> all discoveries are Tier 1.
//   - Match() returns Tier 1 unconditionally if Wantbridge is
//     disabled — never panics, never blocks.
//
// # Metrics surface (D4)
//
//   bitagent_wantbridge_matches_total{tier,source}
//   bitagent_wantbridge_priority_fetches_total{tier}    (D4 -> dhtcrawler)
//   bitagent_wantbridge_skipped_total{reason}           (Tier 2 pre-fetch drop)
//   bitagent_wantbridge_wantlist_size{source}           (gauge)
//   bitagent_wantbridge_arr_poll_errors_total{source}
//   bitagent_wantbridge_arr_poll_duration_seconds{source}
//   bitagent_wantbridge_fingerprint_rebuild_duration_seconds
//
// # Status
//
// **Observe-only as of 2026-06-02** (was "scaffold only" at the
// 2026-04-25 introduction). The package is now referenced from the
// binary: wantbridgefx wires the factory + worker, and the dhtcrawler
// calls Match() on the hot path (internal/dhtcrawler/request_meta_info.go).
// However the Match() result is DISCARDED — it drives the
// bitagent_wantbridge_* metrics only. The dhtcrawler queue-priority
// shim, the Tier-2 pre-fetch skip, and the D5 push-to-*arr path are
// all still deferred to a follow-up MR. So the "three-tier
// prioritisation" and "push vs poll" sections above describe the
// INTENDED design, not current behaviour: enabling wantbridge does not
// yet change fetch order or Torznab results.
//
// See AGENTS/INNOVATIONS.md for the broader roadmap (D4 / D5 / etc.)
// and how wantbridge fits into the demand-driven architecture.
package wantbridge

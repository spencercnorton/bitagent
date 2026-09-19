// Package liveness derives a per-infohash health status (alive,
// suspect, dead) from the evidence stream and exposes it to the
// Torznab adapter as a hard exclusion filter.
//
// Sources of signal:
//
//   - qBittorrent state observations (KindQBStateObservation):
//     stalled / metaDL / error → suspect; seeding / downloading /
//     uploading → alive. The poller emits one observation per
//     torrent per minute when the state class is non-ignore.
//   - *arr import webhooks (KindWebhookImport): a successful import
//     is the strongest possible alive signal — bytes actually
//     reached disk and were renamed into a media library. By
//     default, imports tagged as private-tracker grabs are excluded
//     because they prove nothing about bitagent's catalog (the file
//     came from a different source).
//   - DHT revalidation: the revalidator periodically queries known
//     DHT nodes for peers on dead infohashes; when peers are found
//     above a configurable floor the infohash is restored to alive.
//
// The state machine is single-table: torrent_liveness has at most
// one row per infohash, and every transition is an upsert. Suspect
// is sticky-with-cooldown: a single qB-stall observation does not
// downgrade an infohash that we have recent alive evidence for, but
// N suspect observations spanning at least the configured stall
// threshold flip it to dead.
//
// The Torznab filter is the only consumer outside the evidence
// pipeline. It does a single bytea[] lookup per response (not per
// item) so liveness adds at most one round trip to a search.
package liveness

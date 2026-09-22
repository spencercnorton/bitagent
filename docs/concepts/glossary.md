# Glossary

Terms used throughout BitAgent's documentation, code, and dashboard. Alphabetical.

## A

### `*arr` (or arr suite)
Collective name for Sonarr, Radarr, Lidarr, Readarr, Prowlarr — the family of self-hosted media managers BitAgent integrates with. BitAgent presents itself to them as a Torznab-compatible indexer. See [Integrations](../integrations/sonarr.md).

## B

### BEP-5
BitTorrent Enhancement Proposal 5: the mainline DHT protocol. The Kademlia-style routing table BitAgent participates in to find peers without a tracker. Spec: [bittorrent.org/beps/bep_0005](https://www.bittorrent.org/beps/bep_0005.html).

### BEP-9
BitTorrent Enhancement Proposal 9: extension for peers to send `.torrent` metadata files directly over a peer-to-peer connection. How BitAgent fetches metadata after discovering an infohash via the DHT.

### BEP-15
BitTorrent Enhancement Proposal 15: the UDP tracker protocol, including the `scrape` request. BitAgent's `seeds` worker uses it to pull peer counts from public trackers at ~70 infohashes per datagram — a measured count of peers announcing to that tracker, not of the global swarm. See [concepts/swarm-health.md](swarm-health.md).

### BEP-33
BitTorrent Enhancement Proposal 33: DHT scrapes. Returns a bloom-filter approximation of a swarm's peers from a single node — cheap, but biased low, quantized, and captured once at crawl time. Not a substitute for a tracker scrape.

### BEP-51
BitTorrent Enhancement Proposal 51: DHT infohash indexing — the `sample_infohashes` RPC. The fast-path mechanism BitAgent uses to discover new infohashes from neighbouring DHT nodes.

### ui worker
The Python FastAPI dashboard under `ui/`, run inside the BitAgent image as the core worker `ui` (off by default; `UI_ENABLED=true`). Read-only relative to the core — never mutates indexing state directly; all writes go through the GraphQL API.

## C

### canonical label
A content-type label backed by ground-truth evidence — specifically, a successful `*arr` grab. Stored in the `torrent_canonical_labels` table. Preempts the CEL classifier when present.

### CEL
[Common Expression Language](https://github.com/google/cel-spec). Google's lightweight, sandboxed rules language. BitAgent's classifier rules are written in CEL and bundled in the binary. Inspect with `bitagent classifier show`.

### classifier preempt
Short-circuit of the CEL classifier when a canonical label already exists for the infohash being processed. See [concepts/classification.md](classification.md).

### content filter
Optional two-stage opt-in pipeline that drops torrents matching non-English / non-Latin / blocked-extension / NSFW criteria. Controlled by `CONTENT_FILTER_ENABLED` (shadow mode) and `CONTENT_FILTER_ENFORCE` (apply).

### CSAM blocklist
Pre-fetch double-hashed bloom-filter defense against CSAM infohashes. Closes the swarm-touching exposure window for the one category where post-fetch detection is too late. See [csam-defense.md](../csam-defense.md).

## D

### DHT
Distributed Hash Table. The decentralised peer routing layer underpinning the public BitTorrent network. BitAgent's discovery layer.

### dual-emit
The transition window during which every metric fires under both `bitagent_*` (primary) and `bitmagnet_*` (legacy) namespaces simultaneously. Started 2026-04-24. Allows operators to migrate dashboards/alerts at their own pace.

## E

### evidence
Webhook payloads from `*arr` Connect → Webhook entries (and the qBittorrent poller) that record successful grabs. Evidence is the ground truth that produces canonical labels. See [evidence.md](../evidence.md).

## G

### gqlgen
The Go GraphQL code generator BitAgent uses (`internal/gql/gqlgen.yml`). The schema lives in the core repo's `graphql/schema/`; resolvers in `internal/gql/resolvers/`.

## H

### Hash20
The GraphQL custom scalar for an infohash — 40 lowercase hex chars representing 20 bytes of SHA-1. Equivalent to the wire `info_hash`.

## I

### infohash
40-character hex SHA-1 identifying a unique torrent (the hash of its info dictionary). Sometimes written `info_hash` in DB columns or `Hash20` in the GraphQL schema.

## K

### k-table
Kademlia routing table. The structured peer cache the DHT crawler maintains in memory. Buckets organised by shared-prefix-length with the crawler's own node ID.

## L

### LLM rerank stage
Optional final classifier stage that calls an external LLM for ambiguous torrents that CEL couldn't classify. Two-layer opt-in (`Enabled` + `EnableLive`), aggressively gated, sha256 LRU cached. See [concepts/classification.md#stage-3-llm-rerank-stage](classification.md#stage-3-llm-rerank-stage).

## O

### operator-internal
Refers to the maintainer's Portainer / Gluetun-specific deployment (not shipped). NOT the public quickstart. Operator-internal env vars (`SONARR_URL`, `EVIDENCE_WEBHOOK_SECRET`, `VPN_*`, etc.) are not present in `examples/docker-compose.public.yml`.

## P

### peer
Any other DHT or BitTorrent node BitAgent talks to. Peer port: `BITAGENT_PEER_PORT` (default `3334`).

### preempt
See [classifier preempt](#classifier-preempt).

## R

### release
A versioned metadata snapshot of a torrent in the Postgres `releases` table. Multiple releases can coexist for one infohash if metadata is re-observed (e.g. seeder counts change).

### retention
Pipeline that periodically deletes torrents the predicate considers long-dead. Two-stage opt-in (`RETENTION_ENABLED`, `RETENTION_ENABLE_PURGE`) so you can validate the dry-run trend before flipping to real deletes.

## S

### swarm
The set of peers participating in distributing a specific torrent (i.e. announcing they have or want pieces of one infohash).

### swarm health
The seeder/leecher state of a torrent as BitAgent records it. Deliberately three-valued: *positive* (a tracker reports live peers), *known-zero* (a tracker that knows the hash reports no seeders) and *unknown* (no tracker in the pool knows it — not a zero). See [concepts/swarm-health.md](swarm-health.md).

## T

### TMDB
[The Movie Database](https://www.themoviedb.org/). Optional poster + metadata enrichment source for movie/TV releases. Activated by setting `TMDB_API_KEY`.

### Torznab
The [Newznab-derived API spec](https://torznab.github.io/spec-1.3-draft/) used by `*arr` clients to query indexers. BitAgent serves Torznab at `/torznab/api`. See [reference/torznab-api.md](../reference/torznab-api.md).

## W

### wantbridge
BitAgent's active-acquisition layer. Biases the crawler toward indexing infohashes that match active operator-defined or `*arr`-derived wants. See [concepts/wantbridge.md](wantbridge.md).

### wants
The titles the polled `*arr` applications are still missing; observed in the dashboard's Wants tab. See [ui-guide.md → Wants tab](../ui-guide.md#wants-tab).

## See also

- [Concepts / Architecture](architecture.md)
- [Concepts / DHT crawler](dht-crawler.md)
- [Concepts / Classification](classification.md)
- [Concepts / Swarm health](swarm-health.md)
- [Concepts / Wantbridge](wantbridge.md)
- [CSAM defense](../csam-defense.md)

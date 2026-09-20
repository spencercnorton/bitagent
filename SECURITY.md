# Security Policy

BitAgent is a self-hosted BitTorrent DHT crawler. Most installations sit
behind a tailnet or LAN, but the `/torznab` and (planned) public-release
endpoints are designed to be reachable from the open internet when the
operator chooses. We treat security findings as first-class.

## Supported versions

| Version stream | Supported? |
| --- | --- |
| Latest tagged release | yes — patch releases as needed |
| Older tagged releases | best effort, no SLA |

Every commit on `main` is a tagged release. If you are running an older
tag, the upgrade path is the next tag — not a backport.

## Reporting a vulnerability

**Do not file a public issue, pull request, or discussion thread for a
security finding.** Use GitHub's private vulnerability reporting:
**[Report a vulnerability](https://github.com/spencercnorton/bitagent/security/advisories/new)**,
and include:

- Affected commit SHA or release tag
- Steps to reproduce (curl invocations, payloads, deployment shape)
- Impact assessment (auth bypass, RCE, data exfiltration, DoS, etc.)
- Optional: a suggested fix

The advisory thread is private to you and the maintainer; attach
payloads there rather than in a public place. Please do not include real
credentials, hostnames or library contents — a description and a minimal
reproduction are enough.

We aim to acknowledge within **48 hours** and triage within **7 days**.
A coordinated disclosure window of **90 days** is standard; extensions
are negotiable for complex fixes or upstream coordination.

## Scope

**In scope:**

- The BitAgent Go codebase (`internal/`, `main.go`, build/CI scaffolding)
- The `bitagent-ui` companion repo (Python FastAPI dashboard)
- The example deployment under `examples/`
- Documented integration paths (Sonarr/Radarr evidence webhook, torznab,
  GraphQL admin surface)

**Out of scope:**

- Issues in upstream `bitmagnet-io/bitmagnet` that we have not modified —
  please report those upstream.
- BitTorrent BEP protocol weaknesses — those are protocol-layer and
  belong with the BEP authors / BitTorrent.org.
- Issues in third-party trackers, indexers, or `*arr` projects.
- Findings that require physical access, OS-level compromise, or already
  having admin credentials.

## Bug bounty

There is no monetary bounty. With the reporter's permission, valid
findings are credited in `SECURITY.md` once a fix has shipped.

## Threat model (summary)

- BitAgent is **not** a content-moderation product. The CEL classifier
  and the optional LLM rerank pass are best-effort labels for routing,
  not safety filters. The exception is the dedicated CSAM-defense
  layer (see below) which has a stricter operational guarantee.
- The DHT crawler ingests **untrusted, attacker-controlled input** at
  high volume (torrent names, metainfo strings, peer announcements). All
  parsing paths must be defensive.
- The `/torznab` endpoint is **not** authenticated by default. Set
  `TORZNAB_API_KEY` for any deployment that is reachable from outside
  your tailnet/LAN.
- The GraphQL admin surface is privileged. It currently expects to live
  behind a trusted reverse proxy or tailnet; the public-release plan
  layers an API-key gate on the dashboard side
  (`bitagent-ui!9` — `DASHBOARD_API_KEY`).
- Postgres is the system of record. A SQL-injection or unsafe migration
  would be high severity; only `gorm` parameterised paths are accepted
  in the data layer.

## CSAM defense

A DHT crawler must connect to swarms to fetch metadata, so the operator's
IP is briefly visible to peer-tracking services in every swarm bitagent
fetches from — including swarms hosting CSAM. We close that exposure
window for **community-known** infohashes via a layered defense:

1. **Pre-fetch infohash blocklist** (`internal/csamblocklist`) — an
   in-memory bloom filter of SHA-256-of-(SHA-1 infohash) double-hashes
   loaded from configurable feed URLs. Every DHT-discovered infohash is
   double-hashed and tested against the filter **before** any BEP-9
   metadata fetch is initiated. A match short-circuits the pipeline,
   so the operator's IP never touches the swarm.
2. **Defense-in-depth re-check** at the BEP-9 fetch site, in case a
   feed refresh introduced the hash after triage.
3. **Post-fetch CEL classifier** (`keywords.banned` in
   `internal/classifier/classifier.core.yml`) catches first-observation
   hashes that aren't yet in any community feed — title and file paths
   are matched against a comprehensive banned-keyword list and the
   torrent is deleted.
4. **BlockingManager.Block** records the infohash in this instance's
   own bloom filter so future DHT announcements are caught at step 1
   for this operator going forward.
5. **Self-export** (`csamblocklist.Exporter`) re-checks the
   banned-keyword regex on every classifier-driven delete and, on
   match, appends the double-hash to a local JSONL log. An optional
   outbound POST to a community collection endpoint contributes the
   observation back into the feeds other operators consume at step 1.

The double-hash function (`internal/csamblocklist/doublehash.go`) is
**SHA-256 of the raw 20-byte SHA-1 infohash bytes**. The hex form is
the only on-the-wire encoding for both feeds and the local log.

The defense **converges, it does not eliminate**: the first time any
network observes a new CSAM infohash, one BEP-9 fetch happens before
the post-fetch defense fires. That observation propagates back into
community feeds via self-export. Subsequent observations (anywhere)
are rejected pre-fetch.

The raw infohash is **never** written to disk by the export pipeline.
The local JSONL log only contains double-hashes — its leak does not
re-expose the underlying CSAM infohashes.

See [`docs/csam-defense.md`](docs/csam-defense.md) for the full
architecture, wire format, operator config, and metric surface.
Inspired by upstream issue
[bitmagnet-io/bitmagnet#494](https://github.com/bitmagnet-io/bitmagnet/discussions/494).

## Hall of fame

_(Empty — be the first.)_

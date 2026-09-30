# Operating a private media library

Private media is an optional catalog in the UI's SQLite database. It never
inserts private hashes into the DHT crawler's PostgreSQL corpus. Existing DHT
results retain public magnets. Browser discovery and indexer keys use your
existing SSO identities; explicit owner grants control membership.

## Configure the service

Keep the feature disabled until the seed client, proxy and SSO are ready.
Then enable it on a withheld deployment to approve members, import the catalog
and provision the original seed. Keep public ingress closed during this
bootstrap: the private administration endpoints also return 404 when the
feature is disabled. Admit public traffic only after the seed, network and
client acceptance checks below pass.
These settings are startup-only and cannot be changed through the settings API:

| Setting | Purpose |
|---|---|
| `PRIVATE_INDEXER_ENABLED` | Defaults to `false`; enable only for a prepared member deployment. |
| `PRIVATE_INDEXER_URL` | Canonical HTTPS origin, present in `PUBLIC_LIBRARY_HOSTS`. |
| `PRIVATE_INDEXER_SECRET` | At least 32 bytes of random secret material; protects derived tracker credentials. |
| `PRIVATE_SEEDER_URL` | Fixed internal qBittorrent Web API base URL. |
| `PRIVATE_SEEDER_USERNAME`, `PRIVATE_SEEDER_PASSWORD` | Seed client credentials from a secret manager. |
| `PRIVATE_SEED_VERIFICATION_TTL` | Freshness window, default 1800 seconds, permitted range 60–86400. |
| `PRIVATE_TRACKER_RATE_LIMIT_PER_MIN` | Per-member announce limit, default 10000; size for the seed corpus and its 15-minute cadence. |
| `PRIVATE_PEER_CIDRS` | Optional explicit peer network allowlist for a member VPN. |

Preserve the existing trusted proxy CIDRs and proof boundary. The proxy must
overwrite `X-BitAgent-Peer-IP` with the original client's address and inject
its configured `X-BitAgent-Proxy-Proof`, clearing caller-supplied identity and
peer headers. Uvicorn proxy header rewriting stays disabled. Torznab, private
torrent and announce routes authenticate their own keys; do not put a browser
SSO redirect in their path. Hide `/api/private/` and `/private-admin` from the
public hostname. The operator hostname still requires an explicit owner role.

Disable access logs and URI-bearing error logs on every credentialed proxy
hop. The bundled worker and `python app.py` disable Uvicorn access logging;
when launching Uvicorn yourself, include `--no-proxy-headers --no-access-log`.
Never copy a real torrent, magnet, API-key URL, catalog, seeding map or activity
report into a public issue or repository. Back up SQLite together with the
private secret; secret rotation changes newly generated tracker credentials.

## Approve members

Sign in on the operator hostname and open `/private-admin`. Grant the user's
existing SSO identity ID. Sign-in alone does not grant catalog access.
An approved member opens `/library/private` to create a personal indexer key,
search releases, download torrents, copy personal magnets and view their own
activity. Only the operator can read all members' activity.

Suspending membership revokes its indexer keys and removes tracker peers;
reapproval requires a new key. Key rotation also revokes earlier tracker
credentials. Neither action terminates already-established peer connections.

## Prepare genuine torrents

Run the publisher where the actual media files are mounted. Plex discovery is
read-only. Supply the Plex token through the named environment variable and
explicitly map container paths to directories beneath the media root:

```bash
cd ui
python media_publish.py --plex-url https://plex.example.org \
  --root /media/library --path-map /movies=Movies --path-map /tv=TV \
  --announce https://indexer.example.org/announce --output /private/prepared
```

Set `PLEX_TOKEN` through your secret manager first; never put its value in the
command or URL. The initial announce is a passkey-free preparation placeholder;
the service replaces it outside the `info` dictionary for each member.
The publisher streams piece hashes, rejects symlinks/path escapes and changing
files, and creates movie/episode versions plus real season/show packs. It
fails on unmapped or missing media rather than silently claiming completeness.
It covers movie/TV items available to the Plex token; it cannot infer files
Plex has not indexed or that the token cannot see.

Alternatively supply `--manifest inventory.json`, containing `version: 1` and
`releases` with `source_id`, `title`, `kind` and relative `files`, plus optional
provider IDs, season and episodes. Treat this input as private operational data.
The normalized source ID includes the full content hash, preserving old swarm
history when files change. Duplicate episode/season/show hashes become aliases
of one swarm rather than different copies of the same torrent.

Re-run the same inventory command with `--verify` to compare current files
against the prepared metainfo. Hashing a large library is disk-intensive;
schedule it on the media host and use smaller inventory batches when needed.
No command adds torrents, opens peer ports or modifies the seed client.

## Import and provision the original seed

1. Send each `catalog-0001.json` batch to the operator-only
   `POST /api/private/catalog/import` using the authenticated operator session
   or operator API header. Batches are bounded; `catalog.json` is the full
   offline verification index and must not replace the batched import files.
2. Use `GET /api/private/catalog` to obtain canonical release IDs. New releases
   remain unavailable to members regardless of any `seed_verified` manifest field.
3. Approve a dedicated existing SSO identity for the seed client and create its
   personal indexer key. Use the operator-only
   `GET /api/private/catalog/{id}/seed-torrent?seed_member_id={identity}` to obtain
   its personalized original-seed torrent before public availability exists.
   Keep this torrent private; it contains the seed member's tracker credential.
4. Add that torrent to a dedicated seed client, paused, pointing at the existing
   files using `seeding_map.json`. Single files use their parent directory;
   packs use their actual shared directory. Respect `save_parent_of_root` when
   the pack directory is the configured root. Do not copy or rename the media.
5. Complete a **fresh full qBittorrent hash recheck** of the current files.
   Never skip the check or rely on cached progress/fast-resume state. Resume
   seeding only after every piece verifies. A private hash differs from an
   existing public torrent of the same files because `private=1` is in `info`.
6. Invoke `POST /api/private/catalog/{id}/verify`, or let the background readiness
   worker refresh. It checks the exact hash, size, zero remaining bytes,
   complete progress and an active upload state through the configured client.
   This is a seed-client observation, not independent rehashing by the web app.
7. Verify a synthetic download from a separate approved client, its announce
   accounting and key revocation before publishing the real media catalog.

The worker refreshes availability at most every five minutes, with per-batch
observation timestamps. Failed probes withdraw availability; stale proofs
never satisfy search, downloads or announce. Withdraw a release with
`PATCH /api/private/catalog/{id}` and `{"published": false}`. Re-publishing
clears readiness and requires a fresh seed verification. This preserves history
instead of deleting the member's activity when a release leaves the server.

## Privacy and accounting limits

The private flag suppresses DHT, PEX and local peer discovery in compliant
clients. Credentials authorize tracker discovery. It does **not** authenticate
BitTorrent peer-wire connections cryptographically: someone who knows a peer's
address can still attempt to connect, and recipients can forward files or
credentials. See [BEP 27](https://www.bittorrent.org/beps/bep_0027.html).

For stronger connection-level restrictions, keep the seeder's peer ports inside
an authenticated member VPN, enforce its grants/firewall, and configure the
same network in `PRIVATE_PEER_CIDRS`. Private announce traffic must reach the
service through that network while retaining the original member peer address.
Do not publish or forward the private seed client's peer port on the Internet.
Public DHT clients should remain separate. An application CIDR check cannot
replace the seed client's firewall or prevent an approved member forwarding files.

Metrics distinguish issued links from client-reported uploaded/downloaded byte
deltas and completion events. First observations establish a baseline; resets,
restarts and duplicate completion events do not inflate totals. Ratios are
null until downloaded bytes are observed. These are operational observations,
not independently verified delivery or billing measurements. Public DHT swarm
transfers are outside this tracker and cannot be attributed to website members.

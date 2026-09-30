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
| `PRIVATE_PEER_BINDINGS_REQUIRED` | Defaults to `false`; require a fresh approved IPv4-to-SSO mapping for tracker announces. |

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

### Optional tracker address ownership

Enable `PRIVATE_PEER_BINDINGS_REQUIRED` only when the seed management boundary
provides the authenticated `GET /_bitagent/peer-bindings` endpoint. In this mode,
`PRIVATE_SEEDER_URL` must be an HTTPS origin with a valid server certificate,
and the configured seed session credentials must cover this endpoint. The UI
uses the existing qBittorrent login and a fresh applicable session cookie;
it never follows redirects or environment HTTP proxies.

The endpoint must derive bindings from currently approved devices and active
SSO membership. It returns `application/json`, no more than 256 KiB or 1000
bindings, with this versioned shape:

```json
{"version":1,"issued_at":1900000000,"expires_at":1900000030,"bindings":[{"user_id":"example-member","address":"192.0.2.1"}]}
```

The timestamps are Unix seconds from the successful authority observation;
expiry must be no more than 30 seconds after issuance. Return each canonical
IPv4 address once, with its exact SSO identity. One member may own several
approved device addresses. Restrict this read-only endpoint to the original
seed management service; reject missing/invalid sessions and stale permission
state. Its authorization must be independent of tracker announces and torrent
readiness to avoid a bootstrap cycle.

The UI refreshes the small snapshot separately from seed catalog verification,
with a five-second total request bound and a three-second refresh interval.
Startup begins without trusted addresses. Failed or cancelled observations
withdraw cached trust immediately. Absolute source expiry becomes a monotonic
deadline, and replayed observations cannot extend it or restore trust after a
failure. Keep both hosts' clocks synchronized; future-issued observations are
rejected.

Before recording an announce, the tracker requires the effective client IPv4
to belong to the passkey owner's SSO identity. Peers and seed/leecher counts
are filtered through the same current mappings. Membership and API-key
revocation remain independent checks. Preserve the trusted proxy's original
source address; an ordinary forwarded chain or caller-supplied `ip` parameter
does not establish ownership.

This binds tracker attribution to an approved device address. It does not
measure transferred bytes, prevent sharing between approved members, or stop
already-connected peers. Client counters remain self-reported. Enforce data
network access and permission expiry at the VPN/client firewall as well.

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
files, and creates movie/episode versions plus real season/show packs. Unmapped, absent, unsafe or changing sources are recorded in the private
audit with per-kind rejection counts. Empty seasons/shows and incomplete
packs are also explicit. The command returns exit code 3 for an incomplete
preparation. Its index distinguishes Plex metadata counts, emitted variants,
new hashes, reused checkpoints and rejected entries. It covers movie/TV items available to the Plex token; it cannot infer files
Plex has not indexed or that the token cannot see.

Alternatively supply `--manifest inventory.json`, containing `version: 1` and
`releases` with `source_id`, `title`, `kind` and relative `files`, plus optional
provider IDs, season and episodes. Treat this input as private operational data.
The normalized source ID includes the full content hash, preserving old swarm
history when files change. Duplicate episode/season/show hashes become aliases
of one swarm rather than different copies of the same torrent.

Default preparation streams owner-only `.torrent` artifacts and import shards,
with at most 1000 releases and 4 MiB per shard. `catalog-index.json` names the
current shards, the JSONL seeding map and rejection audit. No monolithic catalog
is required. `--full-catalog` optionally creates a bounded `catalog.json` for a
small export; it is not the import or completeness authority.

Restart the same preparation command with `--resume` after interruption. Its
private SQLite checkpoint reuses a successful hash only when every file's
safe descriptor reports the same device, inode, size, mtime and ctime, with
matching root identity and piece layout. Otherwise it hashes again. A process
lock releases automatically on exit/crash. Checkpoint rows retain each run's
identity, so an interrupted resume leaves the prior complete index independently
verifiable. Torrent files are installed atomically after their full write; a
partial write cannot occupy a final hash filename. Previous complete shard
indexes stay intact if the inventory source fails; import only files named in
the latest completed index. `--sections 10,20` selects explicit Plex sections for separate
exports. An index's completeness applies only to its recorded inventory scope.
`--no-packs` records each season/show as an operator-requested exclusion; those
counts must not be described as complete coverage of all media kinds.

Verify bytes independently of the cache before seeding:

```bash
python media_publish.py --root /media/library --output /private/prepared --verify
```

Preparation, resumed preparation and verification accept an optional
`--hash-mib-per-second 50` to cap source-file hashing reads at that example
budget. Choose a budget appropriate for the media host before starting a full
library run, and pass it to both preparation and `--verify`. The default is
unlimited. One shared limiter covers all files and releases, with at most a
1 MiB read burst. It uses monotonic deadlines, includes time spent reading and
does not bank idle credit. Metadata/stat reads and unchanged hash-cache hits
are unpaced. The limit controls this publisher's reads; other processes,
filesystem caching and a seed client's separate full recheck have their own I/O.

This rehashes every indexed artifact using its checkpointed source mapping,
without requesting Plex metadata again. An optional `--manifest` also checks
coverage against that input. Cache fingerprints are change detection, not proof
of a live seed. Large packs automatically increase piece length up to 16 MiB;
unsupported layouts stay unavailable and appear in the audit. Hashing a large
library is disk-intensive; schedule it on the media host. No command adds
torrents, opens peer ports or modifies the seed client.

## Import and provision the original seed

1. Read `catalog-index.json` and send each file named in its `shards` array to
   the operator-only `POST /api/private/catalog/import`, using the authenticated
   operator session or operator API header. Shards contain `version: 1` and
   bounded release arrays. Review rejection/completeness counts before claiming
   full library coverage; an old or unfinished run is not a current catalog.
2. Use `GET /api/private/catalog` to obtain canonical release IDs. New releases
   remain unavailable to members regardless of any `seed_verified` manifest field.
3. Approve a dedicated existing SSO identity for the seed client and create its
   personal indexer key. Use the operator-only
   `GET /api/private/catalog/{id}/seed-torrent?seed_member_id={identity}` to obtain
   its personalized original-seed torrent before public availability exists.
   Keep this torrent private; it contains the seed member's tracker credential.
4. Add that torrent to a dedicated seed client, paused, pointing at the existing
   files using the JSONL seeding-map file named in `catalog-index.json`. Each
   line carries the inventory ID, catalog ID, hash and actual file mapping.
   Single files use their parent directory;
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
7. Explicitly reannounce the original seed after readiness is verified. The
   tracker may have rejected its earlier announces while the release was
   unavailable. Confirm the seed peer is registered before a member download.
8. Verify a synthetic download from a separate approved client, its announce
   accounting and key revocation before publishing the real media catalog.

### Reconcile a large catalog

The legacy `GET /api/private/catalog` response remains an array of at most
1,000 canonical releases. Use the following operator-only APIs for a complete
inventory; a successful partial import does not prove complete library coverage.

Import each verified shard with `POST /api/private/catalog/import?ack=1`. The
usual `imported` count and `readiness` message remain, with `results` containing
one `{source_id, id, info_hash, size}` record per input, in input order, and a
`catalog_snapshot` containing the database `instance_id` and `revision_token`.
An acknowledgment is returned only after the whole transaction commits.
Unchanged retries preserve the mapping and readiness. Input source IDs are
unique; multiple aliases of the same bytes share one canonical `id`. An import
accepts at most 1,000 records and its acknowledgment is bounded to 512 KiB.
Omit `ack` for the existing count-only response.

Use `GET /api/private/catalog/page?view=aliases&limit=100` to enumerate each
source alias, including its semantic metadata and canonical ID, hash and size.
Use `view=releases` for each canonical swarm. Both views include unready and
withdrawn entries. Responses contain `version`, `view`, `snapshot`, `items`
and `next_cursor`. The snapshot also reports `alias_total` and `release_total`.
Continue with `?cursor=...` until `next_cursor` is null. Pages contain at most
100 records and 2 MiB; they omit torrent bytes, media paths and member keys.
Each request still requires operator authorization.

The signed cursor fixes the view, page limit, immutable ordering and metadata
revision for one hour from the first page. A metadata import or visibility
change invalidates it with HTTP 409; restart the scan without discarding
committed import mappings. Readiness checks do not change the revision, so
`ready` and `verified_at` are current observations per page, not a promise of
later availability. An expired cursor returns HTTP 410. Normal restart with
the same protected signing secret preserves valid cursors. A consistent
database restore preserves its instance and revision; an unchanged restored
catalog may resume, while subsequent metadata edits mint new random revision
tokens. Replacing the database or rotating the signing secret invalidates
old cursors.

After all shards are imported, compare every expected source version, hash,
size and alias metadata against acknowledgments and both complete page views.
Check unique source IDs and exact totals; report missing, unexpected or stale
aliases explicitly. The offline seeding map's catalog ID is its versioned
source ID; use the acknowledged canonical `id` in seed-torrent and verification
requests. Do not create duplicate seed sessions for aliases of the same swarm.
Do not automatically delete a stale alias or withdraw a shared swarm that
still has current aliases. This API does not promote a generation or retire
aliases automatically. Complete metadata reconciliation and measured seed
capacity are separate steps; downloads still require a freshly verified
complete active seed copy for their individual canonical release.

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
deltas and completion events. First observations establish a baseline. Each
active peer and key retains its highest upload and download counters; lower,
duplicate or repeated `started` reports cannot lower that baseline. A `stopped`
event removes the peer. A later observation after a stop, peer expiry, a new
peer ID or a key change establishes a new baseline without crediting lifetime
bytes. Clients resetting counters with the same still-active peer ID should
stop first or use a new peer ID. A reset without an observable session boundary
can miss bytes until counters exceed the previous high-water mark; the tracker
cannot distinguish it from a delayed report. Duplicate completion events in
an active session do not increase its completion count.

The metrics API's `totals` covers all stored rows for the authorized account
(all accounts for an operator). `items` retains the 1,000-row display limit;
`totalItems` and `itemsTruncated` disclose that list's completeness. Account
totals must come from `totals`, rather than summing the display list. Ratios are
null until downloaded bytes are observed. These are operational observations,
not independently verified delivery or billing measurements. Public DHT swarm
transfers are outside this tracker and cannot be attributed to website members.

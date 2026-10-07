# Backend deployment and operations

BitAgent 4.x is a Go backend with PostgreSQL storage. It exposes machine APIs
and metrics, and runs independently of any browser interface or hosted account
system. Optional metadata, evidence, LLM and cleanup features use configuration
you supply; installation does not establish that those features are active.

## Install the public stack

Use Docker Compose with persistent storage and outbound TCP/UDP access:

```sh
git clone https://github.com/spencercnorton/bitagent.git
cd bitagent
cp examples/.env.example examples/.env.public
openssl rand -hex 32   # POSTGRES_PASSWORD
openssl rand -hex 32   # TORZNAB_API_KEY
```

Store the generated values in the ignored environment file, then run:

```sh
docker compose -f examples/docker-compose.public.yml --env-file examples/.env.public config --quiet
docker compose -f examples/docker-compose.public.yml --env-file examples/.env.public up -d --build
docker compose -f examples/docker-compose.public.yml --env-file examples/.env.public ps
curl --fail http://localhost:3333/metrics
```

The example exposes backend HTTP on loopback and peer traffic on the configured
peer port. PostgreSQL stays on the Compose network. There is no browser page.
Use [quickstart](quickstart.md) and [examples](../examples/README.md) for the
exact network layout and commands.

## Configure and verify integrations

| Area | Verify |
|---|---|
| PostgreSQL | Password, persistent volume, capacity and tested restore |
| DHT | UDP routing, bootstrap peers and persisted-torrent metrics |
| Torznab | API key and capabilities from the client network |
| GraphQL/import/metrics | Trusted network or authenticated reverse proxy |
| Metadata | Provider configuration, quota and actual provider responses |
| Evidence | Configured source endpoints and independent webhook secret |
| LLM/cleanup | Explicit mode, budget, dry-run metrics and recovery plan |

[Configuration](configuration.md) documents backend settings. Client services
on another host cannot use your own `localhost`; choose an address reachable
from their network. Protect APIs before expanding the loopback HTTP binding.

## Health and capacity

Verify both services are healthy. Observe `/metrics`, container logs, queue
depth, database growth and provider errors. Crawl warmup varies with routing
and network conditions; an empty early search is not a fixed-duration failure.
Test Torznab capabilities and a synthetic request before connecting a full
client stack. Evaluate optional processing in shadow/dry-run mode before
accepting live verdicts or deletion.

## Catalogue removal

Catalogue removal and crawler blocking require the complete, bounded recovery
transition. The application does not attach this disabled foundation: legacy
CEL delete actions, enforced content-filter drops, GraphQL deletion and
`purge-content-types --write` therefore return a recovery-disabled error before
deletion or bloom admission. The command's `--force` flag cannot bypass it.
Ordinary matching and keep/review rows still persist, including independent
rows in a batch whose removal is held.

Retention purge and both synchronous and Batch junk quarantine are also held:
their previous partial snapshots cannot restore every cascading source row.
Dry-run observations, retained provider results, keep-only Batch settlement,
existing quarantine expiry and operator restoration remain available. A held
destructive Batch run retains its successful provider receipts in `finalizing`;
it is not recorded as applied and does not dispatch again for that hold.

Legacy archive hard purge is held as well; the API returns conflict without
deleting its snapshot or declaring the torrent dead. Legacy restore validates
captured hashes and source bounds, rechecks current raw state and protected
authority, and refuses competing private/canonical/verdict facts. It preserves
newer source observations and retains permanent processing/dispatch histories.
Old partial snapshots cannot reconstruct facts that were never captured.

The explicit `blocking.WithRecovery` integration is for callers that have
qualified source protection, complete capture, bounded retention/storage and
restoration. It does not authorize a deletion policy or configure the public
application. Never enable legacy title/origin/zero-seed decisions merely because
capture is available. Do not downgrade to an older unguarded binary to bypass
the hold.

## Quarantine maintenance

Quarantine expiry retains snapshots indefinitely; it does not delete them or
blacklist a hash. Maintenance processes oldest eligible snapshots in chunks of
at most 256, with at most 1,024 transitions before candidate work in a cycle.
The existing unexpired-snapshot index keeps the retained archive out of that
scan. A larger backlog remains eligible for later ordinary cycles.

When the verdict ledger is configured, each expiry chunk commits its marker,
current state and events together. A failed or canceled chunk rolls back and
can be retried normally; committed chunks are not repeated. Restore and explicit
deletion also commit their ledger transition with the snapshot operation.
Ledger failures therefore fail those operator actions while retaining their
original snapshot, rather than report success with inconsistent state.

Restore validates the locked snapshot after acquiring the raw-torrent lock.
If another operation refreshed or removed that snapshot, the restore rolls
back; an operator can review the current version and retry. Delayed quarantine
observations cannot change a completed restore, deletion or expiry back to
quarantined.

## Backup, upgrade and recovery

Back up PostgreSQL and optional configuration/data volumes. Store secrets
separately in encrypted backups. Restore into an isolated database with DHT
and external calls disabled until recovered configuration has been reviewed.

Record the running release and immutable image digest. Upgrade with the
published digest and verify migrations, APIs, metrics and crawl progress.
A migration can make a binary-only downgrade unsafe; retain the matching
backup and previous artifact. For 3.x combined images, follow
[the 4.0 migration](migration-v4.md).

See [security](operations/security.md), [monitoring](operations/monitoring.md)
and [troubleshooting](troubleshooting.md) for further operational guidance.

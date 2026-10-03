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

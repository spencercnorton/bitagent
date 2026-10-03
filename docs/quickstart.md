# Quickstart: standalone backend

Deploy the Go backend and PostgreSQL, verify its machine APIs, then connect an
indexer client. The public stack does not serve a browser page.

## Prerequisites

Use Docker and Docker Compose, persistent disk, and outbound TCP/UDP access
for DHT and metadata retrieval. PostgreSQL 14+ is required; the example includes
PostgreSQL 16. Start with modest crawl settings and measure memory/disk growth
before sizing a large corpus. Bootstrap time depends on the network.

## Source and configuration

```sh
git clone https://github.com/spencercnorton/bitagent.git
cd bitagent
cp examples/.env.example examples/.env.public
openssl rand -hex 32   # POSTGRES_PASSWORD
openssl rand -hex 32   # TORZNAB_API_KEY
```

Put the two generated values in the ignored environment file. Compose requires
a nonempty database password. A nonempty Torznab key protects indexer requests;
it does not protect GraphQL, import or metrics.

```sh
docker compose -f examples/docker-compose.public.yml --env-file examples/.env.public config --quiet
docker compose -f examples/docker-compose.public.yml --env-file examples/.env.public up -d --build
docker compose -f examples/docker-compose.public.yml --env-file examples/.env.public ps
```

The first command validates configuration, and the second builds from source.
Expect `bitagent` and `postgres` to become healthy. Inspect service logs if either
restarts or fails its probe. The backend HTTP binding is loopback by default;
peer traffic uses the configured port, default 3334 TCP/UDP.

## Verify the APIs

```sh
curl --fail http://localhost:3333/metrics
curl --fail -H 'Content-Type: application/json'   --data '{"query":"{ __typename }"}' http://localhost:3333/graphql
# Supply the same private key configured in your environment file.
curl --fail "http://localhost:3333/torznab/api?t=caps&apikey=$TORZNAB_API_KEY"
```

GraphQL should return a JSON envelope, and Torznab a capabilities document.
Watch `bitagent_dht_crawler_persisted_total` and container logs for crawl progress.
An early empty search may reflect warmup; do not assume a fixed deadline.

## Connect a client

In Sonarr/Radarr/Prowlarr, add a custom Torznab indexer with the backend's
reachable `http://<host>:3333/torznab` URL and the configured API key. A client
in another container cannot use your host's `localhost`. Expand the loopback
binding only with an appropriate trusted-network or reverse-proxy policy.

Test capabilities from the client's network before saving the indexer. Use
[per-app guides](integrations/sonarr.md) for categories and search behavior.
Configure [evidence webhooks](evidence.md) with their separate secret to feed
successful acquisitions back into matching and ranking.

## Continue

- [Configuration](configuration.md)
- [API reference](reference/graphql-api.md)
- [Classification](concepts/classification.md)
- [Security](operations/security.md) and [monitoring](operations/monitoring.md)
- [Troubleshooting](troubleshooting.md)
- [Migrating from 3.x](migration-v4.md)

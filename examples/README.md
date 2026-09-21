# Examples — self-hoster quickstart

Two compose files live in this repo. Pick the one that matches your
deployment shape:

| File | Audience | Image source | VPN integration |
|---|---|---|---|
| `examples/docker-compose.public.yml` (this) | New self-hosters | builds from local source via `Dockerfile` | none — direct host networking |
| the maintainer's private compose | Operator-internal stack | private registry image | `service:gluetun` (VPN-attached) |

If you're reading the public README and want to try BitAgent on your
own machine, **use `examples/docker-compose.public.yml`**. The
private file pins operator-specific Portainer and secrets-store
conventions you don't need.

## Quickstart

```bash
# 1. clone
git clone https://github.com/spencercnorton/bitagent.git
cd bitagent

# 2. set env
cp examples/.env.example examples/.env.public
$EDITOR examples/.env.public   # at minimum: set POSTGRES_PASSWORD

# 3. boot
docker compose -f examples/docker-compose.public.yml \
  --env-file examples/.env.public \
  up -d --build

# 4. verify
curl -s http://localhost:3333/metrics | head -5
# bitagent_dht_crawler_persisted_total{entity="torrent"} 0
# ...

# 5. the operator console (loopback only, no login in this stack)
open http://localhost:8080
# the public library is the same port under another hostname
open http://library.localhost:8080

# 6. (optional) GraphQL playground
open http://localhost:3333/graphql
```

The first `up` builds the image from source — expect 2-4 min on a
warm Docker. Subsequent boots reuse the cached image.

## What you get

- **`bitagent`** — the DHT crawler + classifier + HTTP API on port 3333,
  plus the operator dashboard on port 8080 (the `ui` worker inside the
  same container, `UI_ENABLED=true` in this file; off by default in the
  image)
- **`postgres`** — Postgres 16 on its default 5432, named volume for data

That's it. No VPN, no extra observability stack. Add those layers as
you need them; this stack is the smallest viable BitAgent. To run the
crawler headless, set `UI_ENABLED=false` in `.env.public`.

## Endpoints (default)

| Path | What it does | Auth |
|---|---|---|
| `:8080/` | Operator console (`OPERATOR_HOSTS`) and public library (`PUBLIC_LIBRARY_HOSTS`), selected by `Host` | `REQUIRE_AUTH=false` here, so bound to 127.0.0.1 — see `ui/README.md` before exposing |
| `/graphql` | Query/mutation API | none unless you front it |
| `/torznab` | Newznab/Torznab feed for *arr clients | `TORZNAB_API_KEY` if set |
| `/metrics` | Prometheus exposition | none |
| `/import` | NDJSON bulk-import sink (`POST` only) | none unless you front it |
| `/evidence/arr/*` | Sonarr/Radarr webhook ingest | `X-Evidence-Token` header |

For exposing this publicly, **put a reverse proxy in front** that
adds auth (Caddy/Traefik with `forward_auth`, NPM with `auth_request`,
or similar). The bare HTTP server has no opinion about auth — it
trusts whoever's connecting.

## Adding a VPN

The public quickstart skips VPN integration to keep the dependency
graph small. If you want DHT traffic egressing through a VPN tunnel,
the cleanest pattern is:

1. Add a `gluetun` service to `examples/docker-compose.public.yml`
   (see `deploy/docker-compose.yml` for an example shape).
2. Change bitagent's network mode to `network_mode: service:gluetun`.
3. Move the bitagent `ports:` block to the gluetun service —
   `service:` networking forwards through the gluetun container.
4. Wire your VPN provider's keys into the gluetun env block.

## Upgrading

```bash
git pull
docker compose -f examples/docker-compose.public.yml \
  --env-file examples/.env.public \
  up -d --build
```

The Postgres data volume persists across rebuilds. Migrations run
automatically on boot.

## Removing everything

```bash
docker compose -f examples/docker-compose.public.yml down --volumes
```

Drops all four volumes — your indexed torrents and the dashboard's
SQLite file are gone. Skip
`--volumes` to keep the database between deletes.

## See also

- `README.md` — repo overview
- `ui/README.md` — the dashboard module: every setting it reads and
  the auth tiers for a real deployment
- `SECURITY.md` — supported versions, vulnerability reporting,
  threat model, CSAM-defense layer
- `docs/csam-defense.md` — pre-fetch double-hash blocklist details
- `deploy/README.md` — operator-internal Portainer git-backed stack

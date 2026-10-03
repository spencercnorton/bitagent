# Backend FAQ

## What is distributed here?

The MIT-licensed Go crawler, metadata processor, classifier, title matcher,
cleanup pipeline, evidence adapters, Torznab/GraphQL APIs and metrics. Browser
pages and hosted account features run as a separate product. Version 4.0
formalizes this boundary; see [migration](migration-v4.md).

## Is a particular service or hardware required?

PostgreSQL and a suitable network are required. Generic metadata, *arr,
qBittorrent and LLM adapters use endpoints/credentials you configure. No
particular operator deployment is required. Optional LLM stages stay off by
default; the deterministic processing pipeline works without them.

## Where is persistent state?

PostgreSQL stores the corpus, evidence and processing state. The optional
backend mounts retain `/root/.config/bitmagnet` and
`/root/.local/share/bitmagnet`. The reference stack uses named volumes and
does not ship a browser SQLite database. Back up the database and whichever
optional backend state you use.

## Which APIs need protection?

A configured `TORZNAB_API_KEY` gates Torznab. `EVIDENCE_WEBHOOK_SECRET` gates
webhooks via `X-Evidence-Token`. GraphQL includes mutations and has no built-in
login; import and metrics also need an appropriate trusted network/proxy policy.
A Torznab key does not protect the other APIs. See [security](operations/security.md).

## How do I rotate a key?

Change the backend setting, restart the process and update every client using
that key. There is no built-in dual-key rotation window. Do not publish keys or
resolved configuration in issues or logs.

## Can I open the GraphQL endpoint in a browser?

Use a separate GraphQL client or JSON POST with curl. The backend serves no
HTML playground. [API reference](reference/graphql-api.md) documents queries,
mutations and introspection.

## Why are early searches empty?

The DHT needs working routing and peers before it discovers metadata, and
classification must process that metadata before content searches can find it.
Check metrics and worker logs rather than assuming a fixed warmup duration.
[troubleshooting](troubleshooting.md) separates network and filtering failures.

## Does it download payloads?

The crawler retrieves torrent metainfo rather than content payloads. Its network
activity still exposes the participating IP to peers. The pre-fetch blocklist
rejects known hashes; see [CSAM defense](csam-defense.md) for its limitations.

## How do I attach my *arr clients?

Add a custom Torznab indexer with a reachable backend URL and API key. A service
in another container cannot use your host's `localhost`. Configure the separate
evidence webhook to feed successful acquisition labels back into processing.
See [Prowlarr](integrations/prowlarr.md), [Sonarr](integrations/sonarr.md),
[Radarr](integrations/radarr.md), [Lidarr](integrations/lidarr.md) and
[Readarr](integrations/readarr.md).

## How should I size or upgrade a deployment?

Measure database growth, queue depth, memory and provider load under your own
crawl settings. Keep destructive processing in dry-run until its behavior is
reviewed. Record the installed digest, back up state and test restoration before
upgrades. See [operations](OPERATIONS.md), [monitoring](operations/monitoring.md)
and [releasing](RELEASING.md).

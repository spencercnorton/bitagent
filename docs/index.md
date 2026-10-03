# BitAgent backend documentation

BitAgent is a self-hosted BitTorrent DHT crawler, content indexer and processing
backend. It builds on [bitmagnet](https://github.com/bitmagnet-io/bitmagnet) with
matching, evidence learning, filtering, scrubbing, swarm observations and cleanup.
The public distribution is MIT-licensed and runs independently of a browser site.

## Start here

- [Quickstart](quickstart.md): standalone backend and PostgreSQL
- [Configuration](configuration.md): environment settings and safe opt-in
- [Operations](OPERATIONS.md): health, capacity, backups and upgrades
- [Migrating to 4.0](migration-v4.md): separate backend and site releases

## Processing and integrations

- [Architecture](concepts/architecture.md)
- [DHT crawler](concepts/dht-crawler.md)
- [Classification](concepts/classification.md)
- [Evidence](evidence.md) and [compatibility](concepts/compatibility.md)
- [Swarm health](concepts/swarm-health.md) and [wantbridge](concepts/wantbridge.md)
- [Client integrations](integrations/prowlarr.md)
- [Improvements](project/improvements.md) and [benchmarks](project/benchmarks.md)

## Reference and development

- [GraphQL](reference/graphql-api.md), [Torznab](reference/torznab-api.md),
  [metrics](reference/metrics.md) and [CLI](reference/cli.md)
- [Development setup](development/setup.md) and [release process](RELEASING.md)
- [Security](operations/security.md) and [CSAM defense](csam-defense.md)
- [Monitoring](operations/monitoring.md), [FAQ](faq.md) and
  [troubleshooting](troubleshooting.md)

Attribution to upstream contributors is preserved in [NOTICE](../NOTICE).

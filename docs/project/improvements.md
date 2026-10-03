# Backend improvements over upstream

BitAgent preserves the DHT and indexing foundation of
[bitmagnet](https://github.com/bitmagnet-io/bitmagnet). The public backend adds
processing and operational features that run with configuration you supply.

| Area | Backend behavior | Reference |
|---|---|---|
| Evidence | Configurable *arr webhooks/history and qBittorrent observations derive canonical labels | [Evidence](../evidence.md) |
| Classification | Evidence preempt, CEL rules and bounded optional LLM fallback | [Classification](../concepts/classification.md) |
| Matching | Shared title normalization, anime aliases, metadata matching and evaluation tools | [Benchmarks](benchmarks.md) |
| Curation | Deterministic content filters, verdicts, junk quarantine and safe cleanup gates | [Configuration](../configuration.md) |
| Retention | Dry-run and explicit purge controls for stale metadata | [Configuration](../configuration.md) |
| Swarm observations | Tracker scrapes keep authoritative zero distinct from unknown | [Swarm health](../concepts/swarm-health.md) |
| Defense | Pre-fetch double-hash blocklist and post-fetch checks | [CSAM defense](../csam-defense.md) |
| Observability | Prometheus families, database observations and generic Grafana resources | [Monitoring](../operations/monitoring.md) |
| Distribution | Go-only runtime, checked version/source identity and independent site lifecycle | [Releasing](../RELEASING.md) |

Optional modes are not evidence of live processing. Use the documented metrics,
evaluation methodology and shadow/dry-run stages to qualify behavior in your
own deployment. Browser/account functionality belongs to a separate repository
and is not an improvement shipped in this backend artifact.

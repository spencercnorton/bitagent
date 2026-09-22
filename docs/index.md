---
hide:
  - navigation
---

Self-hosted, high-performance BitTorrent DHT crawler and content indexer.
Drop it into your *arr stack as a Torznab-compatible indexer and let the
public DHT fill the gaps your private trackers miss.

BitAgent is a 2026 fork of
[bitmagnet](https://github.com/bitmagnet-io/bitmagnet), which went dormant
in July 2025. We picked up where upstream left off and added classification,
evidence learning, a dashboard, and multi-tier auth.

MIT-licensed. No telemetry. No accounts. Just an indexer you own.

---

## Feature highlights

- **DHT crawling** --
  Full BEP-5/9/10/33/42/43/51 compliance for broad,
  resilient peer discovery
- **CEL-based classifier with optional LLM rerank** --
  Rule-driven content classification that can escalate ambiguous
  matches to a language model for a second opinion
- **Evidence pipeline** --
  Webhook feedback from Sonarr, Radarr, and the rest of the *arr
  family becomes ground-truth, so the classifier improves over time
- **Tracker-confirmed swarm health** --
  A BEP-15 UDP tracker-scrape worker refreshes seeder and leecher
  counts from public trackers, and keeps "dead" and "unknown" as
  separate answers instead of collapsing both to zero
- **Operator dashboard in the same image** --
  A supervised `ui` worker, off by default: eight tabs covering crawl
  health, the library, wants, evidence, the junk-purge quarantine, LLM
  stage scorecards, configuration and diagnostics, plus a public
  library for the people you share the indexer with
- **Dashboard auth tiers** --
  API key, or a reverse proxy that performs the login and injects
  identity with a proof header -- pick the layer that fits your network
- **TMDB poster integration** --
  Movie and series results include poster art pulled from TMDB
- **Retention controls** --
  Age-off, category filters, and manual purge so your database
  stays the size you want
- **Wantbridge** --
  Active content acquisition that prioritizes torrents matching
  items your *arr clients are actually searching for
- **Torznab API** --
  First-class compatibility with Sonarr, Radarr, Prowlarr,
  Lidarr, and Readarr -- add BitAgent as a custom indexer
  and you are done

---

## Quick links

- [Quickstart](quickstart.md) --
  Docker Compose, one API key, and a working indexer in 15 minutes
- [Architecture](concepts/architecture.md) --
  How the Go core, the `ui` worker, classifier, and evidence
  pipeline fit together in one image
- [Evidence pipeline](evidence.md) --
  What your *arrs send back, how it becomes a canonical label, a
  success prior and a liveness state, and the one metric that says
  the loop is paying for itself
- [Compatibility contract](concepts/compatibility.md) --
  The official *arr interfaces BitAgent uses, and the forked
  download-decision hook it deliberately does not
- [Swarm health](concepts/swarm-health.md) --
  Where seeder counts come from, why a DHT zero is not a real zero,
  and how to tune the tracker scrape
- [FAQ](faq.md) --
  Common questions about crawling, legality, resource usage,
  and integration
- [Integrations](integrations/sonarr.md) --
  Per-app setup guides for Sonarr, Radarr, Prowlarr, Lidarr,
  and Readarr
- [Dashboard guide](ui-guide.md) --
  Walkthrough of the operator console's tabs; the module's own
  settings and auth tiers are in [ui/README.md](../ui/README.md)
- [Improvements over upstream](project/improvements.md) --
  What BitAgent adds to bitmagnet, with the design notes behind each

---

## License and lineage

BitAgent is released under the
[MIT License](https://github.com/spencercnorton/bitagent/blob/main/LICENSE).
It is a fork of
[bitmagnet-io/bitmagnet](https://github.com/bitmagnet-io/bitmagnet) --
full credit to the upstream contributors whose work made this possible.

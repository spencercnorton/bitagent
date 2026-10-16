# Optional processor boundary

Baseline: backend commit `36de3b92d1169207b043f12244c16502b2de0a27`.
This refactor concerns the headless backend. Browser interfaces and individual
processor packaging have separate release and acceptance cycles.

## Existing pipeline and disposition

| Current entry point | Responsibility | Disposition |
| --- | --- | --- |
| `internal/dhtcrawler`, `protocol/dht`, `protocol/metainfo` | DHT discovery, peer traversal, bounded BEP-9 metadata validation, persistence | Core; unchanged protocol and crawler algorithms |
| `internal/importer`, `queue`, `processor` | Durable ingestion/reprocessing, batch isolation, PostgreSQL persistence | Core; optional jobs must not share the crawler's execution slots |
| `classifier/classifier.core.yml`, `parsers`, `search.go`, `tmdb.go`, `animedb` | CEL policy, deterministic type/field parsing, exact/fuzzy/alias matching, normal metadata lookup | Core; metadata providers are distinct from model providers |
| `classifier/llmstage` | Model type fallback before policy and local enrichment | External classify capability; deterministic enrichment stays core |
| `classifier/llmmatch`, `classifier/matcher.go` | Extraction, model reranking, embedding retrieval, provider configuration, inference budgets and caches | Retire embedded inference; optional extraction/matching/embedding proposals use the contract |
| `classifier/contentfilter` | Deterministic extension/type/name/language checks plus hosted language inference and learned rules | Keep deterministic checks; external language/policy proposals; retire inference clients, prompts, learned rule cache and call budget |
| `junkpurge` | Model judgment and hosted Batch submission plus quarantine/restore | Retire inference/submission; preserve quarantine, restoration and receipts; plugins cannot delete |
| `llmwork`, migrations 56–58 | Source-bound jobs, leases and application receipts | Preserve historical tables and receipts; freeze unfinished jobs; use the same PostgreSQL service for generic processor jobs |
| `llmcapture`, migrations 46–55 | Request/result/decision evidence, provider dispatch accounting | Preserve evidence and historical readers; generic processor jobs record versioned input/result/application telemetry |
| `llmeval`, `llmreadiness`, model CLI commands | Model-specific qualification, replay, provider readiness | Retire from the backend runtime; historical source remains at the baseline commit; processor status is independent of core readiness |
| `serving`, `database/search`, `torznab`, `gql` | Privacy, eligibility, search and indexer responses | Core; no missing/failed processor result is an eligibility grant |
| `cataloguepolicy`, `cataloguerecovery`, `blocking`, `verdicts` | Authority, complete snapshots, bounded removal and restore | Core; protected catalogue table schemas and historical migrations remain intact |

The deterministic stage precedes optional processors. Type prediction is not an
identity match. An abstention is not an unsuccessful completed search. Technical
failure and deferral are not negative policy evidence. Policy exclusion is
reversible visibility state, never a deletion instruction. Existing eligible
catalogue results do not depend on processor readiness.

## Integration reconciliation

PR #67 is included in the baseline. Its distinction between provider failure and
a completed miss, candidate/source identity checks, and standard Arr identifiers
must survive the boundary. PR #66 is independent evidence-retention work; this
refactor does not change its piece-hash table or its source/privacy guards.
The external matcher-first integration can implement this generic protocol;
its research evidence, promotion criteria, packaging and production qualification
are not replaced or certified by protocol conformance.

## Design constraints

Use bounded HTTP requests to separately supervised services. Discover operator
manifests dynamically. Identity, processor version, optional model version,
policy version, configuration and input revision are part of job identity.
Persist a frozen ordered plan: stage first, then operator priority, then stable
processor ID. Core facts take precedence; the first valid proposal for an empty
field wins. Failed/abstaining steps leave no positive or negative authority.

Use PostgreSQL transactions, leases and `SKIP LOCKED`, with separate optional
work admission and dispatch limits. A new broker provides no required property
here and adds recovery and operating dependencies. New side tables have no
foreign key into catalogue cascade tables: adding one would invalidate existing
recovery-schema fingerprints. Catalogue mutation must be validated and committed
with its application receipt. Network I/O never holds catalogue locks.

Reviewable implementation stages: (1) baseline and boundary inventory;
(2) remove embedded inference and preserve legacy state;
(3) contract, durable execution, validated application and status API;
(4) independent fixtures, fault/recovery checks, migration and measured comparison.

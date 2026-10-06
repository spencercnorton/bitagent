# Configuration

BitAgent is configured entirely through environment variables — no config file is required for any deployment shape. The runtime resolves env vars at startup, applies defaults, and keeps everything in a single resolved-config tree that you can introspect with `bitagent config show`.

This page is the authoritative list. If a variable is not on this page, it does not exist.

For first-time setup see [Quickstart](quickstart.md). For production layouts see [examples/README.md](../examples/README.md). For the security implications of each auth knob see [operations/security.md](operations/security.md).

## Secrets: prefer the `_FILE` form

**Every variable on this page also accepts a `<NAME>_FILE` form** that names a file to read the value from, the same convention the official `postgres` and `mysql` images use:

```yaml
environment:
  JUNKPURGE_LLM_API_KEY_FILE: /run/secrets/openai_api_key
volumes:
  - /srv/secrets/bitagent/openai_api_key:/run/secrets/openai_api_key:ro
```

Use it for every credential. A value passed as a plain environment variable is stored in container metadata and printed in cleartext by `docker inspect`, so anyone who can reach the docker socket — and anything that scrapes container metadata, including stack-backup tooling — can read it. A mounted file is not in that metadata.

Rules:

- The plain variable wins when it is set to a non-empty value. An **empty** plain value is treated as absent, so a leftover `FOO=` (which is what compose's `${FOO:-}` expands to) cannot silently shadow the file and disable the feature it configures.
- A `_FILE` path that is missing, unreadable, or empty is a **startup error**, not a fall-back to the default. Failing loudly is deliberate: a silently-empty API key reads as "tier disabled" and has caused real outages here.
- Contents are whitespace-trimmed, so `echo "$KEY" > secret` works.
- On a coercion failure the value is not echoed into the error, unlike the plain-env path.

## Required

The single hard requirement is the database password. The container will exit on startup if it is empty.

| Variable | Default | Purpose |
|---|---|---|
| `POSTGRES_PASSWORD` | *(required)* | Postgres credential. Pick something long and random — this is the only thing standing between an attacker on the host and your indexed corpus. |

## Networking

Two ports and a Postgres connection. Defaults work for the bundled `examples/docker-compose.public.yml` stack; override only if you have a port conflict or external Postgres.

| Variable | Default | Purpose |
|---|---|---|
| `BITAGENT_HTTP_PORT` | `3333` | HTTP API port. Hosts `/graphql`, `/torznab/api`, `/metrics`, `/import`, `/evidence/arr/*`. |
| `BITAGENT_PEER_PORT` | `3334` | BitTorrent peer protocol port. Outbound-dominant; opening it inbound increases throughput but is not required. |
| `POSTGRES_HOST` | `postgres` | Hostname or IP. Defaults to the compose service name in the bundled stack. |
| `POSTGRES_PORT` | `5432` | Postgres port. |
| `POSTGRES_NAME` | `bitmagnet` | Database name. The legacy schema name is preserved for compatibility with deployments that pre-date the rebrand; do not rename without a planned migration. |
| `POSTGRES_USER` | `bitmagnet` | Postgres user. Same compat reasoning as `POSTGRES_NAME`. |

## Authentication

BitAgent's HTTP server has no opinions about auth — it trusts whoever connects. Two env vars gate the two surfaces that face external clients.

| Variable | Default | Purpose |
|---|---|---|
| `TORZNAB_API_KEY` | *(none)* | When set, every `/torznab/api` request must include `apikey=<value>`. Empty leaves `/torznab` open — fine on a tailnet or behind a reverse proxy with its own auth, **not fine on the open internet.** Constant-time compare. |
| `EVIDENCE_WEBHOOK_SECRET` | *(none)* | Shared secret for the `*arr` webhook ingester. Mirror this value as the `X-Evidence-Token` Custom Header on every Sonarr/Radarr/Lidarr/Readarr Connect → Webhook configuration. Operator-internal stack only. |

## Classifier

The classifier runs locally with no external dependencies. TMDB enrichment is optional and improves movie/TV title resolution.

| Variable | Default | Purpose |
|---|---|---|
| `TMDB_API_KEY` | *(none)* | Free TMDB v3 API key. Enriches movie + TV records with title, release year, and episode metadata. Empty disables the TMDB stage; the rest of the classifier still works. Get a key at <https://www.themoviedb.org/settings/api>. |

## Optional LLM chat backends

The TMDB matcher and type fallback each select their chat request contract
explicitly. Supported values are `openai` and `ollama`; an empty value keeps
the `openai` default. Endpoint addresses and model names never select a backend
automatically.

| Variable | Default | Purpose |
|---|---|---|
| `CLASSIFIER_LLM_MATCH_CHAT_BACKEND` | `openai` | Chat contract for the matcher's extraction and candidate reranking calls. |
| `CLASSIFIER_LLM_CHAT_BACKEND` | `openai` | Chat contract for the type fallback classifier. |
| `CLASSIFIER_LLM_LIVE_ALLOWED_TYPES` | unset | Comma-separated canonical `movie,tv,music,audiobook,book` values eligible for type-only application. Unset retains all supported types. For example, `movie,tv` keeps other predictions unknown without newly invoking their exclusion policy. The allowlist also bounds shadow `would_apply` evidence and is captured with each decision. |

For an Ollama server reachable on the same host, configure its full
OpenAI-compatible chat endpoint and an installed model explicitly:

```dotenv
CLASSIFIER_LLM_MATCH_CHAT_BACKEND=ollama
CLASSIFIER_LLM_MATCH_ENDPOINT=http://127.0.0.1:11434/v1/chat/completions
CLASSIFIER_LLM_MATCH_MODEL=qwen3.6:35b
CLASSIFIER_LLM_CHAT_BACKEND=ollama
CLASSIFIER_LLM_ENDPOINT=http://127.0.0.1:11434/v1/chat/completions
CLASSIFIER_LLM_MODEL=qwen3.6:35b
```

Use an endpoint reachable from the BitAgent process; container loopback names
the container itself. Existing API-key requirements still apply: the type
fallback requires a nonempty configured key. Backend selection leaves both
stages disabled by default and does not change their separate shadow/live
switches, budgets, privacy checks, source gates or evaluation capture.

The `openai` contract preserves existing `max_completion_tokens` requests and
provider controls. The `ollama` contract instead sends the same requested
limit as `max_tokens`, plus `reasoning_effort: "none"`. Native matcher calls
still request 120 tokens for single-item extraction and 60 for reranking;
uncaptured grouped extraction retains its existing per-item scaling. The type fallback
still uses its configured output cap (64 by default). Ollama's compatibility
API documents `max_tokens` and reasoning control; its v0.30.7 implementation
maps those fields to the generation limit and disabled thinking request.
See [Ollama OpenAI compatibility](https://docs.ollama.com/api/openai-compatibility)
and the [v0.30.7 request conversion](https://raw.githubusercontent.com/ollama/ollama/v0.30.7/openai/openai.go).

Ollama mode rejects OpenAI/OpenRouter hosts, a configured provider pin,
data-sharing flags, and endpoint userinfo, query strings or fragments. It requires
the plain `/v1/chat/completions` path and refuses HTTP redirects. Select
`openai` for the existing hosted-provider contracts. These selectors cover
the matcher and type fallback; the content-filter and junk clients retain
their existing configuration.

Captures identify the selected Ollama contract and retain the exact dispatched
request. Existing deployed-wire evaluation manifests continue to describe their
historical OpenAI contract: Ollama controls fail those fidelity checks. Use the
configured native clients to validate Ollama; do not relabel an old manifest as
Ollama fidelity evidence.

Validate the chosen server and model independently before enabling live
decisions. Request compatibility and a parsed response do not establish model
accuracy, calibrated confidence, provider billing or production acceptance.

## Optional embedding shortlist

The matcher can optionally use an OpenAI-compatible embeddings endpoint to
shortlist candidates before its existing chat rerank. This feature defaults
off and requires an explicitly configured route and enabled evaluation capture.
The embedding route has its own model and credentials; it does not inherit the
chat model, API key or OpenAI data-sharing setting.

| Variable | Default | Purpose |
|---|---|---|
| `CLASSIFIER_LLM_MATCH_EMBEDDINGS_ENABLED` | `false` | Enable optional shortlisting when there are more candidates than the shortlist size. Matcher `ENABLED` and `ENABLE_LIVE` remain independent. |
| `CLASSIFIER_LLM_MATCH_EMBEDDINGS_ENDPOINT` | empty | Full embeddings URL. HTTPS required except for loopback; redirects are refused. |
| `CLASSIFIER_LLM_MATCH_EMBEDDINGS_MODEL` | empty | Explicit embedding model. |
| `CLASSIFIER_LLM_MATCH_EMBEDDINGS_API_KEY_FILE` | empty | Secret file for the embedding route's key. Plain `API_KEY` is also supported. |
| `CLASSIFIER_LLM_MATCH_EMBEDDINGS_OPENROUTER_PROVIDER` | empty | Explicit provider pin required for OpenRouter. |
| `CLASSIFIER_LLM_MATCH_EMBEDDINGS_TIMEOUT` | `15s` | Per-request timeout. |
| `CLASSIFIER_LLM_MATCH_EMBEDDINGS_MAX_REQUEST_BYTES` | `8192` | Serialized embedding request bound. |
| `CLASSIFIER_LLM_MATCH_EMBEDDINGS_DIMENSIONS` | `256` | Requested vector length. `0` omits the optional parameter for models that do not support it. |
| `CLASSIFIER_LLM_MATCH_EMBEDDINGS_MAX_DIMENSIONS` | `4096` | Maximum accepted vector length. |
| `CLASSIFIER_LLM_MATCH_EMBEDDINGS_SHORTLIST_SIZE` | `3` | Number of candidates shown to chat, in `2..100`. |
| `LLM_EVALUATION_CAPTURE_ENABLED` | `false` | Must be enabled for the embedding route's request, first response and final chat decision evidence. |
| `LLM_EVALUATION_CAPTURE_DISPATCH_CONTROL_ENABLED` | `false` | Opt in to durable dispatch ownership and retained first-response replay after migration 56. Requires capture. Keep disabled until the worker and provider boundaries are qualified together. |

For direct OpenAI, use `https://api.openai.com/v1/embeddings` and a model such
as `text-embedding-3-small`. OpenAI supports the `dimensions` parameter on
`text-embedding-3` models; use `0` to omit it for other models. The request
sends an array containing the release/extraction query and candidate texts,
with `encoding_format: "float"`. See the
[OpenAI embeddings guide](https://developers.openai.com/api/docs/guides/embeddings).

OpenRouter requires `https://openrouter.ai/api/v1/embeddings` and an explicit
provider pin. Requests set `order` and `only` to that provider, disable
fallback providers, require supported parameters, deny data collection and
require ZDR. These fields are documented in the
[embeddings API reference](https://openrouter.ai/docs/api/api-reference/embeddings/submit-an-embedding-request)
and [provider preferences](https://openrouter.ai/docs/client-sdks/python/components/providerpreferences).

Every embedding attempt consumes the same persisted daily/monthly allowance
as extraction and chat reranking. Native/private-source checks and a final
database admission recheck run before embedding egress. The exact request and
bounded first HTTP response are stored as the separate `matcher_embedding`
task. The chat capture links that response, records the shortlist and retains
the complete original candidate set as policy evidence. Response bodies are
limited to 128 KiB; oversized, zero, inconsistent, nonfinite or ambiguous
vectors fall back to the original chat candidate list, as do provider outages.
Privacy and audit failures stop follow-on calls.

Cosine similarity controls order and shortlisting only. The ordinary chat
confidence threshold, source title, candidate identity/ambiguity/year guards
and parsed TV episode tuple still control the final match. Shadow remains
immutable. Evaluate shortlist recall and final match precision on independently
reviewed cases before enabling this optional path in production.

## Crawler

A single multiplicative knob covers DHT worker concurrency. Higher values mean more peers, more memory, more CPU.

| Variable | Default | Purpose |
|---|---|---|
| `DHT_SCALING_FACTOR` | `1` | Multiplier applied to internal DHT worker counts. `1` is safe on a 4 GB host. `4`–`10` is appropriate on a beefier box; anything higher should be informed by `bitagent_dht_*` Prometheus signal. |
| `DHT_CRAWLER_SCALING_FACTOR` | inherits | The internal config path that `DHT_SCALING_FACTOR` resolves into. The bundled compose file maps the shorter name to this one — you do not normally need to set it directly. Note there is **no** `BITMAGNET_` prefix on any key: the resolver builds keys from the config section plus the snake_cased Go field name, and an unmatched key is ignored in silence rather than rejected. |

## Logging

| Variable | Default | Purpose |
|---|---|---|
| `LOG_LEVEL` | `info` | One of `debug`, `info`, `warn`, `error`. `info` is fine for steady-state; `debug` is loud and only useful while reproducing a specific issue. |

## CSAM defense

A pre-fetch double-hashed blocklist filters infohashes before BitAgent ever does a BEP-9 metadata fetch — closing the swarm-touching exposure window for the one category of content where post-fetch classification is too late. Defaults are safe: enabled with no feeds is a NoOp until you opt in to a feed you trust. Full architecture is in [csam-defense.md](csam-defense.md).

| Variable | Default | Purpose |
|---|---|---|
| `CSAM_BLOCKLIST_ENABLED` | `true` | Master toggle. Leaving it on with no feeds set has zero runtime cost. |
| `CSAM_BLOCKLIST_FEED_URLS` | *(none)* | Comma-separated list of community blocklist feed URLs. Each feed is fetched, parsed (one double-hash per line), and merged into a bloom filter. Empty = NoOp. |
| `CSAM_BLOCKLIST_EXPORT_ENABLED` | `true` | When the post-fetch classifier flags a CSAM-banned title, append the double-hashed infohash to a local JSONL log. On by default; the file never leaves the host unless `EXPORT_UPSTREAM_URL` is also set. |
| `CSAM_BLOCKLIST_EXPORT_UPSTREAM_URL` | *(none)* | Optional outbound endpoint for community contribution. POSTs each new double-hash observation. Off by default — opt in only if you have an endpoint you trust. |

## Content filter

The deterministic tier checks language tags, title script, release audio
markers, extensions and NSFW criteria. The optional LLM tier classifies the
residual cohort after upstream classification. Both tiers can be observed
before their decisions affect persistence.

| Variable | Default | Purpose |
|---|---|---|
| `CONTENT_FILTER_ENABLED` | `false` | Examines inputs and emits counterfactual metrics. Required for either tier. |
| `CONTENT_FILTER_ENFORCE` | `false` | Applies deterministic drops. Also controls the LLM tier when its mode is inherited. |
| `CONTENT_FILTER_LLM_ENABLED` | `false` | Allows residual language inference after privacy, durable capture and budget admission. |
| `CONTENT_FILTER_LLM_ENFORCE` | `inherit` | `false`: model decisions stay shadow; `true`: applies the selected model disposition; `inherit` or unset: follows `CONTENT_FILTER_ENFORCE` for compatibility. |
| `CONTENT_FILTER_LLM_ACTION` | `drop` | `drop`: legacy destructive disposition; `review`: keeps the torrent and applies the `llm-language-review` tag after a qualifying audited live model decision. Empty means legacy `drop`; invalid values fail startup. |
| `CONTENT_FILTER_LLM_DEFER_ON_UNAVAILABLE` | `false` | With model enforcement active in `drop` mode, unavailable endpoints can defer an input for retry. Review mode keeps it. Malformed responses, exhausted budgets and low-confidence verdicts keep it. |

Set the model mode explicitly in new configurations. For YAML, quote it:
`llm_enforce: "false"`. Values are exactly `inherit`, `true` or `false`; invalid
values fail startup. The shipped defaults keep both tiers inactive, and the
public environment example sets the model mode to `false`.

With both enable flags true, the application modes are:

| `CONTENT_FILTER_ENFORCE` | `CONTENT_FILTER_LLM_ENFORCE` | Result |
|---|---|---|
| `false` | `false` | Both tiers shadow |
| `true` | `false` | Deterministic live, model shadow |
| `false` | `true` | Deterministic shadow, model live on the residual cohort |
| `true` | `true` | Both tiers live |
| either | `inherit` / unset | Legacy behavior: both follow `CONTENT_FILTER_ENFORCE` |

A deterministic match always ends the decision ladder, including when that
tier is shadow. Model-only enforcement therefore acts on eligible residuals;
it does not reclassify deterministic matches. English release tokens (`ENG`,
`English`, `EN`) veto residual model inference so a title-language guess cannot
overrule that English-inclusive release evidence. The veto leaves deterministic
policy unchanged. Existing configurations retain
the inherited behavior on upgrade. Startup logs show effective modes and
warn when an inherited model mode is live.

For a nondeleting model stage, set `CONTENT_FILTER_LLM_ACTION=review` and
`CONTENT_FILTER_LLM_ENFORCE=true` with both enable flags true. A qualifying
audited non-English model response adds `llm-language-review` through the normal
torrent-tag transaction; content remains searchable and is neither deleted,
blocked nor deferred by that model decision. The tag records the model's
concern, not verified audio language. Deterministic policy still runs first and
uses its own enforcement flag. Use `CONTENT_FILTER_ENFORCE=false` when the
desired rollout keeps deterministic matches too.

Review is off by default. With model enforcement false it produces only a
would-review observation. Request capture and terminal decisions bind the
selected disposition, cutoff and live mode; a drop capture cannot authorize a
review tag. Audit failures, private inputs, advertised English tracks, malformed
results and low-confidence negatives never add the tag. Review does not make
the model more accurate and cannot qualify destructive drops. Independent labels
can measure review errors before accepting a later destructive disposition.

Before destructive model enforcement, independently label prospective shadow samples and
qualify the exact model, provider, prompt and request contract. Separate release
families across evaluation splits and inspect English, anime, dual-audio and
ambiguous-title false drops. A model's confidence and the number of shadow
would-drops do not establish accuracy. Keep the model shadow until the
operator's accepted harm bound passes; changing a model or prompt requires
fresh qualification.

Production inference requires durable request, HTTP result and terminal
decision capture, finite daily/monthly budgets and bounded request/output sizes.
Each source must pass current privacy admission; title-family cache entries
cannot authorize a new source. Exact-source durable replay avoids duplicate
provider calls. A failed audit or privacy recheck returns an error for retry
before model application. Private inputs never leave the process. Invalid,
incomplete or malformed results keep; they cannot drop or trigger an
unavailable-provider deferral loop. Track the `llm_non_english` reason in
`bitagent_contentfilter_drop_total` and `bitagent_contentfilter_would_drop_total`
separately from deterministic reasons. To roll back model application, set
`CONTENT_FILTER_LLM_ENFORCE=false`; set `CONTENT_FILTER_LLM_ENABLED=false` to
stop new model dispatches too. In review mode these flags stop new tags;
previous review tags remain ordinary removable torrent tags and do not imply
an active language exclusion. Review counters are
`bitagent_contentfilter_llm_review_total` and
`bitagent_contentfilter_llm_would_review_total`; review decisions never increase
the model drop counters.

## Retention

Periodic deletion of torrents that haven't been seen in DHT announcements for a long time. Same two-stage opt-in pattern as the content filter.

| Variable | Default | Purpose |
|---|---|---|
| `RETENTION_ENABLED` | `false` | Stage 1: dry run. The `would_purge` counter increments for each torrent that *would* be deleted. Nothing actually deletes. |
| `RETENTION_ENABLE_PURGE` | `false` | Stage 2: real delete. Requires `RETENTION_ENABLED=true`. |

The default predicate is conservative: no canonical label, no evidence record, age > 60 days, `updated_at` older than 180 days, and every torrent source reports `seeders=0`. Validate the dry-run trend before flipping `ENABLE_PURGE`.

## Junk classifier

The LLM-backed junk classifier has a separate application gate. Evaluation can
populate reviewable judgments while quarantine remains disabled.

| Variable | Default | Purpose |
|---|---|---|
| `JUNKPURGE_ENABLED` | `false` | Runs candidate capture and judging. |
| `JUNKPURGE_ENABLE_PURGE` | `false` | Allows confident junk to enter restorable quarantine after every cycle-level safety gate passes. |
| `JUNKPURGE_LLM_NAMES_PER_CALL` | `1` | Names grouped into one synchronous request; operators may raise this deliberately to amortize prompt tokens. |
| `JUNKPURGE_LLM_UNAVAILABLE_CONSECUTIVE_LIMIT` | `3` | Stops a synchronous cycle after this many consecutive unavailable groups. One unavailable group always blocks application for the whole cycle; this only bounds provider probing while preserving later successful judgments as evaluation evidence. |

Use `bitagent_junkpurge_cycle_outcomes_total` and
`bitagent_junkpurge_llm_failures_total` to verify that judging is live and to
separate timeouts, rate limits, server errors, and invalid responses. Breaker
cohorts remain queryable through `junkpurge_judgments.reason`; they are never
included in `would_delete_total` or applied.

## Configurable evidence sources

The backend can poll generic *arr and qBittorrent endpoints you configure.
No source is attached by default. Empty base URLs leave the corresponding
source idle. The example stack omits these optional settings; add them to
your own environment or YAML configuration when needed.

| Variable | Default | Purpose |
|---|---|---|
| `EVIDENCE_SONARR_BASE_URL` / `EVIDENCE_SONARR_API_KEY` | empty | Sonarr history source |
| `EVIDENCE_RADARR_BASE_URL` / `EVIDENCE_RADARR_API_KEY` | empty | Radarr history source |
| `EVIDENCE_READARR_BASE_URL` / `EVIDENCE_READARR_API_KEY` | empty | Readarr history source |
| `EVIDENCE_LIDARR_BASE_URL` / `EVIDENCE_LIDARR_API_KEY` | empty | Lidarr history source |
| `EVIDENCE_QB_ALPHA_BASE_URL` / `EVIDENCE_QB_ALPHA_USERNAME` / `EVIDENCE_QB_ALPHA_PASSWORD` | empty | First qBittorrent source |
| `EVIDENCE_QB_BETA_BASE_URL` / `EVIDENCE_QB_BETA_USERNAME` / `EVIDENCE_QB_BETA_PASSWORD` | empty | Second qBittorrent source |
| `EVIDENCE_ARR_POLL_INTERVAL` | `15m` | History polling cadence |
| `EVIDENCE_QB_POLL_INTERVAL` | `15m` | qBittorrent polling cadence |

See [evidence](evidence.md) for webhook authentication, evidence precedence,
liveness and outcome-prior settings. Source credentials belong in an ignored
environment file or secret manager. Network tunnels and human account services
are deployment choices rather than backend configuration.

## Durable optional model work

The `llm_work` worker is disabled by default. It collects bounded source cases
for type inference, language review and identity matching without making
ingestion wait for provider capacity. Each selected stage must already be
enabled and evaluation capture plus dispatch control must be enabled. Language
work requires `CONTENT_FILTER_LLM_ACTION=review`; the worker never runs the
general classifier deletion pipeline.

| Variable | Default | Purpose |
| --- | --- | --- |
| `LLM_WORK_ENABLED` | `false` | Admit source cases and permit owned execution |
| `LLM_WORK_WORKER_ENABLED` | `true` | Execute admitted cases; `false` collects bounded cases only |
| `LLM_WORK_KINDS` | `classifier_type,contentfilter,matcher` | Selected stages |
| `LLM_WORK_POLL_INTERVAL` | `5s` | Owned worker cadence |
| `LLM_WORK_LEASE_DURATION` | `5m` | Generation-fenced source-case lease |
| `LLM_WORK_TASK_TIMEOUT` | `3m` | Maximum execution time, below the lease duration |
| `LLM_WORK_ENQUEUE_TIMEOUT` | `100ms` | Maximum database wait during ingestion |
| `LLM_WORK_MAX_PENDING` | `2400` | Pending capacity per selected stage |
| `LLM_WORK_MAX_TASK_AGE` | `168h` | Proposal lifetime |
| `LLM_WORK_TIME_BUCKETS` | `24` | UTC admission strata that reserve capacity for later traffic |
| `LLM_WORK_SPREAD_ADMISSION` | `true` | Pace new cases under the existing stage allowance |
| `LLM_EVALUATION_CAPTURE_DISPATCH_CONTROL_ENABLED` | `false` | Durable request dispatch and retained-response replay |

Allowances and concurrency limits remain stage-owned. Started matcher cases
receive completion priority. Exact retained first responses replay before a new
allowance reservation. Only positively undispatched work can resume after a
deferral or expired lease. An intent, unknown transport outcome or missing
response body requires explicit reconciliation; it never authorizes automatic
duplicate calls or allowance refunds.

`LLM_WORK_ENABLED=false` disables admission and execution.
`LLM_WORK_WORKER_ENABLED=false` pauses execution while admission remains bounded.
When the registered worker runs, proposal cleanup continues with an already
initialized database pool even if execution is disabled. Expired raw payloads
and lifecycle events are removed; committed application snapshots retain their
source and policy bindings. Digest-only dispatch fences survive both task and
capture cleanup. Snapshots describe committed model applications and cannot
serve as canonical or independently reviewed labels.

Metrics expose admission outcomes, worker cycles and task depth by stage/state
under `bitagent_llm_work_*`, including overflow, sampling and expiry. Qualify
restart, source/privacy mutations, cancellation and concurrent application in
disposable state before enabling execution.

## Live config inspection

Run `bitagent config show` against your running container to dump every config path, type, current value, default, and the source resolver that produced it (env var, default, file, etc.). This is the canonical way to verify a configuration override took effect:

```bash
docker exec bitagent bitagent config show
```

Output is a wide table; pipe to `less -S` if your terminal is narrow. The `From` column tells you exactly where a value came from — invaluable when an override is silently being shadowed by a higher-precedence source.

See [reference/cli.md](reference/cli.md) for the full CLI surface.

## See also

- [Quickstart](quickstart.md)
- [Examples / self-hoster quickstart](../examples/README.md)
- [reference/cli.md](reference/cli.md)
- [reference/metrics.md](reference/metrics.md)
- [csam-defense.md](csam-defense.md)
- [operations/security.md](operations/security.md)

Deferred tasks apply through a narrow compare-and-set adapter. A type task can
only enrich one still-unknown, unattached public row with a qualified movie/TV
prediction and local metadata. A matcher task must retain the normal final
identity, year, confidence and audit checks before attaching an identity. A
language task can add the audited review tag; it cannot remove content. Current
source files, hints, privacy, quarantine, policy, request and response receipts
are rechecked before dispatch and in the transaction that writes the target,
tags, application provenance and task completion.

Committed application facts are retained separately from expiring provider
bodies. Ordinary processing may preserve those facts after response retention
ends, including after a restart, only while current source, policy, target and
tags still match. A missing or changed fact holds processing for explicit
reconciliation. Preservation does not authorize another provider request and
is checked again inside the ordinary persistence transaction.

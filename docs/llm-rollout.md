# Bounded model application

Start with a small, observable scope and keep existing privacy, audit and durable
request allowances. Successful runtime tests establish execution behavior.
Independent reference labels and prospective observations establish model
quality. Record these separately when operating a canary.

For type fallback, enable `CLASSIFIER_LLM_ENABLED` and
`CLASSIFIER_LLM_ENABLE_LIVE`, and set
`CLASSIFIER_LLM_LIVE_ALLOWED_TYPES=movie,tv` when those are retained by the existing
content policy. Other model categories remain unknown and record `policy_declined`.
Keep ordinary content exclusions in place. Review applied types against later
canonical references before expanding the allowed list.

After an accepted new movie/TV type passes the content policy, the default
workflow parses available release attributes once and checks the local mirror
when `local_search_enabled` is true. Attachment requires an exact source title
or stored alias, a compatible movie year and a unique local identity. Ambiguous,
saturated or unavailable local results leave the type and available attributes
usable without attachment. The `type-local-enriched` tag identifies these local
attachments; they do not receive `llm-matched`. This continuation makes no API
or additional model request. Custom workflows may use `enrich_type_fallback`
after their type policy and must apply that policy again afterward.
The reusable `EnrichTypeLocally` helper performs only this parsing and local
lookup. A background caller must validate its retained source/type/policy
evidence and persist with a transactional source check; it must not rerun the
default workflow to apply optional enrichment.

For language review, enable `CONTENT_FILTER_ENABLED` and
`CONTENT_FILTER_LLM_ENABLED`, set
`CONTENT_FILTER_LLM_ENFORCE=true` and `CONTENT_FILTER_LLM_ACTION=review`.
Audited non-English predictions add the model-attributed `llm-language-review`
tag through the normal persistence transaction. The torrent remains searchable.
English-inclusive releases retain their existing model protection. Deterministic
filtering still follows `CONTENT_FILTER_ENFORCE`. Review tags are evidence for an
operator review queue; inspect the source before treating them as language truth.

For junk handling, enable `JUNKPURGE_ENABLED` and set
`JUNKPURGE_ENABLE_PURGE=true` to enable restorable quarantine.
Automatic expiry retains the snapshot indefinitely. A small initial window can
use `JUNKPURGE_BATCH_SIZE=10`, `JUNKPURGE_INTERVAL=24h` and
`JUNKPURGE_MIN_AGE=720h`, while preserving the current junk-rate circuit breaker
and provider allowance. Inspect every initial quarantine and exercise restore,
including reclassification and search visibility. The manual delete endpoint
destroys the snapshot and blacklists the hash; keep that operator decision
separate from model judgment.

Restore preserves authentic source relationships and counts when retained in the
snapshot. Older snapshots remain usable. A separate `quarantine_restore` source
records the actual local restore time with unknown seed/leech counts and no
publish date. Under the default Torznab freshness policy, an unknown restored
item can resurface for seven days; authoritative tracker zero and liveness or
ledger exclusions still apply. Rematch is queued in the same transaction as
restoration, so an enqueue failure retains the snapshot for retry. Source
snapshots added by migration 55 cannot be discarded by a database downgrade
while retained recovery records use them.

For candidate shortlisting, enable `CLASSIFIER_LLM_MATCH_ENABLED` and
`CLASSIFIER_LLM_MATCH_ENABLE_LIVE`, then explicitly configure
`CLASSIFIER_LLM_MATCH_EMBEDDINGS_ENABLED=true`, the `/embeddings` endpoint, model
and credential. An OpenAI-compatible example uses `text-embedding-3-small` with
256 dimensions. The shared matcher allowance bounds all embedding and chat
requests. Full original candidates retain their identity and ambiguity checks;
the final chat result supplies attachment confidence. Inspect embedding captures,
shortlist provenance and final attachment receipts together. The legacy formal
capture exporter rejects the new task and rerank-v3 contract; retain those records
for an adapter preserving the full policy candidate set and embedding receipt.

Before applying new settings, qualify the exact released image, preserve a
compatible database backup and verify the deployment's effective content policy.
After deployment, check source/image identity, startup time, health, audit failures,
budget usage and actual application counters. A configured stage with no applied
effects needs investigation.

Set `CLASSIFIER_LLM_ENABLE_LIVE=false`, `CONTENT_FILTER_LLM_ENFORCE=false`,
`JUNKPURGE_ENABLE_PURGE=false` or `CLASSIFIER_LLM_MATCH_EMBEDDINGS_ENABLED=false`
to stop the corresponding new action independently. Existing language review tags
remain visible for review, and quarantined entries retain their restore snapshots.
Keep the previous qualified image and the matching database recovery procedure.

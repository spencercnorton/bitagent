# Offline review of shadow observations

Runtime tests prove policy and audit behavior. They do not prove model quality.
Use independent source labels, the task's formal evaluation workflow, held-out
release families, safety strata and prospective shadow acceptance before enabling
a model stage. Model confidence and agent-generated labels are diagnostics.

The optional offline command `go run ./tools/readiness` prepares blinded review
packets from a local `bitagent-shadow-snapshot-v1` JSON export. It never queries a
database, calls a provider, changes runtime configuration or grants promotion.
Keep real source exports and generated artifacts outside the source repository.

The utility supports `classifier_type`, `contentfilter` and `junkpurge` evidence.
It does not qualify catalogue matching or embeddings. The legacy formal capture
exporter rejects `matcher_embedding` and the embedding-shortlist rerank-v3
contract. Its successor must preserve the full `policy_candidates` set and the
embedding receipt before formal evaluation of that route.

```sh
go run ./tools/readiness freeze -snapshot snapshot.json -out review-packets \
  -reviewer-a reviewer-one -reviewer-b reviewer-two
go run ./tools/readiness score -snapshot snapshot.json -out diagnostic-report \
  -labels-a labels-one.json -labels-b labels-two.json
```

Each reviewer receives only their packet and label template. Do not provide the
raw snapshot, the other reviewer's answers or provider answers. Reviewers label
source evidence independently before model scoring. Fill every template entry;
retain its case ID and input hash. Templates default to `agent_diagnostic`.
`human` and `operator_reference` describe supplied provenance; the tool cannot
authenticate identity or create a formal `HumanReviewProof`.

Type labels: `movie`, `tv`, `music`, `book`, `audiobook`, `unknown`, `ambiguous`.
Language labels: `english`, `non_english`, `uncertain`, `ambiguous`. Label advertised
English-inclusive availability only when supported by source evidence. A title's
language alone does not prove its audio language. Use `uncertain` when evidence is
insufficient. Junk labels use the content classes in the existing versioned junk
disposition policy; the scorer derives disposition through that single policy.
Disagreements require separate adjudication in the formal review workflow.

The input schema retains capture, source, family, input and contract digests;
model, prompt, build and contract identities; model/task JSON; read-only snapshot
provenance; retained HTTP response bytes/hash and typed decision. Strict parsing
rejects duplicate keys, changed request digests and response hash mismatches.
The exporter's privacy admission and database provenance remain external trust
boundaries. An offline file cannot authenticate a claimed database snapshot.

Statistics isolate model, build, prompt, contract digest and enforcement policy.
Release families form the statistical denominator. Families sharing a grouped
junk request are joined into one statistical cluster, including connections
through repeated families, to avoid counting correlated judgments separately.
Review independence remains procedural; opaque IDs do not prove independence.
The reported conditional
action-risk upper bound measures potential errors among action families; it is
not the population keepworthy-loss metric required for promotion. Disagreements
and ambiguous actions count as potential harms. Legacy synthetic junk captures
without the exact grouped request and first response are unscorable. Every report
sets `grants_production_authority` to false and cannot replace formal gold closure,
calibration, harm gates or prospective shadow acceptance.

Output directories must be new; files are created with owner-only permissions.
Never publish real review packets, source titles or raw model responses.

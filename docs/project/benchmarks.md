# Benchmarks

Measured results for BitAgent's matching pipeline, each dated, with the exact
commands and the population they describe. A number here is a statement about
one corpus at one point in time — read the **Population** and **Limits** of a
run before quoting it.

## 2026-09-22 — identity matching against your \*arrs' own labels

**Question.** When BitAgent attaches a TMDB identity to a torrent, does it agree
with the identity Sonarr or Radarr assigned when they grabbed that torrent? And
which of BitAgent's matching features move that number?

**Answer, in one line.** The deterministic ladder agreed with the \*arr on
**83.3%** of gold rows; three matching features switched off did better
(**91.6%**); the whole difference traced to **one bug** in one of them; with it
fixed and deployed the ladder agrees on **93.7%**; title-level \*arr evidence and
a site-prefix parser fix, measured on the production crawler, take it to **97.8%**
with 99.3% precision and zero regressions. On what the deterministic pipeline
still cannot match, the LLM matcher's answer is the \*arr's identity 83% of the
time, and its identity gate has attached nothing wrong.

### Population

| | |
|---|---|
| Segment | `bench-2026-09` in `eval_frozen` |
| Source | `eval-freeze --fromCanonicalLabels all`: every infohash with a `torrent_canonical_labels` row carrying a media id — i.e. every torrent your \*arrs grabbed and told BitAgent about |
| Rows | **4,070** |
| Rows with a resolved expected TMDB id | **3,344** — movies carry a `tmdb:` id directly; TV rows carry `tvdb:` ids, resolved through a tvdb→tmdb map read from Sonarr's own `Series` table (1,326 of 1,482 series have a `TmdbId`) |
| Distinct expected titles | **262** (median 4 rows per title, maximum 599) |
| Ground truth | the \*arr's identity for the release it chose to grab. A human never looked at these; the \*arr did, via its own title matching and your choice to monitor the series |

### Method

`eval-replay` runs each frozen torrent through the real, fully-wired classifier
workflow and writes one JSON decision per row. It writes nothing to the
catalogue. Two replay defaults matter for reading the numbers:

- **Canonical-label preemption is bypassed**, so the matching ladder has to
  earn every match — otherwise the \*arr label would simply be copied back.
- **Candidates are local-only** (`--apisEnabled` off): matching uses the local
  TMDB content mirror, not live TMDB search. **LLM stages are always off** in
  replay; LLM lift is measured separately (`matcher-eval`).

A row **agrees** when the replay matched it to TMDB with the expected id and
type. Everything else — a different id, a type-only classification, no match —
counts against it.

### Results

**A** is the three matching features switched off: alternative-title matching
(`CLASSIFIER_ALT_TITLE_MATCH`), fuzzy matching (`CLASSIFIER_FUZZY_MATCH_ENABLED`)
and the anime alias backbone (`ANIME_TITLES_ENABLED`). **B** is production: all
three on. A is *not* upstream bitmagnet — it keeps every other BitAgent change
(the episode parser fixes, release granularity, `titlenorm`); it isolates these
three features.

| | Rows agree (n = 3,344) | Titles agree (n = 262, majority of rows) | Precision of what it attached |
|---|---|---|---|
| **A** — features off | **91.6%** [90.6, 92.5] | 82.8% [77.8, 86.9] | 95.6% |
| **B** — production | **83.3%** [82.0, 84.5] | **85.5%** [80.7, 89.2] | 94.9% |

Brackets are 95% Wilson intervals. The two columns disagree in direction, and
both are true:

- **Per title, production wins** — 19 titles right that A gets wrong, 12 the
  other way round. The 19 are exactly what the features were built for:
  releases under an Italian or Portuguese title, anime releases, a series renamed
  after launch, a regional spin-off.
- **Per row, production loses badly** — because one of its 12 losses is a
  long-running series with 221 rows in this corpus. A per-row score weights a
  title by how many of its episodes you grabbed.

**Precision barely moved** (94.9% vs 95.6%). Production does not attach more
*wrong* identities; it **gives up** more often — 409 rows with no match at all,
against A's 138.

### Which feature — single-flag ablations

Re-run on only the 409 rows where A and B disagree (`bench-2026-09-diff`),
switching one feature off at a time from production:

| Configuration | Rows correct (of 409) | Titles correct (of 44) |
|---|---|---|
| A — all three off | 344 | 13 |
| B — production, all on | 65 | 31 |
| anime backbone off | 65 | 31 |
| alternative titles off | 29 | 10 |
| **fuzzy off** | **384** | **33** |

- **Alternative-title matching is a clear win** — switching it off loses rows
  and titles.
- **The anime backbone is neutral** on this population (on the reference
  deployment it resolved about 1.5% of lookups).
- **Fuzzy matching is the whole regression.** Off, the configuration beats every
  other on both axes. It uniquely rescues 25 rows — films whose release year is
  one off TMDB's, and titles carrying a regional suffix — and costs 344.

Both flags had shipped as experiments. `config.go` says of `AltTitleMatch`:
*"Off by default until precision is measured"*; of `FuzzyMatchEnabled`:
*"Flip to true … for A/B shadow measurement."* Both were switched on in
production; this was the first measurement.

### Root cause

`fuzzyFindBestMatch` normalises both titles with `titlenorm.NormalizeTitleForMatch`,
which **deliberately keeps punctuation** — its sibling `FamilyKey` documents why:
*"the fuzzy matcher absorbs it in its distance metrics."* The Levenshtein distance
does. But the scorer applies a **first-token precision gate before computing that
distance**, by exact string equality, so:

```
parsed from the release:  "planes trains and automobiles"   first token "planes"
TMDB title, normalised:   "planes, trains and automobiles"  first token "planes,"
                                                             → gate rejects; distance never computed
```

Ten of the 12 titles production lost have punctuation in their first word — a
comma, a colon or a hyphen — and account for all 322 of the rows it lost to a
type-only result. The comment's own example, `"dexter: new blood"`, fails the
same way.

The other two are a **different fuzzy defect this fix does not address**. They
are wrong picks, not misses: article stripping makes *The X* normalise to
exactly `x`, tying with a different title that is just *X*, and the tie goes to
whichever candidate was retrieved first; a regional suffix (`US`) lost in parsing
ties the UK and US versions of one show the same way. With one same-name reality
franchise, that is all 22 of the rows production matched to the wrong identity. Fuzzy needs a
tie-break that prefers the candidate closest *before* normalisation — or,
better, the same evidence-learned preference described under **the frontier**
below.

A second defect rode along: normalisation converts a standalone roman numeral to
a digit, so a release's standalone `X` became `10` while the catalogue's
hyphenated `X-…` (one token, not purely roman) stayed as it was.

**Fix** (`internal/classifier/fuzzy.go`, `foldTitlePunct`): the scorer folds
punctuation on the raw strings *before* normalising — apostrophes deleted,
every other punctuation mark a word break — so both sides get identical
tokenisation and identical roman-numeral handling. The shared normaliser is
unchanged; its other callers (the LLM identity gate, `FamilyKey`, the
evaluation tools) see no difference. One fix covers all three callers of the
scorer: local search, TMDB movie search and TMDB TV search.
`TestFuzzyFindBestMatch_PunctuatedCanonicalTitles` pins 14 real cases; 11 of
them fail without the fix.

**Measured after deploy** (v2.9.2 on the production crawler, same segment, same
replay, 2026-09-22 20:48Z — the running image's OCI revision label is the fix's
merge commit):

| | Rows agree (n = 3,344) | Titles agree (n = 262) | No match at all | Precision of attached |
|---|---|---|---|---|
| v2.8.1 (before) | 83.3% | 85.5% | 409 | 94.9% |
| features off (A) | 91.6% | 82.8% | 138 | 95.6% |
| **v2.9.2 (after)** | **93.7%** [92.8, 94.5] | **90.8%** [86.7, 93.8] | **63** | 95.5% |

Against the expectations stated before measuring: all **322** type-only rows
returned; all **65** rows production had gained over A were kept; **zero** rows
that were correct before became wrong. The one miss: I expected fuzzy's 22 wrong
picks to be unchanged, and **13 of them are now correct** — folding punctuation
changed several ties. Nine remain.

### What neither configuration gets right — the frontier

215 rows across 48 titles fail in both A and B. They split cleanly:

- **Same-name disambiguation (~130 rows).** Several catalogue entries share a
  name and BitAgent picks the wrong one: a reality franchise's US and Australian
  versions (88 rows), a UK series and its US revival, two originals and their
  revivals, a UK format and its Canadian version, a Spanish edition of an
  international format. A deterministic ladder cannot choose
  without outside evidence — and **your \*arrs already supplied it**: every one
  of these rows carries the \*arr's identity for that exact release series.
  See the next section.
- **Parser failures (~80 rows).** Site prefixes followed by spaces instead of a
  dash (`www.<site>.org      <Title> S01E09 …`), CJK title prefixes
  (`<中文片名>.<English.Title>.2026 …`), `EP`-numbered absolute episodes
  (`<Title>.EP1166 …`), batch ranges (`<Title> 001-367`). Each is a deterministic
  fix.

### Title-level \*arr evidence — measured offline, before deploy

`CLASSIFIER_EVIDENCE_TITLE_IDENTITY` (v2.10.0, off by default) lets the
\*arr labels on **other** torrents with the same parsed title choose the
identity of a torrent that has no label of its own
([`docs/evidence.md`](../evidence.md) §3). Leave-one-out is built in: a torrent
never votes for itself.

Run through the shipped `TitleEvidence` code over these gold rows — every row
in turn treated as a new torrent, voted on by all the others:

| Baseline | Correct | With title evidence | Fixed | Broken |
|---|---|---|---|---|
| v2.8.1 as measured | 2,785 | 3,252 (97.2%) | 467 | 0 |
| **v2.9.2 as deployed and measured** | **3,134** | **3,275 (97.9%)** | **141** | **0** |

Outcomes over the 3,344 rows: `applied` 2,962 · `no_preference` 254 ·
`weak` 128. The 141 are the same-name frontier above plus the nine fuzzy wrong
picks the fix left — resolved without the evidence being told which were wrong.
This is a projection from real decisions; the replay with the flag on, after
v2.10.0 deploys, is the measurement.

**What this does and does not show.** The votes and the ground truth are both
\*arr labels, so on this corpus the feature is, by design, "agree with your
\*arr about a title it has already grabbed twice" — which is exactly the
production case: a new episode of a show you monitor. It says nothing about
titles you have never grabbed (those get `no_preference` and the ordinary
matcher). And "broke 0" is a property of this library: an operator whose
\*arrs label one parsed title as two different series would see the ⅔ rule
decline, and a minority label could be outvoted.

**Measured after deploy** (v2.10.0 on the production crawler, same segment,
`eval-replay` with `CLASSIFIER_EVIDENCE_TITLE_IDENTITY=true` in the replay's own
process, 2026-09-22 21:20Z; production itself still had the flag off):

| | Rows agree (n = 3,344) | Titles agree (n = 262) | No match at all | Precision of attached |
|---|---|---|---|---|
| v2.9.2, as deployed | 93.7% | 90.8% | 63 | 95.5% |
| **v2.10.0 + title evidence** | **97.5%** [97.0, 98.0] | **93.1%** [89.4, 95.6] | **59** | **99.3%** |
| v2.10.2, production config (21:39Z) | **97.8%** [97.2, 98.2] | 93.1% | 51 | 99.3% |

The last row adds v2.10.1's site-prefix fix (a `www.<host>` followed by spaces
rather than a dash): exactly the 8 rows it was predicted to fix, 0 broken.

**128 rows fixed, 0 broken** — 13 fewer than projected. The two computations
differ: the projection voted with the benchmark's resolved TMDB ids over the
gold rows only, while the shipped index votes with each \*arr label's id as
stored (`tvdb:` for TV) over every labelled torrent in the database, and the
winner must then resolve through the local content mirror. The replay is the
number. The gain is almost entirely precision: same-name confusion the \*arrs
had already resolved — a reality franchise's US and Australian versions (83
rows) and six other same-name pairs. The flag was switched on in production
after this replay.

What the 82 remaining rows are:

- **No match, 59 rows** — parser failures on absolute-numbered anime
  (`<Title>.EP1166 …`, `[Group] <Title> - E41 - …`), batch ranges
  (`<Title> 001-367`), a CJK title prefix, `S02 Season 2` duplication, site
  prefixes without a dash (fixed in v2.10.1), and a few titles missing from the
  local content mirror. This is
  the population the LLM matcher sees in production — see below.
- **Wrong identity, 23 rows** — 14 where title evidence applied a series other
  than the one this release's \*arr label names (the \*arrs' own labels for the
  title disagree — an anthology series whose seasons are filed under different
  entries, a docuseries), and 9 the ladder picked without evidence.

### The LLM matcher on what the ladder cannot match

**Question.** On the rows the deterministic ladder leaves unmatched, does the LLM
TMDB matcher find the identity your \*arr chose — and does it attach anything
wrong?

**Population.** The 59 gold rows with no match after title evidence (v2.10.0),
frozen as `bench-2026-09-resid`. First, the ladder itself re-ran them with live
TMDB search on (`eval-replay --apisEnabled`, which the main replay leaves off):
it matched **3** correctly and **1** wrongly. So 55 rows are beyond the
deterministic pipeline even with TMDB's own search. Of the 59, **42** had been
typed movie or TV but not matched — the population the matcher serves in
production; 17 were never typed at all.

**Method.** `matcher-eval --sample` over the 59, on the production crawler
(v2.10.2, 2026-09-22 21:34Z), model `gpt-5.6-sol`, the production decide path
unchanged: admission gates, local-mirror-first candidates, then TMDB search, then
the post-rerank identity gate. It never attaches. v2.10.2 gave the tool its own
call allowance — before that it drew on the crawler's shared budget ledger, which
the crawler spends within minutes each day, and that is why this had never been
measured.

**Results — the 42 typed rows:**

| Stage | Rows | |
|---|---|---|
| Not sent — withheld by the privacy gate | 20 | By design: nothing marked private is ever sent to a model |
| Sent to the model | **22** | |
| — the model's answer was the \*arr's identity | **15** | 83% of the 18 it answered |
| — the model's answer was wrong | 3 | |
| — no answer, or no candidates | 4 | |
| **Accepted by the identity gate** | **6 — all correct** | +6 rows the whole deterministic pipeline, live TMDB included, cannot match |
| Withheld by the identity gate | 9 correct · 3 wrong | the gate stopped every wrong answer, and 60% of the right ones |

Accepted precision was **100%** (0 wrong attachments). One of the six is a
series the ladder with live TMDB attached to a different entry. Three of the six
and one withheld answer are site-prefixed names that v2.10.1's parser fix now
matches deterministically — against today's ladder the matcher's accepted lift
is 3 rows, and the right answers its gate withholds are 8.

The 17 untyped rows barely reach the model: 14 carry no file listing, so the
plausibility gate declines them before any call; of the 3 sent, 2 answers were
right and 1 wrong, and the gate withheld all three.

**What this says.** The model is not the bottleneck on this residual — the
identity gate is. Every withheld right answer came with model confidence ≥ 0.99:
films with a CJK title prefix, Spanish releases carrying the English title in
parentheses, an anime re-edition, an anime series pack. After the model answers,
production also requires the chosen entry to agree with a title parsed
independently of the model (`CLASSIFIER_LLM_MATCH_REQUIRE_SOURCE_TITLE`), and on
names like these that parse is empty (`<Title>.EP1168 …`) or carries the prefix
(`<中文片名> <English Title>`). Since v2.10.4 `matcher-eval` records which check
declined each row (`gate_reason`). Loosening the gate where the model's own
extraction and the candidate agree exactly is the next measurable lever: up to +9
rows here, with the three wrong picks (a film matched to a same-name film, a
franchise film matched to an earlier entry, an anime arc filed under a different
TMDB entry) as the precision test. That is a gate change
measured on this segment, not a new model or an embeddings stage.

**Limits.** 22 rows is a small sample — the 95% interval on 15/18 correct answers
is 61–94%. The residual is by construction the hardest slice of a clean corpus.
One model, one prompt version (`v4-2026-07-19-dual-audio-english`).

### Limits

- **This corpus is clean.** Gold rows are releases your \*arrs grabbed through
  indexers — mostly well-formed scene and P2P names. The DHT crawl's messy long
  tail, which the fuzzy and alternative-title features partly exist for, is
  under-represented. These numbers describe matching on the releases you
  actually want; they are not a claim about the whole catalogue.
- **Local-only.** The main replays use the local content mirror only; the live-TMDB
  path was exercised once, on the 59-row residual (see the LLM section).
- **No LLM in replay.** Replay keeps every LLM stage off by design; the LLM matcher
  is measured separately, on the residual, above.
- **Exact identity.** A regional variant counts as wrong even when arguably
  acceptable.
- **One mirror snapshot** — the local content mirror as of 2026-09-22.

### Reproduce

Inside the running container (`worker run --all`; replay is read-only against
the catalogue):

```bash
# tvdb→tmdb map from Sonarr's database, read-only (no API calls)
python3 - <<'EOF' > /tmp/tvdbmap.json
import json, sqlite3
db = sqlite3.connect("file:/path/to/sonarr.db?mode=ro", uri=True)
print(json.dumps({str(tvdb): {"tmdb_tv": tmdb}
                  for tvdb, tmdb in db.execute("select TvdbId, TmdbId from Series where TmdbId > 0")}))
EOF

bitagent eval-freeze --segment bench-2026-09 --fromCanonicalLabels all \
  --tvdbMap /tmp/tvdbmap.json --replace

# B — production
bitagent eval-replay --segment bench-2026-09 --out /tmp/replay-B.jsonl

# A — the three features off
CLASSIFIER_ALT_TITLE_MATCH=false CLASSIFIER_FUZZY_MATCH_ENABLED=false \
ANIME_TITLES_ENABLED=false \
  bitagent eval-replay --segment bench-2026-09 --out /tmp/replay-A.jsonl
```

Replay is deterministic: re-running A and B on the 409-row diff segment
reproduced the full-run results exactly (344 and 65 correct). A full run takes
~2 minutes with the features off and ~11 with them on.

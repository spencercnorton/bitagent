# Release-name eligibility

The optional `name_policy` module is disabled by default. Enabling it changes
eligibility for acquisition, imports, processing and model work; it retains
existing raw torrents and protection/history records. Denial is a terminal
no-work decision and never calls a destructive block, delete or quarantine
writer.

```yaml
name_policy:
  enabled: false
  excluded_info_hashes: []
  internal_check_token: ""
```

The policy examines the release name. Exact Unicode Han and Cyrillic script
membership excludes a name, including a name that also advertises an English
dub. Accented Latin names, Latin transliterations and incidental foreign
support-file paths remain eligible. An unavailable name is ineligible when
the policy is enabled.

Adult names require multiple distinct terms and a precise strong anchor, or
an explicit porn/porno plus XXX pair. Delimited and defined compact/obfuscated
forms are covered by the same declaration. Standalone xXx, anal, MILF, porn
or porno and weak combinations with ordinary comedy advertising do not create
an adult verdict. Explicit stored adult classification is a separate guard.
No language, country, audio or catalogue identity is inferred from a denial.

Operators may configure up to 4096 exact SHA-1 infohash exclusions. Keep real
exclusions in operator configuration; they are neither a title list nor an
origin ban. The shared decision version is `release-name-policy-v1`; its
fingerprint binds the declaration, Unicode version, enabled state and sorted
hashes. The internal token is excluded from that fingerprint and redacted from
configuration displays.

Names are checked before expensive parsing, model task admission and model
egress. Existing imports resolve the stored name within their write transaction
and recheck after conflict handling. Provider boundaries use the current stored
source when processing existing records. Application transactions recheck the
source after a response; rejected retained work stays unapplied, with permanent
dispatch history, responses and budget debits intact. Batch upload/create holds
source locks through its bounded provider operation; already-paid results can
be reconciled without a new POST and are held before settlement after denial.
Batch admission and complete recovery remain separately configured features.

## Internal name-only check

`POST /internal/name-policy/check` accepts an operator-authenticated batch of
1–32 names owned by another application. Each UTF-8 name is at most 4096 bytes;
the JSON body is at most 160 KiB. An optional `infoHash` must be 40 hexadecimal
characters. The bearer credential is `name_policy.internal_check_token` (or
`NAME_POLICY_INTERNAL_CHECK_TOKEN`), a separate 32–256 byte secret supplied at
startup. With the policy disabled or no token configured, the route returns
404; invalid authorization returns 403, malformed input 400 and an oversized
body 413. Unknown fields and trailing JSON are refused.

```json
{"releases":[{"name":"Synthetic.Café.2026.mkv"}]}
```

```json
{"enabled":true,"version":"release-name-policy-v1","results":[{"index":0,"eligible":true,"reason":"allowed","version":"release-name-policy-v1"}]}
```

The response never echoes a name, hash or token. This pure check performs no
database writes or provider calls. It decides name eligibility; the calling
application must separately retain its membership, source, privacy and
readiness authority. It does not authorize a public hash or accept an alternate
caller name as authority for a stored public torrent.

Denial metrics use only finite stage and reason labels. They count checks,
which can repeat for one release; they do not assert unique exclusions or
measured monetary savings.

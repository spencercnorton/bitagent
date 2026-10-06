# Catalogue policy shadow review

`catalogue-policy-shadow` evaluates public raw source evidence locally. It
defaults to a read-only transaction and reports aggregate states and a resume
cursor. `--write` persists a bounded review receipt, without changing serving,
classification, crawler blocking, quarantine or deletion. It makes no provider,
tracker, DHT or client requests. Nothing runs automatically at startup.

```sh
bitagent catalogue-policy-shadow --limit 128
bitagent catalogue-policy-shadow --limit 128 --anime-english-subs --write
bitagent catalogue-policy-shadow --limit 128 --after-hash <40-hex-character-hash>
```

Each invocation inspects at most 1,024 hashes in pages of at most 128. Private
raw rows, private/bitgrab categories and tags, and private client observations
are excluded. Each receipt retains the current name/file evidence, original
source observation IDs/times/expiry, parser and policy versions, configuration
and a SHA-256 input digest. The digest describes a consistent historical source
snapshot; it is not proof that the source cannot change later. Every evaluation
loads the source and privacy again. Concurrent writes serialize per receipt;
serialization conflicts fail the invocation and can be revisited in a later run.
Older evaluations cannot replace a newer persisted receipt.

English audio and subtitles are separate advertised claims, not inspected
media tracks. Bare title language, translated titles, metadata original language,
legacy `english_audio`, generic dual/multi audio and group reputation do not
prove English in this contract. Explicit English audio qualifies movies and
shows. The optional anime subtitle preference is a draft input; it is disabled
unless the flag is supplied. Missing, contradictory and mixed pack evidence
remains unknown or reviewable. Multi-member packs require member evidence and
file lists over 256 members abstain. An explicit negative requires an advertised
absence or an audio-only list; a foreign-language dub alone cannot prove that
another English track is absent. No result authorizes language enforcement.

Availability states are `fresh_positive`, `fresh_known_zero`, `unknown_stale`
and `suspected_unavailable`. `policy_confirmed_unavailable` is reserved for a
future independently accepted policy; this implementation cannot produce it.
A tracker timeout/unknown result and one zero never establish unavailability.
Repeated source observations must be distinct, qualified, separated in time,
fresh, and unprotected by a recent positive. Public client stalls qualify only
with an explicit healthy-network observation; paused/error/missing/unknown
states do not qualify. Fresh public client/DHT/tracker positives revive the
shadow classification immediately. DHT approximate positives use their original
source creation time, rather than a later denormalized catalogue timestamp.

The experimental defaults are 24-hour freshness, seven-day recent-positive
protection, at least two observations separated by at least one hour over at
least four hours. These are shadow inputs, not an accepted production risk
budget. At most 64 original observations are retained per hash, with a seven-day
history horizon. Duplicate reads do not manufacture repeated observations.
Re-evaluation after negative evidence expires returns unknown; a stored receipt
is historical evidence and must never be treated as a permanent exclusion.

The legacy anime advertised audio tag also stops treating unnamed dual/multi
audio as English. Its destructive foreign-audio filter keeps generic audio
uncertainty rather than converting the corrected tag into a new drop. Explicit
English track evidence remains supported. Matcher extraction has a new contract
version and the same distinction in both single and grouped prompts; exact
rendered-input cache identity prevents replaying a previous prompt's result.

Promotion requires an independently labeled harm assessment, an accepted risk
budget, reversible serving design and a qualified recovery path. This command
does not meet or bypass those gates, and must not be used to activate the broad
legacy content-filter enforcement switch.

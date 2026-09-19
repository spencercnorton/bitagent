// Package contentfilter is the operator's curation layer for what
// bitagent persists. It runs AFTER BEP-9 metadata is fetched and the
// CEL classifier has produced a content_type guess, but BEFORE the
// torrent enters the persist queue.
//
// Design constraints from the operator (2026-04-25):
//
//  1. **English-only.** Drop torrents whose classified language tag
//     is non-English. The classifier's TMDB lookup populates
//     `torrent_contents.languages` for ~60% of titles; for the
//     ~40% that come back empty/null, we fall back to
//     script-detection on the title (Cyrillic, CJK, Arabic,
//     Hebrew, Devanagari, Thai → drop). The remaining residue
//     ("plausibly English by script alone, no language tag") is
//     where Phase 2's LLM tier will sit; this MR doesn't include
//     that yet.
//
//  2. **No mp3-only music torrents.** Real music in lossless
//     formats (flac, alac, m4a, wav, ape, opus) is fine. mp3 is
//     out by operator preference. Mixed (some mp3 + some flac) is
//     kept; pure-mp3 torrents are dropped.
//
//  3. **No iso, zip, ts, rar, exe, dmg, msi, pkg, deb.** Software
//     installers and archive containers — not media. The audit
//     showed ~3k of these out of 3M torrents (0.1% by count) but
//     they're zero-value and cheap to drop.
//
//  4. **No porn / NSFW.** Keyword + classification heuristic;
//     conservative on the keyword side because false positives
//     here cost less than false negatives.
//
//  5. **Highly efficient by default.** Phase 1 is pure Go,
//     deterministic, O(1) per torrent. No LLM, no network calls,
//     no DB lookups during the decision. Phase 2 will add an LLM
//     tier with sha256-keyed LRU cache + automatic rule mining
//     (when N consecutive LLM calls return the same "drop reason"
//     for the same script/pattern, the rule is codified).
//
// Output:
//
//  - `Decision{Allow bool, Reason DropReason}` returned from
//    `Filter.Decide(...)`.
//  - `bitagent_contentfilter_*` metrics: drop_total{reason},
//    keep_total, examined_total. In shadow mode (`Enforce=false`),
//    `would_drop_total{reason}` lights up while every torrent is
//    still kept — same counterfactual measurement primitive used
//    by retention and peerrep.
package contentfilter

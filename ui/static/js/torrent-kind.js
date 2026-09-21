/* ── Shared torrent "kind" classifier ────────────────────────────────────────
 * Single source of truth for classifying a release name as complete | season |
 * episode. Loaded as a classic script BEFORE both app.js and library.js, which
 * call `parseTorrentKind` directly. This module previously lived duplicated
 * (verbatim) as parseKind() in library.js and _parseTorrentKind() in app.js;
 * they drifted-by-lockstep once before being consolidated here so the
 * logic can only ever change in one place.
 *
 * Complete-series is a *positive* match (multi-season range / "complete
 * series"), not a catch-all, so a season pack that merely contains the word
 * "complete" (e.g. "[S02_complete]") is not misfiled as a whole-series pack.
 * Season matching uses (?!\d) rather than a trailing \b so a following "_" or
 * letter (S01_, S02_complete, dangling S02E) doesn't break the match. British
 * "Series N" == Season N.
 */
function parseTorrentKind(name) {
  if (/\bS\d{1,2}\s*[-–~]\s*S?\d{1,2}\b/i.test(name) ||
      /\bseasons?[\s._-]*\d{1,2}\s*[-–~]\s*\d{1,2}\b/i.test(name) ||
      /\bcomplete[\s._-]+(?:series|collection|pack|seasons?)\b/i.test(name) ||
      /\ball[\s._-]+seasons\b/i.test(name)) return { kind: 'complete' };
  // Episode: SxxExx (optional separator, tolerates PROPER/v2 suffixes) or 1x04.
  const ep = name.match(/\bS(\d{1,2})[\s._-]?E(\d{1,3})/i) || name.match(/\b(\d{1,2})x(\d{1,3})\b/i);
  if (ep) return { kind: 'episode', season: +ep[1], episode: +ep[2] };
  const se = name.match(/\bS(\d{1,2})(?!\d)/i) || name.match(/\b(?:Season|Series)[\s._-]*(\d{1,2})\b/i);
  if (se) return { kind: 'season', season: +(se[1] || se[2] || 0) };
  return { kind: 'complete' };
}

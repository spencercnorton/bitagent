/* ── Shared grouping-key title normalizer ────────────────────────────────────
 * Single source of truth for reducing a release name to a groupable base title:
 * strip the season / episode / resolution / source suffixes, collapse
 * whitespace, lower-case. Loaded as a classic script BEFORE both app.js and
 * library.js, which call `normGroupTitle` directly to build the
 * `contentType:name:<norm>` group key for UNMATCHED (non-TMDB) torrents.
 *
 * This module previously lived duplicated (byte-identical regex bodies) as
 * normTitle() in library.js and _normGroupTitle() in app.js; consolidated here
 * (mirroring torrent-kind.js) so the logic can only ever change in one place.
 * Uses the null-safe `String(name || '')` guard — the safer of the two former
 * bodies — so a null/undefined name yields '' instead of throwing.
 *
 * Season stripping uses (?!\d) rather than a trailing \b so a following "_" or
 * letter ("S02_complete") still matches and groups with S01.
 */
function normGroupTitle(name) {
  return String(name || '')
    .replace(/\bS\d{1,2}E\d{1,3}(?:-E\d{1,3})?.*/i, '')
    .replace(/\bS\d{1,2}(?!\d)(?!E).*/i, '')
    .replace(/\bSeason\s+\d+.*/i, '')
    .replace(/\b(480|576|720|1080|2160)[pi]?.*/i, '')
    .replace(/\b(BluRay|BDRip|WEBRip|WEB[-.]DL|HDTV|DVDRip|REMUX|HDR|SDR|IMAX|FLAC|MP3|AAC)\b.*/i, '')
    .replace(/\s+/g, ' ').trim().toLowerCase();
}

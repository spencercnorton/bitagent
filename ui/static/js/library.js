/* ============================================================================
   BitAgent Library — public library-only frontend.
   Self-contained (does not depend on app.js). Talks to the same FastAPI:
     GET /api/library/stats        — public corpus totals for the snapshot banner
     GET /api/torrents             — search + filter the library
     GET /api/titles/releases      — every release of one title
     GET /api/torrents/{infoHash}  — full torrent metadata + files
     GET /api/meta/{type}/{tmdbId} — enriched TMDB details (hero + seasons)
     GET /api/meta/tv/{id}/season/{n} — episode catalog
     GET /api/poster/{id}          — cached TMDB poster proxy
   ========================================================================== */
'use strict';

/* ── Theme ──────────────────────────────────────────────────────────────── */
function syncThemeColor() {
  const dark = document.documentElement.getAttribute('data-theme') === 'dark';
  const meta = document.querySelector('meta[name="theme-color"]');
  if (meta) meta.setAttribute('content', dark ? '#0f1117' : '#ffffff');
}
function toggleTheme() {
  const isDark = document.documentElement.getAttribute('data-theme') === 'dark';
  const next = isDark ? 'light' : 'dark';
  document.documentElement.setAttribute('data-theme', next);
  try { localStorage.setItem('bitagent-theme', next); } catch (_) {}
  syncThemeColor();
}
if (typeof document !== 'undefined') syncThemeColor();

/* ── Small helpers ──────────────────────────────────────────────────────── */
function escHtml(s) {
  return String(s == null ? '' : s)
    .replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;')
    .replace(/"/g, '&quot;').replace(/'/g, '&#39;');
}
function escAttr(s) { return escHtml(s).replace(/`/g, '&#96;'); }
function jsStringArg(s) {
  return String(s == null ? '' : s).replace(/\\/g, '\\\\').replace(/'/g, "\\'");
}
function fmtNum(n) { return (n || 0).toLocaleString('en-US'); }
function fmtBytes(b) {
  b = +b || 0;
  if (b === 0) return '—';
  const u = ['B', 'KB', 'MB', 'GB', 'TB'];
  const i = Math.min(Math.floor(Math.log(b) / Math.log(1024)), u.length - 1);
  return (b / Math.pow(1024, i)).toFixed(i ? 1 : 0) + ' ' + u[i];
}
function fmtDate(v) {
  if (!v) return '—';
  const d = new Date(typeof v === 'number' ? v * 1000 : v);
  if (isNaN(d)) return '—';
  return d.toLocaleDateString('en-US', { year: 'numeric', month: 'short', day: 'numeric' });
}
function fmtRuntime(min) {
  min = +min || 0;
  if (!min) return '';
  const h = Math.floor(min / 60), m = min % 60;
  return h ? `${h}h ${m}m` : `${m}m`;
}
function debounce(fn, ms) {
  let t;
  const wrapped = (...a) => { clearTimeout(t); t = setTimeout(() => fn(...a), ms); };
  wrapped.cancel = () => clearTimeout(t);
  return wrapped;
}
async function api(path, options = {}) {
  try {
    const headers = Object.assign({ 'Accept': 'application/json' }, options.headers || {});
    const r = await fetch(path, Object.assign({}, options, { headers, credentials: 'same-origin' }));
    if (!r.ok) return null;
    return await r.json();
  } catch (_) { return null; }
}

/* ── Library snapshot banner ────────────────────────────────────────────── */
function _isUtcTimestamp(value) {
  return typeof value === 'string' && /(?:Z|\+00:00)$/i.test(value) &&
    Number.isFinite(Date.parse(value));
}
function normalizeLibraryStats(data) {
  if (!data || typeof data !== 'object') return null;
  if (!Number.isSafeInteger(data.totalReleases) || data.totalReleases < 0) return null;
  if (!Number.isSafeInteger(data.releasesAddedLast7Days) || data.releasesAddedLast7Days < 0) return null;
  if (typeof data.totalReleasesIsEstimate !== 'boolean') return null;
  if (!_isUtcTimestamp(data.windowStart) || !_isUtcTimestamp(data.windowEnd) ||
      !_isUtcTimestamp(data.observedAt)) return null;
  if (Date.parse(data.windowStart) > Date.parse(data.windowEnd)) return null;
  return {
    totalReleases: data.totalReleases,
    totalReleasesIsEstimate: data.totalReleasesIsEstimate,
    releasesAddedLast7Days: data.releasesAddedLast7Days,
    windowStart: data.windowStart,
    windowEnd: data.windowEnd,
    observedAt: data.observedAt,
  };
}
function libraryStatsViewFor(data) {
  const stats = normalizeLibraryStats(data);
  if (!stats) return null;
  const totalNumber = fmtNum(stats.totalReleases);
  const totalText = `${stats.totalReleasesIsEstimate ? '~' : ''}${totalNumber}`;
  return {
    totalText,
    addedText: fmtNum(stats.releasesAddedLast7Days),
    totalHint: stats.totalReleasesIsEstimate ? 'Estimated indexed releases' : 'Indexed releases',
    addedHint: 'Last 7 days',
    statusText: `${stats.totalReleasesIsEstimate ? 'Approximately ' : ''}${totalNumber} total torrents and ${fmtNum(stats.releasesAddedLast7Days)} added in the last 7 days.`,
  };
}
function renderLibraryStats(data) {
  if (typeof document === 'undefined') return false;
  const grid = document.getElementById('libStatsGrid');
  const total = document.getElementById('libStatTotal');
  const added = document.getElementById('libStatAdded');
  const totalHint = document.getElementById('libStatTotalHint');
  const addedHint = document.getElementById('libStatAddedHint');
  const unavailable = document.getElementById('libStatsUnavailable');
  const status = document.getElementById('libStatsStatus');
  if (!grid || !total || !added || !totalHint || !addedHint || !unavailable || !status) return false;

  const view = libraryStatsViewFor(data);
  grid.setAttribute('aria-busy', 'false');
  [total, added].forEach(el => el.classList.remove('is-loading'));
  if (!view) {
    total.textContent = '—';
    added.textContent = '—';
    total.setAttribute('aria-hidden', 'true');
    added.setAttribute('aria-hidden', 'true');
    totalHint.textContent = 'Unavailable';
    addedHint.textContent = 'Last 7 days · unavailable';
    unavailable.hidden = false;
    status.textContent = 'Library totals are temporarily unavailable.';
    return false;
  }

  total.textContent = view.totalText;
  added.textContent = view.addedText;
  total.removeAttribute('aria-hidden');
  added.removeAttribute('aria-hidden');
  totalHint.textContent = view.totalHint;
  addedHint.textContent = view.addedHint;
  unavailable.hidden = true;
  status.textContent = view.statusText;
  return true;
}
async function loadLibraryStats(retry = true) {
  // This request deliberately owns no browse AbortController: filter and page
  // navigation must never cancel the independent banner snapshot.
  const data = await api('/api/library/stats');
  renderLibraryStats(data);
  // A cold server cache answers {available:false} while it is still counting;
  // ask once more after it has had time to finish, never in a loop.
  if (retry && data && data.available === false) setTimeout(() => loadLibraryStats(false), 20000);
}

/* ── Accessibility helpers ──────────────────────────────────────────────── */
function _reducedMotion() {
  return !!(window.matchMedia && window.matchMedia('(prefers-reduced-motion: reduce)').matches);
}
function _scrollBehavior() { return _reducedMotion() ? 'auto' : 'smooth'; }

/* Dialog a11y: move focus into an opened overlay, trap it by marking the
   background inert, lock body scroll, and restore focus to the trigger on
   close. Nesting-aware (the torrent drawer opens over the detail view). */
const _modalStack = [];
function _applyModalInert() {
  const anyOpen = _modalStack.length > 0;
  [document.querySelector('.lib-topbar'), document.getElementById('libBrowse')]
    .forEach(el => { if (el) el.toggleAttribute('inert', anyOpen); });
  const detailEl = document.getElementById('libDetail');
  if (detailEl) {
    const idx = _modalStack.findIndex(m => m.panel === detailEl);
    detailEl.toggleAttribute('inert', idx !== -1 && idx < _modalStack.length - 1);
  }
}
function openModal(panel) {
  _modalStack.push({ panel, ret: document.activeElement });
  document.body.style.overflow = 'hidden';
  _applyModalInert();
  requestAnimationFrame(() => {
    const t = panel.querySelector('.lib-detail-back, .lib-iconbtn, button, [href], input, [tabindex]:not([tabindex="-1"])');
    try { (t || panel).focus(); } catch (_) {}
  });
}
function closeModal(panel) {
  const i = _modalStack.map(m => m.panel).lastIndexOf(panel);
  if (i === -1) return;
  const m = _modalStack.splice(i, 1)[0];
  if (!_modalStack.length) document.body.style.overflow = '';
  _applyModalInert();
  if (m.ret && document.contains(m.ret)) { try { m.ret.focus(); } catch (_) {} }
}

/* ── Content-type metadata ──────────────────────────────────────────────── */
const TYPE_META = {
  movie:    { label: 'Movies',   media: 'movie' },
  tv_show:  { label: 'TV',       media: 'tv' },
  music:    { label: 'Music',    media: 'movie' },
  ebook:    { label: 'Books',    media: 'movie' },
  software: { label: 'Software', media: 'movie' },
  unknown:  { label: 'Other',    media: 'movie' },
};
// Only the mapped, poster-bearing categories are user-facing. Music / books /
// software aren't matched in our index, so they're intentionally omitted.
const TYPE_ORDER = ['', 'movie', 'tv_show'];
const ICONS = {
  movie: '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.5"><rect x="2" y="2" width="20" height="20" rx="2"/><line x1="7" y1="2" x2="7" y2="22"/><line x1="17" y1="2" x2="17" y2="22"/><line x1="2" y1="12" x2="22" y2="12"/><line x1="2" y1="7" x2="7" y2="7"/><line x1="2" y1="17" x2="7" y2="17"/><line x1="17" y1="7" x2="22" y2="7"/><line x1="17" y1="17" x2="22" y2="17"/></svg>',
  tv_show: '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.5"><rect x="2" y="7" width="20" height="15" rx="2"/><polyline points="17 2 12 7 7 2"/></svg>',
  music: '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.5"><path d="M9 18V5l12-2v13"/><circle cx="6" cy="18" r="3"/><circle cx="18" cy="16" r="3"/></svg>',
  ebook: '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.5"><path d="M4 19.5A2.5 2.5 0 016.5 17H20"/><path d="M6.5 2H20v20H6.5A2.5 2.5 0 014 19.5v-15A2.5 2.5 0 016.5 2z"/></svg>',
  software: '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.5"><polyline points="16 18 22 12 16 6"/><polyline points="8 6 2 12 8 18"/></svg>',
  unknown: '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.5"><circle cx="12" cy="12" r="10"/><path d="M9.09 9a3 3 0 015.83 1c0 2-3 3-3 3"/><line x1="12" y1="17" x2="12.01" y2="17"/></svg>',
};
function iconFor(ct) { return ICONS[ct] || ICONS.unknown; }
function typeLabel(ct) { return (TYPE_META[ct] || TYPE_META.unknown).label; }
function mediaTypeFor(ct) { return ct === 'tv_show' ? 'tv' : 'movie'; }

/* ── Facet labels + quick filters ──────────────────────────────────────── */
const TMDB_GENRES = {
  'tmdb:12': 'Adventure',
  'tmdb:14': 'Fantasy',
  'tmdb:16': 'Animation',
  'tmdb:18': 'Drama',
  'tmdb:27': 'Horror',
  'tmdb:28': 'Action',
  'tmdb:35': 'Comedy',
  'tmdb:36': 'History',
  'tmdb:37': 'Western',
  'tmdb:53': 'Thriller',
  'tmdb:80': 'Crime',
  'tmdb:99': 'Documentary',
  'tmdb:878': 'Science Fiction',
  'tmdb:9648': 'Mystery',
  'tmdb:10402': 'Music',
  'tmdb:10749': 'Romance',
  'tmdb:10751': 'Family',
  'tmdb:10752': 'War',
  'tmdb:10759': 'Action & Adventure',
  'tmdb:10762': 'Kids',
  'tmdb:10763': 'News',
  'tmdb:10764': 'Reality',
  'tmdb:10765': 'Sci-Fi & Fantasy',
  'tmdb:10766': 'Soap',
  'tmdb:10767': 'Talk',
  'tmdb:10768': 'War & Politics',
  'tmdb:10770': 'TV Movie',
};
const DECADE_PRESETS = [
  { label: '2020s', min: 2020, max: 2029 },
  { label: '2010s', min: 2010, max: 2019 },
  { label: '2000s', min: 2000, max: 2009 },
  { label: '1990s', min: 1990, max: 1999 },
  { label: '1980s', min: 1980, max: 1989 },
  { label: '1970s', min: 1970, max: 1979 },
  { label: 'Classics', min: 1900, max: 1969 },
];
const FEATURE_FILTERS = [
  { key: 'anime', label: 'Anime', anime: true },
  { key: 'hdr', label: 'HDR', rx: /hdr10\+?|\bhdr\b/i },
  { key: 'dv', label: 'Dolby Vision', rx: /dolby.?vision|\bdovi\b|\bdv\b/i },
  { key: 'atmos', label: 'Atmos', rx: /\batmos\b|truehd/i },
  { key: 'remux', label: 'REMUX', rx: /\bremux\b/i },
  { key: 'imax', label: 'IMAX', rx: /\bimax\b/i },
];
const RAW_RELEASE_LIMIT = 500;
function genreLabel(value) {
  const s = String(value || '');
  return TMDB_GENRES[s] || s.replace(/^tmdb:/, 'Genre ');
}

/* ── Release name parsing (edition / quality / season / episode) ─────────── */
function _rname(t) { return t.torrentName || t.name || ''; }
// Grouping-key title normalization lives in the shared static/js/norm-title.js
// module (loaded before this script) as normGroupTitle(), so app.js and
// library.js can't drift.
function displayTitle(name) {
  return String(name || '')
    .replace(/\bS\d{1,2}E\d{1,3}.*/i, '').replace(/\bS\d{1,2}(?!\d)(?!E).*/i, '')
    .replace(/\bSeason\s+\d+.*/i, '')
    // For unmatched (raw torrent-name) titles, drop the technical tail so the
    // card reads as a title, not a release string. Matched TMDB titles lack
    // these markers, so they're unaffected.
    .replace(/\b(480|576|720|1080|2160)[pi]\b.*/i, '')
    .replace(/\b(BluRay|BDRip|WEB[-.]?DL|WEBRip|HDTV|DVDRip|REMUX|HDR10?\+?|HEVC|x26[45])\b.*/i, '')
    .replace(/\s+/g, ' ').trim() || name;
}
// Torrent kind classification (complete | season | episode) lives in the shared
// static/js/torrent-kind.js module (loaded before this script) as
// parseTorrentKind(), so app.js and library.js can't drift.
const EDITIONS = [
  [/final.?cut/i, 'Final Cut'], [/director'?s?.?cut/i, 'Director’s Cut'],
  [/extended(?:.?(?:edition|cut|version))?/i, 'Extended'], [/uncut/i, 'Uncut'],
  [/unrated/i, 'Unrated'], [/theatrical/i, 'Theatrical'], [/\bimax\b/i, 'IMAX'],
  [/criterion/i, 'Criterion'], [/remaster(?:ed)?/i, 'Remastered'],
  [/anniversary/i, 'Anniversary'], [/ultimate(?:.?edition)?/i, 'Ultimate Edition'],
  [/special.?edition/i, 'Special Edition'],
];
function editionOf(name) {
  for (const [rx, label] of EDITIONS) if (rx.test(name || '')) return label;
  return 'Standard';
}
const RES_RANK = { '2160p': 4, '1080p': 3, '720p': 2, '576p': 1, '480p': 1 };
function resOf(t) {
  const low = _rname(t).toLowerCase();
  const m = low.match(/\b(2160|1080|720|576|480)[pi]\b/);
  if (m) return m[1] + 'p';
  if (/\b(4k|uhd)\b/.test(low)) return '2160p';
  if (t.videoResolution) return String(t.videoResolution).replace(/^V?/, '').toLowerCase();
  return '';
}
function srcOf(t) {
  const low = _rname(t).toLowerCase();
  const m = low.match(/\b(blu-?ray|remux|web-?dl|webrip|hdtv|bdrip|dvdrip|hdrip|cam)\b/);
  if (m) {
    const s = m[1].replace(/-/g, '');
    return { bluray: 'BluRay', webdl: 'WEB-DL', webrip: 'WEBRip', remux: 'REMUX' }[s] || s.toUpperCase();
  }
  return t.videoSource ? String(t.videoSource) : '';
}
function flagsOf(t) {
  const low = _rname(t).toLowerCase();
  const f = [];
  if (resOf(t) === '2160p') f.push('4K');
  if (/hdr10\+?|\bhdr\b|dolby.?vision|\bdovi\b|\bdv\b/.test(low)) f.push('HDR');
  if (/dolby.?vision|\bdovi\b|\bdv\b/.test(low)) f.push('DV');
  if (/\batmos\b|truehd|dts-?hd|dts-?x/.test(low)) f.push('ATMOS');
  if (/\bremux\b/.test(low)) f.push('REMUX');
  if (/\bimax\b/.test(low)) f.push('IMAX');
  return f;
}
function relSort(a, b) {
  const ra = RES_RANK[resOf(a)] || 0, rb = RES_RANK[resOf(b)] || 0;
  if (rb !== ra) return rb - ra;
  return (b.seeders || 0) - (a.seeders || 0);
}

/* ── Posters ────────────────────────────────────────────────────────────────
 * Posters are plain <img loading="lazy"> pointing at a same-origin endpoint that
 * resolves the id and 302-redirects to the real cover. The browser handles lazy
 * loading natively — no IntersectionObserver / fetch / new Image() hydration,
 * which used to silently leave the landing grid blank when a burst of cards
 * rendered at once. `onerror` drops the <img> so the placeholder shows through. */
function posterImgSrc(id, source, ct) {
  return `/api/poster/${encodeURIComponent(id)}?source=${source}&media_type=${mediaTypeFor(ct)}&redirect=1`;
}

/* ── Browse state ───────────────────────────────────────────────────────── */
const state = {
  q: '', type: '', sort: 'seeders',
  genres: new Set(), qualities: new Set(), sources: new Set(), features: new Set(),
  yearMin: '', yearMax: '',
  facets: {}, facetsKey: '',
  // Matched-only by default so the grid always shows real posters; the user can
  // opt into unmatched titles via the "Include unmatched" toggle.
  hideUnmatched: true, hideForeign: false,
  groups: [], page: 0, pageSize: 60,
  totalCount: 0, truncated: false, lastQuery: '',
  rawWindow: null,
  // Server-side grouped pagination (core groupByContent, v1.29.0): the grid
  // holds ONE page of title representatives; Next is driven by hasNext from
  // the server, never derived from totalCount (which is usually an estimate).
  hasNext: false, totalIsEstimate: false,
};
const groupCache = new Map();     // groupKey -> group object
let _navSeq = 0;

/* ── URL browse-state + title permalinks ────────────────────────────────────
   The meaningful browse state (q,type,genres,qualities,sources,features,years,
   sort,English-only,Include-unmatched,page) is serialized to the query string
   so a filtered view is shareable and a refresh is non-destructive. Filter tweaks replaceState; explicit search
   submits pushState. Each overlay (detail/drawer/account) pushes its own history
   entry so Back closes exactly one. A title carries a stable permalink
   (?title=&ct=&src=&cid=) resolved on boot via /api/titles/releases. */
function browseParamsFor(browseState) {
  const p = new URLSearchParams();
  if (browseState.q) p.set('q', browseState.q);
  if (browseState.type) p.set('type', browseState.type);
  if (browseState.sort && browseState.sort !== 'seeders') p.set('sort', browseState.sort);
  if (browseState.genres.size) p.set('genres', [...browseState.genres].join(','));
  if (browseState.qualities.size) p.set('qualities', [...browseState.qualities].join(','));
  if (browseState.sources.size) p.set('sources', [...browseState.sources].join(','));
  if (browseState.features.size) p.set('features', [...browseState.features].join(','));
  if (browseState.yearMin || browseState.yearMax) p.set('years', `${browseState.yearMin}-${browseState.yearMax}`);
  if (browseState.hideForeign) p.set('english', '1');
  // Keep the clean default URL, but make the release-visibility choice
  // explicit whenever it differs from matched-only.  `unmatched=0` is needed
  // only for the uncommon "Anime + matched only" combination: older Anime
  // links omitted the flag and intentionally defaulted to include unmatched.
  if (!browseState.hideUnmatched) p.set('unmatched', '1');
  else if (browseState.features.has('anime')) p.set('unmatched', '0');
  if (browseState.page > 0) p.set('page', browseState.page + 1);  // 1-based in the URL
  return p;
}
function browseParams() { return browseParamsFor(state); }
function browseUrl() {
  const qs = browseParams().toString();
  return qs ? `/library?${qs}` : '/library';
}
function syncUrl(mode) {
  const st = { lib: 'browse' };
  if (mode === 'push') history.pushState(st, '', browseUrl());
  else history.replaceState(st, '', browseUrl());
}
function historyModeFor(intent) {
  return intent === 'page' || intent === 'search' ? 'push' : 'replace';
}
function _setInputValue(id, value) {
  const el = document.getElementById(id);
  if (el) el.value = value;
}
function parseBrowseState(search) {
  const p = new URLSearchParams(search || '');
  const toSet = name => new Set((p.get(name) || '').split(',').map(s => s.trim()).filter(Boolean));
  const features = toSet('features');
  const years = p.get('years') || '';
  let yearMin = '', yearMax = '';
  if (years.includes('-')) {
    const [lo, hi] = years.split('-');
    yearMin = cleanYearInput(lo);
    yearMax = cleanYearInput(hi);
  }
  const hasUnmatchedChoice = p.has('unmatched');
  let hideUnmatched = hasUnmatchedChoice ? p.get('unmatched') !== '1' : true;
  let preAnimeHideUnmatched = null;
  if (features.has('anime')) {
    // Backwards compatibility for v1.28/v1.29 Anime links, which predate the
    // serialized release toggle and implicitly included unmatched releases.
    if (!hasUnmatchedChoice) hideUnmatched = false;
    preAnimeHideUnmatched = true;
  }
  return {
    q: p.get('q') || '',
    type: p.get('type') || '',
    sort: p.get('sort') || 'seeders',
    genres: toSet('genres'),
    qualities: toSet('qualities'),
    sources: toSet('sources'),
    features,
    yearMin,
    yearMax,
    hideUnmatched,
    hideForeign: p.get('english') === '1',
    _preAnimeHideUnmatched: preAnimeHideUnmatched,
    page: Math.max(0, (parseInt(p.get('page'), 10) || 1) - 1),
  };
}
function applyStateFromUrl() {
  Object.assign(state, parseBrowseState(location.search));
  // Push hydrated values into the controls loadLibrary reads back.
  _setInputValue('libSearch', state.q);
  _setInputValue('libYearMin', state.yearMin);
  _setInputValue('libYearMax', state.yearMax);
  _setInputValue('libSort', state.sort);
}
function _readPermalink() {
  const p = new URLSearchParams(location.search);
  const title = p.get('title');
  if (!title) return null;
  return { title, ct: p.get('ct') || 'unknown', src: p.get('src') || '', cid: p.get('cid') || '' };
}
function _permalinkKey(pl) {
  return (pl.src === 'tmdb' && pl.cid)
    ? `${pl.ct}:id:${pl.cid}`
    : `${pl.ct}:name:${normGroupTitle(pl.title)}`;
}
// Detail URL = current browse state + the title permalink params (so Back from a
// detail lands on the same filtered browse view).
function _detailUrl(g) {
  const p = browseParams();
  p.set('title', detail.title || displayTitle(g.best.name));
  p.set('ct', g.contentType || 'unknown');
  if (g.contentSource) p.set('src', g.contentSource);
  if (g.contentId) p.set('cid', g.contentId);
  return `/library?${p}`;
}
// Canonical shareable link — title only, no browse filters — for "Copy link".
function _canonicalDetailUrl(g) {
  const p = new URLSearchParams();
  p.set('title', detail.title || displayTitle(g.best.name));
  p.set('ct', g.contentType || 'unknown');
  if (g.contentSource) p.set('src', g.contentSource);
  if (g.contentId) p.set('cid', g.contentId);
  return `${location.origin}/library?${p}`;
}
async function resolvePermalink(pl) {
  const key = _permalinkKey(pl);
  // Minimal placeholder group; openDetail re-fetches the full release set and
  // TMDB metadata via /api/titles/releases + /api/meta.
  const best = { name: pl.title, title: pl.title, contentType: pl.ct,
    contentSource: pl.src || null, contentId: pl.cid || null, seeders: 0, isMapped: !!pl.src };
  const g = { key, contentType: pl.ct, contentId: pl.cid || null,
    contentSource: pl.src || null, isMapped: !!pl.src, best, items: [best] };
  groupCache.set(key, g);
  await openDetail(key);
}
function copyDetailLink() {
  if (!detail.group) return;
  const url = _canonicalDetailUrl(detail.group);
  const done = () => toast('Link copied');
  if (navigator.clipboard && navigator.clipboard.writeText) {
    navigator.clipboard.writeText(url).then(done).catch(() => fallbackCopy(url, done));
  } else fallbackCopy(url, done);
}

function groupTorrents(items) {
  const map = new Map();
  for (const t of items) {
    const ct = t.contentType || 'unknown';
    const key = (t.contentSource === 'tmdb' && t.contentId)
      ? `${ct}:id:${t.contentId}`
      : `${ct}:name:${normGroupTitle(t.name)}`;
    if (!map.has(key)) {
      map.set(key, { key, contentType: ct, contentId: t.contentId,
        contentSource: t.contentSource, isMapped: t.isMapped, best: t, items: [] });
    }
    const g = map.get(key);
    g.items.push(t);
    if ((t.seeders || 0) > (g.best.seeders || 0)) g.best = t;
  }
  return [...map.values()];
}

/* ── Filters UI ─────────────────────────────────────────────────────────── */
function renderTypePills() {
  const c = document.getElementById('libTypeFilters');
  if (!c) return;
  c.innerHTML = TYPE_ORDER.map(type => {
    const label = type ? typeLabel(type) : 'All';
    const icon = type ? `<span aria-hidden="true">${iconFor(type)}</span>` : '';
    return `<button class="lib-typepill ${state.type === type ? 'active' : ''}" onclick="setType('${type}')">${icon}${escHtml(label)}</button>`;
  }).join('');
}
function setType(type) { state.type = type; state.page = 0; renderTypePills(); loadLibrary(); }
function sortTransitionFor(_browseState, value) {
  return {
    sort: value,
    page: 0,
    // Grouped mode needs new global page membership. Raw/anime modes need a
    // fresh bounded window selected in the requested backend order.
    mode: 'refetch',
  };
}
function setLibSort(v) {
  const transition = sortTransitionFor(state, v);
  state.sort = transition.sort;
  state.page = transition.page;
  const el = document.getElementById('libSort');
  if (el) el.value = v;
  // loadLibrary keeps the reset page and refetches either grouped membership
  // or a raw/anime window ordered by the newly selected sort.
  return loadLibrary({ keepPage: true });
}
function toggleFacet(kind, value) {
  const bucket = state[kind];
  if (!(bucket instanceof Set)) return;
  if (bucket.has(value)) bucket.delete(value);
  else bucket.add(value);
  state.page = 0;
  renderFacetControls();
  loadLibrary();
}
function setFacet(which, value) {
  // Backwards-compatible hook for older rendered markup; new controls use chips.
  const map = { genre: 'genres', quality: 'qualities', source: 'sources' };
  const kind = map[which];
  if (kind && value) toggleFacet(kind, value);
}
function clearFacets() {
  state.type = '';
  state.genres.clear(); state.qualities.clear(); state.sources.clear(); state.features.clear();
  state.yearMin = ''; state.yearMax = '';
  state.hideForeign = false; state.hideUnmatched = true; state._preAnimeHideUnmatched = null;
  state.page = 0;
  ['libYearMin', 'libYearMax'].forEach(id => {
    const el = document.getElementById(id);
    if (el) el.value = '';
  });
  renderTypePills();
  renderFacetControls();
  syncControls();
  loadLibrary();
}
function toggleLibFilter(which) {
  if (which === 'unmatched') { state.hideUnmatched = !state.hideUnmatched; state._preAnimeHideUnmatched = null; }
  if (which === 'foreign') state.hideForeign = !state.hideForeign;
  state.page = 0;
  syncControls();
  loadLibrary();
}
function hasAdvancedFiltersFor(browseState) {
  return !!(
    browseState.genres.size || browseState.qualities.size || browseState.sources.size || browseState.features.size ||
    parsedYear(browseState.yearMin) != null || parsedYear(browseState.yearMax) != null
  );
}
function hasAdvancedFilters() { return hasAdvancedFiltersFor(state); }
function isHomeFor(browseState) {
  return !browseState.q && browseState.type === '' && !hasAdvancedFiltersFor(browseState) &&
    browseState.sort === 'seeders' && !browseState.hideForeign && browseState.hideUnmatched;
}
// Every visible non-default browse control leaves the curated landing page.
// Home shelves have fixed ordering and matched-only semantics, so showing them
// under an active Sort / English / Include-unmatched control would be deceptive.
function isHome() { return isHomeFor(state); }
function syncControls() {
  const set = (id, on) => {
    const el = document.getElementById(id);
    if (el) { el.classList.toggle('active', on); el.setAttribute('aria-pressed', String(on)); }
  };
  // "Include unmatched" is the inverse of the matched-only default.
  set('fltShowUnmatched', !state.hideUnmatched);
  set('fltHideForeign', state.hideForeign);
  const clear = document.getElementById('libSearchClear');
  if (clear) clear.style.display = document.getElementById('libSearch').value ? '' : 'none';
  const sort = document.getElementById('libSort');
  if (sort) sort.value = state.sort;
}

function facetList(name) {
  const arr = (state.facets && state.facets[name]) || [];
  return Array.isArray(arr) ? arr.filter(x => x && x.value != null) : [];
}
function fmtFacetResolution(v) {
  const s = String(v || '').replace(/^V/, '').toLowerCase();
  return s === '2160p' ? '2160p / 4K' : s;
}
function qualityRank(value) {
  return RES_RANK[String(value || '').replace(/^V/, '').toLowerCase()] || 0;
}
function renderFacetChipGroup(id, items, activeSet, labelFn, sortFn, kind) {
  const el = document.getElementById(id);
  if (!el) return;
  const rows = items.slice().sort(sortFn);
  activeSet.forEach(v => {
    if (!rows.some(x => String(x.value) === String(v))) rows.unshift({ value: v, count: 0 });
  });
  el.innerHTML = rows.map(x => {
    const value = String(x.value);
    const label = labelFn(value);
    const active = activeSet.has(value);
    const count = x.count ? `<span class="count">${fmtNum(x.count)}</span>` : '';
    return `<button class="lib-filter-chip ${active ? 'active' : ''}" aria-pressed="${active}"
              data-filter-kind="${escAttr(kind)}" data-filter-value="${escAttr(value)}"
              onclick="toggleFacet('${kind}','${escAttr(jsStringArg(value))}')">
        <span>${escHtml(label)}</span>${count}
      </button>`;
  }).join('');
  if (!rows.length) el.innerHTML = '<span class="lib-filter-empty">No options yet</span>';
}
function renderDecadeChips() {
  const el = document.getElementById('libDecadeChips');
  if (!el) return;
  el.innerHTML = DECADE_PRESETS.map(d => {
    const active = String(d.min) === state.yearMin && String(d.max) === state.yearMax;
    return `<button class="lib-decade-chip ${active ? 'active' : ''}" aria-pressed="${active}"
              onclick="setDecade(${d.min},${d.max})">${escHtml(d.label)}</button>`;
  }).join('');
}
function renderFeatureChips() {
  const el = document.getElementById('libFeatureChips');
  if (!el) return;
  el.innerHTML = FEATURE_FILTERS.map(f => {
    const active = state.features.has(f.key);
    return `<button class="lib-feature-toggle ${active ? 'active' : ''}" aria-pressed="${active}"
              data-feature="${escAttr(f.key)}"
              onclick="toggleFeature('${f.key}')">
        <span class="knob"></span><span>${escHtml(f.label)}</span>
      </button>`;
  }).join('');
}
function renderFacetControls() {
  renderDecadeChips();
  renderFeatureChips();
  const min = document.getElementById('libYearMin');
  const max = document.getElementById('libYearMax');
  if (min && min.value !== state.yearMin) min.value = state.yearMin;
  if (max && max.value !== state.yearMax) max.value = state.yearMax;

  renderFacetChipGroup('libGenreChips', facetList('genre'), state.genres, genreLabel,
    (a, b) => (b.count || 0) - (a.count || 0) || genreLabel(a.value).localeCompare(genreLabel(b.value)), 'genres');
  renderFacetChipGroup('libQualityChips', facetList('videoResolution'), state.qualities, fmtFacetResolution,
    (a, b) => qualityRank(b.value) - qualityRank(a.value) || String(a.value).localeCompare(String(b.value)), 'qualities');
  renderFacetChipGroup('libSourceChips', facetList('videoSource'), state.sources, v => v,
    (a, b) => (b.count || 0) - (a.count || 0) || String(a.value).localeCompare(String(b.value)), 'sources');
}
function setDecade(min, max) {
  const active = state.yearMin === String(min) && state.yearMax === String(max);
  state.yearMin = active ? '' : String(min);
  state.yearMax = active ? '' : String(max);
  state.page = 0;
  renderFacetControls();
  loadLibrary();
}
function cleanYearInput(value) {
  const clean = String(value || '').replace(/[^\d]/g, '').slice(0, 4);
  return clean.length === 4 ? String(Math.min(3000, Math.max(1800, parseInt(clean, 10)))) : clean;
}
function setYearBound(which, value, soft) {
  const clean = cleanYearInput(value);
  if (which === 'min') state.yearMin = clean;
  if (which === 'max') state.yearMax = clean;
  state.page = 0;
  renderDecadeChips();
  if (soft) debouncedLoad();
  else loadLibrary();
}
function toggleFeature(key) {
  if (!FEATURE_FILTERS.some(f => f.key === key)) return;
  if (state.features.has(key)) state.features.delete(key);
  else state.features.add(key);
  if (key === 'anime') {
    if (state.features.has('anime')) {
      // Anime is largely release-labelled but not TMDB-mapped, so relax
      // matched-only — remembering the prior choice to restore on toggle-off.
      state._preAnimeHideUnmatched = state.hideUnmatched;
      state.hideUnmatched = false;
    } else if (state._preAnimeHideUnmatched != null) {
      state.hideUnmatched = state._preAnimeHideUnmatched;
      state._preAnimeHideUnmatched = null;
    }
  }
  state.page = 0;
  renderFeatureChips();
  syncControls();
  loadLibrary();
}
function selectedCsv(set) { return [...set].join(','); }
function genreCsvForRequest(browseState = state) {
  return selectedCsv(browseState.genres);
}
function typeCsvForRequest(browseState = state) {
  if (browseState.type) return browseState.type;
  return browseState.features.has('anime') ? 'movie,tv_show' : '';
}
function parsedYear(value) { return /^\d{4}$/.test(value || '') ? parseInt(value, 10) : null; }
function yearValues(browseState = state) {
  const min = parsedYear(browseState.yearMin);
  const max = parsedYear(browseState.yearMax);
  if (min == null && max == null) return '';
  const current = new Date().getFullYear() + 1;
  let lo = min == null ? 1800 : min;
  let hi = max == null ? current : max;
  if (lo > hi) [lo, hi] = [hi, lo];
  lo = Math.max(1800, lo);
  hi = Math.min(3000, hi);
  return Array.from({ length: hi - lo + 1 }, (_, i) => String(lo + i)).join(',');
}
function matchesAnime(t) {
  const text = [_rname(t), t.name, t.title, t.originalTitle, t.releaseGroup].filter(Boolean).join(' ');
  const low = text.toLowerCase();
  if (String(t.originalLanguage || '').toLowerCase() === 'ja') return true;
  if (/[ぁ-ゟ゠-ヿ一-龯]/.test(text)) return true;
  return /anime[ ._-]?time|subsplease|erai[ ._-]?raws|horriblesubs|yameii|kawaiika|vcb[ ._-]?studio|beatrice[ ._-]?raws|anime[ ._-]?rg|judas|ember|lostyears|tsundere|asw|cleo|bonkai77|crunchyroll|\bcr[ ._-]?web[ ._-]?dl\b|hidive|funimation/i.test(low);
}
function matchesFeature(t, key) {
  const f = FEATURE_FILTERS.find(x => x.key === key);
  if (!f) return true;
  if (f.anime) return matchesAnime(t);
  return f.rx.test(_rname(t));
}
function applyFeatureFiltersFor(items, features, backendAnime) {
  if (!features.size) return items;
  return items.filter(t => [...features].every(key =>
    (key === 'anime' && backendAnime) || matchesFeature(t, key)
  ));
}
function applyFeatureFilters(items, backendAnime) {
  return applyFeatureFiltersFor(items, state.features, backendAnime);
}

/* ── Search ─────────────────────────────────────────────────────────────── */
const debouncedLoad = debounce(() => loadLibrary({ debounced: true }), 280);
function libSearchInput() { syncControls(); debouncedLoad(); }
function clearLibSearch() { const i = document.getElementById('libSearch'); i.value = ''; i.focus(); loadLibrary({ push: true }); }
function libSearchKeydown(e) {
  if (e.key === 'Escape') { if (e.target.value) { e.stopPropagation(); clearLibSearch(); } else e.target.blur(); }
  if (e.key === 'Enter') loadLibrary({ push: true });
}

/* ── Load library ───────────────────────────────────────────────────────── */
function showHome(on) {
  document.getElementById('libHome').style.display = on ? '' : 'none';
  document.getElementById('libResults').style.display = on ? 'none' : '';
}

let _browseAbortController = null;
let _gridLoading = false;
function setGridLoading(on) {
  _gridLoading = on;
  if (typeof document === 'undefined') return;
  const spinner = document.getElementById('libSearchSpinner');
  if (spinner) spinner.style.display = on ? '' : 'none';
  if (on) {
    ['libPrev', 'libNext'].forEach(id => {
      const el = document.getElementById(id);
      if (el) el.disabled = true;
    });
  }
}
function beginBrowseNavigation() {
  if (_browseAbortController) _browseAbortController.abort();
  _browseAbortController = typeof AbortController !== 'undefined' ? new AbortController() : null;
  // A grid request may be superseded by Home, which has no grid-finally path.
  // Clear its spinner immediately rather than leaving a stale busy indicator.
  setGridLoading(false);
  return {
    seq: ++_navSeq,
    signal: _browseAbortController ? _browseAbortController.signal : undefined,
  };
}

async function loadLibrary(opts) {
  // Guard against being wired directly as a DOM event handler (onclick=…).
  opts = (opts && typeof opts === 'object' && !(opts instanceof Event)) ? opts : {};
  if (!opts.debounced) debouncedLoad.cancel();
  state.q = document.getElementById('libSearch').value.trim();
  // Any new browse state starts at page 1. popstate restores the URL's page
  // (applyStateFromUrl already set it); libPage() drives paging itself.
  if (!opts.fromPop && !opts.keepPage) state.page = 0;
  const nav = beginBrowseNavigation();
  // Filter tweaks replace; explicit search submits push; popstate reloads never
  // touch history (the URL is already what we're reacting to). Never write while
  // an overlay is on top — its entry owns the current history slot, so a late
  // debounced reload must not clobber it back to the browse URL.
  if (!opts.fromPop && !_modalStack.length) {
    syncUrl(historyModeFor(opts.push ? 'search' : 'filter'));
  }
  renderTypePills(); renderFacetControls(); syncControls();
  if (isHome()) {
    showHome(true);
    if (state.facetsKey !== facetKeyFor(state)) loadFacetOptions(nav.seq, nav.signal);
    return loadHome(nav.seq, nav.signal);
  }
  showHome(false);
  return loadGrid(nav.seq, nav.signal);
}

function backendAnimeModeFor(browseState) {
  return browseState.features.size === 1 && browseState.features.has('anime');
}
function addRequestFilters(params, browseState, backendAnime) {
  const selectedGenres = genreCsvForRequest(browseState);
  const selectedQualities = selectedCsv(browseState.qualities);
  const selectedSources = selectedCsv(browseState.sources);
  const selectedYears = yearValues(browseState);
  if (selectedGenres) params.set('genres', selectedGenres);
  if (selectedYears) params.set('years', selectedYears);
  if (selectedQualities) params.set('qualities', selectedQualities);
  if (selectedSources) params.set('sources', selectedSources);
  if (backendAnime) params.set('anime', 'true');
  return params;
}

// Core grouping selects one representative release per title. It is correct
// only when every active predicate can be applied before that selection.
// English-only, Include-unmatched and technical feature chips are release-level
// (or require unmatched rows), so they deliberately use the raw-release path
// and group only after filtering. Anime alone retains its high-recall backend
// representative mode; Anime + technical features must use truly raw rows.
function serverGroupedFor(browseState) {
  return browseState.hideUnmatched && !browseState.hideForeign && browseState.features.size === 0;
}
function serverGrouped() { return serverGroupedFor(state); }

function gridRequestFor(browseState) {
  const grouped = serverGroupedFor(browseState);
  const backendAnime = !grouped && backendAnimeModeFor(browseState);
  const mode = grouped ? 'grouped' : backendAnime ? 'anime' : 'raw';
  const params = grouped
    ? new URLSearchParams({
        q: browseState.q, content_types: typeCsvForRequest(browseState),
        limit: browseState.pageSize, offset: browseState.page * browseState.pageSize,
        group: 'true', order: browseState.sort,
        hide_unmapped: browseState.hideUnmatched, hide_non_english: browseState.hideForeign,
        aggregate_facets: browseState.page === 0 ? 'true' : 'false',
      })
    : new URLSearchParams({
        q: browseState.q, content_types: typeCsvForRequest(browseState), limit: 500, offset: 0,
        order: browseState.sort,
        hide_unmapped: browseState.hideUnmatched, hide_non_english: browseState.hideForeign,
        aggregate_facets: 'true',
      });
  addRequestFilters(params, browseState, backendAnime);
  return { mode, grouped, backendAnime, params };
}

function facetRequestFor(browseState) {
  const backendAnime = backendAnimeModeFor(browseState);
  const params = new URLSearchParams({
    q: browseState.q, content_types: typeCsvForRequest(browseState), limit: 1, offset: 0,
    hide_unmapped: browseState.hideUnmatched, hide_non_english: browseState.hideForeign,
    aggregate_facets: 'true',
  });
  return addRequestFilters(params, browseState, backendAnime);
}
function facetKeyFor(browseState) {
  const params = facetRequestFor(browseState);
  ['limit', 'offset', 'aggregate_facets'].forEach(key => params.delete(key));
  params.sort();
  return params.toString();
}
function needsFacetFetchFor(browseState) {
  return serverGroupedFor(browseState) && browseState.page > 0 &&
    browseState.facetsKey !== facetKeyFor(browseState);
}

function rawWindowMetaFor(releaseCount, totalCount, limit = RAW_RELEASE_LIMIT) {
  const parsedTotal = totalCount == null || totalCount === '' ? NaN : Number(totalCount);
  const totalReleases = Number.isFinite(parsedTotal) && parsedTotal >= 0 ? parsedTotal : null;
  return {
    releaseCount,
    totalReleases,
    limit,
    // The raw endpoint can post-filter a capped upstream scan, so an unknown
    // total is bounded even when fewer than `limit` rows survive that filter.
    limited: totalReleases == null || totalReleases > releaseCount,
  };
}
function rawResultNoteFor(windowMeta, titleCount, matchingReleaseCount, sort, hasFeatureFilter) {
  const titleLabel = `${fmtNum(titleCount)} title${titleCount === 1 ? '' : 's'}`;
  const releaseLabel = `${fmtNum(matchingReleaseCount)}${hasFeatureFilter ? ' matching' : ''} release${matchingReleaseCount === 1 ? '' : 's'}`;
  const base = `${titleLabel} · ${releaseLabel}`;
  if (!windowMeta || !windowMeta.limited) return base;
  const orderLabels = { seeders: 'most-seeded', newest: 'newest', name: 'A–Z', size: 'largest' };
  const order = orderLabels[sort] || 'selected';
  const windowLabel = windowMeta.totalReleases == null
    ? `a bounded ${fmtNum(windowMeta.releaseCount)}-release window`
    : `${fmtNum(windowMeta.releaseCount)} of ${fmtNum(windowMeta.totalReleases)} releases`;
  return `${base}. Based on ${windowLabel} ordered by ${order}; narrow filters to search beyond this window.`;
}

async function loadFacetOptions(seq, signal) {
  const key = facetKeyFor(state);
  const data = await api(`/api/torrents?${facetRequestFor(state)}`, signal ? { signal } : {});
  if (seq != null && seq !== _navSeq) return;
  if (data && data.aggregations) {
    state.facets = data.aggregations;
    state.facetsKey = key;
    renderFacetControls();
  }
}

async function loadGrid(navSeq, signal) {
  let seq = navSeq;
  if (seq == null) {
    const nav = beginBrowseNavigation();
    seq = nav.seq;
    signal = nav.signal;
  }
  const { mode, grouped, backendAnime, params } = gridRequestFor(state);
  setGridLoading(true);
  const grid = document.getElementById('libraryGrid');
  if (grid && !state.groups.length) {
    grid.innerHTML = Array.from({ length: 12 }, () => '<div class="lib-skeleton"></div>').join('');
  }

  // A cold deep link to page >1 has no page-0 aggregations in memory. Fetch a
  // tiny raw aggregation request in parallel while keeping the grouped page
  // itself on aggregate_facets=false (the v1.29 performance path).
  if (needsFacetFetchFor(state)) loadFacetOptions(seq, signal);
  let data;
  try {
    data = await api(`/api/torrents?${params}`, signal ? { signal } : {});
  } finally {
    if (seq === _navSeq) setGridLoading(false);
  }
  if (seq !== _navSeq) return;

  if (!data) {
    grid.innerHTML = `<div class="lib-empty"><svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.5"><circle cx="12" cy="12" r="10"/><line x1="12" y1="8" x2="12" y2="12"/><line x1="12" y1="16" x2="12.01" y2="16"/></svg><h3>Couldn’t load the library</h3><p>The request failed — your session may have expired. Reload the page and sign in again if it persists.</p></div>`;
    document.getElementById('libPaginationInfo').textContent = 'Failed to load';
    return;
  }

  const rawItems = data.items || [];
  const items = applyFeatureFilters(rawItems, backendAnime);
  if (data.aggregations && typeof data.aggregations === 'object') {
    state.facets = data.aggregations;   // grouped pages >0 retain/fetch their page-0 facet set
    state.facetsKey = facetKeyFor(state);
    renderFacetControls();
  }
  state.totalCount = data.totalCount > 0 ? data.totalCount : 0;
  state.lastQuery = state.q;
  // Map insertion order preserves the first matching release for each title.
  // Keep the backend's response order in every mode: raw Newest is ordered by
  // published_at, which cannot be reconstructed from discoveredAt, and any
  // second client sort could reorder the backend-selected bounded window.
  state.groups = groupTorrents(items);
  if (grouped) {
    state.rawWindow = null;
    state.hasNext = !!data.hasNextPage;
    state.totalIsEstimate = !!data.totalCountIsEstimate;
    state.truncated = false;
  } else if (mode === 'anime') {
    state.rawWindow = null;
    state.truncated = state.totalCount > rawItems.length;
  } else {
    state.rawWindow = rawWindowMetaFor(rawItems.length, data.totalCount);
    state.truncated = state.rawWindow.limited;
  }
  if (!grouped) {
    const maxPage = Math.max(0, Math.ceil(state.groups.length / state.pageSize) - 1);
    const requestedPage = state.page;
    state.page = Math.min(requestedPage, maxPage);
    if (state.page !== requestedPage && !_modalStack.length) syncUrl('replace');
  }
  renderPage();

  const note = document.getElementById('libResultNote');
  const totalTitles = grouped && state.totalCount
    ? `${state.totalIsEstimate ? '~' : ''}${fmtNum(state.totalCount)}` : '';
  if (!items.length) {
    note.textContent = mode === 'raw' && state.rawWindow && state.rawWindow.limited
      ? rawResultNoteFor(state.rawWindow, 0, 0, state.sort, state.features.size > 0)
      : '';
  } else if (!isHome()) {
    if (grouped) {
      note.textContent = totalTitles ? `${totalTitles} titles` : `${fmtNum(state.groups.length)} titles on this page`;
    } else if (mode === 'anime') {
      const bounded = state.truncated
        ? ` · first ${fmtNum(rawItems.length)} of ${fmtNum(state.totalCount)} Anime title representatives`
        : '';
      note.textContent = `${fmtNum(state.groups.length)} Anime title${state.groups.length === 1 ? '' : 's'}${bounded}`;
    } else {
      note.textContent = rawResultNoteFor(
        state.rawWindow, state.groups.length, items.length, state.sort, state.features.size > 0
      );
    }
  } else note.textContent = `Browsing most-seeded titles${totalTitles ? ` - ${totalTitles} indexed` : ''}. Start typing to search everything.`;
}

function libPage(dir) {
  if (_gridLoading) return;
  if (serverGrouped()) {
    // Server paging: each page is a fresh fetch; bounds come from hasNext.
    if (dir > 0 && !state.hasNext) return;
    if (dir < 0 && state.page === 0) return;
    state.page += dir;
    if (!_modalStack.length) syncUrl(historyModeFor('page'));
    loadGrid();
  } else {
    const max = Math.max(0, Math.ceil(state.groups.length / state.pageSize) - 1);
    state.page = Math.min(max, Math.max(0, state.page + dir));
    if (!_modalStack.length) syncUrl(historyModeFor('page'));
    renderPage();
  }
  document.getElementById('libBrowse').scrollIntoView({ behavior: _scrollBehavior(), block: 'start' });
}

function renderPage() {
  const start = state.page * state.pageSize;
  if (serverGrouped()) {
    // state.groups IS the current page. totalCount is usually a planner
    // estimate — display it as "~N", and never derive page bounds from it.
    const n = state.groups.length;
    const totalTxt = state.totalCount > 0
      ? ` of ${state.totalIsEstimate ? '~' : ''}${fmtNum(state.totalCount)}` : '';
    document.getElementById('libPaginationInfo').textContent =
      n === 0 ? '' : `${start + 1}–${start + n}${totalTxt} titles`;
    document.getElementById('libPrev').disabled = state.page === 0;
    document.getElementById('libNext').disabled = !state.hasNext;
    document.getElementById('libPager').style.display = (state.page > 0 || state.hasNext) ? '' : 'none';
    renderGrid(state.groups);
    return;
  }
  const slice = state.groups.slice(start, start + state.pageSize);
  const total = state.groups.length;
  document.getElementById('libPaginationInfo').textContent =
    total === 0 ? '' : `${start + 1}–${Math.min(start + slice.length, total)} of ${fmtNum(total)} titles`;
  document.getElementById('libPrev').disabled = state.page === 0;
  document.getElementById('libNext').disabled = start + state.pageSize >= total;
  document.getElementById('libPager').style.display = total > state.pageSize ? '' : 'none';
  renderGrid(slice);
}

function renderGrid(groups) {
  const grid = document.getElementById('libraryGrid');
  if (!groups.length) {
    const msg = state.lastQuery
      ? `<h3>No titles match “${escHtml(state.lastQuery)}”</h3><p>Try fewer words or the original title — or relax the type / language filters.</p>`
      : `<h3>Nothing here yet</h3><p>Adjust your filters, or wait for the crawler to index more content.</p>`;
    grid.innerHTML = `<div class="lib-empty"><svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.5"><circle cx="11" cy="11" r="8"/><path d="M21 21l-4.35-4.35"/></svg>${msg}</div>`;
    return;
  }
  groupCache.clear();
  groups.forEach(g => groupCache.set(g.key, g));
  grid.innerHTML = groups.map(cardHtml).join('');
}

// Shared poster-card markup, used by both the flat grid and the home rows.
function cardHtml(g) {
  const t = g.best;
  const ct = g.contentType || 'unknown';
  const count = g.items.length;
  const posterId = (t.contentSource === 'tmdb' && t.contentId) ? t.contentId
    : (ct === 'music' && t.contentSource === 'musicbrainz' && t.contentId) ? t.contentId
    : (ct === 'music' && !t.contentId) ? (t.name || '') : '';
  const posterSource = ct === 'music' ? (t.contentSource === 'musicbrainz' ? 'mb' : 'lidarr') : 'tmdb';
  const title = displayTitle(t.name);
  const cardLabel = `${title}, ${typeLabel(ct)}${count > 1 ? `, ${count} releases` : ''}`;
  return `<div class="lib-card" aria-label="${escAttr(cardLabel)}" onclick="openDetail('${escAttr(jsStringArg(g.key))}')" role="button" tabindex="0"
      onkeydown="if(event.key==='Enter'||event.key===' '){event.preventDefault();openDetail('${escAttr(jsStringArg(g.key))}')}">
    <div class="lib-poster">
      <div class="lib-poster-ph" aria-hidden="true">
        ${iconFor(ct)}<span>${escHtml((ct || 'unknown').replace('_', ' '))}</span>
      </div>
      ${posterId ? `<img class="lib-poster-img" loading="lazy" decoding="async" alt="" src="${escAttr(posterImgSrc(posterId, posterSource, ct))}" onerror="this.remove()">` : ''}
      <span class="lib-type-badge">${iconFor(ct)}${escHtml(typeLabel(ct))}</span>
      ${count > 1 ? `<span class="lib-count-badge">${count}</span>` : ''}
      <span class="lib-seed-badge"><span class="dot"></span>${fmtNum(t.seeders || 0)}</span>
      ${!g.isMapped ? '<span class="lib-unmatched-badge">unmatched</span>' : ''}
    </div>
    <div class="lib-card-info">
      <div class="lib-card-title">${escHtml(title)}</div>
      <div class="lib-card-sub">${escHtml(typeLabel(ct))}${count > 1 ? ` · ${count} releases` : ''}</div>
    </div>
  </div>`;
}

/* ── Home: curated, sectioned landing page ──────────────────────────────── */
// Each section is a matched-only, poster-bearing row fetched with a server-side
// order. Rows are horizontally scrollable so the page stays a set of scannable
// shelves rather than one giant grid.
const HOME_SECTIONS = [
  { key: 'popular', title: 'Popular now',   sub: 'Most-seeded across the library',
    params: { content_types: 'movie,tv_show', order: 'seeders' } },
  { key: 'recent',  title: 'Recently added', sub: 'Freshly indexed by the crawler',
    params: { content_types: 'movie,tv_show', order: 'newest' }, seeAllType: null },
  { key: 'movies',  title: 'Movies',         sub: 'Popular films',
    params: { content_types: 'movie', order: 'seeders' }, seeAllType: 'movie' },
  { key: 'tv',      title: 'TV Shows',        sub: 'Popular series',
    params: { content_types: 'tv_show', order: 'seeders' }, seeAllType: 'tv_show' },
];
const HOME_ROW_LIMIT = 24;   // titles rendered per shelf

async function loadHome(navSeq, signal) {
  const host = document.getElementById('libHome');
  const seq = navSeq != null ? navSeq : ++_navSeq;
  // Skeleton shelves while the sections load.
  host.innerHTML = HOME_SECTIONS.map(s => `
    <section class="lib-home-section">
      <div class="lib-home-head"><h2>${escHtml(s.title)}</h2></div>
      <div class="lib-row-wrap"><div class="lib-row">${
        Array.from({ length: 8 }, () => '<div class="lib-skeleton lib-row-skel"></div>').join('')
      }</div></div>
    </section>`).join('');

  const results = await Promise.all(HOME_SECTIONS.map(async s => {
    const p = new URLSearchParams({ ...s.params, hide_unmapped: true, limit: 100, offset: 0 });
    const data = await api(`/api/torrents?${p}`, signal ? { signal } : {});
    // Server already applied the order; grouping preserves first-seen order.
    const groups = groupTorrents((data && data.items) || []).slice(0, HOME_ROW_LIMIT);
    return { section: s, groups };
  }));
  if (seq !== _navSeq) return;  // superseded (user searched/switched)

  // openDetail() reads groupCache — register every rendered group.
  groupCache.clear();
  results.forEach(r => r.groups.forEach(g => groupCache.set(g.key, g)));

  const nonEmpty = results.filter(r => r.groups.length);
  if (!nonEmpty.length) {
    host.innerHTML = `<div class="lib-empty"><svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.5"><circle cx="11" cy="11" r="8"/><path d="M21 21l-4.35-4.35"/></svg><h3>Nothing to show yet</h3><p>No matched titles are indexed yet — try a search, or check back once the crawler has enriched more content.</p></div>`;
    return;
  }
  host.innerHTML = nonEmpty.map(r => {
    const s = r.section;
    const seeAll = s.seeAllType
      ? `<button class="lib-home-more" onclick="setType('${s.seeAllType}')">See all <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2"><polyline points="9 18 15 12 9 6"/></svg></button>`
      : '';
    return `<section class="lib-home-section">
      <div class="lib-home-head">
        <div><h2>${escHtml(s.title)}</h2>${s.sub ? `<p>${escHtml(s.sub)}</p>` : ''}</div>
        ${seeAll}
      </div>
      <div class="lib-row-wrap">
        <button class="lib-row-arrow prev" aria-label="Scroll left" onclick="rowScroll(this,-1)"><svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2.5"><polyline points="15 18 9 12 15 6"/></svg></button>
        <div class="lib-row">${r.groups.map(cardHtml).join('')}</div>
        <button class="lib-row-arrow next" aria-label="Scroll right" onclick="rowScroll(this,1)"><svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2.5"><polyline points="9 18 15 12 9 6"/></svg></button>
      </div>
    </section>`;
  }).join('');
}

function rowScroll(btn, dir) {
  const row = btn.parentElement.querySelector('.lib-row');
  if (row) row.scrollBy({ left: dir * Math.round(row.clientWidth * 0.85), behavior: _scrollBehavior() });
}

/* ── Detail view ────────────────────────────────────────────────────────── */
const detail = { group: null, ct: '', title: '', tmdbId: null, meta: null,
  releases: [], activeSeason: null, quality: 'all', source: 'all', edition: 'all',
  seasonCache: new Map() };

function _star(rating) {
  return `<span class="lib-rating"><svg viewBox="0 0 24 24" fill="currentColor"><polygon points="12 2 15.09 8.26 22 9.27 17 14.14 18.18 21.02 12 17.77 5.82 21.02 7 14.14 2 9.27 8.91 8.26 12 2"/></svg>${(rating || 0).toFixed(1)}</span>`;
}

async function openDetail(key) {
  const g = groupCache.get(key);
  if (!g) return;
  // Cancel any pending search reload so it can't fire behind the modal (its
  // syncUrl is already guarded, but this also avoids a wasted grid refetch).
  debouncedLoad.cancel();
  const t = g.best;
  const ct = g.contentType || 'unknown';
  detail.group = g; detail.ct = ct; detail.title = displayTitle(t.name);
  detail.tmdbId = (t.contentSource === 'tmdb' && t.contentId) ? String(t.contentId) : null;
  detail.meta = null; detail.releases = g.items.slice();
  detail.activeSeason = null; detail.quality = 'all'; detail.source = 'all'; detail.edition = 'all'; detail.seasonCache = new Map();

  const view = document.getElementById('libDetail');
  view.classList.add('open');
  view.setAttribute('aria-hidden', 'false');
  openModal(view);
  // Push a title permalink over the browse entry — or replace if we're already
  // on a detail entry (switching titles, or hydrating a shared link on boot).
  const histState = { lib: 'detail', key };
  const url = _detailUrl(g);
  if (history.state && history.state.lib === 'detail') history.replaceState(histState, '', url);
  else history.pushState(histState, '', url);
  document.getElementById('libDetailScroll').scrollTop = 0;

  renderDetailShell();  // instant paint from cached data

  // Fetch full release set + enriched metadata in parallel.
  const params = new URLSearchParams({
    title: detail.title, content_type: ct,
    content_source: g.contentSource || '', content_id: g.contentId || '',
  });
  const [releases, meta] = await Promise.all([
    api(`/api/titles/releases?${params}`),
    detail.tmdbId ? api(`/api/meta/${mediaTypeFor(ct)}/${encodeURIComponent(detail.tmdbId)}`) : Promise.resolve(null),
  ]);
  if (detail.group !== g) return;  // a newer openDetail superseded this fetch
  if (releases && releases.items) detail.releases = releases.items;
  detail.meta = meta;
  renderDetailShell();
  renderDetailBody();
}

function closeDetail(fromPop) {
  const view = document.getElementById('libDetail');
  if (!view.classList.contains('open')) return;
  // User-initiated close with a matching history entry: pop it and let popstate
  // do the visual close, so exactly one overlay closes per Back.
  if (!fromPop && history.state && history.state.lib === 'detail') { history.back(); return; }
  view.classList.remove('open');
  view.setAttribute('aria-hidden', 'true');
  closeModal(view);
}

function renderDetailShell() {
  const m = detail.meta, g = detail.group, ct = detail.ct;
  const best = g.best;
  const backdrop = m && m.backdrop;
  const posterUrl = m && m.poster;
  const posterId = detail.tmdbId || (ct === 'music' && !best.contentId ? best.name : '');
  const posterSource = ct === 'music' ? (best.contentSource === 'musicbrainz' ? 'mb' : 'lidarr') : 'tmdb';

  const year = m ? m.year : '';
  const metaBits = [];
  if (year) metaBits.push(`<span>${escHtml(year)}</span>`);
  if (m && m.runtime) metaBits.push(`<span>${escHtml(fmtRuntime(m.runtime))}</span>`);
  if (ct === 'tv_show' && m && m.numberOfSeasons) metaBits.push(`<span>${m.numberOfSeasons} season${m.numberOfSeasons !== 1 ? 's' : ''}</span>`);
  if (m && m.voteAverage) metaBits.push(_star(m.voteAverage));
  metaBits.push(`<span class="lib-genre-inline">${escHtml(typeLabel(ct))}</span>`);
  if (!g.isMapped) metaBits.push(`<span style="color:var(--color-warning)">unmatched</span>`);

  const links = [];
  if (m && m.tmdbId) links.push(`<a class="lib-extlink" target="_blank" rel="noopener" href="https://www.themoviedb.org/${m.mediaType}/${m.tmdbId}"><svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2"><path d="M18 13v6a2 2 0 01-2 2H5a2 2 0 01-2-2V8a2 2 0 012-2h6"/><polyline points="15 3 21 3 21 9"/><line x1="10" y1="14" x2="21" y2="3"/></svg>TMDB</a>`);
  // Every title has a stable shareable permalink.
  links.push(`<button type="button" class="lib-extlink" onclick="copyDetailLink()" aria-label="Copy link to this title"><svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2"><path d="M10 13a5 5 0 007.54.54l3-3a5 5 0 00-7.07-7.07l-1.72 1.71"/><path d="M14 11a5 5 0 00-7.54-.54l-3 3a5 5 0 007.07 7.07l1.71-1.71"/></svg>Copy link</button>`);

  const genres = (m && m.genres && m.genres.length)
    ? `<div class="lib-genres">${m.genres.map(x => `<span class="lib-genre">${escHtml(x)}</span>`).join('')}</div>` : '';
  const overview = (m && m.overview) ? `<p class="lib-hero-overview">${escHtml(m.overview)}</p>` : '';
  const tagline = (m && m.tagline) ? `<p class="lib-hero-tagline">${escHtml(m.tagline)}</p>` : '';

  const posterInner = posterUrl
    ? `<img src="${escAttr(posterUrl)}" alt="">`
    : posterId
      ? `<img src="${escAttr(posterImgSrc(posterId, posterSource, ct))}" alt="" loading="lazy" decoding="async" onerror="this.remove()">`
      : `<div class="lib-poster-ph">${iconFor(ct)}<span>${escHtml(typeLabel(ct))}</span></div>`;

  document.getElementById('libDetailScroll').innerHTML = `
    <div class="lib-hero ${backdrop ? '' : 'no-backdrop'}">
      <div class="lib-hero-bg" ${backdrop ? `style="background-image:url('${escAttr(backdrop)}')"` : ''}></div>
      <div class="lib-hero-inner">
        <div class="lib-hero-poster">${posterInner}</div>
        <div class="lib-hero-text">
          <h1 class="lib-hero-title" id="libHeroTitle">${escHtml(detail.title)}</h1>
          ${tagline}
          <div class="lib-hero-metarow">${metaBits.join('<span class="dot-sep"></span>')}</div>
          ${genres}
          ${overview}
          <div class="lib-hero-links">${links.join('')}</div>
        </div>
      </div>
    </div>
    <div class="lib-detail-body" id="libDetailBody"></div>`;

}

function renderDetailBody() {
  const body = document.getElementById('libDetailBody');
  if (!body) return;
  const ct = detail.ct;
  let html = '';
  if (ct === 'tv_show') html = renderTvSection();
  else html = renderMovieSection();

  // Cast row (if enriched)
  const m = detail.meta;
  if (m && m.cast && m.cast.length) {
    html += `<div class="lib-section"><div class="lib-section-head"><h2>Cast</h2></div>
      <div class="lib-cast">${m.cast.map(c => `
        <div class="lib-cast-card">
          <div class="lib-cast-photo">${c.profile ? `<img src="${escAttr(c.profile)}" alt="" loading="lazy">` : `<div class="ph">${escHtml((c.name || '?')[0])}</div>`}</div>
          <div class="lib-cast-name">${escHtml(c.name)}</div>
          ${c.character ? `<div class="lib-cast-role">${escHtml(c.character)}</div>` : ''}
        </div>`).join('')}</div></div>`;
  }
  body.innerHTML = html;
}

/* Release filters shared by movie + TV */
function chipSet(values, active, setter, allLabel) {
  if (!values.length) return '';
  return ['all', ...values].map(v => {
    const label = v === 'all' ? allLabel : v;
    return `<button class="lib-chip-btn ${active === v ? 'active' : ''}" onclick="${setter}('${escAttr(jsStringArg(v))}')">${escHtml(label)}</button>`;
  }).join('');
}
function releaseFilterBar() {
  const resSet = [...new Set(detail.releases.map(resOf).filter(Boolean))]
    .sort((a, b) => (RES_RANK[b] || 0) - (RES_RANK[a] || 0));
  const srcSet = [...new Set(detail.releases.map(srcOf).filter(Boolean))]
    .sort((a, b) => a.localeCompare(b));
  const editionSet = detail.ct === 'tv_show' ? [] : [...new Set(detail.releases.map(t => editionOf(_rname(t))).filter(Boolean))]
    .sort((a, b) => a === 'Standard' ? -1 : b === 'Standard' ? 1 : a.localeCompare(b));
  const groups = [
    chipSet(resSet, detail.quality, 'setQuality', 'All quality'),
    chipSet(srcSet, detail.source, 'setSource', 'All sources'),
    chipSet(editionSet, detail.edition, 'setEdition', 'All editions'),
  ].filter(Boolean);
  return groups.length ? `<div class="lib-release-filters">${groups.map(g => `<div class="lib-chips">${g}</div>`).join('')}</div>` : '';
}
function setQuality(q) { detail.quality = q; renderDetailBody(); }
function setSource(s) { detail.source = s; renderDetailBody(); }
function setEdition(e) { detail.edition = e; renderDetailBody(); }
function filteredReleases() {
  return detail.releases.filter(t => {
    if (detail.quality !== 'all' && resOf(t) !== detail.quality) return false;
    if (detail.source !== 'all' && srcOf(t) !== detail.source) return false;
    if (detail.edition !== 'all' && editionOf(_rname(t)) !== detail.edition) return false;
    return true;
  });
}

/* Movie: releases grouped by edition */
function relRow(t) {
  const res = resOf(t), flags = flagsOf(t), src = srcOf(t);
  const langs = (t.languages || []).filter(l => l && l !== 'en');
  const rn = _rname(t);
  const magnet = `magnet:?xt=urn:btih:${t.infoHash}&dn=${encodeURIComponent(rn)}`;
  const tags = [];
  if (res) tags.push(`<span class="lib-tag res">${escHtml(res)}</span>`);
  flags.forEach(f => tags.push(`<span class="lib-tag ${f === 'HDR' ? 'hdr' : 'flag-' + f.toLowerCase()}">${f}</span>`));
  if (src) tags.push(`<span class="lib-tag src">${escHtml(src)}</span>`);
  if (t.releaseGroup) tags.push(`<span class="lib-tag grp">${escHtml(t.releaseGroup)}</span>`);
  if (langs.length) tags.push(`<span class="lib-tag lang">${escHtml(langs.slice(0, 2).join(', '))}</span>`);
  const ih = escAttr(jsStringArg(t.infoHash));
  return `<div class="lib-rel" role="button" tabindex="0" aria-label="${escAttr('Open details for ' + rn)}"
    onclick="openTorrentDrawer('${ih}')"
    onkeydown="if(event.key==='Enter'||event.key===' '){event.preventDefault();openTorrentDrawer('${ih}')}">
    <div class="lib-rel-main">
      <div class="lib-rel-name">${escHtml(rn)}</div>
      <div class="lib-rel-tags">${tags.join('')}</div>
    </div>
    <div class="lib-rel-meta">
      <span class="lib-rel-size">${fmtBytes(t.size)}</span>
      <span class="lib-rel-seed" title="${t.seeders || 0} seeders / ${t.leechers || 0} leechers"><svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="3"><polyline points="18 15 12 9 6 15"/></svg>${fmtNum(t.seeders || 0)}</span>
      <button class="lib-copybtn" title="Copy magnet link" onclick="event.stopPropagation();copyMagnet('${escAttr(jsStringArg(magnet))}')">
        <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2"><rect x="9" y="9" width="13" height="13" rx="2"/><path d="M5 15H4a2 2 0 01-2-2V4a2 2 0 012-2h9a2 2 0 012 2v1"/></svg>
      </button>
    </div>
  </div>`;
}
function relGroup(title, rows) {
  if (!rows.length) return '';
  return `<div class="lib-relgroup"><div class="lib-relgroup-head">${escHtml(title)}<span class="count">${rows.length}</span></div>${rows.map(relRow).join('')}</div>`;
}

// Collapsed-by-default disclosure for bulk packs (complete-series / season
// packs), so a title with many packs doesn't bury the per-episode breakdown.
function packDisclosure(id, label, rows) {
  if (!rows.length) return '';
  return `<div class="lib-disclosure" id="${escAttr(id)}">
    <button class="lib-disclosure-head" onclick="toggleDisclosure('${escAttr(id)}')" aria-expanded="false">
      <svg class="lib-disclosure-chevron" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2"><polyline points="9 18 15 12 9 6"/></svg>
      <svg class="lib-disclosure-ico" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.8"><path d="M21 8v13H3V8"/><rect x="1" y="3" width="22" height="5" rx="1"/><line x1="10" y1="12" x2="14" y2="12"/></svg>
      <span class="lib-disclosure-label">${escHtml(label)}</span>
      <span class="lib-disclosure-count">${rows.length}</span>
      <span class="lib-disclosure-hint">show</span>
    </button>
    <div class="lib-disclosure-body" style="display:none">${rows.map(relRow).join('')}</div>
  </div>`;
}
function toggleDisclosure(id) {
  const el = document.getElementById(id);
  if (!el) return;
  const body = el.querySelector('.lib-disclosure-body');
  const head = el.querySelector('.lib-disclosure-head');
  const open = el.classList.toggle('open');
  if (body) body.style.display = open ? '' : 'none';
  if (head) head.setAttribute('aria-expanded', String(open));
  const hint = el.querySelector('.lib-disclosure-hint');
  if (hint) hint.textContent = open ? 'hide' : 'show';
}

function renderMovieSection() {
  const rels = filteredReleases();
  const byEd = new Map();
  for (const t of rels) {
    const e = editionOf(_rname(t));
    if (!byEd.has(e)) byEd.set(e, []);
    byEd.get(e).push(t);
  }
  const order = [...byEd.keys()].sort((a, b) => a === 'Standard' ? -1 : b === 'Standard' ? 1 : a.localeCompare(b));
  const inner = order.map(ed => relGroup(ed, byEd.get(ed).sort(relSort))).join('')
    || `<div class="lib-rel-empty">No releases match this filter.</div>`;
  return `<div class="lib-section">
    <div class="lib-toolbar"><div class="lib-section-head"><h2>Downloads</h2><span class="count">${rels.length}/${detail.releases.length} release${detail.releases.length !== 1 ? 's' : ''}</span></div></div>
    ${releaseFilterBar()}${inner}</div>`;
}

/* TV: season navigation + episodes with per-episode torrents */
function availableSeasons(releases = detail.releases) {
  const fromTorrents = new Set(releases
    .map(t => parseTorrentKind(_rname(t)))
    .filter(k => k.season != null && k.kind !== 'complete')
    .map(k => k.season));
  const fromMeta = new Set((detail.meta && detail.meta.seasons ? detail.meta.seasons : [])
    .map(s => s.seasonNumber).filter(n => n != null && n >= 1));
  const all = new Set([...fromTorrents, ...fromMeta]);
  return [...all].sort((a, b) => a - b);
}

function renderTvSection() {
  const rels = filteredReleases();
  const parsed = rels.map(t => ({ t, k: parseTorrentKind(_rname(t)) }));
  const complete = parsed.filter(p => p.k.kind === 'complete').map(p => p.t).sort(relSort);
  const seasons = availableSeasons(rels);

  if (detail.activeSeason == null || !seasons.includes(detail.activeSeason)) detail.activeSeason = seasons.length ? seasons[0] : null;

  // Whole-series packs collapse into one disclosure above the season nav so
  // they never push the episode breakdown off-screen.
  const top = packDisclosure('disc-complete', 'Complete-series packs', complete);

  const nav = seasons.length ? `<div class="lib-season-nav">${seasons.map(s =>
    `<button class="lib-chip-btn ${String(detail.activeSeason) === String(s) ? 'active' : ''}" onclick="setSeason(${s})">Season ${s}</button>`).join('')}</div>` : '';

  const seasonBlock = seasons.length ? `<div id="libSeasonBlock">${renderSeasonBlock(parsed)}</div>` : '';

  const empty = (!complete.length && !seasons.length)
    ? `<div class="lib-rel-empty">No releases match this filter.</div>` : '';

  return `<div class="lib-section">
    <div class="lib-toolbar"><div class="lib-section-head"><h2>Seasons &amp; Episodes</h2><span class="count">${rels.length}/${detail.releases.length} release${detail.releases.length !== 1 ? 's' : ''}</span></div></div>
    ${releaseFilterBar()}
    ${top}
    ${nav}
    ${seasonBlock}
    ${empty}
  </div>`;
}

function setSeason(s) {
  detail.activeSeason = s;
  const block = document.getElementById('libSeasonBlock');
  const parsed = filteredReleases().map(t => ({ t, k: parseTorrentKind(_rname(t)) }));
  if (block) block.innerHTML = renderSeasonBlock(parsed);
  // Refresh nav active state
  document.querySelectorAll('.lib-season-nav .lib-chip-btn').forEach(b => {
    b.classList.toggle('active', b.textContent === `Season ${s}`);
  });
  maybeLoadSeasonMeta();
}

function renderSeasonBlock(parsed) {
  const s = detail.activeSeason;
  if (s == null) return '';
  const inSeason = parsed.filter(p => p.k.season === s && p.k.kind !== 'complete');
  const packs = inSeason.filter(p => p.k.kind === 'season').map(p => p.t).sort(relSort);
  const eps = inSeason.filter(p => p.k.kind === 'episode');

  // torrents per episode number
  const byEp = new Map();
  for (const p of eps) {
    if (!byEp.has(p.k.episode)) byEp.set(p.k.episode, []);
    byEp.get(p.k.episode).push(p.t);
  }

  // Season packs collapse into a disclosure; the episode list stays primary.
  let html = packDisclosure(`disc-season-${s}`, `Season ${s} packs`, packs);

  // Episode catalog from TMDB (if loaded); otherwise derive from torrents.
  const seasonMeta = detail.seasonCache.get(String(s));
  let episodeList;
  if (seasonMeta && seasonMeta.episodes && seasonMeta.episodes.length) {
    episodeList = seasonMeta.episodes.map(e => ({ meta: e, num: e.episodeNumber }));
    // include torrent-only episodes beyond the catalog
    const known = new Set(episodeList.map(e => e.num));
    [...byEp.keys()].filter(n => !known.has(n)).sort((a, b) => a - b)
      .forEach(n => episodeList.push({ meta: null, num: n }));
  } else {
    episodeList = [...byEp.keys()].sort((a, b) => a - b).map(n => ({ meta: null, num: n }));
  }

  if (episodeList.length) {
    const availCount = episodeList.filter(e => (byEp.get(e.num) || []).length).length;
    html += `<div class="lib-episodes-head"><span>Episodes</span><span class="count">${availCount}/${episodeList.length} with torrents</span></div>`;
    html += `<div class="lib-episodes" id="libEpisodes">` + episodeList.map(e => {
      const rows = (byEp.get(e.num) || []).sort(relSort);
      const em = e.meta;
      const num = String(e.num).padStart(2, '0');
      const title = em && em.name ? em.name : `Episode ${e.num}`;
      const metaLine = [];
      if (em && em.airDate) metaLine.push(fmtDate(em.airDate));
      if (em && em.runtime) metaLine.push(fmtRuntime(em.runtime));
      if (em && em.voteAverage) metaLine.push('★ ' + em.voteAverage.toFixed(1));
      const still = em && em.still
        ? `<img src="${escAttr(em.still)}" alt="" loading="lazy">`
        : `<div class="ph">E${num}</div>`;
      const avail = rows.length
        ? `<span class="lib-episode-avail has">${rows.length} torrent${rows.length !== 1 ? 's' : ''}</span>`
        : `<span class="lib-episode-avail none">none</span>`;
      const eid = `ep-${s}-${e.num}`;
      const headKbd = rows.length
        ? `role="button" tabindex="0" aria-expanded="false" onclick="toggleEpisode('${eid}')" onkeydown="if(event.key==='Enter'||event.key===' '){event.preventDefault();toggleEpisode('${eid}')}"`
        : '';
      return `<div class="lib-episode" id="${eid}">
        <div class="lib-episode-head ${rows.length ? '' : 'is-empty'}" ${headKbd}>
          <div class="lib-episode-still">${still}</div>
          <div class="lib-episode-main">
            <div class="lib-episode-title"><span class="epnum">S${String(s).padStart(2,'0')}E${num}</span>${escHtml(title)}</div>
            ${metaLine.length ? `<div class="lib-episode-meta">${escHtml(metaLine.join(' · '))}</div>` : ''}
            ${em && em.overview ? `<div class="lib-episode-overview">${escHtml(em.overview)}</div>` : ''}
          </div>
          <div class="lib-episode-right">
            ${avail}
            <span class="lib-episode-chevron" ${rows.length ? '' : 'style="visibility:hidden"'}><svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2"><polyline points="9 18 15 12 9 6"/></svg></span>
          </div>
        </div>
        ${rows.length ? `<div class="lib-episode-body" style="display:none">${rows.map(relRow).join('')}</div>` : ''}
      </div>`;
    }).join('') + `</div>`;
  } else if (!packs.length) {
    html += `<div class="lib-rel-empty">No episode torrents indexed for this season yet.</div>`;
  }
  return html;
}

function toggleEpisode(eid) {
  const el = document.getElementById(eid);
  if (!el) return;
  const body = el.querySelector('.lib-episode-body');
  if (!body) return;
  const open = el.classList.toggle('open');
  body.style.display = open ? '' : 'none';
  const head = el.querySelector('.lib-episode-head');
  if (head) head.setAttribute('aria-expanded', String(open));
}

/* Lazily fetch the TMDB episode catalog for the active season, then re-render. */
async function maybeLoadSeasonMeta() {
  const s = detail.activeSeason;
  if (s == null || !detail.tmdbId) return;
  if (detail.seasonCache.has(String(s))) return;
  const data = await api(`/api/meta/tv/${encodeURIComponent(detail.tmdbId)}/season/${s}`);
  detail.seasonCache.set(String(s), data || { episodes: [] });
  if (String(detail.activeSeason) !== String(s)) return; // user moved on
  const block = document.getElementById('libSeasonBlock');
  if (block) {
    const parsed = filteredReleases().map(t => ({ t, k: parseTorrentKind(_rname(t)) }));
    block.innerHTML = renderSeasonBlock(parsed);
  }
}

/* ── Torrent drawer ─────────────────────────────────────────────────────── */
let _drawerSeq = 0;
async function openTorrentDrawer(infoHash) {
  const drawer = document.getElementById('libDrawer');
  const scrim = document.getElementById('libDrawerScrim');
  const bodyEl = document.getElementById('libDrawerBody');
  drawer.classList.add('open'); scrim.classList.add('open');
  drawer.setAttribute('aria-hidden', 'false');
  openModal(drawer);
  // Own history entry (over the detail entry) so Back closes just the drawer.
  history.pushState({ lib: 'drawer', infoHash }, '');
  const seq = ++_drawerSeq;
  bodyEl.innerHTML = `<div class="lib-rel-empty">Loading…</div>`;

  const d = await api(`/api/torrents/${encodeURIComponent(infoHash)}`);
  if (seq !== _drawerSeq || !drawer.classList.contains('open')) return;
  if (!d) { bodyEl.innerHTML = `<div class="lib-rel-empty">Couldn’t load this torrent.</div>`; return; }

  const flags = flagsOf(d), res = resOf(d), src = srcOf(d);
  const tags = [];
  if (res) tags.push(`<span class="lib-tag res">${escHtml(res)}</span>`);
  flags.forEach(f => tags.push(`<span class="lib-tag ${f === 'HDR' ? 'hdr' : 'flag-' + f.toLowerCase()}">${f}</span>`));
  if (src) tags.push(`<span class="lib-tag src">${escHtml(src)}</span>`);
  if (d.videoCodec) tags.push(`<span class="lib-tag">${escHtml(d.videoCodec)}</span>`);
  if (d.releaseGroup) tags.push(`<span class="lib-tag grp">${escHtml(d.releaseGroup)}</span>`);

  const kv = [];
  const push = (k, v) => { if (v || v === 0) kv.push(`<dt>${k}</dt><dd>${v}</dd>`); };
  push('Type', escHtml(typeLabel(d.contentType)));
  push('Size', escHtml(fmtBytes(d.size)));
  push('Files', d.filesCount ? fmtNum(d.filesCount) : '—');
  push('Seeders', `<span style="color:var(--color-success);font-weight:600">${fmtNum(d.seeders || 0)}</span>`);
  push('Leechers', fmtNum(d.leechers || 0));
  if (d.seasonNumber != null) push('Season', d.seasonNumber);
  if (d.episodeNumber != null) push('Episode', d.episodeNumber);
  if (d.episodeTitle) push('Episode title', escHtml(d.episodeTitle));
  if (d.languages && d.languages.length) push('Languages', escHtml(d.languages.join(', ')));
  if (d.originalLanguage) push('Original lang', escHtml(d.originalLanguage));
  push('Discovered', escHtml(fmtDate(d.createdAt)));
  push('Updated', escHtml(fmtDate(d.updatedAt)));
  push('Match', d.isMapped ? `${escHtml((d.contentSource || '').toUpperCase())}${d.tmdbId ? ' #' + escHtml(d.tmdbId) : ''}` : '<span style="color:var(--color-warning)">unmatched</span>');
  push('Info hash', `<span style="font-family:var(--font-mono);font-size:11px">${escHtml(d.infoHash)}</span>`);

  const files = (d.files || []).slice().sort((a, b) => (b.size || 0) - (a.size || 0));
  const filesHtml = files.length ? `<div class="lib-files">
    <div class="lib-files-title">Files (${fmtNum(files.length)})</div>
    ${files.slice(0, 100).map(f => `<div class="lib-file"><span class="lib-file-path" title="${escAttr(f.path)}">${escHtml(f.path)}</span><span class="lib-file-size">${fmtBytes(f.size)}</span></div>`).join('')}
    ${files.length > 100 ? `<div class="lib-file"><span class="lib-file-path">+${fmtNum(files.length - 100)} more…</span></div>` : ''}
  </div>` : '';

  const magnet = d.magnetUri || `magnet:?xt=urn:btih:${d.infoHash}`;
  bodyEl.innerHTML = `
    <div class="lib-drawer-title">${escHtml(d.name)}</div>
    ${tags.length ? `<div class="lib-drawer-tags">${tags.join('')}</div>` : ''}
    <dl class="lib-kv">${kv.join('')}</dl>
    <div class="lib-drawer-actions">
      <a class="lib-btn primary" href="${escAttr(magnet)}"><svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2"><path d="M6 3v6a6 6 0 006 6 6 6 0 006-6V3"/><line x1="6" y1="3" x2="2" y2="3"/><line x1="22" y1="3" x2="18" y2="3"/></svg>Open magnet</a>
      <button class="lib-btn" onclick="copyMagnet('${escAttr(jsStringArg(magnet))}')"><svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2"><rect x="9" y="9" width="13" height="13" rx="2"/><path d="M5 15H4a2 2 0 01-2-2V4a2 2 0 012-2h9a2 2 0 012 2v1"/></svg>Copy</button>
    </div>
    ${filesHtml}`;
}
function closeTorrentDrawer(fromPop) {
  const drawer = document.getElementById('libDrawer');
  if (!drawer.classList.contains('open')) return;
  if (!fromPop && history.state && history.state.lib === 'drawer') { history.back(); return; }
  drawer.classList.remove('open');
  document.getElementById('libDrawerScrim').classList.remove('open');
  drawer.setAttribute('aria-hidden', 'true');
  closeModal(drawer);
}

/* ── Toast + clipboard ──────────────────────────────────────────────────── */
let _toastTimer = null;
function toast(msg) {
  const el = document.getElementById('libToast');
  el.textContent = msg;
  el.classList.add('show');
  clearTimeout(_toastTimer);
  _toastTimer = setTimeout(() => el.classList.remove('show'), 2200);
}
function copyMagnet(magnet) {
  const done = () => toast('Magnet link copied');
  if (navigator.clipboard && navigator.clipboard.writeText) {
    navigator.clipboard.writeText(magnet).then(done).catch(() => fallbackCopy(magnet, done));
  } else fallbackCopy(magnet, done);
}
function fallbackCopy(text, done) {
  const ta = document.createElement('textarea');
  ta.value = text; ta.style.position = 'fixed'; ta.style.opacity = '0';
  document.body.appendChild(ta); ta.select();
  try { document.execCommand('copy'); done(); } catch (_) { toast('Copy failed'); }
  document.body.removeChild(ta);
}

/* ── Account / API key panel ───────────────────────────────────────────── */
let accountState = null;
function openAccount() {
  const panel = document.getElementById('libAccount');
  const scrim = document.getElementById('libAccountScrim');
  panel.classList.add('open'); scrim.classList.add('open');
  panel.setAttribute('aria-hidden', 'false');
  openModal(panel);
  // Own history entry so Back closes the account panel (was previously orphaned).
  history.pushState({ lib: 'account' }, '');
  loadAccount();
}
function closeAccount(fromPop) {
  const panel = document.getElementById('libAccount');
  const scrim = document.getElementById('libAccountScrim');
  if (!panel.classList.contains('open')) return;
  if (!fromPop && history.state && history.state.lib === 'account') { history.back(); return; }
  panel.classList.remove('open'); scrim.classList.remove('open');
  panel.setAttribute('aria-hidden', 'true');
  closeModal(panel);
}
async function loadAccount() {
  const data = await api('/api/account');
  if (!data) { toast('Account unavailable'); return; }
  renderAccount(data);
}
function renderAccount(data) {
  accountState = data;
  const ident = data.identity || {};
  const name = ident.display || ident.username || ident.email || ident.id || 'Account';
  const sub = [ident.email || ident.username || ident.id, ident.method].filter(Boolean).join(' · ');
  document.getElementById('acctAvatar').textContent = (name[0] || '?').toUpperCase();
  document.getElementById('acctName').textContent = name;
  document.getElementById('acctSub').textContent = sub;
  document.getElementById('acctTorznabUrl').value = data.torznabUrl || '';

  const key = data.apiKey;
  document.getElementById('acctKeyStatus').textContent = key ? `Active ${key.prefix || ''}` : 'No key';
  document.getElementById('acctRevokeBtn').disabled = !key;
  const genLabel = document.getElementById('acctGenerateLabel');
  if (genLabel) genLabel.textContent = key ? 'Rotate' : 'Generate';

  const keyMeta = document.getElementById('acctKeyMeta');
  if (keyMeta) {
    if (key) {
      const bits = [];
      if (key.createdAt) bits.push('Created ' + fmtDate(key.createdAt));
      bits.push(key.lastUsedAt ? 'Last used ' + fmtDate(key.lastUsedAt) : 'Never used');
      keyMeta.textContent = bits.join(' · ');
      keyMeta.style.display = '';
    } else {
      keyMeta.textContent = '';
      keyMeta.style.display = 'none';
    }
  }

  const secretWrap = document.getElementById('acctSecretWrap');
  const secretInput = document.getElementById('acctApiSecret');
  if (data.apiKeySecret) {
    secretInput.value = data.apiKeySecret;
    secretWrap.style.display = '';
  } else {
    secretInput.value = '';
    secretWrap.style.display = 'none';
  }
}
async function generateAccountKey() {
  const btn = document.getElementById('acctGenerateBtn');
  btn.disabled = true;
  const data = await api('/api/account/api-key', {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ name: 'default' }),
  });
  btn.disabled = false;
  if (!data) { toast('Key update failed'); return; }
  renderAccount(data);
  toast('API key ready');
}
async function revokeAccountKey() {
  const btn = document.getElementById('acctRevokeBtn');
  btn.disabled = true;
  const data = await api('/api/account/api-key', { method: 'DELETE' });
  if (!data) { btn.disabled = false; toast('Revoke failed'); return; }
  renderAccount(data);
  toast('API key revoked');
}
function copyAccountField(id) {
  const el = document.getElementById(id);
  if (!el || !el.value) return;
  if (navigator.clipboard && navigator.clipboard.writeText) {
    navigator.clipboard.writeText(el.value).then(() => toast('Copied')).catch(() => fallbackCopy(el.value, () => toast('Copied')));
  } else fallbackCopy(el.value, () => toast('Copied'));
}

/* ── Global keys + history ──────────────────────────────────────────────── */
if (typeof document !== 'undefined') {
  document.addEventListener('keydown', e => {
    if (e.key === 'Escape') {
      if (document.getElementById('libAccount').classList.contains('open')) { closeAccount(); return; }
      if (document.getElementById('libDrawer').classList.contains('open')) { closeTorrentDrawer(); return; }
      if (document.getElementById('libDetail').classList.contains('open')) { closeDetail(); return; }
    }
    // "/" focuses search when not already typing
    if (e.key === '/' && !/^(input|textarea|select)$/i.test((e.target.tagName || ''))) {
      e.preventDefault(); document.getElementById('libSearch').focus();
    }
  });
  window.addEventListener('popstate', () => {
    // Close only the topmost open overlay (each pushed exactly one entry).
    if (document.getElementById('libAccount').classList.contains('open')) { closeAccount(true); return; }
    if (document.getElementById('libDrawer').classList.contains('open')) { closeTorrentDrawer(true); return; }
    if (document.getElementById('libDetail').classList.contains('open')) { closeDetail(true); return; }
    // No overlay open → a browse-history navigation; re-hydrate and reload.
    applyStateFromUrl();
    loadLibrary({ fromPop: true });
  });
}

/* When the active season changes we also want its TMDB episode catalog. The
   first season's catalog is fetched right after the detail body first paints. */
const _origRenderDetailBody = renderDetailBody;
renderDetailBody = function () { _origRenderDetailBody(); if (detail.ct === 'tv_show') maybeLoadSeasonMeta(); };

/* ── Boot ───────────────────────────────────────────────────────────────── */
if (typeof document !== 'undefined') {
  applyStateFromUrl();
  const _bootPermalink = _readPermalink();
  // Establish a clean browse entry as the history base (strips any title params),
  // then open the shared title (if any) as a pushed entry on top so Back returns
  // to the browse view rather than leaving the site.
  history.replaceState({ lib: 'browse' }, '', browseUrl());
  renderTypePills();
  if (document.getElementById('libStatsGrid')) loadLibraryStats();
  loadLibrary({ fromPop: true });
  if (_bootPermalink) resolvePermalink(_bootPermalink);
}

// Node's built-in test runner imports these pure state/request helpers. The
// browser remains a classic script and never exposes this object globally.
if (typeof module !== 'undefined' && module.exports) {
  module.exports = {
    browseParamsFor,
    parseBrowseState,
    historyModeFor,
    hasAdvancedFiltersFor,
    isHomeFor,
    sortTransitionFor,
    serverGroupedFor,
    backendAnimeModeFor,
    gridRequestFor,
    facetRequestFor,
    facetKeyFor,
    needsFacetFetchFor,
    applyFeatureFiltersFor,
    groupTorrents,
    rawWindowMetaFor,
    rawResultNoteFor,
    normalizeLibraryStats,
    libraryStatsViewFor,
  };
}

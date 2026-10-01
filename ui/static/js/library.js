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
const BitAgentPreferences = typeof module !== 'undefined' && module.exports ? require('./library-preferences.js') : window.BitAgentPreferences;

/* ── Theme ──────────────────────────────────────────────────────────────── */
function syncThemeColor() {
  const dark = document.documentElement.getAttribute('data-theme') === 'dark';
  const meta = document.querySelector('meta[name="theme-color"]');
  if (meta) meta.setAttribute('content', dark ? '#0f1117' : '#ffffff');
}
function toggleTheme() {
  const isDark = document.documentElement.getAttribute('data-theme') === 'dark';
  const next = isDark ? 'light' : 'dark';
  if (typeof setAccountPreference === 'function' && _accountPreferenceStore) setAccountPreference('theme', next);
  else { document.documentElement.setAttribute('data-theme', next); syncThemeColor(); }
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
  return document.documentElement.getAttribute('data-library-motion') === 'reduced' || !!(window.matchMedia && window.matchMedia('(prefers-reduced-motion: reduce)').matches);
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
  document.querySelector('.lib-skiplink')?.toggleAttribute('inert', anyOpen);
  const topPanel = anyOpen ? _modalStack[_modalStack.length - 1].panel : null;
  document.querySelectorAll('.lib-app > [role="dialog"]').forEach(panel => {
    panel.toggleAttribute('inert', !panel.classList.contains('open') || panel !== topPanel);
  });
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
  q: '', type: '', sort: 'seeders', provider: '', network: '', region: 'US',
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
  if (browseState.type || browseState.network) p.set('type', browseState.network ? 'tv_show' : browseState.type);
  if (browseState.network) p.set('network', browseState.network);
  else if (browseState.provider) { p.set('provider', browseState.provider); p.set('region', browseState.region || 'US'); }
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
  const networkValue = p.get('network') || '';
  const network = /^[1-9]\d{0,9}$/.test(networkValue) && Number(networkValue) <= 2147483647 ? networkValue : '';
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
    type: network ? 'tv_show' : p.get('type') || '',
    provider: !network && /^[1-9]\d{0,7}$/.test(p.get('provider') || '') ? p.get('provider') : '',
    network,
    region: /^[A-Z]{2}$/.test(p.get('region') || '') ? p.get('region') : 'US',
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
function applyStateFromUrl(useDefaults = false) {
  const parsed=parseBrowseState(location.search);
  Object.assign(state, useDefaults ? BitAgentPreferences.applyBrowseDefaultsFor(parsed,accountPreferenceSettings(),location.search) : parsed);
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
function setType(type) {
  if (state.network && type !== 'tv_show') state.network = '';
  state.type = type; state.page = 0; renderTypePills(); loadLibrary();
}
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
  state.provider = '';
  state.network = '';
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
  return !browseState.provider && !browseState.network && !browseState.q && browseState.type === '' && !hasAdvancedFiltersFor(browseState) &&
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
  const grid = document.getElementById('libraryGrid');
  if (grid) grid.setAttribute('aria-busy', String(on));
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
  if ((state.provider || state.network) && typeof loadProviderLibrary === 'function') {
    showHome(false);
    return loadProviderLibrary(nav.seq, nav.signal);
  }
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
  if (state.provider || state.network) {
    if ((dir > 0 && !state.hasNext) || (dir < 0 && state.page === 0)) return;
    state.page += dir;
    if (!_modalStack.length) syncUrl(historyModeFor('page'));
    loadLibrary({fromPop:true, keepPage:true});
    document.getElementById('libResults').scrollIntoView({behavior:_scrollBehavior(), block:'start'});
    return;
  }
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
function posterFallbackHtml(title, ct) {
  return `<div class="lib-poster-ph" aria-hidden="true"><span class="lib-poster-fallback-type">${escHtml(typeLabel(ct))}</span><strong class="lib-poster-fallback-title">${escHtml(title)}</strong>${iconFor(ct)}</div>`;
}
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
      ${posterFallbackHtml(title, ct)}
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
  seasonCache: new Map(), loading: false, truncated: false, loadError: false, bulkMode: 'best', filtersExpanded: false };
let _detailAbortController = null;
const magnetSelection = new Map();
const MAGNET_SELECTION_LIMIT = 1000;

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
  if (_detailAbortController) _detailAbortController.abort();
  _detailAbortController = new AbortController();
  const signal = _detailAbortController.signal;
  detail.loading = true; detail.truncated = false; detail.loadError = false;
  detail.activeSeason = null; detail.quality = 'all'; detail.source = 'all'; detail.edition = 'all'; detail.seasonCache = new Map();
  detail.filtersExpanded = false;
  detail.bulkMode = accountPreferenceSettings().magnetMode;

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
  renderDetailBody();

  // Fetch full release set + enriched metadata in parallel.
  const identity = {
    title: detail.title, content_type: ct,
    content_source: g.contentSource || '', content_id: g.contentId || '',
  };
  const current = () => !signal.aborted && detail.group === g;
  // Optional metadata must never hold usable magnet links behind a slow
  // provider. Publish each result as soon as its own request finishes.
  const releaseRequest = BitAgentLibraryTools.collectTitleReleases(identity,
    { request: api, signal, maxPages: 12, maxItems: 3000 }).then(result => {
    if (!current()) return;
    detail.loading = false;
    detail.releases = result.items; detail.truncated = result.truncated;
    refreshDetailContent(false);
  }).catch(() => {
    if (!current()) return;
    detail.loading = false; detail.loadError = true;
    refreshDetailContent(false);
  });
  const metadataRequest = (detail.tmdbId
    ? api(`/api/meta/${mediaTypeFor(ct)}/${encodeURIComponent(detail.tmdbId)}`, { signal })
    : Promise.resolve(null)).then(meta => {
    if (!current() || !meta) return;
    detail.meta = meta;
    refreshDetailContent(true);
  });
  await Promise.allSettled([releaseRequest, metadataRequest]);
}

function refreshDetailContent(shell) {
  const view = document.getElementById('libDetail');
  const active = document.activeElement;
  const label = active && view.contains(active) ? active.getAttribute('aria-label') : null;
  const opened = [...view.querySelectorAll('.lib-disclosure.open, .lib-episode.open')].map(el => el.id);
  if (shell) renderDetailShell();
  renderDetailBody();
  opened.forEach(id => {
    const el = document.getElementById(id);
    if (!el || el.classList.contains('open')) return;
    if (el.classList.contains('lib-episode')) toggleEpisode(id); else toggleDisclosure(id);
  });
  if (label) {
    const next = [...view.querySelectorAll('[aria-label]')].find(el => el.getAttribute('aria-label') === label);
    if (next && next.getClientRects().length && !next.closest('[inert]')) next.focus({ preventScroll: true });
  }
}

function closeDetail(fromPop) {
  const view = document.getElementById('libDetail');
  if (!view.classList.contains('open')) return;
  // User-initiated close with a matching history entry: pop it and let popstate
  // do the visual close, so exactly one overlay closes per Back.
  if (!fromPop && history.state && history.state.lib === 'detail') { history.back(); return; }
  view.classList.remove('open');
  if (_detailAbortController) _detailAbortController.abort();
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
  links.push(`<button type="button" class="lib-extlink" onclick="copyDetailLink()" aria-label="Copy link to this title"><svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2"><path d="M10 13a5 5 0 007.54.54l3-3a5 5 0 00-7.07-7.07l-1.72 1.71"/><path d="M14 11a5 5 0 00-7.54-.54l-3 3a5 5 0 007.07 7.07l1.71-1.71"/></svg>Share title</button>`);

  const genres = (m && m.genres && m.genres.length)
    ? `<div class="lib-genres">${m.genres.map(x => `<span class="lib-genre">${escHtml(x)}</span>`).join('')}</div>` : '';
  const overview = (m && m.overview) ? `<p class="lib-hero-overview">${escHtml(m.overview)}</p>` : '';
  const tagline = (m && m.tagline) ? `<p class="lib-hero-tagline">${escHtml(m.tagline)}</p>` : '';

  const posterImage = posterUrl
    ? `<img src="${escAttr(posterUrl)}" alt="" decoding="async" onerror="this.remove()">`
    : posterId
      ? `<img src="${escAttr(posterImgSrc(posterId, posterSource, ct))}" alt="" loading="lazy" decoding="async" onerror="this.remove()">`
      : '';
  const posterInner = posterFallbackHtml(detail.title, ct) + posterImage;

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
  body.innerHTML = magnetGroupToolbar() + html;
  updateMagnetSelection();
}

/* A collection contains explicit releases; bulk shortcuts never start clients. */
function magnetGroupToolbar() {
  const status = detail.loading ? 'Loading indexed releases…'
    : detail.loadError ? 'Releases could not be refreshed. Showing cached releases; retry by reopening the title.'
      : detail.truncated ? 'Showing a bounded set of indexed releases. More may be available; this selection is partial.'
        : `${fmtNum(detail.releases.length)} indexed releases available. Quality and source filters apply to selection.`;
  const disabled = detail.loading ? 'disabled' : '';
  return `<section class="lib-magnet-actions" aria-label="Copy title magnet links">
    <p class="lib-magnet-status" role="status">${escHtml(status)}</p>
    <div class="lib-bulk-actions">
      <label class="lib-bulk-mode">Versions <select id="libBulkMode" onchange="setMagnetMode(this.value)"><option value="best" ${detail.bulkMode === 'best' ? 'selected' : ''}>Recommended (packs + seed counts)</option><option value="all" ${detail.bulkMode === 'all' ? 'selected' : ''}>All matching versions</option></select></label>
      <button class="lib-btn primary" ${disabled} onclick="copyMagnetGroup('${detail.ct === 'tv_show' ? 'show' : 'title'}')">Copy ${detail.ct === 'tv_show' ? 'show' : 'title'} magnets</button>
      ${detail.ct === 'tv_show' ? `<button class="lib-btn" ${detail.loading || !Number.isInteger(detail.activeSeason) ? 'disabled' : ''} onclick="copyMagnetGroup('season')">Copy season magnets</button>` : ''}
    </div>
  </section>`;
}

function copyMagnetGroup(scope) {
  if (detail.loading || (scope === 'season' && !Number.isInteger(detail.activeSeason))) return;
  const releases = BitAgentLibraryTools.selectReleaseGroup(filteredReleases(), { scope, season: detail.activeSeason, mode: detail.bulkMode });
  const links = releases.slice(0, MAGNET_SELECTION_LIMIT).map(BitAgentLibraryTools.magnetFor).filter(Boolean);
  if (!links.length) { toast('No matching magnet links available'); return; }
  const partial = releases.length > MAGNET_SELECTION_LIMIT || detail.truncated;
  copyText(links.join('\n'), `${links.length} magnet links copied${partial ? ' · partial selection' : ''}`, scope => recordLibraryGrab(links.length, 'copy', scope));
}

function toggleMagnetSelection(infoHash) {
  const hash = BitAgentLibraryTools.normalizeInfoHash(infoHash);
  if (!hash) return;
  if (magnetSelection.has(hash)) magnetSelection.delete(hash);
  else {
    const release = detail.releases.find(t => BitAgentLibraryTools.normalizeInfoHash(t.infoHash) === hash);
    if (!release || !BitAgentLibraryTools.magnetFor(release)) return;
    if (magnetSelection.size >= MAGNET_SELECTION_LIMIT) { toast('Collection limit reached. Copy or save your current links first.'); return; }
    magnetSelection.set(hash, release);
  }
  updateMagnetSelection();
}

function selectMagnetGroup(scope) {
  if (detail.loading) return;
  if (scope === 'season' && !Number.isInteger(detail.activeSeason)) { toast('Choose a season with indexed releases first'); return; }
  const mode = detail.bulkMode;
  const releases = BitAgentLibraryTools.selectReleaseGroup(filteredReleases(), { scope, season: detail.activeSeason, mode });
  let added = 0, limited = false;
  for (const release of releases) {
    const hash = BitAgentLibraryTools.normalizeInfoHash(release.infoHash);
    if (!hash || !BitAgentLibraryTools.magnetFor(release) || magnetSelection.has(hash)) continue;
    if (magnetSelection.size >= MAGNET_SELECTION_LIMIT) { limited = true; break; }
    magnetSelection.set(hash, release); added++;
  }
  updateMagnetSelection();
  toast(limited ? 'Collection limit reached; review the selected links.' : added ? `${added} magnet${added === 1 ? '' : 's'} added` : 'No additional matching releases to select');
}
function setMagnetMode(mode) { if (mode === 'best' || mode === 'all') detail.bulkMode = mode; }

function updateMagnetSelection() {
  document.querySelectorAll('[data-magnet-count]').forEach(el => { el.textContent = String(magnetSelection.size); });
  document.querySelectorAll('[data-magnet-select]').forEach(el => { el.checked = magnetSelection.has(el.dataset.magnetSelect); });
  const panel = document.getElementById('libCollection');
  if (!panel || !panel.classList.contains('open')) return;
  const list = document.getElementById('libCollectionList');
  list.innerHTML = [...magnetSelection.entries()].map(([hash, t]) => `<li><span>${escHtml(_rname(t))}</span><button class="lib-iconbtn" onclick="removeCollectedMagnet('${hash}')" aria-label="${escAttr('Remove ' + _rname(t))}">×</button></li>`).join('') || '<li class="lib-rel-empty">Your collection is empty. Select a release, season, or show to get started.</li>';
  document.getElementById('libCollectionText').value = collectionText();
  panel.querySelectorAll('[data-needs-magnets]').forEach(el => { el.disabled = !magnetSelection.size; });
}

function removeCollectedMagnet(hash) { magnetSelection.delete(hash); updateMagnetSelection(); }
function clearMagnetCollection() { magnetSelection.clear(); updateMagnetSelection(); }
function collectionText() { return [...magnetSelection.values()].map(BitAgentLibraryTools.magnetFor).filter(Boolean).join('\n'); }
function copyMagnetCollection() { const text = collectionText(), count = magnetSelection.size; if (text) copyText(text, `${count} magnet links copied`, scope => recordLibraryGrab(count, 'copy', scope)); }
function saveMagnetCollection() {
  const text = collectionText();
  if (!text) return;
  const url = URL.createObjectURL(new Blob([text + '\n'], { type: 'text/plain;charset=utf-8' }));
  const link = document.createElement('a'); link.href = url; link.download = 'bitagent-magnets.txt';
  document.body.appendChild(link); link.click(); link.remove();
  recordLibraryGrab(magnetSelection.size, 'export');
  setTimeout(() => URL.revokeObjectURL(url), 1000);
}
function openMagnetCollection(fromPop = false) {
  let panel = document.getElementById('libCollection');
  if (!panel) {
    panel = document.createElement('section'); panel.id = 'libCollection'; panel.className = 'lib-collection';
    panel.setAttribute('role', 'dialog'); panel.setAttribute('aria-modal', 'true'); panel.setAttribute('aria-labelledby', 'libCollectionTitle');
    panel.innerHTML = `<div class="lib-collection-head"><div><span class="lib-drawer-eyebrow">Your selection</span><h2 id="libCollectionTitle">Magnet collection · <span data-magnet-count>0</span></h2></div><button class="lib-iconbtn" onclick="closeMagnetCollection()" aria-label="Close magnet collection">×</button></div>
      <p>Each link is a separate release. Copy the list or save it for your torrent client. Your collection stays in this tab until you refresh.</p>
      <ul id="libCollectionList"></ul><div class="lib-collection-actions"><button class="lib-btn primary" data-needs-magnets onclick="copyMagnetCollection()">Copy magnets</button><button class="lib-btn" data-needs-magnets onclick="saveMagnetCollection()">Save .txt</button><button class="lib-btn" data-needs-magnets onclick="clearMagnetCollection()">Clear collection</button></div>
      <details class="lib-collection-raw"><summary>View magnet links</summary><label class="lib-sr-only" for="libCollectionText">Selected magnet links</label><textarea id="libCollectionText" readonly rows="4" spellcheck="false" onclick="this.select()"></textarea></details>`;
    document.querySelector('.lib-app').appendChild(panel);
  }
  if (panel.classList.contains('open')) return;
  panel.classList.add('open'); panel.setAttribute('aria-hidden', 'false');
  if (!fromPop) history.pushState({ lib: 'collection' }, '', window.location.href);
  openModal(panel); updateMagnetSelection();
}
function closeMagnetCollection(fromPop) {
  const panel = document.getElementById('libCollection');
  if (!panel || !panel.classList.contains('open')) return;
  if (!fromPop && history.state && history.state.lib === 'collection') { history.back(); return; }
  panel.classList.remove('open'); panel.setAttribute('aria-hidden', 'true'); closeModal(panel);
}

/* Release filters shared by movie + TV */
function chipSet(values, active, setter, allLabel) {
  if (!values.length) return '';
  const kind = {setQuality:'quality', setSource:'source', setEdition:'edition'}[setter] || 'release';
  return ['all', ...values].map(v => {
    const label = v === 'all' ? allLabel : v;
    return `<button class="lib-chip-btn ${active === v ? 'active' : ''}" aria-pressed="${active === v}" aria-label="${escAttr(`Filter ${kind}: ${label}`)}" onclick="${setter}('${escAttr(jsStringArg(v))}')">${escHtml(label)}</button>`;
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
  const active = [detail.quality, detail.source, detail.edition].filter(value => value !== 'all').length;
  return groups.length ? `<details class="lib-detail-filters" ${detail.filtersExpanded ? 'open' : ''} ontoggle="detail.filtersExpanded=this.open">
    <summary aria-label="Filter releases">Filter releases${active ? ` <span class="count">${active} active</span>` : ''}</summary>
    <div class="lib-detail-filter-body lib-release-filters">${groups.map(g => `<div class="lib-chips">${g}</div>`).join('')}</div></details>` : '';
}
function setReleaseFilter(kind, value) {
  if (!['quality', 'source', 'edition'].includes(kind)) return;
  detail[kind] = value; detail.filtersExpanded = true;
  renderDetailBody();
  // A filter click redraws the release list. Retain its semantic focus so
  // keyboard users can continue refining without returning to the page top.
  const setter = {quality:'setQuality', source:'setSource', edition:'setEdition'}[kind];
  const handler = `${setter}('${jsStringArg(value)}')`;
  const next = [...document.querySelectorAll('.lib-detail-filter-body button')]
    .find(button => button.getAttribute('onclick') === handler);
  if (next) next.focus({preventScroll:true});
}
function setQuality(q) { setReleaseFilter('quality', q); }
function setSource(s) { setReleaseFilter('source', s); }
function setEdition(e) { setReleaseFilter('edition', e); }
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
  const magnet = BitAgentLibraryTools.magnetFor(t);
  const tags = [];
  if (res) tags.push(`<span class="lib-tag res">${escHtml(res)}</span>`);
  flags.forEach(f => tags.push(`<span class="lib-tag ${f === 'HDR' ? 'hdr' : 'flag-' + f.toLowerCase()}">${f}</span>`));
  if (src) tags.push(`<span class="lib-tag src">${escHtml(src)}</span>`);
  if (t.releaseGroup) tags.push(`<span class="lib-tag grp">${escHtml(t.releaseGroup)}</span>`);
  if (langs.length) tags.push(`<span class="lib-tag lang">${escHtml(langs.slice(0, 2).join(', '))}</span>`);
  const ih = escAttr(jsStringArg(t.infoHash));
  return `<div class="lib-rel" role="group" aria-label="${escAttr(rn)}">
    <label class="lib-release-select"><input type="checkbox" data-magnet-select="${escAttr(BitAgentLibraryTools.normalizeInfoHash(t.infoHash) || '')}" ${magnetSelection.has(BitAgentLibraryTools.normalizeInfoHash(t.infoHash)) ? 'checked' : ''} ${magnet ? '' : 'disabled'} onchange="toggleMagnetSelection('${ih}')" aria-label="${escAttr('Select ' + rn)}"></label>
    <button class="lib-rel-main" aria-label="${escAttr('Open details for ' + rn)}"
    onclick="openTorrentDrawer('${ih}')"
    type="button">
      <div class="lib-rel-name">${escHtml(rn)}</div>
      <div class="lib-rel-tags">${tags.join('')}</div>
    </button>
    <div class="lib-rel-meta">
      <span class="lib-rel-size">${fmtBytes(t.size)}</span>
      <span class="lib-rel-seed" title="${t.seeders || 0} seeders / ${t.leechers || 0} leechers"><svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="3"><polyline points="18 15 12 9 6 15"/></svg>${fmtNum(t.seeders || 0)}</span>
      <button class="lib-copybtn" ${magnet ? '' : 'disabled'} title="Copy magnet link" aria-label="${escAttr('Copy magnet for ' + rn)}" onclick="copyMagnet('${escAttr(jsStringArg(magnet))}')">
        <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2"><rect x="9" y="9" width="13" height="13" rx="2"/><path d="M5 15H4a2 2 0 01-2-2V4a2 2 0 012-2h9a2 2 0 012 2v1"/></svg>
        <span>Copy</span>
      </button>
      ${magnet ? `<a class="lib-openmagnet" href="${escAttr(magnet)}" onclick="recordLibraryGrab(1,'open')" aria-label="${escAttr('Open magnet for ' + rn)}">Open ↗</a>` : ''}
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
    <div class="lib-toolbar"><div class="lib-section-head"><h2>Available releases</h2><span class="count">${rels.length}/${detail.releases.length} release${detail.releases.length !== 1 ? 's' : ''}</span></div></div>
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
  const group = detail.group, tmdbId = detail.tmdbId, cache = detail.seasonCache;
  const signal = _detailAbortController ? _detailAbortController.signal : undefined;
  // Deduplicate in-flight catalog requests for the same title and season.
  cache.set(String(s), { episodes: [] });
  const data = await api(`/api/meta/tv/${encodeURIComponent(tmdbId)}/season/${s}`, { signal });
  if ((signal && signal.aborted) || group !== detail.group || cache !== detail.seasonCache) return;
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
async function openTorrentDrawer(infoHash, fromPop = false) {
  const drawer = document.getElementById('libDrawer');
  const scrim = document.getElementById('libDrawerScrim');
  const bodyEl = document.getElementById('libDrawerBody');
  drawer.classList.add('open'); scrim.classList.add('open');
  drawer.setAttribute('aria-hidden', 'false');
  openModal(drawer);
  // Own history entry (over the detail entry) so Back closes just the drawer.
  if (!fromPop) history.pushState({ lib: 'drawer', infoHash }, '');
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

  const magnet = BitAgentLibraryTools.magnetFor(d);
  bodyEl.innerHTML = `
    <div class="lib-drawer-title">${escHtml(d.name)}</div>
    ${tags.length ? `<div class="lib-drawer-tags">${tags.join('')}</div>` : ''}
    <dl class="lib-kv">${kv.join('')}</dl>
    <div class="lib-drawer-actions">
      ${magnet ? `<a class="lib-btn primary" href="${escAttr(magnet)}" onclick="recordLibraryGrab(1,'open')">Open magnet ↗</a>` : ''}
      <button class="lib-btn" ${magnet ? '' : 'disabled'} onclick="copyMagnet('${escAttr(jsStringArg(magnet))}')"><svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2"><rect x="9" y="9" width="13" height="13" rx="2"/><path d="M5 15H4a2 2 0 01-2-2V4a2 2 0 012-2h9a2 2 0 012 2v1"/></svg>Copy</button>
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
  if (magnet) copyText(magnet, 'Magnet link copied', scope => recordLibraryGrab(1, 'copy', scope));
}
function copyText(text, message, onSuccess) {
  const scope={accountId:_accountOwner,epoch:_accountOwnerEpoch,signedIn:_accountSignedIn()};
  const done = () => { toast(message); if (onSuccess) onSuccess(scope); };
  if (navigator.clipboard && navigator.clipboard.writeText) {
    navigator.clipboard.writeText(text).then(done).catch(() => fallbackCopy(text, done));
  } else fallbackCopy(text, done);
}
function fallbackCopy(text, done) {
  const ta = document.createElement('textarea');
  ta.value = text; ta.style.position = 'fixed'; ta.style.opacity = '0';
  document.body.appendChild(ta); ta.select();
  try { if (document.execCommand('copy')) done(); else toast('Copy failed. Use Save .txt from your magnet collection.'); } catch (_) { toast('Copy failed'); }
  document.body.removeChild(ta);
}

/* ── Account preferences and measured activity ──────────────────────────── */
let accountState = null;
let _accountUsageSnapshot = null;
let _accountRequestVersion = 0;
let _accountOwner = typeof document !== 'undefined' ? (document.body.dataset.accountId || 'anonymous') : 'anonymous';
let _accountMethod = typeof document !== 'undefined' ? document.body.dataset.accountMethod : 'anonymous';
let _accountOwnerEpoch = 0;
let _accountKeyBusy = false;
let _accountConnected = false;
let _accountUsagePeriod = 'tracked';
let _accountPreferenceStore = null;
const _accountGrabQueue = [];
let _accountGrabSaving = false;
let _accountGrabStorageAvailable = true;
function _accountGrabCacheKey() { return 'bitagent-library-actions:v1:'+encodeURIComponent(_accountOwner); }
function _saveAccountGrabQueue() {
  try { window.localStorage.setItem(_accountGrabCacheKey(),JSON.stringify({schemaVersion:1,events:_accountGrabQueue.map(({epoch,...event})=>event)}));_accountGrabStorageAvailable=true; } catch (_) {_accountGrabStorageAvailable=false;}
}
function _restoreAccountGrabQueue() {
  _accountGrabQueue.length=0;
  if(!_accountSignedIn())return;
  try {
    const stored=JSON.parse(window.localStorage.getItem(_accountGrabCacheKey()) || 'null');
    if(stored?.schemaVersion!==1 || !Array.isArray(stored.events))return;
    for(const event of stored.events) if(event?.expectedAccountId===_accountOwner && Number.isSafeInteger(event.count) && event.count>=1 && event.count<=1000 && ['copy','open','export'].includes(event.action) && /^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$/i.test(event.eventId))_accountGrabQueue.push({...event,epoch:_accountOwnerEpoch});
  } catch (_) {_accountGrabStorageAvailable=false;}
}
function _accountElementText(id, text) { const el = document.getElementById(id); if (el) el.textContent = text; }
function _accountSignedIn() { return !!_accountOwner && !['anonymous','api-client'].includes(_accountOwner) && !['anonymous','open','api-key'].includes(_accountMethod); }
async function accountRequest(path, options = {}) {
  try {
    const controller=new AbortController(),timer=setTimeout(()=>controller.abort(),6000);
    try {
      const response = await fetch(path, {...options,signal:controller.signal, headers:{Accept:'application/json',...(options.headers || {})}, credentials:'same-origin'});
      return {ok:response.ok, status:response.status, data:response.ok ? await response.json() : null};
    } finally {clearTimeout(timer);}
  } catch (_) { return {ok:false,status:0,data:null}; }
}
function accountPreferenceSettings() { return _accountPreferenceStore?.get() || BitAgentPreferences.normalizeLibraryPreferences({}); }
function _applyAccountAppearance(settings) {
  const dark = settings.theme === 'dark' || (settings.theme === 'system' && window.matchMedia?.('(prefers-color-scheme: dark)').matches);
  document.documentElement.setAttribute('data-theme', dark ? 'dark' : 'light');
  document.documentElement.setAttribute('data-library-density', settings.density);
  document.documentElement.setAttribute('data-library-motion', settings.motion === 'reduced' || window.matchMedia?.('(prefers-reduced-motion: reduce)').matches ? 'reduced' : 'system');
  syncThemeColor();
  window.dispatchEvent(new CustomEvent('bitagent:preferences'));
  renderAccountPreferences();
}
function _initializeAccountPreferences() {
  _accountPreferenceStore?.dispose();
  _restoreAccountGrabQueue();
  let storage; try { storage=window.localStorage; } catch (_) {}
  const epoch = _accountOwnerEpoch;
  _accountPreferenceStore = BitAgentPreferences.createPreferenceStore({ownerId:_accountOwner,authenticated:_accountSignedIn(),storage,
    request:async(changes,ownerId)=> { if(!_accountConnected)return {ok:false}; const response=await accountRequest('/api/account/preferences',{method:'PATCH',headers:{'Content-Type':'application/json'},body:JSON.stringify({expectedAccountId:ownerId,changes})}); return epoch===_accountOwnerEpoch?response:{ok:false}; },
    onChange:_applyAccountAppearance,
    onStatus:status=> { _accountElementText('acctPrefStatus',status); const retry=document.getElementById('acctPrefRetry'); if(retry)retry.hidden=!status.includes('unavailable') && !status.includes('pending'); },
  });
  _applyAccountAppearance(_accountPreferenceStore.get());
  _accountElementText('acctPrefStatus',_accountSignedIn()?'Loading account preferences…':'Browser preferences');
}

function renderAccountPreferences() {
  const settings=accountPreferenceSettings();
  const controls={theme:'acctPrefTheme',motion:'acctPrefMotion',density:'acctPrefDensity',region:'acctPrefRegion',contentType:'acctPrefType',sort:'acctPrefSort',quality:'acctPrefQuality',matchedOnly:'acctPrefMatched',englishOnly:'acctPrefEnglish',magnetMode:'acctPrefMagnetMode'};
  for(const [key,id] of Object.entries(controls)) {
    const el=document.getElementById(id); if(!el)continue;
    if(typeof settings[key]==='boolean')el.checked=settings[key]; else {
      if(key==='region' && !Array.from(el.options || []).some(option=>option.value===settings[key])) { const option=document.createElement('option'); option.value=settings[key];option.textContent=settings[key];el.appendChild(option); }
      el.value=settings[key];
    }
  }
}
function setAccountPreference(name,value) { if(!Object.hasOwn(BitAgentPreferences.PREFERENCE_DEFAULTS,name))return; return _accountPreferenceStore?.set({[name]:value}); }
function retryAccountPreferences() { return _accountPreferenceStore?.retry(); }
function resetAccountPreferences() { return _accountPreferenceStore?.reset(); }
function openAccount(fromPop = false) {
  const panel=document.getElementById('libAccount'), scrim=document.getElementById('libAccountScrim');
  panel.classList.add('open');scrim.classList.add('open');panel.setAttribute('aria-hidden','false');openModal(panel);
  if(!fromPop)history.pushState({lib:'account'},'');
  renderAccountPreferences(); loadAccount();
}
function _clearAccountSecret() { const input=document.getElementById('acctApiSecret'),wrap=document.getElementById('acctSecretWrap'); if(input)input.value='';if(wrap)wrap.style.display='none'; }
function closeAccount(fromPop) {
  const panel=document.getElementById('libAccount'),scrim=document.getElementById('libAccountScrim');
  if(!panel.classList.contains('open'))return;
  if(!fromPop&&history.state?.lib==='account'){history.back();return;}
  panel.classList.remove('open');scrim.classList.remove('open');panel.setAttribute('aria-hidden','true');_clearAccountSecret();closeModal(panel);
}
function _clearAccountView() {
  _accountConnected=false;_accountOwnerEpoch++;_accountRequestVersion++;
  _initializeAccountPreferences();
  accountState=null;_accountUsageSnapshot=null;_clearAccountSecret();
  _accountElementText('acctAvatar','?');_accountElementText('acctName','Account unavailable');_accountElementText('acctSub','Refresh to reconnect to your account');
  _accountElementText('acctKeyStatus','Unavailable');_accountElementText('acctKeyMeta','');
  const url=document.getElementById('acctTorznabUrl');if(url)url.value='';
  for(const id of ['acctGenerateBtn','acctRevokeBtn']) {const btn=document.getElementById(id);if(btn)btn.disabled=true;}
  const scope=document.getElementById('acctPrivateAccess');if(scope){scope.checked=false;scope.disabled=true;}
  renderAccountUsage(null);
  _accountElementText('acctPrefStatus','Account unavailable. Display choices still apply to this visit.');
}
function _validAccountIdentity(identity) { return !!identity && typeof identity.id==='string' && !!identity.id.trim() && identity.id.length<=200; }
async function loadAccount(quiet = false) {
  if(_accountKeyBusy)return false;
  const version=++_accountRequestVersion,epoch=_accountOwnerEpoch;
  _accountElementText('acctUsageStatus','Refreshing…');
  const response=await accountRequest('/api/account');
  if(version!==_accountRequestVersion || epoch!==_accountOwnerEpoch)return false;
  if(!response.ok || !_validAccountIdentity(response.data?.identity)){_clearAccountView();if(!quiet)toast('Account unavailable');return false;}
  renderAccount(response.data); if(_accountGrabQueue.length)retryAccountUsage(); return true;
}
function renderAccount(data) {
  const ident=data.identity || {};
  if(!_validAccountIdentity(ident)){_clearAccountView();return false;}
  if(_accountOwner!==ident.id || _accountMethod!==ident.method) {
    _accountOwner=ident.id;_accountMethod=ident.method;_accountOwnerEpoch++;_accountUsageSnapshot=null;_accountUsagePeriod='tracked';_accountGrabQueue.length=0;_initializeAccountPreferences();
  }
  _accountConnected=true;
  accountState=data;
  if(data.preferences && _accountSignedIn()) { _accountPreferenceStore?.ingest(data.preferences); if(_accountPreferenceStore?.isPending())_accountPreferenceStore.retry(); }
  renderAccountUsage(data.usage || null);
  const name=ident.display||ident.username||ident.email||ident.id||'Account';
  _accountElementText('acctAvatar',(name[0]||'?').toUpperCase());_accountElementText('acctName',name);
  _accountElementText('acctSub',[ident.email||ident.username||ident.id,ident.method].filter(Boolean).join(' · '));
  document.getElementById('acctTorznabUrl').value=data.torznabUrl||'';
  const key=data.apiKey;
  _accountElementText('acctKeyStatus',key?`Active ${key.prefix||''} · ${key.privateAccess===true?'Public + private':'Public only'}`:'No key');_accountElementText('acctGenerateLabel',key?'Rotate':'Generate');
  document.getElementById('acctGenerateBtn').disabled=_accountKeyBusy||!_accountSignedIn();
  document.getElementById('acctRevokeBtn').disabled=_accountKeyBusy||!key||!_accountSignedIn();
  const scope=document.getElementById('acctPrivateAccess');if(scope){scope.checked=key?.privateAccess===true;scope.disabled=_accountKeyBusy||!_accountSignedIn()||data.privateKeyAvailable!==true;}
  _accountElementText('acctPrivateAccessHelp',data.privateKeyAvailable===true?'Private access is optional. Rotation revokes your existing key and tracker credentials; update indexer connections and torrents.':'Public-only keys query the public indexer. Private access is currently unavailable for this account.');
  const meta=document.getElementById('acctKeyMeta');if(meta){meta.textContent=key?[key.createdAt?'Created '+fmtDate(key.createdAt):'',key.lastUsedAt?'Last used '+fmtDate(key.lastUsedAt):'Never used'].filter(Boolean).join(' · '):'';meta.style.display=key?'':'none';}
  _clearAccountSecret();if(data.apiKeySecret){document.getElementById('acctApiSecret').value=data.apiKeySecret;document.getElementById('acctSecretWrap').style.display='';}
  return true;
}
function accountUsageViewFor(usage) { return BitAgentPreferences.accountUsageViewFor(usage); }
function _accountPeriodSnapshot(usage) {
  if(!usage || _accountUsagePeriod==='tracked')return usage;
  const period=usage.periods?.[_accountUsagePeriod==='7d'?'last7Days':'last30Days'];
  if(!period || typeof period.complete!=='boolean' || !_isUtcTimestamp(period.windowStart) || !_isUtcTimestamp(period.windowEnd) || Date.parse(period.windowStart)>Date.parse(period.windowEnd) || !Number.isFinite(period.trackingSince))return null;
  return {...usage,...period,accountId:usage.accountId,revision:usage.revision,observedAt:usage.observedAt,trackingSince:usage.trackingSince};
}
function renderAccountUsage(incoming) {
  if(incoming && incoming.accountId===_accountOwner && !accountUsageViewFor(incoming)) incoming=null;
  if(incoming) {
    const accepted=BitAgentPreferences.acceptAccountUsageSnapshot(_accountUsageSnapshot,incoming,_accountOwner);
    // Missing or malformed responses are unknown, never a remembered zero.
    if(!accepted || (accepted===_accountUsageSnapshot && incoming.accountId!==_accountOwner))return false;
    _accountUsageSnapshot=accepted;
  } else _accountUsageSnapshot=null;
  const usage=_accountPeriodSnapshot(_accountUsageSnapshot),view=accountUsageViewFor(usage);
  const ids={acctDownloaded:'downloaded',acctUploaded:'uploaded',acctGrabs:'grabs',acctHitAndRuns:'hitAndRuns',acctApiSearches:'searches',acctRatio:'ratio',acctCopies:'copies',acctOpens:'opens',acctExports:'exports'};
  for(const [id,key] of Object.entries(ids))_accountElementText(id,view?view[key]:'—');
  _accountElementText('acctUsageHint',view?view.hint:_accountSignedIn()?'Activity is temporarily unavailable. Refresh to try again.':'Sign in to view your personal activity.');
  _accountElementText('acctUsageTracked',view?'Tracking since '+fmtDate(usage.trackingSince):'Tracking unavailable');
  _accountElementText('acctUsageUpdated',view?'Updated '+new Date(usage.observedAt).toLocaleTimeString('en-US',{hour:'numeric',minute:'2-digit'}):'');
  _accountElementText('acctPrefScope',_accountSignedIn()?'Your choices follow your account across devices':'Browser preferences · sign in to sync across devices');
  const period=_accountUsagePeriod==='tracked'?null:_accountUsageSnapshot?.periods?.[_accountUsagePeriod==='7d'?'last7Days':'last30Days'];
  if(view && period) { const utcDate=value=>new Date(value).toLocaleDateString('en-US',{month:'short',day:'numeric',timeZone:'UTC'}); _accountElementText('acctUsageTracked',utcDate(period.windowStart)+' – '+utcDate(period.windowEnd)+' · UTC calendar days'); }
  _accountElementText('acctUsageStatus',_accountGrabQueue.length?'Activity sync pending · '+_accountGrabQueue.length+' action'+(_accountGrabQueue.length===1?'':'s')+(_accountGrabStorageAvailable?'':' · for this visit only'):view?(period&&!period.complete?'Partial window · tracking began '+fmtDate(period.trackingSince):'Synced at last refresh'):'Unavailable');
  const periods=document.getElementById('acctUsagePeriods');if(periods){periods.hidden=!_accountUsageSnapshot?.periods;periods.querySelectorAll('[data-period]').forEach(btn=>btn.setAttribute('aria-pressed',String(btn.dataset.period===_accountUsagePeriod)));}
  const retry=document.getElementById('acctUsageRetry');if(retry)retry.hidden=!_accountGrabQueue.length;
  document.getElementById('acctUsage')?.setAttribute('aria-busy','false');return !!view;
}
function setAccountUsagePeriod(period) { if(!['tracked','7d','30d'].includes(period))return;_accountUsagePeriod=period;renderAccountUsage(_accountUsageSnapshot); }
function refreshAccountUsage() { return loadAccount(); }
function _accountEventId() {
  if(globalThis.crypto?.randomUUID)return globalThis.crypto.randomUUID();
  if(!globalThis.crypto?.getRandomValues)return null;
  const bytes=globalThis.crypto.getRandomValues(new Uint8Array(16));bytes[6]=(bytes[6]&15)|64;bytes[8]=(bytes[8]&63)|128;
  return Array.from(bytes,(b,i)=>([4,6,8,10].includes(i)?'-':'')+b.toString(16).padStart(2,'0')).join('');
}
async function recordLibraryGrab(count,action,scope) {
  if(!Number.isSafeInteger(count) || count<1 || count>1000 || !['copy','open','export'].includes(action))return;
  if(scope && (!scope.signedIn || scope.accountId!==_accountOwner || scope.epoch!==_accountOwnerEpoch)) {
    // A delayed clipboard completion belongs to the account that initiated it.
    if(!scope.signedIn)return;
    const eventId=_accountEventId();if(!eventId)return;
    try {
      const key='bitagent-library-actions:v1:'+encodeURIComponent(scope.accountId);
      const stored=JSON.parse(window.localStorage.getItem(key) || 'null');
      const events=stored?.schemaVersion===1 && Array.isArray(stored.events)?stored.events:[];
      events.push({count,action,eventId,expectedAccountId:scope.accountId});
      window.localStorage.setItem(key,JSON.stringify({schemaVersion:1,events}));
      if(scope.accountId===_accountOwner) {_restoreAccountGrabQueue();renderAccountUsage(_accountUsageSnapshot);if(_accountConnected)retryAccountUsage();}
    } catch (_) {toast('Link copied; previous account activity could not be saved');}
    return;
  }
  if(!_accountSignedIn())return;
  const eventId=_accountEventId();if(!eventId){_accountElementText('acctUsageStatus','Activity could not be synced');return;}
  _accountGrabQueue.push({count,action,eventId,expectedAccountId:_accountOwner,epoch:_accountOwnerEpoch});
  _saveAccountGrabQueue();
  renderAccountUsage(_accountUsageSnapshot);return retryAccountUsage();
}
async function retryAccountUsage() {
  if(_accountGrabSaving || !_accountConnected)return;
  _accountGrabSaving=true;
  try {
    while(_accountGrabQueue.length && _accountConnected) {
      const event=_accountGrabQueue[0];
      if(event.expectedAccountId!==_accountOwner || event.epoch!==_accountOwnerEpoch){_accountGrabQueue.shift();_saveAccountGrabQueue();continue;}
      const {epoch,...body}=event;
      const response=await accountRequest('/api/account/usage/grab',{method:'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify(body)});
      if(epoch!==_accountOwnerEpoch)continue;
      if(!response.ok || !accountUsageViewFor(response.data) || response.data.accountId!==event.expectedAccountId || response.data.receipt?.eventId!==event.eventId || response.data.receipt?.recorded!==true){_accountElementText('acctUsageStatus','Activity sync unavailable. Retry to record completed actions.'+(_accountGrabStorageAvailable?'':' Pending actions are kept for this visit only.'));const retry=document.getElementById('acctUsageRetry');if(retry)retry.hidden=false;return;}
      _accountGrabQueue.shift();_saveAccountGrabQueue();renderAccountUsage(response.data);
    }
  } finally {_accountGrabSaving=false;}
}
async function _mutateAccountKey(method) {
  if(_accountKeyBusy || !_accountSignedIn())return;
  const privateAccess=method==='POST'&&accountState?.privateKeyAvailable===true&&document.getElementById('acctPrivateAccess')?.checked===true;
  if(method==='POST'&&(accountState?.apiKey||privateAccess)&&!window.confirm('Create a '+(privateAccess?'private-enabled':'public-only')+' replacement key? This revokes your existing key and tracker credentials. Update Prowlarr, Jackett and other indexer connections and torrents.'))return;
  _accountKeyBusy=true;const version=++_accountRequestVersion,epoch=_accountOwnerEpoch,owner=_accountOwner;
  for(const id of ['acctGenerateBtn','acctRevokeBtn','acctPrivateAccess'])document.getElementById(id).disabled=true;
  const path='/api/account/api-key'+(method==='DELETE'?'?expectedAccountId='+encodeURIComponent(owner):'');
  const response=await accountRequest(path,method==='POST'?{method,headers:{'Content-Type':'application/json'},body:JSON.stringify({name:'default',expectedAccountId:owner,privateAccess})}:{method});
  _accountKeyBusy=false;
  if(epoch!==_accountOwnerEpoch || version!==_accountRequestVersion){ if(accountState) {document.getElementById('acctGenerateBtn').disabled=!_accountSignedIn();document.getElementById('acctRevokeBtn').disabled=!_accountSignedIn()||!accountState.apiKey;document.getElementById('acctPrivateAccess').disabled=!_accountSignedIn()||accountState.privateKeyAvailable!==true;}return;}
  if(!response.ok || response.data?.identity?.id!==owner){_clearAccountView();toast(method==='POST'?'Key update failed':'Revoke failed');return;}
  renderAccount(response.data);toast(method==='POST'?'API key ready':'API key revoked');
}
function generateAccountKey() {return _mutateAccountKey('POST');}
function revokeAccountKey() {return _mutateAccountKey('DELETE');}
function copyAccountField(id) {const el=document.getElementById(id);if(!el?.value)return;copyText(el.value,'Copied');}
if(typeof document!=='undefined') {
  _initializeAccountPreferences();
  for(const query of ['(prefers-color-scheme: dark)','(prefers-reduced-motion: reduce)'])window.matchMedia?.(query).addEventListener('change',()=>_applyAccountAppearance(accountPreferenceSettings()));
}

/* ── Global keys + history ──────────────────────────────────────────────── */
if (typeof document !== 'undefined') {
  document.addEventListener('keydown', e => {
    if (e.key === 'Tab' && _modalStack.length) {
      const panel = _modalStack[_modalStack.length - 1].panel;
      const focusable = [...panel.querySelectorAll('button:not(:disabled), [href], input:not(:disabled), select:not(:disabled), textarea:not(:disabled), summary, [tabindex="0"]')]
        .filter(el => el.getClientRects().length && !el.closest('[inert]'));
      if (focusable.length && ((e.shiftKey && document.activeElement === focusable[0]) || (!e.shiftKey && document.activeElement === focusable[focusable.length - 1]))) {
        e.preventDefault(); (e.shiftKey ? focusable[focusable.length - 1] : focusable[0]).focus();
      }
    }
    if (e.key === 'Escape') {
      if (document.getElementById('libCollection')?.classList.contains('open')) { closeMagnetCollection(); return; }
      if (document.getElementById('libAccount').classList.contains('open')) { closeAccount(); return; }
      if (document.getElementById('libDrawer').classList.contains('open')) { closeTorrentDrawer(); return; }
      if (document.getElementById('libDetail').classList.contains('open')) { closeDetail(); return; }
    }
    // "/" focuses search when not already typing
    if (e.key === '/' && !/^(input|textarea|select)$/i.test((e.target.tagName || ''))) {
      e.preventDefault(); document.getElementById('libSearch').focus();
    }
  });
  window.addEventListener('popstate', event => restoreLibraryHistory(event.state));
}

function restoreLibraryHistory(destination) {
  const target = destination && destination.lib;
  const isOpen = id => document.getElementById(id)?.classList.contains('open');
  if (target !== 'collection') closeMagnetCollection(true);
  if (target !== 'account') closeAccount(true);
  if (target !== 'drawer') closeTorrentDrawer(true);
  if (target === 'collection') { if (!isOpen('libCollection')) openMagnetCollection(true); return; }
  if (target === 'account') { if (!isOpen('libAccount')) openAccount(true); return; }
  if (target === 'drawer') { if (!isOpen('libDrawer')) openTorrentDrawer(destination.infoHash, true); return; }
  if (target === 'detail') {
    if (detail.group && detail.group.key === destination.key && isOpen('libDetail')) return;
    closeDetail(true);
    if (groupCache.has(destination.key)) openDetail(destination.key);
    else { const permalink = _readPermalink(); if (permalink) resolvePermalink(permalink); }
    return;
  }
  closeDetail(true);
  applyStateFromUrl();
  loadLibrary({ fromPop: true });
}

/* When the active season changes we also want its TMDB episode catalog. The
   first season's catalog is fetched right after the detail body first paints. */
const _origRenderDetailBody = renderDetailBody;
renderDetailBody = function () { _origRenderDetailBody(); if (detail.ct === 'tv_show') maybeLoadSeasonMeta(); };

/* ── Boot ───────────────────────────────────────────────────────────────── */
if (typeof document !== 'undefined') {
  (async () => {
  _applyModalInert();
  await loadAccount(true);
  applyStateFromUrl(true);
  const _bootPermalink = _readPermalink();
  // Establish a clean browse entry as the history base (strips any title params),
  // then open the shared title (if any) as a pushed entry on top so Back returns
  // to the browse view rather than leaving the site.
  history.replaceState({ lib: 'browse' }, '', browseUrl());
  renderTypePills();
  if (document.getElementById('libStatsGrid')) loadLibraryStats();
  loadLibrary({ fromPop: true });
  if (_bootPermalink) resolvePermalink(_bootPermalink);
  })();
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
    accountUsageViewFor,
    loadAccount, renderAccount, renderAccountUsage, recordLibraryGrab, retryAccountUsage, generateAccountKey, revokeAccountKey,
  };
}

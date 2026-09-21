/* ── BitAgent Dashboard — app.js ─────────────────────────────────────── */

const API = '';
let currentTab = 'dashboard';
let libOffset = 0, libLimit = 50, libTotal = 0, libView = 'grid';
let libActiveType = 'movie';
let libSort = 'seeders';
// Library filter state (was checkbox-backed; now toggle pills)
let libHideUnmatched = false, libHideForeign = true;
// Grouped-library state — populated by loadLibrary(), paged by libPage()
let _allGroups = [];
let _groupPage = 0;
const _groupPageSize = 24;
let evOffset = 0, evLimit = 50;
let _dashboardLoadSeq = 0;
let _aiLoadSeq = 0;

/* ── Theme ────────────────────────────────────────────────────────────── */
function syncThemeColor() {
  // Keep the browser/OS chrome (Android title bar, iOS standalone status bar)
  // matched to the active theme. Runs after the stylesheet applies, so read
  // the resolved token rather than hardcoding the two palettes here.
  const meta = document.querySelector('meta[name="theme-color"]');
  const bg = getComputedStyle(document.documentElement).getPropertyValue('--color-bg').trim();
  if (meta && bg) meta.setAttribute('content', bg);
}
function initTheme() {
  const saved = localStorage.getItem('bitagent-theme');
  if (saved === 'dark' || (!saved && window.matchMedia('(prefers-color-scheme: dark)').matches)) {
    document.documentElement.setAttribute('data-theme', 'dark');
  }
  // Stylesheets may not have applied yet at script-eval time; defer one frame.
  requestAnimationFrame(syncThemeColor);
}
let _themeAnimTimer = null;
function _syncThemeSwitchAria() {
  const sw = document.getElementById('themeToggleSwitch');
  if (sw) sw.setAttribute('aria-checked', String(document.documentElement.getAttribute('data-theme') === 'dark'));
}
function toggleTheme() {
  const isDark = document.documentElement.getAttribute('data-theme') === 'dark';
  // Enable the one-shot coordinated colour transition (see .theme-anim in CSS),
  // unless the user prefers reduced motion.
  const reduce = window.matchMedia('(prefers-reduced-motion: reduce)').matches;
  if (!reduce) {
    document.documentElement.classList.add('theme-anim');
    clearTimeout(_themeAnimTimer);
    _themeAnimTimer = setTimeout(() => document.documentElement.classList.remove('theme-anim'), 250);
  }
  document.documentElement.setAttribute('data-theme', isDark ? 'light' : 'dark');
  localStorage.setItem('bitagent-theme', isDark ? 'light' : 'dark');
  syncThemeColor();
  _syncThemeSwitchAria();
}
initTheme();
document.addEventListener('DOMContentLoaded', _syncThemeSwitchAria);

/* ── Sidebar drawer (mobile) ─────────────────────────────────────────── */
function setSidebarOpen(open) {
  document.getElementById('sidebar').classList.toggle('open', open);
  document.getElementById('sidebarBackdrop').classList.toggle('visible', open);
}
function toggleSidebar() {
  setSidebarOpen(!document.getElementById('sidebar').classList.contains('open'));
}
function closeSidebar() { setSidebarOpen(false); }

/* ── Navigation ───────────────────────────────────────────────────────── */
const TAB_META = {
  dashboard:  { title: 'Dashboard',  subtitle: 'System overview and live metrics' },
  library:    { title: 'Library',    subtitle: 'Search the indexed catalog — press / to jump to search' },
  wants:      { title: 'Wants',      subtitle: 'Operator-defined search targets' },
  evidence:   { title: 'Evidence',   subtitle: 'Webhook events from *arr applications' },
  quarantine: { title: 'Quarantine', subtitle: 'Junk-classifier removals — review, restore, or purge' },
  ai:         { title: 'AI',         subtitle: 'Matcher, content filter and junk-purge judge — throughput, latency and quality' },
  settings:   { title: 'Settings',   subtitle: 'Configuration, auth, and integrations' },
  system:     { title: 'System',     subtitle: 'Health checks, diagnostics, and tools' },
};

function switchTab(tab) {
  currentTab = tab;
  document.querySelectorAll('.nav-item').forEach(n => n.classList.toggle('active', n.dataset.tab === tab));
  document.querySelectorAll('.page-body > .tab-panel').forEach(p => p.classList.toggle('active', p.id === `tab-${tab}`));
  const meta = TAB_META[tab] || {};
  document.getElementById('pageTitle').textContent = meta.title || tab;
  document.getElementById('pageSubtitle').textContent = meta.subtitle || '';
  closeSidebar();
  refreshCurrentTab();
}

function refreshCurrentTab() {
  const loaders = { dashboard: loadDashboard, library: loadLibrary, wants: loadWants, evidence: loadEvidence, quarantine: loadQuarantine, ai: loadAiTab, settings: loadSettings, system: loadSystem };
  (loaders[currentTab] || (() => {}))();
}

/* ── Utility ──────────────────────────────────────────────────────────── */
const API_TIMEOUT_MS = 15_000;

async function api(path, opts = {}) {
  const {
    timeoutMs = API_TIMEOUT_MS,
    headers = {},
    signal: callerSignal,
    ...requestOpts
  } = opts;
  const controller = new AbortController();
  const abortFromCaller = () => controller.abort();
  if (callerSignal) {
    if (callerSignal.aborted) controller.abort();
    else callerSignal.addEventListener('abort', abortFromCaller, { once: true });
  }
  const timeoutId = timeoutMs > 0
    ? setTimeout(() => controller.abort(), timeoutMs)
    : null;
  try {
    const resp = await fetch(API + path, {
      ...requestOpts,
      headers: { 'Content-Type': 'application/json', ...headers },
      signal: controller.signal,
    });
    if (!resp.ok) throw new Error(`${resp.status} ${resp.statusText}`);
    return await resp.json();
  } catch (e) {
    const reason = e && e.name === 'AbortError'
      ? `timed out after ${timeoutMs}ms`
      : e;
    console.error(`API ${path}:`, reason);
    return null;
  } finally {
    if (timeoutId !== null) clearTimeout(timeoutId);
    if (callerSignal) callerSignal.removeEventListener('abort', abortFromCaller);
  }
}

function debounce(fn, ms) {
  let t;
  const wrapped = (...a) => { clearTimeout(t); t = setTimeout(() => fn(...a), ms); };
  wrapped.cancel = () => clearTimeout(t);
  return wrapped;
}
const debouncedLoadLibrary = debounce(loadLibrary, 250);

function fmtNum(n) {
  if (n == null || isNaN(n)) return '--';
  return Number(n).toLocaleString();
}
// For counts the core answers from a planner estimate. Rendering
// "2,311,600" implied a census; "~2.31M" is what the number actually knows.
function fmtApproxNum(n) {
  if (n == null || isNaN(n)) return '--';
  const value = Number(n);
  if (Math.abs(value) < 1000) return `~${Math.round(value)}`;
  const units = [[1e9, 'B'], [1e6, 'M'], [1e3, 'K']];
  for (const [scale, suffix] of units) {
    if (Math.abs(value) >= scale) {
      const scaled = value / scale;
      return `~${scaled.toFixed(scaled < 10 ? 2 : scaled < 100 ? 1 : 0)}${suffix}`;
    }
  }
  return `~${Math.round(value)}`;
}
function fmtRatePerMin(n) {
  if (n == null || isNaN(n)) return '--';
  const value = Number(n);
  if (value > 0 && value < 1) return '<1';
  return value.toLocaleString(undefined, { maximumFractionDigits: 2 });
}
function fmtBytes(b) {
  if (!b) return '--';
  const u = ['B', 'KB', 'MB', 'GB', 'TB'];
  let i = 0;
  while (b >= 1024 && i < u.length - 1) { b /= 1024; i++; }
  return `${b.toFixed(1)} ${u[i]}`;
}
function fmtTime(secs) {
  if (!secs) return '--';
  const d = Math.floor(secs / 86400), h = Math.floor((secs % 86400) / 3600), m = Math.floor((secs % 3600) / 60);
  if (d > 0) return `${d}d ${h}h`;
  if (h > 0) return `${h}h ${m}m`;
  return `${m}m`;
}
function fmtDate(ts) {
  if (!ts) return '--';
  const d = typeof ts === 'number' ? new Date(ts * 1000) : new Date(ts);
  return d.toLocaleDateString('en-US', { month: 'short', day: 'numeric', hour: '2-digit', minute: '2-digit' });
}
function fmtAgo(ts) {
  if (!ts) return '--';
  const d = typeof ts === 'number' ? new Date(ts * 1000) : new Date(ts);
  const diff = (Date.now() - d.getTime()) / 1000;
  if (diff < 60) return 'just now';
  if (diff < 3600) return `${Math.floor(diff / 60)}m ago`;
  if (diff < 86400) return `${Math.floor(diff / 3600)}h ago`;
  return `${Math.floor(diff / 86400)}d ago`;
}
// Escapes for BOTH text nodes and quoted-attribute contexts. The textContent
// round-trip only encodes & < >, leaving " and ' intact — which is unsafe the
// moment the result lands inside title="…", value="…", or a single-quoted
// onclick('…'). Torrent/content names are attacker-controlled DHT input, so an
// unescaped quote is either dead UI (apostrophe closes an onclick string) or
// attribute-injection XSS (double quote breaks out of title="…"). Escaping all
// five makes the output safe in every context this codebase interpolates into.
function escHtml(s) {
  return String(s == null ? '' : s)
    .replace(/&/g, '&amp;')
    .replace(/</g, '&lt;')
    .replace(/>/g, '&gt;')
    .replace(/"/g, '&quot;')
    .replace(/'/g, '&#39;');
}
// For a value interpolated into a single-quoted JS string that itself lives
// inside a double-quoted HTML attribute — e.g. onclick="fn('${...}')". Two
// nested contexts: first backslash-escape for the JS string body (\ and '),
// THEN HTML-escape for the "…" attribute. HTML-entity escaping alone is NOT
// enough here: &#39; decodes back to ' before the JS parser runs, so an
// apostrophe still breaks the handler (dead poster cards / copy-magnet buttons).
function escAttrArg(s) {
  return escHtml(String(s == null ? '' : s).replace(/\\/g, '\\\\').replace(/'/g, "\\'"));
}
// Humanized, honestly-colored pill for an evidence event *kind* (qB state
// observation, *arr grab/import/failure, …). Replaces the old resultPill,
// which coloured every row green off a backend-hardcoded result:"success".
function eventPill(kind) {
  if (!kind) return '<span class="pill pill-neutral">--</span>';
  const k = String(kind).toLowerCase();
  const label = String(kind).replace(/[_-]+/g, ' ').replace(/\b\w/g, c => c.toUpperCase());
  let cls = 'pill-info';
  if (/fail|error|dead|reject|stall/.test(k)) cls = 'pill-danger';
  else if (/grab|import|complet|success|download|alive|seed/.test(k)) cls = 'pill-success';
  else if (/observ|poll|state|pending|queue/.test(k)) cls = 'pill-neutral';
  return `<span class="pill ${cls}"><span class="pill-dot"></span>${escHtml(label)}</span>`;
}
// GraphQL VideoResolution enum values carry a leading V ("V1080p") that
// shouldn't reach the screen. Single formatter for every quality label.
function fmtRes(r) {
  return r ? String(r).replace(/^v/i, '') : '';
}
// api() resolves null on ANY failure (401/500/network). Loaders must render
// this — a failed fetch is not an empty dataset (honest-unknown rule).
function loadFailedRow(colspan) {
  return `<tr><td colspan="${colspan}" class="text-sm" style="text-align:center;padding:var(--space-8);color:var(--color-danger)">Couldn't load — the request failed. Check the connection and auth, then refresh.</td></tr>`;
}
function typePill(type) {
  const colors = { movie: 'pill-info', tv_show: 'pill-accent', music: 'pill-success', ebook: 'pill-warning', software: 'pill-neutral', unknown: 'pill-neutral' };
  const labels = { movie: 'Movie', tv_show: 'TV Show', music: 'Music', ebook: 'eBook', software: 'Software', unknown: 'Unknown' };
  return `<span class="pill ${colors[type] || 'pill-neutral'}">${escHtml(labels[type] || type || 'unknown')}</span>`;
}
function toast(message, type = 'info') {
  const container = document.getElementById('toasts');
  const icons = {
    success: '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2"><path d="M22 11.08V12a10 10 0 11-5.93-9.14"/><polyline points="22 4 12 14.01 9 11.01"/></svg>',
    error: '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2"><circle cx="12" cy="12" r="10"/><line x1="15" y1="9" x2="9" y2="15"/><line x1="9" y1="9" x2="15" y2="15"/></svg>',
    info: '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2"><circle cx="12" cy="12" r="10"/><line x1="12" y1="16" x2="12" y2="12"/><line x1="12" y1="8" x2="12.01" y2="8"/></svg>',
  };
  const el = document.createElement('div');
  el.className = `toast toast-${type}`;
  el.innerHTML = `<span class="toast-icon">${icons[type] || icons.info}</span><span>${escHtml(message)}</span>`;
  container.appendChild(el);
  // Exit: add .toast-leaving so the CSS transition (opacity + slide) runs, then
  // remove after it finishes. The 350ms matches --duration-slow.
  setTimeout(() => {
    el.classList.add('toast-leaving');
    setTimeout(() => el.remove(), 350);
  }, 4000);
}

/* ── Dashboard ────────────────────────────────────────────────────────── */
function _setStatusPill(id, ok, okLabel, badLabel) {
  const el = document.getElementById(id);
  if (!el) return;
  el.className = `pill ${ok ? 'pill-success' : 'pill-danger'}`;
  el.innerHTML = `<span class="pill-dot"></span> ${ok ? okLabel : badLabel}`;
}

function _metricEnvelope(stats, name) {
  const metrics = stats && stats.metrics;
  return metrics && Object.prototype.hasOwnProperty.call(metrics, name) ? metrics[name] : null;
}

function _metricValue(stats, name) {
  const metric = _metricEnvelope(stats, name);
  return metric && metric.value !== null && metric.value !== undefined ? metric.value : null;
}

const _REDUCED_MOTION = window.matchMedia
  ? window.matchMedia('(prefers-reduced-motion: reduce)')
  : { matches: false };
const _countUpTimers = new WeakMap();

// Count numbers up to their new value instead of snapping. Keyed on the
// element so a 30s poll landing mid-animation retargets the running tween
// rather than racing a second one against it.
// Stop any in-flight tween on this element. Deleting the map entry alone
// leaves the scheduled frame queued, and its next callback writes the stale
// numeric value straight over whatever replaced it — so "measuring…" or "—"
// flickers back to the old number for one frame.
function _cancelCountUp(el) {
  const prev = _countUpTimers.get(el);
  if (prev) cancelAnimationFrame(prev.raf);
  _countUpTimers.delete(el);
}

function _countUp(el, to, formatter) {
  const prev = _countUpTimers.get(el);
  if (prev) cancelAnimationFrame(prev.raf);
  const from = prev ? prev.current : (Number(el.dataset.countValue) || 0);
  if (_REDUCED_MOTION.matches || from === to || !isFinite(from)) {
    _countUpTimers.delete(el);
    el.dataset.countValue = String(to);
    el.textContent = formatter(to);
    return;
  }
  const start = performance.now();
  const duration = 650;
  const state = { current: from, raf: 0 };
  const step = now => {
    // easeOutCubic: fast commit, soft landing — reads as "settled", not "still loading".
    const t = Math.min((now - start) / duration, 1);
    const eased = 1 - Math.pow(1 - t, 3);
    state.current = from + (to - from) * eased;
    el.textContent = formatter(state.current);
    if (t < 1) {
      state.raf = requestAnimationFrame(step);
    } else {
      _countUpTimers.delete(el);
      el.dataset.countValue = String(to);
      el.textContent = formatter(to);
    }
  };
  state.raf = requestAnimationFrame(step);
  _countUpTimers.set(el, state);
}

// A measured rate whose in-process baseline is still filling is `partial`, not
// `unavailable`: it resolves itself on the next poll. Rendering both as an
// em-dash made every fresh page load look like a broken dashboard.
function _renderMetric(id, stats, name, formatter = fmtNum, opts = {}) {
  const el = document.getElementById(id);
  if (!el) return;
  const metric = _metricEnvelope(stats, name);
  const value = _metricValue(stats, name);
  if (value === null) {
    _cancelCountUp(el);
    delete el.dataset.countValue;
    el.textContent = metric && metric.status === 'partial' ? 'measuring…' : '—';
    el.classList.toggle('stat-value--pending', !!metric && metric.status === 'partial');
  } else {
    el.classList.remove('stat-value--pending');
    if (opts.animate !== false && typeof value === 'number' && isFinite(value)) {
      _countUp(el, value, formatter);
    } else {
      el.textContent = formatter(value);
    }
  }
  if (!metric) {
    el.removeAttribute('title');
    return;
  }
  const detail = [metric.source, metric.observed_at];
  if (metric.stale) detail.push('stale');
  if (metric.error) detail.push(metric.error);
  el.title = detail.filter(Boolean).join(' · ');
}

// Blank a group of stat cards. A headline reset to "—" beside its previous
// sub-label is the same lie as a filled meter beside an em-dash: the detail
// line reads as current when it describes a poll that already failed. Pass
// every element the SUCCESS path writes, so the two stay in step.
function _blankStats(pairs) {
  Object.entries(pairs).forEach(([id, text]) => {
    const el = document.getElementById(id);
    if (el) el.textContent = text;
  });
}

// Fill a .stat-meter bar. `tone` picks the colour band; null ratio empties it.
function _renderMeter(id, ratio, tone) {
  const el = document.getElementById(id);
  if (!el) return;
  const pct = ratio === null || ratio === undefined || isNaN(ratio)
    ? 0 : Math.max(0, Math.min(1, ratio)) * 100;
  el.style.width = `${pct.toFixed(1)}%`;
  el.dataset.tone = tone || 'neutral';
}

// Ratio thresholds where higher is better. Kept in one place so the dashboard
// and the LLM scorecards agree on what "good" looks like.
function _toneForRatio(ratio, good, bad) {
  if (ratio === null || ratio === undefined || isNaN(ratio)) return 'neutral';
  return ratio >= good ? 'good' : ratio < bad ? 'bad' : 'warn';
}

function _probeIsHealthy(stats, name) {
  const metric = _metricEnvelope(stats, name);
  return !!metric && metric.status === 'ok' && metric.value === true && !metric.stale;
}

function _setProbePill(id, stats, name, okLabel, badLabel) {
  const ok = _probeIsHealthy(stats, name);
  _setStatusPill(id, ok, okLabel, badLabel);
  const el = document.getElementById(id);
  const metric = _metricEnvelope(stats, name);
  if (el && metric) {
    el.title = [metric.source, metric.observed_at, metric.error].filter(Boolean).join(' · ');
  } else if (el) {
    el.removeAttribute('title');
  }
}

// (The dashboard's own health-pill painter is gone with the System Health
// card. Per-endpoint reachability is painted by loadSystemTab against the
// sysGql/sysMetrics/sysDb pills, which is the surface built for it.)

async function loadDashboard() {
  const loadSeq = ++_dashboardLoadSeq;
  const stats = await api('/api/stats');
  if (loadSeq !== _dashboardLoadSeq) return;
  // The core answers totalCount from the Postgres planner's estimate once the
  // table outgrows an exact count, so render it rounded rather than to the
  // last digit. Anything that claims precision it does not have is a lie the
  // operator has no way to catch.
  const torrentsEstimated = _metricValue(stats, 'totalTorrentsIsEstimate') === true;
  const fmtTorrents = torrentsEstimated ? fmtApproxNum : fmtNum;
  _renderMetric('statTorrents', stats, 'totalTorrents', fmtTorrents);
  _renderMetric('libCount', stats, 'totalTorrents', fmtTorrents, { animate: false });
  _renderMetric('statEvidence', stats, 'totalEvidence');
  _renderMetric('statThroughput', stats, 'indexerThroughput', fmtRatePerMin);
  _renderMetric('statBlacklist', stats, 'livenessBlacklistSize');
  _renderMetric('statUptime', stats, 'uptimeSeconds', fmtTime, { animate: false });
  _renderMetric('statGrabSuccess', stats, 'grabSuccessRate', value => `${(value * 100).toFixed(1)}%`);
  _renderMetric('statMatchRate', stats, 'matchRate30d', value => `${(value * 100).toFixed(1)}%`);
  if (stats) {
    const torrentsSub = document.getElementById('statTorrentsSub');
    if (torrentsSub) torrentsSub.textContent = torrentsEstimated
      ? 'catalogue size · planner estimate' : 'catalogue size';

    const throughput = _metricValue(stats, 'indexerThroughput');
    const pulse = document.getElementById('statThroughputPulse');
    if (pulse) pulse.hidden = !(throughput > 0);

    // Grab success % (from the core's dashstats gauges). Pending attempts are
    // deliberately outside the ratio — an unresolved grab is neither outcome —
    // so surface the count instead of silently dropping it.
    const grabSuccess = _metricValue(stats, 'grabSuccessCount');
    const grabFailure = _metricValue(stats, 'grabFailureCount');
    const grabPending = _metricValue(stats, 'grabPendingCount');
    const grabSub = document.getElementById('statGrabSub');
    // `grabPending !== null`, not truthiness: a real zero is the answer an
    // operator wants confirmed ("nothing is stuck waiting"), and dropping it
    // makes zero indistinguishable from a core that never reported it — the
    // exact conflation this card was renamed to stop making.
    if (grabSub) grabSub.textContent = grabSuccess !== null && grabFailure !== null
      ? `${fmtNum(grabSuccess)} live / ${fmtNum(grabFailure)} dead`
        + (grabPending !== null ? ` · ${fmtNum(grabPending)} pending` : '')
      : 'unavailable';
    const grabRate = _metricValue(stats, 'grabSuccessRate');
    _renderMeter('statGrabMeter', grabRate, _toneForRatio(grabRate, 0.7, 0.45));

    const matchMatched = _metricValue(stats, 'matchMatched30d');
    const matchTotal = _metricValue(stats, 'matchTotal30d');
    const matchSub = document.getElementById('statMatchSub');
    if (matchSub) matchSub.textContent = matchMatched !== null && matchTotal !== null
      ? `${fmtNum(matchMatched)} of ${fmtNum(matchTotal)} movie/TV` : 'unavailable';
    const matchRate = _metricValue(stats, 'matchRate30d');
    _renderMeter('statMatchMeter', matchRate, _toneForRatio(matchRate, 0.8, 0.6));

    // Lifetime exclusions, not a per-minute rate. The excluded counter moves a
    // few times an hour, so a /min figure on it was pure rounding noise.
    const excluded = _metricValue(stats, 'livenessTotalExcluded');
    const blSub = document.getElementById('statBlacklistSub');
    if (blSub) blSub.textContent = excluded !== null
      ? `${fmtNum(excluded)} search results filtered` : 'currently blocked';

    const chart = document.getElementById('categoryChart');
    const categoryBreakdown = _metricValue(stats, 'categoryBreakdown');
    if (Array.isArray(categoryBreakdown) && categoryBreakdown.length > 0) {
      // Sort largest first so the bar lengths form a clean visual descent
      const breakdown = [...categoryBreakdown].sort((a, b) => b.count - a.count);
      const total = breakdown.reduce((s, c) => s + c.count, 0);
      // Bars are scaled relative to the largest slice (not the total) so even
      // small categories get a visible bar; the percent column carries the
      // share-of-total reading.
      const max = Math.max(...breakdown.map(c => c.count), 1);
      chart.innerHTML = breakdown.map(c => {
        const pct = total > 0 ? ((c.count / total) * 100).toFixed(1) : '0.0';
        const barW = ((c.count / max) * 100).toFixed(1);
        const meta = categoryMeta(c.category);
        return `<div class="cat-row">
          <div class="cat-icon" style="color:${meta.color}">${meta.svg}</div>
          <div class="cat-label">${escHtml(meta.label)}</div>
          <div class="cat-bar"><div class="cat-bar-fill" style="width:${barW}%;background:${meta.color}"></div></div>
          <div class="cat-count">${fmtNum(c.count)}</div>
          <div class="cat-pct">${pct}%</div>
        </div>`;
      }).join('');
    } else if (Array.isArray(categoryBreakdown)) {
      chart.innerHTML = '<div class="empty-state" style="padding:var(--space-8)"><p class="text-sm text-muted">No indexed categories reported</p></div>';
    } else {
      // Empty breakdown on a refresh must clear the prior bars, not leave them
      // stale (the `if` had no else, so a core hiccup froze the last good chart).
      chart.innerHTML = '<div class="empty-state" style="padding:var(--space-8)"><p class="text-sm text-muted">Category metrics unavailable</p></div>';
    }
  } else {
    // A failed poll must clear everything the previous SUCCESSFUL poll drew,
    // not just the headline numbers. Leaving the meters filled and the
    // throughput pulse animating next to a row of em-dashes reads as "the
    // crawler is fine, the numbers are just loading" — the crawler could be
    // down. Every element written inside `if (stats)` is reset here.
    const chart = document.getElementById('categoryChart');
    const pulse = document.getElementById('statThroughputPulse');
    if (pulse) pulse.hidden = true;
    _renderMeter('statGrabMeter', null, 'neutral');
    _renderMeter('statMatchMeter', null, 'neutral');
    [
      ['statTorrentsSub', 'unavailable'],
      ['statGrabSub', 'unavailable'],
      ['statMatchSub', 'unavailable'],
      ['statBlacklistSub', 'unavailable'],
    ].forEach(([id, text]) => {
      const el = document.getElementById(id);
      if (el) el.textContent = text;
    });
    if (chart) chart.innerHTML = '<div class="empty-state" style="padding:var(--space-8)"><p class="text-sm text-muted">Category metrics unavailable</p></div>';
  }
  loadRecentActivity(loadSeq);
  loadWinRate(loadSeq);
}

// North-star KPI card: *arr grab win rate over 30d — headline %, daily
// win-rate sparkline, per-indexer breakdown. Data is aggregate counts only
// (no release names cross the API). Degrades to a "requires core" note when
// the core predates evidence.indexerStats.
async function loadWinRate(loadSeq) {
  const valueEl = document.getElementById('winRateValue');
  if (!valueEl) return;
  const s = await api('/api/indexer-stats?days=30');
  if (loadSeq !== _dashboardLoadSeq) return;
  const countsEl = document.getElementById('winRateCounts');
  const bdEl = document.getElementById('indexerBreakdown');
  const sparkEl = document.getElementById('winRateSpark');

  if (!s || !s.available) {
    valueEl.textContent = '--';
    if (countsEl) countsEl.textContent = '';
    if (bdEl) bdEl.innerHTML = '<div class="empty-state" style="padding:var(--space-8)"><p class="text-sm text-muted">Not available — requires core with evidence.indexerStats (≥ v0.40.0)</p></div>';
    if (sparkEl) sparkEl.innerHTML = '';
    return;
  }

  valueEl.textContent = s.totalGrabs > 0 ? `${(s.bitagentWinRate * 100).toFixed(1)}%` : '—';
  if (countsEl) countsEl.textContent = s.totalGrabs > 0
    ? `${fmtNum(s.bitagentGrabs)} of ${fmtNum(s.totalGrabs)} grabs` : 'no grabs in window';

  // Breakdown bars, largest first (already server-ordered); BitAgent green,
  // everyone else accent. Scaled to the largest indexer like categoryChart.
  const idx = (s.indexers || []).slice(0, 8);
  if (bdEl) {
    if (idx.length > 0) {
      const max = Math.max(...idx.map(i => i.grabs), 1);
      const total = s.totalGrabs || 1;
      bdEl.innerHTML = idx.map(i => {
        const color = /bitagent/i.test(i.indexer) ? 'var(--color-success)' : 'var(--color-accent)';
        return `<div class="cat-row cat-row--indexer">
          <div class="cat-label" title="${escHtml(i.indexer)}">${escHtml(i.indexer)}</div>
          <div class="cat-bar"><div class="cat-bar-fill" style="width:${((i.grabs / max) * 100).toFixed(1)}%;background:${color}"></div></div>
          <div class="cat-count">${fmtNum(i.grabs)}</div>
          <div class="cat-pct">${((i.grabs / total) * 100).toFixed(1)}%</div>
        </div>`;
      }).join('');
    } else {
      bdEl.innerHTML = '<div class="empty-state" style="padding:var(--space-8)"><p class="text-sm text-muted">No grabs in the last 30 days</p></div>';
    }
  }

  // Sparkline: daily win-rate (wins/total per day) as a 0..100% polyline
  // with a dashed 50% reference line. Days with zero grabs plot at 0.
  if (sparkEl) {
    const days = s.days || [];
    if (days.length > 1) {
      const W = 300, H = 60, P = 4;
      const pts = days.map((d, i) => {
        const x = P + (i * (W - 2 * P)) / (days.length - 1);
        const r = d.totalGrabs > 0 ? d.bitagentGrabs / d.totalGrabs : 0;
        const y = H - P - r * (H - 2 * P);
        return `${x.toFixed(1)},${y.toFixed(1)}`;
      });
      sparkEl.innerHTML =
        `<line x1="${P}" y1="${H / 2}" x2="${W - P}" y2="${H / 2}" stroke="var(--color-border, rgba(255,255,255,0.12))" stroke-dasharray="4 4" vector-effect="non-scaling-stroke"/>` +
        `<polyline points="${pts.join(' ')}" fill="none" stroke="var(--color-success)" stroke-width="2" vector-effect="non-scaling-stroke"/>`;
    } else {
      sparkEl.innerHTML = '';
    }
  }
}

// Media type → icon + display label + color. Single source of truth so the
// Library tab, Wants tab, and Dashboard chart all show consistent visuals.
function categoryMeta(cat) {
  const ICONS = {
    movie:     '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><rect x="2" y="2" width="20" height="20" rx="2.18" ry="2.18"/><line x1="7" y1="2" x2="7" y2="22"/><line x1="17" y1="2" x2="17" y2="22"/><line x1="2" y1="12" x2="22" y2="12"/><line x1="2" y1="7" x2="7" y2="7"/><line x1="2" y1="17" x2="7" y2="17"/><line x1="17" y1="17" x2="22" y2="17"/><line x1="17" y1="7" x2="22" y2="7"/></svg>',
    tv_show:   '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><rect x="2" y="7" width="20" height="15" rx="2"/><polyline points="17 2 12 7 7 2"/></svg>',
    music:     '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><path d="M9 18V5l12-2v13"/><circle cx="6" cy="18" r="3"/><circle cx="18" cy="16" r="3"/></svg>',
    ebook:     '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><path d="M4 19.5A2.5 2.5 0 016.5 17H20"/><path d="M6.5 2H20v20H6.5A2.5 2.5 0 014 19.5v-15A2.5 2.5 0 016.5 2z"/></svg>',
    audiobook: '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><path d="M3 18v-6a9 9 0 0118 0v6"/><path d="M21 19a2 2 0 01-2 2h-1v-6h3v4zM3 19a2 2 0 002 2h1v-6H3v4z"/></svg>',
    software:  '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><polygon points="13 2 3 14 12 14 11 22 21 10 12 10 13 2"/></svg>',
    game:      '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><line x1="6" y1="11" x2="10" y2="11"/><line x1="8" y1="9" x2="8" y2="13"/><line x1="15" y1="12" x2="15.01" y2="12"/><line x1="18" y1="10" x2="18.01" y2="10"/><path d="M17.32 5H6.68a4 4 0 00-3.978 3.59c-.006.052-.01.101-.017.152C2.604 9.416 2 14.456 2 16a3 3 0 003 3c1 0 1.5-.5 2-1l1.414-1.414A2 2 0 019.828 16h4.344a2 2 0 011.414.586L17 18c.5.5 1 1 2 1a3 3 0 003-3c0-1.545-.604-6.584-.685-7.258-.007-.05-.011-.1-.017-.151A4 4 0 0017.32 5z"/></svg>',
    porn:      '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><circle cx="12" cy="12" r="10"/><line x1="4.93" y1="4.93" x2="19.07" y2="19.07"/></svg>',
    unknown:   '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><circle cx="12" cy="12" r="10"/><path d="M9.09 9a3 3 0 015.83 1c0 2-3 3-3 3"/><line x1="12" y1="17" x2="12.01" y2="17"/></svg>',
  };
  const LABELS = {
    movie: 'Movie',
    tv_show: 'TV Show',
    music: 'Music',
    ebook: 'eBook',
    audiobook: 'Audiobook',
    software: 'Software',
    game: 'Game',
    porn: 'Adult',
    unknown: 'Unknown',
  };
  const COLORS = {
    movie:     'var(--color-info)',
    tv_show:   'var(--color-accent)',
    music:     'var(--color-success)',
    ebook:     'var(--color-warning)',
    audiobook: '#a78bfa',
    software:  '#94a3b8',
    game:      '#f87171',
    porn:      '#9ca3af',
    unknown:   'var(--color-fg-subtle)',
  };
  return {
    svg: ICONS[cat] || ICONS.unknown,
    label: LABELS[cat] || cat,
    color: COLORS[cat] || COLORS.unknown,
  };
}

async function loadRecentActivity(loadSeq) {
  const data = await api('/api/evidence?limit=10');
  if (loadSeq !== _dashboardLoadSeq) return;
  const tbody = document.getElementById('activityTable');
  if (data === null) { tbody.innerHTML = loadFailedRow(5); return; }
  if (data && data.items && data.items.length > 0) {
    tbody.innerHTML = data.items.map(e =>
      `<tr>
        <td class="font-mono text-xs">${fmtAgo(e.timestamp)}</td>
        <td>${escHtml(e.source || '--')}</td>
        <td class="truncate" style="max-width:300px">${escHtml(e.torrentName || e.infoHash || '--')}</td>
        <td>${typePill(e.contentType)}</td>
        <td>${eventPill(e.eventType)}</td>
      </tr>`
    ).join('');
  } else {
    tbody.innerHTML = '<tr><td colspan="5" class="text-muted text-sm" style="text-align:center;padding:var(--space-6)">No recent activity yet. Events flow in from qBittorrent state polls and *arr webhooks/history.</td></tr>';
  }
}

/* ── Library ──────────────────────────────────────────────────────────── */
// Cache of infoHash -> item from the last library load; used for instant modal render
const _libItemCache = new Map();

// ── Library type filter pills ──────────────────────────────────────────
const _LIB_TYPE_ORDER = ['', 'movie', 'tv_show', 'music', 'ebook', 'software', 'unknown'];
const _LIB_TYPE_ACTIVE_CLASS = {
  movie: 'pill-info', tv_show: 'pill-accent', music: 'pill-success',
  ebook: 'pill-warning', software: 'pill-neutral', unknown: 'pill-neutral',
};

function initLibTypeFilters() {
  const container = document.getElementById('libTypeFilters');
  if (!container) return;
  container.innerHTML = _LIB_TYPE_ORDER.map(type => {
    const meta = type ? categoryMeta(type) : null;
    const label = meta ? meta.label : 'All';
    const icon = meta ? `<span style="display:inline-flex;width:13px;height:13px">${meta.svg}</span>` : '';
    const isActive = libActiveType === type;
    const activeClass = isActive ? `ltp-active ltp-${type || 'all'}` : `ltp-${type || 'all'}`;
    return `<button class="lib-type-pill ${activeClass}" onclick="setLibType('${type}')">${icon}${escHtml(label)}</button>`;
  }).join('');
}

function setLibType(type) {
  libActiveType = type;
  _groupPage = 0;
  initLibTypeFilters();
  loadLibrary();
}

// Monotonic id so a slow earlier response can never clobber a newer one
// (type 'du', then 'dune' — if 'du' resolves last it must be dropped).
let _libRequestSeq = 0;

function _syncLibControls() {
  const setActive = (id, on) => {
    const el = document.getElementById(id);
    if (el) { el.classList.toggle('active', on); el.setAttribute('aria-pressed', String(on)); }
  };
  setActive('fltHideUnmatched', libHideUnmatched);
  setActive('fltHideForeign', libHideForeign);
  const clear = document.getElementById('libSearchClear');
  if (clear) clear.style.display = document.getElementById('libSearch').value ? '' : 'none';
}

function toggleLibFilter(which) {
  if (which === 'unmatched') libHideUnmatched = !libHideUnmatched;
  if (which === 'foreign') libHideForeign = !libHideForeign;
  _groupPage = 0;
  loadLibrary();
}

function setLibSort(v) {
  libSort = v;
  _allGroups = _sortGroups(_allGroups);
  _groupPage = 0;
  _renderGroupPage();
}

function libSearchInput() {
  _syncLibControls(); // show/hide the clear button immediately, pre-debounce
  debouncedLoadLibrary();
}

function clearLibSearch() {
  const input = document.getElementById('libSearch');
  input.value = '';
  input.focus();
  loadLibrary();
}

function libSearchKeydown(e) {
  if (e.key === 'Escape') {
    if (e.target.value) { e.stopPropagation(); clearLibSearch(); }
    else e.target.blur();
  }
  if (e.key === 'Enter') loadLibrary(); // skip the debounce
}

async function loadLibrary() {
  initLibTypeFilters();
  _syncLibControls();
  const q = document.getElementById('libSearch').value;
  const hideNonEn = libHideForeign;
  const hideUnmapped = libHideUnmatched;

  // Fetch a large batch so the client-side grouper can collapse duplicates.
  // Pagination is then done over the resulting groups, not raw items.
  // Limitation: results are capped at 500 raw items. When totalCount > 500,
  // a notice is shown prompting the user to narrow down with search.
  const params = new URLSearchParams({
    q, content_types: libActiveType, limit: 500, offset: 0,
    hide_unmapped: hideUnmapped, hide_non_english: hideNonEn,
  });

  const seq = ++_libRequestSeq;
  const spinner = document.getElementById('libSearchSpinner');
  if (spinner) spinner.style.display = '';

  // Show loading state only when the grid is empty (first load / tab switch);
  // while typing, keep the current results on screen until fresh ones arrive.
  const grid = document.getElementById('libraryGrid');
  if (grid && !_allGroups.length) {
    grid.innerHTML = '<div class="empty-state" style="grid-column:1/-1"><p>Loading…</p></div>';
  }

  const data = await api(`/api/torrents?${params}`, { timeoutMs: 35_000 });
  if (seq !== _libRequestSeq) return; // superseded by a newer request
  if (spinner) spinner.style.display = 'none';
  if (!data) {
    // api() returns null on any fetch failure (401, timeout, network). Without
    // this branch the grid was left on "Loading…" forever and the pagination
    // bar kept its hardcoded "Showing 0 of 0" — indistinguishable from a hang.
    if (grid) grid.innerHTML = '<div class="empty-state" style="grid-column:1/-1"><svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1"><circle cx="12" cy="12" r="10"/><line x1="12" y1="8" x2="12" y2="12"/><line x1="12" y1="16" x2="12.01" y2="16"/></svg><h3>Couldn’t load the library</h3><p>The request failed — your session may have expired. Reload the page, and if it persists, sign in again.</p></div>';
    const pag = document.getElementById('libPaginationInfo');
    if (pag) pag.textContent = 'Failed to load';
    return;
  }

  const items = data.items || [];
  items.forEach(t => _libItemCache.set(t.infoHash, t));

  // Update sidebar badge from totalCount when not filtering
  const serverFiltering = hideNonEn || hideUnmapped;
  const totalCount = data.totalCount || 0;
  if (!serverFiltering && totalCount > 0) {
    document.getElementById('libCount').textContent = fmtNum(totalCount);
  }

  // Bounded whenever more may exist beyond the fetched window: a known total that
  // exceeds what we fetched, OR any server-side filter (Hide non-English is ON by
  // default) — which makes the true total unknown, so a capped upstream scan can
  // hide further matches even when fewer than 500 rows survive the filter (the
  // default browse returns ~489 English movies but the corpus holds far more).
  // Previously gated on `!serverFiltering`, so the DEFAULT operator browse capped
  // silently with no note. Mirrors the public library, which labels every
  // unknown-total window bounded.
  _libTruncated = serverFiltering || totalCount > items.length;
  _libLastQuery = q;

  _allGroups = _sortGroups(_groupTorrents(items));
  _groupPage = 0;
  _renderGroupPage();

  // Context line in the toolbar: what the grid currently shows.
  const note = document.getElementById('libResultNote');
  if (note) {
    if (!items.length) note.textContent = '';
    else if (q) note.textContent = `${fmtNum(_allGroups.length)} title${_allGroups.length !== 1 ? 's' : ''} · ${fmtNum(items.length)} releases`;
    else note.textContent = `browsing most-seeded — type to search ${totalCount > 0 ? fmtNum(totalCount) + ' indexed' : 'everything'}`;
  }
}

// Truncation state set by loadLibrary
let _libTruncated = false;
let _libLastQuery = '';

// Sort the grouped titles by the active sort key. Groups sort by their best
// release, except name which sorts by display title.
function _sortGroups(groups) {
  const by = {
    seeders: (a, b) => (b.best.seeders || 0) - (a.best.seeders || 0),
    newest: (a, b) => new Date(b.best.discoveredAt || 0) - new Date(a.best.discoveredAt || 0),
    size: (a, b) => (b.best.size || 0) - (a.best.size || 0),
    name: (a, b) => _groupDisplayTitle(a.best.name).localeCompare(_groupDisplayTitle(b.best.name)),
  };
  return [...groups].sort(by[libSort] || by.seeders);
}

function _renderGroupPage() {
  const start = _groupPage * _groupPageSize;
  const slice = _allGroups.slice(start, start + _groupPageSize);
  const total = _allGroups.length;

  // When the browse is bounded (filter active, or a known total beyond the fetched
  // window), say so honestly rather than presenting a capped window as the whole
  // library. The known total is a release count, not a title count, so we don't mix
  // units into an "N of M" claim — matching the public library's bounded-window note.
  const truncNote = _libTruncated ? ' (bounded window — search to narrow)' : '';
  document.getElementById('libPaginationInfo').textContent =
    total === 0 ? 'No titles found'
    : `${start + 1}–${Math.min(start + slice.length, total)} of ${total} titles${truncNote}`;
  document.getElementById('libPrev').disabled = _groupPage === 0;
  document.getElementById('libNext').disabled = start + _groupPageSize >= total;

  if (libView === 'grid') renderLibGrid(slice);
  else renderLibTable(slice.map(g => g.best));
}

// Grouping-key title normalization lives in the shared static/js/norm-title.js
// module (loaded before this script) as normGroupTitle(), so app.js and
// library.js can't drift.

// Torrent kind classification (complete | season | episode) lives in the shared
// static/js/torrent-kind.js module (loaded before this script) as
// parseTorrentKind(), so app.js and library.js can't drift.

// Cache of groupKey -> group object (populated in renderLibGrid)
const _groupCache = new Map();

// Group flat torrent list by content identity
function _groupTorrents(items) {
  const map = new Map();
  for (const t of items) {
    const key = (t.contentSource === 'tmdb' && t.contentId)
      ? `${t.contentType}:id:${t.contentId}`
      : `${t.contentType}:name:${normGroupTitle(t.name)}`;
    if (!map.has(key)) {
      map.set(key, { key, contentType: t.contentType, contentId: t.contentId,
        contentSource: t.contentSource, isMapped: t.isMapped, best: t, items: [] });
    }
    const g = map.get(key);
    g.items.push(t);
    if ((t.seeders || 0) > (g.best.seeders || 0)) g.best = t;
  }
  return [...map.values()].sort((a, b) => (b.best.seeders || 0) - (a.best.seeders || 0));
}

// Derive a display title (strip episode/season/quality from raw torrent name)
function _groupDisplayTitle(name) {
  return name
    .replace(/\bS\d{1,2}E\d{1,3}.*/i, '').replace(/\bS\d{1,2}(?!\d)(?!E).*/i, '')
    .replace(/\bSeason\s+\d+.*/i, '')
    .replace(/\s+/g, ' ').trim() || name;
}

const POSTER_ICONS = {
  movie: '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.5"><rect x="2" y="2" width="20" height="20" rx="2.18" ry="2.18"/><line x1="7" y1="2" x2="7" y2="22"/><line x1="17" y1="2" x2="17" y2="22"/><line x1="2" y1="12" x2="22" y2="12"/><line x1="2" y1="7" x2="7" y2="7"/><line x1="2" y1="17" x2="7" y2="17"/><line x1="17" y1="7" x2="22" y2="7"/><line x1="17" y1="17" x2="22" y2="17"/></svg>',
  tv_show: '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.5"><rect x="2" y="7" width="20" height="15" rx="2" ry="2"/><polyline points="17 2 12 7 7 2"/></svg>',
  music: '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.5"><path d="M9 18V5l12-2v13"/><circle cx="6" cy="18" r="3"/><circle cx="18" cy="16" r="3"/></svg>',
  ebook: '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.5"><path d="M4 19.5A2.5 2.5 0 016.5 17H20"/><path d="M6.5 2H20v20H6.5A2.5 2.5 0 014 19.5v-15A2.5 2.5 0 016.5 2z"/></svg>',
  software: '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.5"><polyline points="16 18 22 12 16 6"/><polyline points="8 6 2 12 8 18"/></svg>',
  unknown: '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.5"><circle cx="12" cy="12" r="10"/><path d="M9.09 9a3 3 0 015.83 1c0 2-3 3-3 3"/><line x1="12" y1="17" x2="12.01" y2="17"/></svg>',
};
// Accept either an array of raw torrent items OR pre-computed group objects.
// Pre-computed groups have an `items` property; raw torrents do not.
function renderLibGrid(itemsOrGroups) {
  document.getElementById('libraryGrid').style.display = '';
  document.getElementById('libraryTable').style.display = 'none';
  const grid = document.getElementById('libraryGrid');
  if (!itemsOrGroups || itemsOrGroups.length === 0) {
    const msg = _libLastQuery
      ? `<h3>No titles match “${escHtml(_libLastQuery)}”</h3><p>Try fewer words or the original title — or relax the type/language filters above.</p>`
      : '<h3>No titles found</h3><p>Adjust your filters or wait for the DHT crawler to index content.</p>';
    grid.innerHTML = `<div class="empty-state" style="grid-column:1/-1"><svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1"><circle cx="11" cy="11" r="8"/><path d="M21 21l-4.35-4.35"/></svg>${msg}</div>`;
    return;
  }
  // If already grouped (have .items), use as-is; otherwise group now
  const groups = (itemsOrGroups[0] && itemsOrGroups[0].items !== undefined)
    ? itemsOrGroups
    : _groupTorrents(itemsOrGroups);
  _groupCache.clear();
  groups.forEach(g => _groupCache.set(g.key, g));
  grid.innerHTML = groups.map(g => {
    const t = g.best;
    const ct = g.contentType || 'unknown';
    const icon = POSTER_ICONS[ct] || POSTER_ICONS.unknown;
    const count = g.items.length;
    const posterId = (t.contentSource === 'tmdb' && t.contentId) ? t.contentId
      : (ct === 'music' && t.contentSource === 'musicbrainz' && t.contentId) ? t.contentId
      : (ct === 'music' && !t.contentId) ? (t.name || '') : '';
    const posterSource = ct === 'music'
      ? (t.contentSource === 'musicbrainz' ? 'mb' : 'lidarr') : 'tmdb';
    const mediaType = ct === 'tv_show' ? 'tv' : 'movie';
    const qual = [fmtRes(t.videoResolution), t.videoSource].filter(Boolean).join(' · ');
    const title = _groupDisplayTitle(t.name);
    return `<div class="poster-card" onclick="openGroupDetail('${escAttrArg(g.key)}')">
      <div class="poster-placeholder ${ct}" data-poster-id="${escHtml(posterId)}" data-poster-source="${posterSource}" data-poster-type="${mediaType}">${icon}<span>${escHtml(ct.replace('_',' '))}</span></div>
      ${count > 1 ? `<span class="release-count-badge">${count}</span>` : ''}
      <div class="poster-info">
        <div class="poster-title">${escHtml(title)}</div>
        <div class="poster-meta">
          ${typePill(ct)}
          ${qual ? `<span class="text-xs text-subtle">${escHtml(qual)}</span>` : ''}
        </div>
        <div class="poster-meta mt-2">
          <span class="pill pill-success pill-sm" style="font-size:10px">${t.seeders||0} seeds</span>
          ${!g.isMapped ? '<span class="pill pill-neutral" style="font-size:10px">unmatched</span>' : ''}
        </div>
      </div>
    </div>`;
  }).join('');
  hydrateTmdbPosters(grid);
}

// ── Release detail modal ────────────────────────────────────────────────
// When a poster is opened we re-query the backend for EVERY release of that
// title (the library grid only ever loaded a capped batch, so a group's cached
// items were usually just one). Releases are then grouped by edition (movies)
// or season/episode (TV) and filterable by quality.
let _gd = { group: null, ct: '', title: '', releases: [], season: 'all', quality: 'all', posterUrl: null };

// Edition detection — ordered so more specific patterns win.
const _EDITIONS = [
  [/final.?cut/i, 'Final Cut'],
  [/director'?s?.?cut/i, 'Director’s Cut'],
  [/extended(?:.?(?:edition|cut|version))?/i, 'Extended'],
  [/uncut/i, 'Uncut'],
  [/unrated/i, 'Unrated'],
  [/theatrical/i, 'Theatrical'],
  [/\bimax\b/i, 'IMAX'],
  [/criterion/i, 'Criterion'],
  [/remaster(?:ed)?/i, 'Remastered'],
  [/anniversary/i, 'Anniversary'],
  [/ultimate(?:.?edition)?/i, 'Ultimate Edition'],
  [/special.?edition/i, 'Special Edition'],
];
function _editionOf(name) {
  for (const [rx, label] of _EDITIONS) if (rx.test(name || '')) return label;
  return 'Standard';
}

// Raw torrent filename carries the edition/quality/source tokens; the cleaned
// metadata `name` (e.g. "Batman Begins (2005)") is identical across releases.
function _rname(t) { return t.torrentName || t.name || ''; }

const _RES_RANK = { '2160p': 4, '1080p': 3, '720p': 2, '576p': 1, '480p': 1 };
function _resOf(t) {
  const low = _rname(t).toLowerCase();
  const m = low.match(/\b(2160|1080|720|576|480)[pi]\b/);
  if (m) return m[1] + 'p';
  if (/\b(4k|uhd)\b/.test(low)) return '2160p';
  if (t.videoResolution) return fmtRes(t.videoResolution).toLowerCase();
  return '';
}
function _srcOf(t) {
  const low = _rname(t).toLowerCase();
  const m = low.match(/\b(blu-?ray|remux|web-?dl|webrip|hdtv|bdrip|dvdrip|hdrip|cam)\b/);
  if (m) {
    const s = m[1].replace(/-/g, '');
    return { bluray: 'BluRay', webdl: 'WEB-DL', webrip: 'WEBRip', remux: 'REMUX' }[s]
      || s.toUpperCase();
  }
  return t.videoSource ? String(t.videoSource) : '';
}
function _flagsOf(t) {
  const low = _rname(t).toLowerCase();
  const f = [];
  if (_resOf(t) === '2160p') f.push('4K');
  if (/hdr10\+?|\bhdr\b|dolby.?vision|\bdovi\b|\bdv\b/.test(low)) f.push('HDR');
  if (/dolby.?vision|\bdovi\b|\bdv\b/.test(low)) f.push('DV');
  if (/\batmos\b|truehd|dts-?hd|dts-?x/.test(low)) f.push('ATMOS');
  if (/\bremux\b/.test(low)) f.push('REMUX');
  if (/\bimax\b/.test(low)) f.push('IMAX');
  return f;
}
function _relSort(a, b) {
  const ra = _RES_RANK[_resOf(a)] || 0, rb = _RES_RANK[_resOf(b)] || 0;
  if (rb !== ra) return rb - ra;
  return (b.seeders || 0) - (a.seeders || 0);
}

function _releaseRow(t) {
  const res = _resOf(t);
  const flags = _flagsOf(t);
  const src = _srcOf(t);
  const langs = (t.languages || []).filter(l => l && l !== 'en');
  const rn = _rname(t);
  const magnet = `magnet:?xt=urn:btih:${t.infoHash}&dn=${encodeURIComponent(rn)}`;
  const flagBadges = flags.map(f => `<span class="rel-flag rel-flag-${f.toLowerCase()}">${f}</span>`).join('');
  return `<div class="rel-row" onclick="openTorrentDetail('${escHtml(t.infoHash)}')">
    <div class="rel-row-main">
      <div class="rel-row-name truncate">${escHtml(rn)}</div>
      <div class="rel-row-tags">
        ${res ? `<span class="rel-res">${escHtml(res)}</span>` : ''}
        ${flagBadges}
        ${src ? `<span class="rel-src">${escHtml(src)}</span>` : ''}
        ${t.releaseGroup ? `<span class="rel-grp">${escHtml(t.releaseGroup)}</span>` : ''}
        ${langs.length ? `<span class="rel-lang">${escHtml(langs.slice(0, 2).join(', '))}</span>` : ''}
      </div>
    </div>
    <div class="rel-row-meta">
      <span class="rel-size">${fmtBytes(t.size)}</span>
      <span class="rel-seed" title="${t.seeders || 0} seeders / ${t.leechers || 0} leechers"><span class="rel-seed-ico">▲</span>${t.seeders || 0}</span>
      <button class="rel-copy" title="Copy magnet link" onclick="event.stopPropagation();copyMagnet('${escAttrArg(magnet)}')">
        <svg width="13" height="13" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2"><rect x="9" y="9" width="13" height="13" rx="2" ry="2"/><path d="M5 15H4a2 2 0 0 1-2-2V4a2 2 0 0 1 2-2h9a2 2 0 0 1 2 2v1"/></svg>
      </button>
    </div>
  </div>`;
}

function _relSection(title, rows) {
  if (!rows.length) return '';
  return `<div class="rel-section"><div class="rel-section-head"><span class="rel-section-title">${escHtml(title)}</span><span class="rel-section-count">${rows.length}</span></div>${rows.map(_releaseRow).join('')}</div>`;
}

function _renderMovieBody(rels) {
  const byEd = new Map();
  for (const t of rels) {
    const e = _editionOf(_rname(t));
    if (!byEd.has(e)) byEd.set(e, []);
    byEd.get(e).push(t);
  }
  // Standard first, then named editions alphabetically.
  const order = [...byEd.keys()].sort((a, b) =>
    a === 'Standard' ? -1 : b === 'Standard' ? 1 : a.localeCompare(b));
  return order.map(ed => _relSection(ed, byEd.get(ed).sort(_relSort))).join('');
}

function _renderTvBody(rels, seasonFilter) {
  const parsed = rels.map(t => ({ ...t, _k: parseTorrentKind(_rname(t)) }));
  const seasons = [...new Set(parsed
    .filter(p => p._k.season != null && p._k.kind !== 'complete')
    .map(p => p._k.season))].sort((a, b) => a - b);

  let view = parsed;
  if (seasonFilter !== 'all') {
    const s = +seasonFilter;
    view = parsed.filter(p => p._k.kind === 'complete' || p._k.season === s);
  }
  const complete = view.filter(p => p._k.kind === 'complete').sort(_relSort);
  const packs = view.filter(p => p._k.kind === 'season');
  const eps = view.filter(p => p._k.kind === 'episode');

  let html = _relSection('Complete Series', complete);
  for (const s of [...new Set(packs.map(p => p._k.season))].sort((a, b) => a - b)) {
    html += _relSection(`Season ${s} — Pack`, packs.filter(p => p._k.season === s).sort(_relSort));
  }
  for (const s of [...new Set(eps.map(p => p._k.season))].sort((a, b) => a - b)) {
    const list = eps.filter(p => p._k.season === s)
      .sort((a, b) => (a._k.episode - b._k.episode) || _relSort(a, b));
    html += _relSection(`Season ${s} — Episodes`, list);
  }
  return { html, seasons };
}

function _gdFiltered() {
  let r = _gd.releases;
  if (_gd.quality !== 'all') r = r.filter(t => _resOf(t) === _gd.quality);
  return r;
}
function _gdSetQuality(q) { _gd.quality = q; _renderReleaseDetail(); }
function _gdSetSeason(s) { _gd.season = s; _renderReleaseDetail(); }

function _renderReleaseDetail() {
  const g = _gd.group, ct = _gd.ct, all = _gd.releases;
  const rels = _gdFiltered();

  // Poster / placeholder
  document.getElementById('groupDetailPoster').innerHTML = _gd.posterUrl
    ? `<img src="${escHtml(_gd.posterUrl)}" alt="" class="gd-poster-img">`
    : `<div class="gd-poster-ph ${ct}">${POSTER_ICONS[ct] || POSTER_ICONS.unknown}</div>`;

  // Quality pills — only resolutions actually present.
  const resSet = [...new Set(all.map(_resOf).filter(Boolean))]
    .sort((a, b) => (_RES_RANK[b] || 0) - (_RES_RANK[a] || 0));
  const qPills = ['all', ...resSet].map(q =>
    `<button class="gd-pill ${_gd.quality === q ? 'gd-pill-active' : ''}" onclick="_gdSetQuality('${q}')">${q === 'all' ? 'All' : escHtml(q)}</button>`).join('');

  // Body + optional season chips
  let body = '', chips = '';
  if (ct === 'tv_show') {
    const { html, seasons } = _renderTvBody(rels, _gd.season);
    body = html;
    if (seasons.length > 1 || _gd.season !== 'all') {
      chips = `<div class="gd-chips"><button class="gd-chip ${_gd.season === 'all' ? 'gd-chip-active' : ''}" onclick="_gdSetSeason('all')">All Seasons</button>`
        + seasons.map(s => `<button class="gd-chip ${String(_gd.season) === String(s) ? 'gd-chip-active' : ''}" onclick="_gdSetSeason('${s}')">S${s}</button>`).join('')
        + `</div>`;
    }
  } else {
    body = _renderMovieBody(rels);
  }
  if (!body) body = `<p class="rel-empty">No releases match this filter.</p>`;

  // Stat line
  const statBits = [`${all.length} release${all.length !== 1 ? 's' : ''}`];
  if (ct === 'tv_show') {
    const sc = new Set(all.map(t => parseTorrentKind(_rname(t)).season).filter(s => s != null)).size;
    if (sc) statBits.push(`${sc} season${sc !== 1 ? 's' : ''}`);
  } else {
    const ed = new Set(all.map(t => _editionOf(_rname(t)))).size;
    if (ed > 1) statBits.push(`${ed} editions`);
  }

  document.getElementById('groupDetailTitle').textContent = _gd.title;
  document.getElementById('groupDetailMeta').innerHTML =
    `${typePill(ct)} <span class="text-xs text-muted">${statBits.join(' · ')}</span>`
    + (g && !g.isMapped ? ' <span class="pill pill-neutral" style="font-size:10px">unmatched</span>' : '');
  document.getElementById('groupDetailFilters').innerHTML = `<div class="gd-pills">${qPills}</div>${chips}`;
  document.getElementById('groupDetailBody').innerHTML = body;
}

async function openGroupDetail(key) {
  const g = _groupCache.get(key);
  if (!g) return;
  const modal = document.getElementById('groupDetailModal');
  if (!modal) return;

  const t = g.best;
  const ct = g.contentType || 'unknown';
  _gd = {
    group: g, ct, title: _groupDisplayTitle(t.name),
    releases: (g.items || []).slice(), season: 'all', quality: 'all', posterUrl: null,
  };
  modal.classList.add('open');
  _renderReleaseDetail(); // instant render from the cached (partial) batch

  // Poster identity — mirrors the poster-card logic in renderLibGrid.
  const posterId = (t.contentSource === 'tmdb' && t.contentId) ? t.contentId
    : (ct === 'music' && t.contentSource === 'musicbrainz' && t.contentId) ? t.contentId
    : (ct === 'music' && !t.contentId) ? (t.name || '') : '';
  const posterSource = ct === 'music'
    ? (t.contentSource === 'musicbrainz' ? 'mb' : 'lidarr') : 'tmdb';
  const mediaType = ct === 'tv_show' ? 'tv' : 'movie';

  const params = new URLSearchParams({
    title: _gd.title, content_type: ct,
    content_source: g.contentSource || '', content_id: g.contentId || '',
  });

  const [full] = await Promise.all([
    api(`/api/titles/releases?${params}`, { timeoutMs: 35_000 }),
    (async () => {
      if (!posterId) return;
      try {
        const r = await fetch(`${API}/api/poster/${encodeURIComponent(posterId)}?source=${posterSource}&media_type=${mediaType}`);
        if (r.ok) { const d = await r.json(); if (d.poster_url) _gd.posterUrl = d.poster_url; }
      } catch (_) { /* keep placeholder */ }
    })(),
  ]);

  // Bail if the user closed the modal or opened a different title meanwhile.
  if (!modal.classList.contains('open') || _gd.group !== g) return;

  if (full && full.items) {
    // Union the complete backend set with the cached batch (fetched wins), so
    // we never end up showing FEWER than what was already on screen.
    const map = new Map();
    for (const it of full.items) map.set(it.infoHash, it);
    for (const it of (g.items || [])) if (!map.has(it.infoHash)) map.set(it.infoHash, it);
    _gd.releases = [...map.values()];
    _gd.releases.forEach(it => _libItemCache.set(it.infoHash, it));
  }
  _renderReleaseDetail();
}

async function hydrateTmdbPosters(root) {
  const placeholders = root.querySelectorAll('.poster-placeholder[data-poster-id]:not([data-poster-id=""])');
  // Fire in small batches so we don't open 50 sockets at once.
  const queue = Array.from(placeholders);
  const inFlight = 6;
  async function workOne() {
    const el = queue.shift();
    if (!el) return;
    const id = el.dataset.posterId;
    const source = el.dataset.posterSource || 'tmdb';
    const type = el.dataset.posterType || 'movie';
    try {
      const resp = await fetch(`${API}/api/poster/${encodeURIComponent(id)}?source=${source}&media_type=${type}`);
      if (resp.ok) {
        const data = await resp.json();
        if (data.poster_url) {
          // Decode the image off-DOM first; only replace the placeholder once
          // the bytes are available. On error the placeholder stays untouched.
          // NOTE: do NOT set loading='lazy' — that suppresses the actual
          // network fetch on detached images, so onload never fires.
          const img = new Image();
          img.alt = data.title || '';
          img.decoding = 'async';
          await new Promise((resolve) => {
            img.onload = () => {
              el.innerHTML = '';
              el.appendChild(img);
              el.classList.add('has-poster');
              resolve();
            };
            img.onerror = () => resolve(); // placeholder stays in place
            img.src = data.poster_url;
          });
        }
      }
    } catch (_) { /* fall back to placeholder silently */ }
    return workOne();
  }
  await Promise.all(Array.from({ length: inFlight }, workOne));
}

function renderLibTable(items) {
  document.getElementById('libraryGrid').style.display = 'none';
  document.getElementById('libraryTable').style.display = '';
  const tbody = document.getElementById('libTableBody');
  if (items.length === 0) {
    tbody.innerHTML = '<tr><td colspan="6" class="text-muted text-sm" style="text-align:center;padding:var(--space-8)">No torrents found</td></tr>';
    return;
  }
  tbody.innerHTML = items.map(t => {
    const title = t.name || 'Unknown';
    const qual = [fmtRes(t.videoResolution), t.videoSource].filter(Boolean).join(' ');
    const langs = (t.languages || []).filter(l => l !== 'en');
    const langBadge = langs.length > 0
      ? `<span class="pill pill-warning" style="font-size:10px">${langs.slice(0,2).join(', ')}</span> `
      : '';
    const unmappedBadge = !t.isMapped ? `<span class="pill pill-neutral" style="font-size:10px">unmapped</span> ` : '';
    return `<tr style="cursor:pointer" onclick="openTorrentDetail('${escHtml(t.infoHash)}')">
      <td class="truncate" style="max-width:300px"><strong>${escHtml(title)}</strong> ${langBadge}${unmappedBadge}</td>
      <td>${typePill(t.contentType)}</td>
      <td class="text-xs text-subtle">${escHtml(qual)}</td>
      <td>${fmtBytes(t.size)}</td>
      <td><span class="pill pill-success" style="font-size:10px">${t.seeders || 0}</span></td>
      <td class="text-xs text-subtle">${fmtAgo(t.discoveredAt)}</td>
    </tr>`;
  }).join('');
}

function setLibView(view) {
  libView = view;
  const vg = document.getElementById('viewGrid'), vl = document.getElementById('viewList');
  vg.classList.toggle('active', view === 'grid'); vg.setAttribute('aria-pressed', String(view === 'grid'));
  vl.classList.toggle('active', view === 'list'); vl.setAttribute('aria-pressed', String(view === 'list'));
  // Re-render from the already-loaded groups — no refetch needed.
  _renderGroupPage();
}
function libPage(dir) {
  _groupPage = Math.max(0, _groupPage + dir);
  _renderGroupPage();
}

let _currentTorrent = null;

function _renderTorrentModal(data) {
  _currentTorrent = data;
  const qual = [fmtRes(data.videoResolution), data.videoSource, data.videoCodec].filter(Boolean).join(' · ');
  const langs = (data.languages || []);
  const langStr = langs.length > 0 ? langs.join(', ') : '--';
  const origLang = data.originalLanguage || '--';
  const mappedStr = data.isMapped
    ? `<span class="pill pill-success" style="font-size:11px"><span class="pill-dot"></span>Mapped (${escHtml(data.contentSource || '')})</span>`
    : `<span class="pill pill-neutral" style="font-size:11px">Unmapped</span>`;

  document.getElementById('torrentDetail').innerHTML = `
    <div class="flex flex-col gap-4">
      <div>
        <h3 style="font-size:var(--text-lg);font-weight:var(--weight-semibold)">${escHtml(data.name || data.infoHash)}</h3>
        <div class="flex items-center gap-3 mt-2" style="flex-wrap:wrap;">
          ${typePill(data.contentType)}
          ${mappedStr}
          ${qual ? `<span class="pill pill-neutral" style="font-size:11px">${escHtml(qual)}</span>` : ''}
          <span class="text-sm text-muted">${fmtBytes(data.size)}</span>
          <span class="pill pill-success"><span class="pill-dot"></span>${data.seeders || 0} seeders</span>
        </div>
      </div>
      <div class="divider" style="margin:0"></div>
      <div class="grid-2">
        <div class="input-group"><span class="input-label">Info Hash</span><span class="font-mono text-xs">${escHtml(data.infoHash)}</span></div>
        <div class="input-group"><span class="input-label">Release Group</span><span>${escHtml(data.releaseGroup || '--')}</span></div>
        <div class="input-group"><span class="input-label">Languages</span><span>${escHtml(langStr)}</span></div>
        <div class="input-group"><span class="input-label">Original Language</span><span>${escHtml(origLang)}</span></div>
        ${data.imdbId ? `<div class="input-group"><span class="input-label">IMDb</span><span class="font-mono text-xs">${escHtml(data.imdbId)}</span></div>` : ''}
        ${data.tmdbId ? `<div class="input-group"><span class="input-label">TMDB</span><span class="font-mono text-xs">${escHtml(data.tmdbId)}</span></div>` : ''}
        ${data.seasonNumber != null ? `<div class="input-group"><span class="input-label">Season</span><span>${data.seasonNumber}</span></div>` : ''}
        ${data.episodeNumber != null ? `<div class="input-group"><span class="input-label">Episode</span><span>${data.episodeNumber}</span></div>` : ''}
        <div class="input-group"><span class="input-label">Discovered</span><span>${fmtDate(data.discoveredAt || data.createdAt)}</span></div>
        <div class="input-group"><span class="input-label">Leechers</span><span>${data.leechers || 0}</span></div>
      </div>
      ${data.files && data.files.length > 0 ? `
        <div><span class="input-label mb-2" style="display:block">Files (${data.files.length})</span>
        <div style="max-height:180px;overflow-y:auto">
          ${data.files.map(f => `<div class="flex items-center justify-between" style="padding:var(--space-1) 0;border-bottom:1px solid var(--color-border-subtle)"><span class="text-xs truncate" style="max-width:420px">${escHtml(f.path)}</span><span class="text-xs text-muted">${fmtBytes(f.size)}</span></div>`).join('')}
        </div></div>` : ''}
    </div>`;

  // Footer: magnet link + arr push buttons
  document.getElementById('magnetLink').href = data.magnetUri || `magnet:?xt=urn:btih:${data.infoHash}`;
  const arrBtns = document.getElementById('arrPushBtns');
  const ct = data.contentType || '';
  const arrTargets = [];
  if (ct === 'tv_show' || ct === '') arrTargets.push('sonarr');
  if (ct === 'movie' || ct === '') arrTargets.push('radarr');
  if (ct === 'music') arrTargets.push('lidarr');
  arrBtns.innerHTML = arrTargets.map(arr =>
    `<button class="btn btn-primary btn-sm" onclick="sendToArr('${arr}', event)">
      <svg width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2"><polyline points="22 2 11 13"/><polygon points="22 2 15 22 11 13 2 9 22 2"/></svg>
      Send to ${arr.charAt(0).toUpperCase() + arr.slice(1)}
    </button>`
  ).join('');
  document.getElementById('torrentActions').style.display = '';
}

let _torrentReqHash = null;
async function openTorrentDetail(hash) {
  document.getElementById('torrentModal').classList.add('open');
  document.getElementById('torrentActions').style.display = 'none';
  // Token the in-flight request by hash. Clicking B while A is still loading
  // must not let A's late response clobber B's modal (wrong magnet + wrong
  // arr target). Every render below bails unless it's still the current hash.
  _torrentReqHash = hash;

  // Render immediately from library cache if available — no loading flicker
  const cached = _libItemCache.get(hash);
  if (cached) {
    _renderTorrentModal({ ...cached, magnetUri: `magnet:?xt=urn:btih:${hash}&dn=${encodeURIComponent(cached.name || '')}` });
  } else {
    document.getElementById('torrentDetail').innerHTML = '<div class="skeleton" style="height:200px"></div>';
  }

  // Fetch full detail in the background to enrich with IMDb/TMDB IDs etc.
  const data = await api(`/api/torrents/${hash}`);
  if (_torrentReqHash !== hash) return;  // a newer open superseded this one
  if (data) {
    _renderTorrentModal(data);
  } else if (!cached) {
    document.getElementById('torrentDetail').innerHTML = '<p class="text-muted">Could not load details. The torrent may no longer be indexed.</p>';
  }
  // If cached was already rendered and detail fetch failed, keep the cached render.
}

function closeTorrentModal() {
  document.getElementById('torrentModal').classList.remove('open');
  _currentTorrent = null;
}

function copyMagnet(magnet) {
  const uri = magnet || _currentTorrent?.magnetUri;
  if (!uri) return;
  if (!navigator.clipboard?.writeText) { toast('Clipboard unavailable', 'error'); return; }
  navigator.clipboard.writeText(uri)
    .then(() => toast('Magnet URI copied', 'success'))
    .catch(() => toast('Copy failed — try manually', 'error'));
}

async function sendToArr(arr, ev) {
  if (!_currentTorrent) return;
  const btn = (ev && ev.target && ev.target.closest('button')) || null;
  // Capture innerHTML, not textContent — the button holds an SVG icon + label,
  // and restoring textContent would strip the icon for the rest of the session.
  const orig = btn ? btn.innerHTML : '';
  if (btn) { btn.disabled = true; btn.textContent = 'Sending…'; }
  const r = await api('/api/arr/push', {
    method: 'POST',
    body: JSON.stringify({ arr, info_hash: _currentTorrent.infoHash, title: _currentTorrent.name }),
  });
  if (btn) { btn.disabled = false; btn.innerHTML = orig; }
  if (r && r.status === 'pushed') toast(`Sent to ${arr} (HTTP ${r.httpStatus})`, 'success');
  else if (r === null) toast(`Failed to reach ${arr} — check Settings → Integrations`, 'error');
  else toast((r && r.detail) || `${arr} returned an error`, 'error');
}

/* ── Wants ────────────────────────────────────────────────────────────── */
// The Wants tab used to CRUD a UI-local `wants` table that nothing
// downstream ever read — every reference to it was this app's own API — while
// the core's real *arr wantlist bridge was invisible here. This renders the
// bridge instead. See /api/wantbridge.
async function loadWants() {
  const d = await api('/api/wantbridge');
  const tbody = document.getElementById('wbSourcesTable');
  const banner = document.getElementById('wbEnforceBanner');
  const setStat = (id, text) => _blankStats({ [id]: text });

  if (!d || !d.available) {
    if (tbody) tbody.innerHTML = d === null ? loadFailedRow(5)
      : '<tr><td colspan="5" class="text-muted text-sm" style="text-align:center;padding:var(--space-8)">Wantbridge reports no metrics — it is disabled in the core (WANTBRIDGE_ENABLED=false) or no *arr is configured.</td></tr>';
    _blankStats({
      wbWantlistTotal: '—', wbWantlistSub: 'unavailable',
      wbTier0: '—', wbTier0Sub: 'unavailable',
      wbFingerprint: '—',
      wbCycles: '—', wbCyclesSub: 'unavailable',
    });
    if (banner) banner.style.display = 'none';
    return;
  }

  const tiers = d.matchesByTier || {};
  setStat('wbWantlistTotal', fmtNum(d.wantlistTotal || 0));
  setStat('wbWantlistSub', (d.wantlists || []).map(w => `${w.source} ${fmtNum(w.size)}`).join(' · ') || 'no sources');
  setStat('wbTier0', fmtNum(tiers.tier0 || 0));
  setStat('wbTier0Sub', `${fmtNum(tiers.tier1 || 0)} tier-1 pre-checks`);
  setStat('wbFingerprint', fmtNum(d.fingerprintKeys || 0));
  const polls = d.polls || {};
  const cycles = Object.values(polls).map(p => p.cycles || 0);
  setStat('wbCycles', cycles.length ? fmtNum(Math.max(...cycles)) : '—');
  setStat('wbCyclesSub', Object.keys(polls).length ? `${Object.keys(polls).length} source(s) polling` : 'per source');

  // What an operator most needs to know, stated as the OBSERVATION it is.
  // These are since-boot counters and the core's config is not reachable from
  // this app, so "no tier-2 outcomes" is consistent with enforcement being off
  // AND with a recently booted core where nothing has qualified yet. Asserting
  // the first would send someone to change a setting that may be correct.
  if (banner) {
    banner.style.display = d.tier2Observed ? 'none' : '';
    banner.innerHTML = '<b>No tier-2 outcomes recorded since core boot.</b> Tier 2 is the outcome that exists once a match '
      + 'changes crawl priority, so this is what <span class="font-mono text-xs">WANTBRIDGE_ENFORCE=false</span> looks like — '
      + 'but a core that restarted recently with enforcement on and nothing yet qualifying looks the same. '
      + 'Confirm against the core\'s <span class="font-mono text-xs">wantbridge.enforce</span> setting before changing it.';
  }

  if (!tbody) return;
  const rows = (d.wantlists || []);
  if (!rows.length) {
    tbody.innerHTML = '<tr><td colspan="5" class="text-muted text-sm" style="text-align:center;padding:var(--space-8)">No *arr source configured in the core\'s wantbridge section.</td></tr>';
    return;
  }
  tbody.innerHTML = rows.map(w => {
    const poll = polls[w.source] || {};
    const matches = (d.matchesBySource || {})[w.source] || 0;
    return `<tr>
      <td><strong>${escHtml(w.source)}</strong></td>
      <td class="font-mono">${fmtNum(w.size)}</td>
      <td class="font-mono">${fmtNum(matches)}</td>
      <td class="font-mono text-xs">${fmtNum(poll.cycles || 0)}</td>
      <td class="font-mono text-xs">${poll.avgSeconds != null ? `${poll.avgSeconds.toFixed(2)}s` : '—'}</td>
    </tr>`;
  }).join('');
}

/* ── Quarantine ───────────────────────────────────────────────────────── */
// Quarantine holds thousands of rows in practice (junk classifier at scale)
// — paged like Evidence, with page-level selection for bulk restore/delete
// (the core only exposes per-hash endpoints, so bulk = sequential calls).
let qLimit = 50, qOffset = 0;
const _qSelected = new Set();

async function loadQuarantine() {
  const tbody = document.getElementById('quarantineTable');
  _qSelected.clear();
  _qSyncBulkBar();
  const data = await api(`/api/quarantine?limit=${qLimit}&offset=${qOffset}`);
  if (!data) {
    tbody.innerHTML = '<tr><td colspan="6" class="text-muted text-sm" style="text-align:center;padding:var(--space-8)">Quarantine API unavailable.</td></tr>';
    return;
  }
  const win = document.getElementById('quarantineWindow');
  if (win && data.windowDays) win.textContent = data.windowDays;
  const items = data.items || [];
  const total = data.totalCount || 0;
  if (_qSpotCheck) {
    // Re-fetch once at a random offset now that the total is known. Clearing
    // the flag first means a failed re-fetch cannot loop.
    _qSpotCheck = false;
    if (total > qLimit) {
      qOffset = Math.floor(Math.random() * (total - qLimit));
      return loadQuarantine();
    }
  }
  const info = document.getElementById('qPaginationInfo');
  if (info) info.textContent = total
    ? `Showing ${qOffset + 1}–${Math.min(qOffset + qLimit, total)} of ${fmtNum(total)}`
    : 'Showing 0 of 0';
  const prev = document.getElementById('qPrev'), next = document.getElementById('qNext');
  if (prev) prev.disabled = qOffset === 0;
  if (next) next.disabled = qOffset + qLimit >= total;
  const selAll = document.getElementById('qSelectAll');
  if (selAll) selAll.checked = false;
  if (!items.length) {
    tbody.innerHTML = '<tr><td colspan="6" class="text-muted text-sm" style="text-align:center;padding:var(--space-8)">Nothing in quarantine. The junk classifier moves removals here when purge is enabled.</td></tr>';
    return;
  }
  tbody.innerHTML = items.map(it => {
    const conf = it.confidence != null ? `${(it.confidence * 100).toFixed(0)}%` : '--';
    const days = it.daysLeft != null ? it.daysLeft : '--';
    const dayClass = (it.daysLeft != null && it.daysLeft <= 3) ? 'pill-danger' : 'pill-neutral';
    return `<tr>
      <td><input type="checkbox" class="q-check" data-hash="${escHtml(it.infoHash)}" onchange="qToggleSelect(this)"></td>
      <td class="truncate" style="max-width:340px" title="${escHtml(it.torrentName || it.infoHash || '')}"><strong>${escHtml(it.torrentName || it.infoHash || '--')}</strong></td>
      <td class="text-xs text-subtle">${conf}</td>
      <td class="text-xs text-subtle">${fmtDate(it.quarantinedAt)}</td>
      <td><span class="pill ${dayClass}" style="font-size:11px">${days}d</span></td>
      <td>
        <div class="flex gap-2">
          <button class="btn btn-ghost btn-sm" onclick="restoreQuarantine('${escAttrArg(it.infoHash)}')">Restore</button>
          <button class="btn btn-ghost btn-sm" style="color:var(--color-danger)" onclick="deleteQuarantineNow('${escAttrArg(it.infoHash)}')">Delete now</button>
        </div>
      </td>
    </tr>`;
  }).join('');
}

function qPage(dir) {
  qOffset = Math.max(0, qOffset + dir * qLimit);
  loadQuarantine();
}

// The hold runs to tens of thousands of rows and the core's quarantine API
// takes only limit/offset — no search, no sort. Paging from the newest end
// therefore always shows the same recent slice. A random offset is the honest
// way to sample it: 25 rows the operator did not choose, so a systematic
// misjudgement anywhere in the hold can actually surface.
let _qSpotCheck = false;

function qSpotCheck() {
  _qSpotCheck = true;
  qLimit = 25;
  loadQuarantine();
}

function qResetToNewest() {
  _qSpotCheck = false;
  qLimit = 50;
  qOffset = 0;
  loadQuarantine();
}

function qToggleSelect(cb) {
  if (cb.checked) _qSelected.add(cb.dataset.hash); else _qSelected.delete(cb.dataset.hash);
  _qSyncBulkBar();
}

function qToggleSelectAll(master) {
  document.querySelectorAll('#quarantineTable .q-check').forEach(cb => {
    cb.checked = master.checked;
    if (master.checked) _qSelected.add(cb.dataset.hash); else _qSelected.delete(cb.dataset.hash);
  });
  _qSyncBulkBar();
}

function _qSyncBulkBar() {
  const bar = document.getElementById('qBulkBar');
  if (!bar) return;
  bar.style.display = _qSelected.size ? 'flex' : 'none';
  const count = document.getElementById('qBulkCount');
  if (count) count.textContent = `${_qSelected.size} selected`;
}

// Sequential per-hash calls: restore re-inserts + re-classifies on the core,
// so a gentle series beats a parallel burst. Reports success/fail totals.
async function _qBulk(action) {
  const hashes = [..._qSelected];
  if (!hashes.length) return;
  const verb = action === 'restore' ? 'Restore' : 'Permanently delete and blacklist';
  if (!confirm(`${verb} ${hashes.length} torrent${hashes.length > 1 ? 's' : ''}?${action === 'delete' ? ' This cannot be undone.' : ''}`)) return;
  let ok = 0, fail = 0;
  for (const h of hashes) {
    const res = action === 'restore'
      ? await api(`/api/quarantine/${h}/restore`, { method: 'POST' })
      : await api(`/api/quarantine/${h}`, { method: 'DELETE' });
    const want = action === 'restore' ? 'restored' : 'deleted';
    if (res && res.status === want) ok++; else fail++;
  }
  toast(`${action === 'restore' ? 'Restored' : 'Deleted'} ${ok}${fail ? `, ${fail} failed` : ''}`, fail ? 'error' : 'success');
  loadQuarantine();
}

async function restoreQuarantine(hash) {
  if (!confirm('Restore this torrent? It will be re-inserted into the index and re-classified.')) return;
  const res = await api(`/api/quarantine/${hash}/restore`, { method: 'POST' });
  if (res && res.status === 'restored') {
    toast('Restored — re-inserting & re-classifying', 'success');
  } else {
    toast('Restore failed', 'error');
  }
  loadQuarantine();
}

async function deleteQuarantineNow(hash) {
  if (!confirm('Permanently delete and blacklist this torrent now? This cannot be undone.')) return;
  const res = await api(`/api/quarantine/${hash}`, { method: 'DELETE' });
  if (res && res.status === 'deleted') {
    toast('Deleted and blacklisted', 'success');
  } else {
    toast('Delete failed', 'error');
  }
  loadQuarantine();
}

/* ── Evidence ─────────────────────────────────────────────────────────── */
// The event list answers "what happened"; this answers "is each source
// actually reporting", which is the question an operator has and the one the
// list cannot serve — the core's evidence API takes only limit/offset, and
// ~99% of rows are qBittorrent state polls, so the *arr rows that matter are
// unreachable by paging. The counters behind this carry source+kind labels.
async function renderEvidenceSources() {
  const tbody = document.getElementById('evSourcesTable');
  if (!tbody) return;
  const d = await api('/api/evidence/sources');
  if (!d || !d.available) {
    tbody.innerHTML = d === null ? loadFailedRow(5)
      : '<tr><td colspan="5" class="text-muted text-sm" style="text-align:center;padding:var(--space-8)">Core emits no evidence source counters.</td></tr>';
    return;
  }
  const sources = d.sources || [];
  if (!sources.length) {
    tbody.innerHTML = '<tr><td colspan="5" class="text-muted text-sm" style="text-align:center;padding:var(--space-8)">No source has reported an event yet.</td></tr>';
    return;
  }
  tbody.innerHTML = sources.map(src => {
    // qBittorrent is polled by design and never pushes, so an absent webhook
    // is only worth remarking on for an *arr — and even there it is an
    // observation, not a verdict: these counters reset with the core, so a
    // correctly wired *arr that has not grabbed anything since the last
    // restart reads identically to one with no webhook at all.
    const isArr = src.source !== 'qbittorrent';
    const hook = src.webhookEvents > 0
      ? `<span class="pill pill-success"><span class="pill-dot"></span>${fmtNum(src.webhookEvents)} events</span>`
      : isArr
        ? '<span class="pill pill-warning"><span class="pill-dot"></span>none since boot</span>'
        : '<span class="pill pill-neutral"><span class="pill-dot"></span>poll-only</span>';
    const kinds = (src.kinds || []).map(k =>
      `<span class="pill pill-neutral" style="font-size:11px" title="${escHtml(k.kind)}: ${fmtNum(k.received)} received, ${fmtNum(k.persisted)} persisted">${escHtml(k.kind)} ${fmtNum(k.received)}</span>`
    ).join(' ');
    return `<tr>
      <td><strong>${escHtml(src.source || '--')}</strong></td>
      <td>${hook}</td>
      <td class="font-mono">${fmtNum(src.received)}</td>
      <td class="font-mono">${fmtNum(src.persisted)}</td>
      <td><div class="flex gap-2" style="flex-wrap:wrap">${kinds}</div></td>
    </tr>`;
  }).join('');
}

async function loadEvidence() {
  renderEvidenceSources();
  const data = await api(`/api/evidence?limit=${evLimit}&offset=${evOffset}`);
  const tbody = document.getElementById('evidenceTable');
  if (data === null) {  // API failure — don't masquerade as an empty feed
    tbody.innerHTML = loadFailedRow(5);
    document.getElementById('evPaginationInfo').textContent = 'Showing 0 of 0';
    document.getElementById('evPrev').disabled = true;
    document.getElementById('evNext').disabled = true;
    return;
  }
  if (!data.items || data.items.length === 0) {
    tbody.innerHTML = '<tr><td colspan="5" class="text-muted text-sm" style="text-align:center;padding:var(--space-8)">No evidence events yet. The core feeds this from qBittorrent state polls and *arr webhooks/history.</td></tr>';
    document.getElementById('evPaginationInfo').textContent = 'Showing 0 of 0';
    document.getElementById('evPrev').disabled = evOffset === 0;  // allow paging back
    document.getElementById('evNext').disabled = true;
    return;
  }
  const total = data.totalCount || 0;
  document.getElementById('evPaginationInfo').textContent = `Showing ${evOffset + 1}–${Math.min(evOffset + evLimit, total)} of ${fmtNum(total)}`;
  document.getElementById('evPrev').disabled = evOffset === 0;
  document.getElementById('evNext').disabled = evOffset + evLimit >= total;
  tbody.innerHTML = data.items.map(e =>
    `<tr>
      <td class="font-mono text-xs">${fmtAgo(e.timestamp)}</td>
      <td>${escHtml(e.source || '--')}</td>
      <td class="truncate" style="max-width:350px" title="${escHtml(e.torrentName || e.infoHash || '')}">${escHtml(e.torrentName || e.infoHash || '--')}</td>
      <td>${(!e.contentType || e.contentType === 'unknown') ? '<span class="text-xs text-subtle">—</span>' : typePill(e.contentType)}</td>
      <td>${eventPill(e.eventType)}</td>
    </tr>`
  ).join('');
}
function evPage(dir) { evOffset = Math.max(0, evOffset + dir * evLimit); loadEvidence(); }

/* ── Settings ─────────────────────────────────────────────────────────── */
async function loadSettings() {
  const data = await api('/api/settings');
  if (!data) return;
  const grid = document.getElementById('settingsGrid');
  const descriptions = {
    bitagent_graphql_url: 'GraphQL endpoint for the BitAgent core service',
    bitagent_metrics_url: 'Prometheus metrics endpoint',
    tmdb_api_key: 'TMDB API key for poster art in Library',
    log_level: 'Logging verbosity (debug, info, warn, error)',
    torznab_api_key: 'API key for Torznab endpoint auth',
  };
  grid.innerHTML = Object.entries(data.fields).map(([key, f]) => {
    const isSecret = !!f.sensitive;
    const secretState = f.configured ? `configured (${f.source})` : 'not configured';
    const inputValue = isSecret ? '' : (f.current || '');
    const inputPlaceholder = isSecret
      ? (f.configured ? 'Enter a replacement value' : 'Enter a value')
      : (f.default || '');
    return `<div class="setting-item">
      <div class="setting-item-header">
        <span class="setting-key">${escHtml(key)}</span>
        ${f.overridden ? '<span class="pill pill-info setting-override-pill">overridden</span>' : ''}
      </div>
      <div class="setting-default">${isSecret ? 'State' : 'Default'}: <code>${isSecret ? escHtml(secretState) : escHtml(f.default)}</code></div>
      <p class="text-xs text-muted mb-4">${descriptions[key] || ''}</p>
      <div class="flex gap-2">
        <input class="input font-mono" id="setting-${key}" data-sensitive="${isSecret}" type="${isSecret ? 'password' : 'text'}" value="${escHtml(inputValue)}" style="flex:1" placeholder="${escHtml(inputPlaceholder)}" autocomplete="off">
      </div>
      <div class="setting-actions">
        <button class="btn btn-primary btn-sm" onclick="saveSetting('${key}')">Save</button>
        ${f.overridden ? `<button class="btn btn-ghost btn-sm" style="color:var(--color-danger)" onclick="resetSetting('${key}')">Reset</button>` : ''}
      </div>
    </div>`;
  }).join('');

  // Sensitive settings are state-only. Values never leave the server; blank
  // inputs mean "preserve", and a typed value explicitly replaces the secret.
  const fields = data.fields || {};
  const tzConfigured = !!((fields.torznab_api_key || {}).configured);
  const tzEl = document.getElementById('tzKeyDisplay');
  if (tzEl) { tzEl.value = ''; tzEl.placeholder = tzConfigured ? 'configured (value hidden)' : 'not set'; }
  const tmdbConfigured = !!((fields.tmdb_api_key || {}).configured);
  const tmdbPill = document.getElementById('tmdbStatus');
  if (tmdbPill) {
    tmdbPill.textContent = tmdbConfigured ? 'Configured' : 'Not configured';
    tmdbPill.className = `pill ${tmdbConfigured ? 'pill-success' : 'pill-neutral'}`;
  }
  // The dashboard API key is env-only (not a mutable setting) — reflect
  // whether the tier is active rather than pretending to know the value.
  const tiers = await api('/api/auth/tiers');
  const dashEl = document.getElementById('dashKeyDisplay');
  if (dashEl && tiers) {
    dashEl.value = '';
    dashEl.placeholder = tiers.apiKey ? 'configured via env (value not exposed)' : 'not set';
  }

  loadAuditLog();
  loadAuthStatus();
}

async function saveSetting(key) {
  const input = document.getElementById(`setting-${key}`);
  const value = input.value;
  if (input.dataset.sensitive === 'true' && !value) {
    toast('Enter a replacement value; blank preserves the existing secret', 'error');
    return;
  }
  const r = await api(`/api/settings/overrides/${key}`, { method: 'PUT', body: JSON.stringify({ value }) });
  if (r) { toast(`Setting "${key}" saved`, 'success'); loadSettings(); }
  else toast(`Failed to save "${key}"`, 'error');
}
async function resetSetting(key) {
  const r = await api(`/api/settings/overrides/${key}`, { method: 'DELETE' });
  if (r) { toast(`Setting "${key}" reset to default`, 'success'); loadSettings(); }
  else toast(`Failed to reset "${key}"`, 'error');
}

async function loadAuditLog() {
  const data = await api('/api/settings/audit?limit=50');
  const container = document.getElementById('auditLog');
  if (data === null) {
    container.innerHTML = '<p class="text-sm" style="text-align:center;padding:var(--space-6);color:var(--color-danger)">Couldn\'t load the audit log — the request failed.</p>';
    return;
  }
  if (!data || data.length === 0) {
    container.innerHTML = '<p class="text-sm text-muted" style="text-align:center;padding:var(--space-6)">No configuration changes recorded yet.</p>';
    return;
  }
  container.innerHTML = data.map(a =>
    `<div class="audit-entry">
      <div class="audit-dot"></div>
      <div style="flex:1">
        <div class="flex items-center justify-between">
          <span class="text-sm"><strong>${escHtml(a.key)}</strong> changed by <em>${escHtml(a.actor)}</em></span>
          <span class="audit-time">${fmtDate(a.at)}</span>
        </div>
        <div class="text-xs text-muted mt-2">
          ${a.old != null ? `<span style="color:var(--color-danger)">- ${escHtml(a.old.length > 40 ? a.old.slice(0, 40) + '...' : a.old)}</span><br>` : ''}
          ${a.new != null ? `<span style="color:var(--color-success)">+ ${escHtml(typeof a.new === 'string' && a.new.length > 40 ? a.new.slice(0, 40) + '...' : (a.new || '(deleted)'))}</span>` : '<span style="color:var(--color-danger)">(deleted)</span>'}
        </div>
      </div>
    </div>`
  ).join('');
}

async function loadAuthStatus() {
  const updatePill = (id, active) => {
    const el = document.getElementById(id);
    if (!el) return;
    el.innerHTML = active
      ? '<span class="pill pill-success"><span class="pill-dot"></span>Active</span>'
      : '<span class="pill pill-neutral">Disabled</span>';
  };
  // Reflect startup-only trust config from the backend resolver.
  const tiers = await api('/api/auth/tiers');
  if (!tiers) {
    // Couldn't reach the endpoint — show unknown rather than a fictional status.
    ['authTierApi', 'authTierNpm', 'authTierFwd', 'authTierSso'].forEach(id => {
      const el = document.getElementById(id);
      if (el) el.innerHTML = '<span class="pill pill-neutral">--</span>';
    });
    return;
  }
  updatePill('authTierApi', !!tiers.apiKey);
  updatePill('authTierNpm', !!tiers.npmHeaders);
  updatePill('authTierFwd', !!tiers.forwardedUser);
  updatePill('authTierSso', !!tiers.sso);
}

function switchSettingsTab(tab) {
  document.querySelectorAll('#tab-settings .tab-btn').forEach(b => b.classList.toggle('active', b.dataset.stab === tab));
  document.querySelectorAll('#tab-settings .tab-panel').forEach(p => {
    if (p.id && p.id.startsWith('stab-')) p.classList.toggle('active', p.id === `stab-${tab}`);
  });
  if (tab === 'audit') loadAuditLog();
  if (tab === 'classifier') loadClassifierRules();
  if (tab === 'liveness') loadLivenessOps();
  if (tab === 'filters') loadFiltersStatus();
  if (tab === 'blocklists') loadBlockLists();
  if (tab === 'integrations') loadArrSettings();
}

async function loadFiltersStatus() {
  const data = await api('/api/filters/status');
  const set = (id, val) => { const el = document.getElementById(id); if (el) el.textContent = val; };
  const setPill = (id, status) => {
    const el = document.getElementById(id);
    if (!el) return;
    const tones = {
      success: 'pill-success', warning: 'pill-warning',
      danger: 'pill-danger', info: 'pill-info', neutral: 'pill-neutral',
    };
    const cls = tones[status.tone] || 'pill-neutral';
    el.innerHTML = `<span class="pill ${cls}" title="${escHtml(status.detail || '')}">${escHtml(status.label || 'Unknown')}</span>`;
  };

  if (!data || data.available === false) {
    const unavailable = {
      label: 'Unavailable', tone: 'danger',
      detail: data?.status?.detail || 'The metrics request failed or timed out.',
    };
    ['filterNsfwToggle', 'filterNonLatinToggle', 'filterExtToggle']
      .forEach(id => setPill(id, unavailable));
    ['filterNsfwCount', 'filterNonLatinCount', 'filterExtCount', 'filterExamined']
      .forEach(id => set(id, '—'));
    ['filterNsfwCountLabel', 'filterNonLatinCountLabel', 'filterExtCountLabel']
      .forEach(id => set(id, 'Decision count'));
    set('filterExtTotal', 'unavailable');
    const ext = document.getElementById('filterExtBreakdown');
    if (ext) ext.innerHTML = '<div class="empty-state" style="padding:var(--space-6)"><p class="text-sm" style="color:var(--color-danger)">Couldn’t load content-filter metrics.</p></div>';
    return;
  }

  const status = data.status || { key: 'inactive', label: 'No activity', tone: 'neutral', detail: '' };
  const shadow = status.key === 'shadow';
  const counts = shadow ? (data.wouldDrops || {}) : (data.drops || {});
  const countLabel = shadow ? 'Would drop (shadow)' : 'Enforced drops';
  set('filterNsfwCount', fmtNum(counts.nsfw_keyword || 0));
  set('filterNonLatinCount', fmtNum(counts.non_latin_script || 0));
  set('filterExtCount', fmtNum(counts.blocked_extension || 0));
  set('filterExamined', fmtNum(data.examined || 0));
  ['filterNsfwCountLabel', 'filterNonLatinCountLabel', 'filterExtCountLabel']
    .forEach(id => set(id, countLabel));
  ['filterNsfwToggle', 'filterNonLatinToggle', 'filterExtToggle']
    .forEach(id => setPill(id, status));

  const exts = data.blockedExtensions || [];
  const ext = document.getElementById('filterExtBreakdown');
  if (ext) {
    if (exts.length === 0) {
      const message = shadow
        ? `${fmtNum((data.wouldDrops || {}).blocked_extension || 0)} blocked-extension would-drop decisions were observed, but no per-extension series were emitted.`
        : 'No enforced per-extension drops have been recorded.';
      ext.innerHTML = `<div class="empty-state" style="padding:var(--space-6)"><p class="text-sm text-muted">${escHtml(message)}</p></div>`;
    } else {
      ext.innerHTML = exts.map(e => `
        <div class="ext-cell">
          <span class="ext-name font-mono">.${escHtml(e.ext)}</span>
          <span class="ext-count font-mono">${fmtNum(e.count)}</span>
        </div>`).join('');
    }
    set('filterExtTotal', `${exts.length} extension${exts.length === 1 ? '' : 's'} observed${shadow ? ' in shadow' : ''}`);
  }
}

async function loadBlockLists() {
  const [stats, filters] = await Promise.all([api('/api/stats'), api('/api/filters/status')]);
  const set = (id, val) => { const el = document.getElementById(id); if (el) el.textContent = val; };
  _renderMetric('blListLivenessSize', stats, 'livenessBlacklistSize');
  _renderMetric('blListLivenessExcluded', stats, 'livenessTotalExcluded');
  _renderMetric('blListLivenessRate', stats, 'livenessBlockRatePerMin', fmtRatePerMin);
  if (filters && filters.csam) {
    set('blListCsamEntries', fmtNum(filters.csam.blocklistEntries || 0));
    set('blListCsamLookups', fmtNum(filters.csam.lookups || 0));
    set('blListCsamExports', fmtNum(filters.csam.exports || 0));
  }
  await loadBlockPhrasesTable();
}

async function loadBlockPhrasesTable() {
  const data = await api('/api/block-phrases');
  const tbody = document.getElementById('blockPhraseTable');
  if (!tbody) return;
  if (data === null) { tbody.innerHTML = loadFailedRow(5); return; }
  if (!data || !data.items || data.items.length === 0) {
    tbody.innerHTML = `<tr><td colspan="5"><div class="empty-state" style="padding:var(--space-6)">
      <p class="text-sm text-muted">No phrases yet. Add a phrase above and it'll be filtered out of the Library tab on the next refresh.</p>
    </div></td></tr>`;
    return;
  }
  tbody.innerHTML = data.items.map(p => `
    <tr>
      <td><code class="font-mono text-sm">${escHtml(p.pattern)}</code></td>
      <td class="text-sm text-muted">${escHtml(p.note || '—')}</td>
      <td class="font-mono text-sm" style="text-align:right">${fmtNum(p.hits)}</td>
      <td class="text-xs text-subtle">${fmtAgo(p.createdAt)}</td>
      <td style="text-align:right">
        <button class="btn btn-secondary btn-sm" onclick="deleteBlockPhrase(${p.id})">
          <svg width="12" height="12" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2"><polyline points="3 6 5 6 21 6"/><path d="M19 6v14a2 2 0 01-2 2H7a2 2 0 01-2-2V6m3 0V4a2 2 0 012-2h4a2 2 0 012 2v2"/></svg>
        </button>
      </td>
    </tr>`).join('');
}

async function addBlockPhrase() {
  const input = document.getElementById('blockPhraseInput');
  const noteInput = document.getElementById('blockPhraseNote');
  const errEl = document.getElementById('blockPhraseError');
  const pattern = (input.value || '').trim();
  const note = (noteInput.value || '').trim();
  if (!pattern) { input.focus(); return; }
  errEl.style.display = 'none';
  errEl.textContent = '';
  try {
    const resp = await fetch(API + '/api/block-phrases', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ pattern, note }),
    });
    if (!resp.ok) {
      const body = await resp.json().catch(() => ({}));
      errEl.textContent = body.detail || `${resp.status} ${resp.statusText}`;
      errEl.style.display = 'block';
      return;
    }
    input.value = '';
    noteInput.value = '';
    await loadBlockPhrasesTable();
  } catch (e) {
    errEl.textContent = String(e);
    errEl.style.display = 'block';
  }
}

async function deleteBlockPhrase(id) {
  await fetch(API + '/api/block-phrases/' + id, { method: 'DELETE' });
  await loadBlockPhrasesTable();
}

async function loadLivenessOps() {
  const stats = await api('/api/stats');
  _renderMetric('livenessObsAlive', stats, 'livenessObservationsAlive');
  _renderMetric('livenessObsSuspect', stats, 'livenessObservationsSuspect');
  _renderMetric('livenessBlacklist', stats, 'livenessBlacklistSize');
  _renderMetric('livenessExcluded', stats, 'livenessTotalExcluded');
  _renderMetric('livenessRevalidAlive', stats, 'livenessRevalidationsAlive');
  _renderMetric('livenessRevalidDead', stats, 'livenessRevalidationsDead');
  _renderMetric(
    'livenessRecoveryRate', stats, 'livenessRecoveryRate',
    value => `${(value * 100).toFixed(1)}%`,
  );
}

async function loadArrSettings() {
  const data = await api('/api/settings');
  if (!data) return;
  const fields = data.fields || {};
  for (const arr of ['sonarr', 'radarr', 'lidarr']) {
    const urlEl = document.getElementById(`arr-${arr}-url`);
    const keyEl = document.getElementById(`arr-${arr}-key`);
    if (urlEl && fields[`${arr}_base_url`]) urlEl.value = fields[`${arr}_base_url`].current || '';
    if (keyEl && fields[`${arr}_api_key`]) {
      const field = fields[`${arr}_api_key`];
      keyEl.value = '';
      keyEl.placeholder = field.configured ? 'configured — enter to replace' : 'Enter API key';
    }
  }
}

async function saveArrSettings(arr) {
  const urlEl = document.getElementById(`arr-${arr}-url`);
  const keyEl = document.getElementById(`arr-${arr}-key`);
  const url = urlEl?.value?.trim();
  const key = keyEl?.value?.trim();
  if (!url && !key) { toast('Enter at least URL or key', 'error'); return; }
  const saves = [];
  if (url) saves.push(api(`/api/settings/overrides/${arr}_base_url`, { method: 'PUT', body: JSON.stringify({ value: url }) }));
  if (key) saves.push(api(`/api/settings/overrides/${arr}_api_key`, { method: 'PUT', body: JSON.stringify({ value: key }) }));
  const results = await Promise.all(saves);
  const name = arr.charAt(0).toUpperCase() + arr.slice(1);
  if (results.every(Boolean)) {
    toast(`${name} settings saved`, 'success');
    if (keyEl) keyEl.value = '';
    loadArrSettings();
  }
  else toast(`Failed to save ${name} settings`, 'error');
}

async function saveTmdbKey() {
  const value = document.getElementById('tmdbKeyInput').value;
  if (!value) { toast('Enter a TMDB API key', 'error'); return; }
  const r = await api('/api/settings/overrides/tmdb_api_key', { method: 'PUT', body: JSON.stringify({ value }) });
  if (r) {
    toast('TMDB key saved', 'success');
    document.getElementById('tmdbKeyInput').value = '';
    document.getElementById('tmdbStatus').textContent = 'Configured';
    document.getElementById('tmdbStatus').className = 'pill pill-success';
  }
  else toast('Failed to save TMDB key', 'error');
}

/* ── AI (LLM cost + stage scorecards) ─────────────────────────────────── */
async function loadAiTab() {
  const loadSeq = ++_aiLoadSeq;
  const summary = await api('/api/ai/summary');
  if (loadSeq !== _aiLoadSeq) return;
  renderAiSummary(summary);
}

// Format one scorecard metric. A null value is a metric the core does not
// emit — it renders as an em-dash with the reason in `detail`, never as 0.
function _fmtStageValue(value, format) {
  if (value === null || value === undefined || isNaN(value)) return '—';
  if (format === 'percent') {
    const pct = value * 100;
    // A genuinely tiny share is information ("this model does almost no
    // work"); rounding it to 0.0% throws that away.
    if (pct > 0 && pct < 0.1) return '<0.1%';
    return `${pct.toFixed(1)}%`;
  }
  if (format === 'seconds') {
    return value >= 100 ? `${Math.round(value)}s` : `${value.toFixed(value < 10 ? 2 : 1)}s`;
  }
  return fmtNum(Math.round(value));
}

function renderLlmStages(stages) {
  const grid = document.getElementById('aiStageGrid');
  if (!grid) return;
  if (!Array.isArray(stages) || !stages.length) {
    grid.innerHTML = '<div class="empty-state" style="padding:var(--space-8)"><p class="text-sm text-muted">No LLM metrics reported by the core.</p></div>';
    return;
  }
  grid.innerHTML = stages.map(stage => {
    const status = stage.status || {
      key: stage.available ? 'active' : 'inactive',
      label: stage.available ? 'Activity observed' : 'No activity observed',
      tone: stage.available ? 'info' : 'neutral',
      detail: stage.available ? '' : 'No calls recorded since core boot.',
    };
    const statusTones = {
      success: 'pill-success', warning: 'pill-warning',
      danger: 'pill-danger', info: 'pill-info', neutral: 'pill-neutral',
    };
    const statusPill = `<span class="pill ${statusTones[status.tone] || 'pill-neutral'}">${escHtml(status.label)}</span>`;
    const model = stage.model
      ? `<span class="llm-model" title="From the metric's own model label">${escHtml(stage.model)}</span>`
      : `<span class="llm-model llm-model--unknown" title="${escHtml(stage.modelNote || '')}">${stage.available ? 'model not labelled' : 'LLM not observed'}</span>`;
    const rows = (stage.metrics || []).map(m => `
      <div class="llm-metric" data-tone="${escHtml(m.tone || 'neutral')}"${m.help ? ` data-help="${escHtml(m.help)}"` : ''}>
        <div class="llm-metric-label">${escHtml(m.label)}</div>
        <div class="llm-metric-value">${escHtml(_fmtStageValue(m.value, m.format))}</div>
        <div class="llm-metric-detail">${escHtml(m.detail || '')}</div>
      </div>`).join('');
    const sp = stage.spend;
    // A stage with no token metric has no spend footer at all — an empty one
    // would read as "this model is free".
    const spend = sp ? `<div class="llm-stage-spend"${
      sp.usdPerUnit != null
        ? ` data-help="Spend-to-date for this stage divided by the work it actually did. This is the number that compares stages: a stage can be small in total and still be the worst value per decision."`
        : ''
      }>
        <span>Spend <b>${escHtml(_fmtUsd(sp.usd))}</b></span>
        <span>${sp.priced
          ? (sp.monthlyUsd != null ? `<b>${escHtml(_fmtUsd(sp.monthlyUsd))}</b>/mo` : 'measuring…')
          : 'model has no price on file'}</span>
        ${sp.usdPerUnit != null
          ? `<span><b>${escHtml(_fmtUsdPrecise(sp.usdPerUnit))}</b> ${escHtml(sp.unitLabel)} · ${fmtNum(sp.unitCount)} total</span>`
          : ''}
      </div>` : '';
    const subdued = status.key === 'inactive' || status.key === 'unavailable';
    return `<div class="llm-stage" data-stage="${escHtml(stage.id)}" data-state="${escHtml(status.key)}"${subdued ? ' data-idle="1"' : ''}>
      <div class="llm-stage-head">
        <div>
          <div class="llm-stage-name">${escHtml(stage.name)}</div>
          <div class="llm-stage-role">${escHtml(stage.role)}</div>
        </div>
        <div class="llm-stage-badges">${statusPill}${model}</div>
      </div>
      <div class="llm-stage-status" data-tone="${escHtml(status.tone || 'neutral')}">${escHtml(status.detail || '')}</div>
      <div class="llm-metric-grid">${rows}</div>
      ${spend}
    </div>`;
  }).join('');
  installHelpDots(grid);
}

// Money is the one number on this page that must never be guessed. A model
// with no price entry contributes tokens and no dollars, and the total is then
// rendered as a floor with the unpriced models named — a changed model pin
// must not make the bill read as cheap.
function _fmtUsd(v) {
  if (v === null || v === undefined || isNaN(v)) return '—';
  if (v > 0 && v < 0.01) return '<$0.01';
  return v >= 100 ? `$${Math.round(v).toLocaleString()}` : `$${v.toFixed(2)}`;
}

// Unit costs run to fractions of a cent, and exponential notation ($7.9e-5)
// is unreadable next to a sibling figure like $0.0016 — the whole point of
// this line is comparing the two at a glance.
function _fmtUsdPrecise(v) {
  if (v === null || v === undefined || isNaN(v)) return '—';
  if (v === 0) return '$0';
  if (v >= 1) return `$${v.toFixed(2)}`;
  if (v >= 0.001) return `$${v.toFixed(4)}`;
  if (v >= 0.0000001) return `$${v.toFixed(7).replace(/0+$/, '')}`;
  return '<$0.0000001';
}

function renderAiSpend(spend) {
  const set = (id, text) => { const el = document.getElementById(id); if (el) el.textContent = text; };
  const panel = document.getElementById('aiSpendPanel');
  const prices = document.getElementById('aiSpendPrices');

  if (!spend || !spend.available) {
    // Every sub-label the success path writes, not just the headlines — a
    // stale "9,029,681 in / 323,992 out" under an em-dash describes a poll
    // that already failed.
    _blankStats({
      aiSpendMonthly: '—', aiSpendSub: 'no stage reports tokens',
      aiTokensTotal: '—', aiTokensSub: 'in / out',
      aiSpendTotal: '—', aiSpendTotalSub: 'since core restart',
      aiModelCount: '—', aiModelSub: 'reporting tokens',
    });
    _renderMeter('aiSpendMeter', 0, 'neutral');
    if (panel) panel.innerHTML = '<div class="empty-state" style="padding:var(--space-6)"><p class="text-sm text-muted">No LLM stage is reporting token counts, so spend cannot be derived.</p></div>';
    return;
  }

  const rows = spend.byModel || [];
  const partial = !!spend.totalIsPartial;
  const inTok = rows.reduce((a, r) => a + (r.inputTokens || 0), 0);
  const outTok = rows.reduce((a, r) => a + (r.outputTokens || 0), 0);

  // Monthly is MEASURED across polls, not extrapolated from one scrape — the
  // core publishes no process-start gauge, so a single read cannot know the
  // window the counters cover. A stage that judges in hourly cycles therefore
  // contributes nothing to a short window, so any model still unmeasured makes
  // the total a FLOOR ("≥"), never a confident number that reads under budget.
  const monthly = spend.monthlyUsd;
  const budget = spend.budgetUsd;
  const ratio = spend.budgetRatio;
  const measuring = spend.monthlyMeasuring || [];
  const mins = Math.round((spend.monthlyWindowSeconds || 0) / 60);
  const floor = !!spend.monthlyIsPartial;
  if (monthly === null || monthly === undefined) {
    set('aiSpendMonthly', 'measuring…');
    set('aiSpendSub', mins >= 1 ? `sampling for ${mins} min` : 'needs two samples 10s apart');
    _renderMeter('aiSpendMeter', 0, 'neutral');
  } else {
    set('aiSpendMonthly', (floor ? '≥ ' : '') + _fmtUsd(monthly));
    const budgetTxt = budget
      ? `${_fmtUsd(budget)} budget${ratio != null ? ` · ${(ratio * 100).toFixed(0)}%` : ''}`
      : 'no budget configured';
    set('aiSpendSub', floor
      ? `${budgetTxt} · still measuring ${measuring.join(', ')}`
      : `${budgetTxt} · measured over ${mins} min`);
    // Over budget is the whole point of the card, so clamp the BAR but never
    // the number — and a floor stays neutral, because "at least 86% of budget"
    // is not evidence of being under it.
    _renderMeter('aiSpendMeter', ratio != null ? Math.min(ratio, 1) : 0,
      ratio == null || floor ? 'neutral' : ratio > 1 ? 'bad' : ratio > 0.8 ? 'warn' : 'good');
  }

  set('aiTokensTotal', fmtNum(inTok + outTok));
  set('aiTokensSub', `${fmtNum(inTok)} in / ${fmtNum(outTok)} out`);
  set('aiSpendTotal', (partial ? '≥ ' : '') + _fmtUsd(spend.totalUsd));
  set('aiSpendTotalSub', partial
    ? `unpriced: ${(spend.unpricedModels || []).join(', ')}`
    : 'since core restart');
  set('aiModelCount', String(rows.length));
  set('aiModelSub', rows.length ? rows.map(r => r.stage).join(' · ') : 'reporting tokens');
  if (prices) prices.textContent = `list prices as of ${spend.pricesAsOf || '—'}`;

  if (!panel) return;
  if (!rows.length) {
    panel.innerHTML = '<div class="empty-state" style="padding:var(--space-6)"><p class="text-sm text-muted">No model has recorded tokens yet.</p></div>';
    return;
  }
  const maxUsd = Math.max(...rows.map(r => r.usd || 0), 0.0000001);
  panel.innerHTML = rows.map(r => {
    const width = r.priced ? ((r.usd / maxUsd) * 100).toFixed(1) : 0;
    const colour = r.priced ? 'var(--color-accent)' : 'var(--color-border)';
    const monthlyTxt = r.priced
      ? (r.monthlyUsd != null ? `${_fmtUsd(r.monthlyUsd)}/mo` : 'measuring…')
      : 'no price on file';
    return `<div class="cat-row cat-row--flat">
      <div class="cat-label font-mono text-xs" title="${escHtml(r.model || 'unlabelled')} — ${escHtml(r.stage)}">${escHtml(r.model || 'unlabelled')}</div>
      <div class="cat-bar"><div class="cat-bar-fill" style="width:${width}%;background:${colour}"></div></div>
      <div class="cat-count">${_fmtUsd(r.usd)}</div>
      <div class="cat-pct text-xs text-subtle" style="min-width:96px;text-align:right">${escHtml(monthlyTxt)}</div>
    </div>`;
  }).join('');
}

function renderAiSummary(s) {
  renderAiSpend(s && s.spend);
  renderLlmStages(s && s.stages);

  const note = document.getElementById('aiUnavailableNote');
  const showNote = (text, tone = 'warn') => {
    if (!note) return;
    note.textContent = text;
    note.className = `callout callout-${tone} mb-6`;
    note.style.display = '';
  };
  const blankMatcher = () => {
    _blankStats({
      aiLiveMatches: '—', aiLiveMatchesSub: 'movie / TV',
      aiCacheRatio: '—', aiCacheSub: 'hits / misses',
    });
    _renderMeter('aiCacheMeter', 0, 'neutral');
    const empty = '<div class="empty-state" style="padding:var(--space-6)"><p class="text-sm text-muted">No matcher metrics yet.</p></div>';
    document.getElementById('aiGatePanel').innerHTML = empty;
    document.getElementById('aiAnimePanel').innerHTML = empty;
  };

  if (!s) {
    showNote('AI metrics request failed or timed out. Values were cleared; refresh to retry.', 'danger');
    blankMatcher();
    const grid = document.getElementById('aiStageGrid');
    if (grid) grid.innerHTML = '<div class="empty-state" style="padding:var(--space-8)"><p class="text-sm" style="color:var(--color-danger)">Couldn’t load AI stage metrics.</p></div>';
    return;
  }
  if (s.telemetryAvailable === false) {
    showNote('The core metrics endpoint is unavailable, so all AI stage states are unknown.', 'danger');
    blankMatcher();
    return;
  }
  if (!s.available) {
    const matcher = (s.stages || []).find(stage => stage.id === 'matcher');
    const status = matcher?.status;
    showNote(`TMDB matcher: ${status?.label || 'no activity observed'}. ${status?.detail || 'No matcher calls were recorded since core boot.'}`);
    blankMatcher();
    return;
  }
  if (note) note.style.display = 'none';

  // Live / shadow attaches by media type
  const live = (s.matches && s.matches.live) || { byType: {}, total: 0 };
  const shadow = (s.matches && s.matches.shadow) || { byType: {}, total: 0 };
  document.getElementById('aiLiveMatches').textContent = fmtNum(live.total);
  document.getElementById('aiLiveMatchesSub').textContent =
    `${fmtNum(live.byType.movie || 0)} movie / ${fmtNum(live.byType.tv || 0)} TV`;
  // Matcher decision cache. Rerank rate, extract outcomes and per-stage
  // latency moved into the matcher scorecard above, next to the other two
  // models, so all three read on the same axes.
  const cache = s.cache || {};
  const lookups = (cache.hits || 0) + (cache.misses || 0);
  document.getElementById('aiCacheRatio').textContent = lookups > 0 ? `${(cache.hitRatio * 100).toFixed(1)}%` : '—';
  document.getElementById('aiCacheSub').textContent = lookups > 0
    ? `${fmtNum(cache.hits)} hits / ${fmtNum(cache.misses)} misses` : 'hits / misses';
  _renderMeter('aiCacheMeter', lookups > 0 ? cache.hitRatio : 0,
    _toneForRatio(lookups > 0 ? cache.hitRatio : null, 0.3, 0.05));

  // Gate rejects breakdown (bar per gate, scaled to the largest)
  const gates = s.gateRejects || {};
  const gateKeys = Object.keys(gates).sort((a, b) => gates[b] - gates[a]);
  const gatePanel = document.getElementById('aiGatePanel');
  if (!gateKeys.length) {
    gatePanel.innerHTML = '<div class="empty-state" style="padding:var(--space-6)"><p class="text-sm text-muted">No gate rejects recorded yet.</p></div>';
  } else {
    const max = Math.max(...gateKeys.map(k => gates[k]), 1);
    // cat-row--flat: 3-cell variant — the default .cat-row grid expects
    // icon/label/bar/count/pct, which shoved these labels into the 22px
    // icon column ("plausibility" rendered as "p...").
    gatePanel.innerHTML = gateKeys.map(k =>
      `<div class="cat-row cat-row--flat">
        <div class="cat-label" title="${escHtml(k)}">${escHtml(k)}</div>
        <div class="cat-bar"><div class="cat-bar-fill" style="width:${((gates[k] / max) * 100).toFixed(1)}%;background:var(--color-warning)"></div></div>
        <div class="cat-count">${fmtNum(gates[k])}</div>
      </div>`
    ).join('');
  }

  // Anime English gate table
  const anime = s.anime || { kept: 0, rejected: 0, byEnglish: {} };
  const animePanel = document.getElementById('aiAnimePanel');
  const engKeys = Object.keys(anime.byEnglish || {});
  if (!engKeys.length) {
    animePanel.innerHTML = '<div class="empty-state" style="padding:var(--space-6)"><p class="text-sm text-muted">No anime observed by the matcher yet.</p></div>';
  } else {
    const order = ['dub', 'sub', 'none', 'unknown'];
    engKeys.sort((a, b) => order.indexOf(a) - order.indexOf(b));
    animePanel.innerHTML = `
      <table>
        <thead><tr><th>English track</th><th style="text-align:right">Kept</th><th style="text-align:right">Rejected</th></tr></thead>
        <tbody>
          ${engKeys.map(k => {
            const e = anime.byEnglish[k] || {};
            return `<tr>
              <td><span class="pill pill-neutral">${escHtml(k)}</span></td>
              <td class="font-mono text-sm" style="text-align:right">${fmtNum(e.kept || 0)}</td>
              <td class="font-mono text-sm" style="text-align:right;color:${(e.rejected || 0) > 0 ? 'var(--color-danger)' : 'inherit'}">${fmtNum(e.rejected || 0)}</td>
            </tr>`;
          }).join('')}
          <tr>
            <td class="text-sm" style="font-weight:var(--weight-semibold)">Total</td>
            <td class="font-mono text-sm" style="text-align:right;font-weight:var(--weight-semibold)">${fmtNum(anime.kept)}</td>
            <td class="font-mono text-sm" style="text-align:right;font-weight:var(--weight-semibold)">${fmtNum(anime.rejected)}</td>
          </tr>
        </tbody>
      </table>`;
  }
}

/* ── System ────────────────────────────────────────────────────────────── */
function loadSystem() { loadRawMetrics(); _loadSystemStatus(); }

// Health-check + network cards: everything here is fetched, not asserted.
// Core/SQLite pills consume the source probes in /api/stats. Torznab has no
// dedicated unauthenticated health probe, so it is explicitly not inferred
// from GraphQL reachability.
async function _loadSystemStatus() {
  const [stats, tiers, settings] = await Promise.all([
    api('/api/stats'), api('/api/auth/tiers'), api('/api/settings'),
  ]);
  _setProbePill('sysGql', stats, 'graphqlReachable', 'OK', 'Unreachable');
  _setProbePill('sysMetrics', stats, 'metricsReachable', 'OK', 'Unreachable');
  _setProbePill('sysDb', stats, 'sidecarDbReachable', 'Connected', 'Error');
  const torznab = document.getElementById('sysTz');
  if (torznab) {
    torznab.className = 'pill pill-neutral';
    torznab.innerHTML = '<span class="pill-dot"></span> Not probed';
    torznab.title = 'GraphQL reachability does not verify the Torznab route';
  }

  const authEl = document.getElementById('sysAuthMode');
  if (authEl && tiers) {
    const active = [
      tiers.sso && 'SSO', tiers.forwardedUser && 'Fwd headers',
      tiers.npmHeaders && 'NPM headers', tiers.apiKey && 'API key',
    ].filter(Boolean);
    authEl.className = `pill ${active.length ? 'pill-info' : 'pill-warning'}`;
    authEl.textContent = active.length ? active.join(' + ') : 'Open (no auth)';
  }

  const fields = (settings && settings.fields) || {};
  const setUrl = (id, field) => {
    const el = document.getElementById(id);
    const val = (fields[field] && fields[field].current) || '--';
    if (el) { el.textContent = val; el.title = val; }
  };
  setUrl('sysCoreUrl', 'bitagent_graphql_url');
  setUrl('sysMetricsUrl', 'bitagent_metrics_url');
  const dashEl = document.getElementById('sysDashOrigin');
  if (dashEl) dashEl.textContent = window.location.host;
}

function switchSystemTab(tab) {
  document.querySelectorAll('#tab-system .tab-btn').forEach(b => b.classList.toggle('active', b.dataset.systab === tab));
  document.querySelectorAll('#tab-system > .tab-panel, #tab-system .tab-panel').forEach(p => {
    if (p.id && p.id.startsWith('systab-')) p.classList.toggle('active', p.id === `systab-${tab}`);
  });
  if (tab === 'metrics') loadRawMetrics();
}

async function runHealthChecks() {
  const container = document.getElementById('healthCheckResults');
  container.innerHTML = '<div class="skeleton" style="height:100px"></div>';
  const checks = [
    { name: 'Dashboard API', url: '/healthz' },
    { name: 'Auth Endpoint', url: '/api/me' },
    { name: 'GraphQL Proxy', url: '/api/stats' },
    { name: 'Metrics Proxy', url: '/api/metrics' },
  ];
  const results = await Promise.all(checks.map(async c => {
    const t0 = performance.now();
    try {
      const r = await fetch(c.url);
      const ms = (performance.now() - t0).toFixed(0);
      return { ...c, ok: r.ok, status: r.status, ms };
    } catch (e) {
      return { ...c, ok: false, status: 'ERR', ms: '--' };
    }
  }));
  container.innerHTML = results.map(r =>
    `<div class="flex items-center justify-between" style="padding:var(--space-3) 0;border-bottom:1px solid var(--color-border-subtle)">
      <div class="flex items-center gap-3">
        <span class="pill ${r.ok ? 'pill-success' : 'pill-danger'}"><span class="pill-dot"></span>${r.ok ? 'OK' : 'FAIL'}</span>
        <span class="text-sm">${r.name}</span>
      </div>
      <div class="flex items-center gap-3">
        <span class="font-mono text-xs">${r.status}</span>
        <span class="text-xs text-subtle">${r.ms}ms</span>
      </div>
    </div>`
  ).join('');
}

async function runTorznabTest() {
  // The dashboard has no Torznab proxy — this exercises the same GraphQL-backed
  // search the Library tab uses. The "Search Type" now actually scopes the query
  // by contentType (previously it was read and thrown away).
  const type = document.getElementById('tzSearchType').value;
  const q = document.getElementById('tzQuery').value;
  const result = document.getElementById('tzResult');
  result.style.display = 'block';
  result.textContent = 'Executing…';
  const params = new URLSearchParams({ q, limit: '10' });
  if (type) params.set('content_types', type);
  const data = await api(`/api/torrents?${params.toString()}`, { timeoutMs: 35_000 });
  result.textContent = data ? JSON.stringify(data, null, 2) : 'Request failed — the core may be unreachable.';
}

async function runGqlQuery() {
  const q = document.getElementById('gqlQuery').value;
  let vars = {};
  try { vars = JSON.parse(document.getElementById('gqlVars').value || '{}'); } catch (e) {}
  const result = document.getElementById('gqlResult');
  result.textContent = 'Executing...';
  const data = await api('/api/graphql', { method: 'POST', body: JSON.stringify({ query: q, variables: vars }) });
  result.textContent = JSON.stringify(data, null, 2);
}

async function loadRawMetrics() {
  const data = await api('/api/metrics');
  document.getElementById('rawMetrics').textContent = data ? JSON.stringify(data, null, 2) : 'Could not fetch metrics. Ensure BitAgent core is running.';
}

/* ── Notifications ────────────────────────────────────────────────────── */
let notifPanelOpen = false;
async function toggleNotifications() {
  const panel = document.getElementById('notifPanel');
  notifPanelOpen = !notifPanelOpen;
  panel.classList.toggle('open', notifPanelOpen);
  if (notifPanelOpen) await loadNotifications();
}
async function loadNotifications() {
  const data = await api('/api/notifications');
  const list = document.getElementById('notifList');
  const dot = document.getElementById('notifDot');
  if (data === null) {
    list.innerHTML = '<p class="text-sm" style="padding:var(--space-6);text-align:center;color:var(--color-danger)">Couldn\'t load notifications — the request failed.</p>';
    dot.style.display = 'none';
    return;
  }
  if (!data || data.length === 0) {
    list.innerHTML = '<p class="text-sm text-muted" style="padding:var(--space-6);text-align:center">No notifications yet</p>';
    dot.style.display = 'none';
    return;
  }
  const unread = data.filter(n => !n.read).length;
  dot.style.display = unread > 0 ? '' : 'none';
  const icons = {
    info: '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2"><circle cx="12" cy="12" r="10"/><line x1="12" y1="16" x2="12" y2="12"/><line x1="12" y1="8" x2="12.01" y2="8"/></svg>',
    success: '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2"><path d="M22 11.08V12a10 10 0 11-5.93-9.14"/><polyline points="22 4 12 14.01 9 11.01"/></svg>',
    warning: '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2"><path d="M10.29 3.86L1.82 18a2 2 0 001.71 3h16.94a2 2 0 001.71-3L13.71 3.86a2 2 0 00-3.42 0z"/><line x1="12" y1="9" x2="12" y2="13"/><line x1="12" y1="17" x2="12.01" y2="17"/></svg>',
    error: '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2"><circle cx="12" cy="12" r="10"/><line x1="15" y1="9" x2="9" y2="15"/><line x1="9" y1="9" x2="15" y2="15"/></svg>',
  };
  list.innerHTML = data.map(n =>
    `<div class="notif-entry ${n.read ? '' : 'unread'}" onclick="markNotifRead(${n.id})">
      <div class="notif-entry-icon ${n.level}">${icons[n.level] || icons.info}</div>
      <div class="notif-entry-text">
        <div class="notif-entry-title">${escHtml(n.title)}</div>
        ${n.message ? `<div class="notif-entry-msg">${escHtml(n.message)}</div>` : ''}
        <div class="notif-entry-time">${fmtAgo(n.at)}</div>
      </div>
    </div>`
  ).join('');
}
async function markNotifRead(id) {
  await api(`/api/notifications/${id}/read`, { method: 'PUT' });
  loadNotifications();
}
async function markAllRead() {
  const data = await api('/api/notifications');
  if (data) {
    await Promise.all(data.filter(n => !n.read).map(n => api(`/api/notifications/${n.id}/read`, { method: 'PUT' })));
    loadNotifications();
    toast('All notifications marked as read', 'success');
  }
}
// Close notif panel when clicking outside
document.addEventListener('click', e => {
  const panel = document.getElementById('notifPanel');
  const btn = e.target.closest('.notif-btn');
  if (notifPanelOpen && !panel.contains(e.target) && !btn) {
    notifPanelOpen = false;
    panel.classList.remove('open');
  }
});

/* ── Classifier Rules (illustrative reference only) ───────────────────────
 * These cards are static documentation of the CEL classification *approach*.
 * They are NOT fetched from the running classifier and are NOT editable here.
 * The live, authoritative ruleset is the CEL workflow configured on the
 * BitAgent core via CLASSIFIER_WORKFLOW (e.g. "norvi"), which the UI cannot
 * introspect. Keep these labelled as reference examples — see the banner in
 * the #stab-classifier panel. Do not present them as live rules.
 * ─────────────────────────────────────────────────────────────────────── */
function loadClassifierRules() {
  const referenceRules = [
    { priority: 1, name: 'Evidence Preempt', category: 'any', expression: 'evidence.hasGroundTruth(torrent.infoHash)', action: 'Use evidence label directly', description: 'Ground-truth from *arr webhooks bypasses all classification rules.' },
    { priority: 2, name: 'TV Season Pack', category: 'tv_show', expression: 'torrent.name.matches("(?i)s\\\\d{2}") && !torrent.name.matches("(?i)s\\\\d{2}e\\\\d{2}")', action: 'Classify as tv_show', description: 'Matches season packs (S01, S02) without episode numbers.' },
    { priority: 3, name: 'TV Episode', category: 'tv_show', expression: 'torrent.name.matches("(?i)s\\\\d{2}e\\\\d{2}")', action: 'Classify as tv_show', description: 'Matches standard episode naming (S01E01 pattern).' },
    { priority: 4, name: 'Movie Year', category: 'movie', expression: 'torrent.name.matches("(?i)\\\\(?(19|20)\\\\d{2}\\\\)?") && torrent.name.matches("(?i)(720p|1080p|2160p|bluray|remux)")', action: 'Classify as movie', description: 'Year + quality tag pattern typical of movie releases.' },
    { priority: 5, name: 'Music FLAC', category: 'music', expression: 'torrent.name.matches("(?i)flac") || torrent.files.any(f, f.path.endsWith(".flac"))', action: 'Classify as music', description: 'FLAC keyword or file extension detection.' },
    { priority: 6, name: 'Music MP3', category: 'music', expression: 'torrent.name.matches("(?i)mp3|320kbps|v0") || torrent.files.any(f, f.path.endsWith(".mp3"))', action: 'Classify as music', description: 'MP3 keyword or file extension detection.' },
    { priority: 7, name: 'Ebook Detection', category: 'ebook', expression: 'torrent.files.any(f, f.path.endsWith(".epub") || f.path.endsWith(".mobi") || f.path.endsWith(".pdf"))', action: 'Classify as ebook', description: 'Book file extensions in torrent file list.' },
    { priority: 8, name: 'Software ISO/EXE', category: 'software', expression: 'torrent.files.any(f, f.path.endsWith(".iso") || f.path.endsWith(".exe") || f.path.endsWith(".dmg"))', action: 'Classify as software', description: 'Installer file extensions in torrent file list.' },
    { priority: 9, name: 'Video Resolution Fallback', category: 'movie', expression: 'torrent.name.matches("(?i)(720p|1080p|2160p|4k|uhd)") && !hasLabel(torrent)', action: 'Classify as movie (low confidence)', description: 'Resolution tag without other signals defaults to movie.' },
    { priority: 10, name: 'Catch-all', category: 'unknown', expression: 'true', action: 'Classify as unknown', description: 'Default catch-all for unmatched torrents.' },
  ];
  const container = document.getElementById('classifierRulesList');
  if (!container) return;
  const categoryColors = { any: 'var(--color-fg-subtle)', tv_show: 'var(--color-accent)', movie: 'var(--color-info)', music: 'var(--color-success)', ebook: 'var(--color-warning)', software: 'var(--color-danger)', unknown: 'var(--color-fg-subtle)' };
  const caption = '<p class="text-xs text-muted">Illustrative reference examples — the live ruleset is the CEL workflow (<code class="font-mono">CLASSIFIER_WORKFLOW</code>) on the BitAgent core, not these cards.</p>';
  container.innerHTML = caption + referenceRules.map(r =>
    `<div class="rule-card">
      <div class="rule-header">
        <div class="flex items-center gap-3">
          <div class="rule-priority">${r.priority}</div>
          <div>
            <div style="font-weight:var(--weight-semibold);font-size:var(--text-sm)">${escHtml(r.name)}</div>
            <div class="text-xs text-muted">${escHtml(r.description)}</div>
          </div>
        </div>
        <div class="flex items-center gap-2">
          ${typePill(r.category)}
          <span class="text-xs text-subtle">${escHtml(r.action)}</span>
        </div>
      </div>
      <div class="rule-expression mt-2">${escHtml(r.expression)}</div>
    </div>`
  ).join('');
}

/* ── Init ─────────────────────────────────────────────────────────────── */
document.addEventListener('DOMContentLoaded', () => {
  // Make the click-only controls keyboard-reachable (they're <a>/<div>, not
  // <button>). The keydown handler above turns Enter/Space into a click.
  document.querySelectorAll('.nav-item').forEach(n => {
    n.setAttribute('tabindex', '0');
    n.setAttribute('role', 'button');
  });
  const tog = document.querySelector('.theme-toggle-switch');
  if (tog) { tog.setAttribute('tabindex', '0'); tog.setAttribute('role', 'switch'); tog.setAttribute('aria-label', 'Toggle dark mode'); }
  loadDashboard();
  loadNotifications();
  setInterval(() => {
    if (document.hidden) return;  // don't poll from background browser tabs
    if (currentTab === 'dashboard') loadDashboard();
    // Refresh only the scorecard on the AI tab — auto-rerendering the
    // Recent Matches table yanks rows out from under a review in progress.
    // The table refreshes on tab entry, manual Refresh, or the LLM toggle.
    if (currentTab === 'ai') loadAiTab();
    loadNotifications();  // keep the unread dot fresh regardless of active tab
  }, 30000);
});

// Close modals on backdrop click
document.querySelectorAll('.modal-overlay').forEach(overlay => {
  overlay.addEventListener('click', e => {
    if (e.target === overlay) overlay.classList.remove('open');
  });
});

// Keyboard shortcuts
document.addEventListener('keydown', e => {
  if (e.key === 'Escape') {
    document.querySelectorAll('.modal-overlay.open').forEach(m => m.classList.remove('open'));
    if (notifPanelOpen) { notifPanelOpen = false; document.getElementById('notifPanel').classList.remove('open'); }
  }
  const inField = ['INPUT', 'TEXTAREA', 'SELECT'].includes(document.activeElement.tagName);
  // Alt+number for tab switching (matches sidebar order). Use e.code, not e.key:
  // on macOS the Option modifier rewrites e.key to a special glyph (Alt+1 → "¡"),
  // so parseInt(e.key) never worked there. e.code stays "Digit1"..."Digit8".
  if (e.altKey && !e.ctrlKey && !e.shiftKey && !inField && /^Digit[1-8]$/.test(e.code)) {
    const tabs = ['dashboard', 'library', 'wants', 'evidence', 'quarantine', 'ai', 'settings', 'system'];
    const idx = parseInt(e.code.slice(5)) - 1;
    if (idx >= 0 && idx < tabs.length) { e.preventDefault(); switchTab(tabs[idx]); }
  }
  // Ctrl+K or / for search focus
  if (((e.ctrlKey || e.metaKey) && e.key === 'k') || (e.key === '/' && !inField && !e.ctrlKey && !e.metaKey)) {
    e.preventDefault();
    if (currentTab === 'library') document.getElementById('libSearch').focus();
    else { switchTab('library'); setTimeout(() => document.getElementById('libSearch').focus(), 100); }
  }
  // R for refresh (when not in input)
  if (e.key === 'r' && !e.ctrlKey && !e.metaKey && !['INPUT', 'TEXTAREA', 'SELECT'].includes(document.activeElement.tagName)) {
    refreshCurrentTab();
  }
});

// Keyboard activation for role="button" controls that aren't native <button>s
// (sidebar nav <a>s, the theme-toggle <div>). Enter/Space should fire them, so
// the whole primary navigation is reachable without a mouse.
document.addEventListener('keydown', e => {
  if (e.key !== 'Enter' && e.key !== ' ') return;
  const t = e.target;
  if (t.matches('.nav-item, .theme-toggle-switch')) { e.preventDefault(); t.click(); }
});

// ── Help tooltips ───────────────────────────────────────────────────
// Any element carrying data-help gets a small ⓘ affordance appended to it,
// so anything on the dashboard can carry a plain-English explanation — stat
// cards, panel/section headers, and individual table columns alike. When
// data-help sits on a container (a .stat-card or .card) the ⓘ anchors to that
// element's label/title; when it sits directly on a leaf (a <th>, <h3>, or a
// .card-title span) the ⓘ is appended to the leaf itself. Tap/click toggles a
// floating explainer (works on touch/PWA where native title= never appeared);
// hover-capable pointers also open on hover. One shared popover is reused.
// State is module-scope, not closure-scope, so `installHelpDots` can wire hosts
// that were rendered after page load (the LLM scorecards are rebuilt on every
// 30s poll). Without this they carried data-help and no ⓘ — invisible on
// touch, where there is no native title= fallback.
let _helpPop = null;
let _helpActiveBtn = null;
let _helpPinned = false; // opened by click (stays until dismissed) vs hover (transient)

function _helpPlace(btn) {
  const pop = _helpPop;
  const r = btn.getBoundingClientRect();
  const m = 8;
  pop.style.visibility = 'hidden';
  pop.classList.add('open');
  const pw = pop.offsetWidth, ph = pop.offsetHeight;
  let left = Math.min(Math.max(r.left, m), window.innerWidth - pw - m);
  let top = r.bottom + 8;
  if (top + ph > window.innerHeight - m) top = r.top - ph - 8; // flip above
  if (top < m) top = m;
  pop.style.left = `${left}px`;
  pop.style.top = `${top}px`;
  pop.style.visibility = '';
}
function _helpShow(btn) {
  if (!_helpPop) return;
  _helpPop.textContent = btn.dataset.help;
  _helpActiveBtn = btn;
  btn.setAttribute('aria-expanded', 'true');
  _helpPlace(btn);
  _helpPop.classList.add('open');
}
function _helpHide() {
  if (_helpPop) _helpPop.classList.remove('open');
  if (_helpActiveBtn) _helpActiveBtn.setAttribute('aria-expanded', 'false');
  _helpActiveBtn = null;
  _helpPinned = false;
}

// Append the ⓘ affordance to every [data-help] host under `root` that has not
// been wired yet. Safe to call repeatedly on re-rendered subtrees.
function installHelpDots(root = document) {
  const canHover = window.matchMedia('(hover: hover)').matches;
  root.querySelectorAll('[data-help]').forEach(host => {
    const text = host.getAttribute('data-help');
    if (!text || host.dataset.helpWired === '1') return;
    host.dataset.helpWired = '1';
    const label = host.querySelector('.stat-label, .card-title, .llm-metric-label') || host;
    const btn = document.createElement('button');
    btn.type = 'button';
    btn.className = 'help-dot';
    btn.dataset.help = text;
    btn.setAttribute('aria-label', 'What does this mean?');
    btn.setAttribute('aria-expanded', 'false');
    btn.innerHTML = '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2.2" aria-hidden="true"><circle cx="12" cy="12" r="10"/><line x1="12" y1="11" x2="12" y2="16.5"/><circle cx="12" cy="7.5" r="0.9" fill="currentColor" stroke="none"/></svg>';
    label.appendChild(btn);

    btn.addEventListener('click', e => {
      e.stopPropagation();
      if (_helpActiveBtn === btn && _helpPinned) _helpHide();
      else { _helpShow(btn); _helpPinned = true; }
    });
    if (canHover) {
      btn.addEventListener('mouseenter', () => { if (!_helpPinned) _helpShow(btn); });
      btn.addEventListener('mouseleave', () => { if (!_helpPinned) _helpHide(); });
    }
  });
}

function initCardHelp() {
  if (_helpPop) return; // idempotent
  _helpPop = document.createElement('div');
  _helpPop.className = 'help-pop';
  _helpPop.setAttribute('role', 'tooltip');
  document.body.appendChild(_helpPop);

  installHelpDots(document);

  document.addEventListener('click', () => { if (_helpPinned) _helpHide(); });
  document.addEventListener('keydown', e => { if (e.key === 'Escape') _helpHide(); });
  window.addEventListener('scroll', () => _helpHide(), true);
  window.addEventListener('resize', () => _helpHide());
}
document.addEventListener('DOMContentLoaded', initCardHelp);

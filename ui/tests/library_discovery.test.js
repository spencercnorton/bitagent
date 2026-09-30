'use strict';

const test = require('node:test');
const assert = require('node:assert/strict');
const vm = require('node:vm');
const fs = require('node:fs');
const path = require('node:path');
const { discoveryItemFor, discoveryGroupFor } = require('../static/js/library-discovery.js');
const { browseParamsFor, parseBrowseState, isHomeFor, accountUsageViewFor } = require('../static/js/library.js');
const releaseTools = require('../static/js/library-tools.js');

function title(overrides = {}) {
  return { id: 42, type: 'movie', title: 'Synthetic Discovery', year: '2026',
    overview: 'An invented catalog title.', mode: 'newest', voteAverage: 8,
    backdrop: 'https://image.tmdb.org/t/p/w1280/synthetic-backdrop.jpg',
    poster: 'https://image.tmdb.org/t/p/w500/synthetic-poster.jpg', ...overrides };
}

test('discovery identities open exact title releases without inventing indexed torrents', () => {
  const group = discoveryGroupFor(title({ type: 'tv_show' }));
  assert.equal(group.key, 'tv_show:id:42');
  assert.equal(group.contentSource, 'tmdb');
  assert.equal(group.contentId, '42');
  assert.equal(group.contentType, 'tv_show');
  assert.equal(group.best.name, 'Synthetic Discovery');
  assert.deepEqual(group.items, []);
  assert.equal(group.best.infoHash, undefined);
  assert.equal(group.best.seeders, undefined);
  assert.equal(discoveryItemFor(title()).label, 'New & noteworthy');
  assert.equal(discoveryItemFor(title({ mode: 'popular' })).label, 'Popular now');
});

test('discovery rejects invalid identities and drops unsafe or unrelated artwork URLs', () => {
  for (const item of [null, title({ id: '42<script>' }), title({ id: 0 }),
    title({ id: '1'.repeat(13) }), title({ type: 'torrent' }), title({ title: '' })]) {
    assert.equal(discoveryItemFor(item), null);
    assert.equal(discoveryGroupFor(item), null);
  }
  for (const url of ['javascript:alert(1)', 'data:image/svg+xml,synthetic', '//image.tmdb.org/t/p/w500/a.jpg',
    'http://image.tmdb.org/t/p/w500/a.jpg', 'https://unrelated.invalid/t/p/w500/a.jpg',
    'https://image.tmdb.org/t/p/w500/a.jpg?redirect=1', 'https://image.tmdb.org/t/p/w500/a.jpg#fragment']) {
    const item = discoveryItemFor(title({ backdrop: url, poster: url }));
    assert.equal(item.backdrop, '', url);
    assert.equal(item.poster, '', url);
    assert.equal(item.id, '42', 'missing safe artwork must not discard a usable title');
  }
});

test('provider catalog permalinks preserve service, country, query, type, and paging', () => {
  const original = parseBrowseState('?provider=8&region=CA&q=Synthetic+%26+Series&type=tv_show&page=3');
  const restored = parseBrowseState('?' + browseParamsFor(original));
  assert.equal(restored.provider, '8');
  assert.equal(restored.region, 'CA');
  assert.equal(restored.q, 'Synthetic & Series');
  assert.equal(restored.type, 'tv_show');
  assert.equal(restored.page, 2);
  assert.equal(isHomeFor(restored), false);
  assert.equal(isHomeFor(parseBrowseState('?provider=8&region=CA')), false, 'a provider-only URL must not load ordinary home shelves');
  assert.equal(isHomeFor(parseBrowseState('?region=CA')), true, 'a country choice alone remains library home');
  assert.equal(browseParamsFor(parseBrowseState('')).has('provider'), false);
});

test('malformed provider identities and countries cannot survive permalink parsing', () => {
  for (const query of ['?provider=bad&region=us', '?provider=-1&region=USA', '?provider=123456789&region=CA<script>']) {
    const restored = parseBrowseState(query);
    assert.equal(restored.provider, '');
    assert.equal(restored.region, 'US');
  }
});

test('account usage distinguishes unreported transfers from measured zero activity', () => {
  const missing = accountUsageViewFor({ grabs: 0, apiSearches: 0, downloadedBytes: null, uploadedBytes: null, hitAndRuns: null });
  assert.equal(missing.downloaded, 'Not reported');
  assert.equal(missing.uploaded, 'Not reported');
  assert.equal(missing.hitAndRuns, 'Not reported');
  assert.equal(missing.grabs, '0');
  assert.equal(missing.searches, '0');
  assert.equal(missing.ratio, '—');
  const measured = accountUsageViewFor({ grabs: 0, apiSearches: 0, downloadedBytes: 0, uploadedBytes: 0, hitAndRuns: 0 });
  assert.equal(measured.downloaded, '0 B');
  assert.equal(measured.uploaded, '0 B');
  assert.equal(measured.hitAndRuns, '0');
  assert.equal(measured.ratio, '—', 'zero downloads cannot establish a transfer ratio');
  assert.equal(accountUsageViewFor({ grabs: 1, apiSearches: 2, downloadedBytes: 1024, uploadedBytes: 2560 }).ratio, '2.50');
  for (const usage of [null, { grabs: -1, apiSearches: 0 }, { grabs: 0, apiSearches: 1.5 }, { grabs: '0', apiSearches: 0 }]) {
    assert.equal(accountUsageViewFor(usage), null);
  }
});

// A deterministic timer queue and a minimal DOM exercise the production
// carousel listeners without sleeping, networking, or depending on CSS.
function discoveryController(reducedMotion = false) {
  const nodes = new Map(), timers = new Map(), documentListeners = new Map(), motionListeners = new Map();
  let timerId = 0;
  function element(id = '') {
    const attrs = new Map(), classes = new Set(), listeners = new Map();
    return { id, dataset: {}, children: [], value: id === 'libProviderRegion' ? 'US' : '',
      classList: { add(...names) { names.forEach(n => classes.add(n)); }, remove(...names) { names.forEach(n => classes.delete(n)); },
        toggle(name, enabled) { if (enabled) classes.add(name); else classes.delete(name); } },
      setAttribute(name, value) { attrs.set(name, String(value)); }, getAttribute(name) { return attrs.get(name); },
      removeAttribute(name) { attrs.delete(name); }, replaceChildren(...children) { this.children = children; },
      appendChild(child) { this.children.push(child); }, addEventListener(name, callback) { listeners.set(name, callback); }, listeners };
  }
  const document = { hidden: false, activeElement: null,
    getElementById(id) { if (!nodes.has(id)) nodes.set(id, element(id)); return nodes.get(id); },
    createElement() { return element(); }, querySelectorAll() { return []; },
    addEventListener(name, callback) { documentListeners.set(name, callback); } };
  const context = vm.createContext({ document, URLSearchParams,
    state: { provider: '', region: 'US' }, _modalStack: [], syncControls() {}, loadLibrary() {},
    api: () => new Promise(() => {}), matchMedia: () => ({ matches: reducedMotion, addEventListener(name, callback) { motionListeners.set(name, callback); } }),
    setTimeout(callback) { const id = ++timerId; timers.set(id, callback); return id; }, clearTimeout(id) { timers.delete(id); },
    typeLabel: type => type === 'movie' ? 'Movie' : 'Series', Image: class {} });
  const source = fs.readFileSync(path.join(__dirname, '../static/js/library-discovery.js'), 'utf8');
  vm.runInContext(source + '\nthis.controller = { discoveryState, scheduleSpotlight, renderSpotlight, moveSpotlight, toggleSpotlightPause };', context);
  context.controller.discoveryState.slides = [discoveryItemFor(title({ backdrop: '' })), discoveryItemFor(title({ id: 43, backdrop: '', title: 'Second Synthetic Title' }))];
  return { context, nodes, timers, document, documentListeners, motionListeners,
    fireTimer() { const [id, callback] = timers.entries().next().value; timers.delete(id); callback(); } };
}

test('carousel rotation pauses on keyboard focus, pointer hover, hidden tabs, and offscreen heroes', () => {
  const c = discoveryController();
  c.context.controller.scheduleSpotlight(); assert.equal(c.timers.size, 1);
  c.nodes.get('libSpotlight').listeners.get('focusin')({ target: { id: 'libSpotlightOpen' } });
  assert.equal(c.context.controller.discoveryState.paused, true); assert.equal(c.timers.size, 0);
  c.context.controller.toggleSpotlightPause(false); assert.equal(c.timers.size, 1);
  c.nodes.get('libSpotlight').listeners.get('mouseenter')(); assert.equal(c.timers.size, 0);
  c.nodes.get('libSpotlight').listeners.get('mouseleave')(); assert.equal(c.timers.size, 1);
  c.document.hidden = true; c.documentListeners.get('visibilitychange')(); assert.equal(c.timers.size, 0);
  c.document.hidden = false; c.context.controller.discoveryState.visible = false;
  c.context.controller.scheduleSpotlight(); assert.equal(c.timers.size, 0);
});

test('reduced motion starts paused and a modal prevents an automatic slide change', () => {
  const reduced = discoveryController(true);
  reduced.context.controller.scheduleSpotlight();
  assert.equal(reduced.context.controller.discoveryState.paused, true); assert.equal(reduced.timers.size, 0);
  const c = discoveryController();
  c.context.controller.scheduleSpotlight(); c.context._modalStack.push({}); c.fireTimer();
  assert.equal(c.context.controller.discoveryState.index, 0);
  assert.equal(c.timers.size, 1, 'rotation may retry after the modal closes without advancing the current title');
});

test('carousel updates active dots while preserving existing keyboard focus targets', () => {
  const c = discoveryController();
  c.context.controller.renderSpotlight();
  const dots = c.nodes.get('libSpotlightDots'), first = dots.children[0], second = dots.children[1];
  c.document.activeElement = first;
  c.context.controller.moveSpotlight(1);
  assert.equal(dots.children[0], first); assert.equal(dots.children[1], second);
  assert.equal(c.document.activeElement, first);
  assert.equal(first.getAttribute('aria-current'), 'false'); assert.equal(second.getAttribute('aria-current'), 'true');
});

function release(number, name, quality = '1080p') {
  return { infoHash: number.toString(16).padStart(40, '0'), torrentName: name, seeders: number, videoResolution: quality };
}
function magnetController() {
  const context = vm.createContext({ URLSearchParams, AbortController, setTimeout, clearTimeout, BitAgentLibraryTools: releaseTools });
  const source = fs.readFileSync(path.join(__dirname, '../static/js/library.js'), 'utf8');
  vm.runInContext(source + '\nthis.controller = { detail, magnetSelection, copyMagnetGroup, copyMagnetCollection, renderAccountUsage, recordLibraryGrab };', context);
  context.controller.detail.bulkMode = 'all';
  context.copyText = (text, message, onSuccess) => { context.copied = { text, message }; if (onSuccess) onSuccess(); };
  context.recordLibraryGrab = (count, action) => { context.recorded = { count, action }; };
  context.toast = message => { context.message = message; };
  return context;
}

test('direct title copy includes more than the former collection cap and needs no staged selection', () => {
  const c = magnetController();
  c.controller.detail.releases = Array.from({ length: 601 }, (_, i) => release(i + 1, 'Synthetic Movie ' + i));
  c.controller.copyMagnetGroup('title');
  assert.equal(c.copied.text.split('\n').length, 601);
  assert.equal(c.controller.magnetSelection.size, 0);
  assert.equal(c.recorded.count, 601); assert.equal(c.recorded.action, 'copy');
  assert.doesNotMatch(c.copied.message, /partial/);
});

test('direct season copy respects the active season and release quality filter', () => {
  const c = magnetController();
  c.controller.detail.ct = 'tv_show'; c.controller.detail.activeSeason = 2;
  c.controller.detail.releases = [release(1, 'Synthetic S01E01'), release(2, 'Synthetic S02E01'),
    release(3, 'Synthetic S02E02', '2160p'), release(4, 'Synthetic Complete Series')];
  c.controller.copyMagnetGroup('season');
  assert.equal(c.copied.text.split('\n').length, 2);
  assert.doesNotMatch(c.copied.text, /S01E01|Complete%20Series/);
  c.controller.detail.quality = '1080p'; c.controller.copyMagnetGroup('season');
  assert.equal(c.copied.text.split('\n').length, 1);
  assert.match(c.copied.text, /S02E01/);
});

test('direct magnet copy reports partial limits and blocks loading or unknown seasons', () => {
  const c = magnetController();
  c.controller.detail.releases = Array.from({ length: 1001 }, (_, i) => release(i + 1, 'Synthetic Movie ' + i));
  c.controller.copyMagnetGroup('title');
  assert.equal(c.copied.text.split('\n').length, 1000); assert.match(c.copied.message, /partial selection/);
  delete c.copied; delete c.recorded;
  c.controller.detail.loading = true; c.controller.copyMagnetGroup('title');
  assert.equal(c.copied, undefined); assert.equal(c.recorded, undefined);
  c.controller.detail.loading = false; c.controller.detail.activeSeason = null; c.controller.copyMagnetGroup('season');
  assert.equal(c.copied, undefined);
});

test('delayed clipboard success records the copied collection count before later selection changes', async () => {
  const c = magnetController();
  const first = release(1, 'First Synthetic Title'), second = release(2, 'Second Synthetic Title');
  c.controller.magnetSelection.set(first.infoHash, first); c.controller.magnetSelection.set(second.infoHash, second);
  let complete;
  const pending = new Promise(resolve => { complete = resolve; });
  c.copyText = (text, message, onSuccess) => { c.copied = { text, message }; pending.then(onSuccess); };
  c.controller.copyMagnetCollection();
  assert.equal(c.copied.text.split('\n').length, 2);
  assert.equal(c.recorded, undefined, 'an unresolved clipboard operation must not count a grab');
  c.controller.magnetSelection.clear();
  const later = release(3, 'Later Synthetic Selection'); c.controller.magnetSelection.set(later.infoHash, later);
  complete(); await pending;
  assert.equal(c.recorded.count, 2, 'activity must reflect the copied text, not the current collection');
  assert.equal(c.recorded.action, 'copy');
  assert.equal(c.controller.magnetSelection.size, 1);
});

test('out-of-order account responses cannot decrease aggregate activity within one tracking period', async () => {
  const c = magnetController(), nodes = new Map(), requests = [];
  c.document = { getElementById(id) { if (!nodes.has(id)) nodes.set(id, { setAttribute() {} }); return nodes.get(id); } };
  c.api = () => new Promise(resolve => requests.push(resolve));
  const first = c.controller.recordLibraryGrab(1, 'copy'), second = c.controller.recordLibraryGrab(1, 'copy');
  const trackingSince = '2026-09-30T00:00:00Z';
  requests[1]({ grabs: 12, apiSearches: 42, trackingSince }); await second;
  requests[0]({ grabs: 11, apiSearches: 39, trackingSince }); await first;
  assert.equal(nodes.get('acctGrabs').textContent, '12');
  assert.equal(nodes.get('acctApiSearches').textContent, '42');
  c.controller.renderAccountUsage({ grabs: 14, apiSearches: 40, trackingSince });
  assert.equal(nodes.get('acctGrabs').textContent, '14');
  assert.equal(nodes.get('acctApiSearches').textContent, '42', 'each activity total stays monotonic independently');
  c.controller.renderAccountUsage({ grabs: 13, apiSearches: 44, trackingSince });
  assert.equal(nodes.get('acctGrabs').textContent, '14');
  assert.equal(nodes.get('acctApiSearches').textContent, '44');
  c.controller.renderAccountUsage({ grabs: 0, apiSearches: 0, trackingSince: '2026-10-01T00:00:00Z' });
  assert.equal(nodes.get('acctGrabs').textContent, '0', 'a new tracking period can legitimately reset activity');
  assert.equal(nodes.get('acctApiSearches').textContent, '0');
});

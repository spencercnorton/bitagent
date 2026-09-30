'use strict';

const test = require('node:test');
const assert = require('node:assert/strict');
const vm = require('node:vm');
const fs = require('node:fs');
const path = require('node:path');
const { discoveryItemFor, discoveryGroupFor, discoveryLogoUrlFor, discoveryLogoCandidatesFor, discoveryCatalogueParamsFor } = require('../static/js/library-discovery.js');
const { browseParamsFor, parseBrowseState, isHomeFor, accountUsageViewFor } = require('../static/js/library.js');
const releaseTools = require('../static/js/library-tools.js');
const preferences = require('../static/js/library-preferences.js');

function accountUsage(overrides = {}) {
  return {accountId:'alice',available:true,status:'available',revision:1,observedAt:'2026-09-30T12:00:00Z',
    trackingSince:1790769600.5,grabs:0,apiSearches:0,magnetCopies:0,magnetOpens:0,magnetExports:0,
    downloadedBytes:null,uploadedBytes:null,hitAndRuns:null,...overrides};
}

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

test('network permalinks force series, win over provider, and preserve title search and paging', () => {
  const original = parseBrowseState('?network=49&provider=8&type=movie&q=Synthetic+Series&page=3');
  assert.equal(original.network, '49'); assert.equal(original.provider, ''); assert.equal(original.type, 'tv_show');
  const params = browseParamsFor(original);
  assert.equal(params.get('network'), '49'); assert.equal(params.get('type'), 'tv_show'); assert.equal(params.has('provider'), false);
  assert.equal(params.has('region'), false, 'network origin is not a country availability filter');
  const restored = parseBrowseState('?' + params);
  assert.equal(restored.network, '49'); assert.equal(restored.q, 'Synthetic Series'); assert.equal(restored.page, 2);
  assert.equal(isHomeFor(restored), false); assert.equal(isHomeFor(parseBrowseState('?network=49')), false);
  assert.equal(parseBrowseState('?network=2147483647').network, '2147483647', 'TMDB network IDs support the full positive int32 contract');
  for (const id of ['bad', '-1', '0', '0049', '2147483648', '9999999999', '12345678901']) {
    const malformed = parseBrowseState(`?network=${id}&provider=8`);
    assert.equal(malformed.network, '', id); assert.equal(malformed.provider, '8', 'invalid networks must not discard a valid provider');
  }
});

test('catalogue requests keep providers and networks exclusive and never request network movies', () => {
  const network = discoveryCatalogueParamsFor({network:'49', provider:'8', type:'movie', region:'CA', q:'Synthetic', page:2});
  assert.equal(network.get('network'), '49'); assert.equal(network.has('provider'), false);
  assert.equal(network.get('type'), 'tv_show'); assert.equal(network.get('page'), '3'); assert.equal(network.get('q'), 'Synthetic');
  const provider = discoveryCatalogueParamsFor({provider:'8', network:'', type:'movie', region:'CA', page:0});
  assert.equal(provider.get('provider'), '8'); assert.equal(provider.get('type'), 'movie'); assert.equal(provider.has('network'), false);
});

test('logo candidates accept genuine TMDB originals and SVGs and reject alternate origins or schemes', () => {
  const primary = 'https://image.tmdb.org/t/p/w92/synthetic.png', alternative = 'https://image.tmdb.org/t/p/original/synthetic.svg';
  assert.deepEqual(discoveryLogoCandidatesFor({logo:primary, logoAlternatives:[primary, alternative, 'javascript:alert(1)']}), [primary, alternative]);
  for (const value of ['http://image.tmdb.org/t/p/w92/a.png', '//image.tmdb.org/t/p/w92/a.png',
    'https://image.tmdb.org.evil.invalid/t/p/w92/a.png', 'https://user@image.tmdb.org/t/p/w92/a.png',
    'https://image.tmdb.org:444/t/p/w92/a.png', 'https://image.tmdb.org/t/p/w92/a.png?x=1',
    'https://image.tmdb.org/t/p/w92/a.png#x', 'https://image.tmdb.org/t/p/w92/a.png extra', 'data:image/svg+xml,synthetic']) {
    assert.equal(discoveryLogoUrlFor(value), '', value);
  }
  assert.deepEqual(discoveryLogoCandidatesFor({logo:null, logoAlternatives:'invalid'}), []);
});

test('account usage distinguishes unreported transfers from measured zero activity', () => {
  const missing = accountUsageViewFor(accountUsage());
  assert.equal(missing.downloaded, 'Not reported');
  assert.equal(missing.uploaded, 'Not reported');
  assert.equal(missing.hitAndRuns, 'Not reported');
  assert.equal(missing.grabs, '0');
  assert.equal(missing.searches, '0');
  assert.equal(missing.ratio, '—');
  const measured = accountUsageViewFor(accountUsage({downloadedBytes:0,uploadedBytes:0,hitAndRuns:0}));
  assert.equal(measured.downloaded, '0 B');
  assert.equal(measured.uploaded, '0 B');
  assert.equal(measured.hitAndRuns, '0');
  assert.equal(measured.ratio, '—', 'zero downloads cannot establish a transfer ratio');
  assert.equal(accountUsageViewFor(accountUsage({grabs:1,magnetCopies:1,apiSearches:2,downloadedBytes:1024,uploadedBytes:2560})).ratio, '2.50');
  for (const usage of [null,accountUsage({grabs:-1}),accountUsage({apiSearches:1.5}),accountUsage({grabs:'0'})]) {
    assert.equal(accountUsageViewFor(usage), null);
  }
});

// A deterministic timer queue and a minimal DOM exercise production controllers
// without sleeping, networking, or depending on CSS. Removing a focused subtree
// really loses focus: otherwise a destructive re-render could pass these tests.
function discoveryController(reducedMotion = false) {
  const nodes = new Map(), timers = new Map(), documentListeners = new Map(), motionListeners = new Map();
  let timerId = 0;
  function element(id = '', tagName = 'div') {
    const attrs = new Map(), classes = new Set(), listeners = new Map();
    const node = { id, tagName: tagName.toUpperCase(), dataset: {}, style: {}, children: [], parentNode: null,
      value: id === 'libProviderRegion' ? 'US' : '',
      classList: { add(...names) { names.forEach(n => classes.add(n)); }, remove(...names) { names.forEach(n => classes.delete(n)); },
        contains(name) { return classes.has(name); },
        toggle(name, enabled = !classes.has(name)) { if (enabled) classes.add(name); else classes.delete(name); return enabled; } },
      setAttribute(name, value) { attrs.set(name, String(value)); }, getAttribute(name) { return attrs.get(name); },
      removeAttribute(name) { attrs.delete(name); },
      contains(other) { for (let current = other; current; current = current.parentNode) if (current === this) return true; return false; },
      replaceChildren(...children) {
        for (const child of this.children) {
          if (!children.includes(child) && child.contains(document.activeElement)) document.activeElement = null;
          child.parentNode = null;
        }
        this.children = [];
        children.forEach(child => this.appendChild(child));
      },
      appendChild(child) { child.parentNode = this; this.children.push(child); return child; },
      focus() { if (this.isConnected) document.activeElement = this; },
      addEventListener(name, callback) { listeners.set(name, callback); }, listeners };
    Object.defineProperties(node, {
      className: { get() { return Array.from(classes).join(' '); }, set(value) { classes.clear(); String(value).split(/\s+/).filter(Boolean).forEach(name => classes.add(name)); } },
      isConnected: { get() { return !!this.id || !!this.parentNode?.isConnected; } },
      parentElement: { get() { return this.parentNode; } },
    });
    return node;
  }
  const document = { hidden: false, activeElement: null,
    getElementById(id) { if (!nodes.has(id)) nodes.set(id, element(id)); return nodes.get(id); },
    createElement(tagName) { return element('', tagName); }, querySelector(selector) { return this.getElementById(selector); },
    querySelectorAll(selector) { return selector === '#libProviders button' ? this.getElementById('libProviders').children : []; },
    addEventListener(name, callback) { documentListeners.set(name, callback); } };
  const preferenceListeners = new Map();
  const context = vm.createContext({ document, URL, URLSearchParams, AbortController,
    window:{BitAgentPreferences:preferences,addEventListener(name,callback) { preferenceListeners.set(name,callback); }},
    _reducedMotion:() => reducedMotion,
    state: parseBrowseState(''), _modalStack: [], syncControls() {}, loadLibrary() {}, renderActiveLibraryFilters() {},renderAccountPreferences() {},
    api: () => new Promise(() => {}), matchMedia: () => ({ matches: reducedMotion, addEventListener(name, callback) { motionListeners.set(name, callback); } }),
    setTimeout(callback) { const id = ++timerId; timers.set(id, callback); return id; }, clearTimeout(id) { timers.delete(id); },
    typeLabel: type => type === 'movie' ? 'Movie' : 'Series', Image: class {},
    escHtml: value => String(value), escAttr: value => String(value), fmtNum: value => String(value),
    posterFallbackHtml: () => '<div class="lib-poster-ph"></div>' });
  context.isHome = () => isHomeFor(context.state);
  const source = fs.readFileSync(path.join(__dirname, '../static/js/library-discovery.js'), 'utf8');
  vm.runInContext(source + '\nthis.controller = { discoveryState, scheduleSpotlight, renderSpotlight, moveSpotlight, toggleSpotlightPause, discoveryTile, filterProviders, setDiscoveryMode, searchDiscoveryPicker, loadNetworks, selectNetwork, selectProvider, syncProviderControls, loadProviderLibrary, changeProviderRegion };', context);
  context.controller.discoveryState.slides = [discoveryItemFor(title({ backdrop: '' })), discoveryItemFor(title({ id: 43, backdrop: '', title: 'Second Synthetic Title' }))];
  return { context, nodes, timers, document, documentListeners, motionListeners,preferenceListeners,
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

test('failed logos try supplied safe alternatives then keep the branded name and visible fallback', () => {
  const c = discoveryController();
  const tile = c.context.controller.discoveryTile({id:'49', name:'Synthetic Cable', logo:'https://image.tmdb.org/t/p/w92/first.png',
    logoAlternatives:['https://image.tmdb.org/t/p/original/second.svg'], country:'US'}, 'network');
  const plate = tile.children[0], fallback = plate.children[0], image = plate.children[1];
  assert.equal(image.src, 'https://image.tmdb.org/t/p/w92/first.png'); assert.equal(image.alt, '');
  image.listeners.get('error')(); assert.equal(image.src, 'https://image.tmdb.org/t/p/original/second.svg');
  image.listeners.get('error')(); assert.equal(image.hidden, true); assert.equal(fallback.hidden, false);
  assert.equal(fallback.textContent, 'Logo unavailable'); assert.equal(tile.children[1].textContent, 'Synthetic Cable');
  assert.equal(tile.children[2].textContent, 'Origin: US'); assert.equal(plate.dataset.logoState, 'unavailable');
  const missing = c.context.controller.discoveryTile({id:'50', name:'No Logo Cable', logo:'https://unrelated.invalid/a.png'}, 'network');
  assert.equal(missing.children[0].children.length, 1); assert.equal(missing.children[0].children[0].hidden, false);
  assert.equal(missing.children[1].textContent, 'No Logo Cable');
});

test('network mode hides availability country without changing the active catalogue before selection', () => {
  const c = discoveryController(), s = c.context.controller.discoveryState;
  c.context.state.provider = '8'; c.context.state.type = 'movie';
  c.context.controller.setDiscoveryMode('networks');
  assert.equal(c.context.state.provider, '8'); assert.equal(c.context.state.network, ''); assert.equal(c.context.state.type, 'movie');
  assert.equal(c.nodes.get('libProviderRegion').hidden, true); assert.equal(c.nodes.get('libNetworkMode').getAttribute('aria-pressed'), 'true');
  c.context.controller.selectNetwork('49');
  assert.equal(c.context.state.provider, ''); assert.equal(c.context.state.network, '49'); assert.equal(c.context.state.type, 'tv_show');
  c.context.controller.selectNetwork(''); assert.equal(s.pickerMode, 'networks'); assert.equal(c.context.state.network, '');
  c.context.controller.selectProvider('8'); assert.equal(s.pickerMode, 'streaming'); assert.equal(c.nodes.get('libProviderRegion').hidden, false);
});

test('provider selection synchronizes pressed state while retaining connected focus on the same brand', () => {
  const c = discoveryController();
  c.context.controller.discoveryState.providers = [{id:8, name:'Synthetic Stream'}, {id:9, name:'Another Stream'}];
  c.context.controller.filterProviders();
  const tile = c.nodes.get('libProviders').children[1]; c.document.activeElement = tile;
  c.context.state.provider = '8'; c.context.controller.syncProviderControls();
  const selected = c.nodes.get('libProviders').children[1];
  assert.equal(c.document.activeElement, selected); assert.equal(selected.dataset.provider, tile.dataset.provider);
  assert.equal(selected.isConnected, true); assert.equal(selected.getAttribute('aria-pressed'), 'true');
});

function groupedProvider(overrides = {}) {
  return {id:531, name:'Paramount+', providerIds:[531,582,2616],
    idsByType:{movie:[531,582],tv_show:[531,2616]}, types:['movie','tv_show'],
    aliases:['Paramount Plus','Paramount+ Amazon Channel','Paramount Plus Essential'],
    logo:'https://image.tmdb.org/t/p/w92/paramount-synthetic.png', ...overrides};
}

function largeProviderCatalogue() {
  const majors = [[8,'Netflix'],[9,'Amazon Prime Video'],[337,'Disney Plus'],[15,'Hulu'],[350,'Apple TV'],
    [1899,'HBO Max'],[2303,'Paramount+'],[386,'Peacock'],[526,'AMC+'],[34,'MGM+'],[283,'Crunchyroll']];
  const longTail = Array.from({length:37}, (_, index) => ({id:9000 + index, name:`Synthetic Archive ${index}`,
    brandKey:`synthetic archive ${index}`, providerIds:[9000 + index,19000 + index], aliases:[`Archive Plan ${index}`]}));
  // A useful featured list cannot just take the first few regional rows.
  return [...longTail.slice(0,12), ...majors.map(([id,name]) => ({id,name,brandKey:name.toLowerCase(),providerIds:[id],aliases:[name]})), ...longTail.slice(12)];
}
function providerButtons(c) { return c.nodes.get('libProviders').children.filter(button => button.dataset.provider); }
function toggleProviderPicker(c) { vm.runInContext('toggleProviderPicker()', c.context); }

test('provider disclosure is a native keyboard-operable button controlling the service list', () => {
  const template = fs.readFileSync(path.join(__dirname, '../templates/library.html'), 'utf8');
  const button = template.match(/<button\b[^>]*\bid="libProviderMore"[^>]*>/)?.[0];
  assert.ok(button, 'provider disclosure must use native button keyboard behavior');
  assert.match(button, /\btype="button"/); assert.match(button, /\baria-controls="libProviders"/);
  assert.match(button, /\baria-expanded="false"/);
});

test('a compact picker features major brands and retains a selected long-tail legacy variant', () => {
  const c = discoveryController(), providers = largeProviderCatalogue();
  c.context.controller.discoveryState.providers = providers;
  Object.assign(c.context.state, parseBrowseState('?provider=19036&q=Synthetic&type=tv_show&page=4'));
  c.context.controller.filterProviders();
  const buttons = providerButtons(c), ids = buttons.map(button => button.dataset.provider);
  for (const id of ['8','9','15','2303']) assert.ok(ids.includes(id), `featured service ${id} must be visible despite its source order`);
  assert.ok(buttons.length < providers.length, 'the initial picker must not render the entire catalogue');
  assert.ok(ids.includes('9036'), 'the selected long-tail brand must remain reachable while compact');
  assert.equal(buttons.find(button => button.dataset.provider === '9036').getAttribute('aria-pressed'), 'true');
  const more = c.nodes.get('libProviderMore');
  assert.equal(more.hidden, false); assert.equal(more.getAttribute('aria-expanded'), 'false');
  assert.match(more.textContent || more.innerHTML, new RegExp(`\\b${providers.length}\\b`));
  assert.equal(c.context.state.provider, '19036', 'picker rendering must preserve legacy permalink identity');
});

test('expanding and collapsing services preserves catalogue filters, history and keyboard focus', () => {
  const c = discoveryController(), s = c.context.controller.discoveryState;
  s.providers = largeProviderCatalogue();
  Object.assign(c.context.state, parseBrowseState('?provider=19036&region=US&q=Synthetic&type=tv_show&page=4&genres=28&qualities=1080p&features=anime&years=2020-2026'));
  const before = browseParamsFor(c.context.state).toString(), loads = [];
  c.context.loadLibrary = options => loads.push(options);
  c.context.controller.filterProviders();
  const more = c.nodes.get('libProviderMore'); more.focus();
  toggleProviderPicker(c);
  assert.equal(providerButtons(c).length, s.providers.length);
  assert.equal(more.getAttribute('aria-expanded'), 'true'); assert.match(more.textContent || more.innerHTML, /show less/i);
  assert.equal(c.document.activeElement, more, 'native disclosure focus must survive expanding the list');
  providerButtons(c).find(button => button.dataset.provider === '9036').focus();
  toggleProviderPicker(c);
  assert.equal(more.getAttribute('aria-expanded'), 'false');
  assert.equal(c.document.activeElement?.dataset.provider, '9036', 'a surviving selected tile must retain focus after collapse');
  assert.ok(c.document.activeElement.isConnected, 'focus cannot point at a removed tile');
  assert.equal(browseParamsFor(c.context.state).toString(), before, 'including Set-valued facets and title pagination');
  assert.deepEqual(loads, [], 'disclosure must not navigate, push history, or fetch title results');
});

test('service alias search covers hidden long-tail groups without changing title search or expansion', () => {
  const c = discoveryController(), s = c.context.controller.discoveryState;
  s.providers = largeProviderCatalogue(); c.context.state.q = 'Synthetic Movie'; c.context.state.page = 5;
  c.context.controller.filterProviders();
  assert.equal(providerButtons(c).some(button => button.dataset.provider === '9035'), false);
  const input = c.document.getElementById('libProviderSearch'); input.value = 'aRcHiVe pLaN 35'; input.focus();
  c.context.controller.searchDiscoveryPicker();
  assert.deepEqual(providerButtons(c).map(button => button.dataset.provider), ['9035']);
  assert.equal(c.document.activeElement, input); assert.equal(c.nodes.get('libProviderMore').hidden, true);
  assert.equal(c.context.state.q, 'Synthetic Movie'); assert.equal(c.context.state.page, 5);
  input.value = ''; c.context.controller.searchDiscoveryPicker();
  assert.ok(providerButtons(c).length < s.providers.length, 'clearing search must restore the prior compact presentation');
  assert.equal(c.nodes.get('libProviderMore').getAttribute('aria-expanded'), 'false');
});

test('collapsing an unselected long-tail focus target returns focus to the reachable disclosure button', () => {
  const c = discoveryController(); c.context.controller.discoveryState.providers = largeProviderCatalogue();
  c.context.controller.filterProviders(); toggleProviderPicker(c);
  const removed = providerButtons(c).find(button => button.dataset.provider === '9035'); removed.focus();
  toggleProviderPicker(c);
  assert.equal(removed.isConnected, false);
  assert.equal(c.document.activeElement, c.nodes.get('libProviderMore'));
  assert.equal(c.document.activeElement.hidden, false); assert.equal(c.document.activeElement.getAttribute('aria-expanded'), 'false');
  assert.equal(c.context.state.provider, '', 'collapsing a picker cannot select the focused service');
});

test('network mode hides and isolates provider disclosure while keeping its own paging and search', () => {
  const c = discoveryController(), s = c.context.controller.discoveryState;
  s.providers = largeProviderCatalogue(); c.context.controller.filterProviders(); toggleProviderPicker(c);
  s.networkLoaded = true; s.networkAvailable = true; s.networkHasNext = true; s.networkPage = 3;
  s.networkQuery = 'Synthetic Cable'; s.networks = [{id:'49',name:'Synthetic Cable'}];
  c.context.state.provider = '8'; c.context.state.page = 6; c.context.state.q = 'Synthetic Title';
  c.context.controller.setDiscoveryMode('networks');
  assert.equal(c.nodes.get('libProviderMore').hidden, true); assert.equal(c.nodes.get('libNetworkMore').hidden, false);
  const networkTiles = c.nodes.get('libProviders').children;
  toggleProviderPicker(c);
  assert.equal(c.nodes.get('libProviders').children, networkTiles, 'hidden provider controls cannot replace network results');
  assert.equal(s.networkPage, 3); assert.equal(s.networkQuery, 'Synthetic Cable');
  assert.equal(c.context.state.page, 6); assert.equal(c.context.state.q, 'Synthetic Title'); assert.equal(c.context.state.provider, '8');
  c.context.controller.setDiscoveryMode('streaming');
  assert.equal(c.nodes.get('libNetworkMore').hidden, true); assert.equal(c.nodes.get('libProviderMore').hidden, false);
  assert.equal(providerButtons(c).length, s.providers.length, 'switching picker tabs must retain streaming expansion');
});

test('legacy provider variant permalinks select and name exactly one consolidated brand', () => {
  const c = discoveryController(), s = c.context.controller.discoveryState;
  Object.assign(c.context.state, parseBrowseState('?provider=582&region=US&type=tv_show&q=Synthetic'));
  s.providers = [groupedProvider(), {id:8,name:'Unrelated Stream',providerIds:[8],aliases:['Unrelated Stream']}];
  c.context.controller.filterProviders();
  const focused = c.nodes.get('libProviders').children[1]; c.document.activeElement = focused;
  c.context.controller.syncProviderControls();
  const buttons = c.nodes.get('libProviders').children;
  const selected = buttons.filter(button => button.getAttribute('aria-pressed') === 'true');
  assert.equal(selected.length, 1); assert.equal(selected[0].dataset.provider, '531');
  assert.equal(selected[0].dataset.provider, focused.dataset.provider);
  assert.equal(c.document.activeElement, selected[0], 'legacy selection hydration must retain focus on the canonical brand');
  assert.equal(selected[0].isConnected, true);
  assert.equal(selected[0].children[1].textContent, 'Paramount+');
  assert.equal(c.nodes.get('libSearch').placeholder, 'Search Paramount+ movies and series…');
  assert.equal(c.context.state.provider, '582', 'metadata hydration must not rewrite a legacy history entry');
  assert.equal(browseParamsFor(c.context.state).get('provider'), '582');
});

test('provider alias search finds a single brand row without duplicating matching variants', () => {
  const c = discoveryController(), s = c.context.controller.discoveryState;
  s.providers = [groupedProvider(), {id:9001,name:'Paramount Pictures',providerIds:[9001],aliases:['Paramount Pictures']}];
  c.document.getElementById('libProviderSearch').value = 'eSsEnTiAl'; c.context.controller.searchDiscoveryPicker();
  let buttons = c.nodes.get('libProviders').children;
  assert.equal(buttons.length, 2, 'All services and exactly one matching brand remain');
  assert.equal(buttons[1].children[1].textContent, 'Paramount+');
  c.nodes.get('libProviderSearch').value = 'Paramount+'; c.context.controller.searchDiscoveryPicker();
  buttons = c.nodes.get('libProviders').children;
  assert.equal(buttons.length, 2); assert.equal(buttons[1].dataset.provider, '531');
  assert.equal(c.context.state.q, '', 'service alias search never becomes a title search');
});

test('provider metadata hydration resolves a legacy variant label without changing its request identity', async () => {
  const c = discoveryController();
  Object.assign(c.context.state, parseBrowseState('?provider=2616&region=US&type=movie'));
  c.context.api = async () => ({available:true,providers:[groupedProvider()],regions:[{code:'US',name:'United States'}]});
  // Calling the real provider loader exercises post-response control sync.
  await vm.runInContext('loadProviders()', c.context);
  assert.equal(c.nodes.get('libSearch').placeholder, 'Search Paramount+ movies and series…');
  const selected = c.nodes.get('libProviders').children.filter(button => button.getAttribute('aria-pressed') === 'true');
  assert.equal(selected.length, 1); assert.equal(selected[0].dataset.provider, '531');
  const params = discoveryCatalogueParamsFor(c.context.state);
  assert.equal(params.get('provider'), '2616'); assert.equal(params.get('type'), 'movie');
});

test('a catalogue response names its canonical brand before the provider picker finishes loading', async () => {
  const c = discoveryController();
  Object.assign(c.context.state, parseBrowseState('?provider=2616&type=movie'));
  c.context._navSeq = 7; c.context.groupCache = new Map(); c.context.setGridLoading = () => {};
  c.context.api = async () => ({available:true,provider:2616,providerName:'Paramount+',providerIds:[531,582],items:[],hasNext:false});
  await c.context.controller.loadProviderLibrary(7, new AbortController().signal);
  assert.equal(c.nodes.get('libCatalogueTitle').textContent, 'Titles on Paramount+');
  assert.match(c.nodes.get('libCatalogueSubtitle').textContent, /US.*check indexed releases/);
  assert.equal(c.nodes.get('libResultNote').textContent, '0 titles on this page.');
  assert.equal(c.context.state.provider, '2616');
});

function catalogueController(search) {
  const c = discoveryController(), requests = [], loading = [];
  Object.assign(c.context.state, parseBrowseState(search));
  c.context.controller.discoveryState.providers = [groupedProvider()];
  c.context._navSeq = 1; c.context.groupCache = new Map();
  c.context.setGridLoading = value => loading.push(value);
  c.context.api = (url, options) => new Promise(resolve => requests.push({url,options,resolve}));
  return {...c,requests,loading};
}
function descendantImages(node) {
  return node.children.flatMap(child => [ ...(child.tagName === 'IMG' ? [child] : []), ...descendantImages(child) ]);
}

test('catalogue heading logos use safe fallbacks and clear when returning to indexed search', () => {
  const c = catalogueController('?provider=2616&q=Synthetic');
  c.context.controller.discoveryState.providers = [groupedProvider({logo:'https://unrelated.invalid/logo.png',
    logoAlternatives:['javascript:alert(1)','https://image.tmdb.org/t/p/original/synthetic-heading.svg']})];
  c.context.controller.syncProviderControls();
  const logo = c.nodes.get('libCatalogueLogo'), images = descendantImages(logo);
  assert.equal(logo.hidden, false); assert.equal(images.length, 1);
  assert.equal(images[0].src, 'https://image.tmdb.org/t/p/original/synthetic-heading.svg');
  images[0].listeners.get('error')();
  assert.equal(images[0].hidden, true); assert.equal(logo.children[0].children[0].hidden, false);
  assert.equal(c.nodes.get('libCatalogueTitle').textContent, 'Titles on Paramount+', 'a logo failure cannot erase the selected identity');
  c.context.state.provider = ''; c.context.controller.syncProviderControls();
  assert.equal(c.nodes.get('libCatalogueTitle').textContent, 'Search results');
  assert.equal(logo.hidden, true); assert.equal(descendantImages(logo).length, 0);
  assert.doesNotMatch(c.nodes.get('libCatalogueSubtitle').textContent, /Paramount|availability/i);
  c.context.state.q = ''; c.context.controller.syncProviderControls();
  assert.equal(c.nodes.get('libCatalogueHeading').hidden, true, 'the landing page must not retain a stale service heading');
});

test('catalogue headings identify the selected brand while loading and do not promise indexed availability', async () => {
  const c = catalogueController('?provider=2616&region=US&type=movie&q=Synthetic&page=2');
  const pending = c.context.controller.loadProviderLibrary(1, new AbortController().signal);
  assert.equal(c.nodes.get('libCatalogueHeading').hidden, false);
  assert.match(c.nodes.get('libCatalogueTitle').textContent, /Paramount\+/);
  assert.match(c.nodes.get('libResultNote').textContent, /loading titles/i);
  assert.match(c.nodes.get('libCatalogueSubtitle').textContent, /check indexed releases/i);
  assert.equal((c.nodes.get('libraryGrid').innerHTML.match(/lib-skeleton/g) || []).length, 12);
  const images = descendantImages(c.nodes.get('libCatalogueLogo'));
  assert.equal(images.length, 1); assert.equal(images[0].src, groupedProvider().logo);
  assert.equal(images[0].alt, '', 'the adjacent visible brand name supplies the logo meaning');
  c.requests[0].resolve({available:true,providerName:'Paramount+',items:[title()],hasNextPage:true,partial:true}); await pending;
  assert.match(c.nodes.get('libCatalogueTitle').textContent, /Paramount\+/);
  assert.match(c.nodes.get('libCatalogueSubtitle').textContent, /check indexed releases/i);
  assert.match(c.nodes.get('libResultNote').textContent, /^1 title on this page\./);
  assert.match(c.nodes.get('libResultNote').textContent, /partial/i);
  assert.doesNotMatch(c.nodes.get('libCatalogueSubtitle').textContent, /loading|fetching/i);
  assert.match(c.nodes.get('libraryGrid').innerHTML, /Find releases/);
  assert.doesNotMatch(c.nodes.get('libraryGrid').innerHTML, /magnet:|available releases|download now/i);
  assert.equal(c.nodes.get('libNext').disabled, false); assert.equal(c.nodes.get('libPrev').disabled, false);
});

test('empty and unavailable catalogue states retain their selected identity and distinguish metadata from releases', async () => {
  const c = catalogueController('?provider=2616&type=movie&q=Synthetic');
  let pending = c.context.controller.loadProviderLibrary(1, new AbortController().signal);
  c.requests[0].resolve({available:true,providerName:'Paramount+',items:[],hasNextPage:false}); await pending;
  assert.match(c.nodes.get('libCatalogueTitle').textContent, /Paramount\+/);
  assert.match(c.nodes.get('libraryGrid').innerHTML, /No matching titles/);
  assert.doesNotMatch(c.nodes.get('libraryGrid').innerHTML, /no (?:available |indexed )?releases/i);
  assert.equal(c.nodes.get('libNext').disabled, true);
  pending = c.context.controller.loadProviderLibrary(1, new AbortController().signal);
  c.requests[1].resolve({available:false,items:[],hasNextPage:true}); await pending;
  assert.equal(c.nodes.get('libCatalogueHeading').hidden, false);
  assert.match(c.nodes.get('libCatalogueTitle').textContent, /Paramount\+/);
  assert.match(c.nodes.get('libCatalogueSubtitle').textContent, /check indexed releases/i);
  assert.match(c.nodes.get('libResultNote').textContent, /temporarily unavailable/);
  assert.match(c.nodes.get('libraryGrid').innerHTML, /Retry/);
  assert.equal(c.context.state.hasNext, false); assert.equal(c.nodes.get('libPager').style.display, 'none');
});

test('a stale provider response cannot overwrite a newer network heading, titles, paging or loading status', async () => {
  const c = catalogueController('?provider=2616&type=movie');
  const old = c.context.controller.loadProviderLibrary(1, new AbortController().signal);
  Object.assign(c.context.state, parseBrowseState('?network=49&q=New&page=3'));
  c.context._navSeq = 2; c.context.controller.discoveryState.networkNames.set('49','Synthetic Cable');
  const latest = c.context.controller.loadProviderLibrary(2, new AbortController().signal);
  c.requests[1].resolve({available:true,networkName:'Synthetic Cable',items:[title({id:99,type:'tv_show',title:'New Synthetic Series'})],hasNextPage:true}); await latest;
  const titleText = c.nodes.get('libCatalogueTitle').textContent, subtitle = c.nodes.get('libCatalogueSubtitle').textContent;
  const results = c.nodes.get('libraryGrid').innerHTML, note = c.nodes.get('libResultNote').textContent;
  assert.match(titleText, /Synthetic Cable/); assert.match(results, /New Synthetic Series/);
  c.requests[0].resolve({available:true,providerName:'Outdated Brand',items:[title({id:98,title:'Old Synthetic Film'})],hasNextPage:false}); await old;
  assert.equal(c.nodes.get('libCatalogueTitle').textContent, titleText);
  assert.equal(c.nodes.get('libCatalogueSubtitle').textContent, subtitle);
  assert.equal(c.nodes.get('libraryGrid').innerHTML, results); assert.equal(c.nodes.get('libResultNote').textContent, note);
  assert.equal(c.context.state.hasNext, true); assert.equal(c.nodes.get('libPaginationInfo').textContent, 'Page 3');
  assert.deepEqual(c.loading, [true,true,false], 'stale responses must not clear the latest loading indicator');
  assert.deepEqual(Array.from(c.context.controller.discoveryState.titles.keys()), ['tv_show:99']);
});

test('aborted catalogue responses cannot replace pending catalogue identity or result state', async () => {
  const c = catalogueController('?provider=2616'), controller = new AbortController();
  const pending = c.context.controller.loadProviderLibrary(1, controller.signal);
  const heading = c.nodes.get('libCatalogueTitle').textContent, loadingText = c.nodes.get('libraryGrid').innerHTML;
  controller.abort(); c.requests[0].resolve({available:true,providerName:'Late Brand',items:[title()],hasNextPage:true}); await pending;
  assert.equal(c.nodes.get('libCatalogueTitle').textContent, heading); assert.equal(c.nodes.get('libraryGrid').innerHTML, loadingText);
  assert.deepEqual(c.loading, [true]); assert.equal(c.context.controller.discoveryState.titles.size, 0);
});

function countryController() {
  const c = discoveryController(), requests = [], libraries = [];
  Object.assign(c.context.state, parseBrowseState('?provider=582&region=US&type=movie'));
  c.context.controller.discoveryState.providers = [groupedProvider(), {id:175,name:'Netflix',providerIds:[175],aliases:['Netflix Standard with Ads']}];
  c.context.loadSpotlight = () => {};
  c.context.loadLibrary = options => { libraries.push({provider:c.context.state.provider,region:c.context.state.region,push:!!options.push}); c.context.controller.syncProviderControls(); };
  c.context.api = (url, options) => new Promise(resolve => requests.push({url,options,resolve}));
  return {...c,requests,libraries};
}
function countryProviders(providers) {
  return {available:true,providers,regions:[{code:'US',name:'United States'},{code:'GB',name:'United Kingdom'},{code:'CA',name:'Canada'}]};
}
function ukParamount() {
  return groupedProvider({id:2303,providerIds:[2303,1853],idsByType:{movie:[2303],tv_show:[1853,2303]}});
}
function chooseCountry(c, code) {
  // A real select onchange updates its DOM value before invoking the controller.
  c.document.getElementById('libProviderRegion').value = code;
  return c.context.controller.changeProviderRegion(code);
}

test('country changes remap a vanished variant only after fresh metadata for the same brand arrives', async () => {
  const c = countryController();
  const pending = chooseCountry(c,'GB');
  assert.equal(c.context.state.provider,'582'); assert.equal(c.libraries.length,0);
  assert.equal(new URL(c.requests[0].url,'http://synthetic.invalid').searchParams.get('region'),'GB');
  c.requests[0].resolve(countryProviders([ukParamount()])); await pending;
  assert.equal(c.context.state.provider,'2303');
  assert.deepEqual(c.libraries,[{provider:'2303',region:'GB',push:true}]);
  assert.equal(c.nodes.get('libSearch').placeholder,'Search Paramount+ movies and series…');
  assert.equal(c.nodes.get('libProviders').children[1].getAttribute('aria-pressed'),'true');
});

test('a successful country catalogue without the selected brand falls back to indexed titles', async () => {
  const c = countryController();
  const pending = chooseCountry(c,'GB');
  c.requests[0].resolve(countryProviders([{id:8,name:'Netflix',providerIds:[8]}])); await pending;
  assert.equal(c.context.state.provider,'');
  assert.deepEqual(c.libraries,[{provider:'',region:'GB',push:true}]);
  assert.equal(c.nodes.get('libSearch').placeholder,'Search titles, seasons, and releases…');
});

test('an older country response cannot replace the latest country and canonical service identity', async () => {
  const c = countryController();
  const old = chooseCountry(c,'GB');
  const latest = chooseCountry(c,'CA');
  c.requests[1].resolve(countryProviders([groupedProvider({id:633,providerIds:[633]})])); await latest;
  c.requests[0].resolve(countryProviders([ukParamount()])); await old;
  assert.equal(c.context.state.region,'CA'); assert.equal(c.context.state.provider,'633');
  assert.deepEqual(c.libraries,[{provider:'633',region:'CA',push:true}]);
  assert.equal(c.nodes.get('libProviderRegion').value,'CA');
});

test('a brand selected during a country request is remapped to that brand instead of restoring the original selection', async () => {
  const c = countryController();
  const pending = chooseCountry(c,'GB');
  c.context.controller.selectProvider('175');
  assert.equal(c.context.state.provider,'175','the user choice must remain until the new country metadata arrives');
  c.requests[0].resolve(countryProviders([ukParamount(),{id:8,name:'Netflix',providerIds:[8],aliases:['Netflix']} ])); await pending;
  assert.equal(c.context.state.provider,'8');
  assert.equal(c.libraries.at(-1).provider,'8'); assert.equal(c.libraries.at(-1).region,'GB');
  assert.deepEqual(c.libraries.map(row=>row.provider),['175','8']);
  assert.equal(c.nodes.get('libSearch').placeholder,'Search Netflix movies and series…');
});

test('a country-specific display case and identity change retains the normalized service brand', async () => {
  const c = countryController();
  c.context.state.provider = '446';
  c.context.controller.discoveryState.providers = [{id:446,name:'Retrocrush',brandKey:'retrocrush',providerIds:[446]}];
  const pending = chooseCountry(c,'GB');
  c.requests[0].resolve(countryProviders([{id:295,name:'RetroCrush',brandKey:'retrocrush',providerIds:[295]}])); await pending;
  assert.equal(c.context.state.provider,'295'); assert.equal(c.libraries.at(-1).provider,'295');
  assert.equal(c.nodes.get('libSearch').placeholder,'Search RetroCrush movies and series…');
});

test('network picker load-more uses a separate cursor and deduplicates repeated network identities', async () => {
  const c = discoveryController(), requests = [], s = c.context.controller.discoveryState;
  s.pickerMode = 'networks'; c.context.state.page = 6;
  c.context.api = (url, options) => new Promise(resolve => requests.push({url, options, resolve}));
  const first = c.context.controller.loadNetworks();
  assert.equal(new URL(requests[0].url, 'http://synthetic.invalid').searchParams.get('page'), '1');
  requests[0].resolve({available:true, networks:[{id:49, name:'Synthetic Cable'}], page:1, hasNextPage:true, total:3, partial:true}); await first;
  const second = c.context.controller.loadNetworks(true);
  assert.equal(new URL(requests[1].url, 'http://synthetic.invalid').searchParams.get('page'), '2');
  requests[1].resolve({available:true, networks:[{id:49, name:'Synthetic Cable'}, {id:50, name:'Second Cable'}], page:2, hasNext:false, total:3}); await second;
  assert.deepEqual(Array.from(s.networks, n => n.id), ['49', '50']); assert.equal(s.networkPage, 2);
  assert.equal(c.context.state.page, 6, 'network pagination cannot move the title catalogue');
  assert.equal(c.nodes.get('libNetworkMore').hidden, true);
  assert.equal(s.networkPartial, true, 'appending a complete page must retain earlier partial-page disclosure');
  assert.match(c.nodes.get('libProviderStatus').textContent, /list is partial/);
});

test('out-of-order network responses cannot replace the latest picker search', async () => {
  const c = discoveryController(), requests = [], s = c.context.controller.discoveryState;
  s.pickerMode = 'networks';
  c.context.api = (url, options) => new Promise(resolve => requests.push({url, options, resolve}));
  s.networkQuery = 'old'; const first = c.context.controller.loadNetworks();
  s.networkQuery = 'new'; const second = c.context.controller.loadNetworks();
  assert.equal(requests[0].options.signal.aborted, true);
  requests[1].resolve({available:true, networks:[{id:50, name:'New Cable'}], hasNext:false}); await second;
  requests[0].resolve({available:true, networks:[{id:49, name:'Old Cable'}], hasNext:true}); await first;
  assert.deepEqual(Array.from(s.networks, n => n.name), ['New Cable']); assert.equal(s.networkHasNext, false);
});

test('failed network metadata is disclosed and retried without presenting a false name-search miss', async () => {
  const c = discoveryController(), requests = [], s = c.context.controller.discoveryState;
  s.pickerMode = 'networks'; s.networkQuery = 'Synthetic';
  c.context.api = (url, options) => new Promise(resolve => requests.push({url, options, resolve}));
  const first = c.context.controller.loadNetworks();
  requests[0].resolve({available:true, networks:[], hasNext:false, partial:true, total:5}); await first;
  assert.match(c.nodes.get('libProviderStatus').textContent, /details are temporarily unavailable/);
  assert.doesNotMatch(c.nodes.get('libProviderStatus').textContent, /No networks match/);
  assert.equal(c.nodes.get('libNetworkMore').hidden, false); assert.equal(c.nodes.get('libNetworkMore').textContent, 'Retry networks');
  const retry = c.context.controller.loadNetworks(true);
  assert.equal(new URL(requests[1].url, 'http://synthetic.invalid').searchParams.get('page'), '1');
  requests[1].resolve({available:true, networks:[{id:49, name:'Synthetic Cable'}], hasNext:false, partial:false}); await retry;
  assert.equal(s.networkPartial, false); assert.equal(s.networks.length, 1); assert.equal(c.nodes.get('libNetworkMore').hidden, true);
});

test('network name search debounces requests and never replaces the title query', async () => {
  const c = discoveryController(), requests = [], s = c.context.controller.discoveryState;
  s.pickerMode = 'networks'; c.context.state.q = 'Synthetic Title';
  c.context.api = (url, options) => new Promise(resolve => requests.push({url, options, resolve}));
  c.document.getElementById('libProviderSearch').value = 'First'; c.context.controller.searchDiscoveryPicker();
  c.document.getElementById('libProviderSearch').value = 'Synthetic & Cable'; c.context.controller.searchDiscoveryPicker();
  assert.equal(c.timers.size, 1); assert.equal(requests.length, 0); c.fireTimer();
  assert.equal(requests.length, 1); assert.equal(new URL(requests[0].url, 'http://synthetic.invalid').searchParams.get('q'), 'Synthetic & Cable');
  assert.equal(c.context.state.q, 'Synthetic Title');
  requests[0].resolve({available:true, networks:[], hasNext:false}); await Promise.resolve();
});

test('direct network permalinks acquire the genuine network name from catalogue metadata', async () => {
  const c = discoveryController(), s = c.context.controller.discoveryState;
  c.context.state.network = '49'; c.context.state.type = 'tv_show';
  s.pickerMode = 'networks'; s.selectionKey = 'network:49'; s.networkLoaded = true;
  c.context._navSeq = 7; c.context.groupCache = new Map(); c.context.setGridLoading = () => {};
  c.context.api = async () => ({available:true, network:49, networkName:'Synthetic Cable', items:[], hasNext:false});
  await c.context.controller.loadProviderLibrary(7, new AbortController().signal);
  assert.equal(c.nodes.get('libCatalogueTitle').textContent, 'Titles on Synthetic Cable');
  assert.match(c.nodes.get('libCatalogueSubtitle').textContent, /Series catalogue.*check indexed releases/);
  assert.equal(c.nodes.get('libSearch').placeholder, 'Search Synthetic Cable series…');
  assert.equal(s.networkNames.get('49'), 'Synthetic Cable');
});

test('reduced motion disables rotation and a modal prevents an automatic slide change', () => {
  const reduced = discoveryController(true);
  reduced.context.controller.scheduleSpotlight();
  assert.equal(reduced.nodes.get('libSpotlightPause').disabled,true); assert.equal(reduced.timers.size, 0);
  const c = discoveryController();
  c.context.controller.scheduleSpotlight(); c.context._modalStack.push({}); c.fireTimer();
  assert.equal(c.context.controller.discoveryState.index, 0);
  assert.equal(c.timers.size, 1, 'rotation may retry after the modal closes without advancing the current title');
});

test('motion preference events stop timers and artwork animation without overriding a manual carousel pause', () => {
  const c = discoveryController(); let reduced = false, animations = 0;
  c.context._reducedMotion = () => reduced;
  const image = c.nodes.get('libSpotlightImage') || c.document.getElementById('libSpotlightImage');
  image.animate = () => { animations++; }; c.nodes.get('libSpotlight').querySelector = () => null;
  c.context.controller.toggleSpotlightPause(true);
  reduced = true; c.preferenceListeners.get('bitagent:preferences')();
  assert.equal(c.timers.size,0); assert.equal(c.nodes.get('libSpotlightPause').disabled,true);
  c.context.controller.renderSpotlight(); assert.equal(animations,0);
  reduced = false; c.preferenceListeners.get('bitagent:preferences')();
  assert.equal(c.context.controller.discoveryState.paused,true); assert.equal(c.timers.size,0);
  c.context.controller.toggleSpotlightPause(false); assert.equal(c.timers.size,1);
  reduced = true; c.motionListeners.get('change')(); assert.equal(c.timers.size,0);
  assert.match(c.nodes.get('libSpotlightPause').getAttribute('aria-label'),/reduced motion/i);
  reduced = false; c.preferenceListeners.get('bitagent:preferences')(); c.context.controller.moveSpotlight(1);
  assert.equal(animations,1); assert.equal(c.timers.size,1);
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
  const context = vm.createContext({ URLSearchParams, AbortController, setTimeout, clearTimeout, BitAgentLibraryTools: releaseTools,window:{BitAgentPreferences:preferences} });
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

test('account usage renders coherent latest revisions and permits a new tracking epoch', () => {
  const c = magnetController(), nodes = new Map();
  c.document = {getElementById(id) { if (!nodes.has(id)) nodes.set(id, {setAttribute() {},querySelectorAll() {return [];}}); return nodes.get(id); }};
  vm.runInContext("_accountOwner='alice';_accountMethod='oidc';",c);
  c.controller.renderAccountUsage(accountUsage({grabs:12,magnetCopies:10,magnetOpens:1,magnetExports:1,apiSearches:42,revision:10}));
  c.controller.renderAccountUsage(accountUsage({grabs:11,magnetCopies:9,magnetOpens:1,magnetExports:1,apiSearches:99,revision:9}));
  assert.equal(nodes.get('acctGrabs').textContent, '12');
  assert.equal(nodes.get('acctApiSearches').textContent, '42');
  c.controller.renderAccountUsage(accountUsage({grabs:14,magnetCopies:12,magnetOpens:1,magnetExports:1,apiSearches:44,revision:11}));
  assert.equal(nodes.get('acctGrabs').textContent, '14');
  assert.equal(nodes.get('acctApiSearches').textContent, '44'); assert.equal(nodes.get('acctCopies').textContent,'12');
  c.controller.renderAccountUsage(accountUsage({revision:0,trackingSince:1790812800,observedAt:'2026-10-01T00:00:00Z'}));
  assert.equal(nodes.get('acctGrabs').textContent, '0', 'a new tracking period can legitimately reset activity');
  assert.equal(nodes.get('acctApiSearches').textContent, '0');
});

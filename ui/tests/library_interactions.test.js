'use strict';

const test = require('node:test');
const assert = require('node:assert/strict');
const vm = require('node:vm');
const fs = require('node:fs');
const path = require('node:path');
const releaseTools = require('../static/js/library-tools.js');
const preferences = require('../static/js/library-preferences.js');

function controller(globals = {}) {
  const context = vm.createContext({ URLSearchParams, AbortController, setTimeout, clearTimeout, window:{BitAgentPreferences:preferences},...globals });
  // Import with no DOM, then attach only the UI primitives required by each
  // interaction. Exercise the production functions, including async races.
  const source = fs.readFileSync(path.join(__dirname, '../static/js/library.js'), 'utf8');
  vm.runInContext(source + `
    this.controller = { detail, groupCache, openDetail, maybeLoadSeasonMeta, fallbackCopy, magnetGroupToolbar, setMagnetMode, restoreLibraryHistory, releaseFilterBar, renderDetailBody, refreshDetailContent, setQuality, setSource, setEdition, filteredReleases, copyMagnetGroup,
      loadAccount, renderAccount, renderAccountUsage, recordLibraryGrab, retryAccountUsage, generateAccountKey, revokeAccountKey, closeAccount, setAccountPreference, retryAccountPreferences, resetAccountPreferences, setAccountUsagePeriod, _initializeAccountPreferences, _applyAccountAppearance, _reducedMotion, _scrollBehavior, copyText, copyMagnet };
    this.accountTesting = {get state(){return accountState;},get owner(){return _accountOwner;},get connected(){return _accountConnected;},get queue(){return _accountGrabQueue;},get snapshot(){return _accountUsageSnapshot;}};
  `, context);
  return context;
}

test('a delayed season catalog cannot replace the next title catalog', async () => {
  const c = controller();
  const d = c.controller.detail;
  d.tmdbId = '100'; d.activeSeason = 1; d.group = { key: 'first' };
  let finish;
  c.api = () => new Promise(resolve => { finish = resolve; });
  c.document = { getElementById() { throw new Error('A stale response must not render'); } };
  const pending = c.controller.maybeLoadSeasonMeta();
  d.tmdbId = '200'; d.group = { key: 'second' }; d.seasonCache = new Map();
  finish({ episodes: [{ name: 'Old title episode' }] });
  await pending;
  assert.equal(d.seasonCache.size, 0);
});

test('repeated paints share one in-flight season catalog request', async () => {
  const c = controller();
  const d = c.controller.detail;
  d.tmdbId = '100'; d.activeSeason = 1; d.group = { key: 'first' };
  let calls = 0, finish;
  c.api = () => { calls++; return new Promise(resolve => { finish = resolve; }); };
  c.document = { getElementById() { return null; } };
  const first = c.controller.maybeLoadSeasonMeta();
  await c.controller.maybeLoadSeasonMeta();
  assert.equal(calls, 1);
  finish({ episodes: [] }); await first;
});

test('all-version mode survives toolbar rerenders and invalid choices', () => {
  const c = controller();
  c.controller.setMagnetMode('all');
  c.controller.setMagnetMode('unexpected');
  assert.match(c.controller.magnetGroupToolbar(), /value="all" selected/);
  assert.equal(c.controller.detail.bulkMode, 'all');
});

test('season shortcut is disabled without a valid active season', () => {
  const c = controller();
  c.controller.detail.ct = 'tv_show';
  c.controller.detail.activeSeason = null;
  assert.match(c.controller.magnetGroupToolbar(), /disabled onclick="copyMagnetGroup\('season'\)"/);
});

test('clipboard fallback never reports success when the browser rejects copying', () => {
  const c = controller();
  let success = false, message = '';
  c.toast = value => { message = value; };
  c.document = { createElement() { return { style: {}, select() {} }; },
    body: { appendChild() {}, removeChild() {} }, execCommand() { return false; } };
  c.controller.fallbackCopy('synthetic link', () => { success = true; });
  assert.equal(success, false);
  assert.match(message, /Copy failed/);
});

test('Forward reopens the magnet collection without closing its title', () => {
  const c = controller();
  const calls = [];
  c.document = { getElementById(id) { return { classList: { contains() { return id === 'libDetail'; } } }; } };
  c.closeMagnetCollection = () => calls.push('close collection');
  c.closeAccount = () => calls.push('close account');
  c.closeTorrentDrawer = () => calls.push('close drawer');
  c.closeDetail = () => calls.push('close title');
  c.openMagnetCollection = fromPop => calls.push(fromPop ? 'restore collection' : 'push collection');
  c.controller.restoreLibraryHistory({ lib: 'collection' });
  assert.equal(calls.includes('restore collection'), true);
  assert.equal(calls.includes('close title'), false);
  assert.equal(calls.includes('push collection'), false);
});

test('magnet actions become ready while optional title metadata is still pending', async () => {
  const c = controller();
  const g = { key: 'synthetic', contentType: 'movie', contentSource: 'tmdb', contentId: '100',
    best: { name: 'Synthetic Film', contentSource: 'tmdb', contentId: '100' }, items: [] };
  c.controller.groupCache.set(g.key, g);
  c.controller.detail.filtersExpanded = true;
  const view = { classList: { add() {} }, setAttribute() {} };
  c.document = { getElementById(id) { return id === 'libDetail' ? view : { scrollTop: 0 }; } };
  c.history = { state: { lib: 'browse' }, pushState() {}, replaceState() {} };
  c.openModal = () => {};
  c._detailUrl = () => '/library?title=Synthetic';
  c.renderDetailShell = () => {};
  c.renderDetailBody = () => {};
  c.refreshDetailContent = () => {};
  let finishMetadata;
  c.api = () => new Promise(resolve => { finishMetadata = resolve; });
  c.BitAgentLibraryTools = { collectTitleReleases: async () => ({ items: [{ infoHash: 'a'.repeat(40) }], truncated: false }) };
  const pending = c.controller.openDetail(g.key);
  assert.equal(c.controller.detail.filtersExpanded, false, 'each new title must begin with the native filters collapsed');
  await new Promise(resolve => setImmediate(resolve));
  assert.equal(c.controller.detail.loading, false);
  assert.equal(c.controller.detail.releases.length, 1);
  assert.equal(c.controller.detail.meta, null);
  finishMetadata({ overview: 'Synthetic metadata' });
  await pending;
  assert.equal(c.controller.detail.meta.overview, 'Synthetic metadata');
});

function filterController() {
  const c = controller();
  c.BitAgentLibraryTools = releaseTools;
  const decodeAttribute = value => value.replace(/&(?:amp|quot|#39|lt|gt|#96);/g,
    entity => ({'&amp;':'&','&quot;':'"','&#39;':"'",'&lt;':'<','&gt;':'>','&#96;':'`'}[entity]));
  let markup = '', buttons = [], disclosure = null;
  function focusable(attrs) {
    return {isConnected:true, getAttribute(name) { return attrs[name]; },
      focus(options) { document.activeElement = this; this.focusOptions = options; },
      getClientRects() { return this.isConnected ? [{}] : []; }, closest() { return null; }};
  }
  const body = {get innerHTML() { return markup; }, set innerHTML(value) {
    for (const old of buttons.concat(disclosure?.summary || [])) {
      old.isConnected = false; if (document.activeElement === old) document.activeElement = null;
    }
    markup = value;
    // Model native HTML attribute decoding. Raw apostrophes/ampersands in the
    // DOM differ from the encoded attribute text emitted by the template.
    buttons = Array.from(value.matchAll(/<button\b[^>]*class="lib-chip-btn[^>]*>/g), match => {
      const attrs = Object.fromEntries(Array.from(match[0].matchAll(/([\w-]+)="([^"]*)"/g), attr => [attr[1],decodeAttribute(attr[2])]));
      return focusable(attrs);
    });
    const native = value.match(/<details\b[^>]*class="lib-detail-filters"([^>]*)>/);
    disclosure = native ? {open:/\sopen(?:\s|$)/.test(native[1]),
      handler:decodeAttribute(native[1].match(/ontoggle="([^"]*)"/)[1]), summary:focusable({'aria-label':'Filter releases'})} : null;
  }};
  const scroll = {set innerHTML(value) { this.markup = value; body.innerHTML = ''; }};
  const view = {contains(node) { return buttons.includes(node) || disclosure?.summary === node; },
    querySelectorAll(selector) { return selector === '[aria-label]' && disclosure ? [disclosure.summary] : []; }};
  const document = {activeElement:null, getElementById(id) {
    return {libDetail:view,libDetailBody:body,libDetailScroll:scroll}[id] || null;
  }, querySelectorAll(selector) { return selector === '.lib-detail-filter-body button' ? buttons : []; }};
  c.document = document;
  const releases = [
    ['1','Synthetic.1080p.WEB-DL.Extended'], ['2','Synthetic.2160p.WEB-DL.Extended'],
    ['3','Synthetic.1080p.BluRay.Extended'], ['4','Synthetic.1080p.WEB-DL.Theatrical'],
    ['1','Synthetic.1080p.WEB-DL.Extended'], ['6','Synthetic.1080p'],
  ].map(([hash,torrentName]) => ({infoHash:hash.padStart(40,'0'),torrentName,seeders:10}));
  releases[5].videoSource = 'Archive & "Director\'s"';
  Object.assign(c.controller.detail, {ct:'movie',title:'Synthetic',releases,
    group:{key:'synthetic',best:{name:'Synthetic'},isMapped:true}});
  return {c,document,body,scroll,get disclosure() { return disclosure; },get buttons() { return buttons; },
    toggle(open) {
      disclosure.open = open; c.nativeDetails = disclosure;
      vm.runInContext(`(function(){${disclosure.handler}}).call(nativeDetails)`, c);
    }};
}

test('native release filters start collapsed and preserve manual disclosure through release and metadata paints', () => {
  const f = filterController(), d = f.c.controller.detail;
  f.c.controller.renderDetailBody(); assert.equal(f.disclosure.open, false);
  assert.match(f.body.innerHTML, /<summary aria-label="Filter releases">/);
  assert.doesNotMatch(f.body.innerHTML, /\d active/);
  f.toggle(true); assert.equal(d.filtersExpanded, true);
  d.releases.push({infoHash:'7'.padStart(40,'0'),torrentName:'Synthetic.720p.WEBRip'});
  f.c.controller.refreshDetailContent(false); assert.equal(f.disclosure.open, true);
  d.meta = {overview:'Synthetic enriched metadata',cast:[{name:'Synthetic Performer'}]};
  f.disclosure.summary.focus(); f.c.controller.refreshDetailContent(true);
  assert.equal(f.disclosure.open, true); assert.match(f.scroll.markup, /Synthetic enriched metadata/);
  assert.equal(f.document.activeElement, f.disclosure.summary, 'metadata replacement must retain a reachable focused summary');
  f.toggle(false); f.c.controller.refreshDetailContent(false);
  assert.equal(d.filtersExpanded, false); assert.equal(f.disclosure.open, false);
});

test('release filter setters preserve semantic focus and count only active quality, source and edition filters', () => {
  const f = filterController(); f.c.controller.renderDetailBody();
  const cases = [[f.c.controller.setQuality,'1080p','1 active'],[f.c.controller.setSource,'WEB-DL','2 active'],
    [f.c.controller.setEdition,'Extended','3 active'],[f.c.controller.setQuality,'all','2 active']];
  for (const [set,value,count] of cases) {
    set(value);
    assert.equal(f.disclosure.open, true); assert.match(f.body.innerHTML, new RegExp(count));
    assert.ok(f.buttons.includes(f.document.activeElement)); assert.equal(f.document.activeElement.isConnected, true);
    assert.ok(f.document.activeElement.getAttribute('onclick').includes(`('${value}')`));
    assert.equal(f.document.activeElement.focusOptions.preventScroll, true);
  }
});

test('a source containing quotes and ampersands retains focus on its decoded DOM handler', () => {
  const f = filterController(), source = f.c.controller.detail.releases[5].videoSource;
  f.c.controller.setSource(source);
  assert.equal(f.c.controller.detail.source, source);
  assert.equal(f.document.activeElement?.isConnected, true);
  assert.match(f.document.activeElement.getAttribute('onclick'), /Archive &/);
  assert.doesNotMatch(f.document.activeElement.getAttribute('onclick'), /&amp;|&quot;|&#39;/);
  assert.deepEqual(Array.from(f.c.controller.filteredReleases(), release => release.infoHash), ['6'.padStart(40,'0')]);
});

test('copying filtered title magnets uses the visible quality, source and edition intersection and deduplicates hashes', () => {
  const f = filterController(), copied = [], recorded = [];
  f.c.copyText = (text,message,onSuccess) => { copied.push({text,message}); onSuccess(); };
  f.c.recordLibraryGrab = (count,action) => recorded.push({count,action});
  f.c.controller.detail.bulkMode = 'all';
  f.c.controller.setQuality('1080p'); f.c.controller.setSource('WEB-DL'); f.c.controller.setEdition('Extended');
  assert.equal(f.c.controller.filteredReleases().length, 2, 'both visible versions share one hash');
  f.c.controller.copyMagnetGroup('title');
  assert.equal(copied.length, 1); assert.equal(copied[0].text.split('\n').length, 1);
  assert.match(copied[0].text, new RegExp('btih:' + '1'.padStart(40,'0')));
  for (const excluded of ['2','3','4','6']) assert.ok(!copied[0].text.includes('btih:' + excluded.padStart(40,'0')));
  assert.deepEqual(recorded, [{count:1,action:'copy'}]); assert.match(copied[0].message, /1 magnet links copied/);
});

function measuredUsage(owner = 'alice', overrides = {}) {
  return {accountId:owner,available:true,status:'available',revision:0,observedAt:'2026-09-30T12:00:00Z',
    trackingSince:1790769600.5,grabs:0,apiSearches:0,magnetCopies:0,magnetOpens:0,magnetExports:0,
    downloadedBytes:null,uploadedBytes:null,hitAndRuns:null,...overrides};
}
function accountPayload(owner = 'alice', overrides = {}) {
  return {identity:{id:owner,method:'oidc',display:owner === 'alice' ? 'Alice Fixture' : 'Bob Fixture'},
    usage:measuredUsage(owner),apiKey:null,torznabUrl:'/api/torznab',...overrides};
}
function accountController(options = {}) {
  const nodes = new Map(), requests = [], timers = new Map(), toasts = [], cache = options.cache || new Map();
  let nextTimer = 0, nextEvent = 0;
  function element(id = '') {
    const attrs = new Map(), classes = new Set(), children = [];
    return {id,style:{},dataset:{},value:'',children,options:children,hidden:false,
      classList:{add(name){classes.add(name);},remove(name){classes.delete(name);},contains(name){return classes.has(name);}},
      setAttribute(name,value){attrs.set(name,String(value));},getAttribute(name){return attrs.get(name);},
      appendChild(child){children.push(child);},querySelectorAll(){return [];}};
  }
  const root = element(), themeMeta = element(), document = {documentElement:root,body:{dataset:{},style:{}},
    getElementById(id){if(!nodes.has(id))nodes.set(id,element(id));return nodes.get(id);},createElement(){return element();},
    querySelector(selector){return selector === 'meta[name="theme-color"]' ? themeMeta : null;},querySelectorAll(){return [];}};
  const media = new Map([['(prefers-color-scheme: dark)',{matches:true}],['(prefers-reduced-motion: reduce)',{matches:false}]]);
  const events = [], storage = options.storage || {getItem:key => cache.get(key)||null,setItem:(key,value) => cache.set(key,value)};
  const window = {BitAgentPreferences:preferences,localStorage:storage,matchMedia:query => media.get(query),dispatchEvent:event => events.push(event.type),confirm:options.confirm || (()=>true)};
  const c = controller({window,CustomEvent:class {constructor(type){this.type=type;}},
    crypto:{randomUUID:() => `00000000-0000-4000-8000-${String(++nextEvent).padStart(12,'0')}`},
    setTimeout(callback,delay){const id=++nextTimer;timers.set(id,{callback,delay});return id;},clearTimeout:id => timers.delete(id)});
  c.document = document; c.toast = message => toasts.push(message); c.closeModal = () => {};
  c.history = {state:{lib:'browse'}};
  c.fetch = (url,fetchOptions) => new Promise((resolve,reject) => {
    requests.push({url,options:fetchOptions,resolve,reject});
    fetchOptions.signal.addEventListener('abort',() => reject(new Error('Synthetic aborted request')));
  });
  c.controller._initializeAccountPreferences();
  return {c,nodes,requests,timers,toasts,cache,media,events,document,themeMeta,
    identify(owner = 'alice',extra = {}){c.controller.renderAccount(accountPayload(owner,extra));},
    respond(index,data,status = 200){requests[index].resolve({ok:status>=200&&status<300,status,json:async()=>data});},
    receipt(index,extra = {}){const event=JSON.parse(requests[index].options.body);return measuredUsage(event.expectedAccountId,
      {revision:1,grabs:event.count,magnetCopies:event.action==='copy'?event.count:0,magnetOpens:event.action==='open'?event.count:0,
        magnetExports:event.action==='export'?event.count:0,receipt:{eventId:event.eventId,recorded:true,duplicate:false},...extra});}};
}

test('late account GET responses cannot restore a previous identity or secret after a newer account response', async () => {
  const f = accountController(), first = f.c.controller.loadAccount(), latest = f.c.controller.loadAccount();
  f.respond(1,accountPayload('bob',{usage:measuredUsage('bob',{grabs:2,magnetCopies:2})})); await latest;
  f.respond(0,accountPayload('alice',{apiKeySecret:'synthetic-old-key',usage:measuredUsage('alice',{grabs:11,magnetCopies:11})})); await first;
  assert.equal(f.nodes.get('acctName').textContent,'Bob Fixture'); assert.equal(f.nodes.get('acctGrabs').textContent,'2');
  assert.equal(f.nodes.get('acctApiSecret').value,''); assert.equal(f.c.accountTesting.owner,'bob');
});

test('failed account refresh clears identity, secrets, URL and metrics until a confirmed retry succeeds', async () => {
  const f = accountController(); f.identify('alice',{apiKeySecret:'synthetic-single-use-key',apiKey:{prefix:'fixture'},
    usage:measuredUsage('alice',{grabs:3,magnetCopies:3})});
  const failed = f.c.controller.loadAccount(); f.respond(0,null,401); await failed;
  assert.equal(f.nodes.get('acctApiSecret').value,''); assert.equal(f.nodes.get('acctSecretWrap').style.display,'none');
  assert.equal(f.nodes.get('acctTorznabUrl').value,''); assert.equal(f.nodes.get('acctName').textContent,'Account unavailable');
  assert.equal(f.nodes.get('acctGrabs').textContent,'—'); assert.equal(f.c.accountTesting.state,null);
  assert.equal(f.c.accountTesting.connected,false); assert.equal(f.nodes.get('acctGenerateBtn').disabled,true);
  const retry = f.c.controller.loadAccount(); f.respond(1,accountPayload()); await retry;
  assert.equal(f.c.accountTesting.connected,true); assert.equal(f.nodes.get('acctGrabs').textContent,'0');
  assert.equal(f.nodes.get('acctGenerateBtn').disabled,false);
});

test('missing or malformed same-owner usage clears old totals while wrong-owner usage never paints', () => {
  const f = accountController(); f.identify('alice',{usage:measuredUsage('alice',{grabs:9,magnetCopies:9})});
  f.c.controller.renderAccountUsage(measuredUsage('bob',{grabs:20,magnetCopies:20,revision:20}));
  assert.equal(f.nodes.get('acctGrabs').textContent,'9');
  f.identify('alice',{usage:undefined}); assert.equal(f.nodes.get('acctGrabs').textContent,'—');
  assert.equal(f.c.accountTesting.snapshot,null);
  f.c.controller.renderAccountUsage(measuredUsage('alice',{grabs:9,magnetCopies:'9'}));
  assert.equal(f.nodes.get('acctGrabs').textContent,'—'); assert.match(f.nodes.get('acctUsageHint').textContent,/temporarily unavailable/i);
});

test('an activity response from the previous owner cannot paint the next account or remove its queued action', async () => {
  const f = accountController(); f.identify(); const old = f.c.controller.recordLibraryGrab(1,'copy');
  f.identify('bob',{usage:measuredUsage('bob',{grabs:2,magnetCopies:2})});
  const latest = f.c.controller.recordLibraryGrab(1,'open');
  f.respond(0,f.receipt(0,{grabs:11,magnetCopies:11}));
  await new Promise(resolve => setImmediate(resolve));
  assert.equal(f.nodes.get('acctName').textContent,'Bob Fixture'); assert.equal(f.nodes.get('acctGrabs').textContent,'2');
  assert.equal(JSON.parse(f.requests[1].options.body).expectedAccountId,'bob');
  f.respond(1,f.receipt(1,{grabs:3,magnetCopies:2,magnetOpens:1})); await Promise.all([old,latest]);
  assert.equal(f.nodes.get('acctGrabs').textContent,'3'); assert.equal(f.c.accountTesting.queue.length,0);
});

test('retrying a completed action reuses its event ID and receipt without counting twice or replaying the action', async () => {
  const f = accountController(); f.identify(); const action = f.c.controller.recordLibraryGrab(2,'copy');
  const original = JSON.parse(f.requests[0].options.body); f.respond(0,null,503); await action;
  assert.equal(f.c.accountTesting.queue.length,1); assert.equal(f.nodes.get('acctGrabs').textContent,'0');
  assert.match(f.nodes.get('acctUsageStatus').textContent,/retry/i); assert.equal(f.nodes.get('acctUsageRetry').hidden,false);
  const retry = f.c.controller.retryAccountUsage();
  assert.deepEqual(JSON.parse(f.requests[1].options.body),original);
  f.respond(1,f.receipt(1,{receipt:{eventId:original.eventId,recorded:true,duplicate:true}})); await retry;
  assert.equal(f.nodes.get('acctGrabs').textContent,'2'); assert.equal(f.c.accountTesting.queue.length,0);
  await f.c.controller.retryAccountUsage(); assert.equal(f.requests.length,2);
  const second = f.c.controller.recordLibraryGrab(2,'copy'); const next = JSON.parse(f.requests[2].options.body);
  assert.notEqual(next.eventId,original.eventId,'a second legitimate action gets its own receipt');
  f.respond(2,f.receipt(2,{revision:2,grabs:4,magnetCopies:4})); await second;
  assert.equal(f.nodes.get('acctGrabs').textContent,'4');
});

test('malformed or mismatched activity receipts retain the event for a safe retry', async () => {
  for (const bad of ['owner','event','recorded','counter']) {
    const f = accountController(); f.identify(); const action=f.c.controller.recordLibraryGrab(1,'open');
    const reply = f.receipt(0);
    if(bad==='owner')reply.accountId='bob'; if(bad==='event')reply.receipt.eventId='00000000-0000-4000-8000-999999999999';
    if(bad==='recorded')reply.receipt.recorded=false; if(bad==='counter')reply.grabs=9;
    f.respond(0,reply); await action;
    assert.equal(f.c.accountTesting.queue.length,1,bad); assert.equal(f.nodes.get('acctGrabs').textContent,'0',bad);
    assert.equal(f.nodes.get('acctUsageRetry').hidden,false,bad);
  }
});

test('failed account verification invalidates in-flight activity and defers its retry until identity is confirmed', async () => {
  const f = accountController(); f.identify(); const old = f.c.controller.recordLibraryGrab(1,'copy');
  const verification=f.c.controller.loadAccount(); f.respond(1,null,401); await verification;
  f.respond(0,f.receipt(0)); await old;
  assert.equal(f.nodes.get('acctGrabs').textContent,'—'); assert.equal(f.c.accountTesting.connected,false);
  await f.c.controller.retryAccountUsage(); assert.equal(f.requests.length,2,'unconfirmed identity cannot send queued writes');
  const confirmed=f.c.controller.loadAccount(); f.respond(2,accountPayload()); await confirmed;
  await new Promise(resolve => setImmediate(resolve));
  assert.equal(f.requests.length,4); assert.equal(JSON.parse(f.requests[3].options.body).eventId,JSON.parse(f.requests[0].options.body).eventId);
  f.respond(3,f.receipt(3,{receipt:{eventId:JSON.parse(f.requests[3].options.body).eventId,recorded:true,duplicate:true}}));
  await new Promise(resolve => setImmediate(resolve)); assert.equal(f.nodes.get('acctGrabs').textContent,'1');
});

test('queued activity survives a browser restart in its own owner cache without storing title or credential data', async () => {
  const first = accountController(); first.identify(); const pending=first.c.controller.recordLibraryGrab(3,'export');
  first.respond(0,null,503); await pending; const original=JSON.parse(first.requests[0].options.body);
  const stored=first.cache.get('bitagent-library-actions:v1:alice');
  assert.doesNotMatch(stored,/infoHash|torrent|magnet:|apiKey|secret/i);
  const next = accountController({cache:first.cache}); next.identify();
  assert.equal(next.c.accountTesting.queue.length,1); assert.equal(next.requests.length,0,'replay waits for a confirmed GET');
  const confirmed=next.c.controller.loadAccount(); next.respond(0,accountPayload()); await confirmed;
  assert.equal(next.requests.length,2); assert.deepEqual(JSON.parse(next.requests[1].options.body),original);
  next.respond(1,next.receipt(1,{receipt:{eventId:original.eventId,recorded:true,duplicate:true}}));
  await new Promise(resolve => setImmediate(resolve)); assert.equal(next.c.accountTesting.queue.length,0);
  assert.equal(next.nodes.get('acctGrabs').textContent,'3');
  const bob=accountController({cache:first.cache}); bob.identify('bob'); assert.equal(bob.c.accountTesting.queue.length,0);
});

test('delayed clipboard success stays with the account that initiated the copy', async () => {
  const f=accountController(); f.identify(); let finishCopy;
  f.c.navigator={clipboard:{writeText:()=>new Promise(resolve=>{finishCopy=resolve;})}};
  f.c.controller.copyMagnet('magnet:?xt=urn:btih:synthetic-fixture');
  assert.equal(f.requests.length,0,'clipboard intent alone is not a completed action');
  f.identify('bob'); finishCopy(); await new Promise(resolve=>setImmediate(resolve));
  assert.equal(f.requests.length,0,'the current account must not receive the old account action');
  assert.equal(f.c.accountTesting.queue.length,0); assert.equal(f.nodes.get('acctGrabs').textContent,'0');
  const stored=JSON.parse(f.cache.get('bitagent-library-actions:v1:alice'));
  assert.equal(stored.events.length,1);
  assert.deepEqual(Object.keys(stored.events[0]).sort(),['action','count','eventId','expectedAccountId']);
  assert.equal(stored.events[0].expectedAccountId,'alice'); assert.equal(stored.events[0].action,'copy');
  assert.equal(stored.events[0].count,1); assert.match(stored.events[0].eventId,/^[0-9a-f-]{36}$/i);
  assert.doesNotMatch(JSON.stringify(stored),/magnet:|synthetic-fixture|secret|apiKey/);
  assert.equal(f.nodes.get('acctName').textContent,'Bob Fixture');
});

test('an anonymous clipboard intent is not credited to an account that signs in before completion', async () => {
  const f=accountController(); let finishCopy;
  f.c.navigator={clipboard:{writeText:()=>new Promise(resolve=>{finishCopy=resolve;})}};
  f.c.controller.copyMagnet('magnet:?xt=urn:btih:synthetic-fixture');
  f.identify(); finishCopy(); await new Promise(resolve=>setImmediate(resolve));
  assert.equal(f.requests.length,0); assert.equal(f.c.accountTesting.queue.length,0);
  assert.equal(f.nodes.get('acctGrabs').textContent,'0');
  assert.equal(f.cache.has('bitagent-library-actions:v1:alice'),false);
});

test('clipboard completion after account verification fails waits for that same account to reconnect', async () => {
  const f=accountController(); f.identify(); let finishCopy;
  f.c.navigator={clipboard:{writeText:()=>new Promise(resolve=>{finishCopy=resolve;})}};
  f.c.controller.copyMagnet('magnet:?xt=urn:btih:synthetic-fixture');
  const verification=f.c.controller.loadAccount(); f.respond(0,null,401); await verification;
  finishCopy(); await new Promise(resolve=>setImmediate(resolve));
  assert.equal(f.c.accountTesting.connected,false); assert.equal(f.requests.length,1);
  assert.equal(f.c.accountTesting.queue.length,1); assert.equal(f.nodes.get('acctGrabs').textContent,'—');
  const stored=JSON.parse(f.cache.get('bitagent-library-actions:v1:alice')).events[0];
  const confirmed=f.c.controller.loadAccount(); f.respond(1,accountPayload()); await confirmed;
  assert.equal(f.requests.length,3); assert.deepEqual(JSON.parse(f.requests[2].options.body),stored);
  f.respond(2,f.receipt(2)); await new Promise(resolve=>setImmediate(resolve));
  assert.equal(f.c.accountTesting.queue.length,0); assert.equal(f.nodes.get('acctGrabs').textContent,'1');
});

test('invalid actions are rejected locally before they can block a later valid queued action', async () => {
  const f=accountController(); f.identify();
  for(const [count,action] of [[0,'copy'],[-1,'copy'],[1.5,'copy'],[true,'copy'],[1001,'copy'],[1,'download']])await f.c.controller.recordLibraryGrab(count,action);
  assert.equal(f.requests.length,0); assert.equal(f.c.accountTesting.queue.length,0);
  const valid=f.c.controller.recordLibraryGrab(1,'open'); f.respond(0,f.receipt(0)); await valid;
  assert.equal(f.nodes.get('acctOpens').textContent,'1');
});

test('API-key actions serialize and account GETs cannot overwrite a newly generated one-time key', async () => {
  const f=accountController(); f.identify('alice',{apiKey:{prefix:'fixture'}});
  const generated=f.c.controller.generateAccountKey(); await f.c.controller.revokeAccountKey(); await f.c.controller.loadAccount();
  assert.equal(f.requests.length,1); assert.equal(f.nodes.get('acctGenerateBtn').disabled,true); assert.equal(f.nodes.get('acctRevokeBtn').disabled,true);
  assert.equal(JSON.parse(f.requests[0].options.body).expectedAccountId,'alice');
  f.respond(0,accountPayload('alice',{apiKey:{prefix:'new-fixture'},apiKeySecret:'synthetic-new-key'})); await generated;
  assert.equal(f.nodes.get('acctApiSecret').value,'synthetic-new-key'); assert.equal(f.nodes.get('acctGenerateBtn').disabled,false);
  const revoked=f.c.controller.revokeAccountKey(); assert.match(f.requests[1].url,/expectedAccountId=alice/);
  f.respond(1,accountPayload()); await revoked;
  assert.equal(f.nodes.get('acctApiSecret').value,''); assert.equal(f.nodes.get('acctRevokeBtn').disabled,true);
});

test('late API-key mutations cannot expose the previous owner secret or disable the new account controls', async () => {
  const f=accountController(); f.identify(); const generated=f.c.controller.generateAccountKey();
  f.identify('bob'); f.respond(0,accountPayload('alice',{apiKey:{prefix:'old'},apiKeySecret:'synthetic-old-owner-key'})); await generated;
  assert.equal(f.nodes.get('acctName').textContent,'Bob Fixture'); assert.equal(f.nodes.get('acctApiSecret').value,'');
  assert.equal(f.nodes.get('acctGenerateBtn').disabled,false); assert.equal(f.nodes.get('acctRevokeBtn').disabled,true);
});

test('account request deadlines cover response JSON as well as initial headers', async () => {
  const f=accountController(); let signal;
  f.c.fetch=async(_url,options) => {signal=options.signal;return {ok:true,status:200,json:()=>new Promise((_resolve,reject)=>signal.addEventListener('abort',()=>reject(new Error('Synthetic slow body'))))};};
  const pending=f.c.controller.loadAccount(); await new Promise(resolve=>setImmediate(resolve));
  assert.equal(f.timers.size,1,'response headers must not clear the total request deadline');
  [...f.timers.values()][0].callback(); assert.equal(signal.aborted,true); await pending;
  assert.equal(f.timers.size,0); assert.equal(f.nodes.get('acctName').textContent,'Account unavailable');
});

test('appearance preferences affect the document, browser color and scroll behavior even when storage is unavailable', async () => {
  const blocked={getItem(){throw new Error('Synthetic storage blocked');},setItem(){throw new Error('Synthetic quota full');}};
  const f=accountController({storage:blocked});
  await f.c.controller.setAccountPreference('theme','light'); await f.c.controller.setAccountPreference('density','compact');
  await f.c.controller.setAccountPreference('motion','reduced');
  assert.equal(f.document.documentElement.getAttribute('data-theme'),'light'); assert.equal(f.themeMeta.getAttribute('content'),'#ffffff');
  assert.equal(f.document.documentElement.getAttribute('data-library-density'),'compact'); assert.equal(f.c.controller._scrollBehavior(),'auto');
  assert.match(f.nodes.get('acctPrefStatus').textContent,/visit|session|unavailable/i); assert.equal(f.requests.length,0);
  await f.c.controller.setAccountPreference('motion','system'); assert.equal(f.c.controller._scrollBehavior(),'smooth');
  f.media.get('(prefers-reduced-motion: reduce)').matches=true; assert.equal(f.c.controller._scrollBehavior(),'auto');
  await f.c.controller.resetAccountPreferences();
  assert.equal(f.document.documentElement.getAttribute('data-library-density'),'comfortable');
  assert.equal(f.document.documentElement.getAttribute('data-theme'),'dark'); assert.ok(f.events.includes('bitagent:preferences'));
});

test('period selectors display bounded UTC window counters and retain honest partial coverage', () => {
  const f=accountController(), counts={grabs:3,apiSearches:4,magnetCopies:1,magnetOpens:1,magnetExports:1};
  const period={...counts,days:7,complete:false,trackingSince:1790769600.5,windowStart:'2026-09-24T00:00:00Z',windowEnd:'2026-09-30T12:00:00Z'};
  f.identify('alice',{usage:measuredUsage('alice',{grabs:30,magnetCopies:10,magnetOpens:10,magnetExports:10,
    periods:{last7Days:period,last30Days:{...period,days:30,windowStart:'2026-09-01T00:00:00Z',grabs:6,magnetCopies:2,magnetOpens:2,magnetExports:2}}})});
  f.c.controller.setAccountUsagePeriod('7d'); assert.equal(f.nodes.get('acctGrabs').textContent,'3');
  assert.match(f.nodes.get('acctUsageStatus').textContent,/partial window/i);
  assert.equal(f.nodes.get('acctUsageTracked').textContent,'Sep 24 – Sep 30 · UTC calendar days');
  f.c.controller.setAccountUsagePeriod('30d'); assert.equal(f.nodes.get('acctGrabs').textContent,'6');
  f.c.controller.setAccountUsagePeriod('tracked'); assert.equal(f.nodes.get('acctGrabs').textContent,'30');
  f.c.controller.setAccountUsagePeriod('unsupported'); assert.equal(f.nodes.get('acctGrabs').textContent,'30');
});

test('malformed account identities are not adopted as authenticated users', async () => {
  for(const id of [7,true,[],{},'   ']) {
    const f=accountController(); f.identify(); const pending=f.c.controller.loadAccount();
    f.respond(0,accountPayload('alice',{identity:{id,method:'oidc',display:'Malformed Fixture'}})); await pending;
    assert.equal(f.nodes.get('acctName').textContent,'Account unavailable',JSON.stringify(id));
    assert.equal(f.c.accountTesting.state,null); assert.equal(f.nodes.get('acctApiSecret').value,'');
    assert.equal(f.c.accountTesting.connected,false);
  }
});

test('pending account preference acknowledgements cannot repaint the next owner appearance or controls', async () => {
  const f=accountController(); f.identify(); const pending=f.c.controller.setAccountPreference('theme','light');
  assert.equal(f.document.documentElement.getAttribute('data-theme'),'light');
  f.identify('bob',{preferences:{accountId:'bob',schemaVersion:1,revision:0,settings:preferences.normalizeLibraryPreferences({theme:'dark',density:'compact'})}});
  f.respond(0,{accountId:'alice',schemaVersion:1,revision:1,settings:preferences.normalizeLibraryPreferences({theme:'light'})}); await pending;
  assert.equal(f.nodes.get('acctName').textContent,'Bob Fixture'); assert.equal(f.document.documentElement.getAttribute('data-theme'),'dark');
  assert.equal(f.nodes.get('acctPrefTheme').value,'dark'); assert.equal(f.nodes.get('acctPrefDensity').value,'compact');
  assert.equal(JSON.parse(f.requests[0].options.body).expectedAccountId,'alice');
});

test('failed API-key mutations expose no remembered secret and never repeat the mutation automatically', async () => {
  for(const operation of ['generateAccountKey','revokeAccountKey']) {
    const f=accountController(); f.identify('alice',{apiKey:{prefix:'fixture'},apiKeySecret:'synthetic-previous-key'});
    const pending=f.c.controller[operation](); f.respond(0,null,503); await pending;
    assert.equal(f.nodes.get('acctApiSecret').value,''); assert.equal(f.nodes.get('acctName').textContent,'Account unavailable');
    assert.equal(f.nodes.get('acctGenerateBtn').disabled,true); assert.equal(f.requests.length,1);
    assert.match(f.toasts.at(-1),/failed/i);
  }
});

test('closing account settings removes a newly generated one-time key from its visible copy field', () => {
  const f=accountController(); f.identify('alice',{apiKey:{prefix:'fixture'},apiKeySecret:'synthetic-visible-key'});
  f.document.getElementById('libAccount').classList.add('open'); f.c.controller.closeAccount(true);
  assert.equal(f.nodes.get('acctApiSecret').value,''); assert.equal(f.nodes.get('acctSecretWrap').style.display,'none');
  assert.equal(f.nodes.get('libAccount').getAttribute('aria-hidden'),'true');
});


test('generic key generation stays public-only without an explicit private choice', async () => {
  const f=accountController();f.identify('alice',{privateKeyAvailable:true});
  assert.equal(f.nodes.get('acctPrivateAccess').checked,false);
  const pending=f.c.controller.generateAccountKey();
  assert.equal(JSON.parse(f.requests[0].options.body).privateAccess,false);
  f.respond(0,accountPayload('alice',{apiKey:{prefix:'fixture',privateAccess:false}}));await pending;
  assert.match(f.nodes.get('acctKeyStatus').textContent,/Public only/);
});

test('private key rotation requires explicit choice and a client-reconfiguration confirmation', async () => {
  const prompts=[];const f=accountController({confirm:message=>{prompts.push(message);return true;}});
  f.identify('alice',{privateKeyAvailable:true,apiKey:{prefix:'fixture',privateAccess:false}});
  f.nodes.get('acctPrivateAccess').checked=true;
  const pending=f.c.controller.generateAccountKey();
  assert.equal(prompts.length,1);assert.match(prompts[0],/revokes your existing key/);assert.match(prompts[0],/Update Prowlarr/);
  assert.deepEqual(JSON.parse(f.requests[0].options.body),{name:'default',expectedAccountId:'alice',privateAccess:true});
  f.respond(0,accountPayload('alice',{privateKeyAvailable:true,apiKey:{prefix:'next',privateAccess:true}}));await pending;
  assert.match(f.nodes.get('acctKeyStatus').textContent,/Public \+ private/);
});

test('declining key rotation creates no request or mutation', async () => {
  const f=accountController({confirm:()=>false});f.identify('alice',{privateKeyAvailable:true,apiKey:{prefix:'fixture'}});
  f.nodes.get('acctPrivateAccess').checked=true;
  await f.c.controller.generateAccountKey();assert.equal(f.requests.length,0);
  assert.equal(f.nodes.get('acctGenerateBtn').disabled,false);
});

test('an account change clears the previous private key choice', async () => {
  const f=accountController();f.identify('alice',{privateKeyAvailable:true,apiKey:{privateAccess:true}});
  assert.equal(f.nodes.get('acctPrivateAccess').checked,true);
  f.identify('bob',{privateKeyAvailable:false});
  assert.equal(f.nodes.get('acctPrivateAccess').checked,false);assert.equal(f.nodes.get('acctPrivateAccess').disabled,true);
  const pending=f.c.controller.generateAccountKey();assert.equal(JSON.parse(f.requests[0].options.body).privateAccess,false);
  f.respond(0,accountPayload('bob'));await pending;
});

test('failed account refresh clears and disables private scope choice', async () => {
  const f=accountController();f.identify('alice',{privateKeyAvailable:true,apiKey:{privateAccess:true}});
  const pending=f.c.controller.loadAccount();f.respond(0,null,401);await pending;
  assert.equal(f.nodes.get('acctPrivateAccess').checked,false);assert.equal(f.nodes.get('acctPrivateAccess').disabled,true);
});

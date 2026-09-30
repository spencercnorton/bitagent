'use strict';

const test = require('node:test');
const assert = require('node:assert/strict');
const vm = require('node:vm');
const fs = require('node:fs');
const path = require('node:path');

function controller() {
  const context = vm.createContext({ URLSearchParams, AbortController, setTimeout, clearTimeout });
  // Import with no DOM, then attach only the UI primitives required by each
  // interaction. Exercise the production functions, including async races.
  const source = fs.readFileSync(path.join(__dirname, '../static/js/library.js'), 'utf8');
  vm.runInContext(source + '\nthis.controller = { detail, groupCache, openDetail, maybeLoadSeasonMeta, fallbackCopy, magnetGroupToolbar, setMagnetMode, restoreLibraryHistory };', context);
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
  assert.match(c.controller.magnetGroupToolbar(), /disabled onclick="selectMagnetGroup\('season'\)"/);
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
  await new Promise(resolve => setImmediate(resolve));
  assert.equal(c.controller.detail.loading, false);
  assert.equal(c.controller.detail.releases.length, 1);
  assert.equal(c.controller.detail.meta, null);
  finishMetadata({ overview: 'Synthetic metadata' });
  await pending;
  assert.equal(c.controller.detail.meta.overview, 'Synthetic metadata');
});

'use strict';

const test = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const vm = require('node:vm');

function deferred() {
  let resolve;
  const promise = new Promise(done => { resolve = done; });
  return { promise, resolve };
}

function snapshot() {
  return { fields: {
    log_level: { sensitive: false, current: 'info', default: 'info', source: 'startup', overridden: false },
    torznab_api_key: { sensitive: true, configured: true, source: 'override', overridden: true },
    sonarr_base_url: { sensitive: false, current: 'http://demo.invalid:8989', default: '', source: 'override', overridden: true },
    tmdb_api_key: { sensitive: true, configured: true, source: 'override', overridden: true },
  } };
}

function decode(value) {
  return value.replaceAll('&quot;', '"').replaceAll('&#39;', "'").replaceAll('&lt;', '<').replaceAll('&gt;', '>').replaceAll('&amp;', '&');
}

function controller() {
  const elements = new Map();
  const focusListeners = new Set();
  const document = { activeElement: null, getElementById: id => elements.get(id) || null,
    addEventListener(type, listener) { if (type === 'focusin') focusListeners.add(listener); },
    removeEventListener(type, listener) { if (type === 'focusin') focusListeners.delete(listener); },
  };
  function element(id, attributes = {}) {
    const el = {
      id, attributes, dataset: {}, value: attributes.value || '', textContent: '',
      hidden: !!attributes.hidden, visible: true, isConnected: true,
      selectionStart: 0, selectionEnd: 0,
      setAttribute(name, value) { this.attributes[name] = value; },
      focus() { document.activeElement = this; for (const listener of focusListeners) listener({ target:this }); },
      getClientRects() { return this.visible ? [{}] : []; },
      setSelectionRange(start, end) { this.selectionStart = start; this.selectionEnd = end; },
    };
    let disabled = !!attributes.disabled;
    Object.defineProperty(el, 'disabled', { get() { return disabled; }, set(value) {
      disabled = value;
      if (value && document.activeElement === el) document.activeElement = document.body;
    } });
    elements.set(id, el);
    return el;
  }
  document.body = element('body'); document.documentElement = element('html');
  for (const id of ['settingsStatus', 'settingsAccessStatus', 'settingsReload', 'settingsAccessReload', 'settingsSearch', 'settingsSummary', 'settingsEmpty', 'tzKeyDisplay', 'tmdbStatus', 'dashKeyDisplay']) element(id);
  const markup = new Map();
  for (const gridId of ['settingsGrid', 'settingsAccessGrid']) {
    const grid = element(gridId), owned = new Set();
    grid.contains = el => !!el && owned.has(el.id);
    Object.defineProperty(grid, 'innerHTML', {
      get() { return markup.get(gridId) || ''; },
      set(value) {
        markup.set(gridId, value);
        for (const id of owned) {
          const old = elements.get(id); old.isConnected = false;
          if (document.activeElement === old) document.activeElement = document.body;
          elements.delete(id);
        }
        owned.clear();
        // Model generated controls while exercising actual async functions.
        for (const tag of value.matchAll(/<\w+\s+([^>]+)>/g)) {
          const attributes = {};
          for (const pair of tag[1].matchAll(/([\w-]+)="([^"]*)"/g)) attributes[pair[1]] = decode(pair[2]);
          if (!attributes.id) continue;
          attributes.hidden = /(?:^|\s)hidden(?:\s|$)/.test(tag[1]);
          const el = element(attributes.id, attributes);
          owned.add(attributes.id);
          if (attributes['data-sensitive']) el.dataset.sensitive = attributes['data-sensitive'];
        }
        for (const select of value.matchAll(/<select[^>]+id="([^"]+)"[^>]*>([\s\S]*?)<\/select>/g)) {
          const selected = select[2].match(/<option[^>]+value="([^"]*)"[^>]* selected/);
          if (selected) elements.get(select[1]).value = decode(selected[1]);
        }
      },
    });
  }
  const calls = [];
  const toasts = [];
  let data = snapshot();
  let handler = null;
  const api = async (url, opts = {}) => {
    calls.push({ url, opts });
    if (handler) return handler(url, opts);
    if (url === '/api/settings') return structuredClone(data);
    if (url === '/api/auth/tiers') return { apiKey: false };
    const key = url.split('/').at(-1);
    const field = data.fields[key];
    if (field && opts.method === 'PUT') {
      const value = JSON.parse(opts.body).value;
      data.fields[key] = { ...field, overridden: true, source: 'override', ...(field.sensitive ? { configured: true } : { current: value }) };
      return { key };
    }
    if (field && opts.method === 'DELETE') {
      data.fields[key] = { ...field, overridden: false, source: 'startup', ...(field.sensitive ? { configured: false } : { current: field.default }) };
      return { status: 'deleted' };
    }
    return null;
  };
  const context = vm.createContext({ document, api, Date, currentTab:'settings', _settingsTab:'config',
    escHtml: value => String(value ?? '').replaceAll('&', '&amp;').replaceAll('<', '&lt;').replaceAll('>', '&gt;').replaceAll('"', '&quot;').replaceAll("'", '&#39;'),
    toast: (...args) => toasts.push(args), loadAuditLog() {}, loadAuthStatus() {},
  });
  const source = fs.readFileSync(path.join(__dirname, '../static/js/app.js'), 'utf8');
  const start = source.indexOf('/* ── Settings ─');
  const end = source.indexOf('async function loadAuditLog()', start);
  const sharedStart = source.indexOf('/* ── Integration configuration ─');
  const sharedEnd = source.indexOf('function _arrName(', sharedStart);
  assert.ok(start >= 0 && end > start && sharedEnd > sharedStart);
  vm.runInContext(source.slice(sharedStart, sharedEnd) + source.slice(start, end) + '\nthis.settings = { loadSettings, saveSetting, resetSetting, cancelSettingReset, editSetting, filterSettings };', context);
  return {
    ...context.settings, get: id => elements.get(id), document, calls, toasts,
    get markup() { return [...markup.values()].join(''); },
    get data() { return data; }, set data(value) { data = value; },
    handle(value) { handler = value; },
    route(section, tab = 'settings') { context._settingsTab = section; context.currentTab = tab; },
    get focusListenerCount() { return focusListeners.size; },
    edit(key, value) { this.get(`setting-${key}`).value = value; this.editSetting(key); },
    writes() { return calls.filter(call => ['PUT', 'DELETE'].includes(call.opts.method)); },
  };
}

test('initial configuration failure is visible and a reload recovers labeled controls', async () => {
  const c = controller();
  c.handle(async () => null);
  await c.loadSettings();
  assert.equal(c.get('settingsStatus').dataset.state, 'error');
  assert.match(c.get('settingsStatus').textContent, /Reload/);
  assert.equal(c.get('settingsReload').disabled, false);
  assert.equal(c.get('settingsGrid').attributes['aria-busy'], 'false');
  c.handle(null);
  await c.loadSettings();
  assert.equal(c.get('settingsStatus').dataset.state, 'ready');
  assert.match(c.markup, /<label[^>]+for="setting-log_level"/);
  assert.equal(c.get('setting-log_level').attributes['aria-describedby'], 'setting-description-log_level setting-status-log_level');
  assert.equal(c.get('setting-save-log_level').disabled, true);
});

test('reloads retain drafts and edits made during the request across the two editor groups', async () => {
  const c = controller();
  await c.loadSettings();
  c.edit('log_level', 'debug');
  c.edit('torznab_api_key', 'synthetic-access-key');
  const input = c.get('setting-torznab_api_key');
  input.focus(); input.setSelectionRange(1, 3);
  c.get('settingsSearch').value = 'shared'; c.filterSettings();
  const refresh = deferred();
  c.handle(url => url === '/api/settings' ? refresh.promise : Promise.resolve({ apiKey: false }));
  const first = c.loadSettings();
  const second = c.loadSettings();
  assert.equal(c.calls.filter(call => call.url === '/api/settings').length, 2);
  c.edit('log_level', 'warning');
  refresh.resolve(snapshot());
  await Promise.all([first, second]);
  assert.equal(c.get('setting-log_level').value, 'warning');
  assert.equal(c.get('setting-torznab_api_key').value, 'synthetic-access-key');
  assert.equal(c.document.activeElement.id, 'setting-torznab_api_key');
  assert.equal(c.document.activeElement.selectionStart, 1);
  assert.equal(c.document.activeElement.selectionEnd, 3);
  assert.equal(c.get('setting-item-log_level').hidden, true);
  assert.match(c.get('settingsSummary').textContent, /2 unsaved changes/);
});

test('saving one field preserves other drafts made while pending and rejects repeated writes', async () => {
  const c = controller();
  await c.loadSettings();
  c.edit('log_level', 'debug');
  c.get('setting-save-log_level').focus();
  const write = deferred();
  c.handle((url, opts) => opts.method === 'PUT' ? write.promise : Promise.resolve(url === '/api/settings' ? c.data : { apiKey: false }));
  const pending = c.saveSetting('log_level');
  await c.saveSetting('log_level');
  assert.equal(c.writes().length, 1);
  assert.equal(c.get('setting-log_level').disabled, true);
  assert.equal(c.get('setting-save-log_level').disabled, true);
  c.edit('torznab_api_key', 'synthetic-access-key');
  c.data.fields.log_level.current = 'debug'; c.data.fields.log_level.overridden = true;
  write.resolve({ key: 'log_level' }); await pending;
  assert.equal(c.get('setting-torznab_api_key').value, 'synthetic-access-key');
  assert.equal(c.get('setting-log_level').value, 'debug');
  assert.equal(c.get('setting-save-log_level').disabled, true);
  assert.equal(c.get('setting-log_level').disabled, false);
  assert.match(c.get('setting-status-log_level').textContent, /Saved/);
  assert.equal(c.document.activeElement.id, 'setting-log_level');
});

test('failed secret saves retain the draft without rendering or echoing its value', async () => {
  const c = controller();
  await c.loadSettings();
  c.edit('torznab_api_key', 'synthetic-replacement');
  c.handle(async () => null);
  await c.saveSetting('torznab_api_key');
  assert.equal(c.get('setting-torznab_api_key').value, 'synthetic-replacement');
  assert.equal(c.get('setting-save-torznab_api_key').disabled, false);
  assert.equal(c.get('setting-status-torznab_api_key').dataset.state, 'error');
  assert.doesNotMatch(c.get('setting-status-torznab_api_key').textContent, /synthetic-replacement/);
  assert.doesNotMatch(c.markup, /synthetic-replacement/);
  assert.doesNotMatch(JSON.stringify(c.toasts), /synthetic-replacement/);
});

test('accepted secret saves clear immediately even when the follow-up read fails', async () => {
  const c = controller();
  await c.loadSettings();
  c.edit('torznab_api_key', 'synthetic-replacement');
  c.edit('log_level', 'debug');
  const refresh = deferred();
  const refreshStarted = deferred();
  c.handle((url, opts) => {
    if (opts.method === 'PUT') return Promise.resolve({ key: 'torznab_api_key' });
    if (url === '/api/settings') { refreshStarted.resolve(); return refresh.promise; }
    return Promise.resolve({ apiKey: false });
  });
  const pending = c.saveSetting('torznab_api_key');
  await refreshStarted.promise;
  assert.equal(c.get('setting-torznab_api_key').value, '');
  assert.equal(c.get('setting-log_level').value, 'debug');
  assert.doesNotMatch(c.markup, /synthetic-replacement/);
  refresh.resolve(null); await pending;
  assert.equal(c.get('settingsStatus').dataset.state, 'stale');
  assert.equal(c.get('setting-torznab_api_key').value, '');
  assert.equal(c.get('setting-log_level').value, 'debug');
});

test('reset requires confirmation, can be cancelled, and preserves unrelated drafts', async () => {
  const c = controller();
  await c.loadSettings();
  c.edit('log_level', 'debug');
  c.edit('torznab_api_key', 'synthetic-access-key');
  await c.resetSetting('torznab_api_key');
  assert.equal(c.writes().length, 0);
  assert.equal(c.get('setting-reset-torznab_api_key').textContent, 'Confirm reset');
  assert.equal(c.get('setting-cancel-torznab_api_key').hidden, false);
  c.cancelSettingReset('torznab_api_key');
  assert.equal(c.get('setting-cancel-torznab_api_key').hidden, true);
  assert.equal(c.document.activeElement.id, 'setting-reset-torznab_api_key');
  await c.resetSetting('torznab_api_key');
  await c.resetSetting('torznab_api_key');
  assert.equal(c.writes().length, 1);
  assert.equal(c.writes()[0].opts.method, 'DELETE');
  assert.equal(c.get('setting-torznab_api_key').value, '');
  assert.equal(c.get('setting-log_level').value, 'debug');
  assert.equal(c.get('setting-reset-torznab_api_key'), undefined);
});

test('editing a reset confirmation cancels it, and a failed reset retains its draft', async () => {
  const c = controller();
  await c.loadSettings();
  await c.resetSetting('torznab_api_key');
  c.edit('torznab_api_key', 'synthetic-access-key');
  await c.resetSetting('torznab_api_key');
  assert.equal(c.writes().length, 0);
  const write = deferred();
  c.handle(() => write.promise);
  const pending = c.resetSetting('torznab_api_key');
  await c.resetSetting('torznab_api_key');
  assert.equal(c.writes().length, 1);
  write.resolve(null); await pending;
  assert.equal(c.get('setting-torznab_api_key').value, 'synthetic-access-key');
  assert.equal(c.get('setting-status-torznab_api_key').dataset.state, 'error');
  assert.equal(c.get('setting-reset-torznab_api_key').textContent, 'Reset override');
});

test('malformed snapshots leave existing edits intact and explain that the view is stale', async () => {
  const c = controller();
  await c.loadSettings();
  c.edit('log_level', 'debug');
  c.handle(async () => ({ fields: { log_level: null } }));
  await c.loadSettings();
  assert.equal(c.get('setting-log_level').value, 'debug');
  assert.equal(c.get('settingsStatus').dataset.state, 'stale');
  assert.equal(c.get('setting-save-log_level').disabled, false);
});

test('search describes empty matches without deleting hidden drafts', async () => {
  const c = controller();
  await c.loadSettings();
  c.edit('torznab_api_key', 'synthetic-replacement');
  c.get('settingsSearch').value = 'no matching setting'; c.filterSettings();
  assert.equal(c.get('settingsEmpty').hidden, false);
  assert.match(c.get('settingsSummary').textContent, /0 of 2 settings · 1 unsaved change/);
  c.get('settingsSearch').value = 'shared indexer'; c.filterSettings();
  assert.equal(c.get('settingsEmpty').hidden, true);
  assert.equal(c.get('setting-item-torznab_api_key').hidden, false);
  assert.equal(c.get('setting-torznab_api_key').value, 'synthetic-replacement');
});

test('configuration reload reads only its own source', async () => {
  const c = controller();
  await c.loadSettings();
  assert.deepEqual(c.calls.map(call => call.url), ['/api/settings']);
});

test('general and security have appropriate controls without duplicate connection editors', async () => {
  const c = controller();
  await c.loadSettings();
  assert.match(c.get('settingsGrid').innerHTML, /<select[^>]+id="setting-log_level"/);
  for (const level of ['debug', 'info', 'warning', 'error', 'critical']) assert.match(c.markup, new RegExp(`<option value="${level}"`));
  assert.match(c.get('settingsAccessGrid').innerHTML, /type="password"/);
  assert.equal(c.get('setting-sonarr_base_url'), undefined);
  assert.equal(c.get('setting-tmdb_api_key'), undefined);
  assert.equal(c.get('setting-log_level').value, 'info');
});

test('unknown deployed logging values remain visible until the operator chooses a supported level', async () => {
  const c = controller();
  c.data.fields.log_level.current = 'trace';
  await c.loadSettings();
  assert.match(c.markup, /value="trace" selected disabled/);
  assert.equal(c.get('setting-log_level').value, 'trace');
  c.edit('log_level', 'verbose'); await c.saveSetting('log_level');
  assert.equal(c.writes().length, 0);
  assert.match(c.get('setting-status-log_level').textContent, /supported logging level/);
  c.edit('log_level', 'warning'); await c.saveSetting('log_level');
  assert.equal(c.writes().length, 1);
  assert.equal(JSON.parse(c.writes()[0].opts.body).value, 'warning');
});

test('a persisted warning level remains a supported selection without an unsaved draft', async () => {
  const c = controller();
  await c.loadSettings();
  c.edit('log_level', 'warning'); await c.saveSetting('log_level');
  assert.equal(JSON.parse(c.writes()[0].opts.body).value, 'warning');
  assert.equal(c.data.fields.log_level.current, 'warning');
  assert.match(c.markup, /<option value="warning" selected>Warning/);
  assert.doesNotMatch(c.markup, /Current value: warning/);
  assert.equal(c.get('setting-save-log_level').disabled, true);
  await c.loadSettings();
  assert.equal(c.get('setting-log_level').value, 'warning');
  assert.equal(c.get('setting-item-log_level').dataset.dirty, 'false');
});

test('legacy warn baselines display as warning without a phantom edit and support critical saves', async () => {
  const c = controller();
  c.data.fields.log_level.current = 'warn'; c.data.fields.log_level.default = 'warn';
  await c.loadSettings();
  assert.equal(c.get('setting-log_level').value, 'warning');
  assert.match(c.markup, /Startup value: <code>warning<\/code>/);
  assert.doesNotMatch(c.markup, /Current value: warn/);
  await c.loadSettings();
  c.edit('log_level', 'warning'); await c.saveSetting('log_level');
  assert.equal(c.writes().length, 0);
  assert.equal(c.get('setting-item-log_level').dataset.dirty, 'false');
  assert.equal(c.get('setting-save-log_level').disabled, true);
  c.edit('log_level', 'critical'); await c.saveSetting('log_level');
  assert.equal(c.writes().length, 1);
  assert.equal(JSON.parse(c.writes()[0].opts.body).value, 'critical');
  assert.equal(c.get('setting-log_level').value, 'critical');
  assert.equal(c.get('setting-save-log_level').disabled, true);
});

test('completed and failed settings saves restore focus lost by disabling the initiating button', async () => {
  for (const accepted of [true, false]) {
    const c = controller(); await c.loadSettings(); c.edit('log_level', 'debug');
    const write = deferred();
    c.handle((url, opts) => opts.method === 'PUT' ? write.promise : Promise.resolve(c.data));
    c.get('setting-save-log_level').focus();
    const pending = c.saveSetting('log_level');
    assert.equal(c.document.activeElement, c.document.body);
    if (accepted) c.data.fields.log_level.current = 'debug';
    write.resolve(accepted ? { key:'log_level' } : null); await pending;
    assert.equal(c.document.activeElement.id, 'setting-log_level');
    assert.equal(c.focusListenerCount, 0);
  }
});

test('pending settings saves never reclaim focus after user movement or navigation', async () => {
  for (const scenario of ['other-control', 'moved-then-body', 'other-route', 'hidden-input']) {
    const c = controller(); await c.loadSettings(); c.edit('log_level', 'debug');
    const write = deferred(); c.handle(() => write.promise);
    c.get('setting-save-log_level').focus(); const pending = c.saveSetting('log_level');
    if (['other-control', 'moved-then-body'].includes(scenario)) {
      c.get('settingsAccessReload').focus();
      if (scenario === 'moved-then-body') c.document.body.focus();
    } else if (scenario === 'other-route') c.route('integrations');
    else c.get('setting-log_level').visible = false;
    const focusBefore = c.document.activeElement;
    write.resolve(null); await pending;
    assert.equal(c.document.activeElement, focusBefore, scenario);
    assert.equal(c.focusListenerCount, 0);
  }
});

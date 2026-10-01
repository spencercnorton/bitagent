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
  const fields = {};
  for (const arr of ['sonarr', 'radarr', 'lidarr']) {
    fields[`${arr}_base_url`] = { sensitive: false, current: `http://${arr}.invalid:8000`, default: '', overridden: true, source: 'override' };
    fields[`${arr}_api_key`] = { sensitive: true, configured: arr !== 'lidarr', overridden: true, source: 'override' };
  }
  fields.tmdb_api_key = { sensitive: true, configured: true, overridden: true, source: 'override' };
  return { fields };
}

function controller() {
  const elements = new Map();
  const focusListeners = new Set();
  const document = { activeElement: null, getElementById: id => elements.get(id) || null,
    addEventListener(type, listener) { if (type === 'focusin') focusListeners.add(listener); },
    removeEventListener(type, listener) { if (type === 'focusin') focusListeners.delete(listener); },
  };
  function element(id) {
    const el = { id, value: '', placeholder: '', textContent: '', dataset: {}, attributes: {}, readOnly: false, visible:true, isConnected:true,
      selectionStart: 0, selectionEnd: 0,
      setAttribute(name, value) { this.attributes[name] = value; },
      focus() { document.activeElement = this; for (const listener of focusListeners) listener({ target:this }); },
      getClientRects() { return this.visible ? [{}] : []; },
      setSelectionRange(start, end) { this.selectionStart = start; this.selectionEnd = end; },
    };
    let disabled = false;
    Object.defineProperty(el, 'disabled', { get() { return disabled; }, set(value) {
      disabled = value;
      if (value && document.activeElement === el) document.activeElement = document.body;
    } });
    elements.set(id, el);
    return el;
  }
  document.body = element('body'); document.documentElement = element('html');
  for (const id of ['integrationsStatus', 'arrSettingsGrid', 'tmdbStatus', 'tmdbKeyInput', 'tmdb-save', 'tmdb-save-status', 'setting-tmdb_api_key']) element(id);
  for (const arr of ['sonarr', 'radarr', 'lidarr']) {
    for (const field of ['url', 'key', 'status', 'state']) element(`arr-${arr}-${field}`);
    element(`arr-save-${arr}`);
  }
  const calls = [], toasts = [];
  let data = snapshot(), handler = null;
  const api = async (url, opts = {}) => {
    calls.push({ url, opts });
    if (handler) return handler(url, opts);
    if (url === '/api/settings') return structuredClone(data);
    const key = url.split('/').at(-1);
    if (opts.method === 'PUT' && data.fields[key]) {
      const value = JSON.parse(opts.body).value;
      if (data.fields[key].sensitive) data.fields[key].configured = !!value;
      else data.fields[key].current = value;
      return { key };
    }
    if (opts.method === 'DELETE' && data.fields[key]) {
      data.fields[key].configured = false;
      data.fields[key].overridden = false;
      return { status: 'deleted' };
    }
    return null;
  };
  const context = vm.createContext({ document, api, Date, URL, currentTab:'settings', _settingsTab:'integrations', toast: (...args) => toasts.push(args) });
  const source = fs.readFileSync(path.join(__dirname, '../static/js/app.js'), 'utf8');
  const start = source.indexOf('/* ── Integration configuration ─');
  const end = source.indexOf('/* ── AI (LLM cost', start);
  const settingsStart = source.indexOf('/* ── Settings ─');
  const settingsEnd = source.indexOf('async function loadAuditLog()', settingsStart);
  assert.ok(start >= 0 && end > start && settingsEnd > settingsStart);
  vm.runInContext(source.slice(settingsStart, settingsEnd) + source.slice(start, end) + '\nthis.integration = { loadArrSettings, saveArrSettings, editArrSetting, saveTmdbKey, loadSettings, saveSetting, resetSetting, editSetting };', context);
  return { ...context.integration, calls, toasts, document,
    get: id => elements.get(id),
    get data() { return data; },
    handle(value) { handler = value; },
    route(section, tab = 'settings') { context._settingsTab = section; context.currentTab = tab; },
    get focusListenerCount() { return focusListeners.size; },
    edit(arr, field, value) { this.get(`arr-${arr}-${field}`).value = value; this.editArrSetting(arr, field); },
    writes() { return calls.filter(call => call.opts.method === 'PUT'); },
  };
}

test('initial integration reads retain earlier and in-flight drafts and share one request', async () => {
  const c = controller();
  c.edit('sonarr', 'url', 'http://draft.invalid:8989');
  c.edit('sonarr', 'key', 'synthetic-initial-key');
  const read = deferred();
  c.handle(() => read.promise);
  const first = c.loadArrSettings(), second = c.loadArrSettings();
  assert.equal(c.calls.length, 1);
  assert.equal(c.get('arr-save-sonarr').disabled, true);
  assert.equal(c.get('arr-sonarr-url').readOnly, false);
  c.edit('radarr', 'url', 'http://other-draft.invalid:7878');
  const data = snapshot();
  data.fields.sonarr_api_key.current = 'server-secret-never-exposed';
  read.resolve(data); await Promise.all([first, second]);
  assert.equal(c.get('arr-sonarr-url').value, 'http://draft.invalid:8989');
  assert.equal(c.get('arr-sonarr-key').value, 'synthetic-initial-key');
  assert.equal(c.get('arr-radarr-url').value, 'http://other-draft.invalid:7878');
  assert.equal(c.get('arr-lidarr-key').value, '');
  assert.match(c.get('arr-sonarr-key').placeholder, /configured/);
  assert.doesNotMatch(JSON.stringify([...['sonarr', 'radarr', 'lidarr'].map(arr => c.get(`arr-${arr}-key`).placeholder)]), /server-secret/);
  assert.equal(c.get('tmdbStatus').textContent, 'Configured');
});

test('integration saves restore dropped button focus to the edited field on success and failure', async () => {
  for (const [field, accepted] of [['url',true], ['url',false], ['key',true], ['key',false]]) {
    const c = controller(); await c.loadArrSettings();
    c.edit('sonarr', field, field === 'url' ? 'https://updated.invalid:8989' : 'synthetic-replacement');
    const write = deferred();
    c.handle((url, opts) => opts.method === 'PUT' ? write.promise : Promise.resolve(c.data));
    c.get('arr-save-sonarr').focus(); const pending = c.saveArrSettings('sonarr');
    assert.equal(c.document.activeElement, c.document.body);
    if (accepted && field === 'url') c.data.fields.sonarr_base_url.current = 'https://updated.invalid:8989';
    write.resolve(accepted ? { key:`sonarr_${field === 'url' ? 'base_url' : 'api_key'}` } : null); await pending;
    assert.equal(c.document.activeElement.id, `arr-sonarr-${field}`);
    assert.equal(c.focusListenerCount, 0);
  }
});

test('integration save completions leave user focus and hidden or different routes alone', async () => {
  for (const scenario of ['other-control', 'moved-then-body', 'other-route', 'other-tab', 'hidden-input']) {
    const c = controller(); await c.loadArrSettings(); c.edit('sonarr', 'url', 'https://updated.invalid:8989');
    const write = deferred(); c.handle(() => write.promise);
    c.get('arr-save-sonarr').focus(); const pending = c.saveArrSettings('sonarr');
    if (['other-control', 'moved-then-body'].includes(scenario)) {
      c.get('arr-radarr-url').focus();
      if (scenario === 'moved-then-body') c.document.documentElement.focus();
    } else if (scenario === 'other-route') c.route('config');
    else if (scenario === 'other-tab') c.route('integrations', 'dashboard');
    else c.get('arr-sonarr-url').visible = false;
    const focusBefore = c.document.activeElement;
    write.resolve(null); await pending;
    assert.equal(c.document.activeElement, focusBefore, scenario);
    assert.equal(c.focusListenerCount, 0);
  }
});

test('TMDB saves restore dropped focus but never reclaim it after a user moves', async () => {
  for (const moved of [false,true]) {
    const c = controller(); c.get('tmdbKeyInput').value = 'synthetic-replacement';
    const write = deferred(); c.handle(() => write.promise);
    c.get('tmdb-save').focus(); const pending = c.saveTmdbKey();
    assert.equal(c.document.activeElement, c.document.body);
    if (moved) c.get('arr-radarr-url').focus();
    write.resolve({ key:'tmdb_api_key' }); await pending;
    assert.equal(c.document.activeElement.id, moved ? 'arr-radarr-url' : 'tmdbKeyInput');
    assert.equal(c.focusListenerCount, 0);
  }
});

test('an explicit URL clear during the initial read is retained and saves an empty URL only', async () => {
  const c = controller();
  const read = deferred();
  c.handle(() => read.promise);
  const pending = c.loadArrSettings();
  c.edit('sonarr', 'url', '');
  read.resolve(snapshot()); await pending;
  assert.equal(c.get('arr-sonarr-url').value, '');
  c.handle(null);
  await c.saveArrSettings('sonarr');
  assert.equal(c.writes().length, 1);
  assert.match(c.writes()[0].url, /sonarr_base_url$/);
  assert.equal(JSON.parse(c.writes()[0].opts.body).value, '');
});

test('refresh and malformed reads preserve integration drafts and keyboard selection', async () => {
  const c = controller();
  await c.loadArrSettings();
  c.edit('radarr', 'url', 'http://draft.invalid:7878');
  c.edit('radarr', 'key', 'synthetic-replacement');
  const input = c.get('arr-radarr-url'); input.focus(); input.setSelectionRange(5, 10);
  await c.loadArrSettings();
  assert.equal(c.get('arr-radarr-url'), input);
  assert.equal(c.document.activeElement, input);
  assert.equal(input.selectionStart, 5);
  assert.equal(c.get('arr-radarr-key').value, 'synthetic-replacement');
  c.handle(async () => ({ fields: {} }));
  await c.loadArrSettings();
  assert.equal(c.get('integrationsStatus').dataset.state, 'stale');
  assert.equal(c.get('arr-radarr-url').value, 'http://draft.invalid:7878');
  assert.equal(c.get('arr-radarr-key').value, 'synthetic-replacement');
  assert.equal(c.get('arr-save-radarr').disabled, false);
});

test('URL success and key failure have separate outcomes and preserve failed and other-app drafts', async () => {
  const c = controller();
  await c.loadArrSettings();
  c.edit('sonarr', 'url', 'http://saved.invalid:8989');
  c.edit('sonarr', 'key', 'synthetic-replacement');
  const url = deferred(), key = deferred();
  c.handle((path, opts) => opts.method === 'PUT' ? (path.endsWith('_base_url') ? url.promise : key.promise) : Promise.resolve(c.data));
  const pending = c.saveArrSettings('sonarr');
  await c.saveArrSettings('sonarr');
  assert.equal(c.writes().length, 2);
  assert.equal(c.get('arr-sonarr-url').readOnly, true);
  assert.equal(c.get('arr-sonarr-key').readOnly, true);
  c.edit('radarr', 'url', 'http://other-draft.invalid:7878');
  c.data.fields.sonarr_base_url.current = 'http://saved.invalid:8989';
  url.resolve({ key: 'sonarr_base_url' }); key.resolve(null); await pending;
  assert.equal(c.get('arr-sonarr-key').value, 'synthetic-replacement');
  assert.equal(c.get('arr-radarr-url').value, 'http://other-draft.invalid:7878');
  assert.match(c.get('arr-sonarr-status').textContent, /Base URL saved.*API key save could not be confirmed/);
  assert.equal(c.get('arr-sonarr-status').dataset.state, 'error');
  assert.equal(c.get('arr-sonarr-url').readOnly, false);
  assert.equal(c.get('arr-sonarr-key').readOnly, false);
  assert.equal(c.get('arr-save-sonarr').disabled, false);
  assert.doesNotMatch(c.get('arr-sonarr-status').textContent, /synthetic-replacement/);
});

test('key success clears immediately before an outstanding URL request and a failed follow-up read', async () => {
  const c = controller();
  await c.loadArrSettings();
  c.edit('sonarr', 'url', 'http://draft.invalid:8989');
  c.edit('sonarr', 'key', 'synthetic-replacement');
  const url = deferred(), key = deferred(), cleared = deferred();
  const keyInput = c.get('arr-sonarr-key'); let value = keyInput.value;
  Object.defineProperty(keyInput, 'value', { get() { return value; }, set(next) { value = next; if (next === '') cleared.resolve(); } });
  c.handle((path, opts) => opts.method === 'PUT' ? (path.endsWith('_base_url') ? url.promise : key.promise) : Promise.resolve(null));
  const pending = c.saveArrSettings('sonarr');
  key.resolve({ key: 'sonarr_api_key' }); await cleared.promise;
  assert.equal(keyInput.value, '');
  assert.equal(keyInput.readOnly, true);
  assert.match(keyInput.placeholder, /configured/);
  assert.equal(c.calls.filter(call => call.url === '/api/settings').length, 1);
  url.resolve(null); await pending;
  assert.equal(c.get('integrationsStatus').dataset.state, 'stale');
  assert.equal(keyInput.value, '');
  assert.equal(c.get('arr-sonarr-url').value, 'http://draft.invalid:8989');
  assert.match(c.get('arr-sonarr-status').textContent, /Base URL save could not be confirmed.*API key saved/);
  c.handle(null);
  await c.saveArrSettings('sonarr');
  assert.equal(c.writes().filter(call => call.url.endsWith('sonarr_api_key')).length, 1);
  assert.equal(c.writes().filter(call => call.url.endsWith('sonarr_base_url')).length, 2);
});

test('unchanged URLs and blank keys produce no writes while a key replacement writes only the key', async () => {
  const c = controller();
  await c.loadArrSettings();
  await c.saveArrSettings('sonarr');
  assert.equal(c.writes().length, 0);
  c.edit('sonarr', 'key', 'synthetic-replacement');
  await c.saveArrSettings('sonarr');
  assert.equal(c.writes().length, 1);
  assert.match(c.writes()[0].url, /sonarr_api_key$/);
  assert.equal(c.get('arr-sonarr-key').value, '');
  assert.equal(c.get('arr-save-sonarr').disabled, true);
});

test('a key save after an initial read failure never invents a blank URL change', async () => {
  const c = controller();
  c.handle(async () => null);
  await c.loadArrSettings();
  assert.equal(c.get('integrationsStatus').dataset.state, 'error');
  c.edit('sonarr', 'key', 'synthetic-replacement');
  c.handle(null);
  await c.saveArrSettings('sonarr');
  assert.equal(c.writes().length, 1);
  assert.match(c.writes()[0].url, /sonarr_api_key$/);
  assert.equal(c.get('arr-sonarr-url').value, 'http://sonarr.invalid:8000');
});

test('parallel app saves remain independently readonly and refresh once after all writes finish', async () => {
  const c = controller();
  await c.loadArrSettings();
  c.edit('sonarr', 'key', 'synthetic-sonarr-key');
  c.edit('radarr', 'key', 'synthetic-radarr-key');
  const sonarr = deferred(), radarr = deferred();
  c.handle((path, opts) => opts.method === 'PUT' ? (path.includes('sonarr') ? sonarr.promise : radarr.promise) : Promise.resolve(c.data));
  const first = c.saveArrSettings('sonarr'), second = c.saveArrSettings('radarr');
  sonarr.resolve({ key: 'sonarr_api_key' }); await first;
  assert.equal(c.get('arr-sonarr-key').readOnly, false);
  assert.equal(c.get('arr-radarr-key').readOnly, true);
  assert.equal(c.get('arrSettingsGrid').attributes['aria-busy'], 'true');
  assert.equal(c.calls.filter(call => call.url === '/api/settings').length, 1);
  radarr.resolve({ key: 'radarr_api_key' }); await second;
  assert.equal(c.get('arr-radarr-key').readOnly, false);
  assert.equal(c.calls.filter(call => call.url === '/api/settings').length, 2);
  assert.equal(c.get('arrSettingsGrid').attributes['aria-busy'], 'false');
});

test('TMDB replacement prevents duplicate writes, clears success, and fences an older settings read', async () => {
  const c = controller();
  c.data.fields.tmdb_api_key.configured = false;
  await c.loadArrSettings();
  const read = deferred(), write = deferred();
  c.handle((path, opts) => opts.method === 'PUT' ? write.promise : read.promise);
  const loading = c.loadArrSettings();
  c.get('tmdbKeyInput').value = 'synthetic-tmdb-key';
  const saving = c.saveTmdbKey();
  await c.saveTmdbKey();
  assert.equal(c.writes().length, 1);
  assert.equal(c.get('tmdbKeyInput').readOnly, true);
  assert.equal(c.get('tmdb-save').disabled, true);
  write.resolve({ key: 'tmdb_api_key' }); await saving;
  assert.equal(c.get('tmdbKeyInput').value, '');
  assert.equal(c.get('tmdbStatus').textContent, 'Configured');
  read.resolve(c.data); await loading;
  assert.equal(c.get('tmdbStatus').textContent, 'Configured');
  assert.equal(c.get('tmdbKeyInput').readOnly, false);
  assert.equal(c.get('tmdb-save').disabled, false);
  assert.equal(c.get('tmdb-save-status').dataset.state, 'success');
});

test('failed TMDB replacement preserves its draft and reports a generic failure', async () => {
  const c = controller();
  c.get('tmdbKeyInput').value = 'synthetic-tmdb-key';
  c.handle(async () => null);
  await c.saveTmdbKey();
  assert.equal(c.get('tmdbKeyInput').value, 'synthetic-tmdb-key');
  assert.equal(c.get('tmdbKeyInput').readOnly, false);
  assert.equal(c.get('tmdb-save').disabled, false);
  assert.equal(c.get('tmdb-save-status').dataset.state, 'error');
  assert.doesNotMatch(c.get('tmdb-save-status').textContent, /synthetic-tmdb-key/);
  assert.doesNotMatch(JSON.stringify(c.toasts), /synthetic-tmdb-key/);
});

test('a delayed generic configuration read cannot repaint an accepted integration TMDB save', async () => {
  const c = controller();
  c.data.fields.tmdb_api_key.configured = false;
  const old = structuredClone(c.data), read = deferred();
  c.handle((path, opts) => opts.method === 'PUT' ? Promise.resolve({ key: 'tmdb_api_key' }) : read.promise);
  const loading = c.loadSettings();
  c.get('tmdbKeyInput').value = 'synthetic-tmdb-key';
  await c.saveTmdbKey();
  read.resolve(old); await loading;
  assert.equal(c.get('tmdbStatus').textContent, 'Configured');
  assert.equal(c.get('tmdbKeyInput').value, '');
});

test('a generic TMDB save invalidates an older integration read and publishes its accepted state', async () => {
  const c = controller();
  c.data.fields.tmdb_api_key.configured = false;
  await c.loadSettings(); await c.loadArrSettings();
  const old = structuredClone(c.data), read = deferred(); let reads = 0;
  c.handle((path, opts) => {
    if (opts.method === 'PUT') { c.data.fields.tmdb_api_key.configured = true; return Promise.resolve({ key: 'tmdb_api_key' }); }
    return ++reads === 1 ? read.promise : Promise.resolve(c.data);
  });
  const loading = c.loadArrSettings();
  c.get('setting-tmdb_api_key').value = 'synthetic-generic-key'; c.editSetting('tmdb_api_key');
  await c.saveSetting('tmdb_api_key');
  read.resolve(old); await loading;
  assert.equal(c.get('tmdbStatus').textContent, 'Configured');
  assert.equal(c.get('setting-tmdb_api_key').value, '');
});

test('a generic TMDB reset invalidates an older integration read and confirms the startup state', async () => {
  const c = controller();
  await c.loadSettings(); await c.loadArrSettings();
  const old = structuredClone(c.data), read = deferred(); let reads = 0;
  c.handle((path, opts) => {
    if (opts.method === 'DELETE') { c.data.fields.tmdb_api_key.configured = false; c.data.fields.tmdb_api_key.overridden = false; return Promise.resolve({ status: 'deleted' }); }
    return ++reads === 1 ? read.promise : Promise.resolve(c.data);
  });
  const loading = c.loadArrSettings();
  await c.resetSetting('tmdb_api_key'); await c.resetSetting('tmdb_api_key');
  read.resolve(old); await loading;
  assert.equal(c.get('tmdbStatus').textContent, 'Not configured');
});

test('a delayed post-reset generic read cannot overwrite a newer accepted integration key', async () => {
  const c = controller();
  await c.loadSettings(); await c.loadArrSettings();
  const read = deferred(), started = deferred();
  c.handle((path, opts) => {
    if (opts.method === 'DELETE') { c.data.fields.tmdb_api_key.configured = false; c.data.fields.tmdb_api_key.overridden = false; return Promise.resolve({ status: 'deleted' }); }
    if (opts.method === 'PUT') return Promise.resolve({ key: 'tmdb_api_key' });
    started.resolve(); return read.promise;
  });
  await c.resetSetting('tmdb_api_key');
  const resetting = c.resetSetting('tmdb_api_key');
  await started.promise;
  assert.equal(c.get('tmdbStatus').textContent, 'Unavailable');
  c.get('tmdbKeyInput').value = 'synthetic-newer-key'; await c.saveTmdbKey();
  read.resolve(c.data); await resetting;
  assert.equal(c.get('tmdbStatus').textContent, 'Configured');
});

test('connection cards distinguish stored configuration, setup gaps, and stale reads', async () => {
  const c = controller();
  await c.loadArrSettings();
  assert.equal(c.get('arr-sonarr-state').textContent, 'Configured');
  assert.equal(c.get('arr-lidarr-state').textContent, 'Needs setup');
  c.handle(async () => null); await c.loadArrSettings();
  assert.equal(c.get('arr-sonarr-state').textContent, 'Stale');
  const initial = controller(); initial.handle(async () => null); await initial.loadArrSettings();
  assert.equal(initial.get('arr-sonarr-state').textContent, 'Unavailable');
});

test('invalid or unsupported URL schemes fail visibly without writing either field or losing a key draft', async () => {
  for (const invalid of ['ftp://demo.invalid', 'sonarr:8989', '/relative/path', 'https://']) {
    const c = controller(); await c.loadArrSettings();
    c.edit('sonarr', 'url', invalid); c.edit('sonarr', 'key', 'synthetic-replacement');
    await c.saveArrSettings('sonarr');
    assert.equal(c.writes().length, 0);
    assert.equal(c.get('arr-sonarr-url').value, invalid);
    assert.equal(c.get('arr-sonarr-key').value, 'synthetic-replacement');
    assert.match(c.get('arr-sonarr-status').textContent, /http:\/\/ or https:\/\//);
    assert.equal(c.get('arr-sonarr-url').readOnly, false);
    assert.equal(c.document.activeElement.id, 'arr-sonarr-url');
  }
});

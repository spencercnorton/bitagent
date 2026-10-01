'use strict';
const test = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const vm = require('node:vm');

function fixture(mobile = false) {
  const nodes = new Map();
  const focused = [];
  const element = id => {
    if (!nodes.has(id)) {
      const attrs = new Map(), classes = new Set();
      nodes.set(id, { id, attrs, classes, dataset: {}, inert: false, isConnected: true,
        classList: { contains: c => classes.has(c), toggle: (c, on) => on ? classes.add(c) : classes.delete(c) },
        setAttribute: (k, v) => attrs.set(k, v), removeAttribute: k => attrs.delete(k),
        focus: () => focused.push(id), querySelector: () => element('activeNav') });
    }
    return nodes.get(id);
  };
  const nav = ['dashboard','library','wants','evidence','quarantine','ai','settings','system'].map(tab => { const n = element(tab); n.dataset.tab = tab; return n; });
  const panels = nav.map(n => element(`tab-${n.id}`));
  const origin = element('navigationToggle');
  const document = { addEventListener:()=>{}, activeElement: origin, body: element('body'), getElementById: element,
    querySelectorAll: selector => selector.includes('nav-item') ? nav : panels };
  const history = [], loaded = [];
  const window = { location: { hash: '' }, matchMedia: () => ({ matches: mobile }), history: {
    pushState: (_, __, hash) => { history.push(['push', hash]); window.location.hash = hash; },
    replaceState: (_, __, hash) => { history.push(['replace', hash]); window.location.hash = hash; }
  }};
  const context = vm.createContext({ document, window, ...Object.fromEntries(['Dashboard','Library','Wants','Evidence','Quarantine','AiTab','Settings','SettingsView','System'].map(name => [`load${name}`, () => loaded.push(name)])),
    switchSettingsTab: () => {}, switchSystemTab: () => {} });
  const source = fs.readFileSync(path.join(__dirname, '../static/js/app.js'), 'utf8');
  vm.runInContext('let currentTab = "dashboard";\n' + source.slice(source.indexOf('/* ── Sidebar drawer'), source.indexOf('/* ── Utility')) + '\nthis.nav = {operatorRoute,switchTab,setSidebarOpen,restoreOperatorRoute};', context);
  return { ...context.nav, document, window, element, focused, history, loaded };
}

test('operator routes validate exact tab and sub-section allowlists', () => {
  const f = fixture();
  assert.deepEqual(JSON.parse(JSON.stringify(f.operatorRoute('#settings'))), {tab:'settings',section:'config'});
  assert.equal(f.operatorRoute('#settings/audit').section, 'audit');
  assert.equal(f.operatorRoute('#system/metrics').section, 'metrics');
  for (const hash of ['#constructor', '#__proto__', '#settings/private', '#library/auth', '#dashboard/anything', '#settings/auth/extra']) assert.equal(f.operatorRoute(hash), null);
});

test('navigation updates native-link current state, URL, title and focus', () => {
  const f = fixture();
  f.switchTab('settings');
  assert.deepEqual(f.history, [['push','#settings/config']]);
  assert.equal(f.element('settings').attrs.get('aria-current'), 'page');
  assert.equal(f.element('dashboard').attrs.has('aria-current'), false);
  assert.equal(f.document.title, 'Settings · BitAgent Console');
  assert.deepEqual(f.focused, ['pageTitle']);
  assert.deepEqual(f.loaded, ['SettingsView']);
  f.switchTab('private');
  assert.deepEqual(f.loaded, ['SettingsView']);
});

test('history restoration reloads a supported route without adding history', () => {
  const f = fixture(); f.window.location.hash = '#system/metrics';
  f.restoreOperatorRoute();
  assert.deepEqual(f.history, []);
  assert.deepEqual(f.loaded, ['System']);
});

test('mobile drawer fences background and closed navigation, and restores focus', () => {
  const f = fixture(true);
  f.setSidebarOpen(false);
  assert.equal(f.element('sidebar').inert, true);
  f.setSidebarOpen(true);
  assert.equal(f.element('sidebar').inert, false);
  assert.equal(f.element('mainContent').inert, true);
  assert.equal(f.element('navigationToggle').attrs.get('aria-expanded'), 'true');
  assert.equal(f.element('sidebar').attrs.get('aria-modal'), 'true');
  f.setSidebarOpen(false);
  assert.equal(f.element('mainContent').inert, false);
  assert.deepEqual(f.focused, ['activeNav','navigationToggle']);
});

test('desktop navigation remains available and never inerts the main content', () => {
  const f = fixture(false); f.setSidebarOpen(true);
  assert.equal(f.element('sidebar').inert, false);
  assert.equal(f.element('mainContent').inert, false);
  assert.equal(f.element('sidebar').attrs.has('aria-modal'), false);
});


test('restoring actual system and settings subtab functions loads each selected source once', async () => {
  const source = fs.readFileSync(path.join(__dirname, '../static/js/app.js'), 'utf8');
  const f = fixture();
  let metrics = 0, audit = 0, config = 0;
  const context = vm.createContext({ document:f.document, window:f.window,
    loadDashboard:()=>{},loadLibrary:()=>{},loadWants:()=>{},loadEvidence:()=>{},loadQuarantine:()=>{},loadAiTab:()=>{},
    loadSettings:()=>{config++;},loadAuditLog:()=>{audit++;},loadRawMetrics:()=>{metrics++;},_loadSystemStatus:()=>{},
    loadAuthStatus:()=>{},loadArrSettings:()=>{},loadClassifierRules:()=>{},loadLivenessOps:()=>{},loadFiltersStatus:()=>{},loadBlockLists:()=>{} });
  const nav = source.slice(source.indexOf('/* ── Sidebar drawer'), source.indexOf('/* ── Utility'));
  const settings = source.slice(source.indexOf('function loadSettingsView()'), source.indexOf('async function loadFiltersStatus()'));
  const system = source.slice(source.indexOf('function loadSystem()'), source.indexOf('// Health-check + network cards'));
  const systemTab = source.slice(source.indexOf('function switchSystemTab('), source.indexOf('async function runHealthChecks()'));
  vm.runInContext('let currentTab="dashboard";\n' + nav + settings + system + systemTab + '\nthis.restore=restoreOperatorRoute;this.refresh=refreshCurrentTab;',context);
  f.window.location.hash='#system/metrics'; context.restore();
  assert.equal(metrics,1);
  f.window.location.hash='#settings/audit'; context.restore();
  assert.equal(audit,1); assert.equal(config,0);
  context.refresh();
  assert.equal(audit,2); assert.equal(config,0);
  f.window.location.hash='#settings/config'; context.restore(); assert.equal(config,1);
});


test('invalid runtime routes restore a canonical dashboard URL',()=>{
  const f=fixture(); f.window.location.hash='#settings/unsupported'; f.restoreOperatorRoute();
  assert.deepEqual(f.history,[['replace','#dashboard']]); assert.deepEqual(f.loaded,['Dashboard']);
});

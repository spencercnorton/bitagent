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

function settingsFixture() {
  const f = fixture();
  const source = fs.readFileSync(path.join(__dirname, '../static/js/app.js'), 'utf8');
  const groups = ['config', 'integrations', 'content', 'access'];
  const sections = { retention:'config', setup:'integrations', filters:'content', blocklists:'content',
    classifier:'content', liveness:'content', auth:'access', audit:'access' };
  const panels = groups.map(group => {
    const node = f.element(`settings-group-${group}`); node.dataset.settingsPanel = group; return node;
  });
  const buttons = groups.map(group => {
    const node = f.element(`settings-button-${group}`); node.dataset.settingsGroup = group; return node;
  });
  const details = Object.entries(sections).map(([section, group]) => {
    const node = f.element(`settings-disclosure-${section}`);
    node.dataset.settingsSection = section; node.parentElement = f.element(`settings-group-${group}`); node.open = false;
    const summary = f.element(`summary-${section}`); summary.parentElement = node;
    node.querySelector = selector => selector === 'summary' ? summary : null;
    node.scrollIntoView = () => {};
    return node;
  });
  const contains = function (node) {
    for (; node; node = node.parentElement) if (node === this) return true;
    return false;
  };
  [...panels, ...details].forEach(node => { node.contains = contains; });
  groups.forEach(group => f.element(`settings-heading-${group}`));
  [...panels, ...details, ...groups.map(group => f.element(`settings-heading-${group}`)),
    ...Object.keys(sections).map(section => f.element(`summary-${section}`))].forEach(node => {
    node.focus = () => { f.focused.push(node.id); f.document.activeElement = node; };
  });
  const oldQuery = f.document.querySelectorAll;
  f.document.querySelectorAll = selector => {
    if (selector === '#tab-settings [data-settings-panel]') return panels;
    if (selector === '#tab-settings [data-settings-group]') return buttons;
    const match = selector.match(/^#settings-group-(\w+) \.settings-disclosure$/);
    if (match) return details.filter(node => sections[node.dataset.settingsSection] === match[1]);
    return oldQuery(selector);
  };
  f.document.getElementById = id => {
    if (id.startsWith('settings-disclosure-') && !sections[id.replace('settings-disclosure-', '')]) return null;
    return f.element(id);
  };
  const loaded = [];
  const context = vm.createContext({ document:f.document, window:f.window,
    ...Object.fromEntries(['Dashboard','Library','Wants','Evidence','Quarantine','AiTab','System',
      'Settings','AuthStatus','ArrSettings','ClassifierRules','LivenessOps','FiltersStatus','BlockLists','AuditLog']
      .map(name => [`load${name}`, () => loaded.push(name)])), switchSystemTab:()=>{} });
  const nav = source.slice(source.indexOf('/* ── Sidebar drawer'), source.indexOf('/* ── Utility'));
  const settings = source.slice(source.indexOf('function loadSettingsView()'), source.indexOf('async function loadFiltersStatus()'));
  vm.runInContext('let currentTab="settings";\n' + nav + settings + '\nthis.settings={switchSettingsTab,settingsSectionIntent,restoreOperatorRoute};', context);
  return { ...f, ...context.settings, panels, buttons, details, loaded };
}

test('legacy Settings links select their group and open the requested disclosure', () => {
  const f = settingsFixture();
  const expected = {
    retention:['config','Settings'], setup:['integrations'],
    filters:['content','FiltersStatus'], blocklists:['content','BlockLists'],
    classifier:['content','ClassifierRules'], liveness:['content','LivenessOps'],
    auth:['access','Settings','AuthStatus'], audit:['access','AuditLog']
  };
  for (const [section, [group, ...loads]] of Object.entries(expected)) {
    f.loaded.length = 0; f.window.location.hash = `#settings/${section}`;
    f.restoreOperatorRoute();
    assert.deepEqual(f.panels.filter(n => n.classes.has('active')).map(n => n.dataset.settingsPanel), [group]);
    assert.equal(f.element(`settings-disclosure-${section}`).open, true);
    assert.deepEqual(f.buttons.filter(n => n.attrs.get('aria-pressed') === 'true').map(n => n.dataset.settingsGroup), [group]);
    assert.deepEqual(f.loaded, loads, `${section} should load only its selected data source`);
  }
  assert.deepEqual(f.history, [], 'restoring legacy routes must not add history');
});

test('primary Settings groups expose their useful default section', () => {
  const f = settingsFixture();
  f.switchSettingsTab('content');
  assert.equal(f.element('settings-disclosure-filters').open, true);
  assert.deepEqual(f.loaded, ['FiltersStatus']);
  assert.deepEqual(f.history.at(-1), ['push','#settings/content']);
  f.loaded.length = 0; f.switchSettingsTab('access');
  assert.equal(f.element('settings-disclosure-auth').open, true);
  assert.deepEqual(f.loaded, ['Settings','AuthStatus']);
  f.loaded.length = 0; f.switchSettingsTab('integrations');
  assert.equal(f.element('settings-disclosure-setup').open, false);
  assert.deepEqual(f.loaded, ['ArrSettings']);
});

test('switching Settings preserves a visible keyboard focus target', () => {
  const f = settingsFixture();
  f.switchSettingsTab('filters', { history:false, load:false });
  const input = f.element('filter-control'); input.parentElement = f.element('settings-disclosure-filters');
  f.document.activeElement = input;
  f.switchSettingsTab('classifier', { history:false, load:false });
  assert.equal(f.element('settings-disclosure-filters').open, false);
  assert.equal(f.document.activeElement.id, 'summary-classifier');
  f.switchSettingsTab('integrations', { history:false, load:false });
  assert.equal(f.document.activeElement.id, 'settings-heading-integrations');
});

test('native disclosure intent opens one section and loads it once', () => {
  const f = settingsFixture(); f.switchSettingsTab('content', { history:false, load:false });
  const classifier = f.element('settings-disclosure-classifier');
  f.settingsSectionIntent('classifier', classifier);
  assert.equal(f.element('settings-disclosure-filters').open, false);
  assert.deepEqual(f.loaded, ['ClassifierRules']);
  assert.deepEqual(f.history, [['push','#settings/classifier']]);
  // Native <summary> completes the open operation after the click handler.
  classifier.open = true; f.settingsSectionIntent('classifier', classifier);
  assert.deepEqual(f.loaded, ['ClassifierRules'], 'closing an open disclosure needs no redundant read');
});

test('Settings markup has four primary groups and one native editor per connection secret', () => {
  const html = fs.readFileSync(path.join(__dirname, '../templates/index.html'), 'utf8');
  assert.deepEqual([...html.matchAll(/data-settings-group="([^"]+)"/g)].map(match => match[1]),
    ['config','integrations','content','access']);
  assert.equal((html.match(/<details class="settings-disclosure"/g) || []).length, 8);
  for (const arr of ['sonarr','radarr','lidarr']) {
    assert.match(html, new RegExp(`<input[^>]+type="url"[^>]+id="arr-${arr}-url"`));
    assert.match(html, new RegExp(`<input[^>]+type="password"[^>]+id="arr-${arr}-key"`));
    assert.equal((html.match(new RegExp(`id="arr-${arr}-key"`, 'g')) || []).length, 1);
  }
  assert.equal((html.match(/id="tmdbKeyInput"/g) || []).length, 1);
  assert.doesNotMatch(html, /id="(?:dashKeyDisplay|tzKeyDisplay)"/);
});

'use strict';

const test = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const vm = require('node:vm');

const source = fs.readFileSync(path.join(__dirname, '../static/js/app.js'), 'utf8');
const dashboard = source.slice(source.indexOf('/* ── Dashboard'), source.indexOf('/* ── Library'));
const systemStatus = source.slice(source.indexOf('async function _loadSystemStatus()'), source.indexOf('function switchSystemTab('));

function element() {
  const classes = new Set();
  return {
    textContent: '—', innerHTML: '', title: '', dataset: {}, style: {}, hidden: true,
    classList: {
      add: name => classes.add(name), remove: name => classes.delete(name),
      toggle(name, on) { if (on) classes.add(name); else classes.delete(name); },
      contains: name => classes.has(name),
    },
    removeAttribute(name) { delete this[name]; },
  };
}

function harness({ reducedMotion = true, api = async () => null } = {}) {
  const nodes = new Map();
  const frames = new Map();
  let nextFrame = 0;
  const document = { getElementById(id) { if (!nodes.has(id)) nodes.set(id, element()); return nodes.get(id); } };
  const context = vm.createContext({
    document, api, console,
    window: { matchMedia: () => ({ matches: reducedMotion }), location: { host: 'operator.localhost' } },
    performance: { now: () => 0 },
    requestAnimationFrame(callback) { const id = ++nextFrame; frames.set(id, callback); return id; },
    cancelAnimationFrame(id) { frames.delete(id); },
    fmtNum: value => String(value), fmtApproxNum: value => `~${value}`,
    fmtRatePerMin: value => String(value), fmtTime: value => `${value}s`, fmtAgo: () => 'just now',
    escHtml: value => String(value).replace(/[&<>"']/g, c => ({ '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' }[c])),
    typePill: () => '', eventPill: () => '', loadFailedRow: () => '<tr><td>Request failed</td></tr>',
  });
  vm.runInContext(`let _dashboardLoadSeq = 0;\n${dashboard}\n${systemStatus}\n` +
    `globalThis.metrics = { _metricValue, _metricState, _renderMetric, _startDashboardRefresh, _renderDashboardSnapshotStatus, loadDashboard, loadWinRate, loadRecentActivity, _loadSystemStatus, setSequence(value) { _dashboardLoadSeq = value; } };`, context);
  return { metrics: context.metrics, node: id => document.getElementById(id), frames };
}

const observedAt = '2026-10-01T12:00:00Z';
const envelope = (value, overrides = {}) => ({ value, status: 'ok', source: 'synthetic.metric', observed_at: observedAt, stale: false, error: null, ...overrides });

function snapshot(overrides = {}) {
  return {
    snapshot: { observed_at: observedAt, stale: false, cached: false },
    metrics: {
      totalTorrents: envelope(100), totalTorrentsIsEstimate: envelope(false),
      totalEvidence: envelope(10), indexerThroughput: envelope(2),
      livenessBlacklistSize: envelope(0), uptimeSeconds: envelope(10),
      grabSuccessRate: envelope(0.8), grabSuccessCount: envelope(8),
      grabFailureCount: envelope(2), grabPendingCount: envelope(0),
      matchRate30d: envelope(0.9), matchMatched30d: envelope(9), matchTotal30d: envelope(10),
      livenessTotalExcluded: envelope(0), categoryBreakdown: envelope([{ category: 'movie', count: 100 }]),
      graphqlReachable: envelope(true), metricsReachable: envelope(true),
      evidenceReachable: envelope(true), sidecarDbReachable: envelope(true),
      ...overrides,
    },
  };
}

const grabs = (overrides = {}) => ({
  available: true, totalGrabs: 10, bitagentGrabs: 8, bitagentWinRate: 0.8,
  indexers: [{ indexer: 'Synthetic indexer', grabs: 10 }], days: [], ...overrides,
});

function apiForStats(stats) {
  return async endpoint => endpoint === '/api/stats' ? stats
    : endpoint.startsWith('/api/indexer-stats') ? grabs() : { items: [] };
}

test('a measured zero renders as zero with visible freshness and source provenance', () => {
  const h = harness();
  h.metrics._renderMetric('statBlacklist', snapshot(), 'livenessBlacklistSize');
  assert.equal(h.node('statBlacklist').textContent, '0');
  assert.equal(h.node('statBlacklistStatus').dataset.state, 'fresh');
  assert.match(h.node('statBlacklistStatus').textContent, /^Fresh · /);
  assert.match(h.node('statBlacklistStatus').title, /synthetic.metric/);
  assert.match(h.node('statBlacklist').title, /2026-10-01T12:00:00Z/);
});

test('unavailable and failed envelopes cannot leak a numeric value, and partial rates measure', () => {
  const h = harness();
  for (const [status, expected] of [['unavailable', 'unavailable'], ['error', 'failure']]) {
    h.metrics._renderMetric('statThroughput', snapshot({ indexerThroughput: envelope(99, { status, error: 'Synthetic source unavailable' }) }), 'indexerThroughput');
    assert.equal(h.node('statThroughput').textContent, '—');
    assert.equal(h.node('statThroughputStatus').dataset.state, expected);
    assert.match(h.node('statThroughputStatus').title, /Synthetic source unavailable/);
  }
  h.metrics._renderMetric('statThroughput', snapshot({ indexerThroughput: envelope(null, { status: 'partial', error: 'Two samples required' }) }), 'indexerThroughput');
  assert.equal(h.node('statThroughput').textContent, 'measuring…');
  assert.equal(h.node('statThroughputStatus').textContent, 'Measuring…');
  assert.equal(h.node('statThroughput').dataset.state, 'partial');
  assert.equal(h.metrics._metricValue({ totalTorrents: 12 }, 'totalTorrents'), null, 'legacy flat fields cannot replace an unavailable envelope');
});

test('clearing a metric cancels its pending animation before an old frame can repaint it', () => {
  const h = harness({ reducedMotion: false });
  h.metrics._renderMetric('statTorrents', snapshot(), 'totalTorrents');
  assert.equal(h.frames.size, 1);
  h.metrics._renderMetric('statTorrents', null, 'totalTorrents');
  assert.equal(h.frames.size, 0);
  assert.equal(h.node('statTorrents').textContent, '—');
  assert.equal(h.node('statTorrentsStatus').dataset.state, 'failure');
});

test('stale snapshots keep their labeled values without claiming live throughput or healthy meter tones', async () => {
  const stale = snapshot();
  stale.snapshot.stale = true;
  const h = harness({ api: apiForStats(stale), reducedMotion: false });
  await h.metrics.loadDashboard();
  assert.equal(h.node('statTorrents').textContent, '100');
  assert.equal(h.node('statTorrentsStatus').dataset.state, 'stale');
  assert.match(h.node('statTorrentsStatus').textContent, /^Stale · /);
  assert.equal(h.node('statThroughputPulse').hidden, true);
  assert.equal(h.node('statGrabMeter').dataset.tone, 'neutral');
  assert.equal(h.node('statMatchMeter').dataset.tone, 'neutral');
  assert.equal(h.node('categoryChartStatus').dataset.state, 'stale');
  assert.equal(h.node('dashboardStatus').dataset.state, 'stale');
  assert.equal(h.frames.size, 0, 'stale observations must render immediately without counting up');
});

test('a failed refresh clears every previous chart, meter, live indicator and supporting value', async () => {
  let current = snapshot();
  const h = harness({ api: endpoint => apiForStats(current)(endpoint) });
  await h.metrics.loadDashboard();
  assert.equal(h.node('statThroughputPulse').hidden, false);
  assert.match(h.node('categoryChart').innerHTML, /cat-row/);
  current = null;
  await h.metrics.loadDashboard();
  for (const id of ['statTorrents', 'statEvidence', 'statThroughput', 'statBlacklist', 'statUptime', 'statGrabSuccess', 'statMatchRate']) {
    assert.equal(h.node(id).textContent, '—', id);
    assert.equal(h.node(`${id}Status`).dataset.state, 'failure', id);
  }
  for (const id of ['statTorrentsSub', 'statGrabSub', 'statMatchSub', 'statBlacklistSub']) assert.equal(h.node(id).textContent, 'unavailable', id);
  assert.equal(h.node('statThroughputPulse').hidden, true);
  assert.equal(h.node('statGrabMeter').style.width, '0.0%');
  assert.equal(h.node('statMatchMeter').style.width, '0.0%');
  assert.match(h.node('categoryChart').innerHTML, /unavailable/);
  assert.equal(h.node('dashboardUpdatedAt').textContent, 'No current observation');
});

test('the latest refresh owns loading, values and status when responses arrive out of order', async () => {
  const pending = [];
  const h = harness({ api: endpoint => endpoint === '/api/stats'
    ? new Promise(resolve => pending.push(resolve)) : Promise.resolve(endpoint.startsWith('/api/indexer-stats') ? grabs() : { items: [] }) });
  const first = h.metrics.loadDashboard();
  assert.equal(h.node('statTorrentsStatus').dataset.state, 'loading');
  assert.equal(h.node('dashboardStatus').dataset.state, 'loading');
  const second = h.metrics.loadDashboard();
  pending[1](snapshot({ totalTorrents: envelope(200) }));
  await second;
  pending[0](null);
  await first;
  assert.equal(h.node('statTorrents').textContent, '200');
  assert.equal(h.node('statTorrentsStatus').dataset.state, 'fresh');
  assert.equal(h.node('dashboardStatus').dataset.state, 'fresh');
});

test('source failure is distinct from a current snapshot with a rate still measuring', () => {
  const h = harness();
  h.metrics._renderDashboardSnapshotStatus(snapshot({ indexerThroughput: envelope(null, { status: 'partial' }) }));
  assert.equal(h.node('dashboardStatus').dataset.state, 'partial');
  h.metrics._renderDashboardSnapshotStatus(snapshot({ metricsReachable: envelope(null, { status: 'error' }) }));
  assert.equal(h.node('dashboardStatus').dataset.state, 'failure');
  assert.match(h.node('dashboardStatus').textContent, /Some sources failed/);
});

test('grab aggregate failures and unsupported availability are different and clear prior charts', async () => {
  let current = grabs();
  const h = harness({ api: async () => current });
  h.metrics.setSequence(1);
  await h.metrics.loadWinRate(1);
  assert.equal(h.node('winRateValue').textContent, '80.0%');
  assert.match(h.node('winRateStatus').title, /does not expose its source observation time/);
  current = { available: false };
  await h.metrics.loadWinRate(1);
  assert.equal(h.node('winRateStatus').dataset.state, 'unavailable');
  assert.match(h.node('indexerBreakdown').innerHTML, /Core unreachable or grab aggregate unsupported/);
  current = null;
  await h.metrics.loadWinRate(1);
  assert.equal(h.node('winRateStatus').dataset.state, 'failure');
  assert.equal(h.node('winRateValue').textContent, '—');
  assert.equal(h.node('winRateSpark').innerHTML, '');
  current = grabs({ totalGrabs: null });
  await h.metrics.loadWinRate(1);
  assert.equal(h.node('winRateStatus').dataset.state, 'failure');
  assert.match(h.node('indexerBreakdown').innerHTML, /invalid grab aggregate/);
});

test('a day without grabs has no win-rate point and does not connect neighboring observations', async () => {
  const h = harness({ api: async () => grabs({ days: [
    { date: '2026-09-28', totalGrabs: 2, bitagentGrabs: 1 },
    { date: '2026-09-29', totalGrabs: 0, bitagentGrabs: 0 },
    { date: '2026-09-30', totalGrabs: 2, bitagentGrabs: 2 },
  ] }) });
  h.metrics.setSequence(1);
  await h.metrics.loadWinRate(1);
  const spark = h.node('winRateSpark').innerHTML;
  assert.equal((spark.match(/<circle/g) || []).length, 2);
  assert.doesNotMatch(spark, /<polyline/);
  assert.doesNotMatch(spark, /cx="150\.0"/);
});

test('dates omitted by the core create a gap and points use actual time spacing', async () => {
  const h = harness({ api: async () => grabs({ days: [
    { date: '2026-09-25', totalGrabs: 2, bitagentGrabs: 1 },
    { date: '2026-09-26', totalGrabs: 2, bitagentGrabs: 1 },
    { date: '2026-09-30', totalGrabs: 2, bitagentGrabs: 2 },
  ] }) });
  h.metrics.setSequence(1);
  await h.metrics.loadWinRate(1);
  const spark = h.node('winRateSpark').innerHTML;
  assert.equal((spark.match(/<polyline/g) || []).length, 1);
  assert.equal((spark.match(/<circle/g) || []).length, 1);
  assert.match(spark, /points="4\.0,30\.0 62\.4,30\.0"/);
  assert.match(spark, /cx="296\.0"/);
});

test('zero grabs is an available empty window without a fabricated percentage', async () => {
  const h = harness({ api: async () => grabs({ totalGrabs: 0, bitagentGrabs: 0, bitagentWinRate: 0, indexers: [] }) });
  h.metrics.setSequence(1);
  await h.metrics.loadWinRate(1);
  assert.equal(h.node('winRateValue').textContent, '—');
  assert.equal(h.node('winRateCounts').textContent, 'no grabs in window');
  assert.equal(h.node('winRateStatus').dataset.state, 'fresh');
});

test('old auxiliary responses cannot overwrite current loading or evidence state', async () => {
  const pending = [];
  const h = harness({ api: () => new Promise(resolve => pending.push(resolve)) });
  h.metrics.setSequence(1);
  const win = h.metrics.loadWinRate(1);
  const events = h.metrics.loadRecentActivity(1);
  h.metrics.setSequence(2);
  h.metrics._startDashboardRefresh();
  pending[0](grabs());
  pending[1]({ items: [] });
  await Promise.all([win, events]);
  assert.equal(h.node('winRateStatus').dataset.state, 'loading');
  assert.equal(h.node('activityStatus').dataset.state, 'loading');
});

test('an empty evidence page cannot prove a healthy source with zero real activity', async () => {
  const h = harness({ api: async () => ({ totalCount: 0, items: [] }) });
  h.metrics.setSequence(1);
  await h.metrics.loadRecentActivity(1);
  assert.equal(h.node('activityStatus').dataset.state, 'unavailable');
  assert.equal(h.node('activityStatus').textContent, 'No events returned');
  assert.match(h.node('activityTable').innerHTML, /source is unavailable/);
});

test('System failures replace a previously successful auth mode and source pills', async () => {
  let healthy = true;
  const h = harness({ api: async endpoint => healthy
    ? endpoint === '/api/stats' ? snapshot() : endpoint === '/api/auth/tiers' ? { sso: true } : { fields: {} }
    : null });
  await h.metrics._loadSystemStatus();
  assert.equal(h.node('sysAuthMode').textContent, 'SSO');
  healthy = false;
  await h.metrics._loadSystemStatus();
  assert.equal(h.node('sysAuthMode').textContent, 'Unavailable');
  assert.equal(h.node('sysGql').textContent, 'Unavailable');
  assert.equal(h.node('sysMetrics').textContent, 'Unavailable');
  healthy = true;
  await h.metrics._loadSystemStatus();
  assert.equal(h.node('sysAuthMode').textContent, 'SSO');
  assert.equal(h.node('sysAuthMode').title, undefined);
});


test('System distinguishes present unavailable probes from known negative results', async () => {
  const h = harness({api: async endpoint => endpoint === '/api/stats' ? snapshot({graphqlReachable:envelope(null,{status:'unavailable'}),metricsReachable:envelope(false),sidecarDbReachable:envelope(null,{status:'partial'})}) : {sso:true}});
  await h.metrics._loadSystemStatus();
  assert.equal(h.node('sysGql').textContent,'Unavailable');
  assert.match(h.node('sysMetrics').innerHTML,/Unreachable/);
  assert.equal(h.node('sysDb').textContent,'Measuring');
});

test('formatters retain a measured zero for uptime and byte counts', () => {
  const c=vm.createContext({});
  vm.runInContext(source.slice(source.indexOf('function fmtBytes('),source.indexOf('function fmtDate('))+'\nthis.format={fmtBytes,fmtTime};',c);
  assert.equal(c.format.fmtBytes(0),'0.0 B');
  assert.equal(c.format.fmtTime(0),'0m');
  assert.equal(c.format.fmtTime(null),'--');
});

test('out-of-order raw metric responses cannot replace the latest selected view', async () => {
  const requests=[]; const output={textContent:''};
  const c=vm.createContext({document:{getElementById:()=>output},api:()=>new Promise(resolve=>requests.push(resolve))});
  vm.runInContext(source.slice(source.indexOf('let _rawMetricsLoadSeq'),source.indexOf('/* ── Notifications'))+'\nthis.load=loadRawMetrics;',c);
  const first=c.load(),second=c.load(); requests[1]({new:2}); await second;
  requests[0]({old:1}); await first;
  assert.equal(output.textContent,JSON.stringify({new:2},null,2));
});

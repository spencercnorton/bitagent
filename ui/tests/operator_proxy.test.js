'use strict';
const test = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const vm = require('node:vm');

const source = fs.readFileSync(path.join(__dirname, '../static/js/app.js'), 'utf8');
function client(prefix) {
  const calls = [];
  const context = vm.createContext({
    document: {body: {dataset: {operatorPath: prefix}}}, AbortController,
    setTimeout, clearTimeout, console,
    fetch: async (url, options) => { calls.push({url, options}); return {ok: true, json: async () => ({ok: true})}; },
  });
  vm.runInContext(source.match(/^const API = .*;$/m)[0] + '\n' + source.match(/^const API_TIMEOUT_MS = .*;$/m)[0] + '\n' +
    source.slice(source.indexOf('function operatorPosterUrl('), source.indexOf('let currentTab')) + '\n' +
    source.slice(source.indexOf('async function api('), source.indexOf('function debounce(')) + '\nthis.request = api; this.poster = operatorPosterUrl;', context);
  return {request: context.request, poster: context.poster, calls};
}

test('canonical operator requests retain prefix and mutation options', async () => {
  const c = client('/admin');
  await c.request('/api/settings');
  await c.request('/api/settings/overrides/log_level', {method: 'PUT', body: '{"value":"info"}'});
  assert.deepEqual(c.calls.map(x => x.url), ['/admin/api/settings', '/admin/api/settings/overrides/log_level']);
  assert.equal(c.calls[1].options.method, 'PUT');
  assert.equal(c.calls[1].options.body, '{"value":"info"}');
  assert.equal(c.calls[1].options.headers['Content-Type'], 'application/json');
});

test('standalone console and unrecognized prefix values keep same-origin root requests', async () => {
  for (const prefix of ['', undefined, '//foreign.example.org', '/admin/other']) {
    const c = client(prefix);
    await c.request('/api/me');
    assert.equal(c.calls[0].url, '/api/me');
  }
});

test('operator artwork retains admin routing for the known internal proxy while external artwork stays unchanged', () => {
  const c = client('/admin');
  assert.equal(c.poster('/api/poster-proxy?url=synthetic'), '/admin/api/poster-proxy?url=synthetic');
  assert.equal(c.poster('https://artwork.example.org/image.jpg'), 'https://artwork.example.org/image.jpg');
  assert.equal(c.poster('//artwork.example.org/image.jpg'), '//artwork.example.org/image.jpg');
  assert.equal(client('').poster('/api/poster-proxy?url=synthetic'), '/api/poster-proxy?url=synthetic');
});

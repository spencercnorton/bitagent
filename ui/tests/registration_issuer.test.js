'use strict';
const test = require('node:test');
const assert = require('node:assert/strict');
const {snapshot, createController} = require('../static/js/registration-issuer.js');
const owner = 'synthetic-owner';
function state(changes = {}) { return {accountId: owner, active: false, suspended: false, ...changes}; }
function harness(api) {
  const events = [];
  return {events, controller: createController({owner, api, view: {
    state: (...args) => events.push(['state', ...args]),
    status: message => events.push(['status', message]),
  }})};
}
function pending() { let resolve; const promise = new Promise(done => { resolve = done; }); return {promise, resolve}; }

test('issuer load and preview never admit automatically', async () => {
  const calls = [], h = harness(async method => { calls.push(method); return state(); });
  await h.controller.admit(); await h.controller.load(); await h.controller.load();
  assert.deepEqual(calls, ['GET', 'GET']);
  assert.equal(h.events.at(-1)[1].active, false);
});
test('deliberate issuer action binds exact owner and suppresses a double click', async () => {
  const calls = [], mutation = pending();
  const h = harness(async (method, body) => { calls.push([method, body]); return method === 'GET' ? state() : mutation.promise; });
  await h.controller.load(); const first = h.controller.admit(); await h.controller.admit();
  assert.deepEqual(calls, [['GET', undefined], ['PUT', {expectedAccountId: owner}]]);
  mutation.resolve(state({active: true})); await first; await h.controller.admit();
  assert.equal(calls.length, 2); assert.equal(h.events.at(-1)[1].active, true);
});
test('active and suspended members have no admission action', async () => {
  for (const changes of [{active: true}, {suspended: true}]) {
    const calls = [], h = harness(async method => { calls.push(method); return state(changes); });
    await h.controller.load(); await h.controller.admit(); assert.deepEqual(calls, ['GET']);
  }
});
test('account mismatch cannot admit and a swapped response never marks access active', async () => {
  assert.throws(() => snapshot(state({accountId: 'other'}), owner));
  const calls = [], h = harness(async method => { calls.push(method); return state({accountId: 'other'}); });
  await h.controller.load(); await h.controller.admit(); assert.deepEqual(calls, ['GET']);
  const swapped = harness(async method => state(method === 'GET' ? {} : {accountId: 'other', active: true}));
  await swapped.controller.load(); await swapped.controller.admit(); assert.equal(swapped.events.at(-1)[1], null);
});
test('uncertain mutation requires explicit status reload and never retries itself', async () => {
  const calls = [], h = harness(async method => { calls.push(method); if (method === 'PUT') throw new Error('synthetic network failure'); return state(); });
  await h.controller.load(); await h.controller.admit(); await h.controller.admit();
  assert.deepEqual(calls, ['GET', 'PUT']); assert.equal(h.events.at(-1)[1], null);
  await h.controller.load(); assert.deepEqual(calls, ['GET', 'PUT', 'GET']);
});
test('a response after pagehide cannot enable the admission button', async () => {
  const response = pending(), h = harness(async () => response.promise);
  const load = h.controller.load(); h.controller.close(); const length = h.events.length;
  response.resolve(state()); await load; await h.controller.admit(); assert.equal(h.events.length, length);
});

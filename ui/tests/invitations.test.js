'use strict';
const test = require('node:test');
const assert = require('node:assert/strict');
const invitations = require('../static/js/invitations.js');
const token = 'bi_' + 'A'.repeat(43);
const id = 'a'.repeat(32);
const origin = 'https://library.example.test';
const createdAt = '2026-01-02T10:00:00Z';
const expiresAt = '2026-01-16T10:00:00Z';
function account(changes = {}) {
  return {schemaVersion:1, enabled:true, accountId:'synthetic-user', unlimited:false, year:2026,
    annualLimit:3, annualUsed:0, annualRemaining:3, paidCredits:0,
    price:{amountMinor:5000,currency:'USD'}, checkoutAvailable:false, invitations:[], ...changes};
}
function fresh(changes = {}) { return {id, createdAt, expiresAt, token, creditSource:'annual', shareUrl:origin + '/invite#' + token, ...changes}; }
function harness(api, changes = {}) {
  const events = [];
  const view = Object.fromEntries(['status','busy','secret','manage','recipient','accepted','invalidate','signIn','payment'].map(name => [name, (...args) => events.push([name,...args])]));
  // Legacy invitation cases use a separate empty-order fixture. Payment cases
  // explicitly pass withOrders so every order request reaches their fake API.
  const wrapped = (...args) => args[0] === '/api/account/invitations/orders' && !changes.withOrders
    ? Promise.resolve({schemaVersion:1,accountId:'synthetic-user',orders:[]}) : api(...args);
  return {events, controller:invitations.createController({owner:'synthetic-user', origin, api:wrapped, view, copy:async () => {}, ...changes})};
}
function deferred() { let resolve; const promise = new Promise(done => { resolve = done; }); return {promise,resolve}; }

test('invitation fragment is removed before any asynchronous request', () => {
  const calls = [];
  assert.equal(invitations.takeFragment({hash:'#'+token, pathname:'/invite', search:''}, {replaceState(...args) { calls.push(args); }}), token);
  assert.deepEqual(calls, [[null,'','/invite']]);
  assert.equal(invitations.fragmentToken('?token=' + token), null);
  for (const hash of ['#wrong', '#'+token+'/extra', '#'+token+'?other=1', '#%62i_'+'A'.repeat(43)]) assert.equal(invitations.fragmentToken(hash), null);
});

test('created link must be exact canonical HTTPS origin and token fragment', () => {
  assert.equal(invitations.issued(fresh(), origin).url, origin+'/invite#'+token);
  for (const shareUrl of ['javascript:alert(1)', 'https://other.example.test/invite#'+token, origin+'/other#'+token, origin+'/invite?token=x#'+token, 'http://library.example.test/invite#'+token, 'https://user:pass@library.example.test/invite#'+token]) {
    assert.throws(() => invitations.issued(fresh({shareUrl}), origin));
  }
});

test('snapshot pins owner and rejects machine accounts and malformed metadata', () => {
  assert.equal(invitations.snapshot(account(), 'synthetic-user').annualRemaining, 3);
  for (const [data, owner] of [[account(),'api-client'], [account(),'anonymous'], [account({accountId:'different'}),'synthetic-user'], [account({checkoutAvailable:'true'}),'synthetic-user'], [account({annualRemaining:-1}),'synthetic-user'], [account({invitations:Array(101).fill({})}),'synthetic-user']]) {
    assert.throws(() => invitations.snapshot(data, owner));
  }
  assert.throws(() => invitations.snapshot(account({invitations:[{id,createdAt,expiresAt,status:'<img onerror=evil>'}]}), 'synthetic-user'));
});

test('unlimited allowance is represented by null, not an invented numeric balance', () => {
  assert.equal(invitations.snapshot(account({unlimited:true, annualRemaining:null}), 'synthetic-user').unlimited, true);
  assert.throws(() => invitations.snapshot(account({unlimited:true, annualRemaining:3}), 'synthetic-user'));
});

test('metadata requires unique IDs, UTC timestamps and a later expiry', () => {
  const item = {id,createdAt,expiresAt,status:'pending'};
  for (const items of [[item,item],[{...item,createdAt:123}], [{...item,expiresAt:createdAt}], [{...item,expiresAt:'2026-01-16T10:00:00'}]]) {
    assert.throws(() => invitations.snapshot(account({invitations:items}), 'synthetic-user'));
  }
});

test('double create click performs one mutation and refreshes before displaying the link', async () => {
  const pending = deferred(), calls = [];
  const h = harness(async (path, method, body) => { calls.push([path,method,body]); return method === 'POST' ? pending.promise : account(); });
  await h.controller.load();
  const first = h.controller.create('annual'); await h.controller.create('annual');
  assert.equal(calls.filter(call => call[1] === 'POST').length, 1);
  assert.equal(h.events.filter(event => event[0] === 'secret' && event[1]).length, 0);
  pending.resolve(fresh()); await first;
  assert.deepEqual(calls.find(call => call[1] === 'POST')[2], {expectedAccountId:'synthetic-user',creditSource:'annual'});
  assert.equal(h.events.filter(event => event[0] === 'secret' && event[1]).length, 1);
  assert.equal(calls.at(-1)[0], '/api/account/invitations');
});

test('account switch during the post-create refresh clears the fresh token', async () => {
  let reads = 0;
  const h = harness(async (_path, method) => method === 'POST' ? fresh() : account({accountId:++reads === 1 ? 'synthetic-user' : 'different'}));
  await h.controller.load(); await h.controller.create('annual');
  assert.equal(h.events.some(event => event[0] === 'secret' && event[1]), false);
  assert.match(h.events.findLast(event => event[0] === 'status')[1], /account changed/);
});

test('a pending response cannot display an invitation after pagehide', async () => {
  const pending = deferred();
  const h = harness(async (_path, method) => method === 'POST' ? pending.promise : account());
  await h.controller.load(); const creating = h.controller.create('annual');
  h.controller.close(); pending.resolve(fresh()); await creating;
  assert.equal(h.events.some(event => event[0] === 'secret' && event[1]), false);
});

test('a failed mutation is not automatically retried and requires refresh', async () => {
  const calls = [];
  const h = harness(async (_path, method) => { calls.push(method); if (method === 'POST') throw new Error('synthetic timeout'); return account(); });
  await h.controller.load(); await h.controller.create('annual'); await h.controller.create('annual');
  assert.equal(calls.filter(method => method === 'POST').length, 1);
  assert.match(h.events.findLast(event => event[0] === 'status')[1], /Refresh before/);
});

test('exhausted annual allowance and missing paid credits cannot mutate', async () => {
  const calls = [];
  const h = harness(async (_path, method) => { calls.push(method); return account({annualUsed:3,annualRemaining:0}); });
  await h.controller.load(); await h.controller.create('annual'); await h.controller.create('paid');
  assert.equal(calls.length, 1);
});

test('purchased source remains selectable even when annual quota remains', async () => {
  const calls = [];
  const h = harness(async (_path, method, body) => { calls.push([method,body]); return method === 'POST' ? fresh({creditSource:'paid'}) : account({paidCredits:1}); });
  await h.controller.load(); h.controller.selectionChanged(); await h.controller.create('paid');
  assert.equal(calls.filter(([method]) => !method).length, 2);
  assert.equal(calls.find(([method]) => method === 'POST')[1].creditSource, 'paid');
});

test('revoke pins account in strict JSON and refreshes server quota without refunding', async () => {
  const calls = [];
  const value = account({annualUsed:1,annualRemaining:2,invitations:[{id,createdAt,expiresAt,status:'pending'}]});
  const h = harness(async (path,method,body) => { calls.push([path,method,body]); return method === 'DELETE' ? {id,status:'revoked'} : value; });
  await h.controller.load(); await h.controller.revoke(id);
  assert.deepEqual(calls.find(call => call[1] === 'DELETE'), ['/api/account/invitations/'+id,'DELETE',{expectedAccountId:'synthetic-user'}]);
  assert.equal(h.events.findLast(event => event[0] === 'manage')[1].annualRemaining, 2);
});

test('clipboard rejection offers manual copying and never claims success', async () => {
  const h = harness(async (_path,method) => method === 'POST' ? fresh() : account(), {copy:async () => { throw new Error('denied'); }});
  await h.controller.load(); await h.controller.create('annual'); await h.controller.copy();
  assert.match(h.events.findLast(event => event[0] === 'status')[1], /copy it manually/);
  assert.equal(h.events.some(event => event[0] === 'status' && event[1] === 'Invitation link copied.'), false);
  h.controller.dismiss(); assert.equal(h.events.findLast(event => event[0] === 'secret')[1], null);
});

test('unavailable preview does not fetch account identity or redeem', async () => {
  const calls = [];
  const h = harness(async path => { calls.push(path); return {available:false}; }, {owner:'',token});
  await h.controller.preview(); await h.controller.redeem();
  assert.deepEqual(calls, ['/api/invitations/preview']);
});

test('signed-out invitee gets a clear sign-in instruction without an invented login URL', async () => {
  const calls = [];
  const h = harness(async path => { calls.push(path); if (path === '/api/me') throw {code:401}; return {available:true,expiresAt}; }, {owner:'',token});
  await h.controller.preview(); await h.controller.redeem();
  assert.equal(calls.length, 2); assert.match(h.events.findLast(event => event[0] === 'status')[1], /Sign in.*reopen/);
});

test('shared API client is not accepted as an invitation recipient', async () => {
  const h = harness(async path => path === '/api/me' ? {id:'api-client',method:'api-key'} : {available:true,expiresAt}, {owner:'',token});
  await h.controller.preview(); await h.controller.redeem();
  assert.equal(h.events.some(event => event[0] === 'recipient'), false);
  assert.equal(h.events.some(event => event[0] === 'accepted'), false);
});

test('redemption uses freshly observed SSO owner and repeated clicks do not redeem twice', async () => {
  const pending = deferred(), calls = [];
  const h = harness(async (path,method,body) => {
    calls.push([path,method,body]);
    if (path === '/api/me') return {id:'recipient',method:'forwarded-user',display:'Synthetic Recipient'};
    if (path.endsWith('preview')) return {available:true,expiresAt};
    return pending.promise;
  }, {owner:'',token});
  await h.controller.preview(); const accepting = h.controller.redeem(); await h.controller.redeem();
  assert.deepEqual(calls.at(-1)[2], {token,expectedAccountId:'recipient'});
  pending.resolve({approved:true,alreadyRedeemed:false,accountId:'recipient'}); await accepting;
  assert.equal(h.events.filter(event => event[0] === 'accepted').length, 1);
  await h.controller.redeem(); assert.equal(calls.length, 3);
});

test('redemption account mismatch cannot reveal a success link', async () => {
  const h = harness(async path => path === '/api/me' ? {id:'recipient',method:'npm-header'} : path.endsWith('preview') ? {available:true,expiresAt} : {approved:true,alreadyRedeemed:false,accountId:'other'}, {owner:'',token});
  await h.controller.preview(); await h.controller.redeem();
  assert.equal(h.events.some(event => event[0] === 'accepted'), false);
  assert.match(h.events.findLast(event => event[0] === 'status')[1], /account changed/);
});

test('JSON request uses same-origin no-store no redirects and a DELETE body', async () => {
  let options;
  const result = await invitations.request(async (_path,value) => { options = value; return {ok:true,text:async () => '{"id":"synthetic"}'}; }, '/api/account/invitations/'+id, 'DELETE', {expectedAccountId:'synthetic-user'});
  assert.equal(result.id, 'synthetic');
  assert.equal(options.redirect, 'error'); assert.equal(options.credentials, 'same-origin'); assert.equal(options.cache, 'no-store');
  assert.equal(options.headers['Content-Type'], 'application/json'); assert.equal(JSON.parse(options.body).expectedAccountId, 'synthetic-user');
});

test('bounded stream rejects oversize JSON and cancels the reader', async () => {
  let canceled = false;
  const reader = {read:async () => ({done:false,value:new Uint8Array(65537)}), cancel:async () => { canceled = true; }, releaseLock() {}};
  await assert.rejects(invitations.request(async () => ({ok:true,body:{getReader:() => reader}}), '/api/account/invitations'));
  assert.equal(canceled, true);
});

function dom(mode) {
  class Element {
    constructor() { this.dataset = {}; this.hidden = false; this.disabled = false; this.value = ''; this.textContent = ''; this.children = []; this.listeners = {}; this.options = [{disabled:false},{disabled:false}]; }
    append(...nodes) { this.children.push(...nodes); }
    replaceChildren(...nodes) { this.children = nodes; }
    addEventListener(event, fn) { this.listeners[event] = fn; }
    querySelectorAll() { return this.children.flatMap(child => child.children.filter(node => node.listeners.click)); }
  }
  const elements = new Map();
  const document = {body:{dataset:{invitationView:mode,accountId:'synthetic-user'}}, getElementById(id) { if (id === 'invSignInForm' && !elements.has(id)) return null; if (!elements.has(id)) elements.set(id,new Element()); return elements.get(id); }, createElement:() => new Element()};
  document.getElementById('invCreditSource').value = 'annual';
  return {document,elements};
}

test('real management mount keeps purchased choice without a refreshing change handler', async () => {
  const {document,elements} = dom('manage'); const calls = [], listener = {};
  const win = {location:{origin},navigator:{},addEventListener:(name,fn) => { listener[name] = fn; },fetch:async (path,options) => {
    calls.push([path,options]); const body = path.endsWith('/orders') ? {schemaVersion:1,accountId:'synthetic-user',orders:[]} : options.method === 'POST' ? fresh({creditSource:'paid'}) : account({paidCredits:1});
    return {ok:true,text:async () => JSON.stringify(body)};
  }};
  const controller = invitations.mount(win, document);
  await new Promise(resolve => setImmediate(resolve));
  const select = elements.get('invCreditSource'); select.value = 'paid'; select.listeners.change();
  assert.equal(select.value, 'paid'); assert.equal(calls.length, 2);
  await controller.create(select.value);
  assert.equal(JSON.parse(calls.find(call => call[1].method === 'POST')[1].body).creditSource, 'paid');
  assert.equal(elements.get('invShareUrl').value, origin + '/invite#' + token);
  listener.pagehide(); assert.equal(elements.get('invShareUrl').value, '');
  assert.equal(elements.get('invNewLink').hidden, true);
});

test('landing mount clears fragment first and renders recipient text without HTML', async () => {
  const {document,elements} = dom('redeem'); const order = [];
  const display = '<img src=x onerror=alert(1)>';
  const win = {location:{origin,pathname:'/invite',search:'',hash:'#'+token},history:{replaceState(_state,_title,url) { order.push(['clear',url]); }}, navigator:{},addEventListener() {},fetch:async (path) => {
    order.push(['request',path]);
    return {ok:true,text:async () => JSON.stringify(path === '/api/me' ? {id:'recipient',method:'npm-header',display} : {available:true,expiresAt})};
  }};
  invitations.mount(win, document); await new Promise(resolve => setImmediate(resolve));
  assert.deepEqual(order[0], ['clear','/invite']);
  assert.equal(elements.get('invRecipient').textContent, 'Accepting as ' + display);
  assert.equal(elements.get('invRecipient').children.length, 0);
  assert.equal(elements.get('invAccept').disabled, false);
});


test('sign-in target must be the exact canonical HTTPS bridge route', () => {
  assert.equal(invitations.signInTarget('https://sso.example.test/invitations/start'), 'https://sso.example.test/invitations/start');
  for (const value of ['', undefined, 'http://sso.example.test/invitations/start', 'https://sso.example.test:443/invitations/start', 'https://user@sso.example.test/invitations/start', 'https://sso.example.test/invitations/start?token=x', 'https://sso.example.test/invitations/start#x', 'https://sso.example.test/other', ' https://sso.example.test/invitations/start']) assert.equal(invitations.signInTarget(value), null);
});

test('bridge sign-in requires an available preview and performs one explicit token-only submission', async () => {
  const submissions = [], calls = [];
  const h = harness(async path => { calls.push(path); if (path === '/api/me') throw {code:401}; return {available:true,expiresAt}; },
    {owner:'',token,signInUrl:'https://sso.example.test/invitations/start',submitSignIn:(...args) => submissions.push(args)});
  h.controller.signIn(); assert.equal(submissions.length, 0);
  await h.controller.preview();
  assert.equal(submissions.length, 0); assert.equal(h.events.findLast(item => item[0] === 'signIn')[1], true);
  h.controller.signIn(); h.controller.signIn();
  assert.deepEqual(submissions, [[token,'https://sso.example.test/invitations/start']]);
  assert.deepEqual(calls, ['/api/invitations/preview','/api/me']);
});

test('unavailable, disabled, changed action, and pagehide cannot submit an invitation', async () => {
  for (const changes of [{available:false}, {signInUrl:''}, {signInUrl:'https://sso.example.test/other'}, {close:true}]) {
    const submissions = [];
    const h = harness(async path => { if (path === '/api/me') throw {code:401}; return {available:changes.available !== false,expiresAt}; },
      {owner:'',token,signInUrl:changes.signInUrl ?? 'https://sso.example.test/invitations/start',submitSignIn:(...args) => submissions.push(args)});
    await h.controller.preview(); if (changes.close) h.controller.close(); h.controller.signIn();
    assert.equal(submissions.length, 0);
  }
});

test('landing native form sends only token at explicit submit and clears its DOM on pagehide', async () => {
  const {document,elements} = dom('redeem'); const listeners = {}, sent = [], requests = [];
  const form = {hidden:true,listeners:{},getAttribute:() => 'https://sso.example.test/invitations/start',addEventListener(name,fn) { this.listeners[name] = fn; }};
  elements.set('invSignInForm', form);
  const win = {location:{origin,pathname:'/invite',search:'',hash:'#'+token},history:{replaceState() {}},navigator:{},addEventListener:(name,fn) => { listeners[name] = fn; },
    HTMLFormElement:{prototype:{submit() { sent.push(elements.get('invSignInToken').children.map(input => ({type:input.type,name:input.name,value:input.value}))); }}},
    fetch:async path => { requests.push(path); return path === '/api/me' ? {ok:false,status:401} : {ok:true,text:async () => JSON.stringify({available:true,expiresAt})}; }};
  const controller = invitations.mount(win,document); await new Promise(resolve => setImmediate(resolve));
  assert.equal(elements.get('invSignInToken').children.length, 0); assert.equal(form.hidden, false); assert.equal(sent.length, 0);
  let prevented = false; form.listeners.submit({preventDefault() { prevented = true; }});
  assert.equal(prevented, true); assert.deepEqual(sent, [[{type:'hidden',name:'token',value:token}]]);
  listeners.pagehide(); assert.equal(elements.get('invSignInToken').children.length, 0); assert.equal(form.hidden,true);
  controller.signIn(); assert.equal(sent.length,1); assert.deepEqual(requests,['/api/invitations/preview','/api/me']);
});

test('a changed rendered action fails closed before native form submission', async () => {
  const {document,elements} = dom('redeem'); let action = 'https://sso.example.test/invitations/start', submissions = 0;
  const form = {hidden:true,listeners:{},getAttribute:() => action,addEventListener(name,fn) { this.listeners[name] = fn; }}; elements.set('invSignInForm',form);
  const win = {location:{origin,pathname:'/invite',search:'',hash:'#'+token},history:{replaceState() {}},navigator:{},addEventListener() {},HTMLFormElement:{prototype:{submit() { submissions++; }}},
    fetch:async path => path === '/api/me' ? {ok:false,status:401} : {ok:true,text:async () => JSON.stringify({available:true,expiresAt})}};
  invitations.mount(win,document); await new Promise(resolve => setImmediate(resolve));
  action = 'https://other.example.test/invitations/start'; form.listeners.submit({preventDefault() {}});
  assert.equal(submissions,0); assert.equal(elements.get('invSignInToken').children.length,0); assert.equal(form.hidden,true);
});

const requestId = '12345678-1234-4234-8234-123456789012';
const stripeUrl = 'https://checkout.stripe.com/c/pay/cs_test_Synthetic#fidkdWxOYHwnPyd1blpxYHZxWjA0';
function order(changes = {}) { return {orderId:id,requestId,status:'open',createdAt,retryAllowed:true,...changes}; }
function orderList(items = [], changes = {}) { return {schemaVersion:1,accountId:'synthetic-user',orders:items,...changes}; }
function paymentHarness(api, changes = {}) {
  const opened = [];
  const h = harness(api,{withOrders:true,newRequestId:() => requestId,openCheckout:value => opened.push(value),...changes});
  return {...h,opened};
}
function paymentView(h) { return h.events.findLast(event => event[0] === 'payment')[1]; }

test('checkout target permits only exact Stripe session HTTPS and its legitimate fragment', () => {
  assert.equal(invitations.checkoutTarget(stripeUrl),stripeUrl);
  assert.equal(invitations.checkoutTarget(stripeUrl.split('#')[0]),stripeUrl.split('#')[0]);
  for (const url of ['http://checkout.stripe.com/c/pay/cs_test_x','https://checkout.stripe.com:443/c/pay/cs_test_x','https://user@checkout.stripe.com/c/pay/cs_test_x','https://checkout.stripe.com.evil.test/c/pay/cs_test_x','https://checkout.stripe.com/other/cs_test_x','https://checkout.stripe.com/c/pay/cs_test_x?x=1','https://checkout.stripe.com/c/pay/cs_test_x/extra','https://checkout.stripe.com/c/pay/cs_test_%78',' '+stripeUrl,stripeUrl+'\n',stripeUrl+'☃','https://checkout.stripe.com/c/pay/cs_'+'x'.repeat(4096)]) assert.equal(invitations.checkoutTarget(url),null);
});

test('order list is bounded, owner-pinned, unique and allows only one unresolved order', () => {
  assert.deepEqual(invitations.orders(orderList([order()]),'synthetic-user'),[order()]);
  for (const body of [orderList([], {accountId:'other'}),orderList(Array(21).fill(order())),orderList([order(),order()]),orderList([order(),order({orderId:'b'.repeat(32),requestId:'22345678-1234-4234-8234-123456789012'})]),orderList([order({requestId:requestId.toUpperCase()+'A'})]),orderList([order({status:'credited',retryAllowed:true})]),orderList([order({createdAt:'not-a-time'})]),orderList([order({retryAllowed:'yes'})])]) assert.throws(() => invitations.orders(body,'synthetic-user'));
});

test('checkout response pins owner, request and order before any navigation', () => {
  const result = {accountId:'synthetic-user',requestId,orderId:id,status:'open',checkoutUrl:stripeUrl};
  assert.equal(invitations.checkoutResult(result,'synthetic-user',requestId),result);
  for (const changes of [{accountId:'other'},{requestId:'22345678-1234-4234-8234-123456789012'},{orderId:'invalid'},{status:'prepared'},{checkoutUrl:'https://other.example.test'},{status:'pending'},{status:'credited',checkoutUrl:stripeUrl}]) assert.throws(() => invitations.checkoutResult({...result,...changes},'synthetic-user',requestId));
});

test('load does one order GET and never creates a checkout or invitation automatically', async () => {
  const calls = [];
  const h = paymentHarness(async path => { calls.push(path); return path.endsWith('/orders') ? orderList() : account({checkoutAvailable:true}); });
  await h.controller.load();
  assert.deepEqual(calls,['/api/account/invitations','/api/account/invitations/orders']);
  assert.equal(paymentView(h).showBuy,true); assert.equal(h.opened.length,0);
});

test('owner unlimited and temporarily unavailable proof never offer or post checkout', async () => {
  for (const value of [account({unlimited:true,annualRemaining:null}), account({checkoutAvailable:false})]) {
    const calls=[];
    const h=paymentHarness(async (path,method) => { calls.push([path,method]); return path.endsWith('/orders') ? orderList([order()]) : value; });
    await h.controller.load(); await h.controller.checkout();
    assert.equal(paymentView(h).showBuy,false); assert.equal(calls.filter(call => call[1] === 'POST').length,0);
  }
});

test('checkout opens only after explicit click, one POST, fresh same-account and order confirmation', async () => {
  const pending=deferred(), calls=[]; let submitted=false, newIds=0;
  const h=paymentHarness(async (path,method,body) => {
    calls.push([path,method,body]);
    if (method === 'POST') { submitted=true; return pending.promise; }
    return path.endsWith('/orders') ? orderList(submitted ? [order()] : []) : account({checkoutAvailable:true});
  },{newRequestId:() => { newIds++; return requestId; }});
  await h.controller.load(); assert.equal(newIds,0);
  const creating=h.controller.checkout(); await h.controller.checkout();
  assert.equal(calls.filter(call => call[1] === 'POST').length,1); assert.equal(h.opened.length,0);
  pending.resolve({accountId:'synthetic-user',requestId,orderId:id,status:'open',checkoutUrl:stripeUrl}); await creating;
  assert.deepEqual(calls.find(call => call[1] === 'POST'),['/api/account/invitations/checkout','POST',{expectedAccountId:'synthetic-user',requestId}]);
  assert.deepEqual(h.opened,[stripeUrl]); assert.equal(newIds,1);
  assert.equal(calls.filter(call => call[1] === 'POST' && call[0] === '/api/account/invitations').length,0);
});

test('unknown POST outcome holds UUID and cannot produce a new order or auto retry after empty GET', async () => {
  const calls=[]; let ids=0;
  const h=paymentHarness(async (path,method) => { calls.push([path,method]); if (method === 'POST') throw new Error('synthetic timeout'); return path.endsWith('/orders') ? orderList() : account({checkoutAvailable:true}); },{newRequestId:() => { ids++; return requestId; }});
  await h.controller.load(); await h.controller.checkout(); await h.controller.checkout();
  await h.controller.load(); await h.controller.checkout();
  assert.equal(ids,1); assert.equal(calls.filter(call => call[1] === 'POST').length,1);
  assert.equal(paymentView(h).showBuy,false); assert.equal(paymentView(h).showRefresh,true); assert.match(paymentView(h).text,/awaiting confirmation/);
});

test('server recovery permits explicit same-UUID resume and never regenerates a provider key', async () => {
  const calls=[]; let recovered=false, ids=0;
  const h=paymentHarness(async (path,method,body) => {
    calls.push([path,method,body]);
    if (method === 'POST') { if (!recovered) throw new Error('synthetic timeout'); return {accountId:'synthetic-user',requestId,orderId:id,status:'open',checkoutUrl:stripeUrl}; }
    return path.endsWith('/orders') ? orderList(recovered ? [order({status:'unknown'})] : []) : account({checkoutAvailable:true});
  },{newRequestId:() => { ids++; return requestId; }});
  await h.controller.load(); await h.controller.checkout(); recovered=true; await h.controller.load();
  assert.equal(calls.filter(call => call[1] === 'POST').length,1); assert.equal(paymentView(h).retry,true);
  await h.controller.checkout();
  assert.deepEqual(calls.filter(call => call[1] === 'POST').map(call => call[2].requestId),[requestId,requestId]);
  assert.equal(ids,1); assert.equal(h.opened.length,0); // Ledger is still unknown; URL alone cannot navigate.
});

test('an existing order without retry permission blocks checkout despite active provider proof', async () => {
  const calls=[];
  const h=paymentHarness(async (path,method) => { calls.push([path,method]); return path.endsWith('/orders') ? orderList([order({retryAllowed:false})]) : account({checkoutAvailable:true}); });
  await h.controller.load(); await h.controller.checkout();
  assert.equal(paymentView(h).showBuy,false); assert.equal(calls.filter(call => call[1] === 'POST').length,0);
});

test('order 404 disables payment while free allowance stays usable, 503 holds new purchases', async () => {
  for (const code of [404,503]) {
    const calls=[];
    const h=paymentHarness(async (path,method) => { calls.push([path,method]); if (path.endsWith('/orders')) throw {code}; return method === 'POST' ? fresh() : account({checkoutAvailable:true}); });
    await h.controller.load(); await h.controller.checkout(); await h.controller.create('annual');
    assert.equal(paymentView(h).showBuy,false);
    assert.deepEqual(calls.filter(call => call[1] === 'POST').map(call => call[0]),['/api/account/invitations']);
    assert.equal(h.events.some(event => event[0] === 'secret' && event[1]),true);
  }
});

test('paid confirmation and browser return never mint an invitation or infer credit', async () => {
  const calls=[];
  const h=paymentHarness(async (path,method) => { calls.push([path,method]); return path.endsWith('/orders') ? orderList([order({status:'credited',retryAllowed:false})]) : account({paidCredits:0,checkoutAvailable:false}); });
  await h.controller.load(); await h.controller.load();
  assert.equal(calls.filter(call => call[1] === 'POST').length,0);
  assert.equal(h.events.findLast(event => event[0] === 'manage')[1].paidCredits,0);
  assert.match(paymentView(h).text,/Payment verified/); assert.equal(h.opened.length,0);
});

test('pagehide and account switch during checkout prevent navigation and clear actions', async () => {
  for (const scenario of ['pagehide','account-switch']) {
    const pending=deferred(); let reads=0, submitted=false;
    const h=paymentHarness(async (path,method) => {
      if (method === 'POST') { submitted=true; return pending.promise; }
      if (path.endsWith('/orders')) return orderList(submitted ? [order()] : []);
      return account({checkoutAvailable:true,accountId:++reads > 1 && scenario === 'account-switch' ? 'other' : 'synthetic-user'});
    });
    await h.controller.load(); const action=h.controller.checkout();
    if (scenario === 'pagehide') h.controller.close();
    pending.resolve({accountId:'synthetic-user',requestId,orderId:id,status:'open',checkoutUrl:stripeUrl}); await action;
    assert.equal(h.opened.length,0); assert.equal(paymentView(h).showBuy,false);
  }
});

test('malformed checkout and order refresh cannot navigate or automatically retry', async () => {
  for (const scenario of ['wrong-url','wrong-owner','order-error','proof-lost']) {
    let submitted=false, posts=0;
    const h=paymentHarness(async (path,method) => {
      if (method === 'POST') { submitted=true; posts++; return {accountId:scenario === 'wrong-owner' ? 'other' : 'synthetic-user',requestId,orderId:id,status:'open',checkoutUrl:scenario === 'wrong-url' ? 'https://evil.example.test' : stripeUrl}; }
      if (path.endsWith('/orders')) { if (submitted && scenario === 'order-error') throw {code:503}; return orderList(submitted ? [order()] : []); }
      return account({checkoutAvailable:!submitted || scenario !== 'proof-lost'});
    });
    await h.controller.load(); await h.controller.checkout();
    assert.equal(h.opened.length,0); assert.equal(posts,1);
  }
});

test('missing, throwing, or invalid random UUID fails before a checkout mutation', async () => {
  for (const newRequestId of [() => null,() => 'not-a-uuid',() => { throw new Error('synthetic crypto failure'); }]) {
    const calls=[];
    const h=paymentHarness(async (path,method) => { calls.push([path,method]); return path.endsWith('/orders') ? orderList() : account({checkoutAvailable:true}); },{newRequestId});
    await h.controller.load(); await h.controller.checkout();
    assert.equal(calls.filter(call => call[1] === 'POST').length,0); assert.match(h.events.findLast(event => event[0] === 'status')[1],/unavailable in this browser/);
  }
});

test('mounted payment buttons remain hidden for owner and expose only explicit available action', async () => {
  for (const unlimited of [false,true]) {
    const {document,elements}=dom('manage'); const calls=[];
    const win={location:{origin,assign:() => { throw new Error('unexpected navigation'); }},crypto:{randomUUID:() => requestId},navigator:{},addEventListener() {},fetch:async (path,options) => {
      calls.push([path,options.method]); return {ok:true,text:async () => JSON.stringify(path.endsWith('/orders') ? orderList() : account({unlimited,annualRemaining:unlimited ? null : 3,checkoutAvailable:!unlimited}))};
    }};
    invitations.mount(win,document); await new Promise(resolve => setImmediate(resolve));
    assert.equal(elements.get('invCheckout').hidden,unlimited); assert.equal(elements.get('invCheckout').disabled,false);
    assert.equal(calls.filter(call => call[1] === 'POST').length,0);
  }
});

test('manual hold remains unresolved, cannot retry or navigate, and asks for operator reconciliation', async () => {
  const held=order({status:'manual_hold',retryAllowed:false});
  assert.deepEqual(invitations.orders(orderList([held]),'synthetic-user'),[held]);
  assert.throws(() => invitations.orders(orderList([{...held,retryAllowed:true}]),'synthetic-user'));
  assert.throws(() => invitations.orders(orderList([held,order({orderId:'b'.repeat(32),requestId:'22345678-1234-4234-8234-123456789012'})]),'synthetic-user'));
  assert.equal(invitations.checkoutResult({accountId:'synthetic-user',requestId,orderId:id,status:'manual_hold',checkoutUrl:null},'synthetic-user',requestId).status,'manual_hold');
  const calls=[];
  const h=paymentHarness(async (path,method) => { calls.push([path,method]); return path.endsWith('/orders') ? orderList([held]) : account({checkoutAvailable:true}); });
  await h.controller.load(); await h.controller.checkout(); await h.controller.load();
  assert.equal(calls.filter(call => call[1] === 'POST').length,0);assert.equal(h.opened.length,0);
  assert.equal(paymentView(h).showBuy,false);assert.equal(paymentView(h).showRefresh,true);assert.match(paymentView(h).text,/operator reconciliation/);
  let submitted=false,posts=0;
  const mutation=paymentHarness(async (path,method) => {
    if (method === 'POST') { submitted=true;posts++;return {accountId:'synthetic-user',requestId,orderId:id,status:'manual_hold',checkoutUrl:null}; }
    return path.endsWith('/orders') ? orderList(submitted ? [held] : []) : account({checkoutAvailable:true});
  });
  await mutation.controller.load();await mutation.controller.checkout();await mutation.controller.checkout();
  assert.equal(posts,1);assert.equal(mutation.opened.length,0);assert.equal(paymentView(mutation).showBuy,false);
});

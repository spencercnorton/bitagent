'use strict';

const test = require('node:test');
const assert = require('node:assert/strict');
const {PREFERENCE_DEFAULTS, normalizeLibraryPreferences, preferenceCacheKey, applyBrowseDefaultsFor,
  accountUsageViewFor, acceptAccountUsageSnapshot, createPreferenceStore} = require('../static/js/library-preferences.js');
const {parseBrowseState, browseParamsFor} = require('../static/js/library.js');

function usage(overrides = {}) {
  return {accountId:'alice',available:true,status:'available',revision:3,observedAt:'2026-09-30T12:00:00Z',
    trackingSince:1790769600.5,grabs:3,apiSearches:4,magnetCopies:1,magnetOpens:1,magnetExports:1,
    downloadedBytes:null,uploadedBytes:null,hitAndRuns:null,...overrides};
}
function envelope(settings, revision = 1, accountId = 'alice') {
  return {accountId,schemaVersion:1,revision,settings:normalizeLibraryPreferences(settings)};
}
function storage() {
  const values = new Map();
  return {values,getItem:key => values.get(key) || null,setItem:(key,value) => values.set(key,value)};
}
function deferredStore(options = {}) {
  const requests = [], changes = [], statuses = [], cache = storage();
  const store = createPreferenceStore({ownerId:'alice',authenticated:true,storage:cache,
    request:(patch,ownerId) => new Promise((resolve,reject) => requests.push({patch,ownerId,resolve,reject})),
    onChange:settings => changes.push(settings),onStatus:status => statuses.push(status),...options});
  return {store,requests,changes,statuses,cache};
}

test('preferences accept only supported enums and real booleans without mutating the input', () => {
  const input = Object.freeze({theme:'light',motion:'reduced',density:'compact',region:'GB',contentType:'tv_show',
    sort:'newest',quality:'V1080p',matchedOnly:false,englishOnly:true,magnetMode:'all',unknown:'ignored'});
  const result = normalizeLibraryPreferences(input);
  const {unknown,...known} = input;
  assert.deepEqual(result, {...PREFERENCE_DEFAULTS,...known}); assert.equal(Object.hasOwn(result,'unknown'),false);
});

test('malformed preference records fall back to strict defaults and discard unknown fields', () => {
  for (const input of [null,[],true,'dark',42,{theme:'electric',motion:'full',density:'tiny',region:'USA',
    contentType:'torrent',sort:'popularity',quality:'1080p',matchedOnly:'false',englishOnly:1,magnetMode:'random'}]) {
    assert.deepEqual(normalizeLibraryPreferences(input), {...PREFERENCE_DEFAULTS});
  }
  assert.equal(Object.hasOwn(normalizeLibraryPreferences({unexpected:{nested:true}}),'unexpected'), false);
});

test('bare visits apply saved browse defaults with independent mutable facet Sets', () => {
  const original = parseBrowseState(''); original.genres.add('28'); original.sources.add('tmdb'); original.features.add('anime');
  const before = browseParamsFor(original).toString();
  const saved = {contentType:'movie',sort:'name',quality:'V2160p',matchedOnly:false,englishOnly:true,region:'GB'};
  const result = applyBrowseDefaultsFor(original,saved,'');
  assert.equal(result.type,'movie'); assert.equal(result.sort,'name'); assert.equal(result.region,'GB');
  assert.deepEqual([...result.qualities],['V2160p']); assert.equal(result.hideUnmatched,false); assert.equal(result.hideForeign,true);
  for (const key of ['genres','qualities','sources','features']) {
    assert.notEqual(result[key],original[key]); result[key].add('new selection'); assert.ok(!original[key].has('new selection'));
  }
  assert.equal(browseParamsFor(original).toString(),before);
});

test('every explicit permalink retains its parsed decisions instead of acquiring saved filters', () => {
  const saved = {contentType:'movie',sort:'name',quality:'V2160p',matchedOnly:false,englishOnly:true,region:'GB'};
  for (const search of ['?q=Synthetic','?provider=531&region=US','?network=49&page=3',
    '?type=&sort=seeders&quality=','?q=&genres=28&features=hdr&unmatched=0']) {
    const original = parseBrowseState(search), before = browseParamsFor(original).toString();
    const result = applyBrowseDefaultsFor(original,saved,search);
    assert.equal(browseParamsFor(result).toString(),before,search);
    assert.notEqual(result.genres,original.genres); assert.notEqual(result.qualities,original.qualities);
  }
});

test('preference cache keys separate users and anonymous defaults without interpreting identifier characters', () => {
  const alice = preferenceCacheKey('alice'), bob = preferenceCacheKey('bob');
  assert.notEqual(alice,bob); assert.notEqual(alice,preferenceCacheKey(null));
  assert.equal(preferenceCacheKey(null),preferenceCacheKey(''));
  assert.notEqual(preferenceCacheKey('alice/bob'),preferenceCacheKey('alice%2Fbob'));
});

test('usage validates complete owner-bound counter snapshots and rejects contradictory availability', () => {
  assert.ok(accountUsageViewFor(usage()));
  for (const invalid of [null,usage({accountId:''}),usage({accountId:7}),usage({revision:1.5}),
    usage({observedAt:'not a timestamp'}),usage({observedAt:'2026-09-30T12:00:00+02:00'}),usage({trackingSince:null}),
    usage({trackingSince:0}),usage({grabs:2}),usage({magnetCopies:'1'}),usage({magnetExports:-1}),
    usage({apiSearches:Number.MAX_SAFE_INTEGER+1}),usage({available:false}),usage({available:'yes'}),usage({status:'unavailable'})]) {
    assert.equal(accountUsageViewFor(invalid),null,JSON.stringify(invalid));
  }
});

test('zero activity stays measured while absent and malformed transfer figures stay unreported', () => {
  const zero = usage({grabs:0,apiSearches:0,magnetCopies:0,magnetOpens:0,magnetExports:0});
  const view = accountUsageViewFor(zero);
  assert.equal(view.grabs,'0'); assert.equal(view.searches,'0'); assert.equal(view.copies,'0');
  assert.equal(view.downloaded,'Not reported'); assert.equal(view.uploaded,'Not reported'); assert.equal(view.ratio,'—');
  const measured = accountUsageViewFor({...zero,downloadedBytes:0,uploadedBytes:0,hitAndRuns:0});
  assert.equal(measured.downloaded,'0 B'); assert.equal(measured.hitAndRuns,'0'); assert.equal(measured.ratio,'—');
  const transfers = accountUsageViewFor(usage({downloadedBytes:1024,uploadedBytes:2560}));
  assert.equal(transfers.ratio,'2.50');
  assert.equal(accountUsageViewFor(usage({downloadedBytes:-1,uploadedBytes:'1024'})).downloaded,'Not reported');
});

test('usage snapshots reject old revisions and another owner without independently maximizing counters', () => {
  const current = usage({revision:10,grabs:10,magnetCopies:8,magnetOpens:1,magnetExports:1});
  assert.equal(acceptAccountUsageSnapshot(current,usage({revision:9}), 'alice'),current);
  assert.equal(acceptAccountUsageSnapshot(current,usage({accountId:'bob',revision:99}), 'alice'),current);
  const fresh = usage({revision:11,grabs:11,apiSearches:2,magnetCopies:9,magnetOpens:1,magnetExports:1});
  assert.equal(acceptAccountUsageSnapshot(current,fresh,'alice'),fresh);
  assert.equal(accountUsageViewFor(fresh).searches,'2','the validated authoritative snapshot stays coherent instead of mixing fields from two revisions');
  assert.equal(acceptAccountUsageSnapshot(null,usage({accountId:'bob'}),'alice'),null);
});

test('equal-revision observations can age period totals but an older observation cannot replace them', () => {
  const old = usage({observedAt:'2026-09-30T12:00:00Z',periods:{last7Days:{grabs:3}}});
  const fresh = usage({observedAt:'2026-10-01T00:00:00Z',periods:{last7Days:{grabs:1}}});
  assert.equal(acceptAccountUsageSnapshot(old,fresh,'alice'),fresh);
  assert.equal(acceptAccountUsageSnapshot(fresh,old,'alice'),fresh);
});

test('a new tracking epoch may reset revisions while a previous epoch cannot reappear afterward', () => {
  const old = usage({revision:10}), fresh = usage({revision:0,trackingSince:1790812800,observedAt:'2026-10-01T00:00:00Z',
    grabs:0,apiSearches:0,magnetCopies:0,magnetOpens:0,magnetExports:0});
  assert.equal(acceptAccountUsageSnapshot(old,fresh,'alice'),fresh);
  assert.equal(acceptAccountUsageSnapshot(fresh,old,'alice'),fresh);
});

test('owner-specific caches and current schema load independently without borrowing another account settings', () => {
  const cache = storage(); cache.setItem(preferenceCacheKey('alice'),JSON.stringify(envelope({theme:'light'},4)));
  cache.setItem(preferenceCacheKey('bob'),JSON.stringify(envelope({theme:'dark'},5,'bob')));
  const alice = createPreferenceStore({ownerId:'alice',storage:cache}), bob = createPreferenceStore({ownerId:'bob',storage:cache});
  assert.equal(alice.get().theme,'light'); assert.equal(bob.get().theme,'dark');
  cache.setItem(preferenceCacheKey('third'),JSON.stringify({...envelope({theme:'light'}),schemaVersion:99}));
  assert.deepEqual(createPreferenceStore({ownerId:'third',storage:cache}).get(),{...PREFERENCE_DEFAULTS});
  cache.setItem(preferenceCacheKey('fourth'),'{broken json');
  assert.deepEqual(createPreferenceStore({ownerId:'fourth',storage:cache}).get(),{...PREFERENCE_DEFAULTS});
});

test('newer preference changes survive an older acknowledgement and synchronize as the next patch', async () => {
  const f = deferredStore();
  const first = f.store.set({theme:'dark'}), second = f.store.set({theme:'light',density:'compact'});
  assert.equal(f.requests.length,1); assert.equal(f.store.get().theme,'light');
  f.requests[0].resolve({ok:true,data:envelope({theme:'dark'},1)});
  await new Promise(resolve => setImmediate(resolve));
  assert.equal(f.requests.length,2); assert.equal(f.store.get().theme,'light'); assert.equal(f.store.get().density,'compact');
  assert.deepEqual(f.requests[1].patch,{theme:'light',density:'compact'}); assert.equal(f.requests[1].ownerId,'alice');
  f.requests[1].resolve({ok:true,data:envelope({theme:'light',density:'compact'},2)}); await Promise.all([first,second]);
  assert.equal(f.store.isPending(),false); assert.match(f.statuses.at(-1),/saved to your account/i);
});

test('a failed save remains pending and retry sends the same owner and patch once', async () => {
  const f = deferredStore(), pending = f.store.set({matchedOnly:false});
  f.requests[0].resolve({ok:false}); await pending;
  assert.equal(f.store.isPending(),true); assert.equal(f.requests.length,1); assert.equal(f.store.get().matchedOnly,false);
  assert.match(f.statuses.at(-1),/retry/i);
  const retry = f.store.retry(); assert.equal(f.requests.length,2);
  assert.deepEqual(f.requests[1].patch,f.requests[0].patch); assert.equal(f.requests[1].ownerId,'alice');
  f.requests[1].resolve({ok:true,data:envelope({matchedOnly:false},1)}); await retry;
  assert.equal(f.store.isPending(),false);
});

test('wrong-owner acknowledgements cannot clear an unsynchronized preference or poison its cache', async () => {
  const f = deferredStore(), pending = f.store.set({theme:'light'});
  f.requests[0].resolve({ok:true,data:envelope({theme:'dark'},1,'bob')}); await pending;
  assert.equal(f.store.get().theme,'light'); assert.equal(f.store.isPending(),true);
  const cached = JSON.parse(f.cache.getItem(preferenceCacheKey('alice'))); assert.equal(cached.settings.theme,'light');
  const retry = f.store.retry(); assert.equal(f.requests.length,2);
  f.requests[1].resolve({ok:true,data:envelope({theme:'light'},1)}); await retry;
  assert.equal(f.store.isPending(),false);
});

test('a thrown transport failure releases the in-flight save so a later retry can succeed', async () => {
  const f = deferredStore(), pending = f.store.set({sort:'newest'});
  f.requests[0].reject(new Error('Synthetic offline transport')); await pending;
  assert.equal(f.store.isPending(),true); assert.match(f.statuses.at(-1),/retry|unavailable/i);
  const retry = f.store.retry(); assert.equal(f.requests.length,2);
  f.requests[1].resolve({ok:true,data:envelope({sort:'newest'},1)}); await retry;
  assert.equal(f.store.isPending(),false);
});

test('disposing an old account store prevents its late acknowledgement from rendering or writing', async () => {
  const f = deferredStore(), pending = f.store.set({theme:'light'});
  const notifications = f.changes.length, cached = f.cache.getItem(preferenceCacheKey('alice'));
  f.store.dispose(); f.requests[0].resolve({ok:true,data:envelope({theme:'dark'},10)}); await pending;
  assert.equal(f.changes.length,notifications); assert.equal(f.cache.getItem(preferenceCacheKey('alice')),cached);
  assert.equal(f.store.ingest(envelope({theme:'dark'},11)),false);
  await f.store.retry(); assert.equal(f.requests.length,1);
});

test('unavailable local storage keeps preferences effective for this session and never claims they were persisted', async () => {
  const statuses = [], blocked = {getItem() { throw new Error('Synthetic blocked storage'); },setItem() { throw new Error('Synthetic quota exceeded'); }};
  const store = createPreferenceStore({ownerId:'guest',storage:blocked,onStatus:status => statuses.push(status)});
  assert.deepEqual(store.get(),{...PREFERENCE_DEFAULTS});
  await store.set({theme:'light',motion:'reduced'});
  assert.equal(store.get().theme,'light'); assert.equal(store.get().motion,'reduced');
  assert.match(statuses.at(-1),/session|not saved|unavailable/i);
  assert.doesNotMatch(statuses.at(-1),/^saved in this browser$/i);
});

test('reset persists the defaults as real changed fields while get returns an isolated settings object', async () => {
  const f = deferredStore({authenticated:false}); await f.store.set({theme:'light',matchedOnly:false,magnetMode:'all'});
  const detached = f.store.get(); detached.theme = 'dark'; assert.equal(f.store.get().theme,'light');
  await f.store.reset(); assert.deepEqual(f.store.get(),{...PREFERENCE_DEFAULTS});
  assert.deepEqual(JSON.parse(f.cache.getItem(preferenceCacheKey('alice'))).settings,{...PREFERENCE_DEFAULTS});
});

test('unchanged preferences and a reset at defaults finish without a request or a misleading saving status', async () => {
  const f=deferredStore();
  await f.store.set({theme:'system'}); await f.store.reset(); await f.store.retry();
  assert.equal(f.requests.length,0); assert.equal(f.store.isPending(),false);
  assert.equal(f.statuses.at(-1),'No changes to save'); assert.ok(!f.statuses.includes('Saving…'));
  const changed=f.store.set({theme:'light'}); f.requests[0].resolve({ok:true,data:envelope({theme:'light'},1)}); await changed;
  await f.store.set({theme:'light'}); await f.store.retry();
  assert.equal(f.requests.length,1); assert.equal(f.store.isPending(),false);
  assert.equal(f.statuses.at(-1),'No changes to save'); assert.equal(f.store.get().theme,'light');
});

test('an unsynchronized preference survives a restart and is retried without overwriting its newer local value', async () => {
  const first=deferredStore(), pending=first.store.set({theme:'light',matchedOnly:false});
  first.requests[0].resolve({ok:false}); await pending; first.store.dispose();
  const next=deferredStore({storage:first.cache});
  assert.equal(next.store.isPending(),true); assert.equal(next.store.get().theme,'light');
  assert.equal(next.store.ingest(envelope({theme:'dark'},1)),true);
  assert.equal(next.store.get().theme,'light'); assert.equal(next.store.get().matchedOnly,false);
  const retry=next.store.retry(); assert.deepEqual(next.requests[0].patch,{theme:'light',matchedOnly:false});
  next.requests[0].resolve({ok:true,data:envelope({theme:'light',matchedOnly:false},2)}); await retry;
  assert.equal(next.store.isPending(),false);
});

test('a malformed or stale preference envelope keeps valid local edits pending instead of claiming success', async () => {
  for(const invalid of [{...envelope({theme:'light'}),schemaVersion:99},
    {...envelope({theme:'light'}),settings:{theme:'light'}},
    {...envelope({theme:'light'}),settings:{...PREFERENCE_DEFAULTS,theme:'electric'}},envelope({theme:'light'},3)]) {
    const f=deferredStore(); f.store.ingest(envelope({},4)); const pending=f.store.set({theme:'light'});
    f.requests[0].resolve({ok:true,data:invalid}); await pending;
    assert.equal(f.store.get().theme,'light'); assert.equal(f.store.isPending(),true);
    assert.match(f.statuses.at(-1),/retry/i);
  }
});

test('an unavailable anonymous account response preserves saved browser-only preferences', async () => {
  const f=deferredStore({ownerId:'anonymous',authenticated:false}); await f.store.set({theme:'light',density:'compact'});
  assert.equal(f.store.ingest({...envelope({},0,'anonymous'),available:false}),false);
  assert.equal(f.store.get().theme,'light'); assert.equal(f.store.get().density,'compact'); assert.equal(f.requests.length,0);
});

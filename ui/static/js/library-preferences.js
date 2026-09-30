/* Account-scoped library preferences and validated activity snapshots. */
'use strict';
(function (root) {
  const PREFERENCE_DEFAULTS = Object.freeze({theme:'system', motion:'system', density:'comfortable', region:'US', contentType:'', sort:'seeders', quality:'', matchedOnly:true, englishOnly:false, magnetMode:'best'});
  const choices = {theme:['system','light','dark'], motion:['system','reduced'], density:['comfortable','compact'], contentType:['','movie','tv_show'], sort:['seeders','newest','name','size'], quality:['','V2160p','V1080p','V720p'], magnetMode:['best','all']};
  function normalizeLibraryPreferences(settings) {
    const result = {...PREFERENCE_DEFAULTS};
    if (!settings || typeof settings !== 'object' || Array.isArray(settings)) return result;
    for (const [key, allowed] of Object.entries(choices)) if (allowed.includes(settings[key])) result[key] = settings[key];
    if (typeof settings.region === 'string' && /^[A-Z]{2}$/.test(settings.region)) result.region = settings.region;
    for (const key of ['matchedOnly','englishOnly']) if (typeof settings[key] === 'boolean') result[key] = settings[key];
    return result;
  }
  function preferenceCacheKey(ownerId) { return 'bitagent-library-preferences:v1:' + encodeURIComponent(typeof ownerId === 'string' && ownerId ? ownerId : 'guest'); }
  function applyBrowseDefaultsFor(state, settings, search) {
    const result = {...state};
    for (const key of ['genres','qualities','sources','features']) if (state[key] instanceof Set) result[key] = new Set(state[key]);
    // A permalink is an explicit browsing decision, including its omitted defaults.
    if (new URLSearchParams(search || '').size) return result;
    const p = normalizeLibraryPreferences(settings);
    return {...result, type:p.contentType, sort:p.sort, qualities:new Set(p.quality ? [p.quality] : []), hideUnmatched:p.matchedOnly, hideForeign:p.englishOnly, region:p.region};
  }
  const validCount = n => Number.isSafeInteger(n) && n >= 0;
  const utc = s => typeof s === 'string' && /(?:Z|\+00:00)$/i.test(s) && Number.isFinite(Date.parse(s));
  function validUsage(u) {
    return !!u && u.available === true && u.status === 'available' && typeof u.accountId === 'string' && !!u.accountId && validCount(u.revision) && utc(u.observedAt) && Number.isFinite(u.trackingSince) && u.trackingSince > 0 && u.trackingSince < 8640000000000 && ['grabs','apiSearches','magnetCopies','magnetOpens','magnetExports'].every(k=>validCount(u[k])) && u.grabs === u.magnetCopies + u.magnetOpens + u.magnetExports;
  }
  function acceptAccountUsageSnapshot(current, incoming, ownerId) {
    if (!validUsage(incoming) || incoming.accountId !== ownerId) return current || null;
    if (current && current.accountId === ownerId) {
      if (incoming.trackingSince < current.trackingSince) return current;
      if (incoming.trackingSince === current.trackingSince && (incoming.revision < current.revision || (incoming.revision === current.revision && Date.parse(incoming.observedAt) < Date.parse(current.observedAt)))) return current;
    }
    return incoming;
  }
  function accountUsageViewFor(u) {
    if (!validUsage(u)) return null;
    const number = n => n.toLocaleString('en-US');
    const bytes = n => { if (!validCount(n)) return 'Not reported'; if (!n) return '0 B'; const units=['B','KB','MB','GB','TB','PB']; const i=Math.min(Math.floor(Math.log(n)/Math.log(1024)),units.length-1); return (n/Math.pow(1024,i)).toFixed(i?1:0)+' '+units[i]; };
    return {downloaded:bytes(u.downloadedBytes), uploaded:bytes(u.uploadedBytes), grabs:number(u.grabs), searches:number(u.apiSearches), copies:number(u.magnetCopies), opens:number(u.magnetOpens), exports:number(u.magnetExports), hitAndRuns:validCount(u.hitAndRuns)?number(u.hitAndRuns):'Not reported', ratio:validCount(u.uploadedBytes)&&validCount(u.downloadedBytes)&&u.downloadedBytes>0?(u.uploadedBytes/u.downloadedBytes).toFixed(2):'—', hint:'Magnet actions count links copied, opened, or exported; repeated actions count again. API searches count successful Torznab search responses, including empty results. Transfer and seeding figures require reliable client reporting.'};
  }
  function createPreferenceStore(options) {
    const ownerId = options.ownerId || 'guest', authenticated = !!options.authenticated;
    let settings = {...PREFERENCE_DEFAULTS}, revision = 0, pending = {}, active = true, saving = null, cached = false;
    const notify = status => { if (!active) return; options.onChange?.({...settings}); options.onStatus?.(status); };
    const cache = () => { cached=false; try { if(options.storage) { options.storage.setItem(preferenceCacheKey(ownerId), JSON.stringify({schemaVersion:1,revision,settings,pending})); cached=true; } } catch (_) {} };
    const localStatus = () => cached ? 'Saved in this browser' : 'Applied for this visit; browser storage unavailable';
    const validEnvelope = envelope => {
      if(!envelope || envelope.schemaVersion!==1 || envelope.accountId!==ownerId || !validCount(envelope.revision) || envelope.revision<revision || !envelope.settings || Array.isArray(envelope.settings))return false;
      const normalized=normalizeLibraryPreferences(envelope.settings);
      return Object.keys(PREFERENCE_DEFAULTS).every(key=>Object.hasOwn(envelope.settings,key) && envelope.settings[key]===normalized[key]);
    };
    try { const cached=JSON.parse(options.storage?.getItem(preferenceCacheKey(ownerId)) || 'null'); if(cached?.schemaVersion===1) {settings=normalizeLibraryPreferences(cached.settings); revision=validCount(cached.revision)?cached.revision:0; if(cached.pending && typeof cached.pending==='object') { const normalized=normalizeLibraryPreferences(cached.pending); for(const key of Object.keys(PREFERENCE_DEFAULTS)) if(Object.hasOwn(cached.pending,key) && normalized[key]===cached.pending[key])pending[key]=normalized[key]; }} } catch (_) {}
    function ingest(envelope) {
      if (!active || (!authenticated && envelope?.available===false) || !validEnvelope(envelope)) return false;
      revision=envelope.revision; settings={...normalizeLibraryPreferences(envelope.settings),...pending}; cache(); notify(Object.keys(pending).length?'Account sync pending; retry to sync':authenticated?'Saved to your account':localStatus()); return true;
    }
    async function flush() {
      if (!active || !authenticated || saving || !Object.keys(pending).length) return saving;
      const changes={...pending}; notify('Saving…');
      let succeeded=false;
      saving = (async()=> {
        try {
          const response=await options.request(changes,ownerId);
          if(!active)return;
          if(!response?.ok || !validEnvelope(response.data)) {notify(localStatus()+'. Account sync unavailable; retry to sync.'); return;}
          for(const [key,value] of Object.entries(changes)) if(pending[key]===value) delete pending[key];
          ingest(response.data); succeeded=true;
        } catch (_) { if(active)notify(localStatus()+'. Account sync unavailable; retry to sync.'); }
      })();
      try { await saving; } finally { saving=null; }
      if(active && succeeded && Object.keys(pending).length) return flush();
    }
    function set(changes) {
      const merged=normalizeLibraryPreferences({...settings,...changes});
      for(const key of Object.keys(PREFERENCE_DEFAULTS)) if(merged[key]!==settings[key]) pending[key]=merged[key];
      settings=merged; cache();
      if(!Object.keys(pending).length) {notify(authenticated?'No changes to save':localStatus());return Promise.resolve();}
      notify(authenticated?'Saving…':localStatus());
      if(authenticated) return flush();
      pending={}; cache(); return Promise.resolve();
    }
    return {get:()=>({...settings}), ingest, set, retry:flush, reset:()=>set({...PREFERENCE_DEFAULTS}), dispose:()=>{active=false;}, isPending:()=>Object.keys(pending).length>0};
  }
  const exported={PREFERENCE_DEFAULTS,normalizeLibraryPreferences,preferenceCacheKey,applyBrowseDefaultsFor,accountUsageViewFor,acceptAccountUsageSnapshot,createPreferenceStore};
  if(typeof module!=='undefined'&&module.exports) module.exports=exported;
  root.BitAgentPreferences=exported;
})(typeof window !== 'undefined' ? window : globalThis);

'use strict';

const test = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const vm = require('node:vm');
const source = fs.readFileSync(path.join(__dirname, '../static/js/app.js'), 'utf8');
const aiSource = source.slice(source.indexOf('/* ── AI (LLM cost'), source.indexOf('/* ── System ─'));

function harness(api = async () => null) {
  const nodes = new Map();
  const document = { getElementById(id) {
    if (!nodes.has(id)) nodes.set(id, { textContent: '', innerHTML: '', style: {}, dataset: {}, attributes: {},
      setAttribute(name, value) { this.attributes[name] = value; },
    });
    return nodes.get(id);
  } };
  const escape = value => String(value).replace(/[&<>"']/g, c => ({'&':'&amp;','<':'&lt;','>':'&gt;','"':'&quot;',"'":'&#39;'}[c]));
  const context = vm.createContext({ document, api, console,
    fmtNum: value => value == null ? '—' : Number(value).toLocaleString('en-US'),
    escHtml: escape, installHelpDots: () => {}, _observationTime: value => value ? '12:00:00 PM' : '',
    _setViewStatus(id, state, label, detail) { const el = document.getElementById(id); el.dataset.state = state; el.textContent = label; el.title = detail; },
    _blankStats(pairs) { for (const [id, value] of Object.entries(pairs)) document.getElementById(id).textContent = value; },
    _renderMeter(id, value, tone) { Object.assign(document.getElementById(id).dataset, {value,tone}); },
    _toneForRatio: value => value == null ? 'neutral' : 'good',
  });
  vm.runInContext(`let _aiLoadSeq = 0;\n${aiSource}\nglobalThis.ai = {loadAiTab,renderAiSummary,renderAiSpend,renderLlmStages};`, context);
  return { ai: context.ai, node: id => document.getElementById(id) };
}

const telemetry = {status:'ok',source:'synthetic.metrics',observedAt:'2026-10-01T12:00:00Z',stale:false,error:null};
function summary(overrides = {}) {
  return { available:true, telemetryAvailable:true, telemetry,
    stages:[], spend:{available:false},
    matches:{live:{total:null,byType:{}}}, cache:{hits:null,misses:null,hitRatio:null},
    gateRejects:{}, anime:{kept:null,rejected:null,byEnglish:{}},
    familyPresence:{matches:false,cacheHits:false,cacheMisses:false,gateRejects:false,anime:false}, ...overrides };
}
function spend(overrides = {}) {
  return {available:true,totalUsd:0.4,totalIsPartial:false,monthlyUsd:12,monthlyIsPartial:false,
    monthlyWindowSeconds:20,pricesAsOf:'2026-01-01',byModel:[{model:'synthetic-model',stage:'matcher',priced:true,usd:0.4,inputTokens:200,outputTokens:50,monthlyUsd:12}],...overrides};
}

test('unavailable telemetry says unknown usage rather than asserting absent token counters', () => {
  const h = harness();
  h.ai.renderAiSummary(summary({telemetryAvailable:false,telemetry:{...telemetry,status:'unavailable'},available:false}));
  assert.match(h.node('aiSpendPanel').innerHTML, /could not be read/);
  assert.doesNotMatch(h.node('aiSpendPanel').innerHTML, /No LLM stage is reporting/);
  assert.equal(h.node('aiSpendSub').textContent, 'core telemetry unavailable');
  assert.match(h.node('aiGatePanel').innerHTML, /unavailable/);
  assert.equal(h.node('aiObservationTime').textContent, 'No valid observation');
});

test('a failed refresh clears previous cost, cache, attach and detail displays', () => {
  const h = harness();
  h.ai.renderAiSummary(summary({spend:spend(),matches:{live:{total:5,byType:{movie:5}}},cache:{hits:9,misses:1,hitRatio:0.9},gateRejects:{privacy:4}}));
  assert.equal(h.node('aiSpendTotal').textContent,'$0.40');
  assert.equal(h.node('aiCacheRatio').textContent,'90.0%');
  h.ai.renderAiSummary(null);
  for (const id of ['aiSpendMonthly','aiSpendTotal','aiTokensTotal','aiModelCount','aiLiveMatches','aiCacheRatio']) assert.equal(h.node(id).textContent,'—',id);
  assert.equal(h.node('aiCacheMeter').dataset.tone,'neutral');
  assert.equal(h.node('aiSpendPrices').textContent,'list-price estimate');
  assert.doesNotMatch(h.node('aiGatePanel').innerHTML, /privacy/);
  assert.match(h.node('aiStageGrid').innerHTML,/Couldn’t load/);
});

test('healthy core without AI families is distinct from a source failure', () => {
  const h = harness();
  h.ai.renderAiSummary(summary({available:false}));
  assert.equal(h.node('aiObservationState').dataset.state,'fresh');
  assert.match(h.node('aiObservationTime').textContent,/Observed/);
  assert.match(h.node('aiSpendPanel').innerHTML,/does not report token usage/);
  assert.match(h.node('aiGatePanel').innerHTML,/not reported/);
});

test('unsupported matcher families do not invent zero attaches or no-rejection observations', () => {
  const h = harness();
  h.ai.renderAiSummary(summary());
  assert.equal(h.node('aiLiveMatches').textContent,'—');
  assert.match(h.node('aiLiveMatchesSub').textContent,/not reported/);
  assert.match(h.node('aiCacheSub').textContent,/incomplete counters/);
  assert.match(h.node('aiGatePanel').innerHTML,/not reported/);
  assert.match(h.node('aiAnimePanel').innerHTML,/not reported/);
});

test('observed zero counters stay zero while a zero-denominator ratio stays unmeasured', () => {
  const h = harness();
  h.ai.renderAiSummary(summary({matches:{live:{total:0,byType:{}}},cache:{hits:0,misses:0,hitRatio:null}}));
  assert.equal(h.node('aiLiveMatches').textContent,'0');
  assert.equal(h.node('aiCacheRatio').textContent,'—');
  assert.equal(h.node('aiCacheSub').textContent,'0 hits / 0 misses · no measured ratio');
  assert.equal(h.node('aiCacheMeter').dataset.tone,'neutral');
});

test('an incomplete cache family cannot be converted into a measured hit ratio', () => {
  const h = harness();
  h.ai.renderAiSummary(summary({cache:{hits:null,misses:2,hitRatio:null}}));
  assert.equal(h.node('aiCacheRatio').textContent,'—');
  assert.equal(h.node('aiCacheSub').textContent,'— hits / 2 misses · incomplete counters');
});

test('one model observed in multiple stages is counted once and stages stay visible', () => {
  const h = harness();
  h.ai.renderAiSpend(spend({byModel:[
    {model:'synthetic-model',stage:'matcher',priced:true,usd:0.2,inputTokens:100,outputTokens:10},
    {model:'synthetic-model',stage:'junkpurge',priced:true,usd:0.2,inputTokens:100,outputTokens:40},
  ]}));
  assert.equal(h.node('aiModelCount').textContent,'1');
  assert.equal(h.node('aiModelSub').textContent,'2 reporting stages');
  assert.match(h.node('aiSpendPanel').innerHTML,/matcher/);
  assert.match(h.node('aiSpendPanel').innerHTML,/junkpurge/);
});

test('unpriced observed tokens display unknown cost and projection without a zero-cost floor', () => {
  const h = harness();
  h.ai.renderAiSpend(spend({totalUsd:null,totalIsPartial:true,monthlyUsd:null,unpricedModels:['unknown-model'],byModel:[{model:'unknown-model',stage:'junkpurge',priced:false,usd:null,inputTokens:90,outputTokens:20}]}));
  assert.equal(h.node('aiTokensTotal').textContent,'110');
  assert.equal(h.node('aiSpendTotal').textContent,'—');
  assert.equal(h.node('aiSpendMonthly').textContent,'—');
  assert.equal(h.node('aiSpendSub').textContent,'no model price available');
  assert.match(h.node('aiSpendPanel').innerHTML,/no price on file/);
});

test('multiple models in one stage do not inflate the reporting-stage count', () => {
  const h = harness();
  h.ai.renderAiSpend(spend({byModel:[
    {model:'first-model',stage:'matcher',priced:true,usd:0.2,inputTokens:100,outputTokens:10},
    {model:'second-model',stage:'matcher',priced:true,usd:0.2,inputTokens:100,outputTokens:40},
  ]}));
  assert.equal(h.node('aiModelCount').textContent,'2');
  assert.equal(h.node('aiModelSub').textContent,'1 reporting stage');
});

test('a missing token direction remains unknown while the observed sum is a lower bound', () => {
  const h = harness();
  h.ai.renderAiSpend(spend({totalIsPartial:true,byModel:[{model:'synthetic-model',stage:'filter',priced:true,usd:0.1,inputTokens:null,outputTokens:20,usageIncomplete:true}]}));
  assert.equal(h.node('aiTokensTotal').textContent,'≥ 20');
  assert.equal(h.node('aiTokensSub').textContent,'— in / 20 out · incomplete coverage');
});

test('unknown token directions never become a measured zero total', () => {
  const h = harness();
  h.ai.renderAiSpend(spend({byModel:[{model:'unknown-model',stage:'filter',priced:false,usd:null,inputTokens:null,outputTokens:null}]}));
  assert.equal(h.node('aiTokensTotal').textContent,'—');
  assert.equal(h.node('aiTokensSub').textContent,'— in / — out · incomplete coverage');
});

test('a known model price with missing usage is distinguished from an unknown price', () => {
  const h = harness();
  h.ai.renderAiSpend(spend({totalUsd:null,monthlyUsd:null,byModel:[{model:'known-model',stage:'filter',priced:false,priceAvailable:true,usd:null,inputTokens:null,outputTokens:null}]}));
  assert.equal(h.node('aiSpendSub').textContent,'usage accounting incomplete');
  assert.match(h.node('aiSpendPanel').innerHTML,/usage accounting incomplete/);
  assert.doesNotMatch(h.node('aiSpendPanel').innerHTML,/no price on file/);
});

test('partial estimates retain a neutral projection and identify unpriced models', () => {
  const h = harness();
  h.ai.renderAiSpend(spend({totalIsPartial:true,monthlyIsPartial:true,monthlyMeasuring:['filter'],unpricedModels:['unknown-model']}));
  assert.equal(h.node('aiSpendTotal').textContent,'≥ $0.40');
  assert.match(h.node('aiSpendTotalSub').textContent,/partial estimate; unpriced: unknown-model/);
  assert.match(h.node('aiSpendSub').textContent,/partial projection/);
  assert.equal(h.node('aiSpendMeter').dataset.tone,'neutral');
});

test('short sampling windows are described in seconds and projections are labeled as such', () => {
  const h = harness();
  h.ai.renderAiSpend(spend());
  assert.match(h.node('aiSpendSub').textContent,/sampled 20s; rate × 30 days/);
  assert.doesNotMatch(h.node('aiSpendSub').textContent,/0 min/);
});

test('stage metrics and model labels escape untrusted text', () => {
  const h = harness();
  h.ai.renderLlmStages([{id:'demo',name:'<img>',role:'<script>',model:'<svg>',status:{key:'active',label:'<b>',tone:'info'},metrics:[{label:'<i>',value:0,detail:'<u>'}]}]);
  const html=h.node('aiStageGrid').innerHTML;
  assert.doesNotMatch(html,/<img>|<script>|<svg>|<b>|<i>|<u>/);
  assert.match(html,/&lt;svg&gt;/);
});

test('stage cost and per-work subtotals disclose partial accounting and projection', () => {
  const h = harness();
  h.ai.renderLlmStages([{id:'filter',metrics:[],spend:{priced:true,usd:0.4,monthlyUsd:12,
    usdPerUnit:0.05,unitLabel:'per decision',unitCount:8,totalIsPartial:true,
    monthlyIsPartial:true,usageIncomplete:true,unpricedModels:['<unknown>']}}]);
  const html = h.node('aiStageGrid').innerHTML;
  assert.match(html, /Estimated cost <b>≥ \$0\.40/);
  assert.match(html, /≥ \$12\.00.*rate projection/);
  assert.match(html, /≥ \$0\.0500/);
  assert.match(html, /Partial estimate · usage accounting incomplete; unpriced: &lt;unknown&gt;/);
  assert.doesNotMatch(html, /<unknown>/);
});

test('fractional-cent lower bounds remain legible and identify missing usage per model', () => {
  const h = harness();
  h.ai.renderAiSpend(spend({totalUsd:0.003, totalIsPartial:true, monthlyUsd:0.003,
    monthlyIsPartial:true,byModel:[{model:'known',stage:'filter',priced:true,usd:0.003,
      monthlyUsd:0.003,inputTokens:200,outputTokens:20,usageIncomplete:true}]}));
  assert.equal(h.node('aiSpendTotal').textContent,'≥ $0.0030');
  assert.equal(h.node('aiSpendMonthly').textContent,'≥ $0.0030');
  assert.match(h.node('aiSpendTotalSub').textContent,/usage accounting incomplete/);
  assert.doesNotMatch(h.node('aiSpendPanel').innerHTML,/≥ &lt;|≥ <\$/);
  assert.match(h.node('aiSpendPanel').innerHTML,/≥ \$0\.0030.*partial usage/s);
  h.ai.renderAiSpend(spend({totalUsd:0.00000005,totalIsPartial:true}));
  assert.equal(h.node('aiSpendTotal').textContent,'≥ $5.00e-8');
});

test('an unsampled projection alone does not label complete stage totals as partial', () => {
  const h=harness();
  h.ai.renderLlmStages([{id:'matcher',metrics:[],spend:{priced:true,usd:0.1,
    monthlyUsd:null,totalIsPartial:false,monthlyIsPartial:true}}]);
  assert.match(h.node('aiStageGrid').innerHTML,/measuring rate/);
  assert.doesNotMatch(h.node('aiStageGrid').innerHTML,/Partial estimate/);
});

test('overlapping AI reads keep loading and final observation owned by the latest request', async () => {
  const replies=[];
  const h = harness(() => new Promise(resolve => replies.push(resolve)));
  h.ai.renderAiSummary(summary({spend:spend()}));
  const first=h.ai.loadAiTab();
  const second=h.ai.loadAiTab();
  assert.equal(h.node('tab-ai').attributes['aria-busy'],'true');
  assert.equal(h.node('aiObservationState').dataset.state,'loading');
  assert.equal(h.node('aiSpendTotal').textContent,'—');
  replies[0](null); await first;
  assert.equal(h.node('aiObservationState').dataset.state,'loading');
  replies[1](summary({spend:spend()})); await second;
  assert.equal(h.node('aiObservationState').dataset.state,'fresh');
  assert.equal(h.node('aiSpendTotal').textContent,'$0.40');
  assert.equal(h.node('tab-ai').attributes['aria-busy'],'false');
});

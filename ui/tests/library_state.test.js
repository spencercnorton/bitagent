'use strict';

const test = require('node:test');
const assert = require('node:assert/strict');

const {
  browseParamsFor,
  parseBrowseState,
  historyModeFor,
  isHomeFor,
  sortTransitionFor,
  serverGroupedFor,
  backendAnimeModeFor,
  gridRequestFor,
  facetRequestFor,
  facetKeyFor,
  needsFacetFetchFor,
  applyFeatureFiltersFor,
  groupTorrents,
  rawWindowMetaFor,
  rawResultNoteFor,
  normalizeLibraryStats,
  libraryStatsViewFor,
} = require('../static/js/library.js');

function browseState(overrides = {}) {
  return Object.assign({
    q: '',
    type: '',
    sort: 'seeders',
    genres: new Set(),
    qualities: new Set(),
    sources: new Set(),
    features: new Set(),
    yearMin: '',
    yearMax: '',
    hideUnmatched: true,
    hideForeign: false,
    page: 0,
    pageSize: 60,
    facetsKey: '',
  }, overrides);
}

test('browse URL round-trips release controls and paging', () => {
  const original = browseState({
    q: 'matrix & neo',
    type: 'movie',
    sort: 'name',
    genres: new Set(['tmdb:28', 'tmdb:878']),
    qualities: new Set(['V2160p']),
    sources: new Set(['WEBDL']),
    features: new Set(['hdr']),
    yearMin: '1990',
    yearMax: '2025',
    hideUnmatched: false,
    hideForeign: true,
    page: 2,
  });

  const params = browseParamsFor(original);
  assert.equal(params.get('english'), '1');
  assert.equal(params.get('unmatched'), '1');
  assert.equal(params.get('page'), '3');

  const restored = parseBrowseState(`?${params}`);
  assert.equal(restored.q, original.q);
  assert.equal(restored.type, original.type);
  assert.equal(restored.sort, original.sort);
  assert.deepEqual([...restored.genres], [...original.genres]);
  assert.deepEqual([...restored.qualities], [...original.qualities]);
  assert.deepEqual([...restored.sources], [...original.sources]);
  assert.deepEqual([...restored.features], [...original.features]);
  assert.equal(restored.yearMin, '1990');
  assert.equal(restored.yearMax, '2025');
  assert.equal(restored.hideUnmatched, false);
  assert.equal(restored.hideForeign, true);
  assert.equal(restored.page, 2);
});

test('Anime URLs preserve both legacy include-unmatched and explicit matched-only state', () => {
  assert.equal(parseBrowseState('?features=anime').hideUnmatched, false);

  const matchedOnly = browseState({ features: new Set(['anime']), hideUnmatched: true });
  const params = browseParamsFor(matchedOnly);
  assert.equal(params.get('unmatched'), '0');
  assert.equal(parseBrowseState(`?${params}`).hideUnmatched, true);
});

test('server-grouped sort changes reset to page one and require new membership', () => {
  const current = browseState({ page: 4, sort: 'seeders' });
  const transition = sortTransitionFor(current, 'name');

  assert.deepEqual(transition, { sort: 'name', page: 0, mode: 'refetch' });
  assert.equal(current.page, 4, 'transition helper must not mutate the old page');

  const request = gridRequestFor(Object.assign({}, current, transition));
  assert.equal(request.grouped, true);
  assert.equal(request.params.get('order'), 'name');
  assert.equal(request.params.get('offset'), '0');
  assert.equal(request.params.get('group'), 'true');
});

test('technical release-level and unmatched controls select raw-release fallback', () => {
  for (const feature of ['hdr', 'dv', 'atmos', 'remux', 'imax']) {
    const current = browseState({ features: new Set([feature]) });
    const request = gridRequestFor(current);
    assert.equal(serverGroupedFor(current), false, `${feature} must not filter representatives`);
    assert.equal(request.mode, 'raw');
    assert.equal(request.params.has('group'), false);
    assert.equal(request.params.get('limit'), '500');
    assert.equal(request.params.get('order'), 'seeders');
  }

  for (const current of [
    browseState({ hideForeign: true }),
    browseState({ hideUnmatched: false }),
  ]) {
    const request = gridRequestFor(current);
    assert.equal(request.grouped, false);
    assert.equal(request.mode, 'raw');
    assert.equal(request.params.has('group'), false);
  }
});

test('Anime-only preserves the high-recall representative backend path', () => {
  const current = browseState({ features: new Set(['anime']), hideUnmatched: false });
  const request = gridRequestFor(current);

  assert.equal(backendAnimeModeFor(current), true);
  assert.equal(request.mode, 'anime');
  assert.equal(request.backendAnime, true);
  assert.equal(request.params.get('anime'), 'true');
  assert.equal(request.params.get('order'), 'seeders');
});

test('every Anime plus technical-feature combination uses raw rows and both predicates', () => {
  const markers = {
    hdr: 'HDR',
    dv: 'DV',
    atmos: 'Atmos',
    remux: 'REMUX',
    imax: 'IMAX',
  };

  for (const [feature, marker] of Object.entries(markers)) {
    const features = new Set(['anime', feature]);
    const current = browseState({ features, hideUnmatched: false });
    const request = gridRequestFor(current);

    assert.equal(backendAnimeModeFor(current), false, `Anime + ${feature} cannot use representatives`);
    assert.equal(request.mode, 'raw');
    assert.equal(request.backendAnime, false);
    assert.equal(request.params.has('anime'), false);
    assert.equal(request.params.get('content_types'), 'movie,tv_show');

    const both = { name: `[SubsPlease] Example Show S01E01 ${marker}`, releaseGroup: 'SubsPlease' };
    const animeOnly = { name: '[SubsPlease] Example Show S01E02', releaseGroup: 'SubsPlease' };
    const featureOnly = { name: `Ordinary Release 2026 ${marker}`, releaseGroup: 'Scene' };
    assert.deepEqual(
      applyFeatureFiltersFor([both, animeOnly, featureOnly], features, false),
      [both],
      `Anime + ${feature} must apply both client predicates to raw releases`,
    );
  }
});

test('raw fallback sort changes refetch a newly ordered bounded window', () => {
  const current = browseState({ features: new Set(['hdr']), page: 3, sort: 'seeders' });
  const transition = sortTransitionFor(current, 'newest');

  assert.deepEqual(transition, { sort: 'newest', page: 0, mode: 'refetch' });
  for (const sort of ['seeders', 'newest', 'name', 'size']) {
    const request = gridRequestFor(Object.assign({}, current, { sort, page: 0 }));
    assert.equal(request.mode, 'raw');
    assert.equal(request.params.get('order'), sort);
    assert.equal(request.params.get('offset'), '0');
  }
});

test('default and core-native facets keep v1.29 grouped pagination', () => {
  const current = browseState({
    q: 'matrix',
    type: 'movie',
    qualities: new Set(['V2160p']),
    sources: new Set(['WEBDL']),
    genres: new Set(['tmdb:28']),
    page: 1,
  });
  const request = gridRequestFor(current);

  assert.equal(request.grouped, true);
  assert.equal(request.params.get('group'), 'true');
  assert.equal(request.params.get('offset'), '60');
  assert.equal(request.params.get('qualities'), 'V2160p');
  assert.equal(request.params.get('sources'), 'WEBDL');
  assert.equal(request.params.get('genres'), 'tmdb:28');
});

test('every non-default landing control transitions to results', () => {
  assert.equal(isHomeFor(browseState()), true);
  assert.equal(isHomeFor(browseState({ sort: 'newest' })), false);
  assert.equal(isHomeFor(browseState({ hideForeign: true })), false);
  assert.equal(isHomeFor(browseState({ hideUnmatched: false })), false);
  assert.equal(isHomeFor(browseState({ features: new Set(['hdr']) })), false);
});

test('cold grouped page two requests facets separately without slowing the page query', () => {
  const current = browseState({
    q: 'matrix',
    type: 'movie',
    qualities: new Set(['V1080p']),
    page: 1,
  });
  const grid = gridRequestFor(current).params;
  const facets = facetRequestFor(current);

  assert.equal(grid.get('aggregate_facets'), 'false');
  assert.equal(grid.get('offset'), '60');
  assert.equal(facets.get('aggregate_facets'), 'true');
  assert.equal(facets.get('limit'), '1');
  assert.equal(facets.get('q'), grid.get('q'));
  assert.equal(facets.get('qualities'), grid.get('qualities'));
  assert.equal(needsFacetFetchFor(current), true);

  const hydrated = Object.assign({}, current, { facetsKey: facetKeyFor(current) });
  assert.equal(needsFacetFetchFor(hydrated), false);
});

test('every unknown-total raw response is bounded, including short post-filter windows', () => {
  const unknown = rawWindowMetaFor(500, -1);
  assert.equal(unknown.totalReleases, null);
  assert.equal(unknown.limited, true);
  assert.equal(rawWindowMetaFor(500, null).limited, true);

  const shortUnknown = rawWindowMetaFor(137, -1);
  assert.equal(shortUnknown.limited, true);
  assert.equal(rawWindowMetaFor(0, null).limited, true);

  const note = rawResultNoteFor(shortUnknown, 12, 18, 'newest', true);
  assert.match(note, /12 titles/);
  assert.match(note, /18 matching releases/);
  assert.match(note, /bounded 137-release window/);
  assert.match(note, /ordered by newest/);
  assert.match(note, /narrow filters/);
});

test('client filtering and grouping retain the backend first-seen title order', () => {
  const backendOrdered = [
    {
      name: 'Published First 2024 HDR', contentType: 'movie', contentSource: 'tmdb', contentId: '1',
      seeders: 5, discoveredAt: '2024-01-01T00:00:00Z',
    },
    {
      name: 'Published First 2024 Alternate HDR', contentType: 'movie', contentSource: 'tmdb', contentId: '1',
      seeders: 15, discoveredAt: '2025-01-01T00:00:00Z',
    },
    {
      name: 'Published Second 2023 HDR', contentType: 'movie', contentSource: 'tmdb', contentId: '2',
      seeders: 100, discoveredAt: '2030-01-01T00:00:00Z',
    },
  ];
  const filtered = applyFeatureFiltersFor(backendOrdered, new Set(['hdr']), false);
  const groups = groupTorrents(filtered);

  assert.deepEqual(groups.map(group => group.contentId), ['1', '2']);
  assert.equal(groups[0].best.seeders, 15, 'representative selection must not move the first group');
});

test('known raw totals distinguish partial windows from complete release sets', () => {
  const partial = rawWindowMetaFor(500, 1200);
  assert.equal(partial.limited, true);
  assert.match(rawResultNoteFor(partial, 40, 500, 'size', false), /500 of 1,200 releases/);
  assert.equal(rawWindowMetaFor(495, 1200).limited, true, 'post-filter short pages remain partial when total is known');

  const complete = rawWindowMetaFor(500, 500);
  assert.equal(complete.limited, false);
  const note = rawResultNoteFor(complete, 40, 500, 'size', false);
  assert.equal(note, '40 titles · 500 releases');
  assert.doesNotMatch(note, /narrow filters/);
});

test('explicit paging and searches push history while filter resets replace', () => {
  assert.equal(historyModeFor('page'), 'push');
  assert.equal(historyModeFor('search'), 'push');
  assert.equal(historyModeFor('filter'), 'replace');
});

test('library snapshot formats exact and estimated counts without hiding zero', () => {
  const base = {
    totalReleases: 1234567,
    totalReleasesIsEstimate: false,
    releasesAddedLast7Days: 0,
    windowStart: '2026-07-10T12:00:00Z',
    windowEnd: '2026-07-17T12:00:00+00:00',
    observedAt: '2026-07-17T11:59:00Z',
  };

  assert.deepEqual(libraryStatsViewFor(base), {
    totalText: '1,234,567',
    addedText: '0',
    totalHint: 'Indexed releases',
    addedHint: 'Last 7 days',
    statusText: '1,234,567 total torrents and 0 added in the last 7 days.',
  });

  const estimated = libraryStatsViewFor(Object.assign({}, base, { totalReleasesIsEstimate: true }));
  assert.equal(estimated.totalText, '~1,234,567');
  assert.equal(estimated.totalHint, 'Estimated indexed releases');
  assert.match(estimated.statusText, /^Approximately 1,234,567/);
});

test('library snapshot rejects malformed or misleading payloads', () => {
  const valid = {
    totalReleases: 42,
    totalReleasesIsEstimate: false,
    releasesAddedLast7Days: 7,
    windowStart: '2026-07-10T00:00:00Z',
    windowEnd: '2026-07-17T00:00:00Z',
    observedAt: '2026-07-17T00:00:00Z',
  };

  assert.deepEqual(normalizeLibraryStats(valid), valid);
  for (const bad of [
    null,
    Object.assign({}, valid, { totalReleases: -1 }),
    Object.assign({}, valid, { totalReleases: '42' }),
    Object.assign({}, valid, { releasesAddedLast7Days: 1.5 }),
    Object.assign({}, valid, { totalReleasesIsEstimate: 'false' }),
    Object.assign({}, valid, { windowStart: 'not-a-date' }),
    Object.assign({}, valid, { windowStart: '2026-07-18T00:00:00Z' }),
    Object.assign({}, valid, { observedAt: '2026-07-17T00:00:00' }),
  ]) {
    assert.equal(libraryStatsViewFor(bad), null);
  }
});

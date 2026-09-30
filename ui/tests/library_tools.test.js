'use strict';

const test = require('node:test');
const assert = require('node:assert/strict');
const { normalizeInfoHash, magnetFor, dedupeReleases, releaseCoverage,
  selectReleaseGroup, collectTitleReleases } = require('../static/js/library-tools.js');

function release(number, name, seeders = 0) {
  return { infoHash: number.toString(16).padStart(40, '0'), torrentName: name, seeders };
}

test('magnet links validate their hash and escape Unicode titles and URI syntax', () => {
  const row = release(1, 'Synthetic & title 🎬 #1?x=two');
  assert.equal(magnetFor(row), `magnet:?xt=urn:btih:${row.infoHash}&dn=Synthetic%20%26%20title%20%F0%9F%8E%AC%20%231%3Fx%3Dtwo`);
  assert.equal(magnetFor({ infoHash: 'javascript:alert(1)', name: 'Unsafe' }), '');
  assert.equal(magnetFor({ infoHash: `${row.infoHash}&tr=https://evil.invalid`, name: 'Unsafe' }), '');
  assert.equal(magnetFor({ infoHash: row.infoHash, name: '\uD800' }).endsWith('dn=%EF%BF%BD'), true);
  assert.equal(magnetFor(null), '');
});

test('base32 and mixed-case hex hashes share one canonical identity', () => {
  const hex = '0123456789abcdef0123456789abcdef01234567';
  const base32 = 'AERUKZ4JVPG66AJDIVTYTK6N54ASGRLH';
  assert.equal(normalizeInfoHash(base32), hex);
  assert.equal(normalizeInfoHash(` ${hex.toUpperCase()} `), hex);
  assert.equal(normalizeInfoHash('a'.repeat(64)), '');
  assert.equal(dedupeReleases([{ infoHash: hex }, { infoHash: base32 }, { infoHash: 'bad' }]).length, 1);
});

test('season selection prefers one pack and excludes other seasons and whole-show packs', () => {
  const rows = [release(1, 'Synthetic S01E01', 80), release(2, 'Synthetic S01 pack', 10),
    release(3, 'Synthetic S01 pack 2160p', 20), release(4, 'Synthetic S02E01', 90),
    release(5, 'Synthetic Complete Series', 200), release(6, 'Synthetic unclassified', 250)];
  assert.deepEqual(selectReleaseGroup(rows, { scope: 'season', season: 1 }).map(row => row.infoHash), [rows[2].infoHash]);
  assert.equal(selectReleaseGroup(rows, { scope: 'season', season: 1, mode: 'all' }).length, 3);
  assert.throws(() => selectReleaseGroup(rows, { scope: 'season' }), /valid season/);
});

test('best show prefers packs and avoids redundant versions when coverage permits', () => {
  const rows = [release(1, 'Synthetic S01-S03', 20), release(2, 'Synthetic S02 pack', 100),
    release(3, 'Synthetic S03E02', 60), release(4, 'Synthetic S04E01-E03', 10),
    release(5, 'Synthetic S04E02', 200), release(6, 'Synthetic S04E04', 10),
    release(7, 'Synthetic S04E04 2160p', 30), release(8, 'Synthetic ambiguous', 300)];
  const selected = selectReleaseGroup(rows, { scope: 'show' });
  assert.deepEqual(selected.map(row => row.infoHash), [rows[0], rows[3], rows[6]].map(row => row.infoHash));
  assert.equal(selectReleaseGroup(rows, { scope: 'show', mode: 'all' }).length, rows.length);
  const whole = release(9, 'Synthetic Complete Series', 3);
  assert.deepEqual(selectReleaseGroup([...rows, whole], { scope: 'show' }), [whole]);
});

test('episode-only seasons choose highest-seeded equivalents and account for multi-episode names', () => {
  const rows = [release(1, 'Synthetic S01E01', 5), release(2, 'Synthetic S01E01', 50),
    release(3, 'Synthetic S01E02E03', 8), release(4, 'Synthetic S01E03', 80)];
  assert.deepEqual(selectReleaseGroup(rows, { scope: 'season', season: 1 }).map(row => row.infoHash), [rows[2], rows[1]].map(row => row.infoHash));
  assert.deepEqual(releaseCoverage(rows[2]), { kind: 'episode', season: 1, episodes: [2, 3] });
  assert.equal(releaseCoverage(release(8, 'Synthetic S01E02-S02E03')).kind, 'unknown');
  assert.equal(releaseCoverage(release(9, 'Synthetic S02_complete')).kind, 'season');
  assert.equal(releaseCoverage(release(10, 'Synthetic S01E09-E02')).kind, 'unknown');
});

test('repeated episode tokens and full-token ranges preserve coverage conservatively', () => {
  assert.equal(releaseCoverage(release(1, 'Synthetic.1x01-2x03')).kind, 'unknown');
  assert.deepEqual(releaseCoverage(release(2, 'Synthetic.1x01-1x03')),
    { kind: 'episode', season: 1, episodes: [1, 2, 3] });
  assert.deepEqual(releaseCoverage(release(3, 'Synthetic.S01E01.S01E03')),
    { kind: 'episode', season: 1, episodes: [1, 3] });
  assert.deepEqual(releaseCoverage(release(4, 'Synthetic.S01E01-S01E03')),
    { kind: 'episode', season: 1, episodes: [1, 2, 3] });
  assert.deepEqual(releaseCoverage(release(5, 'Synthetic_S01E01_1080p')),
    { kind: 'episode', season: 1, episodes: [1] });
  assert.deepEqual(releaseCoverage(release(6, 'Synthetic_S02_complete')),
    { kind: 'season', seasons: [2] });
});

test('best show retains an overlapping pack when it is the only available coverage', () => {
  const first = release(1, 'Synthetic S01-S02', 100);
  const second = release(2, 'Synthetic S02-S03', 90);
  assert.deepEqual(selectReleaseGroup([first, second], { scope: 'show' }), [first, second]);
  const episodes = [release(3, 'Synthetic S04E01-E02', 100), release(4, 'Synthetic S04E02-E03', 90)];
  assert.deepEqual(selectReleaseGroup(episodes, { scope: 'season', season: 4 }), episodes);
});

test('title collection follows raw offsets through empty filtered windows and deduplicates', async () => {
  const row = release(1, 'Synthetic S01E01');
  const paths = [], progress = [];
  const pages = [{ items: [], scannedCount: 2, hasNextPage: true, nextOffset: 2 },
    { items: [row], scannedCount: 2, hasNextPage: true, nextOffset: 4 },
    { items: [row, release(2, 'Synthetic S01E02')], scannedCount: 2, hasNextPage: false, nextOffset: null }];
  const result = await collectTitleReleases({ title: 'Synthetic & TV', content_type: 'tv_show', content_source: 'tmdb', content_id: '123' }, {
    pageSize: 2, request: async path => { paths.push(path); return pages.shift(); }, onProgress: value => progress.push(value),
  });
  assert.equal(result.items.length, 2);
  assert.equal(result.pages, 3);
  assert.equal(result.scannedCount, 6);
  assert.equal(result.truncated, false);
  assert.deepEqual(paths.map(path => new URL(path, 'https://example.invalid').searchParams.get('offset')), ['0', '2', '4']);
  assert.equal(new URL(paths[0], 'https://example.invalid').searchParams.get('title'), 'Synthetic & TV');
  assert.equal(progress.at(-1).itemCount, 2);
});

test('title collection explicitly reports page and item bounds', async () => {
  let calls = 0;
  const result = await collectTitleReleases({ title: 'Synthetic' }, { maxPages: 2,
    request: async () => ({ items: [release(++calls, 'Synthetic')], scannedCount: 250, hasNextPage: true, nextOffset: calls * 250 }),
  });
  assert.equal(calls, 2);
  assert.equal(result.truncated, true);
  assert.equal(result.nextOffset, 500);
  const capped = await collectTitleReleases({ title: 'Synthetic' }, { maxItems: 1,
    request: async () => ({ items: [release(1, 'Synthetic'), release(2, 'Synthetic')], hasNextPage: false }),
  });
  assert.equal(capped.items.length, 1);
  assert.equal(capped.truncated, true, 'a local item cap remains partial even on the last backend page');
});

test('title collection rejects failed requests and non-advancing cursors', async () => {
  await assert.rejects(collectTitleReleases({ title: 'Synthetic' }, { request: async () => null }), /Could not load/);
  await assert.rejects(collectTitleReleases({ title: 'Synthetic' }, {
    request: async () => ({ items: [], hasNextPage: true, nextOffset: 0 }),
  }), /pagination cursor/);
  const controller = new AbortController();
  controller.abort();
  let called = false;
  await assert.rejects(collectTitleReleases({ title: 'Synthetic' }, {
    signal: controller.signal, request: async () => { called = true; },
  }), { name: 'AbortError' });
  assert.equal(called, false);
});

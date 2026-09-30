/* Public-library release tools. No DOM, storage, or network access at import. */
(function (root, factory) {
  const tools = factory();
  if (typeof module === 'object' && module.exports) module.exports = tools;
  else root.BitAgentLibraryTools = tools;
})(typeof globalThis === 'object' ? globalThis : this, function () {
  'use strict';

  /** Canonical v1 info hash; base32 and hex identities deduplicate together. */
  function normalizeInfoHash(value) {
    if (typeof value !== 'string') return '';
    const hash = value.trim();
    if (/^[a-f\d]{40}$/i.test(hash)) return hash.toLowerCase();
    if (!/^[a-z2-7]{32}$/i.test(hash)) return '';
    const alphabet = 'ABCDEFGHIJKLMNOPQRSTUVWXYZ234567';
    let bits = 0, buffer = 0, hex = '';
    for (const char of hash.toUpperCase()) {
      buffer = (buffer << 5) | alphabet.indexOf(char);
      bits += 5;
      if (bits >= 8) {
        bits -= 8;
        hex += ((buffer >>> bits) & 255).toString(16).padStart(2, '0');
        buffer &= (1 << bits) - 1;
      }
    }
    return hex;
  }

  function releaseName(release) {
    return String(release.torrentName || release.name || release.title || '');
  }

  /** Construct the URI from trusted syntax, never from a supplied magnetUri. */
  function magnetFor(release) {
    if (!release || typeof release !== 'object') return '';
    const hash = normalizeInfoHash(release.infoHash);
    if (!hash) return '';
    // Corrupt metadata may contain an unpaired UTF-16 surrogate. Preserve all
    // valid Unicode characters while replacing only those invalid code units.
    const name = Array.from(releaseName(release), char =>
      char.length === 1 && /[\uD800-\uDFFF]/.test(char) ? '\uFFFD' : char).join('');
    return `magnet:?xt=urn:btih:${hash}&dn=${encodeURIComponent(name)}`;
  }

  function dedupeReleases(releases) {
    const seen = new Set();
    return (Array.isArray(releases) ? releases : []).filter(release => {
      const hash = release && normalizeInfoHash(release.infoHash);
      if (!hash || seen.has(hash)) return false;
      seen.add(hash);
      return true;
    });
  }

  function compareReleases(a, b) {
    const seeders = release => Number.isFinite(Number(release.seeders))
      ? Math.max(0, Number(release.seeders)) : 0;
    return seeders(b) - seeders(a)
      || normalizeInfoHash(a.infoHash).localeCompare(normalizeInfoHash(b.infoHash));
  }

  function numberRange(first, last, maximum) {
    if (last < first || last > maximum) return [];
    return Array.from({ length: last - first + 1 }, (_, index) => first + index);
  }

  /** Conservative coverage parsing: an unclassified name is never a show pack. */
  function releaseCoverage(release) {
    const name = releaseName(release);
    const seasons = name.match(/(?<![a-z\d])S(\d{1,2})\s*[-–~]\s*S?(\d{1,2})(?!\d)/i)
      || name.match(/(?<![a-z\d])seasons?[\s._-]*(\d{1,2})\s*[-–~]\s*(\d{1,2})(?!\d)/i);
    if (seasons) {
      const range = numberRange(+seasons[1], +seasons[2], 99);
      return range.length ? { kind: 'multi-season', seasons: range } : { kind: 'unknown' };
    }
    if (/(?<![a-z\d])complete[\s._-]+(?:series|collection|pack|seasons?)(?![a-z\d])|(?<![a-z\d])all[\s._-]+seasons(?![a-z\d])/i.test(name)) {
      return { kind: 'show' };
    }
    const episode = name.match(/(?<![a-z\d])S(\d{1,2})[\s._-]?E(\d{1,3})(?!\d)/i)
      || name.match(/(?<![a-z\d])(\d{1,2})x(\d{1,3})(?!\d)/i);
    if (episode) {
      // Cross-season episode bundles cannot be inferred from a first token.
      const fullTokens = [...name.matchAll(/(?<![a-z\d])S(\d{1,2})[\s._-]?E(\d{1,3})(?!\d)|(?<![a-z\d])(\d{1,2})x(\d{1,3})(?!\d)/gi)];
      if (fullTokens.some(token => +(token[1] || token[3]) !== +episode[1])) return { kind: 'unknown' };
      const suffix = name.slice(episode.index + episode[0].length);
      const range = suffix.match(/^\s*[-–~]\s*(?:S\d{1,2}[\s._-]?E|\d{1,2}x|E)?(\d{1,3})(?!\d)/i);
      let episodes = [+episode[2]];
      if (range) {
        episodes = numberRange(+episode[2], +range[1], 999);
        if (!episodes.length) return { kind: 'unknown' };
      }
      episodes.push(...fullTokens.map(token => +(token[2] || token[4])));
      const extra = suffix.match(/^(?:[\s._-]?E\d{1,3})+/i);
      if (extra) episodes.push(...[...extra[0].matchAll(/E(\d{1,3})/gi)].map(token => +token[1]));
      return { kind: 'episode', season: +episode[1], episodes: [...new Set(episodes)] };
    }
    const season = name.match(/(?<![a-z\d])S(\d{1,2})(?!\d)(?!E)/i)
      || name.match(/(?<![a-z\d])(?:Season|Series)[\s._-]*(\d{1,2})(?!\d)/i);
    if (season) return { kind: 'season', seasons: [+season[1]] };
    if (Number.isInteger(release.seasonNumber) && Number.isInteger(release.episodeNumber)
        && release.seasonNumber >= 0 && release.episodeNumber >= 0) {
      return { kind: 'episode', season: release.seasonNumber, episodes: [release.episodeNumber] };
    }
    return { kind: 'unknown' };
  }

  /** Prefer packs and seeded equivalents while preserving available coverage.
   * A bundle adding coverage can overlap another pack. UI must disclose this.
   */
  function selectReleaseGroup(releases, options = {}) {
    const { scope = 'title', mode = 'best', season } = options;
    if (!['title', 'season', 'show'].includes(scope) || !['best', 'all'].includes(mode)) {
      throw new TypeError('Invalid release selection scope or mode');
    }
    if (scope === 'season' && (!Number.isInteger(season) || season < 0 || season > 99)) {
      throw new TypeError('A valid season number is required');
    }
    const rows = dedupeReleases(releases).slice().sort(compareReleases);
    if (scope === 'title') return mode === 'all' ? rows : rows.slice(0, 1);
    let candidates = rows.map(release => ({ release, coverage: releaseCoverage(release) }));
    if (scope === 'season') {
      // A multi-season/show pack downloads more than the requested season.
      candidates = candidates.filter(({ coverage }) =>
        (coverage.kind === 'season' && coverage.seasons[0] === season)
        || (coverage.kind === 'episode' && coverage.season === season));
    }
    if (mode === 'all') return candidates.map(row => row.release);
    if (scope === 'show') {
      const wholeShow = candidates.find(row => row.coverage.kind === 'show');
      if (wholeShow) return [wholeShow.release];
    }
    const selected = [], coveredSeasons = new Set(), coveredEpisodes = new Set();
    const packs = candidates.filter(row => ['season', 'multi-season'].includes(row.coverage.kind));
    // Prefer greater pack coverage, then highest-seeded equivalent versions.
    packs.sort((a, b) => b.coverage.seasons.length - a.coverage.seasons.length
      || compareReleases(a.release, b.release));
    for (const row of packs) {
      if (row.coverage.seasons.every(value => coveredSeasons.has(value))) continue;
      row.coverage.seasons.forEach(value => coveredSeasons.add(value));
      selected.push(row.release);
    }
    const episodes = candidates.filter(row => row.coverage.kind === 'episode');
    episodes.sort((a, b) => b.coverage.episodes.length - a.coverage.episodes.length
      || compareReleases(a.release, b.release));
    for (const row of episodes) {
      const { season: seasonNumber, episodes: episodeNumbers } = row.coverage;
      const keys = episodeNumbers.map(value => `${seasonNumber}:${value}`);
      if (coveredSeasons.has(seasonNumber) || keys.every(key => coveredEpisodes.has(key))) continue;
      keys.forEach(key => coveredEpisodes.add(key));
      selected.push(row.release);
    }
    return selected;
  }

  function boundedInteger(value, fallback, maximum) {
    return Number.isInteger(value) && value > 0 ? Math.min(value, maximum) : fallback;
  }

  function checkAbort(signal) {
    if (signal && signal.aborted) {
      const error = new Error('Release collection canceled');
      error.name = 'AbortError';
      throw error;
    }
  }

  /** Page raw title-search windows sequentially with explicit collection limits. */
  async function collectTitleReleases(identity, options = {}) {
    const { request, signal, onProgress } = options;
    if (typeof request !== 'function') throw new TypeError('A request function is required');
    if (!identity || typeof identity.title !== 'string' || !identity.title.trim()) {
      throw new TypeError('A title is required');
    }
    const maxPages = boundedInteger(options.maxPages, 12, 20);
    const maxItems = boundedInteger(options.maxItems, 3000, 5000);
    const pageSize = boundedInteger(options.pageSize, 250, 500);
    const base = new URLSearchParams({ title: identity.title.trim() });
    for (const key of ['content_type', 'content_source', 'content_id']) {
      if (identity[key] != null && identity[key] !== '') base.set(key, String(identity[key]));
    }
    let offset = 0, pages = 0, scannedCount = 0, hasNextPage = true, truncated = false;
    const items = [], seen = new Set();
    while (hasNextPage && pages < maxPages && items.length < maxItems) {
      checkAbort(signal);
      const params = new URLSearchParams(base);
      params.set('limit', String(pageSize));
      params.set('offset', String(offset));
      const page = await request(`/api/titles/releases?${params}`, signal ? { signal } : {});
      checkAbort(signal);
      if (!page || !Array.isArray(page.items) || typeof page.hasNextPage !== 'boolean') {
        throw new Error('Could not load release collection');
      }
      pages += 1;
      scannedCount += Number.isInteger(page.scannedCount) && page.scannedCount >= 0
        ? page.scannedCount : page.items.length;
      for (const release of dedupeReleases(page.items)) {
        const hash = normalizeInfoHash(release.infoHash);
        if (seen.has(hash)) continue;
        if (items.length === maxItems) { truncated = true; break; }
        seen.add(hash);
        items.push(release);
      }
      hasNextPage = page.hasNextPage;
      if (hasNextPage) {
        if (!Number.isInteger(page.nextOffset) || page.nextOffset <= offset) {
          throw new Error('Invalid release pagination cursor');
        }
        offset = page.nextOffset;
      }
      if (typeof onProgress === 'function') onProgress({ pages, itemCount: items.length, scannedCount });
    }
    return { items, pages, scannedCount, hasNextPage, truncated: truncated || hasNextPage,
      nextOffset: hasNextPage ? offset : null };
  }

  return { normalizeInfoHash, magnetFor, dedupeReleases, releaseCoverage,
    selectReleaseGroup, collectTitleReleases };
});

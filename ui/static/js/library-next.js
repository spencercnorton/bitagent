/* Public-library navigation and removable search filters. No extra network work. */
'use strict';

function toggleFilterRail(force) {
  const rail = document.getElementById('libFilterRail');
  const button = document.getElementById('libFilterToggle');
  if (!rail || !button) return;
  const open = typeof force === 'boolean' ? force : !rail.classList.contains('is-open');
  rail.classList.toggle('is-open', open);
  button.setAttribute('aria-expanded', String(open));
}

function clearAllFilters() {
  document.getElementById('libSearch').value = '';
  state.sort = 'seeders';
  clearFacets();
}

function removeLibraryFilter(kind, value) {
  if (kind === 'q') { clearLibSearch(); return; }
  if (kind === 'provider') { selectProvider(''); return; }
  if (kind === 'network') { selectNetwork(''); return; }
  if (kind === 'type') { setType(''); return; }
  if (kind === 'sort') { setLibSort('seeders'); return; }
  if (kind === 'year') { state.yearMin = ''; state.yearMax = ''; loadLibrary(); return; }
  if (kind === 'english') { toggleLibFilter('foreign'); return; }
  if (kind === 'unmatched') { toggleLibFilter('unmatched'); return; }
  if (kind === 'features') { toggleFeature(value); return; }
  toggleFacet(kind, value);
}

function renderActiveLibraryFilters() {
  const mount = document.getElementById('libActiveFilters');
  if (!mount) return;
  const filters = [];
  const add = (kind, value, label) => filters.push({ kind, value, label });
  if (state.q) add('q', '', `Search: ${state.q}`);
  if (state.provider) add('provider', '', typeof providerName === 'function' ? providerName(state.provider) : `Provider ${state.provider}`);
  if (state.network) add('network', '', typeof networkName === 'function' ? networkName(state.network) : `Network ${state.network}`);
  if (state.type) add('type', '', typeLabel(state.type));
  if (state.yearMin || state.yearMax) add('year', '', `${state.yearMin || 'Any year'} – ${state.yearMax || 'today'}`);
  state.genres.forEach(v => add('genres', v, genreLabel(v)));
  state.qualities.forEach(v => add('qualities', v, fmtFacetResolution(v)));
  state.sources.forEach(v => add('sources', v, v));
  state.features.forEach(v => add('features', v, v === '2160p' ? '4K' : v.toUpperCase()));
  if (state.hideForeign) add('english', '', 'English only');
  if (!state.hideUnmatched) add('unmatched', '', 'Include unmatched');
  if (state.sort !== 'seeders') add('sort', '', `Sort: ${state.sort}`);
  mount.replaceChildren();
  for (const f of filters) {
    const button = document.createElement('button');
    button.className = 'lib-active-filter';
    button.type = 'button';
    button.textContent = `${f.label} ×`;
    button.setAttribute('aria-label', `Remove ${f.label} filter`);
    button.addEventListener('click', () => removeLibraryFilter(f.kind, f.value));
    mount.appendChild(button);
  }
  mount.hidden = !filters.length;
  const summary = document.getElementById('libFilterSummary');
  if (summary) summary.hidden = !filters.length;
  document.getElementById('libBrowse').classList.toggle('is-searching', !isHome());
  document.querySelectorAll('[data-lib-quality]').forEach(el => {
    const active = state.qualities.has(el.dataset.libQuality);
    el.classList.toggle('active', active);
    el.setAttribute('aria-pressed', String(active));
  });
  document.querySelectorAll('[data-lib-type]').forEach(el => {
    const active = el.dataset.libType === state.type;
    el.classList.toggle('active', active);
    el.setAttribute('aria-pressed', String(active));
  });
  document.querySelectorAll('[data-lib-feature]').forEach(el => {
    const active = state.features.has(el.dataset.libFeature);
    el.classList.toggle('active', active);
    el.setAttribute('aria-pressed', String(active));
  });
  const count = document.getElementById('libFilterCount');
  if (count) { count.textContent = String(filters.length); count.hidden = !filters.length; }
  _setInputValue('libYearMin', state.yearMin);
  _setInputValue('libYearMax', state.yearMax);
}

// Controls synchronize whenever the original browse controller changes state,
// including Back/Forward and permalink hydration.
const _syncLibraryControls = syncControls;
syncControls = function () { _syncLibraryControls(); renderActiveLibraryFilters(); };
renderActiveLibraryFilters();

document.addEventListener('keydown', event => {
  if (event.key === 'Escape') toggleFilterRail(false);
  if ((event.ctrlKey || event.metaKey) && event.key.toLowerCase() === 'k' && !_modalStack.length) {
    event.preventDefault();
    document.getElementById('libSearch').focus();
  }
});

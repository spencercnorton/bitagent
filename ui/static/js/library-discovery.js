/* Cached metadata discovery, provider browsing, and a lightweight image carousel. */
'use strict';

function discoveryItemFor(item) {
  if (!item || !/^\d{1,12}$/.test(String(item.id || '')) || !['movie', 'tv_show'].includes(item.type) || !item.title) return null;
  const image = value => typeof value === 'string' && /^https:\/\/image\.tmdb\.org\/t\/p\/[a-z0-9]+\/[^?#\s]+$/i.test(value) ? value : '';
  return { id: String(item.id), type: item.type, title: String(item.title), year: String(item.year || ''),
    overview: String(item.overview || ''), backdrop: image(item.backdrop), poster: image(item.poster),
    voteAverage: Number.isFinite(item.voteAverage) ? item.voteAverage : 0, label: item.mode === 'newest' ? 'New & noteworthy' : 'Popular now' };
}

function discoveryGroupFor(item) {
  const d = discoveryItemFor(item);
  if (!d) return null;
  // A metadata card is a title identity, never evidence of an indexed release.
  // openDetail fetches exact source/id releases before offering magnet actions.
  const best = { name: d.title, contentType: d.type, contentSource: 'tmdb', contentId: d.id };
  return { key: `${d.type}:id:${d.id}`, best, items: [], contentType: d.type, contentSource: 'tmdb', contentId: d.id, isMapped: true };
}

const discoveryState = { providers: [], regions: [], titles: new Map(), providerSeq: 0,
  slides: [], index: 0, paused: false, hovering: false, visible: true, timer: null, spotlightSeq: 0 };
function providerName(id) {
  const provider = discoveryState.providers.find(p => String(p.id) === String(id));
  return provider ? provider.name : `Provider ${id}`;
}
function filterProviders() {
  const query = document.getElementById('libProviderSearch').value.trim().toLocaleLowerCase();
  const host = document.getElementById('libProviders');
  host.replaceChildren();
  const all = document.createElement('button');
  all.className = 'lib-provider'; all.type = 'button'; all.textContent = 'All services';
  all.setAttribute('aria-pressed', String(!state.provider)); all.addEventListener('click', () => selectProvider(''));
  host.appendChild(all);
  const providers = discoveryState.providers.filter(p => p.name.toLocaleLowerCase().includes(query));
  for (const provider of providers) {
    const button = document.createElement('button'); button.type = 'button'; button.className = 'lib-provider';
    button.dataset.provider = String(provider.id); button.setAttribute('aria-pressed', String(state.provider === String(provider.id)));
    if (provider.logo) { const img = document.createElement('img'); img.src = provider.logo; img.alt = ''; img.loading = 'lazy'; img.decoding = 'async'; img.addEventListener('error', () => img.remove()); button.appendChild(img); }
    const name = document.createElement('span'); name.className = 'lib-provider-name'; name.textContent = provider.name; button.appendChild(name);
    button.addEventListener('click', () => selectProvider(String(provider.id))); host.appendChild(button);
  }
  document.getElementById('libProviderStatus').textContent = !discoveryState.providers.length
    ? 'Provider browsing is unavailable. You can still search indexed releases below.'
    : !providers.length ? 'No providers match your search.' : `Streaming availability in ${regionName(state.region)}. Choose a service to browse or search its catalogue.`;
}
function regionName(code) { return (discoveryState.regions.find(r => r.code === code) || {}).name || code; }
async function loadProviders() {
  const seq = ++discoveryState.providerSeq;
  const data = await api(`/api/discovery/providers?region=${encodeURIComponent(state.region)}`);
  if (seq !== discoveryState.providerSeq) return;
  discoveryState.providers = data && data.available ? (data.providers || []) : [];
  discoveryState.regions = data && data.regions && data.regions.length ? data.regions : [{ code:'US', name:'United States' }];
  const select = document.getElementById('libProviderRegion'); select.replaceChildren();
  for (const r of discoveryState.regions) { const option = document.createElement('option'); option.value = r.code; option.textContent = r.name; select.appendChild(option); }
  select.value = state.region;
  if (!select.value) { const option = document.createElement('option'); option.value = state.region; option.textContent = state.region; select.appendChild(option); select.value = state.region; }
  filterProviders(); renderActiveLibraryFilters();
}
function selectProvider(id) {
  state.provider = id; state.page = 0; state.sort = 'seeders';
  // Release-quality facets belong to indexed releases, not provider catalogues.
  state.genres.clear(); state.qualities.clear(); state.sources.clear(); state.features.clear();
  state.yearMin = ''; state.yearMax = ''; state.hideForeign = false; state.hideUnmatched = true;
  state._preAnimeHideUnmatched = null;
  if (!['', 'movie', 'tv_show'].includes(state.type)) state.type = '';
  loadLibrary({ push:true });
}
function changeProviderRegion(code) {
  if (!/^[A-Z]{2}$/.test(code)) return;
  state.region = code; state.page = 0; loadProviders(); loadSpotlight();
  if (state.provider) loadLibrary({ push:true });
}
function syncProviderControls() {
  const browse = document.getElementById('libBrowse');
  browse.classList.toggle('lib-provider-catalog', !!state.provider);
  document.getElementById('libSearch').placeholder = state.provider ? `Search ${providerName(state.provider)} movies and series…` : 'Search titles, seasons, and releases…';
  document.querySelectorAll('#libProviders button').forEach(button => button.setAttribute('aria-pressed', String((button.dataset.provider || '') === state.provider)));
  const select = document.getElementById('libProviderRegion'); if (select.value !== state.region) { select.value = state.region; loadProviders(); loadSpotlight(); }
}
function catalogueCard(item) {
  const title = discoveryItemFor(item); if (!title) return '';
  discoveryState.titles.set(`${title.type}:${title.id}`, title);
  return `<button class="lib-card" type="button" onclick="openDiscoveryTitle('${title.type}:${title.id}')" aria-label="Find indexed releases for ${escAttr(title.title)}">
    <div class="lib-poster"><div class="lib-poster-ph" aria-hidden="true">${iconFor(title.type)}</div>${title.poster ? `<img class="lib-poster-img" src="${escAttr(title.poster)}" alt="" loading="lazy" decoding="async" onerror="this.remove()">` : ''}<span class="lib-type-badge">${escHtml(typeLabel(title.type))}</span><span class="lib-catalog-badge">Find releases ↗</span></div>
    <div class="lib-card-info"><div class="lib-card-title">${escHtml(title.title)}</div><div class="lib-card-sub">${escHtml(title.year)} · ${escHtml(typeLabel(title.type))}</div></div></button>`;
}
async function loadProviderLibrary(seq, signal) {
  setGridLoading(true);
  const grid = document.getElementById('libraryGrid'), note = document.getElementById('libResultNote');
  grid.innerHTML = '<div class="lib-rel-empty">Loading provider catalogue…</div>';
  note.textContent = `${providerName(state.provider)} · ${regionName(state.region)}`;
  document.getElementById('libPrev').disabled = true; document.getElementById('libNext').disabled = true;
  document.getElementById('libPager').style.display = '';
  const params = new URLSearchParams({provider:state.provider,region:state.region,page:state.page + 1,q:state.q,type:state.type || 'all'});
  const data = await api(`/api/discovery?${params}`, {signal});
  if ((signal && signal.aborted) || seq !== _navSeq) return;
  setGridLoading(false);
  discoveryState.titles.clear(); groupCache.clear();
  if (!data || !data.available) {
    state.hasNext = false;
    note.textContent = 'Provider catalogue is temporarily unavailable.';
    grid.innerHTML = '<div class="lib-empty"><h3>Try again shortly</h3><p>You can search the indexed library using All services above.</p><button class="lib-btn" onclick="loadLibrary({keepPage:true})">Retry</button></div>';
    document.getElementById('libPaginationInfo').textContent = '';
    document.getElementById('libPager').style.display = 'none';
    return;
  }
  state.hasNext = !!data.hasNextPage;
  grid.innerHTML = (data.items || []).map(catalogueCard).join('') || '<div class="lib-empty"><h3>No matching titles</h3><p>Try a different search, provider, or country.</p></div>';
  note.textContent = `${providerName(state.provider)} · ${regionName(state.region)} · Provider catalogue. Open a title to check indexed releases.${data.partial ? ' Some provider checks are unavailable; these results are partial.' : ''}`;
  document.getElementById('libPrev').disabled = state.page === 0;
  document.getElementById('libNext').disabled = !state.hasNext;
  document.getElementById('libPaginationInfo').textContent = `Page ${state.page + 1}`;
  document.getElementById('libPager').style.display = state.page > 0 || state.hasNext ? '' : 'none';
}
function openDiscoveryTitle(key) {
  const item = discoveryState.titles.get(key), group = discoveryGroupFor(item);
  if (!group) return;
  groupCache.set(group.key, group); openDetail(group.key);
}

function clearSpotlightTimer() { clearTimeout(discoveryState.timer); discoveryState.timer = null; }
function scheduleSpotlight() {
  clearSpotlightTimer();
  if (discoveryState.paused || discoveryState.hovering || document.hidden || !discoveryState.visible || discoveryState.slides.length < 2) return;
  discoveryState.timer = setTimeout(() => { if (!_modalStack.length) moveSpotlight(1, false); else scheduleSpotlight(); }, 7000);
}
function renderSpotlight() {
  const slide = discoveryState.slides[discoveryState.index]; if (!slide) return;
  const hero = document.getElementById('libSpotlight'), img = document.getElementById('libSpotlightImage');
  const changed = discoveryState.renderedKey !== `${slide.type}:${slide.id}`;
  discoveryState.renderedKey = `${slide.type}:${slide.id}`;
  hero.classList.remove('is-loading', 'is-empty'); hero.classList.toggle('has-image', !!slide.backdrop);
  img.hidden = !slide.backdrop; if (slide.backdrop) img.src = slide.backdrop; else img.removeAttribute('src');
  img.onerror = () => { img.hidden = true; hero.classList.remove('has-image'); };
  document.getElementById('libSpotlightLabel').textContent = slide.label;
  document.getElementById('libSpotlightTitle').textContent = slide.title;
  document.getElementById('libSpotlightMeta').textContent = [typeLabel(slide.type), slide.year, slide.voteAverage > 0 ? `★ ${slide.voteAverage.toFixed(1)}` : ''].filter(Boolean).join(' · ');
  document.getElementById('libSpotlightOverview').textContent = slide.overview;
  document.getElementById('libSpotlightOpen').hidden = false;
  const dots = document.getElementById('libSpotlightDots');
  if (dots.children.length !== discoveryState.slides.length || dots.dataset.slides !== String(discoveryState.spotlightSeq)) {
    dots.replaceChildren(); dots.dataset.slides = String(discoveryState.spotlightSeq);
    discoveryState.slides.forEach((s, i) => { const button = document.createElement('button'); button.type = 'button'; button.setAttribute('aria-label', `Show ${s.title}`); button.addEventListener('click', () => { discoveryState.index = i; renderSpotlight(); scheduleSpotlight(); }); dots.appendChild(button); });
  }
  Array.from(dots.children).forEach((button, i) => button.setAttribute('aria-current', String(i === discoveryState.index)));
  document.getElementById('libSpotlightControls').hidden = discoveryState.slides.length < 2;
  if (changed && typeof img.animate === 'function' && !matchMedia('(prefers-reduced-motion: reduce)').matches) {
    img.animate([{opacity:0, transform:'scale(1.025)'}, {opacity:1, transform:'scale(1)'}], {duration:600, easing:'ease-out'});
    const copy = hero.querySelector('.lib-spotlight-content');
    if (copy && typeof copy.animate === 'function') copy.animate([{opacity:0, transform:'translateY(9px)'}, {opacity:1, transform:'translateY(0)'}], {duration:420, easing:'ease-out'});
  }
  const next = discoveryState.slides[(discoveryState.index + 1) % discoveryState.slides.length];
  if (next.backdrop) { const preload = new Image(); preload.src = next.backdrop; }
}
function moveSpotlight(direction, manual = true) {
  if (!discoveryState.slides.length) return;
  discoveryState.index = (discoveryState.index + direction + discoveryState.slides.length) % discoveryState.slides.length;
  renderSpotlight(); scheduleSpotlight();
  if (manual) document.getElementById('libSpotlight').setAttribute('aria-label', `Featured title: ${discoveryState.slides[discoveryState.index].title}`);
}
function toggleSpotlightPause(force) {
  discoveryState.paused = typeof force === 'boolean' ? force : !discoveryState.paused;
  const button = document.getElementById('libSpotlightPause');
  button.textContent = discoveryState.paused ? '▶' : 'Ⅱ'; button.setAttribute('aria-label', `${discoveryState.paused ? 'Resume' : 'Pause'} featured title rotation`);
  scheduleSpotlight();
}
function openSpotlightTitle() {
  const slide = discoveryState.slides[discoveryState.index]; if (!slide) return;
  const key = `${slide.type}:${slide.id}`; discoveryState.titles.set(key, slide); openDiscoveryTitle(key);
}
async function loadSpotlight() {
  const seq = ++discoveryState.spotlightSeq; clearSpotlightTimer();
  const data = await api(`/api/discovery/spotlight?region=${encodeURIComponent(state.region)}`);
  if (seq !== discoveryState.spotlightSeq) return;
  discoveryState.slides = ((data && data.items) || []).map(discoveryItemFor).filter(Boolean).slice(0, 6);
  discoveryState.index = 0;
  if (!discoveryState.slides.length) {
    const hero = document.getElementById('libSpotlight'); hero.classList.remove('is-loading', 'has-image'); hero.classList.add('is-empty');
    document.getElementById('libSpotlightTitle').textContent = 'Movies & series, in focus.';
    document.getElementById('libSpotlightOverview').textContent = 'Spotlight artwork is temporarily unavailable. Explore indexed releases below.';
    document.getElementById('libSpotlightImage').hidden = true; document.getElementById('libSpotlightOpen').hidden = true; document.getElementById('libSpotlightControls').hidden = true;
    return;
  }
  renderSpotlight(); scheduleSpotlight();
}

if (typeof document !== 'undefined') {
  const originalSync = syncControls;
  syncControls = function () { originalSync(); syncProviderControls(); };
  const hero = document.getElementById('libSpotlight');
  hero.addEventListener('mouseenter', () => { discoveryState.hovering = true; clearSpotlightTimer(); });
  hero.addEventListener('mouseleave', () => { discoveryState.hovering = false; scheduleSpotlight(); });
  hero.addEventListener('focusin', event => { if (event.target.id !== 'libSpotlightPause') toggleSpotlightPause(true); });
  document.addEventListener('visibilitychange', scheduleSpotlight);
  const reducedMotion = matchMedia('(prefers-reduced-motion: reduce)');
  toggleSpotlightPause(reducedMotion.matches);
  reducedMotion.addEventListener('change', event => toggleSpotlightPause(event.matches));
  if (typeof IntersectionObserver !== 'undefined') new IntersectionObserver(entries => { discoveryState.visible = entries[0].isIntersecting; scheduleSpotlight(); }).observe(hero);
  loadProviders(); loadSpotlight(); syncProviderControls();
  // The base controller boots first; supersede its initial request for provider permalinks.
  if (state.provider) loadLibrary({fromPop:true, keepPage:true});
}
if (typeof module !== 'undefined' && module.exports) module.exports = {discoveryItemFor, discoveryGroupFor};

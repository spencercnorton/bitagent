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

function discoveryLogoUrlFor(value) {
  if (typeof value !== 'string' || /[\s?#]/.test(value)) return '';
  try {
    const url = new URL(value);
    if (url.origin !== 'https://image.tmdb.org' || url.username || url.password ||
      !/^\/t\/p\/(?:w\d+|h\d+|original)\/[^/]+\.(?:png|jpe?g|webp|svg)$/i.test(url.pathname)) return '';
    return url.href;
  } catch (_) { return ''; }
}
function discoveryLogoCandidatesFor(entry) {
  const supplied = [entry && entry.logo, ...((entry && Array.isArray(entry.logoAlternatives)) ? entry.logoAlternatives : [])];
  return [...new Set(supplied.map(discoveryLogoUrlFor).filter(Boolean))];
}
function discoveryCatalogueParamsFor(browseState) {
  const params = new URLSearchParams({region:browseState.region || 'US', page:browseState.page + 1, q:browseState.q || ''});
  if (browseState.network) { params.set('network', browseState.network); params.set('type', 'tv_show'); }
  else { params.set('provider', browseState.provider); params.set('type', browseState.type || 'all'); }
  return params;
}

const discoveryState = { providers: [], regions: [], titles: new Map(), providerSeq: 0,
  pickerMode: 'streaming', providerQuery: '', providerExpanded: false, providerNames: new Map(), networkQuery: '', networks: [], networkNames: new Map(),
  networkSeq: 0, networkController: null, networkSearchTimer: null, networkPage: 0, networkHasNext: false,
  networkLoaded: false, networkLoading: false, networkAvailable: false, networkPartial: false, networkTotal: 0,
  selectionKey: '', slides: [], index: 0, paused: false, hovering: false, visible: true, timer: null, spotlightSeq: 0 };
function providerMatchesId(entry, id) {
  if (!entry || !id) return false;
  return String(entry.id) === String(id) ||
    (Array.isArray(entry.providerIds) && entry.providerIds.some(value => String(value) === String(id)));
}
function providerForId(id) { return discoveryState.providers.find(entry => providerMatchesId(entry, id)); }
function providerBrandKeyFor(entry) {
  if (!entry) return '';
  return typeof entry.brandKey === 'string' ? entry.brandKey :
    typeof entry.name === 'string' ? entry.name.normalize('NFKC').trim().replace(/\s+/g, ' ').toLocaleLowerCase() : '';
}
function providerBrandMatches(first, second) {
  const key = providerBrandKeyFor(first);
  return !!key && key === providerBrandKeyFor(second);
}
function providerName(id) {
  const provider = providerForId(id);
  return provider ? provider.name : discoveryState.providerNames.get(String(id)) || `Provider ${id}`;
}
function networkName(id) { return discoveryState.networkNames.get(String(id)) || `Network ${id}`; }
function appendDiscoveryLogo(button, entry) {
  const plate = document.createElement('span'); plate.className = 'lib-provider-logo';
  const fallback = document.createElement('span'); fallback.className = 'lib-provider-logo-fallback'; fallback.textContent = 'Logo unavailable';
  const candidates = discoveryLogoCandidatesFor(entry);
  plate.dataset.logoState = candidates.length ? 'loading' : 'unavailable';
  fallback.hidden = !!candidates.length; plate.appendChild(fallback); button.appendChild(plate);
  if (!candidates.length) return;
  const img = document.createElement('img'); img.alt = ''; img.loading = 'lazy'; img.decoding = 'async';
  let next = 0;
  img.addEventListener('load', () => { plate.dataset.logoState = 'ready'; fallback.hidden = true; });
  img.addEventListener('error', () => {
    if (next < candidates.length) { img.src = candidates[next++]; return; }
    img.hidden = true; fallback.hidden = false; plate.dataset.logoState = 'unavailable';
  });
  plate.appendChild(img); img.src = candidates[next++];
}
function discoveryTile(entry, kind) {
  const button = document.createElement('button'); button.type = 'button'; button.className = 'lib-provider';
  const id = String(entry.id); button.dataset[kind] = id;
  const selected = kind === 'provider' ? providerMatchesId(entry, state.provider) : state.network === id;
  button.setAttribute('aria-pressed', String(selected));
  appendDiscoveryLogo(button, entry);
  const name = document.createElement('span'); name.className = 'lib-provider-name'; name.textContent = entry.name; button.appendChild(name);
  if (kind === 'network' && entry.country) {
    const origin = document.createElement('span'); origin.className = 'lib-network-origin'; origin.textContent = `Origin: ${entry.country}`; button.appendChild(origin);
  }
  button.addEventListener('click', () => kind === 'network' ? selectNetwork(id) : selectProvider(id));
  return button;
}
function appendAllDiscoveryTile(host, kind) {
  const all = document.createElement('button'); all.className = 'lib-provider lib-provider-all'; all.type = 'button'; all.dataset[kind] = '';
  const glyph = document.createElement('span'); glyph.className = 'lib-provider-all-icon'; glyph.textContent = '◎'; glyph.setAttribute('aria-hidden', 'true');
  const label = document.createElement('span'); label.className = 'lib-provider-name'; label.textContent = kind === 'network' ? 'All networks' : 'All services';
  all.appendChild(glyph); all.appendChild(label);
  all.setAttribute('aria-pressed', String(!state[kind] && !state[kind === 'network' ? 'provider' : 'network']));
  all.addEventListener('click', () => kind === 'network' ? selectNetwork('') : selectProvider('')); host.appendChild(all);
}
const FEATURED_PROVIDER_BRANDS = ['netflix', 'amazon prime video', 'disney plus', 'apple tv', 'hbo max',
  'hulu', 'paramount+', 'peacock', 'crunchyroll', 'amc+', 'mgm+'];
function providerPickerRowsFor(providers, query, expanded, selectedId) {
  const search = String(query || '').trim().toLocaleLowerCase();
  if (search) return providers.filter(p => [p.name, ...(Array.isArray(p.aliases) ? p.aliases : [])]
    .some(name => typeof name === 'string' && name.toLocaleLowerCase().includes(search)));
  if (expanded) return providers;
  const featured = FEATURED_PROVIDER_BRANDS.map(brand => providers.find(p => providerBrandKeyFor(p) === brand)).filter(Boolean);
  for (const provider of providers) {
    if (featured.length >= 11) break;
    if (!featured.includes(provider)) featured.push(provider);
  }
  // Place the current brand first so long-tail and legacy selections also
  // remain visible in the smaller mobile roster.
  const selected = providers.find(p => providerMatchesId(p, selectedId));
  return selected ? [selected, ...featured.filter(p => p !== selected)].slice(0, 11) : featured.slice(0, 11);
}
function toggleProviderPicker() {
  if (discoveryState.pickerMode !== 'streaming') return;
  discoveryState.providerExpanded = !discoveryState.providerExpanded;
  filterProviders();
}
function filterProviders() {
  if (discoveryState.pickerMode !== 'streaming') return;
  const query = discoveryState.providerQuery.trim();
  const host = document.getElementById('libProviders');
  const focused = host.contains(document.activeElement) ? document.activeElement.dataset.provider : null;
  host.replaceChildren();
  host.classList.toggle('is-compact', !discoveryState.providerExpanded && !query);
  host.classList.toggle('is-searching', !!query);
  appendAllDiscoveryTile(host, 'provider');
  const providers = providerPickerRowsFor(discoveryState.providers, query, discoveryState.providerExpanded, state.provider);
  for (const provider of providers) host.appendChild(discoveryTile(provider, 'provider'));
  const more = document.getElementById('libProviderMore');
  if (more) {
    more.hidden = !!query || discoveryState.providers.length <= 11;
    more.textContent = discoveryState.providerExpanded ? 'Show less' : `Browse all ${fmtNum(discoveryState.providers.length)} services`;
    more.setAttribute('aria-expanded', String(discoveryState.providerExpanded));
  }
  if (focused != null) {
    const next = [...host.children].find(button => button.dataset.provider === focused);
    (next || (more && !more.hidden ? more : document.getElementById('libProviderSearch')))?.focus({preventScroll:true});
  }
  host.setAttribute('aria-busy', 'false');
  document.querySelector('.lib-providers').classList.toggle('has-picker-notice', !discoveryState.providers.length || !providers.length);
  document.getElementById('libProviderStatus').textContent = !discoveryState.providers.length
    ? 'Provider browsing is unavailable. You can still search indexed releases below.'
    : !providers.length ? 'No streaming services match your search.' : `Streaming availability in ${regionName(state.region)}. Choose a service to browse or search its catalogue.`;
}
function renderNetworkPicker() {
  if (discoveryState.pickerMode !== 'networks') return;
  const host = document.getElementById('libProviders'); host.replaceChildren(); appendAllDiscoveryTile(host, 'network');
  host.classList.remove('is-compact', 'is-searching');
  const providerMore = document.getElementById('libProviderMore'); if (providerMore) providerMore.hidden = true;
  for (const network of discoveryState.networks) host.appendChild(discoveryTile(network, 'network'));
  host.setAttribute('aria-busy', String(discoveryState.networkLoading));
  document.querySelector('.lib-providers').classList.toggle('has-picker-notice', discoveryState.networkLoading || !discoveryState.networkAvailable || discoveryState.networkPartial || !discoveryState.networks.length);
  const more = document.getElementById('libNetworkMore');
  const retryEmpty = discoveryState.networkPartial && !discoveryState.networks.length && discoveryState.networkLoaded;
  more.hidden = !retryEmpty && !discoveryState.networkHasNext && (discoveryState.networkAvailable || !discoveryState.networkLoaded);
  more.disabled = discoveryState.networkLoading;
  more.textContent = discoveryState.networkLoading ? 'Loading networks…' : discoveryState.networkAvailable && !retryEmpty ? 'More networks' : 'Retry networks';
  document.getElementById('libProviderStatus').textContent = discoveryState.networkLoading ? 'Loading TV and cable networks…'
    : !discoveryState.networkAvailable ? 'Network browsing is temporarily unavailable. You can still search indexed releases below.'
    : retryEmpty ? 'Network details are temporarily unavailable. Retry to load this list.'
    : !discoveryState.networks.length ? `No networks match “${discoveryState.networkQuery}”. Try another name.`
    : `${discoveryState.networks.length}${discoveryState.networkTotal ? ` of ${discoveryState.networkTotal}` : ''} networks${discoveryState.networkQuery ? ' matching your search' : ''}. Choose a network to browse its series.${discoveryState.networkPartial ? ' Some network details are unavailable; this list is partial.' : ''}`;
}
function updateDiscoveryModeControls() {
  const networks = discoveryState.pickerMode === 'networks';
  document.getElementById('libStreamingMode').setAttribute('aria-pressed', String(!networks));
  document.getElementById('libNetworkMode').setAttribute('aria-pressed', String(networks));
  document.getElementById('libProviderRegion').hidden = networks;
  document.getElementById('libProviderAttribution').hidden = networks;
  document.getElementById('libNetworkAttribution').hidden = !networks;
  document.querySelector('.lib-providers').classList.toggle('is-network-mode', networks);
  const search = document.getElementById('libProviderSearch');
  search.placeholder = networks ? 'Find a TV or cable network…' : 'Find a streaming service…';
  search.setAttribute('aria-label', networks ? 'Find a TV or cable network' : 'Find a streaming service');
  document.getElementById('libProviders').setAttribute('aria-label', networks ? 'TV and cable networks' : 'Streaming services');
  if (networks) { const more = document.getElementById('libProviderMore'); if (more) more.hidden = true; }
  if (!networks) document.getElementById('libNetworkMore').hidden = true;
}
function setDiscoveryMode(mode) {
  if (!['streaming', 'networks'].includes(mode)) return;
  const changed = discoveryState.pickerMode !== mode;
  discoveryState.pickerMode = mode;
  if (changed) document.getElementById('libProviderSearch').value = mode === 'networks' ? discoveryState.networkQuery : discoveryState.providerQuery;
  updateDiscoveryModeControls();
  if (mode === 'networks') { if (!discoveryState.networkLoaded && !discoveryState.networkLoading) loadNetworks(); else if (changed) renderNetworkPicker(); }
  else if (changed) filterProviders();
}
function searchDiscoveryPicker() {
  const query = document.getElementById('libProviderSearch').value.trim();
  if (discoveryState.pickerMode === 'streaming') { discoveryState.providerQuery = query; filterProviders(); return; }
  discoveryState.networkQuery = query;
  clearTimeout(discoveryState.networkSearchTimer);
  if (discoveryState.networkController) discoveryState.networkController.abort();
  ++discoveryState.networkSeq; // Invalidate the old query even before the debounce completes.
  discoveryState.networks = []; discoveryState.networkLoading = true; discoveryState.networkHasNext = false;
  renderNetworkPicker();
  discoveryState.networkSearchTimer = setTimeout(() => loadNetworks(), 250);
}
async function loadNetworks(append = false) {
  clearTimeout(discoveryState.networkSearchTimer); discoveryState.networkSearchTimer = null;
  if (discoveryState.networkPartial && !discoveryState.networks.length) append = false;
  if (append && discoveryState.networkAvailable && (!discoveryState.networkHasNext || discoveryState.networkLoading)) return;
  append = append && discoveryState.networkAvailable && discoveryState.networkLoaded;
  if (discoveryState.networkController) discoveryState.networkController.abort();
  const controller = new AbortController(), seq = ++discoveryState.networkSeq;
  discoveryState.networkController = controller; discoveryState.networkLoading = true;
  const page = append ? discoveryState.networkPage + 1 : 1;
  if (!append) { discoveryState.networks = []; discoveryState.networkHasNext = false; }
  renderNetworkPicker();
  const params = new URLSearchParams({q:discoveryState.networkQuery, page});
  const data = await api(`/api/discovery/networks?${params}`, {signal:controller.signal});
  if (controller.signal.aborted || seq !== discoveryState.networkSeq) return;
  discoveryState.networkLoading = false; discoveryState.networkLoaded = true;
  discoveryState.networkAvailable = !!(data && data.available);
  discoveryState.networkHasNext = !!(data && (data.hasNext || data.hasNextPage));
  discoveryState.networkPartial = !!(data && data.partial) || (append && discoveryState.networkPartial);
  discoveryState.networkTotal = Number.isFinite(data && data.total) ? data.total : 0;
  if (discoveryState.networkAvailable) {
    const known = new Set(discoveryState.networks.map(n => String(n.id)));
    for (const network of data.networks || []) {
      if (!network || !/^[1-9]\d{0,9}$/.test(String(network.id)) || Number(network.id) > 2147483647 || !network.name) continue;
      const id = String(network.id); discoveryState.networkNames.set(id, String(network.name));
      if (!known.has(id)) { discoveryState.networks.push({...network, id, name:String(network.name)}); known.add(id); }
    }
    discoveryState.networkPage = page;
  }
  renderNetworkPicker(); renderActiveLibraryFilters();
}
function regionName(code) { return (discoveryState.regions.find(r => r.code === code) || {}).name || code; }
async function loadProviders() {
  const seq = ++discoveryState.providerSeq;
  const data = await api(`/api/discovery/providers?region=${encodeURIComponent(state.region)}`);
  if (seq !== discoveryState.providerSeq) return false;
  discoveryState.providers = data && data.available ? (data.providers || []) : [];
  discoveryState.regions = data && data.regions && data.regions.length ? data.regions : [{ code:'US', name:'United States' }];
  const select = document.getElementById('libProviderRegion'); select.replaceChildren();
  for (const r of discoveryState.regions) { const option = document.createElement('option'); option.value = r.code; option.textContent = r.name; select.appendChild(option); }
  const accountRegion=document.getElementById('acctPrefRegion');
  if(accountRegion) { accountRegion.replaceChildren(); for(const r of discoveryState.regions) {const option=document.createElement('option');option.value=r.code;option.textContent=r.name;accountRegion.appendChild(option);} renderAccountPreferences(); }
  select.value = state.region;
  if (!select.value) { const option = document.createElement('option'); option.value = state.region; option.textContent = state.region; select.appendChild(option); select.value = state.region; }
  filterProviders(); syncProviderControls(); renderActiveLibraryFilters();
  return !!(data && data.available);
}
function resetDiscoveryFacets() {
  state.page = 0; state.sort = 'seeders';
  // Release-quality facets belong to indexed releases, not metadata catalogues.
  state.genres.clear(); state.qualities.clear(); state.sources.clear(); state.features.clear();
  state.yearMin = ''; state.yearMax = ''; state.hideForeign = false; state.hideUnmatched = true;
  state._preAnimeHideUnmatched = null;
}
function selectProvider(id) {
  state.provider = id; state.network = ''; resetDiscoveryFacets();
  if (!['', 'movie', 'tv_show'].includes(state.type)) state.type = '';
  setDiscoveryMode('streaming'); loadLibrary({ push:true });
}
function selectNetwork(id) {
  state.network = id; state.provider = ''; resetDiscoveryFacets(); state.type = id ? 'tv_show' : '';
  setDiscoveryMode('networks'); loadLibrary({ push:true });
}
async function changeProviderRegion(code) {
  if (!/^[A-Z]{2}$/.test(code)) return;
  const selectedId = state.provider, previousProviders = discoveryState.providers.slice();
  state.region = code; state.page = 0;
  document.getElementById('libProviderRegion').value = code;
  const seq = discoveryState.providerSeq + 1;
  const loaded = await loadProviders();
  if (seq !== discoveryState.providerSeq || code !== state.region) return;
  if (loaded && state.provider) {
    // A user may choose another brand while the country request is pending.
    // Resolve that latest choice, rather than restoring the captured selection.
    const selected = previousProviders.find(entry => providerMatchesId(entry, state.provider));
    const current = providerForId(state.provider) || discoveryState.providers.find(entry => providerBrandMatches(entry, selected));
    if (current || selected || state.provider === selectedId) {
      state.provider = current ? String(current.id) : '';
      if (!current && typeof toast === 'function') toast(`${selected ? selected.name : 'This service'} is unavailable in ${regionName(code)}. Showing all services.`);
    }
  }
  loadSpotlight();
  if (selectedId || state.provider) loadLibrary({ push:true });
}
function syncProviderControls() {
  const selection = state.network ? `network:${state.network}` : state.provider ? `provider:${state.provider}` : '';
  if (selection !== discoveryState.selectionKey) {
    discoveryState.selectionKey = selection;
    if (selection) setDiscoveryMode(state.network ? 'networks' : 'streaming');
    if (discoveryState.pickerMode === 'streaming') filterProviders();
  }
  const browse = document.getElementById('libBrowse');
  browse.classList.toggle('lib-provider-catalog', !!(state.provider || state.network));
  browse.classList.toggle('has-title-query', !!state.q);
  document.getElementById('libSearch').placeholder = state.network ? `Search ${networkName(state.network)} series…`
    : state.provider ? `Search ${providerName(state.provider)} movies and series…` : 'Search titles, seasons, and releases…';
  const provider = providerForId(state.provider);
  document.querySelectorAll('#libProviders button').forEach(button => {
    const selected = discoveryState.pickerMode === 'networks' ? state.network : String(provider ? provider.id : state.provider);
    const other = discoveryState.pickerMode === 'networks' ? state.provider : state.network;
    const id = discoveryState.pickerMode === 'networks' ? button.dataset.network : button.dataset.provider;
    button.setAttribute('aria-pressed', String((id || '') === (selected || '') && !other));
  });
  const select = document.getElementById('libProviderRegion'); if (select.value !== state.region) { select.value = state.region; loadProviders(); loadSpotlight(); }
  renderCatalogueHeading();
}
function renderCatalogueHeading() {
  const heading = document.getElementById('libCatalogueHeading');
  if (!heading) return;
  heading.hidden = isHome();
  if (heading.hidden) return;
  const name = state.network ? networkName(state.network) : state.provider ? providerName(state.provider) : '';
  document.getElementById('libCatalogueTitle').textContent = name ? `Titles on ${name}`
    : state.q ? 'Search results' : state.type === 'movie' ? 'Movies' : state.type === 'tv_show' ? 'Series' : 'Library titles';
  document.getElementById('libCatalogueSubtitle').textContent = [
    state.q ? `Results for “${state.q}”` : '',
    state.network ? 'Series catalogue · Open a title to check indexed releases'
      : state.provider ? `${regionName(state.region)} · Open a title to check indexed releases` : '',
  ].filter(Boolean).join(' · ');
  const logo = document.getElementById('libCatalogueLogo'), entry = state.provider ? providerForId(state.provider) : null;
  if (logo) {
    const key = entry ? `${entry.id}:${discoveryLogoCandidatesFor(entry).join('|')}` : '';
    if (logo.dataset.logoKey !== key) { logo.replaceChildren(); logo.dataset.logoKey = key; if (entry) appendDiscoveryLogo(logo, entry); }
    logo.hidden = !entry || !discoveryLogoCandidatesFor(entry).length;
  }
}
function catalogueCard(item) {
  const title = discoveryItemFor(item); if (!title) return '';
  discoveryState.titles.set(`${title.type}:${title.id}`, title);
  return `<button class="lib-card" type="button" onclick="openDiscoveryTitle('${title.type}:${title.id}')" aria-label="Find indexed releases for ${escAttr(title.title)}">
    <div class="lib-poster">${posterFallbackHtml(title.title, title.type)}${title.poster ? `<img class="lib-poster-img" src="${escAttr(title.poster)}" alt="" loading="lazy" decoding="async" onerror="this.remove()">` : ''}<span class="lib-type-badge">${escHtml(typeLabel(title.type))}</span><span class="lib-catalog-badge">Find releases ↗</span></div>
    <div class="lib-card-info"><div class="lib-card-title">${escHtml(title.title)}</div><div class="lib-card-sub">${escHtml(title.year)} · ${escHtml(typeLabel(title.type))}</div></div></button>`;
}
async function loadProviderLibrary(seq, signal) {
  setGridLoading(true);
  const grid = document.getElementById('libraryGrid'), note = document.getElementById('libResultNote');
  const isNetwork = !!state.network, selectedId = isNetwork ? state.network : state.provider;
  renderCatalogueHeading();
  grid.innerHTML = Array.from({length:12}, () => '<div class="lib-skeleton" aria-hidden="true"></div>').join('');
  note.textContent = 'Loading titles…';
  document.getElementById('libPrev').disabled = true; document.getElementById('libNext').disabled = true;
  document.getElementById('libPaginationInfo').textContent = '';
  document.getElementById('libPager').style.display = '';
  const params = discoveryCatalogueParamsFor(state);
  const data = await api(`/api/discovery?${params}`, {signal});
  if ((signal && signal.aborted) || seq !== _navSeq) return;
  setGridLoading(false);
  discoveryState.titles.clear(); groupCache.clear();
  if (!data || !data.available) {
    state.hasNext = false;
    note.textContent = `${isNetwork ? 'Network' : 'Service'} catalogue is temporarily unavailable.`;
    grid.innerHTML = `<div class="lib-empty"><h3>We couldn’t load this catalogue</h3><p>Try again, or return to the indexed library.</p><button class="lib-btn" onclick="loadLibrary({keepPage:true})">Retry catalogue</button><button class="lib-btn" onclick="${isNetwork ? 'selectNetwork' : 'selectProvider'}('')">Browse the library</button></div>`;
    document.getElementById('libPaginationInfo').textContent = '';
    document.getElementById('libPager').style.display = 'none';
    return;
  }
  if (isNetwork && data.networkName) discoveryState.networkNames.set(selectedId, String(data.networkName));
  if (!isNetwork && data.providerName) discoveryState.providerNames.set(String(selectedId), String(data.providerName));
  state.hasNext = !!(data.hasNextPage || data.hasNext);
  const cards = (data.items || []).map(catalogueCard).filter(Boolean);
  grid.innerHTML = cards.join('') || `<div class="lib-empty"><h3>${state.q ? 'No matching titles' : 'No titles to show'}</h3><p>${state.q ? 'Try a shorter title or clear your title search.' : 'Try another service, network, or country.'}</p>${state.q ? '<button class="lib-btn" onclick="clearLibSearch()">Clear title search</button>' : ''}</div>`;
  note.textContent = `${fmtNum(cards.length)} ${cards.length === 1 ? 'title' : 'titles'} on this page.${data.partial ? ' Some catalogue checks are unavailable; these results are partial.' : ''}`;
  syncProviderControls(); renderActiveLibraryFilters();
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
  if (_reducedMotion() || discoveryState.paused || discoveryState.hovering || document.hidden || !discoveryState.visible || discoveryState.slides.length < 2) return;
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
  if (changed && typeof img.animate === 'function' && !_reducedMotion()) {
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
function syncSpotlightPauseControl() {
  const button = document.getElementById('libSpotlightPause');
  const reduced = _reducedMotion();
  button.disabled = reduced;
  button.textContent = reduced || discoveryState.paused ? '▶' : 'Ⅱ';
  button.setAttribute('aria-label', reduced ? 'Featured title rotation disabled by reduced motion' : `${discoveryState.paused ? 'Resume' : 'Pause'} featured title rotation`);
}
function toggleSpotlightPause(force) {
  discoveryState.paused = typeof force === 'boolean' ? force : !discoveryState.paused;
  syncSpotlightPauseControl();
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
    document.getElementById('libSpotlightLabel').textContent = 'IN THE SPOTLIGHT';
    document.getElementById('libSpotlightMeta').textContent = '';
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
  const refreshMotion = () => { syncSpotlightPauseControl(); scheduleSpotlight(); };
  refreshMotion();
  matchMedia('(prefers-reduced-motion: reduce)').addEventListener('change', refreshMotion);
  window.addEventListener('bitagent:preferences', refreshMotion);
  if (typeof IntersectionObserver !== 'undefined') new IntersectionObserver(entries => { discoveryState.visible = entries[0].isIntersecting; scheduleSpotlight(); }).observe(hero);
  loadProviders(); loadSpotlight(); syncProviderControls();
  // The base controller boots first; supersede its initial request for provider permalinks.
  if (state.provider || state.network) loadLibrary({fromPop:true, keepPage:true});
}
if (typeof module !== 'undefined' && module.exports) module.exports = {discoveryItemFor, discoveryGroupFor, discoveryLogoUrlFor, discoveryLogoCandidatesFor, discoveryCatalogueParamsFor};

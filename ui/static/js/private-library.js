'use strict';
(() => {
  const $ = id => document.getElementById(id);
  const admin = document.body.dataset.admin === 'true';
  const pageSize = 25;
  let offset = 0, total = 0, generation = 0;
  const status = text => { $('status').textContent = text; };
  const bytes = n => {
    if (n == null) return '—';
    let value = Number(n), unit = 0;
    const units = ['B', 'KiB', 'MiB', 'GiB', 'TiB'];
    while (value >= 1024 && unit < units.length - 1) { value /= 1024; unit++; }
    return `${value.toFixed(unit ? 1 : 0)} ${units[unit]}`;
  };
  async function api(path, options) {
    const response = await fetch(path, {credentials: 'same-origin', ...options});
    if (!response.ok) {
      let message = `Request failed (${response.status})`;
      try { message = (await response.json()).detail || message; } catch (_) { /* keep status */ }
      throw new Error(message);
    }
    return response.json();
  }
  function table(target, headings, rows) {
    const wrapper = document.createElement('div'); wrapper.className = 'scroll';
    const grid = document.createElement('table');
    const head = document.createElement('thead'), header = document.createElement('tr');
    headings.forEach(text => { const cell = document.createElement('th'); cell.scope = 'col'; cell.textContent = text; header.append(cell); });
    head.append(header); grid.append(head);
    const body = document.createElement('tbody');
    rows.forEach(values => {
      const row = document.createElement('tr');
      values.forEach(value => { const cell = document.createElement('td'); if (value instanceof Node) cell.append(value); else cell.textContent = String(value ?? '—'); row.append(cell); });
      body.append(row);
    });
    grid.append(body); wrapper.append(grid); target.replaceChildren(wrapper);
    if (!rows.length) { const empty = document.createElement('p'); empty.textContent = 'No records to show.'; target.append(empty); }
  }
  async function metrics() {
    const result = await api(admin ? '/api/private/metrics' : '/api/account/private/metrics');
    const headings = [...(admin ? ['Member'] : []), 'Release', 'Link requests', 'Observed upload', 'Observed download', 'Ratio', 'Completed events'];
    table($('metrics'), headings, result.items.map(item => [...(admin ? [item.user_id] : []), item.title, item.issued, bytes(item.uploaded), bytes(item.downloaded), item.ratio == null ? '—' : item.ratio.toFixed(2), item.completed]));
  }
  async function releases() {
    const current = ++generation;
    const params = new URLSearchParams(new FormData($('search-form')));
    params.set('offset', String(offset)); params.set('limit', String(pageSize));
    status('Loading releases…');
    const result = await api('/api/library/private?' + params);
    if (current !== generation) return;
    total = result.total;
    const rows = result.items.map(item => {
      const actions = document.createElement('div'); actions.className = 'actions';
      const download = document.createElement('a'); download.textContent = 'Torrent'; download.href = `/api/library/private/${encodeURIComponent(item.id)}/torrent`; actions.append(download);
      const copy = document.createElement('button'); copy.textContent = 'Copy magnet';
      copy.addEventListener('click', async () => {
        copy.disabled = true;
        try {
          const result = await api(`/api/library/private/${encodeURIComponent(item.id)}/magnet`);
          await navigator.clipboard.writeText(result.magnetUri);
          status('Personal magnet copied. Keep its tracker credential private.');
          await metrics();
        } catch (error) { status(error.message); } finally { copy.disabled = false; }
      });
      actions.append(copy);
      return [item.title, item.kind, bytes(item.size), `${item.seeders} / ${item.leechers}`, actions];
    });
    table($('releases'), ['Release', 'Type', 'Size', 'Seeds / peers', 'Download'], rows);
    $('page-status').textContent = total ? `${offset + 1}–${offset + rows.length} of ${total}` : 'No available releases';
    $('prev-page').disabled = offset === 0; $('next-page').disabled = offset + pageSize >= total;
    status('Only seeder-verified releases appear here.');
  }
  async function members() {
    const rows = await api('/api/private/members');
    table($('members'), ['SSO member ID', 'Access'], rows.map(item => [item.user_id, item.active ? 'Approved' : 'Suspended']));
  }
  async function start() {
    if (admin) {
      $('member-form').addEventListener('submit', async event => {
        event.preventDefault(); const form = new FormData(event.target);
        try {
          await api('/api/private/members/' + encodeURIComponent(form.get('user').trim()), {method: 'PUT', headers: {'Content-Type': 'application/json'}, body: JSON.stringify({active: form.get('active') === 'true'})});
          status('Membership saved.'); await members();
        } catch (error) { status(error.message); }
      });
      await members();
    } else {
      const account = await api('/api/account/private');
      $('connection').textContent = account.privateTorznabUrl;
      $('rotate-key').addEventListener('click', async () => {
        if (!window.confirm('Rotate your key? Existing indexer connections and torrent tracker credentials will need updating.')) return;
        try {
          const result = await api('/api/account/api-key', {method: 'POST', headers: {'Content-Type': 'application/json'}, body: JSON.stringify({name: 'private library'})});
          $('secret').value = result.apiKeySecret; $('secret-label').hidden = false;
          status('Personal key created. Copy it now; it cannot be displayed again.');
        } catch (error) { status(error.message); }
      });
      $('search-form').addEventListener('submit', event => { event.preventDefault(); offset = 0; releases().catch(error => status(error.message)); });
      $('prev-page').addEventListener('click', () => { offset = Math.max(0, offset - pageSize); releases().catch(error => status(error.message)); });
      $('next-page').addEventListener('click', () => { if (offset + pageSize < total) { offset += pageSize; releases().catch(error => status(error.message)); } });
      await releases();
    }
    await metrics();
  }
  start().catch(error => status(error.message));
})();

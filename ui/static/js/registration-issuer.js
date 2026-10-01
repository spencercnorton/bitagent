(function (root) {
  'use strict';
  const path = '/api/registration/issuer';
  function snapshot(data, owner) {
    if (!data || typeof owner !== 'string' || !owner || data.accountId !== owner ||
        typeof data.active !== 'boolean' || typeof data.suspended !== 'boolean' ||
        (data.active && data.suspended)) throw new Error('Account changed. Reload this page.');
    return {active: data.active, suspended: data.suspended};
  }
  function createController({owner, api, view}) {
    let state = null, busy = false, closed = false;
    function update() { if (!closed) view.state(state, busy); }
    async function load() {
      if (closed || busy) return;
      busy = true; state = null; update();
      try {
        const data = await api('GET');
        if (closed) return;
        state = snapshot(data, owner);
        view.status(state.active ? 'Invitations are enabled for your account.' :
          state.suspended ? 'This membership is suspended. Contact an operator.' :
          'Choose Enable invitations to allow your account to create invite codes.');
      } catch (_) {
        if (!closed) view.status('Could not verify your account. Reload this page before continuing.');
      } finally { busy = false; update(); }
    }
    async function admit() {
      if (closed || busy || !state || state.active || state.suspended) return;
      busy = true; update();
      try {
        const data = await api('PUT', {expectedAccountId: owner});
        if (closed) return;
        state = snapshot(data, owner);
        if (!state.active) throw new Error('Admission unavailable');
        view.status('Invitations are enabled for your account.');
      } catch (_) {
        state = null;
        if (!closed) view.status('Could not confirm access. Reload status before trying again.');
      } finally { busy = false; update(); }
    }
    function close() { closed = true; state = null; }
    return {load, admit, close};
  }
  if (typeof module === 'object' && module.exports) module.exports = {snapshot, createController};
  if (!root.document) return;
  const document = root.document, owner = document.body.dataset.registrationAccountId;
  const button = document.getElementById('issuerAdmit'), refresh = document.getElementById('issuerRefresh');
  const controller = createController({owner,
    api: async function (method, body) {
      const options = {method, credentials: 'same-origin', cache: 'no-store', redirect: 'error'};
      if (body) { options.headers = {'Content-Type': 'application/json'}; options.body = JSON.stringify(body); }
      const response = await root.fetch(path, options);
      if (!response.ok) throw new Error('Issuer request unavailable');
      return response.json();
    },
    view: {
      status: message => { document.getElementById('issuerStatus').textContent = message; },
      state: (state, busy) => {
        button.disabled = busy || !state || state.active || state.suspended;
        refresh.disabled = busy;
        document.getElementById('issuerReady').hidden = !state || !state.active;
      },
    },
  });
  document.getElementById('issuerForm').addEventListener('submit', event => { event.preventDefault(); controller.admit(); });
  refresh.addEventListener('click', () => controller.load());
  root.addEventListener('pagehide', () => controller.close());
  controller.load();
})(typeof window !== 'undefined' ? window : globalThis);

/* Optional invitation pages. Secrets stay in memory and never enter telemetry. */
'use strict';
(function (root) {
  const TOKEN = /^bi_[A-Za-z0-9_-]{43}$/;
  const ID = /^[a-f0-9]{32}$/;
  const STATES = new Set(['pending', 'expired', 'revoked', 'redeemed']);
  const UUID = /^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/;
  const ORDER_STATES = new Set(['prepared','unknown','open','manual_hold','credited','invalidated','expired']);
  const UNRESOLVED = new Set(['prepared','unknown','open','manual_hold']);
  const integer = value => Number.isSafeInteger(value) && value >= 0;
  const timestamp = value => typeof value === 'string' && /^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(?:\.\d{1,6})?Z$/.test(value) && Number.isFinite(Date.parse(value)) && Date.parse(value) > 0;
  const personal = value => typeof value === 'string' && value.length > 0 && value.length <= 200 && !['anonymous', 'api-client'].includes(value);
  const fail = code => Object.assign(new Error('Invitation request unavailable'), {code});
  const isToken = value => typeof value === 'string' && value.length === 46 && TOKEN.test(value);

  function fragmentToken(hash) {
    const value = typeof hash === 'string' && hash.startsWith('#') ? hash.slice(1) : '';
    return isToken(value) ? value : null;
  }
  function takeFragment(location, history) {
    const token = fragmentToken(location.hash);
    // Remove even an invalid fragment before any request or other page code.
    history.replaceState(null, '', location.pathname + location.search);
    return token;
  }
  function signInTarget(value) {
    try {
      if (typeof value !== 'string' || value.length > 2048) return null;
      const url = new URL(value);
      return url.protocol === 'https:' && !url.port && !url.username && !url.password &&
        url.pathname === '/invitations/start' && !url.search && !url.hash && url.href === value ? value : null;
    } catch (_) { return null; }
  }
  function snapshot(data, owner) {
    if (!data || !personal(owner) || data.accountId !== owner) throw fail(409);
    if (data.enabled === false) return {enabled:false, accountId:owner};
    if (data.enabled !== true || data.schemaVersion !== 1 || typeof data.unlimited !== 'boolean' || !integer(data.year) || data.year < 1970 || data.year > 9998 ||
        !integer(data.annualLimit) || !integer(data.annualUsed) || !integer(data.paidCredits) ||
        (data.unlimited ? data.annualRemaining !== null : (!integer(data.annualRemaining) || data.annualRemaining > data.annualLimit)) ||
        !data.price || !integer(data.price.amountMinor) || data.price.currency !== 'USD' || typeof data.checkoutAvailable !== 'boolean' || (data.unlimited && data.checkoutAvailable) ||
        !Array.isArray(data.invitations) || data.invitations.length > 100) throw fail('shape');
    const ids = new Set();
    for (const item of data.invitations) {
      if (!item || !ID.test(item.id) || ids.has(item.id) || !timestamp(item.createdAt) || !timestamp(item.expiresAt) || Date.parse(item.expiresAt) <= Date.parse(item.createdAt) || !STATES.has(item.status)) throw fail('shape');
      ids.add(item.id);
    }
    return data;
  }
  function issued(data, origin) {
    if (!data || !ID.test(data.id) || !isToken(data.token) || !timestamp(data.createdAt) || !timestamp(data.expiresAt) || Date.parse(data.expiresAt) <= Date.parse(data.createdAt) || !['annual','paid'].includes(data.creditSource)) throw fail('shape');
    let url;
    try { url = new URL(data.shareUrl); } catch (_) { throw fail('shape'); }
    if (url.protocol !== 'https:' || url.origin !== origin || url.username || url.password || url.pathname !== '/invite' || url.search || url.hash !== '#' + data.token) throw fail('shape');
    return {id:data.id, url:url.href, expiresAt:data.expiresAt};
  }
  function checkoutTarget(value) {
    try {
      if (typeof value !== 'string' || value.length > 4096 || /[^\x21-\x7e]/.test(value)) return null;
      const url = new URL(value);
      return url.protocol === 'https:' && url.host === 'checkout.stripe.com' && !url.username && !url.password &&
        /^\/c\/pay\/cs_[A-Za-z0-9_]+$/.test(url.pathname) && !url.search && url.href === value ? value : null;
    } catch (_) { return null; }
  }
  function orders(data, owner) {
    if (!data || data.schemaVersion !== 1 || data.accountId !== owner || !personal(owner) || !Array.isArray(data.orders) || data.orders.length > 20) throw fail(409);
    const ids = new Set(), requests = new Set(); let unresolved = 0;
    for (const order of data.orders) {
      if (!order || !ID.test(order.orderId) || !UUID.test(order.requestId) || ids.has(order.orderId) || requests.has(order.requestId) ||
          !ORDER_STATES.has(order.status) || !timestamp(order.createdAt) || typeof order.retryAllowed !== 'boolean' ||
          ((!UNRESOLVED.has(order.status) || order.status === 'manual_hold') && order.retryAllowed)) throw fail('shape');
      ids.add(order.orderId); requests.add(order.requestId); if (UNRESOLVED.has(order.status)) unresolved++;
    }
    if (unresolved > 1) throw fail('shape');
    return data.orders;
  }
  function checkoutResult(data, owner, requestId) {
    if (!data || data.accountId !== owner || data.requestId !== requestId || !ID.test(data.orderId) ||
        !['open','pending','manual_hold','credited','invalidated','expired'].includes(data.status) ||
        (data.status === 'open' ? !checkoutTarget(data.checkoutUrl) : data.checkoutUrl !== null)) throw fail('shape');
    return data;
  }
  function date(value) {
    return new Date(value).toLocaleString(undefined, {dateStyle:'medium', timeStyle:'short'});
  }

  async function request(fetcher, path, method = 'GET', body = null) {
    const controller = new AbortController();
    const timer = setTimeout(() => controller.abort(), 10000);
    try {
      const response = await fetcher(path, {method, credentials:'same-origin', mode:'same-origin', redirect:'error', cache:'no-store', signal:controller.signal,
        headers:{Accept:'application/json', ...(body === null ? {} : {'Content-Type':'application/json'})}, ...(body === null ? {} : {body:JSON.stringify(body)})});
      if (!response.ok) throw fail(response.status);
      let raw;
      if (response.body && response.body.getReader) {
        const reader = response.body.getReader();
        const chunks = []; let size = 0;
        try {
          while (true) {
            const chunk = await reader.read();
            if (chunk.done) break;
            size += chunk.value.byteLength;
            if (size > 65536) throw fail('shape');
            chunks.push(chunk.value);
          }
          const bytes = new Uint8Array(size); let at = 0;
          for (const chunk of chunks) { bytes.set(chunk, at); at += chunk.byteLength; }
          raw = new TextDecoder('utf-8', {fatal:true}).decode(bytes);
        } finally { await reader.cancel().catch(() => {}); reader.releaseLock(); }
      } else {
        raw = await response.text();
        if (raw.length > 65536) throw fail('shape');
      }
      return JSON.parse(raw);
    } finally { clearTimeout(timer); }
  }

  function createController(options) {
    const api = options.api;
    const view = options.view;
    const signInUrl = signInTarget(options.signInUrl);
    const state = {owner:options.owner || '', snapshot:null, fresh:null, token:options.token || null, signInReady:false, autoHandoffAttempted:false, checkoutRequestId:null, orders:[], ordersLoaded:false, paymentUnavailable:false, busy:false, closed:false, generation:0};
    options.token = null;
    function clearSecret() { state.fresh = null; view.secret(null); }
    function clearSignIn() { state.signInReady = false; if (view.signIn) view.signIn(false); }
    function close() { clearSignIn(); state.closed = true; state.generation++; state.token = null; state.checkoutRequestId = null; state.orders = []; clearSecret(); state.snapshot = null; if (view.entry) view.entry(false); view.busy(true); paymentView(); }
    function status(message, error = false) { view.status(message, error); }
    function failure(error, mutation = false) {
      clearSecret(); state.snapshot = null;
      view.invalidate();
      if ([401,403,409].includes(error.code)) {
        state.token = null;
        status(error.code === 409 ? 'Your account changed. Reload this page before continuing.' : 'Sign in with an approved account before continuing.', true);
      } else if (error.code === 404) status('Invitations are unavailable on this site.', true);
      else status(mutation ? 'The request could not be confirmed. Refresh before trying again.' : 'Invitations could not be loaded. Try refreshing.', true);
    }
    function paymentView() {
      if (!view.payment) return;
      const value = state.snapshot, current = state.orders.find(item => UNRESOLVED.has(item.status));
      const held = !!state.checkoutRequestId && !state.orders.some(item => item.requestId === state.checkoutRequestId && !UNRESOLVED.has(item.status));
      const available = !!value && value.enabled && !value.unlimited && value.checkoutAvailable && state.ordersLoaded;
      let text = 'Purchases are currently unavailable.';
      if (value && value.unlimited) text = 'Your account already has unlimited invitations.';
      else if (state.paymentUnavailable) text = 'Payment status is temporarily unavailable. Check again before continuing.';
      else if (current?.status === 'manual_hold') text = 'This purchase needs operator reconciliation. Another purchase is unavailable until it is resolved.';
      else if (current || held) text = 'Checkout is awaiting confirmation. Check payment status before creating another purchase.';
      else if (state.orders[0]?.status === 'credited') text = 'Payment verified. Your available purchased invitations are shown above.';
      else if (state.orders[0]?.status === 'invalidated') text = 'This purchase is unavailable. Its payment did not add an available invitation.';
      else if (state.orders[0]?.status === 'expired') text = 'The previous checkout expired. You can start a new purchase when available.';
      else if (available) text = 'Continue to Stripe to purchase one additional invitation. Payment confirmation adds a credit; create your invitation afterward.';
      const retry = !!current && current.retryAllowed;
      view.payment({text,showBuy:available && (!held && !current || retry),retry:!!current,disabled:state.busy || state.closed,
        showRefresh:!!value && value.enabled && !value.unlimited && (state.orders.length > 0 || held || state.paymentUnavailable)});
    }
    async function readOrders(generation) {
      let value;
      try { value = orders(await api('/api/account/invitations/orders'), state.owner); }
      catch (error) {
        if (state.closed || generation !== state.generation) return;
        state.ordersLoaded = false;
        if ([401,403,409].includes(error.code)) throw error;
        state.paymentUnavailable = error.code !== 404;
        paymentView(); return;
      }
      if (state.closed || generation !== state.generation) return;
      state.orders = value; state.ordersLoaded = true; state.paymentUnavailable = false;
      const current = value.find(item => UNRESOLVED.has(item.status));
      if (current) state.checkoutRequestId = current.requestId;
      else if (value.some(item => item.requestId === state.checkoutRequestId && !UNRESOLVED.has(item.status))) state.checkoutRequestId = null;
      paymentView();
    }
    async function load() {
      if (state.closed || state.busy) return;
      const generation = ++state.generation;
      clearSecret(); state.busy = true; view.busy(true);
      try {
        const value = snapshot(await api('/api/account/invitations'), state.owner);
        if (state.closed || generation !== state.generation) return;
        state.snapshot = value; view.manage(value);
        if (value.enabled) await readOrders(generation);
        if (state.closed || generation !== state.generation) return;
        status(value.enabled ? '' : 'Invitations are unavailable on this site.');
      } catch (error) { if (!state.closed && generation === state.generation) failure(error); }
      finally { if (!state.closed && generation === state.generation) { state.busy = false; view.busy(false, state.snapshot); paymentView(); } }
    }
    async function create(creditSource) {
      if (state.closed || state.busy || !state.snapshot || !state.snapshot.enabled || !['annual','paid'].includes(creditSource)) return;
      if (creditSource === 'paid' ? state.snapshot.paidCredits < 1 : (!state.snapshot.unlimited && state.snapshot.annualRemaining < 1)) return;
      const generation = ++state.generation;
      state.busy = true; view.busy(true); clearSecret(); status('Creating your invitation…');
      try {
        const link = issued(await api('/api/account/invitations', 'POST', {expectedAccountId:state.owner, creditSource}), options.origin);
        // Refresh ownership/allowance before showing a token from a mutation.
        const value = snapshot(await api('/api/account/invitations'), state.owner);
        if (state.closed || generation !== state.generation) return;
        state.snapshot = value; state.fresh = link; view.manage(value); view.secret(link); status('Invitation created. Copy your link now.');
      } catch (error) { if (!state.closed && generation === state.generation) failure(error, true); }
      finally { if (!state.closed && generation === state.generation) { state.busy = false; view.busy(false, state.snapshot); paymentView(); } }
    }
    async function checkout() {
      const value = state.snapshot;
      if (state.closed || state.busy || !value || !value.enabled || value.unlimited || !value.checkoutAvailable || !state.ordersLoaded) return;
      const current = state.orders.find(item => UNRESOLVED.has(item.status));
      if (current && !current.retryAllowed || !current && state.checkoutRequestId) return;
      let requestId;
      try { requestId = current ? current.requestId : options.newRequestId(); }
      catch (_) { status('Checkout is unavailable in this browser.', true); return; }
      if (!UUID.test(requestId || '')) { status('Checkout is unavailable in this browser.', true); return; }
      state.checkoutRequestId = requestId;
      const generation = ++state.generation;
      state.busy = true; view.busy(true); clearSecret(); paymentView(); status('Preparing your checkout…');
      try {
        const result = checkoutResult(await api('/api/account/invitations/checkout','POST',{expectedAccountId:state.owner,requestId}), state.owner, requestId);
        if (state.closed || generation !== state.generation) return;
        const fresh = snapshot(await api('/api/account/invitations'),state.owner);
        if (state.closed || generation !== state.generation) return;
        state.snapshot = fresh; view.manage(fresh);
        await readOrders(generation);
        if (state.closed || generation !== state.generation) return;
        const observed = state.orders.find(item => item.orderId === result.orderId && item.requestId === requestId);
        if (result.status === 'open' && observed?.status === 'open' && fresh.checkoutAvailable && !fresh.unlimited && state.ordersLoaded) {
          options.openCheckout(checkoutTarget(result.checkoutUrl));
        } else status('Checkout status refreshed. A browser return never confirms payment.');
      } catch (error) {
        if (!state.closed && generation === state.generation) {
          state.ordersLoaded = false; state.paymentUnavailable = true;
          if ([401,403,409].includes(error.code)) failure(error, true);
          else status('Checkout could not be confirmed. Check payment status before continuing.', true);
        }
      } finally { if (!state.closed && generation === state.generation) { state.busy = false; view.busy(false,state.snapshot); paymentView(); } }
    }
    async function revoke(id) {
      if (state.closed || state.busy || !state.snapshot || !state.snapshot.enabled || !state.snapshot.invitations.some(item => item.id === id && item.status === 'pending')) return;
      const generation = ++state.generation;
      state.busy = true; view.busy(true); clearSecret();
      try {
        const result = await api('/api/account/invitations/' + id, 'DELETE', {expectedAccountId:state.owner});
        if (!result || result.id !== id || result.status !== 'revoked') throw fail('shape');
        const value = snapshot(await api('/api/account/invitations'), state.owner);
        if (state.closed || generation !== state.generation) return;
        state.snapshot = value; view.manage(value); status('Invitation revoked.');
      } catch (error) { if (!state.closed && generation === state.generation) failure(error, true); }
      finally { if (!state.closed && generation === state.generation) { state.busy = false; view.busy(false, state.snapshot); paymentView(); } }
    }
    async function copy() {
      if (!state.fresh || state.closed) return;
      const value = state.fresh;
      try {
        await options.copy(value.url);
        if (!state.closed && state.fresh === value) status('Invitation link copied.');
      } catch (_) { if (!state.closed && state.fresh === value) status('Select the invitation link and copy it manually.', true); }
    }
    async function enterToken(value) {
      if (state.closed || state.busy || state.autoHandoffAttempted) return;
      clearSignIn(); state.owner = ''; state.token = null;
      if (!isToken(value)) { status('Enter a valid invite code.', true); return; }
      state.token = value;
      await preview();
    }
    async function preview() {
      if (state.closed || state.busy || state.autoHandoffAttempted) return;
      if (!isToken(state.token)) { state.token = null; status('This invitation is unavailable or has expired.', true); return; }
      state.busy = true; view.busy(true);
      if (view.entry) view.entry(false);
      const generation = ++state.generation;
      let handoff = false;
      clearSignIn();
      try {
        const result = await api('/api/invitations/preview', 'POST', {token:state.token});
        if (!result || result.available !== true || !timestamp(result.expiresAt)) { state.token = null; throw fail(404); }
        if (state.closed || generation !== state.generation) return;
        if (options.signInUrl) {
          if (!signInUrl || typeof options.submitSignIn !== 'function') throw fail('shape');
          // Profile enrollment belongs to the identity service for all users.
          // An existing browser session must not bypass its required email.
          state.owner = ''; state.signInReady = true; if (view.signIn) view.signIn(true);
          status('Create your profile to accept this invitation. An email address is required; SSO is optional.');
          handoff = true;
          return;
        }
        const identity = await api('/api/me');
        if (!personal(identity && identity.id) || !['npm-header','forwarded-user'].includes(identity.method)) throw fail(401);
        if (state.closed || generation !== state.generation) return;
        state.owner = identity.id; view.recipient(identity.display || 'your account', result.expiresAt); status('Your invitation is ready to accept.');
      } catch (error) {
        if (!state.closed && generation === state.generation) {
          if (error.code === 401) { state.token = null; status('Sign in with your usual account, then reopen this invitation link.'); }
          else { state.token = null; status('This invitation is unavailable or has expired.', true); }
          if (view.entry) view.entry(true);
        }
      } finally {
        if (!state.closed && generation === state.generation) {
          state.busy = false; view.busy(false, null, !!state.token && personal(state.owner));
          if (handoff) signIn(true);
        }
      }
    }
    function signIn(automatic = false) {
      if (state.closed || state.busy || !state.signInReady || !signInUrl || !isToken(state.token) || automatic && state.autoHandoffAttempted) return;
      const token = state.token;
      if (automatic) state.autoHandoffAttempted = true;
      clearSignIn(); state.busy = true; view.busy(true);
      try {
        options.submitSignIn(token, signInUrl);
        if (state.closed) return;
        if (automatic) {
          // Native submit cannot confirm that browser navigation was allowed.
          // Keep a deliberate fallback, never automatically retry the handoff.
          state.busy = false; state.signInReady = true; if (view.signIn) view.signIn(true);
          view.busy(false, null, false);
          status('Opening profile setup. If this page stays open, select Create your profile to continue. An email address is required; SSO is optional.');
        } else {
          state.token = null;
          status('Opening profile setup…');
        }
      } catch (error) {
        if (state.closed) return;
        if (error && error.code === 'shape') { close(); status('Profile setup could not be opened safely. Reopen your invitation link before trying again.', true); return; }
        state.busy = false; state.signInReady = true; if (view.signIn) view.signIn(true);
        view.busy(false, null, false);
        status('Profile setup could not open automatically. Select Create your profile to continue.', true);
      }
    }
    async function redeem() {
      if (options.signInUrl || state.closed || state.busy || !isToken(state.token) || !personal(state.owner)) return;
      state.busy = true; view.busy(true); const generation = ++state.generation;
      try {
        const result = await api('/api/invitations/redeem', 'POST', {token:state.token, expectedAccountId:state.owner});
        if (!result || result.approved !== true || result.accountId !== state.owner || typeof result.alreadyRedeemed !== 'boolean') throw fail(409);
        if (state.closed || generation !== state.generation) return;
        state.token = null; view.accepted(); status('Invitation accepted. Welcome to the library.');
      } catch (error) { if (!state.closed && generation === state.generation) { state.token = null; failure(error, true); } }
      finally { if (!state.closed && generation === state.generation) { state.busy = false; view.busy(false, null, false); } }
    }
    return {load, create, checkout, revoke, copy, preview, enterToken, signIn, redeem, close, dismiss:clearSecret,
      selectionChanged:() => view.busy(state.busy, state.snapshot)};
  }

  function mount(win, document) {
    const mode = document.body.dataset.invitationView;
    if (!['manage','redeem'].includes(mode)) return;
    const element = id => document.getElementById(id);
    let token = mode === 'redeem' ? takeFragment(win.location, win.history) : null;
    const hasFragmentToken = !!token;
    const put = (id, text) => { element(id).textContent = text; };
    let controller;
    const signInForm = mode === 'redeem' ? element('invSignInForm') : null;
    const codeForm = mode === 'redeem' ? element('invCodeForm') : null;
    const codeInput = mode === 'redeem' ? element('invCode') : null;
    if (codeInput) codeInput.value = '';
    const view = {
      status(text, error) { put('invStatus', text); element('invStatus').dataset.error = String(!!error); },
      busy(busy, value, accept) {
        if (mode === 'redeem') { if (codeInput) codeInput.disabled = busy; if (codeForm) element('invCodeSubmit').disabled = busy; if (signInForm) element('invSignIn').disabled = busy || signInForm.hidden; element('invAccept').disabled = busy || !accept; element('invAccept').hidden = !accept; return; }
        const selected = element('invCreditSource').value;
        element('invCreate').disabled = busy || !value || !value.enabled || (selected === 'paid' ? value.paidCredits < 1 : (!value.unlimited && value.annualRemaining < 1));
        element('invCreditSource').disabled = busy;
        element('invRefresh').disabled = busy;
        for (const button of element('invList').querySelectorAll('button')) button.disabled = busy;
      },
      secret(link) {
        element('invNewLink').hidden = !link;
        element('invShareUrl').value = link ? link.url : '';
        put('invNewExpiry', link ? 'Expires ' + date(link.expiresAt) : '');
      },
      invalidate() {
        if (mode === 'manage') { element('invManagement').hidden = true; element('invList').replaceChildren(); }
        else { put('invRecipient', ''); element('invRecipient').hidden = true; put('invExpiry', ''); element('invExpiry').hidden = true; }
      },
      manage(value) {
        element('invManagement').hidden = !value.enabled;
        if (!value.enabled) return;
        put('invAllowance', value.unlimited ? 'Unlimited invitations' : value.annualRemaining + ' of ' + value.annualLimit + ' available');
        put('invReset', value.unlimited ? 'Your account has an unlimited invitation allowance.' : 'Resets January 1, ' + (value.year + 1) + ' (UTC).');
        put('invPaidBalance', value.paidCredits + ' purchased invitation' + (value.paidCredits === 1 ? '' : 's'));
        put('invPrice', new Intl.NumberFormat(undefined, {style:'currency',currency:value.price.currency}).format(value.price.amountMinor / 100) + ' per additional invitation.');
        const select = element('invCreditSource');
        select.options[0].disabled = !value.unlimited && value.annualRemaining < 1;
        select.options[1].disabled = value.paidCredits < 1;
        select.value = value.unlimited || value.annualRemaining > 0 ? 'annual' : 'paid';
        const list = element('invList'); list.replaceChildren();
        if (!value.invitations.length) { const empty = document.createElement('p'); empty.className = 'inv-muted'; empty.textContent = 'No invitations yet. Your next invitation will appear here.'; list.append(empty); }
        for (const item of value.invitations) {
          const row = document.createElement('div'); row.className = 'inv-row';
          const summary = document.createElement('div');
          const tag = document.createElement('span'); tag.className = 'inv-tag'; tag.dataset.status = item.status; tag.textContent = item.status[0].toUpperCase() + item.status.slice(1);
          const info = document.createElement('p'); info.className = 'inv-muted'; info.textContent = 'Created ' + date(item.createdAt) + ' · Expires ' + date(item.expiresAt);
          summary.append(tag, info); row.append(summary);
          if (item.status === 'pending') { const button = document.createElement('button'); button.type = 'button'; button.className = 'inv-secondary'; button.textContent = 'Revoke'; button.addEventListener('click', () => controller.revoke(item.id)); row.append(button); }
          list.append(row);
        }
      },
      recipient(display, expiresAt) { put('invRecipient', 'Accepting as ' + display); element('invRecipient').hidden = false; put('invExpiry', 'Expires ' + date(expiresAt)); element('invExpiry').hidden = false; },
      payment(value) {
        if (mode !== 'manage') return;
        put('invCheckoutStatus',value.text);
        element('invCheckout').hidden = !value.showBuy; element('invCheckout').disabled = value.disabled;
        put('invCheckout',value.retry ? 'Continue existing checkout' : 'Buy an additional invitation');
        element('invCheckoutRefresh').hidden = !value.showRefresh; element('invCheckoutRefresh').disabled = value.disabled;
      },
      signIn(ready) { if (signInForm) { element('invSignInToken').replaceChildren(); signInForm.hidden = !ready; element('invSignIn').disabled = !ready; } },
      entry(visible) { if (codeForm) codeForm.hidden = !visible; if (codeInput) codeInput.value = ''; },
      accepted() { element('invAccept').hidden = true; element('invContinue').hidden = false; },
    };
    // Landing has no secret display elements; close remains safe for either page.
    if (mode === 'redeem') view.secret = () => {};
    controller = createController({owner:document.body.dataset.accountId, token, origin:win.location.origin,
      signInUrl:signInForm ? signInForm.getAttribute('action') : '',
      submitSignIn(value, target) {
        if (!signInForm || signInForm.getAttribute('action') !== target) throw fail('shape');
        const input = document.createElement('input'); input.type = 'hidden'; input.name = 'token'; input.value = value;
        element('invSignInToken').replaceChildren(input);
        win.HTMLFormElement.prototype.submit.call(signInForm);
      },
      api:(...args) => request(win.fetch.bind(win), ...args), view,
      newRequestId:() => win.crypto && win.crypto.randomUUID ? win.crypto.randomUUID() : null,
      openCheckout:url => win.location.assign(url),
      copy:text => win.navigator.clipboard ? win.navigator.clipboard.writeText(text) : Promise.reject(fail('clipboard'))});
    token = null;
    win.addEventListener('pagehide', controller.close);
    if (mode === 'manage') {
      element('invCheckout').addEventListener('click',controller.checkout);
      element('invCheckoutRefresh').addEventListener('click',controller.load);
      element('invCreateForm').addEventListener('submit', event => { event.preventDefault(); controller.create(element('invCreditSource').value); });
      element('invCreditSource').addEventListener('change', controller.selectionChanged);
      element('invRefresh').addEventListener('click', controller.load);
      element('invDismiss').addEventListener('click', controller.dismiss);
      element('invCopy').addEventListener('click', controller.copy);
      controller.load();
    } else {
      if (codeForm) codeForm.addEventListener('submit', event => {
        event.preventDefault();
        const value = codeInput.value;
        codeInput.value = '';
        controller.enterToken(value);
      });
      element('invAccept').addEventListener('click', controller.redeem);
      if (signInForm) signInForm.addEventListener('submit', event => { event.preventDefault(); controller.signIn(); });
      if (hasFragmentToken) controller.preview();
    }
    return controller;
  }
  const api = {fragmentToken, takeFragment, signInTarget, checkoutTarget, checkoutResult, orders, snapshot, issued, request, createController, mount};
  if (typeof module !== 'undefined' && module.exports) module.exports = api;
  if (root && root.document) mount(root, root.document);
})(typeof window !== 'undefined' ? window : null);

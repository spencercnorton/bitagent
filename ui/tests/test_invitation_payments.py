"""Synthetic Stripe transport, actual ASGI gates and durable SQLite ledgers."""
import asyncio
import copy
import hashlib
import hmac
import json
import time
from urllib.parse import parse_qs
import uuid

import httpx
import pytest

import config
import database
import invitation_payments as pay
from test_invitations import invitation_config as _invitation_config, headers, mint

invitation_config = _invitation_config
REAL_CLIENT = httpx.AsyncClient
API_KEY = b"sk_test_synthetic_key_at_least_thirty_two_bytes"
WEBHOOK_KEY = b"whsec_synthetic_webhook_key_at_least_thirty_two_bytes"


class Wire(httpx.AsyncByteStream):
    def __init__(self, raw):
        self.raw = raw

    async def __aiter__(self):
        yield self.raw


class Stripe:
    def __init__(self):
        self.calls = []
        self.sessions = {}
        self.forms = {}
        self.refunded = 0
        self.disputed = False
        self.fail_post = False
        self.fail_get = False
        self.change = None

    def session(self, form):
        order = form['client_reference_id'][0]
        return {'object': 'checkout.session', 'id': 'cs_test_' + order, 'livemode': False,
                'mode': 'payment', 'client_reference_id': order,
                'metadata': {'purpose': pay.PURPOSE, 'order_id': order},
                'currency': 'usd', 'amount_total': 5000, 'amount_subtotal': 5000,
                'expires_at': int(form['expires_at'][0]), 'payment_method_types': ['card'],
                'allow_promotion_codes': False, 'automatic_tax': {'enabled': False},
                'adaptive_pricing': {'enabled': False}, 'shipping_cost': None, 'shipping_options': [],
                'total_details': {'amount_discount': 0, 'amount_shipping': 0, 'amount_tax': 0},
                'status': 'open', 'payment_status': 'unpaid', 'payment_intent': 'pi_' + order,
                'url': 'https://checkout.stripe.com/c/pay/cs_test_' + order + '#synthetic-fragment'}

    def charge(self, order):
        return {'object': 'charge', 'id': 'ch_' + order, 'payment_intent': 'pi_' + order,
                'paid': True, 'status': 'succeeded', 'currency': 'usd', 'amount': 5000,
                'livemode': False, 'amount_refunded': self.refunded, 'disputed': self.disputed}

    def intent(self, order):
        return {'object': 'payment_intent', 'id': 'pi_' + order, 'livemode': False,
                'metadata': {'purpose': pay.PURPOSE, 'order_id': order}, 'currency': 'usd',
                'amount': 5000, 'status': 'succeeded', 'amount_received': 5000,
                'latest_charge': self.charge(order)}

    def handle(self, request):
        assert request.url.host == 'api.stripe.com'
        assert request.headers['authorization'] == 'Bearer ' + API_KEY.decode()
        assert request.headers['stripe-version'] == pay.API_VERSION
        assert request.headers['accept-encoding'] == 'identity'
        self.calls.append((request.method, request.url.path, request.headers.get('idempotency-key')))
        path = request.url.path
        if self.fail_get and request.method == 'GET':
            return httpx.Response(500, content=b'synthetic failure')
        if path == '/v1/account':
            body = {'id': 'acct_synthetic', 'object': 'account'}
        elif path == '/v1/prices/price_synthetic':
            body = {'id': 'price_synthetic', 'object': 'price', 'active': True, 'type': 'one_time',
                    'recurring': None, 'currency': 'usd', 'unit_amount': 5000, 'livemode': False}
        elif path == '/v1/checkout/sessions':
            form = parse_qs(request.content.decode())
            key = request.headers['idempotency-key']
            if key in self.forms:
                assert self.forms[key] == form
            self.forms[key] = form
            order = form['client_reference_id'][0]
            self.sessions.setdefault(order, self.session(form))
            body = copy.deepcopy(self.sessions[order])
            if self.fail_post:
                raise httpx.ReadError('invented lost provider response', request=request)
        elif path.startswith('/v1/checkout/sessions/'):
            order = path.split('/')[4].removeprefix('cs_test_')
            if path.endswith('/line_items'):
                body = {'object': 'list', 'has_more': False, 'data': [{'price': {'id': 'price_synthetic'},
                        'quantity': 1, 'amount_total': 5000, 'amount_subtotal': 5000,
                        'amount_discount': 0, 'amount_tax': 0, 'currency': 'usd'}]}
            else:
                body = copy.deepcopy(self.sessions[order])
        elif path.startswith('/v1/payment_intents/pi_'):
            body = self.intent(path.split('pi_')[1])
        elif path.startswith('/v1/charges/ch_'):
            body = self.charge(path.split('ch_')[1])
        elif path.startswith('/v1/disputes/dp_'):
            order = path.split('dp_')[1]
            body = {'object': 'dispute', 'id': 'dp_' + order, 'livemode': False, 'charge': 'ch_' + order}
        else:
            raise AssertionError('unexpected synthetic Stripe route')
        if self.change is not None:
            body = self.change(path, body)
        return httpx.Response(200, headers={'Content-Type': 'application/json'},
                              stream=Wire(json.dumps(body).encode()))


@pytest.fixture(autouse=True)
def payment_config(invitation_config, monkeypatch, tmp_path):
    api, webhook = tmp_path / 'api-key', tmp_path / 'webhook-key'
    for path, value in ((api, API_KEY), (webhook, WEBHOOK_KEY)):
        path.write_bytes(value)
        path.chmod(0o600)
    for field, value in {'private_invitation_checkout_enabled': True,
                         'invitation_stripe_api_key_file': str(api),
                         'invitation_stripe_webhook_secret_file': str(webhook),
                         'invitation_stripe_account_id': 'acct_synthetic',
                         'invitation_stripe_price_id': 'price_synthetic',
                         'invitation_stripe_live_mode': False}.items():
        monkeypatch.setattr(config.settings, field, value)
    pay.validate_settings()
    monkeypatch.setattr(pay, 'start_worker', lambda: None)
    monkeypatch.setattr(pay, '_SLOTS', asyncio.Semaphore(4))
    stripe = Stripe()
    def fake_client(**kwargs):
        assert kwargs['verify'] is True and kwargs['trust_env'] is False and kwargs['follow_redirects'] is False
        return REAL_CLIENT(transport=httpx.MockTransport(stripe.handle), **kwargs)
    monkeypatch.setattr(pay.httpx, 'AsyncClient', fake_client)
    async def clear():
        db = await database.get_db()
        for table in ('invitation_checkout_orders', 'invitation_stripe_events'):
            await db.execute('DELETE FROM ' + table)
        await db.commit()
    asyncio.run(clear())
    yield stripe
    pay._CONFIG = pay._API_KEY = pay._WEBHOOK_KEY = None
    pay._PROOF_UNTIL = 0


def checkout(client, user='alice', request_id=None):
    return client.post('/api/account/invitations/checkout', headers=headers(user),
                       json={'expectedAccountId': user, 'requestId': request_id or str(uuid.uuid4())})


def event(client, order, kind='checkout.session.completed', event_id=None, **changes):
    object_id = ('cs_test_' if kind.startswith('checkout.') else 'ch_' if kind == 'charge.refunded' else 'dp_') + order
    value = {'id': event_id or 'evt_' + uuid.uuid4().hex, 'object': 'event', 'api_version': pay.API_VERSION,
             'livemode': False, 'type': kind, 'data': {'object': {'id': object_id}}, **changes}
    raw = json.dumps(value, separators=(',', ':')).encode()
    stamp = str(int(time.time()))
    signature = hmac.new(WEBHOOK_KEY, stamp.encode() + b'.' + raw, hashlib.sha256).hexdigest()
    response = client.post(pay.WEBHOOK, headers={'Host': 'library.example.org', 'Content-Type': 'application/json',
                                                'Stripe-Signature': 't=' + stamp + ',v1=' + signature}, content=raw)
    return response, value['id']


def paid(stripe, order):
    stripe.sessions[order]['status'] = 'complete'
    stripe.sessions[order]['payment_status'] = 'paid'


def balance(client):
    return client.get('/api/account/invitations', headers=headers()).json()['paidCredits']


def test_checkout_fixed_price_idempotency_and_return_no_credit(client, payment_config):
    stripe = payment_config
    request_id = str(uuid.uuid4())
    result = checkout(client, request_id=request_id)
    assert result.status_code == 200 and result.json()['status'] == 'open'
    order = result.json()['orderId']
    assert result.json()['checkoutUrl'] == stripe.sessions[order]['url']
    assert checkout(client, request_id=request_id).json() == result.json()
    assert len(stripe.forms) == 1 and balance(client) == 0
    form = next(iter(stripe.forms.values()))
    assert form['line_items[0][quantity]'] == ['1'] and form['line_items[0][price]'] == ['price_synthetic']
    assert form['success_url'] == ['https://library.example.org/invitations']
    assert form['payment_method_types[0]'] == ['card']
    assert checkout(client).status_code == 409
    assert client.get('/invitations?session_id=cs_forged', headers=headers()).status_code in {200, 404}
    assert balance(client) == 0


def test_owner_and_machine_cannot_purchase_before_provider_io(client, payment_config):
    assert checkout(client, 'owner').status_code == 409
    assert not payment_config.calls
    r = client.post('/api/account/invitations/checkout', headers=headers(Authorization='Bearer fake'),
                    json={'expectedAccountId': 'alice', 'requestId': str(uuid.uuid4())})
    assert r.status_code in {401, 403} and not payment_config.calls


def test_lost_provider_response_held_same_key_and_old_key_never_reused(client, payment_config):
    stripe = payment_config
    stripe.fail_post = True
    request_id = str(uuid.uuid4())
    pending = checkout(client, request_id=request_id).json()
    assert pending['status'] == 'pending' and pending['checkoutUrl'] is None and len(stripe.forms) == 1
    assert checkout(client).status_code == 409
    stripe.fail_post = False
    assert checkout(client, request_id=request_id).json()['orderId'] == pending['orderId']
    async def age():
        db = await database.get_db()
        await db.execute('UPDATE invitation_checkout_orders SET created_at=created_at-?', (24 * 3600,))
        await db.commit()
    asyncio.run(age())
    before = len(stripe.calls)
    assert checkout(client, request_id=request_id).status_code == 409
    assert len(stripe.calls) == before and balance(client) == 0


def test_signed_receipt_requires_object_reconciliation_and_credits_once(client, payment_config):
    stripe = payment_config
    order = checkout(client).json()['orderId']
    paid(stripe, order)
    response, eid = event(client, order)
    assert response.status_code == 200 and balance(client) == 0
    asyncio.run(pay.process_pending())
    assert balance(client) == 1
    response, _ = event(client, order, event_id=eid)
    assert response.status_code == 200
    event(client, order)
    asyncio.run(pay.process_pending())
    assert balance(client) == 1
    invite = mint(client, creditSource='paid').json()
    assert mint(client, creditSource='paid').status_code == 409
    async def check():
        db = await database.get_db()
        assert len(await db.execute_fetchall('SELECT * FROM invitation_payments')) == 1
        use = (await db.execute_fetchall('SELECT * FROM invitation_credit_uses'))[0]
        assert use['invitation_id'] == invite['id'] and use['payment_id'] == 'pi_' + order
    asyncio.run(check())


@pytest.mark.parametrize('stage', ['unused', 'pending', 'redeemed', 'before_credit'])
@pytest.mark.parametrize('kind', ['charge.refunded', 'charge.dispute.created'])
def test_refund_dispute_terminal_no_membership_revocation(client, payment_config, stage, kind):
    stripe = payment_config
    order = checkout(client).json()['orderId']
    paid(stripe, order)
    invitation = None
    if stage != 'before_credit':
        event(client, order)
        asyncio.run(pay.process_pending())
        assert balance(client) == 1
        if stage in {'pending', 'redeemed'}:
            invitation = mint(client, creditSource='paid').json()
            if stage == 'redeemed':
                response = client.post('/api/invitations/redeem', headers=headers('new-subject'),
                    json={'expectedAccountId': 'new-subject', 'token': invitation['token']})
                assert response.status_code == 200
    stripe.refunded = 100 if kind == 'charge.refunded' else 0
    stripe.disputed = kind != 'charge.refunded'
    assert event(client, order, kind)[0].status_code == 200
    asyncio.run(pay.process_pending())
    assert balance(client) == 0 and mint(client, creditSource='paid').status_code == 409
    # Even a late paid event with a now-clean provider object cannot restore it.
    stripe.refunded, stripe.disputed = 0, False
    event(client, order)
    asyncio.run(pay.process_pending())
    assert balance(client) == 0
    async def check():
        db = await database.get_db()
        assert len(await db.execute_fetchall('SELECT * FROM invitation_payment_tombstones')) == 1
        if invitation:
            row = (await db.execute_fetchall('SELECT * FROM membership_invitations WHERE id=?', (invitation['id'],)))[0]
            assert (row['revoked_at'] is not None) == (stage == 'pending')
        if stage == 'redeemed':
            assert (await db.execute_fetchall('SELECT active FROM private_members WHERE user_id=?', ('new-subject',)))[0]['active'] == 1
    asyncio.run(check())


@pytest.mark.parametrize('mutation', ['account', 'price', 'session_amount', 'metadata', 'quantity', 'more', 'intent', 'charge'])
def test_wrong_provider_proof_never_credits(client, payment_config, mutation):
    stripe = payment_config
    order = checkout(client).json()['orderId']
    paid(stripe, order)
    def change(path, body):
        if mutation == 'account' and path == '/v1/account':
            body['id'] = 'acct_other'
        elif mutation == 'price' and path.endswith('/line_items'):
            body['data'][0]['price']['id'] = 'price_other'
        elif mutation == 'session_amount' and path.startswith('/v1/checkout/sessions/'):
            body['amount_total'] = 4999
        elif mutation == 'metadata' and path.startswith('/v1/checkout/sessions/'):
            body['metadata'] = {'purpose': 'other', 'order_id': order}
        elif mutation == 'quantity' and path.endswith('/line_items'):
            body['data'][0]['quantity'] = 2
        elif mutation == 'more' and path.endswith('/line_items'):
            body['has_more'] = True
        elif mutation == 'intent' and path.startswith('/v1/payment_intents/'):
            body['amount_received'] = 4999
        elif mutation == 'charge' and path.startswith('/v1/payment_intents/'):
            body['latest_charge']['payment_intent'] = 'pi_other'
        return body
    stripe.change = change
    event(client, order)
    asyncio.run(pay.process_pending())
    assert balance(client) == 0


@pytest.mark.parametrize('changes', [{'livemode': True}, {'account': 'acct_connect'}, {'api_version': 'other'}])
def test_webhook_wrong_mode_account_version(client, changes):
    assert event(client, 'a' * 32, **changes)[0].status_code == 400


def test_webhook_requires_raw_signature_uniform_ingress_and_body_bound(client):
    headers_in = {'Host': 'library.example.org', 'Content-Type': 'application/json', 'Stripe-Signature': 't=0,v1=' + '0' * 64}
    assert client.post(pay.WEBHOOK, headers=headers_in, content=b'{}').status_code == 400
    for path in (pay.WEBHOOK + '?event=forged', pay.WEBHOOK + '/', pay.WEBHOOK + '/extra'):
        assert client.post(path, headers=headers_in, content=b'{}').status_code == 404
    assert client.post(pay.WEBHOOK, headers={**headers_in, 'Host': 'console.example.org'}, content=b'{}').status_code == 404
    assert client.post(pay.WEBHOOK, headers={**headers_in, 'Origin': 'https://library.example.org'}, content=b'{}').status_code == 400
    assert client.post(pay.WEBHOOK, headers=headers_in, content=b'x' * (pay.BODY_LIMIT + 1)).status_code == 400


def test_owner_config_change_preserves_existing_paid_fulfillment(client, payment_config, monkeypatch):
    order = checkout(client).json()['orderId']
    paid(payment_config, order)
    monkeypatch.setattr(config.settings, 'invitation_owner_ids', 'alice,owner')
    assert checkout(client).status_code == 409
    event(client, order)
    asyncio.run(pay.process_pending())
    assert balance(client) == 1


def test_feature_off_no_provider_or_ledger(client, payment_config, monkeypatch):
    monkeypatch.setattr(config.settings, 'private_invitation_checkout_enabled', False)
    assert checkout(client).status_code == 404
    assert event(client, 'a' * 32)[0].status_code == 404
    assert not payment_config.calls


def test_webhook_restart_inbox_and_duplicate_conflict(client, payment_config):
    order = checkout(client).json()['orderId']
    paid(payment_config, order)
    _, eid = event(client, order)
    asyncio.run(database.close_all())
    assert event(client, order, event_id=eid, type='charge.refunded')[0].status_code == 400
    asyncio.run(pay.process_pending())
    assert balance(client) == 1


@pytest.mark.parametrize('key_prefix', [b'sk_test_', b'rk_test_'])
def test_restricted_or_standard_key_mode_exact(payment_config, key_prefix):
    path = config.settings.invitation_stripe_api_key_file
    with open(path, 'wb') as file:
        file.write(key_prefix + b'synthetic_secret_long_enough_for_fixture')
    pay.validate_settings()
    assert pay._API_KEY.startswith(key_prefix)


@pytest.mark.parametrize('change', ['livekey', 'mode', 'perms', 'symlink', 'price', 'amount'])
def test_startup_rejects_mismatched_key_scope_config(payment_config, change, monkeypatch, tmp_path):
    path = config.settings.invitation_stripe_api_key_file
    if change == 'livekey':
        with open(path, 'wb') as file:
            file.write(b'rk_live_synthetic_secret_long_enough_for_fixture')
    elif change == 'mode':
        monkeypatch.setattr(config.settings, 'invitation_stripe_live_mode', True)
    elif change == 'perms':
        import os
        os.chmod(path, 0o644)
    elif change == 'symlink':
        link = tmp_path / 'link'
        link.symlink_to(path)
        monkeypatch.setattr(config.settings, 'invitation_stripe_api_key_file', str(link))
    elif change == 'price':
        monkeypatch.setattr(config.settings, 'invitation_stripe_price_id', 'price_bad/path')
    else:
        monkeypatch.setattr(config.settings, 'invitation_price_usd_cents', 4999)
    with pytest.raises(RuntimeError):
        pay.validate_settings()
    assert pay._CONFIG is None and pay._API_KEY is None


def test_configuration_failure_removes_checkout_availability(client, payment_config):
    checkout(client)
    assert pay.checkout_available('alice') and not pay.checkout_available('owner')
    payment_config.fail_get = True
    # An explicit same-request retry still verifies account+price before POST.
    listed = client.get('/api/account/invitations/orders', headers=headers()).json()
    assert checkout(client, request_id=listed['orders'][0]['requestId']).json()['status'] == 'pending'
    assert not pay.checkout_available('alice')


def test_price_proof_failure_before_session_post(client, payment_config):
    payment_config.change = lambda path, body: {**body, 'unit_amount': 4999} if path.startswith('/v1/prices/') else body
    assert checkout(client).json()['status'] == 'pending'
    assert not payment_config.forms and not pay.checkout_available('alice')


def test_provider_total_deadline_after_accepted_post_preserves_unknown(client, payment_config, monkeypatch):
    stripe = payment_config
    monkeypatch.setattr(pay, 'PROVIDER_TIMEOUT', .03)
    async def slow(request):
        response = stripe.handle(request)
        if request.method == 'POST':
            await asyncio.sleep(1)
        return response
    def fake_client(**kwargs):
        return REAL_CLIENT(transport=httpx.MockTransport(slow), **kwargs)
    monkeypatch.setattr(pay.httpx, 'AsyncClient', fake_client)
    result = checkout(client).json()
    assert result['status'] == 'pending' and len(stripe.forms) == 1
    assert not pay.checkout_available('alice')
    async def check():
        db = await database.get_db()
        row = (await db.execute_fetchall('SELECT * FROM invitation_checkout_orders'))[0]
        assert row['state'] == 'unknown' and row['session_id'] is None
        assert not await db.execute_fetchall('SELECT * FROM invitation_payments')
    asyncio.run(check())


def test_signed_webhook_body_changed_expired_duplicate_and_deep_json(client):
    raw = b'{"a":1}'
    stamp = str(int(time.time()))
    sign = hmac.new(WEBHOOK_KEY, stamp.encode() + b'.' + raw, hashlib.sha256).hexdigest()
    h = {'Host': 'library.example.org', 'Content-Type': 'application/json', 'Stripe-Signature': f't={stamp},v1={sign}'}
    assert client.post(pay.WEBHOOK, headers=h, content=raw + b' ').status_code == 400
    assert client.post(pay.WEBHOOK, headers={**h, 'Stripe-Signature': 't=0,v1=' + sign}, content=raw).status_code == 400
    duplicate = list(h.items()) + [('Stripe-Signature', h['Stripe-Signature'])]
    assert client.post(pay.WEBHOOK, headers=duplicate, content=raw).status_code == 400
    for invalid in (b'{"id":"evt_x","id":"evt_y"}', b'{' + b'"x":[' + b'[' * 2000 + b']' * 2000 + b']}'):
        sig = hmac.new(WEBHOOK_KEY, stamp.encode() + b'.' + invalid, hashlib.sha256).hexdigest()
        assert client.post(pay.WEBHOOK, headers={**h, 'Stripe-Signature': f't={stamp},v1={sig}'}, content=invalid).status_code == 400


def test_redirect_or_compressed_provider_cannot_create_checkout(client, payment_config, monkeypatch):
    for response in (httpx.Response(302, headers={'Location': 'https://foreign.example.org'}),
                     httpx.Response(200, headers={'Content-Encoding': 'gzip'}, stream=Wire(b'compressed'))):
        def fake_client(**kwargs):
            return REAL_CLIENT(transport=httpx.MockTransport(lambda _: response), **kwargs)
        monkeypatch.setattr(pay.httpx, 'AsyncClient', fake_client)
        request_id = str(uuid.uuid4())
        assert checkout(client, request_id=request_id).json()['status'] == 'pending'
        # Reuse the same durable prepared row for the next attempted observation.
        async def clear():
            db = await database.get_db()
            await db.execute('DELETE FROM invitation_checkout_orders')
            await db.commit()
        asyncio.run(clear())
    assert not payment_config.forms and balance(client) == 0


def test_old_mapped_and_unmapped_paid_credit_accounting(client):
    async def seed():
        db = await database.get_db()
        for index in range(3):
            await db.execute('INSERT INTO invitation_payments VALUES (?,?,?,?,?,?)',
                             (f'pi_legacy{index}', f'evt_legacy{index}', 'alice', 5000, 'USD', index))
        await db.execute('INSERT INTO membership_invitations VALUES (?,?,?,?,?,?,?,NULL,NULL,NULL)',
                         ('a' * 32, 'b' * 64, 'alice', 2026, 'paid', 0, 1))
        await db.commit()
    asyncio.run(seed())
    assert balance(client) == 2
    assert mint(client, creditSource='paid').status_code == 201
    assert balance(client) == 1


def test_two_distinct_checkout_requests_only_one_order_under_concurrency(client, payment_config):
    import app as app_module
    from conftest import _with_transport_peer
    async def run():
        async with REAL_CLIENT(transport=httpx.ASGITransport(app=_with_transport_peer(app_module.app, '127.0.0.1')),
                               base_url='https://library.example.org') as browser:
            async def make():
                return await browser.post('/api/account/invitations/checkout', headers=headers(),
                                          json={'expectedAccountId': 'alice', 'requestId': str(uuid.uuid4())})
            results = await asyncio.gather(make(), make())
            assert sorted(r.status_code for r in results) == [200, 409]
        assert len(payment_config.forms) == 1
    asyncio.run(run())


def test_signed_credit_and_refund_race_tombstone_wins(client, payment_config):
    order = checkout(client).json()['orderId']
    paid(payment_config, order)
    _, credit_id = event(client, order)
    payment_config.refunded = 100
    _, refund_id = event(client, order, 'charge.refunded')
    async def run():
        await asyncio.gather(pay.reconcile_event(credit_id), pay.reconcile_event(refund_id))
    asyncio.run(run())
    assert balance(client) == 0


def test_over25_verified_foreign_events_do_not_starve_our_payment(client, payment_config):
    stripe = payment_config
    order = checkout(client).json()['orderId']
    paid(stripe, order)
    for index in range(36):
        foreign = f'{index:032x}'
        form = {'client_reference_id': [foreign], 'expires_at': [str(int(time.time()) + 3600)]}
        stripe.sessions[foreign] = stripe.session(form)
        stripe.sessions[foreign]['metadata'] = {} if index % 2 else {'purpose': 'other'}
        assert event(client, foreign)[0].status_code == 200
    assert event(client, order)[0].status_code == 200
    asyncio.run(pay.process_pending())
    assert balance(client) == 1
    assert pay.checkout_available('alice')
    async def check():
        db = await database.get_db()
        states = await db.execute_fetchall('SELECT state,COUNT(*) AS n FROM invitation_stripe_events GROUP BY state')
        assert {r['state']: r['n'] for r in states} == {'ignored': 36, 'done': 1}
    asyncio.run(check())


@pytest.mark.parametrize('metadata', [
    {'purpose': pay.PURPOSE}, {'purpose': pay.PURPOSE, 'order_id': []},
    {'purpose': pay.PURPOSE, 'order_id': 'f' * 32}, ['malformed'],
])
def test_our_unknown_or_malformed_order_is_durable_manual_hold(client, payment_config, metadata):
    order = checkout(client).json()['orderId']
    paid(payment_config, order)
    payment_config.sessions[order]['metadata'] = metadata
    _, eid = event(client, order)
    asyncio.run(pay.process_pending())
    async def check():
        row = (await (await database.get_db()).execute_fetchall('SELECT state FROM invitation_stripe_events WHERE id=?', (eid,)))[0]
        assert row['state'] == 'manual_hold'
    asyncio.run(check())
    calls = len(payment_config.calls)
    asyncio.run(pay.process_pending())
    assert len(payment_config.calls) == calls and balance(client) == 0


def test_transient_provider_retry_moves_after_new_events(client, payment_config):
    order = checkout(client).json()['orderId']
    paid(payment_config, order)
    _, old = event(client, order)
    payment_config.fail_get = True
    asyncio.run(pay.process_pending())
    async def prepare():
        db = await database.get_db()
        await db.execute('UPDATE invitation_stripe_events SET next_attempt=1 WHERE id=?', (old,))
        await db.commit()
    asyncio.run(prepare())
    _, new = event(client, order)
    seen = []
    original = pay.reconcile_event
    async def recording(eid):
        seen.append(eid)
        await original(eid)
    pay.reconcile_event = recording
    try:
        payment_config.fail_get = False
        asyncio.run(pay.process_pending())
    finally:
        pay.reconcile_event = original
    assert seen == [new, old] and balance(client) == 1


@pytest.mark.parametrize('field', ['subtotal', 'session_discount', 'line_total', 'line_subtotal', 'line_discount', 'line_tax', 'received'])
def test_float_money_cannot_authorize_credit_or_repeat_charge(client, payment_config, field):
    order = checkout(client).json()['orderId']
    paid(payment_config, order)
    def floating(path, body):
        if field == 'subtotal' and path.startswith('/v1/checkout/sessions/') and not path.endswith('/line_items'):
            body['amount_subtotal'] = 5000.0
        elif field == 'session_discount' and path.startswith('/v1/checkout/sessions/') and not path.endswith('/line_items'):
            body['total_details']['amount_discount'] = 0.0
        elif field.startswith('line_') and path.endswith('/line_items'):
            key = {'line_total': 'amount_total', 'line_subtotal': 'amount_subtotal', 'line_discount': 'amount_discount', 'line_tax': 'amount_tax'}[field]
            body['data'][0][key] = float(body['data'][0][key])
        elif field == 'received' and path.startswith('/v1/payment_intents/'):
            body['amount_received'] = 5000.0
        return body
    payment_config.change = floating
    _, eid = event(client, order)
    asyncio.run(pay.process_pending())
    assert balance(client) == 0 and checkout(client).status_code == 409
    rows = client.get('/api/account/invitations/orders', headers=headers()).json()['orders']
    assert rows[0]['status'] == 'manual_hold' and rows[0]['retryAllowed'] is False
    async def check():
        assert (await (await database.get_db()).execute_fetchall('SELECT state FROM invitation_stripe_events WHERE id=?', (eid,)))[0]['state'] == 'manual_hold'
    asyncio.run(check())


@pytest.mark.parametrize('delay_first', [True, False])
def test_absolute_middleware_body_deadline_precedes_order_or_provider(client, payment_config, monkeypatch, delay_first):
    import app as app_module
    import invitations
    from conftest import _with_transport_peer
    monkeypatch.setattr(invitations, 'BODY_TIMEOUT', .01)
    class SlowBody(httpx.AsyncByteStream):
        async def __aiter__(self):
            if not delay_first:
                yield b'{'
            await asyncio.sleep(1)
            yield b'}'
    async def run():
        async with REAL_CLIENT(transport=httpx.ASGITransport(app=_with_transport_peer(app_module.app, '127.0.0.1')),
                               base_url='https://library.example.org') as browser:
            result = await browser.post('/api/account/invitations/checkout', headers={**headers(), 'Content-Type': 'application/json'}, content=SlowBody())
        assert result.status_code == 408
        db = await database.get_db()
        assert not await db.execute_fetchall('SELECT * FROM invitation_checkout_orders')
        assert not payment_config.calls
    asyncio.run(run())

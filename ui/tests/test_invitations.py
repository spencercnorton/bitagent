"""Invitation allowances and redemption use actual SQLite and ASGI gates."""
import asyncio
from datetime import datetime, timezone
import hashlib
import json

import httpx
import pytest
from fastapi import HTTPException

import app as app_module
import config
import database
import invitations as invites
import private_indexer as private
from conftest import _with_transport_peer

PROOF = "synthetic-invitation-proxy-proof-at-least32bytes"


def headers(user="alice", role="VIEWER", **extra):
    return {"Host": "library.example.org", "X-Auth-User-Id": user,
            "X-Auth-Priv": role, "X-BitAgent-Proxy-Proof": PROOF,
            "Sec-Fetch-Site": "same-origin", **extra}


async def member(uid, active=True):
    db = await database.get_db()
    await db.execute("INSERT OR REPLACE INTO private_members VALUES (?,?,?,?)", (uid, int(active), "fixture", invites._now()))
    await db.commit()


@pytest.fixture(autouse=True)
def invitation_config(monkeypatch):
    for key, value in {"require_auth": True, "trust_npm_headers": True,
                       "private_indexer_enabled": True, "private_invitations_enabled": True,
                       "private_indexer_secret": "s" * 40, "private_indexer_url": "https://library.example.org",
                       "private_seeder_url": "https://seeder.example.org", "dashboard_api_key": "",
                       "trusted_proxy_cidrs": "127.0.0.0/8", "proxy_auth_secret": PROOF,
                       "invitation_owner_ids": "owner", "invitation_annual_allowance": 3,
                       "invitation_price_usd_cents": 5000}.items():
        monkeypatch.setattr(config.settings, key, value)
    monkeypatch.setattr(private, "_WRITE_LOCK", asyncio.Lock())
    monkeypatch.setattr(private, "start_readiness_worker", lambda: None)
    async def no_stop():
        pass
    monkeypatch.setattr(private, "stop_readiness_worker", no_stop)
    invites._RATE_BUCKETS.clear()
    async def clear():
        db = await database.get_db()
        for table in ("membership_invitations", "invitation_payments", "invitation_payment_events", "invitation_credit_uses", "invitation_payment_tombstones", "private_members"):
            await db.execute("DELETE FROM " + table)
        await db.commit()
        await member("alice")
        await member("owner")
    asyncio.run(clear())
    yield
    invites._RATE_BUCKETS.clear()
    asyncio.run(database.close_all())


def mint(client, user="alice", **extra):
    return client.post("/api/account/invitations", headers=headers(user), json={"expectedAccountId": user, **extra})


def preview(client, token):
    return client.post("/api/invitations/preview", headers={"Host": "library.example.org"}, json={"token": token})


def redeem(client, token, user="new-user"):
    return client.post("/api/invitations/redeem", headers=headers(user), json={"token": token, "expectedAccountId": user})


def test_mint_is_one_time_secret_and_default_allowance(client):
    minted = [mint(client) for _ in range(3)]
    assert all(r.status_code == 201 for r in minted)
    assert mint(client).status_code == 409
    value = minted[0].json()
    assert invites._TOKEN.fullmatch(value["token"])
    assert value["shareUrl"] == "https://library.example.org/invite#" + value["token"]
    assert (datetime.fromisoformat(value["expiresAt"].replace("Z", "+00:00")) - datetime.fromisoformat(value["createdAt"].replace("Z", "+00:00"))).total_seconds() == 7 * 86400
    listed = client.get("/api/account/invitations", headers=headers()).json()
    assert listed["annualUsed"] == 3 and listed["annualRemaining"] == 0
    assert listed["checkoutAvailable"] is False and listed["price"] == {"amountMinor": 5000, "currency": "USD"}
    assert value["token"] not in json.dumps(listed) and "token_hash" not in json.dumps(listed)
    async def check():
        row = (await (await database.get_db()).execute_fetchall("SELECT * FROM membership_invitations WHERE id=?", (value["id"],)))[0]
        assert row["token_hash"] == hashlib.sha256(value["token"].encode()).hexdigest()
        assert value["token"] not in str(dict(row))
    asyncio.run(check())


def test_owner_is_exact_subject_not_role_and_history_still_counts(client, monkeypatch):
    for _ in range(5):
        assert mint(client, "owner").status_code == 201
    owner = client.get("/api/account/invitations", headers=headers("owner")).json()
    assert owner["unlimited"] is True and owner["annualUsed"] == 5 and owner["annualRemaining"] is None
    monkeypatch.setattr(config.settings, "invitation_owner_ids", "other-owner")
    assert mint(client, "owner").status_code == 409
    for _ in range(3):
        assert client.post("/api/account/invitations", headers=headers("alice", "OWNER"), json={"expectedAccountId": "alice"}).status_code == 201
    assert mint(client).status_code == 409


def test_utc_calendar_year_and_no_expiry_or_revoke_refund(client, monkeypatch):
    now = datetime(2026, 12, 31, 23, 59, 59, tzinfo=timezone.utc).timestamp()
    monkeypatch.setattr(invites, "_now", lambda: now)
    values = [mint(client).json() for _ in range(3)]
    assert client.request("DELETE", "/api/account/invitations/" + values[0]["id"], headers=headers(), json={"expectedAccountId": "alice"}).status_code == 200
    assert mint(client).status_code == 409
    now += 1
    status = client.get("/api/account/invitations", headers=headers()).json()
    assert status["year"] == 2027 and status["annualRemaining"] == 3
    assert mint(client).status_code == 201
    now += 8 * 86400
    assert client.get("/api/account/invitations", headers=headers()).json()["annualUsed"] == 1


def test_concurrent_allowance_and_redemption_single_use():
    async def run():
        transport = httpx.ASGITransport(app=_with_transport_peer(app_module.app, "127.0.0.1"))
        async with httpx.AsyncClient(transport=transport, base_url="https://library.example.org") as c:
            responses = await asyncio.gather(*(c.post("/api/account/invitations", headers=headers(), json={"expectedAccountId": "alice"}) for _ in range(8)))
            assert sorted(r.status_code for r in responses) == [201] * 3 + [409] * 5
            token = next(r.json()["token"] for r in responses if r.status_code == 201)
            redeemed = await asyncio.gather(*(c.post("/api/invitations/redeem", headers=headers(user), json={"token": token, "expectedAccountId": user}) for user in ("bob", "carol")))
            assert sorted(r.status_code for r in redeemed) == [200, 409]
            winner = next(r.json()["accountId"] for r in redeemed if r.status_code == 200)
            assert (await c.post("/api/invitations/redeem", headers=headers(winner), json={"token": token, "expectedAccountId": winner})).json()["alreadyRedeemed"] is True
            rows = await (await database.get_db()).execute_fetchall("SELECT user_id FROM private_members WHERE user_id IN ('bob','carol')")
            assert [r["user_id"] for r in rows] == [winner]
    asyncio.run(run())


def test_redemption_preserves_subject_and_does_not_enroll_peers_or_keys(client):
    token = mint(client).json()["token"]
    response = redeem(client, token, "-12345")
    assert response.status_code == 200 and response.json()["accountId"] == "-12345"
    assert redeem(client, token, "other").status_code == 409
    async def check():
        db = await database.get_db()
        row = (await db.execute_fetchall("SELECT * FROM private_members WHERE user_id='-12345'"))[0]
        assert row["active"] == 1 and row["actor"].startswith("invitation:")
        for table in ("private_peers", "user_api_keys"):
            assert not await db.execute_fetchall(f"SELECT * FROM {table} WHERE user_id='-12345'")
    asyncio.run(check())


def test_suspended_existing_member_cannot_reactivate_or_ack(client):
    token = mint(client).json()["token"]
    asyncio.run(member("suspended", False))
    assert redeem(client, token, "suspended").status_code == 403
    assert preview(client, token).json()["available"] is True
    assert redeem(client, token, "alice").status_code == 409
    assert redeem(client, token, "new-user").status_code == 200
    asyncio.run(member("new-user", False))
    assert redeem(client, token, "new-user").status_code == 409


def test_member_withdrawal_revokes_pending_and_later_regrant_does_not_revive(client):
    value = mint(client).json()
    response = client.put("/api/private/members/alice", headers={"X-Auth-User-Id": "owner", "X-Auth-Priv": "OWNER", "X-BitAgent-Proxy-Proof": PROOF}, json={"active": False})
    assert response.status_code == 200
    assert preview(client, value["token"]).json() == {"available": False}
    asyncio.run(member("alice"))
    assert preview(client, value["token"]).json() == {"available": False}
    assert redeem(client, value["token"]).status_code == 409


@pytest.mark.parametrize("token", [None, True, [], {}, "", "unknown", "bi_" + "a" * 42, "bi_" + "a" * 44, "bi_" + "a" * 43])
def test_preview_is_opaque_for_invalid_unknown_values(client, token):
    response = preview(client, token)
    assert response.status_code == 200 and response.json() == {"available": False}
    assert response.headers["referrer-policy"] == "no-referrer"


def test_revocation_and_expiry_have_same_opaque_preview(client, monkeypatch):
    first, second = mint(client).json(), mint(client).json()
    assert set(preview(client, first["token"]).json()) == {"available", "expiresAt"}
    assert client.request("DELETE", "/api/account/invitations/" + first["id"], headers=headers(), json={"expectedAccountId": "alice"}).status_code == 200
    now = invites._now() + 7 * 86400
    monkeypatch.setattr(invites, "_now", lambda: now)
    assert preview(client, first["token"]).json() == preview(client, second["token"]).json() == {"available": False}


def test_auth_expected_owner_csrf_json_and_body_limits(client, monkeypatch):
    body = {"expectedAccountId": "alice"}
    assert client.post("/api/account/invitations", headers={"Host": "library.example.org"}, json=body).status_code == 401
    assert client.post("/api/account/invitations", headers=headers("other"), json=body).status_code == 403  # membership gate first
    assert client.post("/api/account/invitations", headers=headers(), json={"expectedAccountId": "other"}).status_code == 409
    for extra in ({"Origin": "https://elsewhere.example.org"}, {"Sec-Fetch-Site": "same-site"}, {"Sec-Fetch-Site": "cross-site"}):
        assert client.post("/api/account/invitations", headers=headers(**extra), json=body).status_code == 403
    assert client.post("/api/account/invitations", headers=headers(), data=body).status_code == 415
    assert client.post("/api/invitations/preview", headers={"Host": "library.example.org", "Content-Type": "application/json"}, content=b"x" * 8193).status_code == 413
    assert mint(client, unexpected=True).status_code == 422
    assert mint(client, creditSource=[]).status_code == 422
    for extra in ({"X-Api-Key": "synthetic-unused-key"}, {"Authorization": "Bearer synthetic-unused-key"}):
        assert client.post("/api/account/invitations", headers=headers(**extra), json=body).status_code == 403
    assert client.post("/api/account/invitations?apikey=synthetic-unused-key", headers=headers(), json=body).status_code == 403
    monkeypatch.setattr(config.settings, "dashboard_api_key", "synthetic-machine-key")
    assert client.post("/api/account/invitations", headers={**headers("owner"), "X-Api-Key": "synthetic-machine-key"}, json={"expectedAccountId": "api-client"}).status_code == 403
    assert redeem(client, "bi_" + "a" * 43).status_code == 409
    monkeypatch.setattr(config.settings, "require_auth", False)
    assert client.post("/api/account/invitations", headers=headers("owner"), json={"expectedAccountId": "anonymous"}).status_code == 403


def test_disabled_default_does_not_create_invites(client, monkeypatch):
    monkeypatch.setattr(config.settings, "private_invitations_enabled", False)
    assert client.get("/api/account/invitations", headers=headers()).json() == {"enabled": False, "accountId": "alice", "checkoutAvailable": False}
    assert mint(client).status_code == 404
    assert preview(client, "unknown").status_code == 404


def test_paid_credit_is_separate_and_idempotent_without_http_fulfillment(client):
    async def run():
        args = {"user_id": "alice", "payment_id": "cs_synthetic1", "event_id": "evt_synthetic1", "amount": 5000, "currency": "USD"}
        rows = await asyncio.gather(*(invites.fulfill_paid_credit(**args) for _ in range(4)))
        assert sum(r["credited"] for r in rows) == 1
        assert (await invites.fulfill_paid_credit(**{**args, "event_id": "evt_synthetic2"}))["duplicate"] is True
        with pytest.raises(ValueError):
            await invites.fulfill_paid_credit(**{**args, "event_id": "evt_synthetic2", "payment_id": "cs_other"})
        with pytest.raises(ValueError):
            await invites.fulfill_paid_credit(**{**args, "user_id": "other"})
        for field, value in (("amount", True), ("amount", 1), ("currency", "usd")):
            with pytest.raises(ValueError):
                await invites.fulfill_paid_credit(**{**args, field: value})
    asyncio.run(run())
    assert client.get("/api/account/invitations", headers=headers()).json()["paidCredits"] == 1
    value = mint(client, creditSource="paid").json()
    assert value["creditSource"] == "paid"
    assert mint(client, creditSource="paid").status_code == 409
    assert client.get("/api/account/invitations", headers=headers()).json()["annualUsed"] == 0
    assert client.request("DELETE", "/api/account/invitations/" + value["id"], headers=headers(), json={"expectedAccountId": "alice"}).status_code == 200
    assert mint(client, creditSource="paid").status_code == 409
    assert client.post("/api/invitations/payments", headers=headers(), json={}).status_code == 404


def test_transaction_failure_rolls_back_allowance_and_member(client, monkeypatch):
    from contextlib import asynccontextmanager
    original = invites.private_write
    @asynccontextmanager
    async def abort():
        async with original() as db:
            yield db
            raise HTTPException(503, "Synthetic rollback")
    token = mint(client).json()["token"]
    monkeypatch.setattr(invites, "private_write", abort)
    assert mint(client).status_code == 503
    assert redeem(client, token).status_code == 503
    async def check():
        db = await database.get_db()
        assert not await db.execute_fetchall("SELECT * FROM private_members WHERE user_id='new-user'")
        assert len(await db.execute_fetchall("SELECT * FROM membership_invitations")) == 1
    asyncio.run(check())


def test_rate_state_bounded_and_stale_buckets_purged(monkeypatch):
    clock = 10.0
    monkeypatch.setattr(invites.time, "monotonic", lambda: clock)
    monkeypatch.setattr(invites, "_RATE_BUCKET_LIMIT", 2)
    invites._rate("a", 1)
    invites._rate("b", 1)
    with pytest.raises(HTTPException) as exc:
        invites._rate("c", 1)
    assert exc.value.status_code == 429 and len(invites._RATE_BUCKETS) == 2
    clock += 61
    invites._rate("c", 1)
    assert list(invites._RATE_BUCKETS) == ["c"]


@pytest.mark.parametrize("raw", [b'{"token":"one","token":"two"}', b'{"token":NaN}',
                                 b'{"token":"\xff"}', b'{', b'[]',
                                 b'{"token":' + b'[' * 2000 + b'0' + b']' * 2000 + b'}'])
def test_malformed_preview_json_is_uniform_and_never_reflects_input(client, raw):
    response = client.post("/api/invitations/preview", headers={"Host": "library.example.org", "Content-Type": "application/json"}, content=raw)
    assert response.status_code == 200 and response.json() == {"available": False}


def test_duplicate_mutation_fields_rejected_without_token_reflection(client):
    response = client.post("/api/account/invitations", headers={**headers(), "Content-Type": "application/json"}, content=b'{"expectedAccountId":"alice","expectedAccountId":"other"}')
    assert response.status_code == 422 and response.json() == {"detail": "Invalid invitation request"}


def test_chunked_body_bounded_before_json_even_with_false_content_length():
    async def run():
        chunks_read = 0
        async def body():
            nonlocal chunks_read
            for _ in range(3):
                chunks_read += 1
                yield b"x" * 5000
        transport = httpx.ASGITransport(app=_with_transport_peer(app_module.app, "127.0.0.1"))
        async with httpx.AsyncClient(transport=transport, base_url="https://library.example.org") as c:
            response = await c.post("/api/invitations/preview", headers={"Content-Type": "application/json", "Content-Length": "1"}, content=body())
            assert response.status_code == 413 and chunks_read == 2
    asyncio.run(run())


def test_mint_rechecks_membership_after_waiting_for_transaction():
    async def run():
        await private._WRITE_LOCK.acquire()
        transport = httpx.ASGITransport(app=_with_transport_peer(app_module.app, "127.0.0.1"))
        async with httpx.AsyncClient(transport=transport, base_url="https://library.example.org") as c:
            request = asyncio.create_task(c.post("/api/account/invitations", headers=headers(), json={"expectedAccountId": "alice"}))
            await asyncio.sleep(.01)
            await member("alice", False)
            private._WRITE_LOCK.release()
            response = await request
            assert response.status_code == 403
            assert not await (await database.get_db()).execute_fetchall("SELECT * FROM membership_invitations")
    asyncio.run(run())


def test_metadata_is_bounded_and_account_scoped(client):
    async def run():
        db = await database.get_db()
        now = invites._now()
        await db.executemany("INSERT INTO membership_invitations VALUES (?,?,?,?,?,?,?,NULL,NULL,NULL)",
                             [(f"{i:032x}", f"synthetic-digest-{i}", "owner", invites._year(now), "annual", now+i/1000, now+86400) for i in range(125)])
        await db.commit()
    asyncio.run(run())
    owner = client.get("/api/account/invitations", headers=headers("owner")).json()
    assert owner["annualUsed"] == 125 and len(owner["invitations"]) == 100
    assert client.get("/api/account/invitations", headers=headers()).json()["invitations"] == []
    assert all(set(item) == {"id", "createdAt", "expiresAt", "status"} for item in owner["invitations"])


def test_startup_settings_exact_owner_and_disabled_route_no_ledger_access(monkeypatch):
    invites.validate_settings()
    for key, value in (("invitation_owner_ids", "api-client"), ("invitation_annual_allowance", -1),
                       ("invitation_price_usd_cents", 0), ("private_indexer_enabled", False), ("require_auth", False)):
        original = getattr(config.settings, key)
        monkeypatch.setattr(config.settings, key, value)
        with pytest.raises(RuntimeError):
            invites.validate_settings()
        monkeypatch.setattr(config.settings, key, original)
    monkeypatch.setattr(config.settings, "private_invitations_enabled", False)
    async def forbidden():
        raise AssertionError("Disabled route touched ledger")
    monkeypatch.setattr(invites, "get_db", forbidden)
    async def run():
        transport = httpx.ASGITransport(app=_with_transport_peer(app_module.app, "127.0.0.1"))
        async with httpx.AsyncClient(transport=transport, base_url="https://library.example.org") as c:
            assert (await c.get("/api/account/invitations", headers=headers())).json()["enabled"] is False
            assert (await c.post("/api/invitations/preview", json={"token": "unknown"})).status_code == 404
    asyncio.run(run())

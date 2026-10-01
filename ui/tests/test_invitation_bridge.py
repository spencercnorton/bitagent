"""Purpose-signed bootstrap never substitutes for verified human SSO."""
import asyncio
import hashlib
import hmac
import json
import os
import secrets
import time
from types import SimpleNamespace

import httpx
import pytest

import app as app_module
import config
import database
import invitation_bridge as bridge
import invitations
import private_indexer as private
from conftest import _with_transport_peer
from test_invitations import PROOF, headers, invitation_config as _invitation_config, member, mint

invitation_config = _invitation_config

KEY = b"synthetic-dedicated-bootstrap-key-at-least32bytes"
ORIGIN = "https://console.example.org"


@pytest.fixture(autouse=True)
def bridge_config(invitation_config, monkeypatch, tmp_path):
    key = tmp_path / "bridge-key"
    key.write_bytes(KEY)
    key.chmod(0o600)
    for field, value in {
        "private_invitation_bridge_enabled": True,
        "invitation_bridge_url": ORIGIN + bridge.PATH,
        "invitation_bridge_secret_file": str(key),
        "invitation_sign_in_url": "https://sso.example.org/invitations/start",
    }.items():
        monkeypatch.setattr(config.settings, field, value)
    bridge.validate_settings()
    async def clear():
        db = await database.get_db()
        await db.execute("DELETE FROM invitation_bootstrap_enrollments")
        await db.execute("DELETE FROM invitation_bootstrap_nonces")
        await db.commit()
    asyncio.run(clear())
    yield
    bridge._KEY, bridge._CONFIG = None, None


def start_body(token, **changes):
    return {"version": 1, "operation": "start", "token": token,
            "authRequestId": secrets.token_hex(16), "browserBindingHash": "a" * 64, **changes}


def redemption(body, enrollment_id, **changes):
    return {"version": 1, "operation": "redeem", "enrollmentId": enrollment_id,
            "authRequestId": body["authRequestId"], "principalId": 12345,
            "provider": "google", "subjectBindingHash": "b" * 64, **changes}


def signed(body, *, timestamp=None, nonce=None, key=KEY, raw=None, direction="request", **changes):
    raw = raw if raw is not None else json.dumps(body, separators=(",", ":")).encode()
    timestamp = str(int(time.time())) if timestamp is None else timestamp
    nonce = nonce or secrets.token_urlsafe(24)
    digest = hashlib.sha256(raw).hexdigest()
    message = bridge.canonical(direction=direction, origin=ORIGIN, operation=body["operation"],
        timestamp=timestamp, nonce=nonce, body_hash=digest)
    headers_out = {"Host": "console.example.org", "Content-Type": "application/json",
        "X-BitAgent-Proxy-Proof": PROOF, "X-Invitation-Client": bridge.CLIENT,
        "X-Invitation-Time": timestamp, "X-Invitation-Nonce": nonce,
        "X-Invitation-Signature": hmac.new(key, message, hashlib.sha256).hexdigest(), **changes}
    return raw, headers_out


def rpc(client, body, **kwargs):
    raw, h = signed(body, **kwargs)
    response = client.post(bridge.PATH, headers=h, content=raw)
    if response.headers.get("X-Invitation-Signature"):
        message = bridge.canonical(direction="response", origin=ORIGIN, operation=body["operation"],
            timestamp=response.headers["X-Invitation-Time"], nonce=h["X-Invitation-Nonce"],
            body_hash=hashlib.sha256(response.content).hexdigest(), request_hash=hashlib.sha256(raw).hexdigest(),
            status=response.status_code)
        assert response.headers["X-Invitation-Signature"] == hmac.new(KEY, message, hashlib.sha256).hexdigest()
        assert len(response.content) <= 4096
    return response


def test_actual_start_redeem_status_and_no_peer_or_identity_changes(client):
    token = mint(client).json()["token"]
    begin = start_body(token)
    started = rpc(client, begin)
    assert started.status_code == 200
    enrollment_id = started.json()["enrollmentId"]
    assert bridge._HEX_ID.fullmatch(enrollment_id)
    assert 590 <= started.json()["expiresAt"] - time.time() <= 600
    assert rpc(client, begin).json() == started.json()
    bound = redemption(begin, enrollment_id)
    pending = rpc(client, {**bound, "operation": "status"}).json()
    assert pending["approved"] is False and pending["state"] == "unavailable"
    redeemed = rpc(client, bound)
    assert redeemed.status_code == 200
    assert redeemed.json()["accountId"] == "-12345" and redeemed.json()["alreadyRedeemed"] is False
    assert rpc(client, bound).json()["alreadyRedeemed"] is True
    status = rpc(client, {**bound, "operation": "status"}).json()
    assert status["approved"] is True and status["state"] == "redeemed"
    assert 0 < status["expiresAt"] - status["issuedAt"] <= 30
    async def check():
        db = await database.get_db()
        assert (await db.execute_fetchall("SELECT active FROM private_members WHERE user_id='-12345'"))[0]["active"] == 1
        for table in ("private_peers", "user_api_keys"):
            assert not await db.execute_fetchall(f"SELECT * FROM {table} WHERE user_id='-12345'")
        assert token not in str([dict(r) for r in await db.execute_fetchall("SELECT * FROM invitation_bootstrap_enrollments")])
    asyncio.run(check())


def test_binding_retry_cannot_switch_token_browser_principal_or_provider(client):
    token, other_token = mint(client).json()["token"], mint(client).json()["token"]
    begin = start_body(token)
    enrollment = rpc(client, begin).json()["enrollmentId"]
    assert rpc(client, {**begin, "token": other_token}).status_code == 409
    assert rpc(client, {**begin, "browserBindingHash": "c" * 64}).status_code == 409
    bound = redemption(begin, enrollment)
    assert rpc(client, bound).status_code == 200
    for changed in ({"principalId": 12}, {"provider": "discord"}, {"subjectBindingHash": "c" * 64}, {"authRequestId": "d" * 32}):
        assert rpc(client, {**bound, **changed}).status_code == 409
        assert rpc(client, {**bound, **changed, "operation": "status"}).json()["approved"] is False


@pytest.mark.parametrize("withdraw", ["issuer", "member", "invite", "enrollment"])
def test_recovery_requires_current_issuer_member_and_expiry(client, withdraw):
    token = mint(client).json()["token"]
    begin = start_body(token)
    eid = rpc(client, begin).json()["enrollmentId"]
    bound = redemption(begin, eid)
    assert rpc(client, bound).status_code == 200
    async def close():
        db = await database.get_db()
        if withdraw in {"issuer", "member"}:
            await db.execute("UPDATE private_members SET active=0 WHERE user_id=?", ("alice" if withdraw == "issuer" else "-12345",))
        elif withdraw == "invite":
            await db.execute("UPDATE membership_invitations SET expires_at=0")
        else:
            await db.execute("UPDATE invitation_bootstrap_enrollments SET expires_at=0")
        await db.commit()
    asyncio.run(close())
    assert rpc(client, bound).status_code == 409
    assert rpc(client, {**bound, "operation": "status"}).json()["approved"] is False


def test_inactive_existing_subject_does_not_reactivate(client):
    asyncio.run(member("-12345", False))
    begin = start_body(mint(client).json()["token"])
    eid = rpc(client, begin).json()["enrollmentId"]
    assert rpc(client, redemption(begin, eid)).status_code == 403
    async def check():
        db = await database.get_db()
        assert (await db.execute_fetchall("SELECT active FROM private_members WHERE user_id='-12345'"))[0]["active"] == 0
        assert not await db.execute_fetchall("SELECT * FROM membership_invitations WHERE redeemed_at IS NOT NULL")
    asyncio.run(check())


def test_concurrent_principals_one_consumption_and_response_lost_recovery(client):
    begin = start_body(mint(client).json()["token"])
    eid = rpc(client, begin).json()["enrollmentId"]
    async def run():
        transport = httpx.ASGITransport(app=_with_transport_peer(app_module.app, "127.0.0.1"))
        async with httpx.AsyncClient(transport=transport, base_url=ORIGIN) as c:
            requests = [redemption(begin, eid, principalId=p) for p in (321, 654)]
            async def call(body):
                raw, h = signed(body)
                return await c.post(bridge.PATH, headers=h, content=raw)
            result = await asyncio.gather(*(call(b) for b in requests))
            assert sorted(r.status_code for r in result) == [200, 409]
            winner = next(r.json()["principalId"] for r in result if r.status_code == 200)
            status = await call(redemption(begin, eid, principalId=winner, operation="status"))
            assert status.json()["approved"] is True
            assert len(await (await database.get_db()).execute_fetchall("SELECT * FROM private_members WHERE user_id IN ('-321','-654')")) == 1
    asyncio.run(run())


@pytest.mark.parametrize("changes", [
    {"Host": "library.example.org"}, {"Host": "console.example.org:443"},
    {"Cookie": "session=forged"}, {"Authorization": "Bearer forged"},
    {"X-Auth-User-Id": "owner"}, {"X-Auth-Priv": "OWNER"},
    {"X-Forwarded-User": "owner"}, {"X-BitAgent-Proxy-Proof": "forged"},
    {"X-Invitation-Client": "operator"}, {"Origin": "https://library.example.org"},
])
def test_bridge_rejects_public_human_and_unproven_machine_ingress(client, changes):
    begin = start_body(mint(client).json()["token"])
    assert rpc(client, begin, **changes).status_code in {401, 404}


def test_nonce_replay_expired_future_badkey_direction_and_duplicates(client):
    begin = start_body(mint(client).json()["token"])
    raw, h = signed(begin)
    assert client.post(bridge.PATH, headers=h, content=raw).status_code == 200
    assert client.post(bridge.PATH, headers=h, content=raw).status_code == 401
    for ts in (str(int(time.time()) - 61), str(int(time.time()) + 61), "01", "-1"):
        assert rpc(client, begin, timestamp=ts).status_code == 401
    assert rpc(client, begin, key=b"wrong-key").status_code == 401
    assert rpc(client, begin, direction="response").status_code == 401
    hlist = list(signed(begin)[1].items()) + [("X-Invitation-Time", str(int(time.time()))) ]
    assert client.post(bridge.PATH, headers=hlist, content=raw).status_code == 401


@pytest.mark.parametrize("change", [
    {"principalId": True}, {"principalId": 0}, {"principalId": -1}, {"principalId": 2**63},
    {"principalId": "12345"}, {"provider": "telegram"}, {"provider": []},
    {"subjectBindingHash": "B" * 64}, {"accountId": "owner"}, {"version": True},
])
def test_strict_provider_principal_and_unknown_fields(client, change):
    body = redemption(start_body("bi_" + "a" * 43), "b" * 32, **change)
    assert rpc(client, body).status_code == 422


def test_no_query_alternate_path_method_oversize_or_untrusted_transport(client):
    body = start_body(mint(client).json()["token"])
    raw, h = signed(body)
    for path in (bridge.PATH + "?token=secret", bridge.PATH + "/", bridge.PATH + "/extra"):
        assert client.post(path, headers=h, content=raw).status_code == 404
    assert client.get(bridge.PATH, headers=h).status_code == 404
    assert client.post(bridge.PATH, headers=h, content=b"x" * 4097).status_code == 413
    async def untrusted():
        transport = httpx.ASGITransport(app=_with_transport_peer(app_module.app, "203.0.113.123"))
        async with httpx.AsyncClient(transport=transport, base_url=ORIGIN) as c:
            return await c.post(bridge.PATH, headers=h, content=raw)
    assert asyncio.run(untrusted()).status_code == 401


def test_default_off_and_startup_protected_key_and_exact_origins(client, monkeypatch, tmp_path):
    monkeypatch.setattr(config.settings, "private_invitation_bridge_enabled", False)
    bridge.validate_settings()
    assert rpc(client, start_body("bi_" + "a" * 43)).status_code == 404
    assert bridge.sign_in_url() == ""
    monkeypatch.setattr(config.settings, "private_invitation_bridge_enabled", True)
    original = config.settings.invitation_bridge_url
    for url in ("http://console.example.org" + bridge.PATH, original + "/", original + "?x=1",
                original.replace("console", "CONSOLE"), original.replace(".org", ".org:443"),
                "https://library.example.org" + bridge.PATH):
        monkeypatch.setattr(config.settings, "invitation_bridge_url", url)
        with pytest.raises(RuntimeError):
            bridge.validate_settings()
    monkeypatch.setattr(config.settings, "invitation_bridge_url", original)
    path = config.settings.invitation_bridge_secret_file
    os.chmod(path, 0o644)
    with pytest.raises(RuntimeError):
        bridge.validate_settings()
    os.chmod(path, 0o600)
    linked = tmp_path / "key-symlink"
    linked.symlink_to(path)
    monkeypatch.setattr(config.settings, "invitation_bridge_secret_file", str(linked))
    with pytest.raises(RuntimeError):
        bridge.validate_settings()


def test_landing_only_form_origin_and_feature_off(client, monkeypatch):
    landing = client.get("/invite", headers={"Host": "library.example.org"})
    assert "form-action 'self' https://sso.example.org" in landing.headers["Content-Security-Policy"]
    management = client.get("/invitations", headers=headers())
    assert "sso.example.org" not in management.headers["Content-Security-Policy"]
    monkeypatch.setattr(config.settings, "private_invitation_bridge_enabled", False)
    landing = client.get("/invite", headers={"Host": "library.example.org"})
    assert "sso.example.org" not in landing.headers["Content-Security-Policy"]


@pytest.mark.parametrize("raw", [b'{"operation":[]}', b'{"operation":{}}', b'{"operation":null}', b'[]',
                                b'{"operation":"start","operation":"redeem"}', b'{"version":NaN}',
                                b'[' * 2000 + b']' * 2000])
def test_malformed_json_is_bounded_and_sanitized(client, raw):
    body = start_body("bi_" + "a" * 43)
    assert rpc(client, body, raw=raw).status_code == 422


def test_failure_after_membership_write_rolls_back_but_consumes_request_nonce(client, monkeypatch):
    begin = start_body(mint(client).json()["token"])
    eid = rpc(client, begin).json()["enrollmentId"]
    original = invitations._redeem_for_subject
    async def fail_after_write(db, row, uid, now):
        await original(db, row, uid, now)
        raise bridge.HTTPException(409, "Invitation unavailable")
    monkeypatch.setattr(invitations, "_redeem_for_subject", fail_after_write)
    raw, h = signed(redemption(begin, eid))
    assert client.post(bridge.PATH, headers=h, content=raw).status_code == 409
    assert client.post(bridge.PATH, headers=h, content=raw).status_code == 401
    async def check():
        db = await database.get_db()
        assert not await db.execute_fetchall("SELECT * FROM private_members WHERE user_id='-12345'")
        assert not await db.execute_fetchall("SELECT * FROM membership_invitations WHERE redeemed_at IS NOT NULL")
        assert not await db.execute_fetchall("SELECT * FROM invitation_bootstrap_enrollments WHERE principal_id IS NOT NULL")
    asyncio.run(check())


def test_enrollment_and_replay_ledgers_are_bounded(client, monkeypatch):
    begin = start_body(mint(client).json()["token"])
    monkeypatch.setattr(bridge, "_ACTIVE_LIMIT", 1)
    assert rpc(client, begin).status_code == 200
    assert rpc(client, {**begin, "authRequestId": "b" * 32}).status_code == 429
    monkeypatch.setattr(bridge, "_NONCE_LIMIT", 2)
    assert rpc(client, begin).status_code == 429
    async def check():
        db = await database.get_db()
        assert len(await db.execute_fetchall("SELECT * FROM invitation_bootstrap_enrollments")) == 1
        assert len(await db.execute_fetchall("SELECT * FROM invitation_bootstrap_nonces")) == 2
    asyncio.run(check())


def test_disabled_bridge_never_reads_database_or_key(client, monkeypatch):
    monkeypatch.setattr(config.settings, "private_invitation_bridge_enabled", False)
    monkeypatch.setattr(config.settings, "invitation_bridge_secret_file", "/unavailable/secret")
    bridge.validate_settings()
    async def forbidden():
        raise AssertionError("Disabled bridge touched database")
    monkeypatch.setattr(bridge, "get_db", forbidden)
    assert rpc(client, start_body("bi_" + "a" * 43)).status_code == 404


def test_cancellation_while_writer_is_busy_creates_no_enrollment_or_nonce(client):
    begin = start_body(mint(client).json()["token"])
    async def run():
        transport = httpx.ASGITransport(app=_with_transport_peer(app_module.app, "127.0.0.1"))
        raw, h = signed(begin)
        await private._WRITE_LOCK.acquire()
        try:
            async with httpx.AsyncClient(transport=transport, base_url=ORIGIN) as c:
                task = asyncio.create_task(c.post(bridge.PATH, headers=h, content=raw))
                await asyncio.sleep(.02)
                task.cancel()
                with pytest.raises(asyncio.CancelledError):
                    await task
        finally:
            private._WRITE_LOCK.release()
        db = await database.get_db()
        assert not await db.execute_fetchall("SELECT * FROM invitation_bootstrap_enrollments")
        assert not await db.execute_fetchall("SELECT * FROM invitation_bootstrap_nonces")
    asyncio.run(run())


@pytest.mark.parametrize("source", ["invite", "enrollment"])
def test_subsecond_authority_rolls_back_membership_and_consumption(client, monkeypatch, source):
    begin = start_body(mint(client).json()["token"])
    eid = rpc(client, begin).json()["enrollmentId"]
    now = int(time.time()) + .125
    monkeypatch.setattr(bridge, "time", SimpleNamespace(time=lambda: now, monotonic=time.monotonic))
    async def expire():
        db = await database.get_db()
        table = "membership_invitations" if source == "invite" else "invitation_bootstrap_enrollments"
        await db.execute(f"UPDATE {table} SET expires_at=?", (now + .25,))
        await db.commit()
    asyncio.run(expire())
    assert rpc(client, redemption(begin, eid)).status_code == 409
    async def check():
        db = await database.get_db()
        assert not await db.execute_fetchall("SELECT * FROM private_members WHERE user_id='-12345'")
        assert not await db.execute_fetchall("SELECT * FROM membership_invitations WHERE redeemed_at IS NOT NULL")
        assert not await db.execute_fetchall("SELECT * FROM invitation_bootstrap_enrollments WHERE principal_id IS NOT NULL")
    asyncio.run(check())


def test_subsecond_recovery_never_returns_approved_authority(client, monkeypatch):
    begin = start_body(mint(client).json()["token"])
    eid = rpc(client, begin).json()["enrollmentId"]
    bound = redemption(begin, eid)
    assert rpc(client, bound).status_code == 200
    now = int(time.time()) + .125
    monkeypatch.setattr(bridge, "time", SimpleNamespace(time=lambda: now, monotonic=time.monotonic))
    async def expire():
        db = await database.get_db()
        await db.execute("UPDATE invitation_bootstrap_enrollments SET expires_at=?", (now + .25,))
        await db.commit()
    asyncio.run(expire())
    assert rpc(client, bound).status_code == 409
    status = rpc(client, {**bound, "operation": "status"}).json()
    assert status["approved"] is False and status["state"] == "unavailable"
    assert status["expiresAt"] > status["issuedAt"]


@pytest.mark.parametrize("reference", ["proxy_auth_secret", "dashboard_api_key", "private_indexer_secret"])
def test_whitespace_copy_of_other_credentials_is_not_a_distinct_bridge_key(monkeypatch, reference):
    other = "  synthetic-other-purpose-secret-at-least32bytes  "
    monkeypatch.setattr(config.settings, reference, other)
    with open(config.settings.invitation_bridge_secret_file, "wb") as out:
        out.write(other.strip().encode() + b"\n")
    with pytest.raises(RuntimeError, match="distinct protected"):
        bridge.validate_settings()
    assert bridge._KEY is None

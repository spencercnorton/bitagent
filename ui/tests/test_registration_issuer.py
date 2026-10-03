"""Explicit owner admission uses real ASGI and SQLite, without private access."""
import asyncio

import httpx
import pytest

import app as app_module
import config
import database
import registration_issuer as issuer
from conftest import _with_transport_peer
from test_invitations import headers, member, mint
from test_site_registration import (
    bridge_config as _bridge_config, invitation_config as _invitation_config,
    registration_config as _registration_config,
)

invitation_config = _invitation_config
bridge_config = _bridge_config
registration_config = _registration_config
OWNER = "-4242"
ORIGIN = "https://console.example.org"


@pytest.fixture(autouse=True)
def issuer_config(registration_config, monkeypatch):
    monkeypatch.setattr(config.settings, "site_registration_issuer_admission_enabled", True)
    monkeypatch.setattr(config.settings, "operator_ui_url", ORIGIN)
    monkeypatch.setattr(config.settings, "operator_roles", "OWNER")
    issuer.validate_settings()

    async def clear():
        db = await database.get_db()
        await db.execute("DELETE FROM private_members")
        await db.commit()
    asyncio.run(clear())


def owner_headers(user=OWNER, **changes):
    return headers(user, role="OWNER", Host="console.example.org", Origin=ORIGIN, **changes)


def admit(client, user=OWNER, **changes):
    return client.put(issuer.PATH, headers=owner_headers(user), json={"expectedAccountId": user, **changes})


async def snapshot():
    db = await database.get_db()
    tables = await db.execute_fetchall("SELECT name FROM sqlite_master WHERE type='table' AND name NOT LIKE 'sqlite_%' ORDER BY name")
    return {row["name"]: [tuple(value) for value in await db.execute_fetchall(f'SELECT * FROM "{row["name"]}" ORDER BY rowid')]
            for row in tables}


def test_defaults_off_and_startup_only(client, monkeypatch):
    assert config.Settings.model_fields["site_registration_issuer_admission_enabled"].default is False
    assert "site_registration_issuer_admission_enabled" in config.FROZEN_OVERRIDE_FIELDS
    monkeypatch.setattr(config.settings, "site_registration_issuer_admission_enabled", False)
    for path in (issuer.PATH, issuer.PAGE):
        assert client.get(path, headers=owner_headers()).status_code == 404
    assert admit(client).status_code == 404
    assert '/registration-admin' not in client.get("/", headers=owner_headers()).text


@pytest.mark.parametrize("field,value", [
    ("site_registration_enabled", False), ("private_invitations_enabled", False),
    ("private_invitation_bridge_enabled", False), ("private_indexer_enabled", True),
    ("private_invitation_checkout_enabled", True), ("require_auth", False),
    ("operator_roles", "VIEWER"), ("trust_npm_headers", False),
])
def test_unsafe_startup_and_runtime_boundary_denied(client, field, value, monkeypatch):
    monkeypatch.setattr(config.settings, field, value)
    with pytest.raises(RuntimeError):
        issuer.validate_settings()
    assert admit(client).status_code == 404


@pytest.mark.parametrize("origin", [
    "", "http://console.example.org", ORIGIN + "/", ORIGIN + "/path",
    ORIGIN + "?", ORIGIN + "#", ORIGIN + ":443", "https://user@console.example.org",
    "https://CONSOLE.example.org", ORIGIN + ".", "https://elsewhere.example.org",
    "https://console..example.org", ORIGIN + "\\other", ORIGIN + "\n", "https://[invalid",
])
def test_noncanonical_operator_origin_fails_startup(origin, monkeypatch):
    monkeypatch.setattr(config.settings, "operator_ui_url", origin)
    with pytest.raises(RuntimeError):
        issuer.validate_settings()


def test_status_and_page_never_admit_but_explicit_self_action_preserves_every_other_table(client):
    assert client.post("/api/account/api-key", headers=headers(OWNER), json={"name": "synthetic"}).status_code == 200
    before = asyncio.run(snapshot())
    response = client.get(issuer.PATH, headers=owner_headers())
    assert response.json() == {"accountId": OWNER, "active": False, "suspended": False}
    page = client.get(issuer.PAGE, headers=owner_headers())
    assert page.status_code == 200 and 'data-registration-account-id="-4242"' in page.text
    assert 'Enable invitations for my account' in page.text
    assert page.headers["Cache-Control"] == "no-store" and page.headers["Referrer-Policy"] == "no-referrer"
    assert '/registration-admin' in client.get("/", headers=owner_headers()).text
    assert asyncio.run(snapshot()) == before
    accepted = admit(client)
    assert accepted.status_code == 200 and accepted.json() == {"accountId": OWNER, "active": True, "suspended": False}
    after = asyncio.run(snapshot())
    changed = {name for name in before if before[name] != after[name]}
    assert changed == {"private_members"}
    assert len(after["private_members"]) == 1
    row = after["private_members"][0]
    assert row[:3] == (OWNER, 1, OWNER)
    assert all(value["private_access"] == 0 for value in asyncio.run(_key_rows()))
    assert admit(client).status_code == 200 and asyncio.run(snapshot()) == after
    minted = mint(client, OWNER)
    assert minted.status_code == 201


@pytest.mark.parametrize("suffix", ["/admin", "/admin/"])
def test_canonical_admin_origin_keeps_exact_operator_transport_and_owner_csrf_gate(client, monkeypatch, suffix):
    monkeypatch.setattr(config.settings, "operator_ui_url", "https://library.example.org" + suffix)
    monkeypatch.setattr(config.settings, "operator_ingress_host", "console.example.org")
    issuer.validate_settings()
    h = owner_headers()
    h["Origin"] = "https://library.example.org"
    page = client.get(issuer.PAGE, headers=h)
    assert page.status_code == 200
    assert 'data-operator-path="/admin"' in page.text
    assert 'href="/admin/"' in page.text
    before = asyncio.run(snapshot())
    # A browser/public Host and alternate operator Hosts cannot impersonate the
    # fixed ingress, even with matching forwarded-host and owner claims.
    for host in ["library.example.org", "console-alt.example.org", "console.example.org:443"]:
        denied = {**h, "Host": host, "X-Forwarded-Host": "library.example.org"}
        assert client.put(issuer.PATH, headers=denied, json={"expectedAccountId": OWNER}).status_code == 404
    for origin in [ORIGIN, "https://library.example.org/admin", "https://foreign.example.org"]:
        assert client.put(issuer.PATH, headers={**h, "Origin": origin}, json={"expectedAccountId": OWNER}).status_code == 403
    assert client.put(issuer.PATH, headers={**h, "X-Auth-Priv": "VIEWER"}, json={"expectedAccountId": OWNER}).status_code == 403
    assert asyncio.run(snapshot()) == before
    assert client.put(issuer.PATH, headers=h, json={"expectedAccountId": OWNER}).status_code == 200
    after = asyncio.run(snapshot())
    assert {name for name in before if before[name] != after[name]} == {"private_members"}


@pytest.mark.parametrize("ingress", ["", "library.example.org", "console.example.org:443", "CONSOLE.example.org", "unknown.example.org"])
def test_canonical_admin_issuer_requires_explicit_exact_operator_ingress(ingress, monkeypatch):
    monkeypatch.setattr(config.settings, "operator_ui_url", "https://library.example.org/admin")
    monkeypatch.setattr(config.settings, "operator_ingress_host", ingress)
    with pytest.raises(RuntimeError):
        issuer.validate_settings()


async def _key_rows():
    db = await database.get_db()
    return [dict(row) for row in await db.execute_fetchall("SELECT * FROM user_api_keys WHERE user_id=?", (OWNER,))]


@pytest.mark.parametrize("user", ["-8888", "principal:8888", "synthetic-oauth-subject"])
def test_exact_existing_namespace_is_preserved_without_aliases(client, user):
    assert admit(client, user).status_code == 200

    async def check():
        db = await database.get_db()
        rows = await db.execute_fetchall("SELECT user_id,actor FROM private_members")
        assert [tuple(row) for row in rows] == [(user, user)]
    asyncio.run(check())


def test_suspension_never_reactivated_and_other_account_cannot_be_chosen(client):
    asyncio.run(member(OWNER, False))
    before = asyncio.run(snapshot())
    assert client.get(issuer.PATH, headers=owner_headers()).json()["suspended"] is True
    assert admit(client).status_code == 409
    assert admit(client, expectedAccountId="other").status_code == 403
    assert asyncio.run(snapshot()) == before


@pytest.mark.parametrize("changes,expected", [
    ({"X-Auth-Priv": "VIEWER"}, 403), ({"X-Auth-Priv": "OWNER "}, 403),
    ({"X-BitAgent-Proxy-Proof": "bad"}, 401),
    ({"X-Auth-User-Id": ""}, 401),
    ({"X-Auth-User": "other"}, 403), ({"X-Forwarded-User": "other"}, 403),
    ({"Authorization": "Bearer synthetic-machine"}, 403), ({"X-Api-Key": "synthetic-machine"}, 403),
    ({"Origin": "https://foreign.example.org"}, 403), ({"Origin": "null"}, 403),
    ({"Sec-Fetch-Site": "same-site"}, 403),
])
def test_bad_authority_or_origin_cannot_admit(client, changes, expected):
    before = asyncio.run(snapshot())
    h = owner_headers()
    h.update(changes)
    assert client.put(issuer.PATH, headers=h, json={"expectedAccountId": OWNER}).status_code == expected
    assert asyncio.run(snapshot()) == before


@pytest.mark.parametrize("header", ["X-Auth-User-Id", "X-Auth-Priv", "X-BitAgent-Proxy-Proof", "Origin", "Content-Type"])
def test_duplicate_authority_or_origin_headers_denied(client, header):
    h = [*owner_headers().items(), ("Content-Type", "application/json")]
    value = dict(h)[header] if header in dict(h) else "application/json"
    h.append((header, value))
    before = asyncio.run(snapshot())
    assert client.put(issuer.PATH, headers=h, content='{"expectedAccountId":"-4242"}').status_code in {403, 415}
    assert asyncio.run(snapshot()) == before


def test_missing_proof_or_origin_and_machine_tier_fail_closed(client, monkeypatch):
    for removed in ("X-BitAgent-Proxy-Proof", "Origin"):
        h = owner_headers()
        h.pop(removed)
        assert client.put(issuer.PATH, headers=h, json={"expectedAccountId": OWNER}).status_code == 403
    monkeypatch.setattr(config.settings, "dashboard_api_key", "synthetic-dashboard-key")
    h = owner_headers(Authorization="Bearer synthetic-dashboard-key")
    assert client.put(issuer.PATH, headers=h, json={"expectedAccountId": OWNER}).status_code == 403


@pytest.mark.parametrize("host,status", [
    ("library.example.org", 404), ("console-alt.example.org", 404),
    ("console.example.org:443", 404), ("CONSOLE.example.org", 404),
    ("console.example.org.", 404), ("foreign.example.org", 421),
])
def test_all_host_aliases_hide_admission(client, host, status):
    h = owner_headers()
    h["Host"] = host
    for path in (issuer.PATH, issuer.PAGE):
        assert client.get(path, headers=h).status_code == status
    assert client.put(issuer.PATH, headers=h, json={"expectedAccountId": OWNER}).status_code == status


@pytest.mark.parametrize("raw,status", [
    ('{}', 422), ('[]', 422), ('null', 422), ('{"expectedAccountId":true}', 403),
    ('{"expectedAccountId": "-4242", "actor": "other"}', 422),
    ('{"expectedAccountId":"-4242","expectedAccountId":"other"}', 422),
    ('{"expectedAccountId":"-4242","active":true}', 422), ('{', 422),
    ('{"expectedAccountId":"' + 'a' * 2048 + '"}', 413),
])
def test_bounded_unambiguous_body_cannot_select_authority(client, raw, status):
    h = owner_headers()
    h["Content-Type"] = "application/json"
    before = asyncio.run(snapshot())
    assert client.put(issuer.PATH, headers=h, content=raw).status_code == status
    assert asyncio.run(snapshot()) == before


def test_unverified_transport_cannot_spoof_owner_and_forwarded_aliases_remain_consistent():
    async def run():
        transport = httpx.ASGITransport(app=_with_transport_peer(app_module.app, "198.51.100.99"))
        async with httpx.AsyncClient(transport=transport, base_url=ORIGIN) as client:
            assert (await client.put(issuer.PATH, headers=owner_headers(), json={"expectedAccountId": OWNER})).status_code == 401
    asyncio.run(run())


def test_explicit_forwarded_human_tier_keeps_same_exact_identity(client, monkeypatch):
    monkeypatch.setattr(config.settings, "trust_npm_headers", False)
    monkeypatch.setattr(config.settings, "trust_forwarded_user", True)
    h = owner_headers()
    h.pop("X-Auth-User-Id")
    h["X-Forwarded-User"] = OWNER
    assert client.put(issuer.PATH, headers=h, json={"expectedAccountId": OWNER}).json()["accountId"] == OWNER


def test_a_slow_body_has_a_bounded_deadline_and_no_membership_write(monkeypatch):
    monkeypatch.setattr(issuer, "DEADLINE", .01)

    async def run():
        async def slow():
            yield b'{"expectedAccountId":'
            await asyncio.sleep(.05)
            yield b'"-4242"}'
        transport = httpx.ASGITransport(app=_with_transport_peer(app_module.app, "127.0.0.1"))
        async with httpx.AsyncClient(transport=transport, base_url=ORIGIN) as client:
            h = owner_headers()
            h["Content-Type"] = "application/json"
            assert (await client.put(issuer.PATH, headers=h, content=slow())).status_code == 408
        db = await database.get_db()
        assert not await db.execute_fetchall("SELECT * FROM private_members")
    asyncio.run(run())


def test_queued_admission_rechecks_boundary_before_its_only_write(monkeypatch):
    import private_indexer

    async def run():
        await private_indexer._WRITE_LOCK.acquire()
        transport = httpx.ASGITransport(app=_with_transport_peer(app_module.app, "127.0.0.1"))
        async with httpx.AsyncClient(transport=transport, base_url=ORIGIN) as client:
            pending = asyncio.create_task(client.put(issuer.PATH, headers=owner_headers(), json={"expectedAccountId": OWNER}))
            await asyncio.sleep(.02)
            monkeypatch.setattr(config.settings, "site_registration_issuer_admission_enabled", False)
            private_indexer._WRITE_LOCK.release()
            assert (await pending).status_code == 404
        db = await database.get_db()
        assert not await db.execute_fetchall("SELECT * FROM private_members")
    asyncio.run(run())


def test_queued_canonical_admission_rechecks_current_browser_origin_before_write(monkeypatch):
    import private_indexer
    monkeypatch.setattr(config.settings, "operator_ui_url", "https://library.example.org/admin")
    monkeypatch.setattr(config.settings, "operator_ingress_host", "console.example.org")

    async def run():
        await private_indexer._WRITE_LOCK.acquire()
        transport = httpx.ASGITransport(app=_with_transport_peer(app_module.app, "127.0.0.1"))
        async with httpx.AsyncClient(transport=transport, base_url=ORIGIN) as client:
            h = {**owner_headers(), "Origin": "https://library.example.org"}
            pending = asyncio.create_task(client.put(issuer.PATH, headers=h, json={"expectedAccountId": OWNER}))
            await asyncio.sleep(.02)
            # The transport Host remains valid, but the original browser origin
            # no longer names the configured console when the writer resumes.
            monkeypatch.setattr(config.settings, "operator_ui_url", ORIGIN)
            private_indexer._WRITE_LOCK.release()
            assert (await pending).status_code == 403
        db = await database.get_db()
        assert not await db.execute_fetchall("SELECT * FROM private_members")
    asyncio.run(run())


def test_concurrent_explicit_requests_insert_one_self_row_and_remain_idempotent():
    async def run():
        transport = httpx.ASGITransport(app=_with_transport_peer(app_module.app, "127.0.0.1"))
        async with httpx.AsyncClient(transport=transport, base_url=ORIGIN) as client:
            responses = await asyncio.gather(*[client.put(issuer.PATH, headers=owner_headers(), json={"expectedAccountId": OWNER}) for _ in range(8)])
            assert all(response.status_code == 200 for response in responses)
            db = await database.get_db()
            rows = await db.execute_fetchall("SELECT user_id,active,actor FROM private_members")
            assert [tuple(row) for row in rows] == [(OWNER, 1, OWNER)]
    asyncio.run(run())


def test_issuer_admission_never_opens_private_routes(client):
    assert admit(client).status_code == 200
    for path in (
        "/api/library/private", "/library/private", "/private-admin", "/api/account/private",
        "/api/account/private/metrics", "/api/private/catalog", "/api/private/catalog/page",
        "/api/private/members", "/api/private/metrics", "/private/announce/synthetic",
        "/api/library/private/synthetic/magnet", "/api/library/private/synthetic/torrent",
    ):
        for host in ("library.example.org", "console.example.org"):
            h = owner_headers()
            h["Host"] = host
            assert client.get(path, headers=h).status_code == 404

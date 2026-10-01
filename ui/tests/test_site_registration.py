"""Site registration uses real SQLite without admitting private traffic."""
import asyncio

import pytest

import config
import database
import invitation_bridge as bridge
import invitations
from test_invitations import headers, member, mint, preview, redeem
from test_invitation_bridge import (
    bridge_config as _bridge_config, invitation_config as _invitation_config,
    redemption, rpc, start_body,
)

invitation_config = _invitation_config
bridge_config = _bridge_config


@pytest.fixture(autouse=True)
def registration_config(bridge_config, monkeypatch):
    for field, value in {
        "site_registration_enabled": True, "private_indexer_enabled": False,
        "private_indexer_secret": "", "private_seeder_url": "",
        "private_invitation_checkout_enabled": False,
    }.items():
        monkeypatch.setattr(config.settings, field, value)
    invitations.validate_settings()
    bridge.validate_settings()


def test_registration_defaults_off_and_cannot_be_enabled_by_runtime_overrides(client, monkeypatch):
    assert config.Settings.model_fields["site_registration_enabled"].default is False
    assert "site_registration_enabled" in config.FROZEN_OVERRIDE_FIELDS
    monkeypatch.setattr(config.settings, "site_registration_enabled", False)
    assert client.get("/invite", headers={"Host": "library.example.org"}).status_code == 404
    assert preview(client, "bi_" + "a" * 43).status_code == 404
    assert mint(client).status_code == 404


@pytest.mark.parametrize("field,value", [
    ("private_invitations_enabled", False), ("private_invitation_bridge_enabled", False),
    ("require_auth", False), ("trust_npm_headers", False),
    ("private_invitation_checkout_enabled", True),
])
def test_registration_requires_existing_auth_bridge_and_no_checkout(field, value, monkeypatch):
    monkeypatch.setattr(config.settings, field, value)
    monkeypatch.setattr(config.settings, "trust_forwarded_user", False)
    with pytest.raises(RuntimeError):
        invitations.validate_settings()


@pytest.mark.parametrize("origin", [
    "http://library.example.org", "https://library.example.org/",
    "https://library.example.org/path", "https://library.example.org?",
    "https://library.example.org?code=synthetic", "https://library.example.org#",
    "https://library.example.org#synthetic", "https://library.example.org:443",
    "https://user@library.example.org", "https://elsewhere.example.org",
    "https://LIBRARY.example.org", "https://library.example.org.",
    "https://library.example.org\\other", "https://library.example.org\n",
    "https://[malformed", "https://library..example.org",
])
def test_registration_rejects_noncanonical_public_origins(origin, monkeypatch):
    monkeypatch.setattr(config.settings, "private_indexer_url", origin)
    with pytest.raises(RuntimeError):
        invitations.validate_settings()


def test_anonymous_manual_entry_is_exact_host_only_and_never_places_codes_in_html(client, monkeypatch):
    monkeypatch.setattr(config.settings, "public_library_hosts", "library.example.org,alias.example.org")
    response = client.get("/invite", headers={"Host": "library.example.org"})
    assert response.status_code == 200
    assert 'id="invCode"' in response.text and 'maxlength="46"' in response.text
    assert 'name="token"' not in response.text and "bi_" not in response.text
    assert response.headers["Referrer-Policy"] == "no-referrer"
    assert response.headers["Cache-Control"] == "no-store"
    assert "https://sso.example.org" in response.headers["Content-Security-Policy"]
    for host in ("alias.example.org", "library.example.org:443", "library.example.org.", "LIBRARY.example.org", "console.example.org"):
        assert client.get("/invite", headers={"Host": host}).status_code == 404
        assert client.post("/api/invitations/preview", headers={"Host": host}, json={"token": "synthetic"}).status_code == 404
    assert client.get("/invite?code=synthetic", headers={"Host": "library.example.org"}).status_code == 404
    assert preview(client, "synthetic-invalid-code").json() == {"available": False}


def test_signed_local_registration_preserves_public_accounts_and_private_routes_closed(client):
    # Existing public key/account ledgers are exact-owner data, not mappings.
    assert client.post("/api/account/api-key", headers=headers("existing"), json={"name": "synthetic"}).status_code == 200
    tables = ("user_api_keys", "account_usage", "account_usage_events", "account_usage_daily", "account_preferences")

    async def snapshot():
        db = await database.get_db()
        return {table: [tuple(row) for row in await db.execute_fetchall(f"SELECT * FROM {table} ORDER BY rowid")] for table in tables}

    before = asyncio.run(snapshot())
    token = mint(client).json()["token"]
    assert preview(client, token).json()["available"] is True
    begin = start_body(token)
    started = rpc(client, begin)
    assert started.status_code == 200
    bound = redemption(begin, started.json()["enrollmentId"], provider="bitagent-local")
    accepted = rpc(client, bound)
    assert accepted.status_code == 200 and accepted.json()["accountId"] == "-12345"
    assert rpc(client, {**bound, "operation": "status"}).json()["approved"] is True
    assert redeem(client, token).status_code == 403  # Profile bridge cannot be bypassed.
    assert asyncio.run(snapshot()) == before

    for path in (
        "/api/library/private", "/library/private", "/private-admin", "/api/account/private",
        "/api/account/private/metrics", "/api/private/catalog", "/api/private/catalog/page",
        "/api/private/members", "/api/private/metrics", "/private/announce/synthetic",
        "/api/library/private/synthetic/magnet", "/api/library/private/synthetic/torrent",
    ):
        for host in ("library.example.org", "console.example.org"):
            assert client.get(path, headers=headers("owner", role="OWNER", Host=host)).status_code == 404
    assert client.post("/api/account/invitations/checkout", headers=headers(), json={}).status_code == 404
    assert client.post(bridge.PATH, headers=headers(), json=begin).status_code == 404
    assert client.post(bridge.PATH, headers={"Host": "console.example.org"}, json=begin).status_code == 401


def test_registration_never_invents_issuer_membership_or_revives_suspension(client):
    assert mint(client, "unapproved").status_code == 403
    token = mint(client).json()["token"]
    begin = start_body(token)
    started = rpc(client, begin)
    asyncio.run(member("alice", False))
    assert preview(client, token).json() == {"available": False}
    assert rpc(client, redemption(begin, started.json()["enrollmentId"], provider="bitagent-local")).status_code == 409

    async def absent():
        db = await database.get_db()
        assert not await db.execute_fetchall("SELECT * FROM private_members WHERE user_id='-12345'")

    asyncio.run(absent())

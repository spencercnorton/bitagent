"""Persistent preferences are strict, sparse and scoped to the current account."""
from __future__ import annotations

import asyncio

import pytest

import account_preferences
import account_usage
import config
import database

_PROOF = "preferences-test-proxy-proof-at-least-32-characters"
_PUBLIC_HOST = {"Host": "library.example.org"}


def _identity(user="alice"):
    return {**_PUBLIC_HOST, "X-Auth-User": user, "X-BitAgent-Proxy-Proof": _PROOF, "Sec-Fetch-Site": "same-origin"}


@pytest.fixture(autouse=True)
def _preferences_accounts():
    config.settings.require_auth = True
    config.settings.dashboard_api_key = ""
    config.settings.trust_npm_headers = True
    config.settings.trust_forwarded_user = False
    config.settings.proxy_auth_secret = _PROOF

    async def clear():
        db = await database.get_db()
        for table in ("account_preferences", "account_usage", "account_usage_events", "account_usage_daily"):
            await db.execute(f"DELETE FROM {table}")
        await db.commit()
    asyncio.run(clear())
    yield
    asyncio.run(database.close_all())


def _patch(client, changes, user="alice", **extra):
    return client.patch("/api/account/preferences", headers=_identity(user), json={"expectedAccountId": user, "changes": changes, **extra})


def test_defaults_and_persistence_are_isolated_per_account(client):
    initial = client.get("/api/account", headers=_identity()).json()["preferences"]
    assert initial == {"schemaVersion": 1, "accountId": "alice", "available": True, "revision": 0, "updatedAt": None, "settings": account_preferences.DEFAULT_SETTINGS}
    changed = _patch(client, {"region": "GB", "theme": "dark", "matchedOnly": False}).json()
    assert changed["revision"] == 1 and changed["updatedAt"].endswith("Z")
    assert changed["settings"]["region"] == "GB" and changed["settings"]["theme"] == "dark"
    assert changed["settings"]["matchedOnly"] is False
    assert client.get("/api/account", headers=_identity("bob")).json()["preferences"]["settings"] == account_preferences.DEFAULT_SETTINGS
    assert client.get("/api/account", headers=_identity()).json()["preferences"] == changed


def test_noop_does_not_create_revision_or_change_update_time(client):
    initial = client.get("/api/account", headers=_identity()).json()["preferences"]
    assert _patch(client, {}).json() == initial
    assert _patch(client, {"region": "US", "theme": "system"}).json() == initial
    changed = _patch(client, {"density": "compact"}).json()
    assert _patch(client, {"density": "compact"}).json() == changed
    reset = _patch(client, account_preferences.DEFAULT_SETTINGS).json()
    assert reset["revision"] == 2 and reset["settings"] == account_preferences.DEFAULT_SETTINGS


@pytest.mark.parametrize("changes", [
    {"unexpected": "value"}, {"apiKey": "synthetic-secret"}, {"theme": "blue"},
    {"motion": "full"}, {"density": "dense"}, {"region": "us"}, {"region": "USA"},
    {"region": 12}, {"region": "A\n"}, {"contentType": "music"}, {"sort": "downloads"},
    {"quality": "1080p"}, {"matchedOnly": 1}, {"englishOnly": "false"},
    {"magnetMode": "fast"}, {"theme": None}, {"matchedOnly": None},
])
def test_preferences_reject_unknown_invalid_and_coerced_values(client, changes):
    assert _patch(client, changes).status_code == 422
    assert client.get("/api/account", headers=_identity()).json()["preferences"]["revision"] == 0


def test_preference_auth_owner_and_csrf_checks(client):
    body = {"expectedAccountId": "alice", "changes": {"theme": "dark"}}
    assert client.patch("/api/account/preferences", headers=_PUBLIC_HOST, json=body).status_code == 401
    assert client.patch("/api/account/preferences", headers=_identity("bob"), json=body).status_code == 409
    assert client.patch("/api/account/preferences", headers={**_identity(), "Sec-Fetch-Site": "cross-site"}, json=body).status_code == 403
    assert client.patch("/api/account/preferences", headers=_identity(), json={"changes": {}}).status_code == 422
    assert _patch(client, {}, unexpected=True).status_code == 422
    assert _patch(client, {}, expectedAccountId=True).status_code == 422


def test_anonymous_preferences_are_browser_defaults_and_writes_rejected(client):
    config.settings.require_auth = False
    prefs = client.get("/api/account", headers=_PUBLIC_HOST).json()["preferences"]
    assert prefs["available"] is False and prefs["settings"] == account_preferences.DEFAULT_SETTINGS
    assert prefs["revision"] == 0 and prefs["updatedAt"] is None
    assert client.patch("/api/account/preferences", headers=_PUBLIC_HOST, json={"expectedAccountId": "anonymous", "changes": {"theme": "dark"}}).status_code == 403
    async def check():
        assert not await (await database.get_db()).execute_fetchall("SELECT * FROM account_preferences WHERE user_id='anonymous'")
    asyncio.run(check())


def test_concurrent_sparse_updates_preserve_other_tabs_fields_and_noops():
    async def run():
        results = await asyncio.gather(*(
            account_preferences.patch_account_preferences("alice", change)
            for change in ({"theme": "dark"}, {"region": "GB"}, {"motion": "reduced"})
        ))
        assert sorted(result["revision"] for result in results) == [1, 2, 3]
        final = await account_preferences.get_account_preferences("alice")
        assert final["revision"] == 3
        assert (final["settings"]["theme"], final["settings"]["region"], final["settings"]["motion"]) == ("dark", "GB", "reduced")
        same = await asyncio.gather(*(account_preferences.patch_account_preferences("alice", {"region": "GB"}) for _ in range(8)))
        assert all(result == final for result in same)
    asyncio.run(run())


def test_preferences_survive_restart_and_reset_never_changes_usage():
    async def run():
        before = await account_usage.record_magnet_grab("alice", 4, "copy")
        saved = await account_preferences.patch_account_preferences("alice", {"quality": "V2160p", "magnetMode": "all", "englishOnly": True})
        await database.close_all()
        assert await account_preferences.get_account_preferences("alice") == saved
        await account_preferences.patch_account_preferences("alice", account_preferences.DEFAULT_SETTINGS)
        usage = await account_usage.get_account_usage("alice")
        assert usage["grabs"] == before["grabs"] and usage["revision"] == before["revision"]
    asyncio.run(run())

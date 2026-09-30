"""Account aggregates are durable, private and separate from torrent transfers."""
from __future__ import annotations

import asyncio

import httpx
import pytest

import account_usage
import config
import database
import torznab

_PROOF = "usage-test-proxy-proof-at-least-32-characters"
_PUBLIC_HOST = {"Host": "library.example.org"}


def _identity(user: str) -> dict:
    return {
        **_PUBLIC_HOST,
        "X-Auth-User": user,
        "X-BitAgent-Proxy-Proof": _PROOF,
        "Sec-Fetch-Site": "same-origin",
    }


@pytest.fixture(autouse=True)
def _usage_accounts():
    config.settings.require_auth = True
    config.settings.dashboard_api_key = ""
    config.settings.trust_npm_headers = True
    config.settings.trust_forwarded_user = False
    config.settings.proxy_auth_secret = _PROOF

    async def clear():
        db = await database.get_db()
        await db.execute("DELETE FROM account_usage")
        await db.execute("DELETE FROM user_api_keys")
        await db.commit()

    asyncio.run(clear())
    torznab._TZ_BUCKETS.clear()
    yield
    torznab._TZ_BUCKETS.clear()


def test_account_usage_needs_auth_and_isolates_accounts(client):
    assert client.get("/api/account", headers=_PUBLIC_HOST).status_code == 401
    assert client.post(
        "/api/account/usage/grab", headers=_PUBLIC_HOST,
        json={"count": 2, "action": "copy"},
    ).status_code == 401

    before = client.get("/api/account", headers=_identity("alice")).json()["usage"]
    assert before["grabs"] == before["apiSearches"] == 0
    assert before["trackingSince"] > 0
    response = client.post(
        "/api/account/usage/grab", headers=_identity("alice"),
        json={"count": 2, "action": "copy", "userId": "bob"},
    )
    assert response.status_code == 200
    alice = client.get("/api/account", headers=_identity("alice")).json()["usage"]
    bob = client.get("/api/account", headers=_identity("bob")).json()["usage"]
    assert alice["grabs"] == alice["magnetCopies"] == 2
    assert alice["trackingSince"] == before["trackingSince"]
    assert bob["grabs"] == 0
    for key in ("downloadedBytes", "uploadedBytes", "hitAndRuns"):
        assert alice[key] is None
    assert alice["grabsSource"] == "magnet-link-actions"
    assert alice["apiSearchesSource"] == "successful-torznab-searches"


@pytest.mark.parametrize("count", [True, False, 0, -1, 1001, 1.5, "2", None])
def test_grab_endpoint_requires_bounded_strict_integer(client, count):
    response = client.post(
        "/api/account/usage/grab", headers=_identity("alice"),
        json={"count": count, "action": "copy"},
    )
    assert response.status_code == 422
    assert client.get("/api/account", headers=_identity("alice")).json()["usage"]["grabs"] == 0


def test_grab_endpoint_rejects_unknown_action_and_cross_site(client):
    assert client.post(
        "/api/account/usage/grab", headers=_identity("alice"),
        json={"count": 1, "action": "download"},
    ).status_code == 422
    assert client.post(
        "/api/account/usage/grab",
        headers={**_identity("alice"), "Sec-Fetch-Site": "cross-site"},
        json={"count": 1, "action": "open"},
    ).status_code == 403


def test_aggregate_updates_are_atomic_survive_rotation_and_keep_no_history():
    async def run():
        first = await account_usage.get_account_usage("alice")
        await asyncio.gather(*(
            account_usage.record_magnet_grab("alice", 1, "open") for _ in range(20)
        ))
        await account_usage.record_magnet_grab("alice", 1000, "export")
        await database.create_user_api_key("alice", database._hash_user_api_key("first"), "prefix")
        await database.create_user_api_key("alice", database._hash_user_api_key("second"), "prefix")
        await database.revoke_user_api_key("alice")
        usage = await account_usage.get_account_usage("alice")
        assert usage["grabs"] == 1020
        assert usage["magnetOpens"] == 20
        assert usage["magnetExports"] == 1000
        assert usage["trackingSince"] == first["trackingSince"]
        db = await database.get_db()
        columns = await db.execute_fetchall("PRAGMA table_info(account_usage)")
        assert {row["name"] for row in columns} == {
            "user_id", "tracking_since", "grabs", "api_searches",
            "magnet_copies", "magnet_opens", "magnet_exports",
        }
        await database.close_all()
        assert await account_usage.get_account_usage("alice") == usage

    asyncio.run(run())


def _user_key(user: str, secret: str):
    asyncio.run(database.create_user_api_key(user, database._hash_user_api_key(secret), "ba_test..."))


def _upstream(monkeypatch, status: int, body: bytes):
    calls = []

    class FakeClient:
        async def __aenter__(self):
            return self

        async def __aexit__(self, *args):
            return False

        async def request(self, method, url, **kwargs):
            calls.append(kwargs)
            return httpx.Response(status, content=body, headers={"content-type": "application/xml"})

    monkeypatch.setattr(torznab.httpx, "AsyncClient", lambda *args, **kwargs: FakeClient())
    return calls


@pytest.mark.parametrize("function", ["search", "movie", "tvsearch", "music", "book"])
def test_successful_torznab_search_is_attributed_to_key_owner(client, monkeypatch, function):
    key = "ba_test_alice_not_a_real_key"
    core_key = "unit-test-core-key"
    _user_key("alice", key)
    config.settings.torznab_api_key = core_key
    calls = _upstream(monkeypatch, 200, b'<?xml version="1.0"?><rss><channel/></rss>')
    response = client.get(
        "/torznab/api", params={"t": function, "apikey": key}, headers=_PUBLIC_HOST,
    )
    assert response.status_code == 200
    assert ("apikey", core_key) in calls[0]["params"]
    alice = client.get("/api/account", headers=_identity("alice")).json()
    bob = client.get("/api/account", headers=_identity("bob")).json()
    assert alice["usage"]["apiSearches"] == 1
    assert alice["usage"]["grabs"] == bob["usage"]["apiSearches"] == 0
    assert key not in str(alice)
    assert core_key not in str(alice)


@pytest.mark.parametrize("method,function,status,body", [
    ("HEAD", "search", 200, b"<rss><channel/></rss>"),
    ("GET", "caps", 200, b"<caps/>"),
    ("GET", "get", 200, b"<rss><channel/></rss>"),
    ("GET", "search", 500, b"<rss><channel/></rss>"),
    ("GET", "search", 200, b'<error code="202" description="Unknown function"/>'),
    ("GET", "search", 200, b"<rss><channel><error/></channel></rss>"),
    ("GET", "search", 200, b"<rss><channel/>"),
    ("GET", "search", 200, b'<!DOCTYPE rss [<!ENTITY x "x">]><rss><channel/></rss>'),
    ("GET", "search", 200, '<!DOCTYPE rss [<!ENTITY x "x">]><rss><channel/></rss>'.encode("utf-16")),
])
def test_torznab_non_search_or_failure_does_not_increment(client, monkeypatch, method, function, status, body):
    _user_key("alice", "ba_test_alice_not_a_real_key")
    _upstream(monkeypatch, status, body)
    client.request(
        method, "/torznab/api", params={"t": function, "apikey": "ba_test_alice_not_a_real_key"},
        headers=_PUBLIC_HOST,
    )
    usage = client.get("/api/account", headers=_identity("alice")).json()["usage"]
    assert usage["apiSearches"] == usage["grabs"] == 0


def test_revoked_torznab_key_does_not_increment(client, monkeypatch):
    _user_key("alice", "ba_test_alice_not_a_real_key")
    asyncio.run(database.revoke_user_api_key("alice"))
    calls = _upstream(monkeypatch, 200, b"<rss><channel/></rss>")
    response = client.get(
        "/torznab/api", params={"t": "search", "apikey": "ba_test_alice_not_a_real_key"},
        headers=_PUBLIC_HOST,
    )
    assert response.status_code == 401
    assert calls == []
    assert client.get("/api/account", headers=_identity("alice")).json()["usage"]["apiSearches"] == 0


def test_upstream_exception_does_not_log_api_keys(client, monkeypatch, caplog):
    _user_key("alice", "ba_test_alice_not_a_real_key")

    class FailingClient:
        async def __aenter__(self):
            return self

        async def __aexit__(self, *args):
            return False

        async def request(self, *args, **kwargs):
            raise httpx.ConnectError("http://core/torznab/api?apikey=do-not-log-core-key")

    monkeypatch.setattr(torznab.httpx, "AsyncClient", lambda *args, **kwargs: FailingClient())
    response = client.get(
        "/torznab/api", params={"t": "search", "apikey": "ba_test_alice_not_a_real_key"},
        headers=_PUBLIC_HOST,
    )
    assert response.status_code == 502
    assert "do-not-log-core-key" not in caplog.text
    assert "ba_test_alice_not_a_real_key" not in caplog.text
    assert "ConnectError" in caplog.text
    assert client.get("/api/account", headers=_identity("alice")).json()["usage"]["apiSearches"] == 0

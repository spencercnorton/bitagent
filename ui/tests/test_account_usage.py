"""Account aggregates are durable, private and separate from torrent transfers."""
from __future__ import annotations

import asyncio
from datetime import datetime, timezone
from uuid import uuid4

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
        for table in ("account_usage", "account_usage_events", "account_usage_daily", "account_preferences"):
            await db.execute(f"DELETE FROM {table}")
        await db.execute("DELETE FROM user_api_keys")
        await db.commit()

    asyncio.run(clear())
    torznab._TZ_BUCKETS.clear()
    yield
    torznab._TZ_BUCKETS.clear()
    asyncio.run(database.close_all())


def test_account_usage_needs_auth_and_isolates_accounts(client):
    assert client.get("/api/account", headers=_PUBLIC_HOST).status_code == 401
    assert client.post(
        "/api/account/usage/grab", headers=_PUBLIC_HOST,
        json={"count": 2, "action": "copy"},
    ).status_code == 401

    before = client.get("/api/account", headers=_identity("alice")).json()["usage"]
    assert before["grabs"] == before["apiSearches"] == 0
    assert before["trackingSince"] > 0
    assert before["available"] and before["accountId"] == "alice"
    assert before["revision"] == 0 and before["observedAt"].endswith("Z")
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
            "magnet_copies", "magnet_opens", "magnet_exports", "revision", "period_tracking_since",
        }
        await database.close_all()
        restarted = await account_usage.get_account_usage("alice")
        for key in ("grabs", "magnetOpens", "magnetExports", "trackingSince", "revision", "periodTrackingSince"):
            assert restarted[key] == usage[key]
        assert restarted["periods"]["last30Days"]["grabs"] == 1020

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


def test_receipt_retry_owner_scope_and_mismatched_reuse(client):
    event = str(uuid4())
    payload = {"count": 3, "action": "copy", "eventId": event, "expectedAccountId": "alice"}
    first = client.post("/api/account/usage/grab", headers=_identity("alice"), json=payload).json()
    assert first["receipt"] == {"eventId": event, "duplicate": False, "recorded": True}
    # Simulates a lost response: retry the exact event after it committed.
    retry = client.post("/api/account/usage/grab", headers=_identity("alice"), json=payload).json()
    assert retry["grabs"] == retry["magnetCopies"] == 3
    assert retry["revision"] == first["revision"] == 1
    assert retry["receipt"]["duplicate"] is True
    assert retry["periods"]["last7Days"]["grabs"] == 3
    for change in ({"count": 4}, {"action": "open"}):
        assert client.post("/api/account/usage/grab", headers=_identity("alice"), json={**payload, **change}).status_code == 409
    assert client.post("/api/account/usage/grab", headers=_identity("bob"), json=payload).status_code == 409
    bob = client.post("/api/account/usage/grab", headers=_identity("bob"), json={**payload, "expectedAccountId": "bob"}).json()
    assert bob["accountId"] == "bob" and bob["grabs"] == 3
    assert bob["receipt"]["duplicate"] is False


@pytest.mark.parametrize("change", [{"eventId": "not-a-uuid"}, {"eventId": 123}, {"expectedAccountId": True}])
def test_receipt_metadata_is_strict(client, change):
    assert client.post("/api/account/usage/grab", headers=_identity("alice"), json={"count": 1, "action": "copy", **change}).status_code == 422
    assert client.get("/api/account", headers=_identity("alice")).json()["usage"]["grabs"] == 0


def test_concurrent_duplicate_events_increment_once():
    async def run():
        event = uuid4()
        results = await asyncio.gather(*(account_usage.record_magnet_grab("alice", 2, "export", event) for _ in range(12)))
        assert sum(not result["receipt"]["duplicate"] for result in results) == 1
        usage = await account_usage.get_account_usage("alice")
        assert usage["grabs"] == usage["magnetExports"] == 2
        assert usage["revision"] == 1
        assert usage["periods"]["last30Days"]["magnetExports"] == 2
    asyncio.run(run())


def test_failed_receipt_transaction_can_retry_without_partial_counts(monkeypatch):
    async def fail(*args):
        raise RuntimeError("simulated aggregate failure")

    async def run():
        event = uuid4()
        with monkeypatch.context() as m:
            m.setattr(account_usage, "_daily_increment", fail)
            with pytest.raises(RuntimeError):
                await account_usage.record_magnet_grab("alice", 5, "open", event)
        usage = await account_usage.get_account_usage("alice")
        assert usage["grabs"] == usage["revision"] == 0
        retried = await account_usage.record_magnet_grab("alice", 5, "open", event)
        assert not retried["receipt"]["duplicate"] and retried["grabs"] == 5
    asyncio.run(run())


def test_anonymous_activity_is_unavailable_and_never_persisted(client):
    config.settings.require_auth = False
    payload = client.get("/api/account", headers=_PUBLIC_HOST).json()
    assert payload["usage"]["available"] is False
    assert payload["usage"]["status"] == "anonymous"
    assert payload["usage"]["grabs"] is payload["usage"]["apiSearches"] is None
    assert payload["usage"]["trackingSince"] is payload["usage"]["periods"] is None
    assert payload["preferences"]["available"] is False
    assert client.post("/api/account/usage/grab", headers=_PUBLIC_HOST, json={"count": 1, "action": "copy"}).status_code == 403
    assert client.post("/api/account/api-key", headers=_PUBLIC_HOST).status_code == 403
    assert client.delete("/api/account/api-key", headers=_PUBLIC_HOST).status_code == 403

    async def check():
        db = await database.get_db()
        assert not await db.execute_fetchall("SELECT * FROM account_usage WHERE user_id='anonymous'")
        with pytest.raises(ValueError):
            await account_usage.record_api_search("anonymous")
    asyncio.run(check())


def test_shared_operator_credential_has_no_personal_storage_or_keys(client):
    config.settings.dashboard_api_key = "synthetic-shared-operator-credential"
    headers = {**_PUBLIC_HOST, "Authorization": "Bearer synthetic-shared-operator-credential"}
    payload = client.get("/api/account", headers=headers).json()
    assert payload["identity"]["id"] == "api-client"
    assert payload["usage"]["available"] is payload["preferences"]["available"] is False
    assert payload["usage"]["status"] == "shared-credential" and payload["usage"]["grabs"] is None
    assert payload["apiKey"] is None
    assert client.post("/api/account/usage/grab", headers=headers, json={"count": 1, "action": "copy"}).status_code == 403
    assert client.patch("/api/account/preferences", headers=headers, json={"expectedAccountId": "api-client", "changes": {"theme": "dark"}}).status_code == 403
    assert client.post("/api/account/api-key", headers=headers).status_code == 403
    assert client.delete("/api/account/api-key", headers=headers).status_code == 403
    async def check():
        db = await database.get_db()
        for table in ("account_usage", "account_preferences", "user_api_keys"):
            assert not await db.execute_fetchall(f"SELECT * FROM {table} WHERE user_id='api-client'")
    asyncio.run(check())


def test_utc_period_boundaries_and_bounded_daily_history(monkeypatch):
    def timestamp(text):
        return datetime.fromisoformat(text.replace("Z", "+00:00")).timestamp()
    clock = [timestamp("2026-08-31T12:00:00Z")]
    monkeypatch.setattr(account_usage, "_now", lambda: clock[0])

    async def run():
        await account_usage.record_magnet_grab("alice", 1, "copy")
        for instant, count, action in (
            ("2026-09-01T00:00:00Z", 2, "export"),
            ("2026-09-23T23:59:59Z", 3, "open"),
            ("2026-09-24T00:00:00Z", 4, "copy"),
            ("2026-09-30T23:59:59Z", 5, "copy"),
        ):
            clock[0] = timestamp(instant)
            await account_usage.record_magnet_grab("alice", count, action)
        await account_usage.record_api_search("alice")
        usage = await account_usage.get_account_usage("alice")
        seven, thirty = usage["periods"]["last7Days"], usage["periods"]["last30Days"]
        assert usage["grabs"] == 15 and usage["revision"] == 6
        assert seven["windowStart"] == "2026-09-24T00:00:00Z" and seven["grabs"] == 9
        assert thirty["windowStart"] == "2026-09-01T00:00:00Z" and thirty["grabs"] == 14
        assert seven["complete"] and thirty["complete"]
        assert seven["apiSearches"] == thirty["apiSearches"] == 1
        assert thirty["grabs"] == sum(thirty[key] for key in ("magnetCopies", "magnetOpens", "magnetExports"))
        clock[0] = timestamp("2026-10-01T00:00:00Z")
        next_day = await account_usage.get_account_usage("alice")
        assert next_day["revision"] == usage["revision"]
        assert next_day["periods"]["last7Days"]["grabs"] == 5
        assert next_day["periods"]["last30Days"]["grabs"] == 12
        rows = await (await database.get_db()).execute_fetchall("SELECT day FROM account_usage_daily WHERE user_id='alice'")
        assert len(rows) <= 30 and all(row["day"] >= "2026-09-02" for row in rows)
    asyncio.run(run())


def test_legacy_schema_migration_preserves_totals_without_inventing_history(tmp_path, monkeypatch):
    import aiosqlite
    clock = datetime(2026, 9, 30, 12, tzinfo=timezone.utc).timestamp()
    monkeypatch.setattr(account_usage, "_now", lambda: clock)
    asyncio.run(database.close_all())
    monkeypatch.setattr(config.settings, "db_path", str(tmp_path / "legacy-account.db"))

    async def run():
        try:
            async with aiosqlite.connect(config.settings.db_path) as db:
                await db.execute("CREATE TABLE account_usage (user_id TEXT PRIMARY KEY,tracking_since REAL NOT NULL,grabs INTEGER NOT NULL DEFAULT 0,api_searches INTEGER NOT NULL DEFAULT 0,magnet_copies INTEGER NOT NULL DEFAULT 0,magnet_opens INTEGER NOT NULL DEFAULT 0,magnet_exports INTEGER NOT NULL DEFAULT 0)")
                await db.execute("INSERT INTO account_usage VALUES ('alice',1,100,7,60,40,0)")
                await db.commit()
            usage = await account_usage.get_account_usage("alice")
            assert usage["grabs"] == 100 and usage["apiSearches"] == 7 and usage["trackingSince"] == 1
            assert usage["periodTrackingSince"] == clock
            for period in usage["periods"].values():
                assert period["grabs"] == period["apiSearches"] == 0 and not period["complete"]
            await account_usage.record_magnet_grab("alice", 2, "copy", uuid4())
            fresh = await account_usage.get_account_usage("alice")
            assert fresh["grabs"] == 102 and fresh["periods"]["last7Days"]["grabs"] == 2
        finally:
            await database.close_all()
    asyncio.run(run())


def test_key_mutations_guard_owner_and_preserve_account_payload(client):
    asyncio.run(database.create_user_api_key("bob", database._hash_user_api_key("synthetic-bob-key"), "test..."))
    assert client.post("/api/account/api-key", headers=_identity("bob"), json={"name": "default", "expectedAccountId": "alice"}).status_code == 409
    assert client.delete("/api/account/api-key?expectedAccountId=alice", headers=_identity("bob")).status_code == 409
    assert asyncio.run(database.get_user_api_key("bob")) is not None
    client.post("/api/account/usage/grab", headers=_identity("alice"), json={"count": 2, "action": "open"})
    client.patch("/api/account/preferences", headers=_identity("alice"), json={"expectedAccountId": "alice", "changes": {"region": "GB"}})
    generated = client.post("/api/account/api-key", headers=_identity("alice"), json={"expectedAccountId": "alice"}).json()
    assert generated["usage"]["accountId"] == generated["preferences"]["accountId"] == "alice"
    assert generated["usage"]["grabs"] == 2 and generated["preferences"]["settings"]["region"] == "GB"
    revoked = client.delete("/api/account/api-key?expectedAccountId=alice", headers=_identity("alice")).json()
    assert revoked["apiKey"] is None and "apiKeySecret" not in revoked
    assert revoked["usage"]["grabs"] == 2 and revoked["preferences"] == generated["preferences"]

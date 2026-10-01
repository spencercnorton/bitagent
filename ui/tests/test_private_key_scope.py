"""Real SQLite/ASGI personal-key entitlement and revocation boundaries."""
from __future__ import annotations

import asyncio
from contextlib import asynccontextmanager
import sqlite3

import aiosqlite
import httpx
import pytest

import config
import database
import private_indexer as private
import torznab
from test_private_indexer import (
    announce, bind_peers, download, identity, peer_observations,
    private_config as private_config, setup_release,
)


async def mutate(key_id, column, value):
    assert column in {"private_access", "revoked_at", "user_id"}
    async with aiosqlite.connect(config.settings.db_path) as db:
        await db.execute(f"UPDATE user_api_keys SET {column}=? WHERE id=?", (value, key_id))
        await db.commit()


def key_row(secret):
    return asyncio.run(database.lookup_user_api_key(database._hash_user_api_key(secret)))


def test_legacy_schema_adds_deny_without_rewriting_rows(tmp_path):
    path = tmp_path / "legacy.db"
    conn = sqlite3.connect(path)
    conn.execute("""CREATE TABLE user_api_keys(id INTEGER PRIMARY KEY AUTOINCREMENT,user_id TEXT NOT NULL,
        name TEXT NOT NULL DEFAULT 'default',key_hash TEXT NOT NULL UNIQUE,key_prefix TEXT NOT NULL,
        created_at REAL NOT NULL,last_used_at REAL,revoked_at REAL)""")
    conn.executemany("INSERT INTO user_api_keys VALUES (?,?,?,?,?,?,?,?)", [
        (1, "legacy-a", "default", "invented-a", "fixture", 1, 2, None),
        (2, "legacy-a", "old", "invented-b", "fixture", 1, None, 3),
        (3, "legacy-b", "default", "invented-c", "fixture", 4, None, None),
    ])
    before = conn.execute("SELECT * FROM user_api_keys ORDER BY id").fetchall()
    conn.commit()
    conn.close()
    async def upgrade():
        async with aiosqlite.connect(path) as db:
            db.row_factory = aiosqlite.Row
            await database._init_tables(db)
            await database._init_tables(db)
            return await db.execute_fetchall("SELECT * FROM user_api_keys ORDER BY id")
    rows = asyncio.run(upgrade())
    assert [tuple(row)[:-1] for row in rows] == before
    assert [row["private_access"] for row in rows] == [0, 0, 0]


@pytest.mark.parametrize("function", ["caps", "search", "movie", "tvsearch", "get"])
def test_public_key_remap_does_not_grant_private_surfaces(client, monkeypatch, function):
    _, release_id = setup_release(client)
    secret = client.post("/api/account/api-key", headers=identity()).json()["apiKeySecret"]
    row = key_row(secret)
    assert row["private_access"] is False
    assert client.put("/api/private/members/canonical", headers=identity("owner", "OWNER"), json={"active": True}).status_code == 200
    asyncio.run(mutate(row["id"], "user_id", "canonical"))
    params = {"t": function, "apikey": secret, "id": release_id}
    assert client.get("/torznab/private/api", params=params).status_code == 403
    assert client.get(f"/private/torrents/{release_id}.torrent", params={"apikey": secret}).status_code == 403
    assert client.get(f"/api/library/private/{release_id}/magnet", headers=identity("canonical")).status_code == 403
    assert client.get(f"/api/library/private/{release_id}/torrent", headers=identity("canonical")).status_code == 403
    assert client.get(f"/api/private/catalog/{release_id}/seed-torrent", params={"seed_member_id": "canonical"}, headers=identity("owner", "OWNER")).status_code == 403
    class PublicClient:
        async def __aenter__(self):
            return self
        async def __aexit__(self, *args):
            return False
        async def request(self, *args, **kwargs):
            return httpx.Response(200, content=b"<caps/>")
    monkeypatch.setattr(torznab.httpx, "AsyncClient", lambda **kwargs: PublicClient())
    # Even a missing/suspended private membership cannot disable public DHT.
    async def suspend_without_revoking_public():
        async with private.private_write() as db:
            await db.execute("UPDATE private_members SET active=0 WHERE user_id='canonical'")
    asyncio.run(suspend_without_revoking_public())
    assert client.get("/torznab/api", params={"apikey": secret, "t": "caps"}).status_code == 200


@pytest.mark.parametrize("value", ["true", 1, None, {}, []])
def test_private_scope_is_strict_boolean(client, value):
    setup_release(client)
    assert client.post("/api/account/api-key", headers=identity(), json={"privateAccess": value, "expectedAccountId": "member"}).status_code == 422


def test_explicit_private_rotation_is_human_exact_member_and_revokes_previous(client):
    secret, release_id = setup_release(client)
    assert client.get("/api/account", headers=identity()).json()["apiKey"]["privateAccess"] is True
    assert client.post("/api/account/api-key", headers=identity(), json={"privateAccess": True}).status_code == 422
    assert client.post("/api/account/api-key", headers=identity(), json={"privateAccess": True, "expectedAccountId": "other"}).status_code == 409
    config.settings.dashboard_api_key = "synthetic-operator-secret"
    assert client.post("/api/account/api-key", headers={"x-api-key": "synthetic-operator-secret"}, json={"privateAccess": True, "expectedAccountId": "api-client"}).status_code == 403
    assert client.post("/api/account/api-key", headers={**identity(), "x-api-key": "synthetic-operator-secret"}, json={"privateAccess": True, "expectedAccountId": "member"}).status_code in {403, 409}
    assert client.post("/api/account/api-key", headers=identity("outsider"), json={"privateAccess": True, "expectedAccountId": "outsider"}).status_code == 403
    config.settings.dashboard_api_key = ""
    response = client.post("/api/account/api-key", headers=identity(), json={"privateAccess": True, "expectedAccountId": "member"})
    assert response.status_code == 200
    new_secret = response.json()["apiKeySecret"]
    assert response.json()["apiKey"]["privateAccess"] is True
    assert key_row(secret) is None
    bind_peers({"192.0.2.1": "member"})
    assert b"failure reason" not in announce(client, download(client, new_secret, release_id))
    public = client.post("/api/account/api-key", headers=identity(), json={"expectedAccountId": "member"}).json()
    assert public["apiKey"]["privateAccess"] is False
    assert client.get(f"/private/torrents/{release_id}.torrent", params={"apikey": public["apiKeySecret"]}).status_code == 403


@pytest.mark.parametrize("column,value", [("private_access", 0), ("revoked_at", 123), ("user_id", "other")])
def test_queued_tracker_mint_revalidates_scope_owner_and_revocation(client, monkeypatch, column, value):
    secret, release_id = setup_release(client)
    row = key_row(secret)
    original = private.private_write
    intercepted = False
    @asynccontextmanager
    async def queued(**kwargs):
        nonlocal intercepted
        if not intercepted:
            intercepted = True
            await mutate(row["id"], column, value)
        async with original(**kwargs) as db:
            yield db
    monkeypatch.setattr(private, "private_write", queued)
    assert client.get(f"/private/torrents/{release_id}.torrent", params={"apikey": secret}).status_code == 403
    async def read():
        return await (await database.get_db()).execute_fetchall("SELECT api_key_id FROM private_tracker_keys")
    assert asyncio.run(read()) == []


@pytest.mark.parametrize("column,value", [("private_access", 0), ("revoked_at", 123), ("user_id", "other")])
def test_queued_announce_denies_without_strict_binding_and_rolls_back(client, monkeypatch, column, value):
    secret, release_id = setup_release(client)
    path = download(client, secret, release_id)
    row = key_row(secret)
    before = peer_observations()
    config.settings.private_peer_bindings_required = False
    original = private.private_write
    intercepted = False
    @asynccontextmanager
    async def queued(**kwargs):
        nonlocal intercepted
        if not intercepted:
            intercepted = True
            await mutate(row["id"], column, value)
        async with original(**kwargs) as db:
            yield db
    monkeypatch.setattr(private, "private_write", queued)
    assert b"failure reason" in announce(client, path, downloaded=100)
    assert peer_observations() == before


@pytest.mark.parametrize("endpoint", ["torrent", "magnet"])
def test_final_download_check_denies_revocation_after_readiness_work(client, monkeypatch, endpoint):
    secret, release_id = setup_release(client)
    row = key_row(secret)
    original = private._require_release_proof
    async def late(release):
        await original(release)
        await mutate(row["id"], "private_access", 0)
    monkeypatch.setattr(private, "_require_release_proof", late)
    if endpoint == "torrent":
        response = client.get(f"/private/torrents/{release_id}.torrent", params={"apikey": secret})
    else:
        response = client.get(f"/api/library/private/{release_id}/magnet", headers=identity())
    assert response.status_code == 403


def test_old_tracker_literal_denied_and_mismatched_peer_owner_excluded(client):
    secret, release_id = setup_release(client)
    path = download(client, secret, release_id)
    assert b"failure reason" not in announce(client, path)
    row = key_row(secret)
    assert client.put("/api/private/members/other", headers=identity("owner", "OWNER"), json={"active": True}).status_code == 200
    async def mismatched_peer():
        async with private.private_write() as db:
            await db.execute("UPDATE private_peers SET user_id='other'")
    asyncio.run(mismatched_peer())
    result = announce(client, path, peer_id=b"-TEST02-abcdefghijkl")
    assert result[b"incomplete"] == 1
    assert result[b"peers"] == b""
    feed = client.get("/api/library/private", headers=identity()).json()
    assert feed["items"][0]["leechers"] == 1
    asyncio.run(mutate(row["id"], "private_access", 0))
    assert b"failure reason" in announce(client, path)
    assert client.get("/torznab/private/api", params={"t": "caps", "apikey": secret}).status_code == 403


def test_private_mint_membership_is_rechecked_inside_transaction(client, monkeypatch):
    setup_release(client)
    before = client.get("/api/account", headers=identity()).json()["apiKey"]["id"]
    original = private.private_write
    intercepted = False
    @asynccontextmanager
    async def queued(**kwargs):
        nonlocal intercepted
        if not intercepted:
            intercepted = True
            async with aiosqlite.connect(config.settings.db_path) as db:
                await db.execute("UPDATE private_members SET active=0 WHERE user_id='member'")
                await db.commit()
        async with original(**kwargs) as db:
            yield db
    monkeypatch.setattr(private, "private_write", queued)
    response = client.post("/api/account/api-key", headers=identity(), json={"privateAccess": True, "expectedAccountId": "member"})
    assert response.status_code == 403
    assert asyncio.run(database.get_user_api_key("member"))["id"] == before


@pytest.mark.parametrize("function,boundary", [("caps", "touch"), ("search", "usage")])
def test_private_metadata_rechecks_after_last_accounting_await(client, monkeypatch, function, boundary):
    secret, _ = setup_release(client)
    row = key_row(secret)
    if boundary == "touch":
        original = torznab.touch_user_api_key
        async def late(key_id):
            await original(key_id)
            await mutate(row["id"], "private_access", 0)
        monkeypatch.setattr(torznab, "touch_user_api_key", late)
    else:
        original = torznab.record_api_search
        async def late(user_id):
            await original(user_id)
            await mutate(row["id"], "revoked_at", 123)
        monkeypatch.setattr(torznab, "record_api_search", late)
    assert client.get("/torznab/private/api", params={"t": function, "apikey": secret}).status_code == 403


@pytest.mark.parametrize("column,value", [("private_access", 0), ("revoked_at", 123), ("user_id", "other")])
def test_final_announce_snapshot_rechecks_caller_after_commit(client, monkeypatch, column, value):
    secret, release_id = setup_release(client)
    path = download(client, secret, release_id)
    row = key_row(secret)
    original = private._final_announce_peers
    async def late(*args):
        await mutate(row["id"], column, value)
        return await original(*args)
    monkeypatch.setattr(private, "_final_announce_peers", late)
    assert b"failure reason" in announce(client, path)
    # A valid observation committed before this later revocation remains an
    # historical observation; no successful peer response is disclosed.
    assert len(peer_observations()[0]) == 1


def test_final_announce_filters_peer_permission_revoked_after_commit(client, monkeypatch):
    secret, release_id = setup_release(client)
    path = download(client, secret, release_id)
    assert client.put("/api/private/members/other", headers=identity("owner", "OWNER"), json={"active": True}).status_code == 200
    other_secret = client.post("/api/account/api-key", headers=identity("other"), json={"privateAccess": True, "expectedAccountId": "other"}).json()["apiKeySecret"]
    other_path = download(client, other_secret, release_id)
    assert b"failure reason" not in announce(client, other_path, peer_id=b"-OTHER1-abcdefghijkl")
    other_row = key_row(other_secret)
    original = private._final_announce_peers
    async def late(*args):
        await mutate(other_row["id"], "private_access", 0)
        return await original(*args)
    monkeypatch.setattr(private, "_final_announce_peers", late)
    result = announce(client, path)
    assert result[b"peers"] == b""
    assert result[b"incomplete"] == 1


def test_forged_dictionary_cannot_grant_capability(client):
    secret, _ = setup_release(client)
    row = key_row(secret)
    asyncio.run(mutate(row["id"], "private_access", 0))
    row["private_access"] = True
    from fastapi import HTTPException
    with pytest.raises(HTTPException) as failure:
        asyncio.run(private._tracker_key(row))
    assert failure.value.status_code == 403

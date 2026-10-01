"""Real catalog transactions, cursor snapshots and synthetic large inventories."""

import asyncio
import hashlib
import json
import sqlite3

import aiosqlite
import pytest
from fastapi import HTTPException
from starlette.datastructures import QueryParams

import config
import database
import private_indexer as private
import privatecatalog as catalog
from test_private_indexer import identity, metainfo, mark_ready, private_config as private_config


OWNER = identity("owner", "OWNER")


def imported(client, items, ack=True):
    return client.post("/api/private/catalog/import" + ("?ack=1" if ack else ""),
                       json={"version": 1, "releases": items}, headers=OWNER)


def page(client, **params):
    return client.get("/api/private/catalog/page", params=params, headers=OWNER)


def current_state():
    async def read():
        return await catalog.state(await database.get_db())
    return asyncio.run(read())


async def seed_aliases(count, swarms=3):
    """Few real metainfo rows, then cheap SQL aliases rather than mass hashing."""
    item = private._validate_release(metainfo())
    async with private.private_write() as db:
        for i in range(swarms):
            await db.execute("""INSERT INTO private_releases
                (id,source_id,title,kind,info_hash,size,metadata,metainfo,created_at)
                VALUES (?,?,?,?,?,?,?,?,?)""",
                (f"{i:032x}", f"asset:{i:08d}", "Synthetic title", "movie",
                 hashlib.sha1(f"swarm:{i}".encode()).hexdigest(), item["size"], item["metadata"], item["metainfo"], 1))
        for start in range(0, count, 1000):
            await db.executemany("INSERT INTO private_release_aliases VALUES (?,?,?,?,?)",
                                 [(f"asset:{i:08d}", f"{i % swarms:032x}", "Synthetic title", "movie", "{}")
                                  for i in range(start, min(start+1000, count))])
        await catalog.changed(db)


def test_legacy_contract_and_import_acknowledges_shared_swarm_once_per_source(client):
    first = metainfo()
    second = {**first, "source_id": "synthetic:episode:2", "title": "Synthetic episode", "kind": "episode", "episodes": [2]}
    before = current_state()
    result = imported(client, [first, second]).json()
    assert result["imported"] == 2 and len(result["results"]) == 2
    assert [row["source_id"] for row in result["results"]] == [first["source_id"], second["source_id"]]
    assert len({row["id"] for row in result["results"]}) == 1
    assert all(row["info_hash"] == first["info_hash"] and row["size"] == first["size"] for row in result["results"])
    assert result["catalog_snapshot"]["revision_token"] != before["revision_token"]
    state = current_state()
    legacy = client.get("/api/private/catalog", headers=OWNER).json()
    assert isinstance(legacy, list) and len(legacy) == 1
    assert set(legacy[0]) == {"id", "source_id", "title", "kind", "info_hash", "size", "ready", "verified_at", "withdrawn"}
    retry = imported(client, [first, second])
    assert retry.json() == result and retry.headers["cache-control"] == "no-store"
    assert imported(client, [first], ack=False).json() == {"imported": 1, "readiness": "Seeder verification required"}
    assert current_state() == state
    aliases = page(client).json()
    assert aliases["snapshot"] == {**state, "alias_total": 2, "release_total": 1}
    assert {row["title"] for row in aliases["items"]} == {first["title"], second["title"]}
    assert all("metadata" in row and "metainfo" not in row for row in aliases["items"])


def test_more_than_1000_aliases_and_legacy_canonical_limit(client):
    asyncio.run(seed_aliases(1203, swarms=1003))
    assert len(client.get("/api/private/catalog", headers=OWNER).json()) == 1000
    for view, count, key in (("aliases", 1203, "source_id"), ("releases", 1003, "id")):
        keys, cursor, snapshot = [], None, None
        while True:
            response = page(client, **({"cursor": cursor} if cursor else {"view": view}))
            assert response.status_code == 200, response.text
            value = response.json()
            assert len(value["items"]) <= 100 and len(response.content) <= catalog.PAGE_BYTES
            snapshot = snapshot or value["snapshot"]
            assert value["snapshot"] == snapshot
            keys.extend(row[key] for row in value["items"])
            cursor = value["next_cursor"]
            if cursor is None:
                break
        assert len(keys) == count and keys == sorted(set(keys))


def test_more_than_100000_synthetic_aliases_stream_exact_coverage():
    async def run():
        count = 100003
        await seed_aliases(count, swarms=5)
        cursor, seen, snapshot = None, 0, None
        while True:
            value = await catalog.page(QueryParams({"cursor": cursor} if cursor else {}))
            snapshot = snapshot or value["snapshot"]
            assert value["snapshot"] == snapshot
            assert snapshot["alias_total"] == count and snapshot["release_total"] == 5
            assert len(value["items"]) <= 100
            for row in value["items"]:
                assert row["source_id"] == f"asset:{seen:08d}"
                assert row["id"] == f"{seen % 5:032x}"
                seen += 1
            cursor = value["next_cursor"]
            if cursor is None:
                break
        assert seen == count
    asyncio.run(run())


def test_readiness_changes_do_not_disrupt_enumeration_or_ack_retry(client):
    items = [{**metainfo(), "source_id": f"asset:{i}"} for i in range(3)]
    result = imported(client, items).json()
    first = page(client, limit=1).json()
    asyncio.run(mark_ready())
    assert imported(client, items).json() == result
    second = page(client, cursor=first["next_cursor"]).json()
    assert second["items"][0]["ready"] == 1 and second["snapshot"] == first["snapshot"]
    asyncio.run(private.reset_readiness())
    third = page(client, cursor=second["next_cursor"]).json()
    assert third["items"][0]["ready"] == 0 and third["next_cursor"] is None
    assert [first["items"][0]["source_id"], second["items"][0]["source_id"], third["items"][0]["source_id"]] == ["asset:0", "asset:1", "asset:2"]


@pytest.mark.parametrize("mutation", ["insert-before", "insert-after", "metadata", "visibility"])
def test_effective_metadata_change_invalidates_cursor_once(client, mutation):
    first_item = {**metainfo(), "source_id": "asset:b"}
    result = imported(client, [first_item, {**first_item, "source_id": "asset:d"}]).json()
    first = page(client, limit=1).json()
    if mutation.startswith("insert"):
        item = {**first_item, "source_id": "asset:a" if mutation == "insert-before" else "asset:z"}
        assert imported(client, [item]).status_code == 200
    elif mutation == "metadata":
        assert imported(client, [{**first_item, "title": "Changed title"}]).status_code == 200
    else:
        rid = result["results"][0]["id"]
        assert client.patch(f"/api/private/catalog/{rid}", json={"published": False}, headers=OWNER).status_code == 200
        token = current_state()["revision_token"]
        assert client.patch(f"/api/private/catalog/{rid}", json={"published": False}, headers=OWNER).status_code == 200
        assert current_state()["revision_token"] == token
    assert current_state()["revision_token"] != first["snapshot"]["revision_token"]
    assert page(client, cursor=first["next_cursor"]).status_code == 409
    restarted = page(client).json()
    assert restarted["snapshot"]["alias_total"] == (3 if mutation.startswith("insert") else 2)


def test_late_import_conflict_and_ack_limit_roll_back_metadata_and_revision(client, monkeypatch):
    first = metainfo()
    imported(client, [first])
    before = current_state()
    changed = dict(first)
    # A second valid torrent with the same source ID conflicts after the new alias.
    from torrent_metainfo import bdecode, bencode
    import base64
    torrent = bdecode(base64.b64decode(changed["metainfo_base64"]))
    torrent[b"info"][b"name"] = b"Different.mkv"
    changed["metainfo_base64"] = base64.b64encode(bencode(torrent)).decode()
    changed["info_hash"] = hashlib.sha1(bencode(torrent[b"info"])).hexdigest()
    assert imported(client, [{**first, "source_id": "new:alias"}, changed]).status_code == 409
    assert current_state() == before and page(client).json()["snapshot"]["alias_total"] == 1
    monkeypatch.setattr(catalog, "ACK_BYTES", 10)
    assert imported(client, [{**first, "source_id": "new:alias"}]).status_code == 409
    assert current_state() == before and page(client).json()["snapshot"]["alias_total"] == 1


def test_corrupt_alias_is_not_acknowledged_or_silently_omitted(client):
    item = metainfo()
    imported(client, [item])
    before = current_state()
    async def corrupt():
        async with private.private_write() as db:
            await db.execute("UPDATE private_release_aliases SET release_id='absent'")
    asyncio.run(corrupt())
    assert imported(client, [{**item, "title": "Changed"}]).status_code == 409
    assert page(client).status_code == 409
    assert current_state() == before


def test_page_uses_one_wal_snapshot_without_blocking_metadata_writer(client, monkeypatch):
    imported(client, [{**metainfo(), "source_id": f"asset:{i}"} for i in range(2)])
    expected = current_state()
    real_state = catalog.state
    armed = True
    async def observe(db):
        nonlocal armed
        result = await real_state(db)
        if armed:
            armed = False
            async with private.private_write() as writer:
                await writer.execute("INSERT INTO private_release_aliases SELECT 'asset:3',release_id,title,kind,metadata FROM private_release_aliases LIMIT 1")
                await catalog.changed(writer)
        return result
    monkeypatch.setattr(catalog, "state", observe)
    first = page(client, limit=1).json()
    assert first["snapshot"] == {**expected, "alias_total": 2, "release_total": 1}
    assert first["items"][0]["source_id"] == "asset:0"
    assert page(client, cursor=first["next_cursor"]).status_code == 409
    assert page(client).json()["snapshot"]["alias_total"] == 3


def test_restart_and_unchanged_restore_preserve_cursor_but_later_edits_do_not_reuse_token(client, tmp_path):
    items = [{**metainfo(), "source_id": f"asset:{i}"} for i in range(3)]
    imported(client, items)
    first = page(client, limit=1).json()
    path = config.settings.db_path
    backup = tmp_path / "backup.db"
    with sqlite3.connect(path) as source, sqlite3.connect(backup) as target:
        source.backup(target)
    asyncio.run(database.close_all())
    asyncio.run(database.get_db())
    assert page(client, cursor=first["next_cursor"]).status_code == 200
    imported(client, [{**items[0], "title": "First change"}])
    edited_token = current_state()["revision_token"]
    assert page(client, cursor=first["next_cursor"]).status_code == 409
    asyncio.run(database.close_all())
    with sqlite3.connect(backup) as source, sqlite3.connect(path) as target:
        source.backup(target)
    asyncio.run(database.get_db())
    assert page(client, cursor=first["next_cursor"]).status_code == 200
    imported(client, [{**items[0], "title": "Different change"}])
    assert current_state()["revision_token"] not in {first["snapshot"]["revision_token"], edited_token}
    assert page(client, cursor=first["next_cursor"]).status_code == 409


def test_atomic_migration_recovers_after_interruption_and_preserves_identity(tmp_path, monkeypatch):
    monkeypatch.setattr(config.settings, "db_path", str(tmp_path / "migration.db"))
    real_execute = aiosqlite.Connection.execute
    def fail_insert(self, sql, parameters=None):
        if sql.startswith("INSERT OR IGNORE INTO private_catalog_state"):
            raise RuntimeError("Synthetic interruption")
        return real_execute(self, sql, parameters or ())
    with monkeypatch.context() as patch:
        patch.setattr(aiosqlite.Connection, "execute", fail_insert)
        with pytest.raises(RuntimeError):
            asyncio.run(catalog.initialize())
    with sqlite3.connect(config.settings.db_path) as db:
        assert db.execute("SELECT name FROM sqlite_master WHERE name='private_catalog_state'").fetchall() == []
    async def initialized():
        await catalog.initialize()
        async with aiosqlite.connect(config.settings.db_path) as db:
            db.row_factory = aiosqlite.Row
            return await catalog.state(db)
    assert asyncio.run(initialized()) == asyncio.run(initialized())


@pytest.mark.parametrize("query", ["limit=0", "limit=101", "limit=-1", "limit=true", "view=invalid", "limit=1&limit=2", "other=1", "cursor=", "cursor="+"a"*1025])
def test_invalid_page_options_fail(client, query):
    assert client.get("/api/private/catalog/page?"+query, headers=OWNER).status_code == 422


def test_cursor_signing_expiry_parameters_and_secret_rotation(client, monkeypatch):
    imported(client, [{**metainfo(), "source_id": f"asset:{i}"} for i in range(3)])
    first = page(client, limit=1).json()
    token = first["next_cursor"]
    payload = catalog._decode_cursor(token)
    assert page(client, cursor=token, view="releases").status_code == 422
    assert page(client, cursor=token, limit=2).status_code == 422
    assert page(client, cursor=token[:-1]+("0" if token[-1] != "0" else "1")).status_code == 422
    for key, value in (("version", True), ("limit", True), ("purpose", "other"), ("after", "../media"), ("issued_at", 2**63), ("alias_total", -1)):
        forged = catalog._encode_cursor({**payload, key: value})
        assert page(client, cursor=forged).status_code == 422
    assert page(client, cursor=catalog._encode_cursor({**payload, "instance_id": "0"*32})).status_code == 409
    with monkeypatch.context() as patch:
        patch.setattr(config.settings, "private_indexer_secret", "different"*8)
        assert page(client, cursor=token).status_code == 422
    with monkeypatch.context() as patch:
        patch.setattr(catalog.time, "time", lambda: payload["issued_at"]+1)
        next_payload = catalog._decode_cursor(page(client, cursor=token).json()["next_cursor"])
        assert (next_payload["issued_at"], next_payload["expires_at"]) == (payload["issued_at"], payload["expires_at"])
    with monkeypatch.context() as patch:
        patch.setattr(catalog.time, "time", lambda: payload["expires_at"])
        assert page(client, cursor=token).status_code == 410


def test_operator_auth_precedes_cursor_work_and_no_paths_or_member_credentials_return(client, monkeypatch):
    imported(client, [metainfo()])
    def forbidden(_token):
        raise AssertionError("Unauthenticated cursor parsing")
    monkeypatch.setattr(catalog, "_decode_cursor", forbidden)
    for headers in ({}, identity(), {**OWNER, "host": "library.example.org"}, {**OWNER, "x-bitagent-proxy-proof": "wrong"}):
        assert client.get("/api/private/catalog/page?cursor=invalid", headers=headers).status_code in {401, 403, 404}
    response = page(client)
    assert response.status_code == 200 and response.headers["cache-control"] == "no-store"
    assert not ({"files", "metainfo", "metainfo_base64", "passkey", "save_path", "user_id", "address"} & response.json()["items"][0].keys())
    monkeypatch.setattr(config.settings, "private_indexer_enabled", False)
    assert page(client, cursor="invalid").status_code == 404


def test_stored_unknown_metadata_and_response_budget_fail_without_leaking_data(client, monkeypatch):
    imported(client, [metainfo()])
    async def corrupt():
        async with private.private_write() as db:
            await db.execute("UPDATE private_release_aliases SET metadata=?", (json.dumps({"files": ["synthetic private path"]}),))
    asyncio.run(corrupt())
    result = page(client)
    assert result.status_code == 409 and "synthetic private path" not in result.text
    imported(client, [metainfo()])
    monkeypatch.setattr(catalog, "PAGE_BYTES", 10)
    assert page(client).status_code == 409


def test_import_ack_bounds_empty_batch_and_duplicates(client):
    assert imported(client, []).json()["results"] == []
    before = current_state()
    assert imported(client, [metainfo(), metainfo()]).status_code == 422
    assert current_state() == before
    for query in ("ack=true", "ack=2", "ack=1&ack=1"):
        assert client.post("/api/private/catalog/import?"+query, json={"releases": []}, headers=OWNER).status_code == 422
    # Maximum input count returns exactly one bounded mapping per alias.
    items = [{**metainfo(), "source_id": "x"*195+f"{i:05d}"} for i in range(1000)]
    result = imported(client, items)
    assert result.status_code == 200 and len(result.json()["results"]) == 1000
    assert len(result.content) <= catalog.ACK_BYTES
    assert len(page(client, limit=1).json()["next_cursor"]) <= catalog.CURSOR_BYTES
    assert imported(client, items+[metainfo()]).status_code == 422


def test_empty_catalog_has_complete_zero_counts_and_no_cursor(client):
    value = page(client, limit=1).json()
    assert value["items"] == [] and value["next_cursor"] is None
    assert value["snapshot"]["alias_total"] == value["snapshot"]["release_total"] == 0
    assert set(value["snapshot"]) == {"instance_id", "revision_token", "alias_total", "release_total"}


def test_signed_duplicate_json_fields_are_rejected(client):
    raw = b'{"version":1,"version":1}'
    token = catalog._b64(raw)+"."+catalog._sign(raw)
    with pytest.raises(HTTPException) as error:
        catalog._decode_cursor(token)
    assert error.value.status_code == 422


def test_failed_commit_returns_no_ack_and_restores_revision_and_rows(client, monkeypatch):
    imported(client, [metainfo()])
    before = current_state()
    async def failed_commit(_db):
        raise RuntimeError("Synthetic commit failure")
    with monkeypatch.context() as patch:
        patch.setattr(aiosqlite.Connection, "commit", failed_commit)
        with pytest.raises(RuntimeError, match="Synthetic commit failure"):
            imported(client, [{**metainfo(), "source_id": "new:alias"}])
    assert current_state() == before
    assert page(client).json()["snapshot"]["alias_total"] == 1


def test_revision_token_changes_once_for_effective_batch_and_not_identical_retry(client, monkeypatch):
    calls = []
    real_changed = catalog.changed
    async def changed(db):
        calls.append(True)
        await real_changed(db)
    monkeypatch.setattr(catalog, "changed", changed)
    items = [{**metainfo(), "source_id": f"asset:{i}"} for i in range(10)]
    result = imported(client, items).json()
    assert len(calls) == 1
    assert imported(client, items).json() == result and len(calls) == 1
    altered = [{**item, "title": "Changed", "season": 2} for item in items]
    assert imported(client, altered).status_code == 200 and len(calls) == 2


def test_maximum_semantic_fields_fit_bounded_page_without_torrent_paths(client):
    item = {**metainfo(), "title": "\U0001f600"*500, "episodes": list(range(1400)), "kind": "season"}
    assert imported(client, [{**item, "source_id": f"asset:{i:03d}"} for i in range(100)]).status_code == 200
    response = page(client)
    assert response.status_code == 200 and len(response.content) <= catalog.PAGE_BYTES
    assert len(response.json()["items"]) == 100 and response.json()["next_cursor"] is None
    assert all(row["metadata"]["episodes"] == item["episodes"] for row in response.json()["items"])


@pytest.mark.parametrize("change", ["unknown", "nonfinite", "bool-count", "wrong-after-type", "bad-expiry"])
def test_signed_cursor_schema_is_strict(client, change):
    imported(client, [{**metainfo(), "source_id": f"asset:{i}"} for i in range(2)])
    payload = catalog._decode_cursor(page(client, limit=1).json()["next_cursor"])
    if change == "unknown":
        payload["extra"] = "value"
    elif change == "bool-count":
        payload["alias_total"] = True
    elif change == "wrong-after-type":
        payload["after"] = []
    elif change == "bad-expiry":
        payload["expires_at"] += 1
    raw = json.dumps(payload).encode()
    if change == "nonfinite":
        raw = raw.replace(b'"limit": 1', b'"limit": NaN')
    token = catalog._b64(raw)+"."+catalog._sign(raw)
    assert page(client, cursor=token).status_code == 422

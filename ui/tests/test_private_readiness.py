"""Epoch, bounded cohort and actual WAL/lifecycle readiness regressions."""
from __future__ import annotations

import asyncio
from contextlib import asynccontextmanager
import hashlib
import json
import os
from pathlib import Path
import sqlite3
import subprocess
import sys
import time
from types import SimpleNamespace

import aiosqlite
import httpx
import pytest

import config
import database
import private_indexer as private
import private_readiness as readiness
_ASYNC_CLIENT = httpx.AsyncClient

from test_private_indexer import identity, mark_ready, metainfo, private_config as private_config, setup_release


async def cohort(count):
    async with private.private_write() as db:
        for start in range(0, count, 500):
            await db.executemany("""INSERT INTO private_releases
                (id,source_id,title,kind,info_hash,size,metadata,metainfo,created_at,publication_nonce)
                VALUES (?,?,?,?,?,?,?,?,?,?)""", [
                (f"{i*2:032x}", f"fixture:{i}", "Invented", "movie", hashlib.sha1(f"fixture:{i}".encode()).hexdigest(),
                 1, "{}", b"", 1, "c" * 32) for i in range(start, min(count, start+500))])
        await db.execute("INSERT INTO private_release_order(release_id) SELECT id FROM private_releases ORDER BY id")


def seeder_transport(monkeypatch, action=None):
    original = _ASYNC_CLIENT
    calls = []
    async def handle(request):
        assert request.headers["accept-encoding"] == "identity"
        if request.url.path.endswith("auth/login"):
            return httpx.Response(204, headers={"set-cookie": "SID=" + "a" * 32 + "; Path=/"})
        hashes = request.url.params["hashes"].split("|")
        assert len(hashes) <= 100
        calls.append(hashes)
        if action:
            result = await action(len(calls), hashes)
            if result is not None:
                return result
        return httpx.Response(200, json=[{"hash": value, "size": 1, "amount_left": 0,
                                          "progress": 1, "state": "stalledUP"} for value in hashes])
    monkeypatch.setattr(readiness.httpx, "AsyncClient", lambda **kwargs: original(transport=httpx.MockTransport(handle), **kwargs))
    return calls


def test_fixed_publication_sequence_covers_100003_and_defers_insertions(client, monkeypatch):
    async def run():
        await cohort(100003)
        async def add_first(page, _hashes):
            if page == 1:
                async with private.private_write() as db:
                    # These IDs are both below and above the old random-ID max.
                    for value in ("0" * 31 + "1", "f" * 32):
                        await db.execute("""INSERT INTO private_releases
                            (id,source_id,title,kind,info_hash,size,metadata,metainfo,created_at,publication_nonce)
                            VALUES (?,?,?,?,?,1,'{}',X'',1,?)""",
                            (value, value, "Invented", "movie", hashlib.sha1(value.encode()).hexdigest(), "d" * 32))
                        await readiness.publish_order(db, value)
        calls = seeder_transport(monkeypatch, add_first)
        assert await private.refresh_readiness()
        assert sum(map(len, calls)) == 100003 and len(calls) == 1001
        # No Python full cohort; every requested hash is visited exactly once.
        assert len({value for page in calls for value in page}) == 100003
        db = await database.get_db()
        counts = await db.execute_fetchall("SELECT sum(ready) AS ready,count(*) AS n FROM private_releases")
        assert counts[0]["ready"] == 100003 and counts[0]["n"] == 100005
        assert (await db.execute_fetchall("SELECT ready FROM private_releases WHERE id=?", ("f"*32,)))[0]["ready"] == 0
    asyncio.run(run())


def test_late_failure_withdraws_early_commits_and_recovery_cannot_revive_untouched(client, monkeypatch):
    async def run():
        await cohort(201)
        async def fail_last(page, _hashes):
            if page == 3:
                assert readiness.capture().active
                return httpx.Response(503)
        seeder_transport(monkeypatch, fail_last)
        assert not await private.refresh_readiness()
        old = readiness.capture()
        clause, args = readiness.predicate()
        assert not old.active and not await (await database.get_db()).execute_fetchall("SELECT id FROM private_releases WHERE " + clause, args)
        seeder_transport(monkeypatch)
        assert await readiness.verify(await database.get_db(), "0"*32, private.private_write, private._seeder_login_ok)
        current = readiness.capture()
        assert current.epoch == old.epoch  # Old ready rows belong to the earlier epoch.
        clause, args = readiness.predicate()
        rows = await (await database.get_db()).execute_fetchall("SELECT id FROM private_releases WHERE " + clause, args)
        assert [row["id"] for row in rows] == ["0"*32]
    asyncio.run(run())


def test_failed_durable_withdrawal_requires_new_persisted_recovery_epoch(client, monkeypatch):
    async def run():
        await cohort(2)
        await mark_ready()
        original = readiness.capture().epoch
        @asynccontextmanager
        async def failed_write(**_kwargs):
            raise sqlite3.OperationalError("invented busy")
            yield
        await readiness.invalidate(failed_write, original)
        assert readiness.capture().dirty and not readiness.capture().active
        seeder_transport(monkeypatch)
        assert await readiness.verify(await database.get_db(), "0"*32, private.private_write, private._seeder_login_ok)
        assert readiness.capture().epoch != original and not readiness.capture().dirty
        clause, args = readiness.predicate()
        rows = await (await database.get_db()).execute_fetchall("SELECT id FROM private_releases WHERE " + clause, args)
        assert [row["id"] for row in rows] == ["0"*32]
    asyncio.run(run())


def test_visibility_aba_refuses_probe_and_unchanged_visibility_also_rotates(client, monkeypatch):
    key, release_id = setup_release(client, ready=False)
    async def action(_page, _hashes):
        async with private.private_write() as db:
            await db.execute("UPDATE private_releases SET withdrawn=1,publication_nonce=? WHERE id=?", ("a"*32, release_id))
            await db.execute("UPDATE private_releases SET withdrawn=0,publication_nonce=? WHERE id=?", ("b"*32, release_id))
        return httpx.Response(200, json=[{"hash": metainfo()["info_hash"], "size": metainfo()["size"],
                                       "amount_left": 0, "progress": 1, "state": "stalledUP"}])
    seeder_transport(monkeypatch, action)
    assert client.post(f"/api/private/catalog/{release_id}/verify", headers=identity("owner", "OWNER")).status_code == 409
    assert client.get(f"/private/torrents/{release_id}.torrent", params={"apikey": key}).status_code == 404
    async def nonce():
        return (await (await database.get_db()).execute_fetchall("SELECT publication_nonce FROM private_releases WHERE id=?", (release_id,)))[0][0]
    first = asyncio.run(nonce())
    assert client.patch(f"/api/private/catalog/{release_id}", json={"published": True}, headers=identity("owner", "OWNER")).status_code == 200
    assert asyncio.run(nonce()) != first


@pytest.mark.parametrize("kind", ["torrent", "magnet"])
def test_failure_recovery_aba_after_key_await_never_issues_old_proof(client, monkeypatch, kind):
    key, release_id = setup_release(client)
    original = private._tracker_key
    async def recover_after_key(row):
        token = await original(row)
        await private.reset_readiness()
        await mark_ready(release_id)
        return token
    monkeypatch.setattr(private, "_tracker_key", recover_after_key)
    response = client.get(f"/private/torrents/{release_id}.torrent", params={"apikey": key}) if kind == "torrent" else client.get(f"/api/library/private/{release_id}/magnet", headers=identity())
    assert response.status_code == 404
    assert client.get("/api/account/private/metrics", headers=identity()).json()["totals"]["issued"] == 0


@pytest.mark.parametrize("raw", [b'[{"hash":"x","hash":"y"}]', b'[{"progress":NaN}]', b'{}', b'[true]', b'['*100])
def test_parser_rejects_malformed_records(raw):
    with pytest.raises(ValueError):
        readiness.parse_status(raw, [{"info_hash": "a"*40, "size": 1}])


@pytest.mark.parametrize("field,value", [("progress", True), ("amount_left", False), ("size", True), ("progress", 1.1)])
def test_parser_rejects_bool_and_invalid_numbers(field, value):
    row = {"hash": "a"*40, "size": 1, "progress": 1, "amount_left": 0, "state": "uploading", field: value}
    with pytest.raises(ValueError):
        readiness.parse_status(json.dumps([row]), [{"info_hash": "a"*40, "size": 1}])


@pytest.mark.parametrize("failure", ["compressed", "overflow", "cancelled"])
def test_stream_failure_clears_all_proofs(client, monkeypatch, failure):
    async def run():
        await cohort(1)
        await mark_ready()
        async def fail(_page, _hashes):
            if failure == "cancelled":
                raise asyncio.CancelledError
            return httpx.Response(200, content=b"x"*(readiness.INFO_BYTES+1) if failure == "overflow" else b"[]",
                                  headers={"content-encoding": "gzip"} if failure == "compressed" else {})
        seeder_transport(monkeypatch, fail)
        if failure == "cancelled":
            with pytest.raises(asyncio.CancelledError):
                await private.refresh_readiness()
        else:
            assert not await private.refresh_readiness()
        assert not readiness.capture().active
    asyncio.run(run())


def test_proofs_keep_request_start_time_and_expire_while_response_waits(client, monkeypatch):
    async def run():
        await cohort(1)
        clock = SimpleNamespace(wall=time.time(), mono=time.monotonic())
        monkeypatch.setattr(readiness, "time", SimpleNamespace(time=lambda: clock.wall, monotonic=lambda: clock.mono))
        async def delay(_page, _hashes):
            clock.wall += 2
            clock.mono += 2
        seeder_transport(monkeypatch, delay)
        start = clock.wall
        assert await private.refresh_readiness()
        row = (await (await database.get_db()).execute_fetchall("SELECT * FROM private_releases"))[0]
        assert row["verified_at"] == start and row["ready_until_mono"] == clock.mono-2+config.settings.private_seed_verification_ttl
        captured = readiness.capture()
        clock.wall -= 10
        assert not readiness.effective(row, captured)  # Future wall timestamps fail closed.
        clock.wall = start+3
        clock.mono = row["ready_until_mono"]
        assert not readiness.effective(row, captured)
    asyncio.run(run())


def test_short_transaction_releases_wal_writer_between_batches(client, monkeypatch):
    async def run():
        await cohort(101)
        events = []
        async def interleave(page, _hashes):
            # A real independent WAL writer commits during HTTP; no readiness
            # transaction holds the global writer while the probe is pending.
            async with aiosqlite.connect(config.settings.db_path, timeout=.1) as writer:
                await writer.execute("INSERT OR REPLACE INTO private_members VALUES ('writer',1,'fixture',?)", (page,))
                await writer.commit()
            events.append(page)
        seeder_transport(monkeypatch, interleave)
        assert await private.refresh_readiness()
        assert events == [1, 2]
    asyncio.run(run())


def test_owner_rejects_second_process_and_hardlink_and_releases(client, tmp_path):
    script = "import config,private_readiness as r;config.settings.private_indexer_enabled=True;config.settings.db_path=__import__('sys').argv[1];r.acquire_owner();r.release_owner()"
    env = {**os.environ, "PYTHONPATH": str(Path(private.__file__).parent)}
    active = subprocess.run([sys.executable, "-c", script, config.settings.db_path], env=env, capture_output=True, timeout=5)
    assert active.returncode != 0 and b"ownership unavailable" in active.stderr
    alias = tmp_path / "database-alias.db"
    os.link(config.settings.db_path, alias)
    try:
        result = subprocess.run([sys.executable, "-c", script, str(alias)], env=env, capture_output=True, timeout=5)
        assert result.returncode != 0 and b"regular database identity" in result.stderr
    finally:
        alias.unlink()
    # The real client owns this lock until lifespan shutdown, not per probe.
    assert readiness._OWNER is not None


def test_default_import_and_owner_noop_remain_portable(monkeypatch):
    monkeypatch.setattr(config.settings, "private_indexer_enabled", False)
    before = readiness._OWNER
    readiness.acquire_owner()
    assert readiness._OWNER is before


def test_migration_populates_order_without_loading_metainfo_and_preserves_history(tmp_path):
    async def run():
        async with aiosqlite.connect(tmp_path / "old.db") as db:
            await db.executescript("""CREATE TABLE private_releases(id TEXT PRIMARY KEY,info_hash TEXT,size INTEGER,withdrawn INTEGER,metainfo BLOB);
                CREATE TABLE private_peers(updated_at REAL);
                INSERT INTO private_releases VALUES ('one','hash',1,0,zeroblob(4194304));""")
            await readiness.initialize(db)
            await readiness.initialize(db)
            assert (await db.execute_fetchall("SELECT count(*) FROM private_release_order"))[0][0] == 1
            assert (await db.execute_fetchall("SELECT length(metainfo),length(publication_nonce) FROM private_releases"))[0][:] == (4194304, 32)
    asyncio.run(run())


def test_expiry_index_covers_100000_peer_and_ledger_rows(client):
    async def run():
        async with private.private_write() as db:
            for start in range(0, 100000, 500):
                await db.executemany("INSERT INTO private_peers VALUES (?,?,?,?,?,?,?,?,?,?,?)",
                    [(f"{i:032x}", "fixture", 1, b"invented", "192.0.2.1", 6881, 1, 1, 0, 0, 1)
                     for i in range(start, start+500)])
                await db.executemany("INSERT INTO private_transfer_totals VALUES (?,?,?,?,?,?)",
                    [("fixture", f"{i:032x}", 1, 1, 0, 0) for i in range(start, start+500)])
            plan = await db.execute_fetchall("EXPLAIN QUERY PLAN DELETE FROM private_peers WHERE updated_at<?", (2,))
            assert any("private_peers_expiry" in row[3] for row in plan)
            await db.execute("DELETE FROM private_peers WHERE updated_at<?", (2,))
        db = await database.get_db()
        assert (await db.execute_fetchall("SELECT count(*) FROM private_peers"))[0][0] == 0
        assert (await db.execute_fetchall("SELECT sum(uploaded) FROM private_transfer_totals"))[0][0] == 100000
    asyncio.run(run())


@pytest.mark.parametrize("kind", ["symlink", "fifo", "permissions"])
def test_owner_rejects_unsafe_sidecar_without_touching_target(tmp_path, monkeypatch, kind):
    path = tmp_path / "fixture.db"
    path.write_bytes(b"invented")
    monkeypatch.setattr(config.settings, "db_path", str(path))
    lock = Path(str(path) + ".private-readiness.lock")
    target = tmp_path / "unchanged"
    target.write_text("sentinel")
    if kind == "symlink":
        lock.symlink_to(target)
    elif kind == "fifo":
        os.mkfifo(lock)
    else:
        lock.write_bytes(b"")
        lock.chmod(0o666)
    with pytest.raises(RuntimeError, match="ownership unavailable"):
        readiness.acquire_owner()
    assert target.read_text() == "sentinel" and readiness._OWNER is None


def test_ownership_is_required_for_direct_verify_recovery(tmp_path, monkeypatch):
    # Direct calls cannot make --lifespan off a supported private deployment.
    assert readiness._OWNER is None
    async def run():
        with pytest.raises(RuntimeError, match="lifespan owner"):
            await readiness.recover(private.private_write, time.monotonic()+1)
    asyncio.run(run())


def test_readiness_write_busy_budget_withdraws_before_long_sqlite_default(client, monkeypatch):
    async def run():
        await cohort(1)
        await mark_ready()
        seeder_transport(monkeypatch)
        blocker = sqlite3.connect(config.settings.db_path)
        blocker.execute("BEGIN IMMEDIATE")
        started = time.monotonic()
        try:
            assert not await private.refresh_readiness()
            assert not readiness.capture().active and readiness.capture().dirty
            assert time.monotonic()-started < 2
        finally:
            blocker.rollback()
            blocker.close()
    asyncio.run(run())


def test_probe_queue_is_bounded_and_timeout_cleans_waiter(client):
    async def run():
        readiness._PROBE_LOCK = asyncio.Lock()
        await readiness._PROBE_LOCK.acquire()
        try:
            with pytest.raises(TimeoutError):
                async with readiness.admission(time.monotonic()+.01, operator=True):
                    pytest.fail("blocked probe was admitted")
            assert readiness._OPERATORS == 0
            readiness._OPERATORS = readiness.MAX_OPERATORS
            with pytest.raises(ValueError, match="queue full"):
                async with readiness.admission(time.monotonic()+1, operator=True):
                    pytest.fail("unbounded operator queue")
        finally:
            readiness._OPERATORS = 0
            readiness._PROBE_LOCK.release()
    asyncio.run(run())


def test_operator_catalog_checks_nonce_after_its_wal_snapshot(client, monkeypatch):
    import privatecatalog
    _key, release_id = setup_release(client)
    original = readiness.matching_proofs
    changed = False
    async def withdraw_before_final(db, rows, gate):
        nonlocal changed
        if not changed:
            changed = True
            async with private.private_write() as writer:
                await writer.execute("UPDATE private_releases SET ready=0,publication_nonce=? WHERE id=?", ("a"*32, release_id))
        return await original(db, rows, gate)
    monkeypatch.setattr(readiness, "matching_proofs", withdraw_before_final)
    response = client.get("/api/private/catalog/page", headers=identity("owner", "OWNER"))
    assert response.status_code == 200 and response.json()["items"][0]["ready"] == 0
    assert len(response.content) <= privatecatalog.PAGE_BYTES


def test_search_checks_original_proofs_after_peer_queries(client, monkeypatch):
    _key, release_id = setup_release(client)
    async def install():
        db = await database.get_db()
        original = db.execute_fetchall
        changed = False
        async def lookup(query, args=()):
            nonlocal changed
            result = await original(query, args)
            if "SELECT p.remaining,p.ip,p.user_id" in query and not changed:
                changed = True
                async with private.private_write() as writer:
                    await writer.execute("UPDATE private_releases SET ready=0,publication_nonce=? WHERE id=?", ("a"*32, release_id))
            return result
        monkeypatch.setattr(db, "execute_fetchall", lookup)
    asyncio.run(install())
    assert client.get("/api/library/private", headers=identity()).json()["items"] == []


def test_raw_compression_is_refused_without_reading_decompressor():
    class ForbiddenStream(httpx.AsyncByteStream):
        async def __aiter__(self):
            pytest.fail("compressed response was read")
            yield b""
    async def run():
        def handle(_request):
            return httpx.Response(200, headers={"content-encoding": "gzip"}, stream=ForbiddenStream())
        async with _ASYNC_CLIENT(transport=httpx.MockTransport(handle)) as client:
            with pytest.raises(ValueError, match="Encoded"):
                await readiness.response_bytes(client, "GET", "http://fixture.example.org", 10)
    asyncio.run(run())


def test_total_probe_deadline_withdraws_a_slow_stream(client, monkeypatch):
    async def run():
        await cohort(1)
        await mark_ready()
        monkeypatch.setattr(readiness, "REQUEST_SECONDS", .02)
        class Dribble(httpx.AsyncByteStream):
            async def __aiter__(self):
                for _ in range(20):
                    await asyncio.sleep(.005)
                    yield b" "
        def handle(request):
            if request.url.path.endswith("auth/login"):
                return httpx.Response(204, headers={"set-cookie": "SID="+"a"*32+"; Path=/"})
            return httpx.Response(200, stream=Dribble())
        monkeypatch.setattr(readiness.httpx, "AsyncClient", lambda **kwargs: _ASYNC_CLIENT(transport=httpx.MockTransport(handle), **kwargs))
        assert not await private.refresh_readiness()
        assert not readiness.capture().active
    asyncio.run(run())


def test_migration_failure_rolls_back_columns_and_order(tmp_path, monkeypatch):
    async def run():
        async with aiosqlite.connect(tmp_path / "old.db") as db:
            await db.executescript("CREATE TABLE private_releases(id TEXT PRIMARY KEY);CREATE TABLE private_peers(updated_at REAL);")
            original = db.execute
            async def fail(query, parameters=()):
                if "CREATE TABLE IF NOT EXISTS private_release_order" in query:
                    raise sqlite3.OperationalError("invented migration failure")
                return await original(query, parameters)
            monkeypatch.setattr(db, "execute", fail)
            with pytest.raises(sqlite3.OperationalError):
                await readiness.initialize(db)
            assert [row[1] for row in await db.execute_fetchall("PRAGMA table_info(private_releases)")] == ["id"]
            assert not await db.execute_fetchall("SELECT name FROM sqlite_master WHERE name='private_readiness_state'")
    asyncio.run(run())


def test_known_failure_withdraws_before_slow_response_cleanup(client, monkeypatch):
    async def run():
        await cohort(1)
        await mark_ready()
        closing, release = asyncio.Event(), asyncio.Event()
        class SlowClose(httpx.AsyncByteStream):
            async def __aiter__(self):
                yield b"[]"
            async def aclose(self):
                closing.set()
                await release.wait()
        def handle(request):
            if request.url.path.endswith("auth/login"):
                return httpx.Response(204, headers={"set-cookie": "SID="+"a"*32+"; Path=/"})
            return httpx.Response(503, stream=SlowClose())
        monkeypatch.setattr(readiness.httpx, "AsyncClient", lambda **kwargs: _ASYNC_CLIENT(transport=httpx.MockTransport(handle), **kwargs))
        task = asyncio.create_task(private.refresh_readiness())
        try:
            await asyncio.wait_for(closing.wait(), 1)
            assert not readiness.capture().active and readiness.capture().dirty
        finally:
            release.set()
            assert not await task
    asyncio.run(run())


@pytest.mark.parametrize("operation,when", [("refresh", "admission"), ("refresh", "exit"), ("verify", "exit")])
def test_cancellation_withdraws_while_client_exit_is_pending(client, monkeypatch, operation, when):
    async def run():
        await cohort(1)
        await mark_ready()
        gate = readiness.capture()
        entered, closing, release, fenced = (asyncio.Event() for _ in range(4))
        original_withdraw = readiness.withdraw_epoch
        def withdraw(epoch):
            original_withdraw(epoch)
            if not readiness.live(gate):
                fenced.set()
        monkeypatch.setattr(readiness, "withdraw_epoch", withdraw)
        class SlowCloseClient(_ASYNC_CLIENT):
            async def __aenter__(self):
                value = await super().__aenter__()
                entered.set()
                return value
            async def __aexit__(self, *args):
                closing.set()
                await release.wait()
                return await super().__aexit__(*args)
        def handle(request):
            if request.url.path.endswith("auth/login"):
                return httpx.Response(204, headers={"set-cookie": "SID="+"a"*32+"; Path=/"})
            return httpx.Response(200, json=[{"hash": request.url.params["hashes"], "size": 1,
                "amount_left": 0, "progress": 1, "state": "stalledUP"}])
        monkeypatch.setattr(readiness.httpx, "AsyncClient", lambda **kwargs: SlowCloseClient(transport=httpx.MockTransport(handle), **kwargs))
        lock = readiness._PROBE_LOCK
        if when == "admission":
            await lock.acquire()
        task = asyncio.create_task(private.refresh_readiness() if operation == "refresh" else
            readiness.verify(await database.get_db(), "0"*32, private.private_write, private._seeder_login_ok))
        try:
            await asyncio.wait_for(entered.wait() if when == "admission" else closing.wait(), 1)
            assert readiness.live(gate) and not task.done()
            task.cancel()
            await asyncio.wait_for(fenced.wait(), 1)
            await asyncio.wait_for(closing.wait(), 1)
            # No cleanup completion is needed to stop either existing proof
            # identities or a fresh SQL query from authorizing this release.
            assert not readiness.live(gate) and readiness.capture().dirty and not task.done()
            clause, args = readiness.predicate()
            assert not await (await database.get_db()).execute_fetchall("SELECT id FROM private_releases WHERE " + clause, args)
        finally:
            release.set()
            if when == "admission":
                lock.release()
            with pytest.raises(asyncio.CancelledError):
                await task
    asyncio.run(run())


def test_inherited_owner_cannot_authorize_or_recover_in_another_pid(client, monkeypatch):
    key, release_id = setup_release(client)
    gate = readiness.capture()
    assert readiness.live(gate)
    monkeypatch.setattr(readiness.os, "getpid", lambda: readiness._OWNER["pid"]+1)
    assert not readiness.live(gate)
    with pytest.raises(RuntimeError, match="lifespan owner"):
        readiness.require_owner()


@pytest.mark.parametrize("when", ["before-mutation", "after-ledger-write"])
def test_announce_readiness_withdrawal_rolls_back_peer_and_reported_deltas(client, monkeypatch, when):
    from test_private_indexer import announce, download, peer_observations
    key, release_id = setup_release(client)
    path = download(client, key, release_id)
    assert b"failure reason" not in announce(client, path)
    before = peer_observations()
    original = private.private_write
    @asynccontextmanager
    async def interrupted_write(**kwargs):
        async with original(**kwargs) as db:
            if when == "before-mutation":
                readiness.withdraw_local()
            else:
                execute = db.execute
                async def changed(query, parameters=()):
                    result = await execute(query, parameters)
                    if "INSERT INTO private_transfer_totals" in query:
                        readiness.withdraw_local()
                    return result
                monkeypatch.setattr(db, "execute", changed)
            yield db
    monkeypatch.setattr(private, "private_write", interrupted_write)
    assert b"failure reason" in announce(client, path, event="", uploaded=100, downloaded=100)
    assert peer_observations() == before

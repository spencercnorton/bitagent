"""Bounded private-seed observations with one owner and fail-closed epochs.

A proof records a client's seeder state, not a file rehash or byte delivery.
The local fence is effective immediately; persisted epochs prevent recovery
from making previously withdrawn observations current again.
"""
from __future__ import annotations

import asyncio
from contextlib import asynccontextmanager
from dataclasses import dataclass
import json
import math
import os
from pathlib import Path
import secrets
import stat
import time

import aiosqlite
import httpx

from config import settings

PAGE = 100
INFO_BYTES = 1024 * 1024
LOGIN_BYTES = 8192
REQUEST_SECONDS = 15
MAX_OPERATORS = 16
_COLUMNS = "ready_epoch,ready_until_mono,proof_id,publication_nonce"
_PROJECTION = "id,info_hash,size,publication_nonce"
_OWNER = None
_GATE = None
_PROBE_LOCK = None
_OPERATORS = 0


@dataclass(frozen=True)
class Gate:
    epoch: str
    active: bool = False
    dirty: bool = True


def capture():
    return _GATE


def live(gate):
    return bool(_OWNER is not None and _OWNER["pid"] == os.getpid() and gate and _GATE and gate.epoch == _GATE.epoch and gate.active and _GATE.active and not _GATE.dirty and not gate.dirty)


def acquire_owner():
    """Acquire the enabled app's same-host, cooperative lifecycle ownership."""
    global _OWNER, _GATE, _PROBE_LOCK
    if not settings.private_indexer_enabled:
        return
    if _OWNER is not None:
        raise RuntimeError("Private readiness already has an application owner")
    if os.name != "posix" or not all(hasattr(os, name) for name in ("O_NOFOLLOW", "O_DIRECTORY")):
        raise RuntimeError("Private readiness requires POSIX ownership support")
    import fcntl  # Disabled/default imports remain portable.
    path = Path(settings.db_path).expanduser().resolve()
    if path.exists():
        entry = path.stat()
        if not stat.S_ISREG(entry.st_mode) or entry.st_nlink != 1:
            raise RuntimeError("Private readiness requires one regular database identity")
    # Resolve the actual parent, then anchor the sidecar operation to that
    # directory. No followed sidecar leaf or hard-link alias is accepted.
    parent = os.open(path.anchor, os.O_RDONLY | os.O_DIRECTORY | os.O_NOFOLLOW)
    try:
        for component in path.parent.parts[1:]:
            child = os.open(component, os.O_RDONLY | os.O_DIRECTORY | os.O_NOFOLLOW, dir_fd=parent)
            os.close(parent)
            parent = child
            entry = os.fstat(parent)
            if entry.st_uid not in {0, os.geteuid()} or (entry.st_mode & stat.S_IWOTH and not entry.st_mode & stat.S_ISVTX):
                raise RuntimeError("Unsafe private readiness parent")
        current = path.parent.stat()
        opened = os.fstat(parent)
        if (current.st_dev, current.st_ino) != (opened.st_dev, opened.st_ino):
            raise RuntimeError("Private readiness parent changed")
    except BaseException:
        os.close(parent)
        raise
    fd = None
    try:
        fd = os.open(path.name + ".private-readiness.lock", os.O_RDWR | os.O_CREAT | os.O_NOFOLLOW | os.O_CLOEXEC | os.O_NONBLOCK,
                     0o600, dir_fd=parent)
        entry = os.fstat(fd)
        if not stat.S_ISREG(entry.st_mode) or entry.st_nlink != 1 or entry.st_uid != os.geteuid() or entry.st_mode & 0o077:
            raise RuntimeError("Unsafe private readiness ownership file")
        fcntl.flock(fd, fcntl.LOCK_EX | fcntl.LOCK_NB)
        # Refuse a concurrently swapped directory/lock entry after admission.
        if os.stat(path.name + ".private-readiness.lock", dir_fd=parent, follow_symlinks=False).st_ino != entry.st_ino:
            raise RuntimeError("Private readiness ownership identity changed")
        _OWNER = {"pid": os.getpid(), "fd": fd, "parent": parent, "path": path, "lock_inode": (entry.st_dev, entry.st_ino),
                  "db_inode": (path.stat().st_dev, path.stat().st_ino) if path.exists() else None}
        fd = None
        _GATE = Gate(secrets.token_hex(16))
        _PROBE_LOCK = asyncio.Lock()
    except (OSError, RuntimeError) as error:
        raise RuntimeError("Private readiness ownership unavailable") from error
    finally:
        if fd is not None:
            os.close(fd)
        if _OWNER is None:
            os.close(parent)


def require_owner():
    if _OWNER is None or _OWNER["pid"] != os.getpid():
        raise RuntimeError("Private readiness requires its application lifespan owner")
    owner = _OWNER
    path = owner["path"]
    lock = os.stat(path.name + ".private-readiness.lock", dir_fd=owner["parent"], follow_symlinks=False)
    directory, actual = os.fstat(owner["parent"]), path.parent.stat()
    entry = path.stat()
    identity = (entry.st_dev, entry.st_ino)
    if (Path(settings.db_path).expanduser().resolve() != path
            or (lock.st_dev, lock.st_ino) != owner["lock_inode"]
            or not stat.S_ISREG(lock.st_mode) or lock.st_nlink != 1 or lock.st_mode & 0o077
            or (directory.st_dev, directory.st_ino) != (actual.st_dev, actual.st_ino)
            or not stat.S_ISREG(entry.st_mode) or entry.st_nlink != 1
            or owner["db_inode"] is not None and owner["db_inode"] != identity):
        raise RuntimeError("Private readiness ownership identity changed")
    owner["db_inode"] = identity


def withdraw_local():
    global _GATE
    _GATE = Gate(secrets.token_hex(16))


def withdraw_epoch(epoch):
    if epoch is not None and _GATE is not None and _GATE.epoch == epoch:
        withdraw_local()


@asynccontextmanager
async def fenced_context(manager, epoch):
    """Withdraw before awaited cleanup, including cancellation during exit.

    Cleanup runs in a shielded task so cancellation reaches this owner first;
    a transport's own asynchronous exit cannot postpone the local fence.
    The managers used here do not suppress exceptions.
    """
    try:
        value = await manager.__aenter__()
    except BaseException:
        withdraw_epoch(epoch())
        raise
    error = None
    try:
        yield value
    except BaseException as caught:
        withdraw_epoch(epoch())
        error = caught
        raise
    finally:
        closing = asyncio.create_task(manager.__aexit__(
            type(error) if error is not None else None, error,
            error.__traceback__ if error is not None else None))
        try:
            await asyncio.shield(closing)
        except BaseException:
            withdraw_epoch(epoch())
            try:
                await closing
            except BaseException:
                pass
            raise


def release_owner():
    global _OWNER, _GATE, _PROBE_LOCK
    _GATE = None
    _PROBE_LOCK = None
    if _OWNER is not None:
        os.close(_OWNER["fd"])
        os.close(_OWNER["parent"])
        _OWNER = None


async def initialize(db):
    """Add proof fencing and immutable publication order to an existing store."""
    await db.execute("BEGIN IMMEDIATE")
    try:
        columns = {row[1] for row in await db.execute_fetchall("PRAGMA table_info(private_releases)")}
        for name, declaration in (("ready_epoch", "TEXT NOT NULL DEFAULT ''"),
                                  ("ready_until_mono", "REAL"), ("proof_id", "TEXT NOT NULL DEFAULT ''"),
                                  ("publication_nonce", "TEXT NOT NULL DEFAULT ''")):
            if name not in columns:
                await db.execute(f"ALTER TABLE private_releases ADD COLUMN {name} {declaration}")
        await db.execute("""CREATE TABLE IF NOT EXISTS private_readiness_state (
            singleton INTEGER PRIMARY KEY CHECK(singleton=1), epoch TEXT NOT NULL, active INTEGER NOT NULL)""")
        await db.execute("INSERT OR IGNORE INTO private_readiness_state VALUES (1,?,0)", (secrets.token_hex(16),))
        existing_order = bool(await db.execute_fetchall("SELECT name FROM sqlite_master WHERE name='private_release_order' AND type='table'"))
        await db.execute("""CREATE TABLE IF NOT EXISTS private_release_order (
            sequence INTEGER PRIMARY KEY AUTOINCREMENT, release_id TEXT NOT NULL UNIQUE)""")
        if not existing_order:
            await db.execute("""INSERT INTO private_release_order(release_id)
                SELECT id FROM private_releases ORDER BY id""")
        if "publication_nonce" not in columns:
            await db.execute("UPDATE private_releases SET publication_nonce=lower(hex(randomblob(16)))")
        await db.execute("CREATE INDEX IF NOT EXISTS private_peers_expiry ON private_peers(updated_at)")
        await db.commit()
    except BaseException:
        await db.rollback()
        raise


async def publish_order(db, release_id):
    await db.execute("INSERT OR IGNORE INTO private_release_order(release_id) VALUES (?)", (release_id,))
    await db.execute("UPDATE private_releases SET publication_nonce=? WHERE id=? AND publication_nonce=''",
                     (secrets.token_hex(16), release_id))


async def reset(write):
    global _GATE
    require_owner()
    _GATE = Gate(secrets.token_hex(16))
    await recover(write, time.monotonic() + REQUEST_SECONDS)


async def recover(write, deadline):
    global _GATE
    require_owner()
    if _GATE is None or _GATE.dirty:
        gate = Gate(secrets.token_hex(16))
        _GATE = gate
        async with write(deadline=deadline) as db:
            updated = await db.execute("UPDATE private_readiness_state SET epoch=?,active=0 WHERE singleton=1", (gate.epoch,))
            if updated.rowcount != 1:
                raise ValueError("Readiness state unavailable")
            if _GATE is not gate:
                raise ValueError("Superseded readiness recovery")
        if _GATE is not gate:
            raise ValueError("Superseded readiness recovery")
        _GATE = Gate(gate.epoch, dirty=False)
    return _GATE.epoch


async def invalidate(write, epoch=None):
    global _GATE
    if epoch is not None and (_GATE is None or _GATE.epoch != epoch):
        if _GATE is None or not _GATE.dirty:
            return
        gate = _GATE  # Persist the immediate failure fence, never reopen it.
    else:
        gate = Gate(secrets.token_hex(16))
        _GATE = gate  # Withdraw before any await, even if persistence fails.
    try:
        require_owner()
        async with write(deadline=time.monotonic() + 1) as db:
            if _GATE is not gate:
                return
            await db.execute("UPDATE private_readiness_state SET epoch=?,active=0 WHERE singleton=1", (gate.epoch,))
            if _GATE is not gate:
                raise ValueError("Superseded readiness withdrawal")
        if _GATE is gate:
            _GATE = Gate(gate.epoch, dirty=False)
    except Exception:
        # Dirty recovery must durably rotate again before it can activate.
        pass


def predicate(alias="", gate=None):
    """SQL parameters for the exact current epoch and individually fresh proof."""
    gate = capture() if gate is None else gate
    prefix = alias + "." if alias else ""
    if not live(gate):
        return "0", ()
    now, mono = time.time(), time.monotonic()
    clause = (f"{prefix}ready=1 AND {prefix}withdrawn=0 AND {prefix}ready_epoch=? AND "
              f"{prefix}ready_until_mono>? AND {prefix}verified_at>? AND {prefix}verified_at<=? AND "
              "EXISTS(SELECT 1 FROM private_readiness_state s WHERE s.singleton=1 AND s.active=1 AND s.epoch=?)")
    return clause, (gate.epoch, mono, now-settings.private_seed_verification_ttl, now, gate.epoch)


def effective(row, gate):
    return bool(live(gate) and row["ready"] == 1 and row["withdrawn"] == 0
                and row["ready_epoch"] == gate.epoch
                and row["ready_until_mono"] is not None and row["ready_until_mono"] > time.monotonic()
                and row["verified_at"] is not None
                and time.time()-settings.private_seed_verification_ttl < row["verified_at"] <= time.time())


async def matching_proofs(db, rows, gate):
    """One live SELECT checks a bounded response against its original proofs."""
    candidates = [row for row in rows if effective(row, gate)]
    if not candidates:
        return set()
    if len(candidates) > 1000:
        raise ValueError("Readiness response exceeds limit")
    clause, args = predicate(gate=gate)
    values = json.dumps([[row["id"], row["proof_id"], row["publication_nonce"]] for row in candidates])
    current = await db.execute_fetchall("SELECT id,proof_id FROM private_releases WHERE " + clause +
        " AND (id,proof_id,publication_nonce) IN (SELECT json_extract(value,'$[0]'),json_extract(value,'$[1]'),"
        "json_extract(value,'$[2]') FROM json_each(?))", (*args, values))
    return {(row["id"], row["proof_id"]) for row in current} if live(gate) else set()


async def recheck(db, row, gate):
    return (row["id"], row["proof_id"]) in await matching_proofs(db, [row], gate)


def display(rows, gate):
    items = []
    for row in rows:
        item = dict(row)
        if not effective(item, gate):
            item.update(ready=0, verified_at=None)
        for field in _COLUMNS.split(","):
            item.pop(field, None)
        items.append(item)
    return items


def _object(pairs):
    result = {}
    for key, value in pairs:
        if key in result:
            raise ValueError("Duplicate response field")
        result[key] = value
    return result


def _constant(_value):
    raise ValueError("Nonfinite response value")


def parse_status(content, rows):
    torrents = json.loads(content, object_pairs_hook=_object, parse_constant=_constant)
    if not isinstance(torrents, list) or len(torrents) > PAGE:
        raise ValueError("Invalid seeder response")
    requested = {row["info_hash"]: row for row in rows}
    ready, seen = set(), set()
    for torrent in torrents:
        if not isinstance(torrent, dict):
            raise ValueError("Invalid seeder record")
        value = torrent.get("hash")
        if not isinstance(value, str) or value not in requested or value in seen:
            raise ValueError("Unexpected seeder hash")
        seen.add(value)
        if (type(torrent.get("size")) is not int or type(torrent.get("amount_left")) is not int
                or type(torrent.get("progress")) not in {int, float}
                or not math.isfinite(torrent["progress"]) or not 0 <= torrent["progress"] <= 1
                or torrent["amount_left"] < 0 or not isinstance(torrent.get("state"), str)):
            raise ValueError("Invalid seeder state")
        if (torrent["size"] == requested[value]["size"] and torrent["amount_left"] == 0
                and torrent["progress"] == 1 and torrent["state"] in {"uploading", "stalledUP", "forcedUP"}):
            ready.add(value)
    return ready


async def response_bytes(client, method, url, maximum, *, failure_epoch=None, statuses=None, validator=None, **kwargs):
    async with fenced_context(client.stream(method, url, **kwargs), lambda: failure_epoch) as response:
        try:
            if statuses is not None and response.status_code not in statuses:
                raise ValueError("Unexpected seeder status")
            if response.headers.get("content-encoding", "identity").lower() != "identity":
                raise ValueError("Encoded seeder response unsupported")
            raw = bytearray()
            if response.is_stream_consumed:
                # Buffered synthetic transports obey the same cap.
                if len(response.content) > maximum:
                    raise ValueError("Seeder response exceeds limit")
                raw.extend(response.content)
            else:
                async for chunk in response.aiter_raw(65536):
                    if len(raw) + len(chunk) > maximum:
                        raise ValueError("Seeder response exceeds limit")
                    raw.extend(chunk)
            result = httpx.Response(response.status_code, headers=response.headers, content=bytes(raw), request=response.request)
            validated = validator(result) if validator is not None else None
            return (result, validated) if validator is not None else result
        except BaseException:
            # Known failure withdraws before response/socket cleanup can await.
            withdraw_epoch(failure_epoch)
            raise


@asynccontextmanager
async def admission(deadline, operator=False):
    global _PROBE_LOCK, _OPERATORS
    if _PROBE_LOCK is None:
        _PROBE_LOCK = asyncio.Lock()
    if operator and _OPERATORS >= MAX_OPERATORS:
        raise ValueError("Readiness verification queue full")
    if operator:
        _OPERATORS += 1
    acquired = False
    try:
        async with asyncio.timeout_at(deadline):
            await _PROBE_LOCK.acquire()
            acquired = True
            yield
    finally:
        if acquired:
            _PROBE_LOCK.release()
        if operator:
            _OPERATORS -= 1


async def probe(rows, client, write, login_ok, deadline):
    epoch = await recover(write, deadline)
    try:
        return await _probe(rows, client, write, login_ok, deadline, epoch)
    except BaseException:
        withdraw_epoch(epoch)
        raise


async def _probe(rows, client, write, login_ok, deadline, epoch):
    global _GATE
    base = settings.private_seeder_url.rstrip("/")
    if not base:
        raise ValueError("Seeder not configured")
    if not client.cookies:
        def validate_login(response):
            if not login_ok(response, client, base):
                raise ValueError("Seeder authentication failed")
        await response_bytes(client, "POST", base + "/api/v2/auth/login", LOGIN_BYTES,
                             failure_epoch=epoch, statuses={200, 204}, validator=validate_login,
                             data={"username": settings.private_seeder_username, "password": settings.private_seeder_password})
    wall, mono = time.time(), time.monotonic()
    _, ready = await response_bytes(client, "GET", base + "/api/v2/torrents/info", INFO_BYTES,
                                    failure_epoch=epoch, statuses={200}, validator=lambda response: parse_status(response.content, rows),
                                    params={"hashes": "|".join(row["info_hash"] for row in rows)})
    expires = mono + settings.private_seed_verification_ttl
    proof = secrets.token_hex(16)
    async with write(deadline=deadline, failure_epoch=epoch) as db:
        state = await db.execute_fetchall("SELECT epoch FROM private_readiness_state WHERE singleton=1")
        if not state or state[0]["epoch"] != epoch:
            raise ValueError("Superseded persisted readiness epoch")
        if not _GATE or _GATE.epoch != epoch or _GATE.dirty or time.monotonic() >= expires:
            raise ValueError("Superseded readiness proof")
        await db.executemany("""UPDATE private_releases SET ready=?,verified_at=?,ready_epoch=?,ready_until_mono=?,proof_id=?
            WHERE id=? AND info_hash=? AND size=? AND publication_nonce=? AND withdrawn=0""",
            [(int(row["info_hash"] in ready), wall if row["info_hash"] in ready else None, epoch,
              expires if row["info_hash"] in ready else None, proof, row["id"],
              row["info_hash"], row["size"], row["publication_nonce"]) for row in rows])
        applied = await db.execute_fetchall("SELECT info_hash FROM private_releases WHERE id IN (SELECT value FROM json_each(?)) AND ready=1 AND ready_epoch=? AND proof_id=?",
                                            (json.dumps([row["id"] for row in rows]), epoch, proof))
        ready.intersection_update(row["info_hash"] for row in applied)
        await db.execute("UPDATE private_readiness_state SET active=1 WHERE singleton=1 AND epoch=?", (epoch,))
        if not _GATE or _GATE.epoch != epoch or _GATE.dirty or time.monotonic() >= expires:
            raise ValueError("Superseded readiness proof")
    if not _GATE or _GATE.epoch != epoch or _GATE.dirty:
        raise ValueError("Superseded readiness proof")
    _GATE = Gate(epoch, active=True, dirty=False)
    return ready


def period():
    return min(300, settings.private_seed_verification_ttl / 3)


async def refresh(write, login_ok):
    gate = capture()
    epoch = gate.epoch if gate and not gate.dirty else None
    deadline = time.monotonic() + period()
    try:
        async with asyncio.timeout_at(deadline):
            async with fenced_context(aiosqlite.connect(settings.db_path, timeout=.25), lambda: epoch) as db:
                db.row_factory = aiosqlite.Row
                await db.execute("PRAGMA query_only=ON")
                async with fenced_context(db.execute("SELECT coalesce(max(sequence),0) FROM private_release_order"), lambda: epoch) as cursor:
                    cutoff = (await cursor.fetchone())[0]
                after = 0
                async with fenced_context(httpx.AsyncClient(timeout=REQUEST_SECONDS, follow_redirects=False, trust_env=False,
                                            headers={"Referer": settings.private_seeder_url.rstrip("/") + "/",
                                                     "Accept-Encoding": "identity"}), lambda: epoch) as client:
                    while after < cutoff:
                        async with fenced_context(db.execute("""SELECT o.sequence,r.""" + _PROJECTION.replace(",", ",r.") + """
                            FROM private_release_order o JOIN private_releases r ON r.id=o.release_id
                            WHERE o.sequence>? AND o.sequence<=? AND r.withdrawn=0 ORDER BY o.sequence LIMIT ?""",
                            (after, cutoff, PAGE)), lambda: epoch) as cursor:
                            rows = [dict(row) for row in await cursor.fetchall()]
                        if not rows:
                            break
                        async with admission(min(deadline, time.monotonic()+REQUEST_SECONDS)):
                            current = await recover(write, deadline)
                            if epoch is not None and current != epoch:
                                raise ValueError("Superseded readiness sweep")
                            epoch = current
                            await probe(rows, client, write, login_ok, deadline)
                        after = rows[-1]["sequence"]
                        await asyncio.sleep(0)
    except asyncio.CancelledError:
        await invalidate(write, epoch)
        raise
    except Exception:
        await invalidate(write, epoch)
        return False
    return True


async def verify(db, release_id, write, login_ok):
    rows = await db.execute_fetchall("SELECT " + _PROJECTION + " FROM private_releases WHERE id=? AND withdrawn=0", (release_id,))
    if not rows:
        return None
    epoch = capture().epoch if capture() else None
    deadline = time.monotonic() + REQUEST_SECONDS
    try:
        async with admission(deadline, operator=True):
            epoch = await recover(write, deadline)
            async with fenced_context(httpx.AsyncClient(timeout=REQUEST_SECONDS, follow_redirects=False, trust_env=False,
                                        headers={"Referer": settings.private_seeder_url.rstrip("/") + "/",
                                                 "Accept-Encoding": "identity"}), lambda: epoch) as client:
                ready = await probe([dict(rows[0])], client, write, login_ok, deadline)
                return rows[0]["info_hash"] in ready
    except asyncio.CancelledError:
        await invalidate(write, epoch)
        raise
    except Exception:
        await invalidate(write, epoch)
        return False

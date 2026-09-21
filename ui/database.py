from __future__ import annotations
import asyncio
import hashlib
import time
from contextlib import asynccontextmanager
import aiosqlite
from config import settings, MUTABLE_FIELDS, SENSITIVE_FIELDS

_REDACTED_AUDIT_VALUE = "[redacted]"


def _audit_value(key: str, value: str | None) -> str | None:
    if value is None:
        return None
    return _REDACTED_AUDIT_VALUE if key in SENSITIVE_FIELDS else value

# One connection carries every write (single-writer, so commits stay serialized
# and consistent). Reads that fan out — the ~60-poster grid hitting poster_cache,
# tmdb_meta_cache lookups — go through a small pool of read-only connections so
# they run concurrently instead of queueing behind an in-flight write commit on
# the shared connection. WAL lets readers and the writer proceed in parallel.
_db: aiosqlite.Connection | None = None

_READ_POOL_SIZE = 3
_read_pool: list[aiosqlite.Connection] = []
_read_pool_q: asyncio.Queue | None = None
_read_pool_lock: asyncio.Lock | None = None


async def get_db() -> aiosqlite.Connection:
    global _db
    if _db is None:
        _db = await aiosqlite.connect(settings.db_path)
        _db.row_factory = aiosqlite.Row
        await _db.execute("PRAGMA journal_mode=WAL")
        await _init_tables(_db)
    return _db


async def _ensure_read_pool() -> asyncio.Queue:
    """Lazily build the read-only connection pool (WAL, query_only guard)."""
    global _read_pool_q, _read_pool_lock
    if _read_pool_q is not None:
        return _read_pool_q
    if _read_pool_lock is None:
        _read_pool_lock = asyncio.Lock()
    async with _read_pool_lock:
        if _read_pool_q is not None:
            return _read_pool_q
        # Ensure the schema/WAL exist before opening read connections.
        await get_db()
        # Open into a local list first; only publish to the module pool once ALL
        # connections succeed, closing any partials on failure — otherwise a
        # mid-build error would orphan the already-opened connections and the
        # next call would open a fresh batch on top of them.
        conns: list[aiosqlite.Connection] = []
        try:
            for _ in range(_READ_POOL_SIZE):
                conn = await aiosqlite.connect(settings.db_path)
                conn.row_factory = aiosqlite.Row
                await conn.execute("PRAGMA journal_mode=WAL")
                await conn.execute("PRAGMA query_only=ON")
                conns.append(conn)
        except Exception:
            for c in conns:
                try:
                    await c.close()
                except Exception:
                    pass
            raise
        q: asyncio.Queue = asyncio.Queue()
        for conn in conns:
            _read_pool.append(conn)
            q.put_nowait(conn)
        _read_pool_q = q
        return q


@asynccontextmanager
async def read_conn():
    """Borrow a read-only connection from the WAL read pool for a SELECT.

    Falls back to the shared write connection if the pool cannot be built (e.g.
    called before the DB is initialised). Never write through this — the pooled
    connections are opened `query_only=ON`."""
    try:
        q = await _ensure_read_pool()
    except Exception:
        yield await get_db()
        return
    conn = await q.get()
    try:
        yield conn
    finally:
        q.put_nowait(conn)


async def close_all() -> None:
    """Close the write connection and every pooled read connection so aiosqlite's
    worker threads exit cleanly on app shutdown."""
    global _db, _read_pool_q
    for conn in _read_pool:
        try:
            await conn.close()
        except Exception:
            pass
    _read_pool.clear()
    _read_pool_q = None
    if _db is not None:
        try:
            await _db.close()
        except Exception:
            pass
        _db = None


async def bump_block_phrase_hits(counts: dict[int, int]) -> None:
    """Apply block-phrase hit increments in one transaction + one commit.

    Callers accumulate per-request matches into a Counter and flush once here,
    instead of a UPDATE+commit per matched item — the per-commit fsync was the
    real cost, and a request can match the same phrase across dozens of items."""
    if not counts:
        return
    db = await get_db()
    await db.executemany(
        "UPDATE block_phrases SET hits = hits + ? WHERE id = ?",
        [(int(n), int(pid)) for pid, n in counts.items() if n],
    )
    await db.commit()


async def _init_tables(db: aiosqlite.Connection):
    await db.executescript("""
        CREATE TABLE IF NOT EXISTS settings_overrides (
            key TEXT PRIMARY KEY,
            value TEXT NOT NULL,
            updated_at REAL NOT NULL
        );
        CREATE TABLE IF NOT EXISTS audit_log (
            id INTEGER PRIMARY KEY AUTOINCREMENT,
            key TEXT NOT NULL,
            old_value TEXT,
            new_value TEXT,
            actor TEXT NOT NULL DEFAULT 'operator',
            timestamp REAL NOT NULL
        );
        CREATE TABLE IF NOT EXISTS wants (
            id INTEGER PRIMARY KEY AUTOINCREMENT,
            title TEXT NOT NULL,
            content_type TEXT NOT NULL DEFAULT 'any',
            query TEXT NOT NULL,
            status TEXT NOT NULL DEFAULT 'active',
            priority INTEGER NOT NULL DEFAULT 50,
            created_at REAL NOT NULL,
            updated_at REAL NOT NULL,
            notes TEXT DEFAULT '',
            source TEXT NOT NULL DEFAULT 'manual',
            source_id TEXT NOT NULL DEFAULT ''
        );
        CREATE TABLE IF NOT EXISTS poster_cache (
            tmdb_id TEXT PRIMARY KEY,
            poster_url TEXT,
            title TEXT,
            year TEXT,
            fetched_at REAL NOT NULL
        );
        CREATE TABLE IF NOT EXISTS notifications (
            id INTEGER PRIMARY KEY AUTOINCREMENT,
            level TEXT NOT NULL DEFAULT 'info',
            title TEXT NOT NULL,
            message TEXT NOT NULL DEFAULT '',
            read INTEGER NOT NULL DEFAULT 0,
            created_at REAL NOT NULL
        );
        CREATE TABLE IF NOT EXISTS block_phrases (
            id INTEGER PRIMARY KEY AUTOINCREMENT,
            pattern TEXT NOT NULL UNIQUE,
            scope TEXT NOT NULL DEFAULT 'title',
            note TEXT DEFAULT '',
            hits INTEGER NOT NULL DEFAULT 0,
            created_at REAL NOT NULL
        );
        -- Enriched TMDB detail/season payloads for the public library detail
        -- page (backdrop, overview, genres, cast, season/episode catalog).
        -- Keyed by an opaque cache key ("<media_type>:<id>" or
        -- "<id>:season:<n>"); value is the normalised JSON blob.
        CREATE TABLE IF NOT EXISTS tmdb_meta_cache (
            cache_key TEXT PRIMARY KEY,
            payload TEXT NOT NULL,
            fetched_at REAL NOT NULL
        );
        CREATE TABLE IF NOT EXISTS user_api_keys (
            id INTEGER PRIMARY KEY AUTOINCREMENT,
            user_id TEXT NOT NULL,
            name TEXT NOT NULL DEFAULT 'default',
            key_hash TEXT NOT NULL UNIQUE,
            key_prefix TEXT NOT NULL,
            created_at REAL NOT NULL,
            last_used_at REAL,
            revoked_at REAL
        );
        CREATE INDEX IF NOT EXISTS idx_user_api_keys_user_active
            ON user_api_keys(user_id, revoked_at, created_at);
        CREATE INDEX IF NOT EXISTS idx_user_api_keys_hash_active
            ON user_api_keys(key_hash, revoked_at);
    """)
    # Scrub legacy rows that predate server-side redaction. The marker is
    # idempotent, and future writes below are redacted before insertion.
    if SENSITIVE_FIELDS:
        placeholders = ",".join("?" for _ in SENSITIVE_FIELDS)
        await db.execute(
            f"""UPDATE audit_log
                SET old_value = CASE WHEN old_value IS NULL THEN NULL ELSE ? END,
                    new_value = CASE WHEN new_value IS NULL THEN NULL ELSE ? END
                WHERE key IN ({placeholders})""",
            (_REDACTED_AUDIT_VALUE, _REDACTED_AUDIT_VALUE, *sorted(SENSITIVE_FIELDS)),
        )
    # Overrides are deny-by-default: retain only the explicit mutable allowlist.
    # This also removes arbitrary or renamed legacy keys that are no longer
    # declared Settings fields, not just the startup-only fields known today.
    if MUTABLE_FIELDS:
        placeholders = ",".join("?" for _ in MUTABLE_FIELDS)
        await db.execute(
            f"DELETE FROM settings_overrides WHERE key NOT IN ({placeholders})",
            tuple(sorted(MUTABLE_FIELDS)),
        )
    else:
        await db.execute("DELETE FROM settings_overrides")
    await db.commit()
    # Migrations for columns added after initial deploy
    for stmt in [
        "ALTER TABLE wants ADD COLUMN source TEXT NOT NULL DEFAULT 'manual'",
        "ALTER TABLE wants ADD COLUMN source_id TEXT NOT NULL DEFAULT ''",
    ]:
        try:
            await db.execute(stmt)
            await db.commit()
        except Exception:
            pass  # column already exists


async def get_override(key: str) -> str | None:
    db = await get_db()
    row = await db.execute_fetchall(
        "SELECT value FROM settings_overrides WHERE key = ?", (key,)
    )
    return row[0][0] if row else None


async def set_override(key: str, value: str, actor: str) -> dict:
    db = await get_db()
    old_rows = await db.execute_fetchall(
        "SELECT value FROM settings_overrides WHERE key = ?", (key,)
    )
    old_value = old_rows[0][0] if old_rows else None
    now = time.time()
    await db.execute(
        "INSERT OR REPLACE INTO settings_overrides (key, value, updated_at) VALUES (?, ?, ?)",
        (key, value, now),
    )
    await db.execute(
        "INSERT INTO audit_log (key, old_value, new_value, actor, timestamp) VALUES (?, ?, ?, ?, ?)",
        (key, _audit_value(key, old_value), _audit_value(key, value), actor, now),
    )
    await db.commit()
    return {
        "key": key,
        "old": _audit_value(key, old_value),
        "new": _audit_value(key, value),
        "actor": actor,
        "at": now,
    }


async def get_all_overrides() -> dict[str, str]:
    db = await get_db()
    rows = await db.execute_fetchall("SELECT key, value FROM settings_overrides")
    return {r[0]: r[1] for r in rows}


async def get_audit_log(limit: int = 100) -> list[dict]:
    db = await get_db()
    rows = await db.execute_fetchall(
        "SELECT id, key, old_value, new_value, actor, timestamp FROM audit_log ORDER BY timestamp DESC LIMIT ?",
        (limit,),
    )
    return [
        {
            "id": r[0],
            "key": r[1],
            # Defense in depth for a database created by an older build that
            # has not yet run the startup scrub migration.
            "old": _audit_value(r[1], r[2]),
            "new": _audit_value(r[1], r[3]),
            "actor": r[4],
            "at": r[5],
        }
        for r in rows
    ]


async def delete_override(key: str, actor: str) -> bool:
    db = await get_db()
    old_rows = await db.execute_fetchall(
        "SELECT value FROM settings_overrides WHERE key = ?", (key,)
    )
    if not old_rows:
        return False
    now = time.time()
    await db.execute("DELETE FROM settings_overrides WHERE key = ?", (key,))
    await db.execute(
        "INSERT INTO audit_log (key, old_value, new_value, actor, timestamp) VALUES (?, ?, ?, ?, ?)",
        (key, _audit_value(key, old_rows[0][0]), None, actor, now),
    )
    await db.commit()
    return True


def _safe_api_key_row(row) -> dict | None:
    if not row:
        return None
    return {
        "id": row["id"],
        "user_id": row["user_id"],
        "name": row["name"],
        "key_prefix": row["key_prefix"],
        "created_at": row["created_at"],
        "last_used_at": row["last_used_at"],
        "revoked_at": row["revoked_at"],
    }


def _hash_user_api_key(api_key: str) -> str:
    return hashlib.sha256(api_key.encode("utf-8")).hexdigest()


async def get_user_api_key(user_id: str) -> dict | None:
    db = await get_db()
    rows = await db.execute_fetchall(
        """
        SELECT id, user_id, name, key_prefix, created_at, last_used_at, revoked_at
        FROM user_api_keys
        WHERE user_id = ? AND revoked_at IS NULL
        ORDER BY created_at DESC, id DESC
        LIMIT 1
        """,
        (user_id,),
    )
    return _safe_api_key_row(rows[0]) if rows else None


async def create_user_api_key(
    user_id: str,
    key_hash: str,
    key_prefix: str,
    name: str = "default",
) -> dict:
    db = await get_db()
    now = time.time()
    await db.execute(
        "UPDATE user_api_keys SET revoked_at = ? WHERE user_id = ? AND revoked_at IS NULL",
        (now, user_id),
    )
    cur = await db.execute(
        """
        INSERT INTO user_api_keys (user_id, name, key_hash, key_prefix, created_at)
        VALUES (?, ?, ?, ?, ?)
        """,
        (user_id, name or "default", key_hash, key_prefix, now),
    )
    await db.commit()
    return {
        "id": cur.lastrowid,
        "user_id": user_id,
        "name": name or "default",
        "key_prefix": key_prefix,
        "created_at": now,
        "last_used_at": None,
        "revoked_at": None,
    }


async def revoke_user_api_key(user_id: str) -> bool:
    db = await get_db()
    now = time.time()
    cur = await db.execute(
        "UPDATE user_api_keys SET revoked_at = ? WHERE user_id = ? AND revoked_at IS NULL",
        (now, user_id),
    )
    await db.commit()
    return bool(cur.rowcount)


async def lookup_user_api_key(key_hash: str) -> dict | None:
    db = await get_db()
    rows = await db.execute_fetchall(
        """
        SELECT id, user_id, name, key_prefix, created_at, last_used_at, revoked_at
        FROM user_api_keys
        WHERE key_hash = ? AND revoked_at IS NULL
        LIMIT 1
        """,
        (key_hash,),
    )
    return _safe_api_key_row(rows[0]) if rows else None


async def touch_user_api_key(key_id: int) -> None:
    db = await get_db()
    await db.execute(
        "UPDATE user_api_keys SET last_used_at = ? WHERE id = ? AND revoked_at IS NULL",
        (time.time(), key_id),
    )
    await db.commit()

"""Tests for the database layer's WAL read pool and batched block-phrase hits.

The DB is redirected to a throwaway temp file by conftest before the app opens
a connection, so these run against a real (empty) SQLite file.
"""
from __future__ import annotations

import asyncio

import database
from config import FROZEN_OVERRIDE_FIELDS, MUTABLE_FIELDS


def test_read_conn_serves_reads_and_rejects_writes():
    async def _run():
        # A pooled read connection answers SELECTs.
        async with database.read_conn() as db:
            rows = await db.execute_fetchall("SELECT 1")
        assert rows[0][0] == 1

        # query_only=ON: the pooled connection refuses writes.
        wrote = True
        async with database.read_conn() as db:
            try:
                await db.execute("CREATE TABLE _should_fail (a INTEGER)")
                await db.commit()
            except Exception:
                wrote = False
        assert wrote is False

    asyncio.run(_run())


def test_read_pool_hands_out_distinct_connections_concurrently():
    async def _run():
        # Borrow every pooled connection at once; they must be distinct objects,
        # proving reads can run in parallel rather than serializing on one conn.
        seen = []

        async def _hold():
            async with database.read_conn() as db:
                seen.append(id(db))
                await asyncio.sleep(0.02)

        await asyncio.gather(*(_hold() for _ in range(database._READ_POOL_SIZE)))
        assert len(set(seen)) == database._READ_POOL_SIZE

    asyncio.run(_run())


def test_bump_block_phrase_hits_batches_counts():
    async def _run():
        db = await database.get_db()
        await db.execute("DELETE FROM block_phrases")
        cur = await db.execute(
            "INSERT INTO block_phrases (pattern, scope, note, hits, created_at) "
            "VALUES ('p', 'title', '', 0, 0)"
        )
        await db.commit()
        pid = cur.lastrowid

        # A single flush applies the accumulated count.
        await database.bump_block_phrase_hits({pid: 5})
        rows = await db.execute_fetchall(
            "SELECT hits FROM block_phrases WHERE id = ?", (pid,)
        )
        assert rows[0][0] == 5

        # Empty / no-op flush is safe.
        await database.bump_block_phrase_hits({})
        await database.bump_block_phrase_hits({pid: 0})
        rows = await db.execute_fetchall(
            "SELECT hits FROM block_phrases WHERE id = ?", (pid,)
        )
        assert rows[0][0] == 5

    asyncio.run(_run())


def test_startup_migration_scrubs_legacy_secret_audit_values():
    async def _run():
        db = await database.get_db()
        await db.execute("DELETE FROM audit_log")
        await db.execute(
            "INSERT INTO audit_log (key, old_value, new_value, actor, timestamp) "
            "VALUES ('tmdb_api_key', 'legacy-old', 'legacy-new', 'legacy', 1)"
        )
        await db.commit()

        await database._init_tables(db)
        rows = await db.execute_fetchall(
            "SELECT old_value, new_value FROM audit_log WHERE key = 'tmdb_api_key'"
        )
        assert tuple(rows[0]) == ("[redacted]", "[redacted]")

    asyncio.run(_run())


def test_startup_migration_purges_nonmutable_overrides_and_preserves_mutable_ones():
    async def _run():
        db = await database.get_db()
        await db.execute("DELETE FROM settings_overrides")

        # The two upstream rows are the persistent-SSRF regression this
        # migration closes. Seed every other startup-only setting too so future
        # trust-boundary fields remain deny-by-default.
        assert {
            "bitagent_torznab_url",
            "bitagent_graphql_url",
            "bitagent_metrics_url",
            "trusted_proxy_cidrs",
            "proxy_auth_secret",
            "operator_hosts",
            "operator_roles",
        } <= FROZEN_OVERRIDE_FIELDS
        frozen_rows = [
            (key, f"legacy-{key}", 1.0)
            for key in sorted(FROZEN_OVERRIDE_FIELDS)
        ]
        unknown_rows = [
            ("renamed_legacy_proxy_mode", "legacy-unknown-value", 1.0),
        ]
        mutable_rows = [
            (key, f"current-{key}", 2.0)
            for key in sorted(MUTABLE_FIELDS)
        ]
        await db.executemany(
            "INSERT INTO settings_overrides (key, value, updated_at) VALUES (?, ?, ?)",
            frozen_rows + unknown_rows + mutable_rows,
        )
        await db.commit()

        await database._init_tables(db)
        rows = await db.execute_fetchall(
            "SELECT key, value FROM settings_overrides ORDER BY key"
        )
        persisted = {row[0]: row[1] for row in rows}
        assert "renamed_legacy_proxy_mode" not in persisted
        assert persisted == {
            key: value for key, value, _ in mutable_rows
        }

        await db.execute("DELETE FROM settings_overrides")
        await db.commit()

    asyncio.run(_run())

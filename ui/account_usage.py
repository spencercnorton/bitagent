"""Durable per-account activity totals, without an acquisition history.

Magnet grabs are successful UI copy/open/export actions reported by the browser.
API searches are successful Torznab searches observed by the authenticated proxy.
Neither event establishes that a torrent client downloaded or seeded anything.
"""
from __future__ import annotations

import time

from database import get_db, read_conn

_GRAB_COLUMNS = {
    "copy": "magnet_copies",
    "open": "magnet_opens",
    "export": "magnet_exports",
}


def _user_id(user_id: str) -> str:
    if not isinstance(user_id, str) or not user_id:
        raise ValueError("An authenticated account identifier is required")
    return user_id


async def get_account_usage(user_id: str) -> dict:
    """Read only this account's totals; first use establishes the tracking epoch."""
    user_id = _user_id(user_id)
    async with read_conn() as db:
        rows = await db.execute_fetchall(
            "SELECT * FROM account_usage WHERE user_id = ?", (user_id,)
        )
    if not rows:
        db = await get_db()
        await db.execute(
            "INSERT OR IGNORE INTO account_usage (user_id, tracking_since) VALUES (?, ?)",
            (user_id, time.time()),
        )
        await db.commit()
        async with read_conn() as db:
            rows = await db.execute_fetchall(
                "SELECT * FROM account_usage WHERE user_id = ?", (user_id,)
            )
    row = rows[0]
    return {
        "grabs": row["grabs"],
        "apiSearches": row["api_searches"],
        "magnetCopies": row["magnet_copies"],
        "magnetOpens": row["magnet_opens"],
        "magnetExports": row["magnet_exports"],
        "trackingSince": row["tracking_since"],
        "downloadedBytes": None,
        "uploadedBytes": None,
        "hitAndRuns": None,
        "source": "account-aggregate",
        "grabsSource": "magnet-link-actions",
        "apiSearchesSource": "successful-torznab-searches",
        "transferStatus": "unavailable",
    }


async def record_magnet_grab(user_id: str, count: int, action: str) -> dict:
    """Atomically add link actions, never file sizes or reported transfer bytes."""
    user_id = _user_id(user_id)
    if type(count) is not int or not 1 <= count <= 1000:
        raise ValueError("Magnet count must be an integer from 1 to 1000")
    if not isinstance(action, str) or action not in _GRAB_COLUMNS:
        raise ValueError("Unknown magnet action")
    # Column names come exclusively from the fixed allowlist above.
    column = _GRAB_COLUMNS[action]
    db = await get_db()
    await db.execute(
        f"""INSERT INTO account_usage (user_id, tracking_since, grabs, {column})
            VALUES (?, ?, ?, ?)
            ON CONFLICT(user_id) DO UPDATE SET
                grabs = account_usage.grabs + excluded.grabs,
                {column} = account_usage.{column} + excluded.{column}""",
        (user_id, time.time(), count, count),
    )
    await db.commit()
    return await get_account_usage(user_id)


async def record_api_search(user_id: str) -> None:
    """Record one successful Torznab GET search; totals survive API-key rotation."""
    user_id = _user_id(user_id)
    db = await get_db()
    await db.execute(
        """INSERT INTO account_usage (user_id, tracking_since, api_searches)
            VALUES (?, ?, 1)
            ON CONFLICT(user_id) DO UPDATE SET
                api_searches = account_usage.api_searches + 1""",
        (user_id, time.time()),
    )
    await db.commit()

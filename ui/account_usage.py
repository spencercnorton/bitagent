"""Durable account action totals, without a torrent acquisition history.

Browser receipts describe copied links, open requests and export requests.
Torznab searches are observed successful requests. Neither proves a transfer.
"""
from __future__ import annotations

from contextlib import asynccontextmanager
from datetime import datetime, timedelta, timezone
import time
from uuid import UUID

import aiosqlite

from config import settings
from database import get_db

_GRAB_COLUMNS = {"copy": "magnet_copies", "open": "magnet_opens", "export": "magnet_exports"}
_COUNTERS = {
    "grabs": "grabs", "apiSearches": "api_searches", "magnetCopies": "magnet_copies",
    "magnetOpens": "magnet_opens", "magnetExports": "magnet_exports",
}


class UsageEventConflict(ValueError):
    """An existing receipt cannot be reused for a different action."""


def personal_account(user_id: str) -> bool:
    return isinstance(user_id, str) and bool(user_id.strip()) and user_id not in {"anonymous", "api-client"}


def _user_id(user_id: str) -> str:
    if not personal_account(user_id):
        raise ValueError("A named authenticated account is required")
    return user_id


def utc_timestamp(timestamp: float) -> str:
    return datetime.fromtimestamp(timestamp, timezone.utc).isoformat().replace("+00:00", "Z")


def _now() -> float:
    return time.time()


@asynccontextmanager
async def account_write():
    """Isolate multi-statement receipts/preferences from unrelated app commits."""
    await get_db()  # Ensure the additive schema exists before opening a writer.
    async with aiosqlite.connect(settings.db_path, timeout=5) as db:
        db.row_factory = aiosqlite.Row
        await db.execute("BEGIN IMMEDIATE")
        try:
            yield db
            await db.commit()
        except BaseException:
            await db.rollback()
            raise


async def _ensure_account(db, user_id: str, now: float):
    await db.execute(
        "INSERT OR IGNORE INTO account_usage (user_id, tracking_since, period_tracking_since) VALUES (?, ?, ?)",
        (user_id, now, now),
    )
    await db.execute(
        "UPDATE account_usage SET period_tracking_since=? WHERE user_id=? AND period_tracking_since IS NULL",
        (now, user_id),
    )


async def _daily_increment(db, user_id: str, now: float, increments: dict[str, int]):
    # Every interpolated identifier comes from this module's fixed mappings.
    columns = list(increments)
    day = datetime.fromtimestamp(now, timezone.utc).date().isoformat()
    await db.execute(
        f"INSERT INTO account_usage_daily (user_id, day, {','.join(columns)}) "
        f"VALUES ({','.join('?' for _ in range(2 + len(columns)))}) "
        "ON CONFLICT(user_id,day) DO UPDATE SET "
        + ",".join(f"{column}={column}+excluded.{column}" for column in columns),
        (user_id, day, *increments.values()),
    )


async def _snapshot(db, user_id: str, now: float) -> dict:
    row = (await db.execute_fetchall("SELECT * FROM account_usage WHERE user_id=?", (user_id,)))[0]
    today = datetime.fromtimestamp(now, timezone.utc).replace(hour=0, minute=0, second=0, microsecond=0)
    cutoff = (today - timedelta(days=29)).date().isoformat()
    await db.execute("DELETE FROM account_usage_daily WHERE user_id=? AND day<?", (user_id, cutoff))
    rows = await db.execute_fetchall(
        "SELECT * FROM account_usage_daily WHERE user_id=? AND day>=? AND day<=? ORDER BY day",
        (user_id, cutoff, today.date().isoformat()),
    )
    periods = {}
    for days in (7, 30):
        start = today - timedelta(days=days - 1)
        selected = [daily for daily in rows if daily["day"] >= start.date().isoformat()]
        periods[f"last{days}Days"] = {
            "days": days, "windowStart": utc_timestamp(start.timestamp()), "windowEnd": utc_timestamp(now),
            "trackingSince": row["period_tracking_since"],
            "complete": row["period_tracking_since"] <= start.timestamp(),
            **{key: sum(daily[column] for daily in selected) for key, column in _COUNTERS.items()},
        }
    return {
        "accountId": user_id, "available": True, "status": "available", "revision": row["revision"],
        "observedAt": utc_timestamp(now), "trackingSince": row["tracking_since"],
        "periodTrackingSince": row["period_tracking_since"], "periods": periods,
        **{key: row[column] for key, column in _COUNTERS.items()},
        "downloadedBytes": None, "uploadedBytes": None, "hitAndRuns": None,
        "source": "account-aggregate", "grabsSource": "magnet-link-actions",
        "apiSearchesSource": "successful-torznab-searches", "transferStatus": "unavailable",
    }


async def get_account_usage(user_id: str) -> dict:
    if not personal_account(user_id):
        return {
            "accountId": user_id, "available": False,
            "status": "shared-credential" if user_id == "api-client" else "anonymous", "revision": 0,
            "observedAt": utc_timestamp(_now()), "trackingSince": None, "periodTrackingSince": None,
            "periods": None, **{key: None for key in _COUNTERS},
            "downloadedBytes": None, "uploadedBytes": None, "hitAndRuns": None,
            "source": "unavailable", "grabsSource": "magnet-link-actions",
            "apiSearchesSource": "successful-torznab-searches", "transferStatus": "unavailable",
        }
    async with account_write() as db:
        now = _now()
        await _ensure_account(db, user_id, now)
        return await _snapshot(db, user_id, now)


async def record_magnet_grab(user_id: str, count: int, action: str, event_id: UUID | str | None = None) -> dict:
    user_id = _user_id(user_id)
    if type(count) is not int or not 1 <= count <= 1000:
        raise ValueError("Magnet count must be an integer from 1 to 1000")
    if not isinstance(action, str) or action not in _GRAB_COLUMNS:
        raise ValueError("Unknown magnet action")
    normalized_event = str(UUID(str(event_id))) if event_id is not None else None
    async with account_write() as db:
        now = _now()
        await _ensure_account(db, user_id, now)
        duplicate = False
        if normalized_event:
            rows = await db.execute_fetchall(
                "SELECT action,count FROM account_usage_events WHERE user_id=? AND event_id=?",
                (user_id, normalized_event),
            )
            if rows:
                if rows[0]["action"] != action or rows[0]["count"] != count:
                    raise UsageEventConflict("Event ID already records a different action")
                duplicate = True
            else:
                await db.execute(
                    "INSERT INTO account_usage_events (user_id,event_id,action,count) VALUES (?,?,?,?)",
                    (user_id, normalized_event, action, count),
                )
        if not duplicate:
            column = _GRAB_COLUMNS[action]
            await db.execute(
                f"UPDATE account_usage SET grabs=grabs+?, {column}={column}+?, revision=revision+1 WHERE user_id=?",
                (count, count, user_id),
            )
            await _daily_increment(db, user_id, now, {"grabs": count, column: count})
        usage = await _snapshot(db, user_id, now)
        usage["receipt"] = {"eventId": normalized_event, "duplicate": duplicate, "recorded": True}
        return usage


async def record_api_search(user_id: str) -> None:
    user_id = _user_id(user_id)
    async with account_write() as db:
        now = _now()
        await _ensure_account(db, user_id, now)
        await db.execute(
            "UPDATE account_usage SET api_searches=api_searches+1, revision=revision+1 WHERE user_id=?", (user_id,)
        )
        await _daily_increment(db, user_id, now, {"api_searches": 1})
        # Bound daily history even if this user never opens the account panel.
        await _snapshot(db, user_id, now)

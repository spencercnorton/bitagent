"""Landing-page caches: the public banner counts and the home-shelf rows.

Both are viewer-independent and expensive at the core (measured 2026-09-19: the
two-alias stats document 14.3 s, a 400-row shelf query 0.5-0.8 s), so a cold
visit must never wait on them past the edge timeout and concurrent visitors
must share one upstream call.
"""
from __future__ import annotations

import asyncio
import concurrent.futures
import time

import pytest
from fastapi import HTTPException

import app as app_module
import config
import graphql_client as gql

PUBLIC = {"host": "library.example.org"}
SNAPSHOT = {"available": True, "totalReleases": 10, "releasesAddedLast7Days": 2}
SHELF = {"content_types": "movie,tv_show", "order": "seeders",
         "hide_unmapped": "true", "limit": 100, "offset": 0}
SEARCH = {"data": {"torrentContent": {"search": {"totalCount": 0, "items": [], "aggregations": {}}}}}


@pytest.fixture(autouse=True)
def _open_auth():
    config.settings.require_auth = False  # restored by conftest _restore_settings


def _stale(monkeypatch, snapshot):
    stale_at = time.monotonic() - app_module._LIBRARY_STATS_CACHE_TTL_SECONDS - 1
    monkeypatch.setattr(app_module, "_LIBRARY_STATS_CACHE", (stale_at, snapshot))


# ── /api/library/stats ─────────────────────────────────────────────────────

def test_library_stats_stale_snapshot_is_served_while_refresh_runs(monkeypatch):
    calls = 0

    async def scenario():
        nonlocal calls
        release = asyncio.Event()

        async def fake_collect():
            nonlocal calls
            calls += 1
            await release.wait()
            return {**SNAPSHOT, "totalReleases": 11}

        monkeypatch.setattr(app_module, "_collect_library_stats", fake_collect)
        _stale(monkeypatch, SNAPSHOT)
        first = await app_module._get_library_stats()   # stale: served now, refresh started
        second = await app_module._get_library_stats()  # refresh running: same snapshot, no 2nd
        release.set()
        await app_module._LIBRARY_STATS_INFLIGHT
        await asyncio.sleep(0)  # done-callback publishes
        third = await app_module._get_library_stats()
        return first, second, third

    first, second, third = asyncio.run(scenario())
    assert first == second == SNAPSHOT
    assert third["totalReleases"] == 11
    assert calls == 1


def test_library_stats_failed_refresh_keeps_the_snapshot_and_backs_off(monkeypatch):
    calls = 0

    async def scenario():
        async def fake_collect():
            nonlocal calls
            calls += 1
            raise HTTPException(502, "Library statistics unavailable")

        monkeypatch.setattr(app_module, "_collect_library_stats", fake_collect)
        _stale(monkeypatch, SNAPSHOT)
        first = await app_module._get_library_stats()  # stale: one refresh starts ...
        await asyncio.sleep(0)                          # ... runs and fails ...
        await asyncio.sleep(0)                          # ... and the callback records the backoff
        within = [await app_module._get_library_stats() for _ in range(5)]
        await asyncio.sleep(0)
        await asyncio.sleep(0)
        calls_within, delay_after_one = calls, app_module._LIBRARY_STATS_RETRY_DELAY
        # The window passes: exactly one more attempt, and the backoff doubles.
        monkeypatch.setattr(app_module, "_LIBRARY_STATS_RETRY_NOT_BEFORE", time.monotonic() - 1)
        after = await app_module._get_library_stats()
        await asyncio.sleep(0)
        await asyncio.sleep(0)
        return first, within, after, calls_within, delay_after_one

    first, within, after, calls_within, delay_after_one = asyncio.run(scenario())
    assert first == after == SNAPSHOT and within == [SNAPSHOT] * 5  # never blanked
    assert calls_within == 1  # five stale reads inside the window started nothing
    assert delay_after_one == app_module._LIBRARY_STATS_RETRY_BASE_SECONDS
    assert calls == 2
    assert app_module._LIBRARY_STATS_RETRY_DELAY == 2 * app_module._LIBRARY_STATS_RETRY_BASE_SECONDS


def test_library_stats_backoff_caps_at_the_cadence_and_resets_on_success(monkeypatch):
    outcomes = iter(["fail", "ok"])

    async def scenario():
        async def fake_collect():
            if next(outcomes) == "fail":
                raise HTTPException(502, "Library statistics unavailable")
            return {**SNAPSHOT, "totalReleases": 12}

        monkeypatch.setattr(app_module, "_collect_library_stats", fake_collect)
        monkeypatch.setattr(app_module, "_LIBRARY_STATS_RETRY_DELAY", 480.0)  # deep in a failure run
        _stale(monkeypatch, SNAPSHOT)
        await app_module._get_library_stats()  # fails
        await asyncio.sleep(0)
        await asyncio.sleep(0)
        capped = app_module._LIBRARY_STATS_RETRY_DELAY
        monkeypatch.setattr(app_module, "_LIBRARY_STATS_RETRY_NOT_BEFORE", time.monotonic() - 1)
        await app_module._get_library_stats()  # window passed: this one succeeds
        await app_module._LIBRARY_STATS_INFLIGHT
        await asyncio.sleep(0)
        return capped, await app_module._get_library_stats()

    capped, fresh = asyncio.run(scenario())
    assert capped == app_module._LIBRARY_STATS_CACHE_TTL_SECONDS  # 960 s would exceed the cadence
    assert fresh["totalReleases"] == 12
    assert app_module._LIBRARY_STATS_RETRY_DELAY == 0.0
    assert app_module._LIBRARY_STATS_RETRY_NOT_BEFORE == 0.0


def test_library_stats_cold_cache_wait_is_bounded_and_honest(client, monkeypatch):
    calls = 0

    async def fake_collect():
        nonlocal calls
        calls += 1
        await asyncio.sleep(0.2)  # well past the (shortened) inline wait
        return SNAPSHOT

    monkeypatch.setattr(app_module, "_collect_library_stats", fake_collect)
    monkeypatch.setattr(app_module, "_LIBRARY_STATS_INLINE_WAIT_SECONDS", 0.01)
    first = client.get("/api/library/stats", headers=PUBLIC)
    second = client.get("/api/library/stats", headers=PUBLIC)
    assert first.status_code == second.status_code == 200  # not a 5xx
    assert first.json() == second.json() == {"available": False}
    assert calls == 1  # the second request attached to the running collection

    # The collection outlives the bounded wait and publishes for the next call.
    deadline = time.monotonic() + 5
    while app_module._LIBRARY_STATS_CACHE is None and time.monotonic() < deadline:
        time.sleep(0.01)
    assert client.get("/api/library/stats", headers=PUBLIC).json() == SNAPSHOT
    assert calls == 1


# ── /api/torrents home shelves ─────────────────────────────────────────────

def test_shelf_concurrent_viewers_share_one_upstream_call(client, monkeypatch):
    calls = 0

    async def fake_query(q, variables=None, **kwargs):
        nonlocal calls
        calls += 1
        await asyncio.sleep(0.3)  # long enough for the second viewer to arrive
        return SEARCH

    monkeypatch.setattr(gql, "query", fake_query)
    with concurrent.futures.ThreadPoolExecutor(2) as pool:
        futures = [pool.submit(client.get, "/api/torrents", params=SHELF) for _ in range(2)]
        a, b = [f.result() for f in futures]
    assert a.status_code == b.status_code == 200
    assert a.json() == b.json()
    assert calls == 1
    # A later viewer inside the TTL is a plain hit.
    assert client.get("/api/torrents", params=SHELF).json() == a.json()
    assert calls == 1


def test_shelves_with_different_params_are_separate_entries(client, monkeypatch):
    seen = []

    async def fake_query(q, variables=None, **kwargs):
        seen.append((variables or {}).get("input", {}).get("orderBy"))
        return SEARCH

    monkeypatch.setattr(gql, "query", fake_query)
    for order in ("seeders", "newest", "seeders"):
        assert client.get("/api/torrents", params={**SHELF, "order": order}).status_code == 200
    assert seen == [
        [{"field": "seeders", "descending": True}],
        [{"field": "published_at", "descending": True}],
    ]


def test_torrents_stale_entry_is_served_and_refreshed_once(monkeypatch):
    calls = 0

    async def scenario():
        async def compute():
            nonlocal calls
            calls += 1
            return {"n": calls}

        first = await app_module._torrents_singleflight("k", compute)  # miss: inline
        stale_at = time.monotonic() - app_module._TORRENTS_CACHE_TTL - 1
        app_module._torrents_cache["k"] = (stale_at, first)
        stale = await app_module._torrents_singleflight("k", compute)   # served now
        during = await app_module._torrents_singleflight("k", compute)  # no 2nd refresh
        await app_module._torrents_inflight["k"]
        await asyncio.sleep(0)  # publish
        refreshed = await app_module._torrents_singleflight("k", compute)  # fresh hit
        too_old = time.monotonic() - app_module._TORRENTS_CACHE_STALE_TTL - 1
        app_module._torrents_cache["k"] = (too_old, refreshed)
        inline = await app_module._torrents_singleflight("k", compute)  # past the window
        return first, stale, during, refreshed, inline

    first, stale, during, refreshed, inline = asyncio.run(scenario())
    assert first == stale == during == {"n": 1}
    assert refreshed == {"n": 2}
    assert inline == {"n": 3}
    assert calls == 3
    assert "k" not in app_module._torrents_inflight

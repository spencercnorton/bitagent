"""Short-TTL singleflight cache for the operator indexer-stats aggregate.

The dashboard re-polls /api/indexer-stats every 30s; the aggregate is operator-
identical and expensive, so a usable snapshot is cached per day-window and
concurrent polls collapse into one collection. A transient "available: False" is
not cached so it self-heals on the next poll.
"""
import asyncio

import app as app_module
import graphql_client as gql


def test_usable_snapshot_is_cached_and_singleflighted(monkeypatch):
    calls = {"n": 0}

    async def fake(days):
        calls["n"] += 1
        await asyncio.sleep(0)
        return {"available": True, "days": days}

    monkeypatch.setattr(gql, "fetch_indexer_stats", fake)
    app_module._reset_indexer_stats_cache()

    async def run():
        first = asyncio.create_task(app_module._get_indexer_stats(30))
        await asyncio.sleep(0)
        second = asyncio.create_task(app_module._get_indexer_stats(30))
        a, b = await asyncio.gather(first, second)
        await asyncio.sleep(0)  # let the done-callback populate the cache
        c = await app_module._get_indexer_stats(30)
        return a, b, c

    a, b, c = asyncio.run(run())
    assert a == b == c == {"available": True, "days": 30}
    assert calls["n"] == 1  # concurrent callers shared one collection; third hit cache


def test_windows_are_cached_independently(monkeypatch):
    calls = {"n": 0}

    async def fake(days):
        calls["n"] += 1
        return {"available": True, "days": days}

    monkeypatch.setattr(gql, "fetch_indexer_stats", fake)
    app_module._reset_indexer_stats_cache()

    async def run():
        r30 = await app_module._get_indexer_stats(30)
        await asyncio.sleep(0)
        r7 = await app_module._get_indexer_stats(7)
        return r30, r7

    r30, r7 = asyncio.run(run())
    assert r30["days"] == 30 and r7["days"] == 7
    assert calls["n"] == 2  # different windows do not share a cache slot


def test_unavailable_result_is_not_cached(monkeypatch):
    calls = {"n": 0}

    async def fake(days):
        calls["n"] += 1
        if calls["n"] == 1:
            return {"available": False}
        return {"available": True, "days": days}

    monkeypatch.setattr(gql, "fetch_indexer_stats", fake)
    app_module._reset_indexer_stats_cache()

    async def run():
        down = await app_module._get_indexer_stats(30)
        await asyncio.sleep(0)  # done-callback runs; unavailable must not be cached
        up = await app_module._get_indexer_stats(30)
        return down, up

    down, up = asyncio.run(run())
    assert down == {"available": False}
    assert up == {"available": True, "days": 30}
    assert calls["n"] == 2  # the down result was refetched, not served from cache


def test_route_still_clamps_and_passes_through(client, monkeypatch):
    import config
    config.settings.require_auth = False  # restored by conftest _restore_settings
    captured = {}

    async def fake_query(q, variables=None, **kwargs):
        captured["days"] = (variables or {})["input"]["days"]
        return {"data": {"evidence": {"indexerStats": {"totalGrabs": 1, "bitagentWinRate": 0.5}}}}

    monkeypatch.setattr(gql, "query", fake_query)
    r = client.get("/api/indexer-stats?days=99999")
    assert r.status_code == 200
    assert 1 <= captured["days"] <= 365  # clamp still applies before the cache key

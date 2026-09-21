"""Truthfulness and load bounds for the operator telemetry foundation."""
from __future__ import annotations

import asyncio

import app as app_module
from telemetry import ProbeResult, TelemetryUnavailable, collect_bounded_probes


def _fake_snapshot(observed_at: str, *, status: str = "ok", value=1):
    return {
        "snapshot": {"observed_at": observed_at, "stale": False},
        "metrics": {
            "example": {
                "value": value,
                "status": status,
                "source": "source:example",
                "observed_at": observed_at,
                "stale": False,
                "error": None if status == "ok" else "source unavailable",
            },
        },
    }


def test_probe_runner_is_parallel_but_concurrency_bounded():
    async def exercise():
        active = 0
        max_seen = 0
        first_pair_started = asyncio.Event()
        release = asyncio.Event()
        started = 0

        async def probe(value):
            nonlocal active, max_seen, started
            active += 1
            started += 1
            max_seen = max(max_seen, active)
            if started == 2:
                first_pair_started.set()
            await release.wait()
            active -= 1
            return value

        tasks = {
            str(index): (f"source:{index}", lambda index=index: probe(index))
            for index in range(4)
        }
        collecting = asyncio.create_task(collect_bounded_probes(
            tasks,
            observed_at="2026-07-16T00:00:00Z",
            timeout_seconds=1,
            max_concurrency=2,
        ))
        await asyncio.wait_for(first_pair_started.wait(), timeout=1)
        assert max_seen == 2
        release.set()
        results = await collecting
        return max_seen, results

    max_seen, results = asyncio.run(exercise())
    assert max_seen == 2
    assert {name: result.value for name, result in results.items()} == {
        "0": 0, "1": 1, "2": 2, "3": 3,
    }
    assert all(result.status == "ok" for result in results.values())


def test_probe_timeout_does_not_erase_independent_success():
    async def never_returns():
        await asyncio.Event().wait()

    async def succeeds():
        return {"count": 0}

    results = asyncio.run(collect_bounded_probes(
        {
            "slow": ("source:slow", never_returns),
            "fast": ("source:fast", succeeds),
        },
        observed_at="2026-07-16T00:00:00Z",
        timeout_seconds=0.01,
        max_concurrency=2,
    ))

    assert results["fast"].status == "ok"
    assert results["fast"].value == {"count": 0}
    assert results["slow"].status == "error"
    assert results["slow"].value is None
    assert results["slow"].error == "probe timed out after 0.01s"


def test_probe_unavailable_is_distinct_from_error_and_zero():
    async def unavailable():
        raise TelemetryUnavailable("metric family not emitted")

    result = asyncio.run(collect_bounded_probes(
        {"metric": ("source:metric", unavailable)},
        observed_at="2026-07-16T00:00:00Z",
        timeout_seconds=1,
        max_concurrency=1,
    ))["metric"]

    assert result.status == "unavailable"
    assert result.value is None
    assert result.error == "metric family not emitted"


def test_counter_rate_uses_only_samples_after_latest_reset():
    app_module._METRIC_HISTORY[:] = [
        (0.0, {"counter": 5.0}),
        (10.0, {"counter": 100.0}),
        (20.0, {"counter": 10.0}),  # core restart/reset
        (30.0, {"counter": 20.0}),
    ]
    assert app_module._counter_rate_per_min("counter", 30.0) == 60.0


def test_counter_rate_is_unknown_without_minimum_baseline():
    app_module._METRIC_HISTORY[:] = [
        (0.0, {"counter": 5.0}),
        (5.0, {"counter": 5.0}),
    ]
    assert app_module._counter_rate_per_min("counter", 5.0) is None


def test_counter_rate_is_unknown_when_current_scrape_drops_family():
    app_module._METRIC_HISTORY[:] = [
        (0.0, {"counter": 0.0}),
        (40.0, {"counter": 1.0}),
        (50.0, {"unrelated": 9.0}),
    ]
    assert app_module._counter_rate_per_min("counter", 50.0) is None


def test_fractional_measured_rates_are_preserved():
    app_module._METRIC_HISTORY[:] = [
        (0.0, {"counter": 0.0}),
        (40.0, {"counter": 1.0}),
    ]
    measured = app_module._counter_rate_per_min("counter", 40.0)
    assert measured == 1.5
    assert app_module._nonnegative_rate(measured) == 1.5
    assert app_module._nonnegative_rate(0.25) == 0.25


def test_labeled_sum_requires_a_matching_series_not_just_family_presence():
    lines = {
        'metric_total{class="alive"}': 0.0,
        'metric_total{class="other"}': 4.0,
    }
    assert app_module._labeled_metric_sum(
        lines, "metric_total", **{"class": "alive"}
    ) == 0.0
    assert app_module._labeled_metric_sum(
        lines, "metric_total", **{"class": "suspect"}
    ) is None


def test_stats_snapshot_singleflights_concurrent_callers(monkeypatch):
    calls = 0
    entered = asyncio.Event()
    release = asyncio.Event()

    async def collect():
        nonlocal calls
        calls += 1
        entered.set()
        await release.wait()
        return _fake_snapshot("2026-07-16T01:00:00Z")

    monkeypatch.setattr(app_module, "_collect_stats_snapshot", collect)
    app_module._reset_stats_snapshot_cache()

    async def exercise():
        callers = [
            asyncio.create_task(app_module._get_stats_snapshot())
            for _ in range(8)
        ]
        await asyncio.wait_for(entered.wait(), timeout=1)
        await asyncio.sleep(0)
        assert calls == 1
        release.set()
        results = await asyncio.gather(*callers)
        await asyncio.sleep(0)  # let the cache-publish callback run
        cached = await app_module._get_stats_snapshot()
        return results, cached

    results, cached = asyncio.run(exercise())
    assert calls == 1
    assert {result["snapshot"]["observed_at"] for result in results} == {
        "2026-07-16T01:00:00Z"
    }
    assert all(result["snapshot"]["cached"] is False for result in results)
    assert cached["snapshot"]["cached"] is True
    assert cached["snapshot"]["stale"] is False


def test_stats_collection_declares_exact_bounded_four_probe_set(monkeypatch):
    captured = {}

    async def collect(probes, *, observed_at, timeout_seconds, max_concurrency):
        captured.update({
            "names": set(probes),
            "timeout": timeout_seconds,
            "max_concurrency": max_concurrency,
        })

        def result(name, value):
            return ProbeResult(
                value=value,
                status="ok",
                source=f"source:{name}",
                observed_at=observed_at,
            )

        return {
            "graphql": result("graphql", {
                "data": {"version": "v"},
                "search": {
                    "totalCount": 0,
                    "aggregations": {"contentType": []},
                },
            }),
            "prometheus": result("prometheus", {"sum": {}, "lines": {}}),
            "evidence": result("evidence", 0),
            "db": result("db", True),
        }

    monkeypatch.setattr(app_module, "collect_bounded_probes", collect)
    snapshot = asyncio.run(app_module._collect_stats_snapshot())

    assert captured == {
        "names": {"graphql", "prometheus", "evidence", "db"},
        "timeout": app_module._STATS_PROBE_TIMEOUT_SECONDS,
        "max_concurrency": 4,
    }
    assert snapshot["totalTorrents"] == 0


def test_failed_collection_does_not_poison_singleflight(monkeypatch):
    calls = 0

    async def collect():
        nonlocal calls
        calls += 1
        if calls == 1:
            raise RuntimeError("collector failed")
        return _fake_snapshot("2026-07-16T02:00:00Z")

    monkeypatch.setattr(app_module, "_collect_stats_snapshot", collect)
    app_module._reset_stats_snapshot_cache()

    async def exercise():
        try:
            await app_module._get_stats_snapshot()
        except RuntimeError:
            pass
        else:
            raise AssertionError("first collection should fail")
        await asyncio.sleep(0)
        return await app_module._get_stats_snapshot()

    recovered = asyncio.run(exercise())
    assert calls == 2
    assert recovered["metrics"]["example"]["status"] == "ok"


def test_error_snapshot_expires_and_is_reprobed(monkeypatch):
    calls = 0
    clock = {"now": 100.0}

    async def collect():
        nonlocal calls
        calls += 1
        if calls == 1:
            return _fake_snapshot(
                "2026-07-16T03:00:00Z", status="unavailable", value=None
            )
        return _fake_snapshot("2026-07-16T03:00:06Z")

    monkeypatch.setattr(app_module, "_collect_stats_snapshot", collect)
    monkeypatch.setattr(app_module, "_stats_cache_now", lambda: clock["now"])
    app_module._reset_stats_snapshot_cache()

    async def exercise():
        first = await app_module._get_stats_snapshot()
        await asyncio.sleep(0)
        clock["now"] = 101.0
        cached = await app_module._get_stats_snapshot()
        clock["now"] = 106.0
        recovered = await app_module._get_stats_snapshot()
        return first, cached, recovered

    first, cached, recovered = asyncio.run(exercise())
    assert calls == 2
    assert first["metrics"]["example"]["status"] == "unavailable"
    assert cached["snapshot"]["cached"] is True
    assert cached["metrics"]["example"]["status"] == "unavailable"
    assert recovered["snapshot"]["cached"] is False
    assert recovered["metrics"]["example"]["status"] == "ok"


def test_snapshot_presentation_marks_expired_data_stale():
    presented = app_module._present_stats_snapshot(
        _fake_snapshot("2026-07-16T04:00:00Z"),
        cached=True,
        age_seconds=app_module._STATS_SNAPSHOT_TTL_SECONDS + 1,
    )
    assert presented["snapshot"]["stale"] is True
    assert presented["metrics"]["example"]["stale"] is True
    assert presented["metrics"]["example"]["status"] == "stale"

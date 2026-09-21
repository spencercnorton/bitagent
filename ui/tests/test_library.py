"""Public library surface — host-based routing + TMDB metadata endpoints.

Covers the additions that back library.example.org: the `/` route
serves the library shell on an allowed public host and the operator dashboard
on an allowed operator host, `/library` always serves the library on either,
and the /api/meta/*
enrichment endpoints validate + soft-404 correctly. Backend TMDB calls are
faked so the suite stays hermetic.
"""
from __future__ import annotations

import asyncio
from datetime import datetime, timezone

import pytest

import app as app_module
import config
import graphql_client as gql
import tmdb


@pytest.fixture(autouse=True)
def _open_auth():
    config.settings.require_auth = False


# ── /api/torrents order param (drives the home Popular / Recently-added rows) ─

def _capture_order(client, monkeypatch, **params):
    captured = {}

    async def fake_query(q, variables=None, **kwargs):
        captured["input"] = (variables or {}).get("input", {})
        return {"data": {"torrentContent": {"search": {"totalCount": 0, "items": []}}}}

    monkeypatch.setattr(gql, "query", fake_query)
    r = client.get("/api/torrents", params=params)
    assert r.status_code == 200
    return captured["input"].get("orderBy")


def test_torrents_order_newest_maps_to_published_at(client, monkeypatch):
    assert _capture_order(client, monkeypatch, order="newest") == [
        {"field": "published_at", "descending": True}
    ]


def test_torrents_order_seeders_maps_to_seeders(client, monkeypatch):
    assert _capture_order(client, monkeypatch, order="seeders") == [
        {"field": "seeders", "descending": True}
    ]


def test_torrents_order_size_maps_to_size_for_raw_fallback(client, monkeypatch):
    assert _capture_order(client, monkeypatch, order="size") == [
        {"field": "size", "descending": True}
    ]


def test_torrents_order_name_maps_to_name_for_raw_fallback(client, monkeypatch):
    assert _capture_order(client, monkeypatch, order="name") == [
        {"field": "name", "descending": False}
    ]


def test_torrents_no_order_omits_orderby(client, monkeypatch):
    assert _capture_order(client, monkeypatch) is None


def test_torrents_unknown_order_omits_orderby(client, monkeypatch):
    # An unrecognised order keyword must not inject a bad orderBy.
    assert _capture_order(client, monkeypatch, order="bogus") is None


# ── /api/library/stats headline counts ─────────────────────────────────────

def test_library_stats_uses_aligned_exact_rolling_window(client, monkeypatch):
    fixed_now = datetime(2026, 7, 17, 18, 30, tzinfo=timezone.utc)
    captured = {}

    async def fake_query(query, variables=None, **kwargs):
        captured["query"] = query
        captured["variables"] = variables
        return {
            "data": {
                "torrentContent": {
                    "total": {
                        "totalCount": 3_157_643,
                        "totalCountIsEstimate": False,
                    },
                    "recent": {
                        "totalCount": 12_345,
                        "totalCountIsEstimate": False,
                    },
                }
            }
        }

    monkeypatch.setattr(gql, "query", fake_query)
    monkeypatch.setattr(app_module, "_library_stats_now", lambda: fixed_now)

    response = client.get(
        "/api/library/stats",
        headers={"host": "library.example.org"},
    )
    assert response.status_code == 200
    assert response.json() == {
        "available": True,
        "totalReleases": 3_157_643,
        "totalReleasesIsEstimate": False,
        "releasesAddedLast7Days": 12_345,
        "windowStart": "2026-07-10T18:30:00Z",
        "windowEnd": "2026-07-17T18:30:00Z",
        "observedAt": "2026-07-17T18:30:00Z",
    }

    assert captured["query"] == gql.LIBRARY_STATS
    total_input = captured["variables"]["totalInput"]
    assert total_input == {
        "limit": 0,
        "totalCount": True,
        "aggregationBudget": 0,
        "facets": {"contentType": {"filter": ["movie", "tv_show"]}},
    }
    recent_input = captured["variables"]["recentInput"]
    assert recent_input == {
        "limit": 0,
        "totalCount": True,
        "aggregationBudget": 0,
        "torrentCreatedAfter": "2026-07-10T18:30:00Z",
        "torrentCreatedBefore": "2026-07-17T18:30:00Z",
        "facets": {"contentType": {"filter": ["movie", "tv_show"]}},
    }


def test_library_stats_success_is_cached(client, monkeypatch):
    calls = 0

    async def fake_query(*args, **kwargs):
        nonlocal calls
        calls += 1
        return {
            "data": {
                "torrentContent": {
                    "total": {"totalCount": 10, "totalCountIsEstimate": False},
                    "recent": {"totalCount": 2, "totalCountIsEstimate": False},
                }
            }
        }

    monkeypatch.setattr(gql, "query", fake_query)
    headers = {"host": "library.example.org"}
    first = client.get("/api/library/stats", headers=headers)
    second = client.get("/api/library/stats", headers=headers)

    assert first.status_code == second.status_code == 200
    assert first.json() == second.json()
    assert calls == 1


@pytest.mark.parametrize(
    "payload",
    [
        {"errors": [{"message": "core unavailable"}]},
        {
            "data": {
                "torrentContent": {
                    "total": {"totalCount": 10, "totalCountIsEstimate": True},
                    "recent": {"totalCount": 2, "totalCountIsEstimate": False},
                }
            }
        },
        {
            "data": {
                "torrentContent": {
                    "total": {"totalCount": 10, "totalCountIsEstimate": False},
                    "recent": {"totalCount": 2, "totalCountIsEstimate": True},
                }
            }
        },
        {"data": []},
        {"data": {"torrentContent": "bad"}},
        {
            "data": {
                "torrentContent": {
                    "total": {"totalCount": 10, "totalCountIsEstimate": False},
                    "recent": {"totalCount": None, "totalCountIsEstimate": False},
                }
            }
        },
    ],
)
def test_library_stats_invalid_upstream_is_unavailable(client, monkeypatch, payload):
    async def fake_query(*args, **kwargs):
        return payload

    monkeypatch.setattr(gql, "query", fake_query)
    response = client.get(
        "/api/library/stats",
        headers={"host": "library.example.org"},
    )
    assert response.status_code == 502
    assert response.json() == {"detail": "Library statistics unavailable"}


def test_library_stats_is_hidden_on_operator_host(client, monkeypatch):
    async def should_not_query(*args, **kwargs):
        raise AssertionError("operator host must be rejected before GraphQL")

    monkeypatch.setattr(gql, "query", should_not_query)
    response = client.get(
        "/api/library/stats",
        headers={"host": "console.example.org"},
    )
    assert response.status_code == 404


def test_library_stats_concurrent_callers_share_one_collection(monkeypatch):
    calls = 0

    async def scenario():
        started = asyncio.Event()
        release = asyncio.Event()

        async def fake_collect():
            nonlocal calls
            calls += 1
            started.set()
            await release.wait()
            return {"totalReleases": 10, "releasesAddedLast7Days": 2}

        monkeypatch.setattr(app_module, "_collect_library_stats", fake_collect)
        first = asyncio.create_task(app_module._get_library_stats())
        await started.wait()
        second = asyncio.create_task(app_module._get_library_stats())
        await asyncio.sleep(0)
        release.set()
        return await asyncio.gather(first, second)

    results = asyncio.run(scenario())
    assert calls == 1
    assert results == [
        {"totalReleases": 10, "releasesAddedLast7Days": 2},
        {"totalReleases": 10, "releasesAddedLast7Days": 2},
    ]


def test_library_stats_failure_is_not_cached_and_retries_after_the_backoff_window(
    client, monkeypatch
):
    calls = 0

    async def fake_query(*args, **kwargs):
        nonlocal calls
        calls += 1
        if calls == 1:
            return {"errors": [{"message": "temporary outage"}]}
        return {
            "data": {
                "torrentContent": {
                    "total": {"totalCount": 10, "totalCountIsEstimate": False},
                    "recent": {"totalCount": 2, "totalCountIsEstimate": False},
                }
            }
        }

    monkeypatch.setattr(gql, "query", fake_query)
    headers = {"host": "library.example.org"}
    failed = client.get("/api/library/stats", headers=headers)
    # Inside the backoff window nothing is cached and nothing is started.
    backed_off = client.get("/api/library/stats", headers=headers)
    assert failed.status_code == 502
    assert backed_off.status_code == 200
    assert backed_off.json() == {"available": False}
    assert calls == 1

    # Once the window has elapsed the next request retries, and succeeds.
    monkeypatch.setattr(app_module, "_LIBRARY_STATS_RETRY_NOT_BEFORE", 0.0)
    retried = client.get("/api/library/stats", headers=headers)
    assert retried.status_code == 200
    assert retried.json()["releasesAddedLast7Days"] == 2
    assert calls == 2


def test_torrents_exposes_original_language_for_anime_filter(client, monkeypatch):
    async def fake_query(q, variables=None, **kwargs):
        return {
            "data": {
                "torrentContent": {
                    "search": {
                        "totalCount": 1,
                        "items": [{
                            "infoHash": "abc",
                            "title": "Kaiju No. 8 / 怪獣８号 (2024) S01E01",
                            "contentType": "tv_show",
                            "contentSource": "tmdb",
                            "contentId": "123",
                            "seeders": 9,
                            "leechers": 1,
                            "createdAt": "2026-07-05T00:00:00Z",
                            "updatedAt": "2026-07-05T00:00:00Z",
                            "languages": [{"id": "en"}],
                            "videoResolution": "V1080p",
                            "videoSource": "WEBDL",
                            "releaseGroup": "Yameii",
                            "content": {
                                "originalLanguage": {"id": "ja", "name": "Japanese"},
                                "originalTitle": "怪獣８号",
                            },
                            "torrent": {"name": "[Yameii] Kaiju No. 8 - S01E01.mkv", "size": 42, "filesCount": 1},
                        }],
                        "aggregations": {},
                    }
                }
            }
        }

    monkeypatch.setattr(gql, "query", fake_query)
    r = client.get("/api/torrents", params={"genres": "tmdb:16"})
    assert r.status_code == 200
    item = r.json()["items"][0]
    assert item["originalLanguage"] == "ja"
    assert item["originalLanguageName"] == "Japanese"
    assert item["originalTitle"] == "怪獣８号"


# ── Host-based routing ─────────────────────────────────────────────────────

def test_public_host_serves_library_shell(client):
    # Default public-library host is an explicit configured surface.
    r = client.get("/", headers={"host": "library.example.org"})
    assert r.status_code == 200
    assert "library.js" in r.text
    assert "Library — BitAgent" in r.text
    # The operator SPA bundle must NOT be on the public surface.
    assert "js/app.js" not in r.text


def test_public_library_shell_contains_snapshot_banner_only(client):
    public = client.get("/", headers={"host": "library.example.org"})
    operator = client.get("/", headers={"host": "console.example.org"})

    assert public.status_code == 200
    assert 'id="libStatsGrid"' in public.text
    assert 'id="libStatTotal"' in public.text
    assert 'id="libStatAdded"' in public.text
    assert 'aria-busy="true"' in public.text
    assert "Last 7 days" in public.text
    assert operator.status_code == 200
    assert 'id="libStatsGrid"' not in operator.text


def test_internal_host_serves_operator_dashboard(client):
    r = client.get("/", headers={"host": "console.example.org"})
    assert r.status_code == 200
    assert "js/app.js" in r.text


def test_library_path_always_serves_library(client):
    # Even on a non-public host, /library is the library (for testing / links).
    r = client.get("/library", headers={"host": "console.example.org"})
    assert r.status_code == 200
    assert "library.js" in r.text
    assert 'id="libStatsGrid"' not in r.text


def test_public_host_matching_ignores_port(client):
    r = client.get("/", headers={"host": "library.example.org:8080"})
    assert r.status_code == 200
    assert "library.js" in r.text


# ── TMDB metadata endpoints ────────────────────────────────────────────────

def test_meta_rejects_unknown_media_type(client):
    r = client.get("/api/meta/podcast/123")
    assert r.status_code == 400


def test_meta_details_404_when_unavailable(client, monkeypatch):
    async def _none(tmdb_id, media_type="movie"):
        return None
    monkeypatch.setattr(tmdb, "get_details", _none)
    r = client.get("/api/meta/movie/999999")
    assert r.status_code == 404


def test_meta_details_returns_payload(client, monkeypatch):
    payload = {"tmdbId": "42", "mediaType": "movie", "title": "X", "seasons": []}

    async def _ok(tmdb_id, media_type="movie"):
        assert tmdb_id == "42"
        return payload
    monkeypatch.setattr(tmdb, "get_details", _ok)
    r = client.get("/api/meta/movie/42")
    assert r.status_code == 200
    assert r.json()["title"] == "X"


def test_meta_season_404_when_unavailable(client, monkeypatch):
    async def _none(tmdb_id, season_number):
        return None
    monkeypatch.setattr(tmdb, "get_season", _none)
    r = client.get("/api/meta/tv/100088/season/1")
    assert r.status_code == 404


def test_meta_season_returns_episodes(client, monkeypatch):
    async def _ok(tmdb_id, season_number):
        assert (tmdb_id, season_number) == ("100088", 2)
        return {"seasonNumber": 2, "episodes": [{"episodeNumber": 1, "name": "Ep"}]}
    monkeypatch.setattr(tmdb, "get_season", _ok)
    r = client.get("/api/meta/tv/100088/season/2")
    assert r.status_code == 200
    assert r.json()["episodes"][0]["name"] == "Ep"

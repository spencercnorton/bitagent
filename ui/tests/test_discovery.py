"""Synthetic TMDB discovery/provider contract and bounded cache regression tests."""
from __future__ import annotations

import asyncio

import httpx
import pytest

import config
import discovery
import tmdb


@pytest.fixture(autouse=True)
def _reset_discovery():
    config.settings.require_auth = False
    config.settings.tmdb_api_key = "synthetic-discovery-key"
    discovery.reset_cache()
    yield
    discovery.reset_cache()


def _title(identifier=10, *, tv=False):
    return {"id": identifier, "name" if tv else "title": "Synthetic Show" if tv else "Synthetic Movie",
            "first_air_date" if tv else "release_date": "2026-01-02",
            "backdrop_path": "/synthetic.jpg", "poster_path": "/poster.jpg", "overview": "An invented title."}


def _provider(identifier=8, name="Synthetic Stream"):
    return {"provider_id": identifier, "provider_name": name,
            "logo_path": "/logo.jpg", "display_priorities": {"US": 1, "GB": 2}}


def _mock(monkeypatch, responder=None):
    calls = []

    def route(request):
        calls.append(request)
        path = request.url.path.removeprefix("/3/")
        if responder:
            result = responder(path, request)
            if result is not None:
                return httpx.Response(200, json=result)
        if path == "watch/providers/regions":
            body = {"results": [{"iso_3166_1": "US", "english_name": "United States"},
                                {"iso_3166_1": "GB", "english_name": "United Kingdom"}]}
        elif path in ("watch/providers/movie", "watch/providers/tv"):
            body = {"results": [_provider()]}
        else:
            body = {"results": [_title(tv=path.endswith("/tv"))], "page": 1, "total_pages": 3}
        return httpx.Response(200, json=body)

    upstream = httpx.AsyncClient(transport=httpx.MockTransport(route))
    monkeypatch.setattr(tmdb, "_get_client", lambda: upstream)
    return calls


def test_unconfigured_discovery_never_calls_upstream(client, monkeypatch):
    config.settings.tmdb_api_key = ""
    monkeypatch.setattr(tmdb, "_get_client", lambda: pytest.fail("No key must not perform network I/O"))
    for route in ("/api/discovery", "/api/discovery/providers", "/api/discovery/spotlight"):
        body = client.get(route).json()
        assert body["available"] is False
        assert body["reason"] == "not_configured"
        assert body["items"] == []


def test_provider_catalogue_merges_types_sorts_priorities_and_includes_regions(client, monkeypatch):
    calls = _mock(monkeypatch)
    response = client.get("/api/discovery/providers")
    assert response.status_code == 200
    body = response.json()
    assert body["available"] is True
    assert body["providers"] == [{"id": 8, "name": "Synthetic Stream", "logo": "https://image.tmdb.org/t/p/w92/logo.jpg",
                                 "displayPriority": 1, "types": ["movie", "tv_show"], "logoAlternatives": [],
                                 "providerIds": [8], "idsByType": {"movie": [8], "tv_show": [8]},
                                 "aliases": ["Synthetic Stream"], "brandKey": "synthetic stream"}]
    assert {row["code"] for row in body["regions"]} == {"US", "GB"}
    assert body["attribution"] == "Provider availability: JustWatch via TMDB"
    client.get("/api/discovery/providers")
    assert len(calls) == 3, "Repeated provider loads must reuse the server cache"


def test_provider_discover_applies_region_and_returns_metadata_without_index_claim(client, monkeypatch):
    calls = _mock(monkeypatch)
    body = client.get("/api/discovery", params={"provider": 8, "region": "GB", "type": "tv_show", "page": 2}).json()
    request = next(call for call in calls if call.url.path.endswith("/discover/tv"))
    assert request.url.params["with_watch_providers"] == "8"
    assert request.url.params["watch_region"] == "GB"
    assert request.url.params["page"] == "2"
    assert body["hasNextPage"] is True
    assert body["items"][0]["availability"] == "unchecked"
    assert body["items"][0]["type"] == "tv_show"
    assert "synthetic-discovery-key" not in str(body)


def test_provider_search_intersects_real_country_attribution_not_name_search_alone(client, monkeypatch):
    def respond(path, _request):
        if path == "search/movie":
            return {"results": [_title(10), _title(11), _title(12)], "total_pages": 2}
        if path.endswith("/watch/providers"):
            identifier = int(path.split("/")[1])
            provider = _provider(8 if identifier == 10 else 9)
            return {"results": {"US": {"flatrate": [provider]}, "GB": {"flatrate": [_provider(8)]}}}
        return None

    calls = _mock(monkeypatch, respond)
    body = client.get("/api/discovery", params={"provider": 8, "q": "Synthetic", "region": "US"}).json()
    assert [row["id"] for row in body["items"]] == ["10"]
    assert body["partial"] is False
    assert body["hasNextPage"] is True
    assert len([call for call in calls if "/watch/providers" in call.url.path and "/movie/" in call.url.path]) == 3
    client.get("/api/discovery", params={"provider": 8, "q": "Synthetic", "region": "US"})
    assert len(calls) == 7, "Search and country attributions must reuse cache"


def test_empty_filtered_search_page_can_have_more_results(client, monkeypatch):
    def respond(path, _request):
        if path == "movie/10/watch/providers":
            return {"results": {"US": {"buy": [_provider(99)]}}}
        return None

    _mock(monkeypatch, respond)
    body = client.get("/api/discovery", params={"provider": 8, "q": "Synthetic"}).json()
    assert body["available"] is True
    assert body["items"] == []
    assert body["hasNextPage"] is True


def test_failed_provider_attribution_is_partial_and_never_passes_provider_filter(client, monkeypatch):
    _mock(monkeypatch, lambda path, _request: {"success": False} if path == "movie/10/watch/providers" else None)
    body = client.get("/api/discovery", params={"provider": 8, "q": "Synthetic"}).json()
    assert body["available"] is True
    assert body["items"] == []
    assert body["partial"] is True


def test_newest_movie_and_tv_use_native_date_fields_and_no_future_release(client, monkeypatch):
    calls = _mock(monkeypatch)
    client.get("/api/discovery", params={"mode": "newest", "type": "movie"})
    client.get("/api/discovery", params={"mode": "newest", "type": "tv_show"})
    movie, tv = calls
    assert movie.url.params["sort_by"] == "primary_release_date.desc"
    assert "primary_release_date.lte" in movie.url.params
    assert tv.url.params["sort_by"] == "first_air_date.desc"
    assert "first_air_date.lte" in tv.url.params
    assert "primary_release_date.lte" not in tv.url.params


def test_spotlight_mixes_media_and_shelves_and_deduplicates_identity(client, monkeypatch):
    _mock(monkeypatch)
    body = client.get("/api/discovery/spotlight").json()
    assert body["available"] is True
    assert {row["type"] for row in body["items"]} == {"movie", "tv_show"}
    assert len(body["items"]) == 2
    assert all(row["availability"] == "unchecked" for row in body["items"])


def test_all_media_feed_interleaves_independently_paged_movie_and_show_results(client, monkeypatch):
    def respond(path, request):
        if path in ("discover/movie", "discover/tv"):
            tv = path.endswith("/tv")
            assert request.url.params["page"] == "2"
            return {"results": [_title(10, tv=tv), _title(11, tv=tv)], "total_pages": 3 if tv else 2}
        return None

    _mock(monkeypatch, respond)
    body = client.get("/api/discovery", params={"provider": 8, "type": "all", "page": 2}).json()
    assert body["available"] is True
    assert body["hasNextPage"] is True
    assert [row["type"] for row in body["items"]] == ["movie", "tv_show", "movie", "tv_show"]


def test_all_media_feed_can_use_provider_with_only_one_supported_media_type(client, monkeypatch):
    _mock(monkeypatch, lambda path, _request: {"results": []} if path == "watch/providers/tv" else None)
    body = client.get("/api/discovery", params={"provider": 8, "type": "all"}).json()
    assert body["available"] is True
    assert {row["type"] for row in body["items"]} == {"movie"}
    unsupported = client.get("/api/discovery", params={"provider": 8, "type": "tv_show"})
    assert unsupported.status_code == 200
    assert unsupported.json()["items"] == []


def test_all_media_feed_marks_one_failed_upstream_as_partial(client, monkeypatch):
    _mock(monkeypatch, lambda path, _request: {"success": False} if path == "discover/movie" else None)
    body = client.get("/api/discovery", params={"type": "all"}).json()
    assert body["available"] is True
    assert body["partial"] is True
    assert {row["type"] for row in body["items"]} == {"tv_show"}


def test_first_six_spotlight_items_include_both_shelves_and_media_types(client, monkeypatch):
    def respond(path, request):
        if path.startswith("discover/"):
            newest = request.url.params["sort_by"] != "popularity.desc"
            return {"results": [_title((20 if newest else 10) + i, tv=path.endswith("/tv")) for i in range(5)], "total_pages": 1}
        return None

    _mock(monkeypatch, respond)
    body = client.get("/api/discovery/spotlight").json()
    assert len(body["items"]) == 8
    assert {row["mode"] for row in body["items"][:6]} == {"popular", "newest"}
    assert {row["type"] for row in body["items"][:6]} == {"movie", "tv_show"}


@pytest.mark.parametrize("params", [{"provider": 0}, {"page": 51}, {"page": 0}, {"region": "us"}, {"type": "music"}, {"q": "x" * 201}])
def test_discovery_inputs_are_bounded(client, params):
    assert client.get("/api/discovery", params=params).status_code == 422


def test_unsupported_provider_region_or_type_is_rejected(client, monkeypatch):
    _mock(monkeypatch)
    assert client.get("/api/discovery", params={"provider": 99}).status_code == 400
    assert client.get("/api/discovery/providers", params={"region": "ZZ"}).status_code == 400


def test_failed_or_malformed_upstream_does_not_become_empty_success(client, monkeypatch):
    _mock(monkeypatch, lambda path, _request: {"success": False} if path == "discover/movie" else None)
    body = client.get("/api/discovery").json()
    assert body["available"] is False
    assert body["reason"] == "upstream_unavailable"


def test_discovery_requires_authentication_before_upstream_access(client, monkeypatch):
    config.settings.require_auth = True
    config.settings.trust_npm_headers = False
    config.settings.trust_forwarded_user = False
    monkeypatch.setattr(tmdb, "_get_client", lambda: pytest.fail("Unauthenticated discovery cannot call upstream"))
    for route in ("/api/discovery", "/api/discovery/providers", "/api/discovery/spotlight"):
        assert client.get(route, headers={"host": "library.example.org"}).status_code == 401


def test_malformed_provider_attribution_cannot_be_treated_as_a_known_miss(client, monkeypatch):
    _mock(monkeypatch, lambda path, _request: {"results": {"US": {"flatrate": "bad"}}}
          if path == "movie/10/watch/providers" else None)
    body = client.get("/api/discovery", params={"provider": 8, "q": "Synthetic"}).json()
    assert body["partial"] is True
    assert body["items"] == []


def test_singleflight_is_cancellation_safe_and_requests_are_bounded(monkeypatch):
    async def scenario():
        entered, release = asyncio.Event(), asyncio.Event()
        calls = 0

        async def request(_request):
            nonlocal calls
            calls += 1
            entered.set()
            await release.wait()
            return httpx.Response(200, json={"results": []})

        upstream = httpx.AsyncClient(transport=httpx.MockTransport(request))
        monkeypatch.setattr(tmdb, "_get_client", lambda: upstream)
        first = asyncio.create_task(discovery._cached_get("discover/movie", {}, 60))
        await entered.wait()
        second = asyncio.create_task(discovery._cached_get("discover/movie", {}, 60))
        first.cancel()
        with pytest.raises(asyncio.CancelledError):
            await first
        release.set()
        assert await second == {"results": []}
        assert calls == 1
        await upstream.aclose()

    asyncio.run(scenario())

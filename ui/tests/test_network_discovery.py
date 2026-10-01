"""Hermetic provider-logo, TV-network catalogue, and access regressions."""
from __future__ import annotations

import asyncio
import gzip
import json
from datetime import datetime, timezone

import httpx
import pytest

import config
import discovery
import network_catalog
import private_indexer
import tmdb


@pytest.fixture(autouse=True)
def _settings_and_caches(monkeypatch):
    monkeypatch.setattr(config.settings, "require_auth", False)
    monkeypatch.setattr(config.settings, "private_indexer_enabled", False)
    monkeypatch.setattr(config.settings, "tmdb_api_key", "synthetic-network-key")
    discovery.reset_cache()
    yield
    discovery.reset_cache()


@pytest.fixture
def upstream(monkeypatch):
    """Install only explicit synthetic responses; unexpected requests fail."""
    clients = []

    def install(responder):
        calls = []

        def respond(request):
            calls.append(request)
            result = responder(request.url.path.removeprefix("/3/"), request)
            return result if isinstance(result, httpx.Response) else httpx.Response(200, json=result)

        client = httpx.AsyncClient(transport=httpx.MockTransport(respond))
        clients.append(client)
        monkeypatch.setattr(tmdb, "_get_client", lambda: client)
        return calls

    yield install
    for client in clients:
        asyncio.run(client.aclose())


def _export(rows):
    return gzip.compress(("\n".join(json.dumps(row) for row in rows) + "\n").encode())


def _network(identifier, **overrides):
    return {"id": identifier, "name": f"Synthetic Network {identifier}",
            "logo_path": "/synthetic-network.png", "origin_country": "US", **overrides}


def _show(identifier):
    return {"id": identifier, "name": f"Synthetic Series {identifier}",
            "first_air_date": "2026-01-02", "poster_path": "/synthetic-show.png"}


def _index(monkeypatch, rows):
    async def get_index():
        return rows

    monkeypatch.setattr(network_catalog, "get_index", get_index)


def _no_io(monkeypatch):
    async def forbidden_index():
        pytest.fail("The daily network index must not be fetched")

    monkeypatch.setattr(network_catalog, "get_index", forbidden_index)
    monkeypatch.setattr(tmdb, "_get_client", lambda: pytest.fail("TMDB must not be contacted"))


@pytest.mark.parametrize("missing_logo", [None, "/movie1.png?bad=1"], ids=["missing", "invalid"])
def test_provider_union_recovers_missing_logo_preserves_safe_alternatives_and_all_180(client, upstream, missing_logo):
    movie = [{"provider_id": i, "provider_name": f"Synthetic Provider {i}",
              "logo_path": missing_logo if i == 1 else f"/movie{i}.png", "display_priority": i + 100}
             for i in range(1, 181)]
    television = [
        {"provider_id": 1, "provider_name": "Synthetic Provider 1", "logo_path": "/recovered.svg", "display_priority": 1},
        {"provider_id": 2, "provider_name": "Synthetic Provider 2", "logo_path": "/alternative.png", "display_priority": 2},
        {"provider_id": 2, "provider_name": "Synthetic Provider 2", "logo_path": "/alternative.png", "display_priority": 2},
        {"provider_id": 2, "provider_name": "Synthetic Provider 2", "logo_path": "https://unrelated.invalid/logo.png", "display_priority": 2},
        {"provider_id": 3, "provider_name": "Synthetic Provider 3", "logo_path": None, "display_priority": 3},
    ]

    def respond(path, request):
        if path == "watch/providers/regions":
            return {"results": [{"iso_3166_1": "US", "english_name": "United States"}]}
        assert request.url.params["watch_region"] == "US"
        assert path in {"watch/providers/movie", "watch/providers/tv"}
        return {"results": movie if path.endswith("movie") else television}

    calls = upstream(respond)
    body = client.get("/api/discovery/providers").json()
    assert body["available"] is True
    assert len(body["providers"]) == 180
    by_id = {row["id"]: row for row in body["providers"]}
    assert set(by_id) == set(range(1, 181))
    assert by_id[1]["logo"] == "https://image.tmdb.org/t/p/original/recovered.svg"
    assert by_id[1]["types"] == ["movie", "tv_show"]
    assert by_id[1]["displayPriority"] == 1
    assert by_id[2]["logo"] == "https://image.tmdb.org/t/p/w92/movie2.png"
    assert by_id[2]["logoAlternatives"] == ["https://image.tmdb.org/t/p/w92/alternative.png"]
    assert by_id[3]["logo"] == "https://image.tmdb.org/t/p/w92/movie3.png"
    assert body["providers"][-1]["id"] == 180
    assert client.get("/api/discovery/providers").json() == body
    assert len(calls) == 3, "The complete catalogue must reuse cached movie/TV/region responses"


def test_network_catalogue_pages_complete_index_with_only_24_metadata_requests_and_featured_order(client, monkeypatch, upstream):
    featured = [49, 67, 88, 174, 74, 64]
    rows = [{"id": i, "name": f"Synthetic Channel {i}"} for i in range(1000, 1035)]
    rows += [{"id": i, "name": f"Z Synthetic Featured {i}"} for i in reversed(featured)]
    rows.append({"id": 9999, "name": "Synthetic Tail Channel"})
    _index(monkeypatch, rows)
    names = {row["id"]: row["name"] for row in rows}

    def respond(path, _request):
        assert path.startswith("network/") and not path.endswith("/images")
        identifier = int(path.split("/")[1])
        return _network(identifier, name=names[identifier])

    calls = upstream(respond)
    first = client.get("/api/discovery/networks").json()
    assert first["available"] is True and first["total"] == 42
    assert first["partial"] is False and first["hasNextPage"] is True
    assert len(first["networks"]) == len(calls) == 24
    assert [row["id"] for row in first["networks"][:6]] == featured
    second = client.get("/api/discovery/networks", params={"page": 2}).json()
    assert len(second["networks"]) == 18 and second["hasNextPage"] is False
    assert {row["id"] for row in first["networks"] + second["networks"]} == set(names)
    assert len(calls) == 42
    # Searching the full index must find a channel outside the initial page.
    found = client.get("/api/discovery/networks", params={"q": "  TAIL  "}).json()
    assert found["total"] == 1 and found["hasNextPage"] is False
    assert [row["id"] for row in found["networks"]] == [9999]
    assert len(calls) == 42, "Already resolved metadata must remain cached"
    empty = client.get("/api/discovery/networks", params={"page": 3}).json()
    assert empty["available"] is True and empty["networks"] == [] and empty["hasNextPage"] is False
    assert len(calls) == 42


@pytest.mark.parametrize("bad_id", [True, 49.0, "49", 50, None])
def test_network_metadata_requires_exact_integer_identity(client, monkeypatch, upstream, bad_id):
    _index(monkeypatch, [{"id": 49, "name": "Synthetic Network"}])
    upstream(lambda path, _request: _network(bad_id) if path == "network/49" else pytest.fail(path))
    body = client.get("/api/discovery/networks").json()
    assert body["available"] is True and body["total"] == 1
    assert body["networks"] == [] and body["partial"] is True


def test_missing_network_logo_uses_valid_svg_and_distinct_image_alternatives(client, monkeypatch, upstream):
    _index(monkeypatch, [{"id": 49, "name": "Synthetic Network"}])

    def respond(path, _request):
        if path == "network/49":
            return _network(49, logo_path=None)
        assert path == "network/49/images"
        return {"logos": [{"file_path": value} for value in
                          ("https://unrelated.invalid/a.png", "/logo.svg", "/alternative.png",
                           "/logo.svg", "/../unsafe.png", "/bad.png?query=1")]}

    calls = upstream(respond)
    body = client.get("/api/discovery/networks").json()
    assert body["partial"] is False
    assert body["networks"][0]["logo"] == "https://image.tmdb.org/t/p/original/logo.svg"
    assert body["networks"][0]["logoAlternatives"] == ["https://image.tmdb.org/t/p/w185/alternative.png"]
    assert len(calls) == 2


def test_network_missing_all_logos_keeps_usable_identity_when_images_fail(client, monkeypatch, upstream):
    _index(monkeypatch, [{"id": 49, "name": "Synthetic Network"}])

    def respond(path, _request):
        if path == "network/49":
            return _network(49, logo_path="//unrelated.invalid/logo.png", origin_country="invalid")
        assert path == "network/49/images"
        return httpx.Response(503)

    upstream(respond)
    body = client.get("/api/discovery/networks").json()
    assert body["available"] is True and body["partial"] is False
    assert body["networks"] == [{"id": 49, "name": "Synthetic Network 49", "logo": None,
                                 "logoAlternatives": [], "country": ""}]


def test_network_browse_uses_tv_with_networks_and_preserves_page(client, upstream):
    def respond(path, request):
        if path == "network/49":
            return _network(49)
        assert path == "discover/tv"
        assert request.url.params["with_networks"] == "49"
        assert request.url.params["page"] == "2"
        assert "with_watch_providers" not in request.url.params
        assert "watch_region" not in request.url.params
        return {"results": [_show(10)], "page": 2, "total_pages": 3}

    calls = upstream(respond)
    body = client.get("/api/discovery", params={"network": 49, "type": "all", "page": 2}).json()
    assert body["available"] is True and body["type"] == "tv_show"
    assert body["networkName"] == "Synthetic Network 49" and body["hasNextPage"] is True
    assert body["items"][0]["availability"] == "unchecked"
    assert "not current streaming availability" in body["attribution"]
    assert len(calls) == 2


def test_network_search_checks_title_identity_and_membership_excludes_unknowns_and_is_partial(client, upstream):
    def respond(path, request):
        if path == "network/49":
            return _network(49)
        if path == "search/tv":
            assert request.url.params["query"] == "Synthetic"
            assert "with_networks" not in request.url.params
            return {"results": [_show(i) for i in range(10, 16)], "total_pages": 2}
        identifier = int(path.split("/")[1])
        assert path == f"tv/{identifier}"
        if identifier == 10:
            return {"id": 10, "networks": [{"id": 49}]}
        if identifier == 11:
            return {"id": 11, "networks": [{"id": 67}]}
        if identifier == 12:
            return {"id": 12}  # Unknown network attribution is not a known miss.
        if identifier == 13:
            return httpx.Response(503)
        if identifier == 14:
            return {"id": 999, "networks": [{"id": 49}]}  # Wrong title cannot pass.
        return {"id": 15, "networks": [{"id": "49"}, {"id": True}, {"id": 49.0}]}

    calls = upstream(respond)
    body = client.get("/api/discovery", params={"network": 49, "type": "tv_show", "q": " Synthetic "}).json()
    assert body["available"] is True and body["partial"] is True
    assert body["hasNextPage"] is True
    assert body["searchScope"] == "network_checked_search_page"
    assert [item["id"] for item in body["items"]] == ["10"]
    assert len(calls) == 8


def test_known_empty_network_search_page_retains_next_page_without_partial_flag(client, upstream):
    def respond(path, _request):
        if path == "network/49":
            return _network(49)
        if path == "search/tv":
            return {"results": [_show(10)], "total_pages": 2}
        assert path == "tv/10"
        return {"id": 10, "networks": []}

    upstream(respond)
    body = client.get("/api/discovery", params={"network": 49, "type": "tv_show", "q": "Synthetic"}).json()
    assert body["items"] == [] and body["hasNextPage"] is True
    assert body["available"] is True and body["partial"] is False


@pytest.mark.parametrize("path,params", [
    ("/api/discovery/networks", {"page": 0}),
    ("/api/discovery/networks", {"page": 1001}),
    ("/api/discovery/networks", {"q": "x" * 201}),
    ("/api/discovery", {"network": 0, "type": "tv_show"}),
    ("/api/discovery", {"network": 2_147_483_648, "type": "tv_show"}),
    ("/api/discovery", {"network": "49.5", "type": "tv_show"}),
])
def test_network_route_limits_reject_invalid_input_before_io(client, monkeypatch, path, params):
    _no_io(monkeypatch)
    assert client.get(path, params=params).status_code == 422


@pytest.mark.parametrize("params", [
    {"provider": 8, "network": 49, "type": "tv_show"},
    {"network": 49, "type": "movie"},
])
def test_network_and_provider_are_exclusive_and_networks_cannot_filter_movies(client, monkeypatch, params):
    _no_io(monkeypatch)
    assert client.get("/api/discovery", params=params).status_code == 400


@pytest.mark.parametrize("route", ["/api/discovery/networks", "/api/discovery?network=49&type=tv_show"])
def test_network_routes_require_auth_before_io(client, monkeypatch, route):
    config.settings.require_auth = True
    _no_io(monkeypatch)
    assert client.get(route).status_code == 401


def test_private_public_member_requires_approval_before_discovery_io(client, monkeypatch):
    config.settings.require_auth = True
    config.settings.private_indexer_enabled = True
    config.settings.private_indexer_secret = "s" * 40
    config.settings.private_indexer_url = "https://library.example.org"
    config.settings.trust_npm_headers = True
    config.settings.proxy_auth_secret = "p" * 40
    headers = {"host": "library.example.org", "x-auth-user-id": "synthetic-unapproved-member",
               "x-auth-priv": "VIEWER", "x-bitagent-proxy-proof": "p" * 40}

    async def unapproved(_user_id):
        return False

    monkeypatch.setattr(private_indexer, "member_active", unapproved)
    _no_io(monkeypatch)
    for route in ("/api/discovery/networks", "/api/discovery?network=49&type=tv_show"):
        assert client.get(route, headers=headers).status_code == 403


def test_unconfigured_network_discovery_never_fetches_export_or_metadata(client, monkeypatch):
    config.settings.tmdb_api_key = ""
    _no_io(monkeypatch)
    for route in ("/api/discovery/networks", "/api/discovery?network=49&type=all"):
        body = client.get(route).json()
        assert body["available"] is False and body["reason"] == "not_configured"


def test_network_index_budget_returns_explicit_unavailable(client, monkeypatch):
    async def blocked():
        await asyncio.Event().wait()

    monkeypatch.setattr(network_catalog, "get_index", blocked)
    monkeypatch.setattr(discovery, "_PROVIDER_CHECK_BUDGET", 0.01)
    monkeypatch.setattr(tmdb, "_get_client", lambda: pytest.fail("No metadata call after index timeout"))
    body = client.get("/api/discovery/networks").json()
    assert body["available"] is False and body["reason"] == "upstream_unavailable"


def test_concurrent_network_pages_share_metadata_fetches_and_respect_upstream_concurrency(monkeypatch):
    rows = [{"id": i, "name": f"Synthetic Network {i}"} for i in range(1000, 1030)]
    _index(monkeypatch, rows)

    async def run():
        active, maximum, calls = 0, 0, []

        async def respond(request):
            nonlocal active, maximum
            calls.append(request)
            active += 1
            maximum = max(maximum, active)
            try:
                await asyncio.sleep(0)  # Allow both callers and queued requests to overlap.
                identifier = int(request.url.path.rsplit("/", 1)[1])
                return httpx.Response(200, json=_network(identifier))
            finally:
                active -= 1

        async with httpx.AsyncClient(transport=httpx.MockTransport(respond)) as client:
            monkeypatch.setattr(tmdb, "_get_client", lambda: client)
            first, concurrent = await asyncio.gather(discovery.get_networks(), discovery.get_networks())
            assert first == concurrent and len(first["networks"]) == 24
            assert len(calls) == 24, "Concurrent pages must share each metadata request"
            assert maximum <= 4, "Metadata fanout must respect the shared upstream budget"
            assert await discovery.get_networks() == first
            assert len(calls) == 24

    asyncio.run(run())


@pytest.mark.parametrize("abandon_consumer", [False, True], ids=["connected", "cancelled"])
def test_queued_upstream_request_expires_safely_and_never_starts_when_slots_reopen(monkeypatch, abandon_consumer):
    async def run():
        saturated, release = asyncio.Event(), asyncio.Event()
        calls = []

        async def respond(request):
            calls.append(request.url.path)
            if len(calls) == 4:
                saturated.set()
            await release.wait()
            return httpx.Response(200, json={"id": int(request.url.path.rsplit("/", 1)[1])})

        async with httpx.AsyncClient(transport=httpx.MockTransport(respond)) as client:
            monkeypatch.setattr(tmdb, "_get_client", lambda: client)
            # Give the active calls a longer budget, then expire only the queued call.
            monkeypatch.setattr(discovery, "_UPSTREAM_REQUEST_BUDGET", 1.0)
            blockers = [asyncio.create_task(discovery._cached_get(f"tv/{i}", {}, 60)) for i in range(1, 5)]
            try:
                await asyncio.wait_for(saturated.wait(), timeout=0.2)
                monkeypatch.setattr(discovery, "_UPSTREAM_REQUEST_BUDGET", 0.02)
                if abandon_consumer:
                    queued = asyncio.create_task(discovery._cached_get("tv/999", {}, 60))
                    await asyncio.sleep(0)
                    queued.cancel()
                    with pytest.raises(asyncio.CancelledError):
                        await queued
                    # Another caller attaches to the shielded fetch left by the cancelled check.
                with pytest.raises(discovery.DiscoveryUnavailable, match="^upstream_unavailable$"):
                    await asyncio.wait_for(discovery._cached_get("tv/999", {}, 60), timeout=0.2)
                assert len(calls) == 4, "An expired queue waiter must never contact TMDB"
            finally:
                release.set()
                await asyncio.wait_for(asyncio.gather(*blockers), timeout=0.2)
            await asyncio.sleep(0)  # Drain cancellation/completion callbacks after slots reopen.
            assert len(calls) == 4 and set(calls) == {f"/3/tv/{i}" for i in range(1, 5)}

    asyncio.run(run())


def test_selected_network_metadata_deadline_returns_explicit_unavailable(monkeypatch):
    cancelled = []

    async def slow_network(_identifier):
        try:
            await asyncio.Event().wait()
        except asyncio.CancelledError:
            cancelled.append(True)
            raise

    monkeypatch.setattr(discovery, "_network_details", slow_network)
    monkeypatch.setattr(discovery, "_PROVIDER_CHECK_BUDGET", 0.02)
    monkeypatch.setattr(tmdb, "_get_client", lambda: pytest.fail("No title feed before network metadata resolves"))

    async def run():
        body = await asyncio.wait_for(discovery.get_discovery(network=49, type="tv_show"), timeout=0.2)
        assert body["available"] is False and body["reason"] == "upstream_unavailable"
        assert body["items"] == [] and body["hasNextPage"] is False
        assert cancelled == [True], "Selected-network metadata must be cancelled at its deadline"

    asyncio.run(run())


def test_slow_network_search_attribution_is_excluded_as_unknown_and_disclosed_partial(monkeypatch):
    cancelled = []

    async def request(path, _params, _ttl):
        if path == "network/49":
            return _network(49)
        if path == "search/tv":
            return {"results": [_show(10)], "total_pages": 2}
        assert path == "tv/10"
        try:
            await asyncio.Event().wait()
        except asyncio.CancelledError:
            cancelled.append(True)
            raise

    monkeypatch.setattr(discovery, "_cached_get", request)
    monkeypatch.setattr(discovery, "_PROVIDER_CHECK_BUDGET", 0.02)

    async def run():
        body = await asyncio.wait_for(
            discovery.get_discovery(network=49, type="tv_show", q="Synthetic"), timeout=0.2)
        assert body["available"] is True and body["items"] == []
        assert body["partial"] is True and body["hasNextPage"] is True
        assert body["searchScope"] == "network_checked_search_page"
        assert cancelled == [True]

    asyncio.run(run())


def test_export_parser_deduplicates_strict_ids_and_preserves_complete_names():
    rows = [{"id": 2, "name": " Z Synthetic "}, {"id": 1, "name": "A Synthetic"},
            {"id": 2, "name": "B Synthetic"}, {"id": True, "name": "Invalid Boolean"},
            {"id": 3.0, "name": "Invalid Float"}, {"id": "4", "name": "Invalid String"},
            {"id": 0, "name": "Invalid Zero"}, {"id": 2_147_483_648, "name": "Invalid Large"},
            {"id": 5, "name": " "}]
    assert network_catalog._parse_export(_export(rows)) == [
        {"id": 1, "name": "A Synthetic"}, {"id": 2, "name": "B Synthetic"}]


@pytest.mark.parametrize("content", [
    b"not a gzip file", gzip.compress(b"not json\n"), gzip.compress(b"[]\n"),
    gzip.compress(b"{\"id\": 1, \"name\": \"\xff\"}\n"), gzip.compress(b""),
    b"\x1f\x8b\x08\x00" + b"\x00" * 6 + b"\xff\xff\xff\xff",
])
def test_export_malformed_gzip_or_rows_raise_safe_failure(content):
    with pytest.raises(network_catalog.NetworkIndexUnavailable):
        network_catalog._parse_export(content)


@pytest.mark.parametrize("bound", ["compressed", "decompressed", "networks"])
def test_export_bounds_reject_oversize_content(monkeypatch, bound):
    rows = [{"id": i, "name": "Synthetic Network"} for i in range(1, 4)]
    content = _export(rows)
    if bound == "compressed":
        monkeypatch.setattr(network_catalog, "_MAX_COMPRESSED", len(content) - 1)
    elif bound == "decompressed":
        monkeypatch.setattr(network_catalog, "_MAX_DECOMPRESSED", 20)
    else:
        monkeypatch.setattr(network_catalog, "_MAX_NETWORKS", 2)
    with pytest.raises(network_catalog.NetworkIndexUnavailable):
        network_catalog._parse_export(content)


def test_export_today_404_uses_yesterday_without_sending_tmdb_credential(monkeypatch):
    class FixedDate(datetime):
        @classmethod
        def now(cls, tz=None):
            return cls(2026, 1, 2, tzinfo=timezone.utc)

    monkeypatch.setattr(network_catalog, "datetime", FixedDate)
    calls = []

    def respond(request):
        calls.append(request)
        assert request.url.host == "files.tmdb.org"
        assert "api_key" not in request.url.params and "authorization" not in request.headers
        assert config.settings.tmdb_api_key not in str(request.url)
        if request.url.path.endswith("01_02_2026.json.gz"):
            return httpx.Response(404)
        assert request.url.path.endswith("01_01_2026.json.gz")
        return httpx.Response(200, content=_export([{"id": 49, "name": "Synthetic Network"}]))

    async def run():
        async with httpx.AsyncClient(transport=httpx.MockTransport(respond)) as client:
            monkeypatch.setattr(tmdb, "_get_client", lambda: client)
            assert await network_catalog.get_index() == [{"id": 49, "name": "Synthetic Network"}]
            assert await network_catalog.get_index() == [{"id": 49, "name": "Synthetic Network"}]

    asyncio.run(run())
    assert len(calls) == 2


@pytest.mark.parametrize("status,expected_calls", [(404, 2), (503, 1)])
def test_export_failure_is_safe_and_only_404_tries_prior_day(monkeypatch, status, expected_calls):
    calls = []

    def respond(request):
        calls.append(request)
        return httpx.Response(status)

    async def run():
        async with httpx.AsyncClient(transport=httpx.MockTransport(respond)) as client:
            monkeypatch.setattr(tmdb, "_get_client", lambda: client)
            with pytest.raises(network_catalog.NetworkIndexUnavailable):
                await network_catalog.get_index()

    asyncio.run(run())
    assert len(calls) == expected_calls


def test_export_download_rejects_oversize_stream_before_consuming_remaining_chunks(monkeypatch):
    monkeypatch.setattr(network_catalog, "_MAX_COMPRESSED", 32)

    class OversizeStream(httpx.AsyncByteStream):
        async def __aiter__(self):
            yield b"x" * 33
            pytest.fail("The export download must stop at its compressed byte bound")

    async def run():
        transport = httpx.MockTransport(lambda _request: httpx.Response(200, stream=OversizeStream()))
        async with httpx.AsyncClient(transport=transport) as client:
            monkeypatch.setattr(tmdb, "_get_client", lambda: client)
            with pytest.raises(network_catalog.NetworkIndexUnavailable):
                await network_catalog.get_index()

    asyncio.run(run())


def test_export_concurrent_callers_share_fetch_survive_cancellation_and_cache(monkeypatch):
    async def run():
        entered, release = asyncio.Event(), asyncio.Event()
        calls = []

        async def respond(request):
            calls.append(request)
            entered.set()
            await release.wait()
            return httpx.Response(200, content=_export([{"id": 49, "name": "Synthetic Network"}]))

        async with httpx.AsyncClient(transport=httpx.MockTransport(respond)) as client:
            monkeypatch.setattr(tmdb, "_get_client", lambda: client)
            abandoned = asyncio.create_task(network_catalog.get_index())
            await asyncio.wait_for(entered.wait(), timeout=1)
            waiting = [asyncio.create_task(network_catalog.get_index()) for _ in range(8)]
            abandoned.cancel()
            with pytest.raises(asyncio.CancelledError):
                await abandoned
            release.set()
            results = await asyncio.wait_for(asyncio.gather(*waiting), timeout=1)
            assert all(rows == [{"id": 49, "name": "Synthetic Network"}] for rows in results)
            assert await network_catalog.get_index() == results[0]
            assert len(calls) == 1

    asyncio.run(run())


def test_stalled_export_stream_expires_for_shared_callers_and_allows_fresh_retry(monkeypatch):
    monkeypatch.setattr(network_catalog, "_DOWNLOAD_BUDGET", 0.02)

    async def run():
        entered, release = asyncio.Event(), asyncio.Event()
        calls, cancelled, closed = [], [], []
        expected = [{"id": 49, "name": "Synthetic Retry Network"}]

        class StalledStream(httpx.AsyncByteStream):
            async def __aiter__(self):
                entered.set()
                try:
                    await release.wait()
                    yield _export(expected)
                except asyncio.CancelledError:
                    cancelled.append(True)
                    raise

            async def aclose(self):
                closed.append(True)

        def respond(request):
            calls.append(request)
            if len(calls) == 1:
                return httpx.Response(200, stream=StalledStream())
            return httpx.Response(200, content=_export(expected))

        try:
            async with httpx.AsyncClient(transport=httpx.MockTransport(respond)) as client:
                monkeypatch.setattr(tmdb, "_get_client", lambda: client)
                first = asyncio.create_task(network_catalog.get_index())
                await asyncio.wait_for(entered.wait(), timeout=0.2)
                waiters = [first, *(asyncio.create_task(network_catalog.get_index()) for _ in range(4))]
                results = await asyncio.wait_for(asyncio.gather(*waiters, return_exceptions=True), timeout=0.2)
                assert all(isinstance(result, network_catalog.NetworkIndexUnavailable) for result in results)
                assert len(calls) == 1, "Shared callers must not multiply the stalled download"
                assert cancelled == [True] and closed == [True]
                assert await network_catalog.get_index() == expected
                assert len(calls) == 2, "A failed shared task must permit a fresh retry"
                assert await network_catalog.get_index() == expected
                assert len(calls) == 2, "The successful retry must populate the cache"
        finally:
            release.set()

    asyncio.run(run())

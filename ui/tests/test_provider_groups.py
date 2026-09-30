"""Brand consolidation preserves genuine logos and exact regional availability."""
from __future__ import annotations

import asyncio

import httpx
import pytest

import config
import discovery
import provider_catalog
import tmdb


def _raw(identifier, name, types=("movie", "tv_show"), *, priority=5, logo=None):
    return {"id": identifier, "name": name, "types": list(types), "displayPriority": priority,
            "logo": logo or f"https://image.tmdb.org/t/p/w92/logo-{identifier}.png",
            "logoAlternatives": []}


def _paramount_rows():
    # The media distribution is deliberately synthetic: a brand's channel can
    # support one type while another channel supports both in the same market.
    return [
        _raw(2303, "Paramount Plus Premium", priority=10),
        _raw(2616, "Paramount Plus Essential", ("tv_show",)),
        _raw(582, "Paramount+ Amazon Channel", ("movie",), priority=0),
        _raw(1853, "Paramount Plus Apple TV channel", ("tv_show",)),
        _raw(633, "Paramount+ Roku Premium Channel"),
    ]


def _group_for(rows, member):
    return next(row for row in rows if member in row["providerIds"])


def test_brand_variants_keep_every_id_alias_and_media_scope_with_the_direct_logo():
    raw = _paramount_rows()
    groups = provider_catalog.group_providers(raw)
    assert len(groups) == 1
    group = groups[0]
    assert group["name"] == "Paramount+"
    assert group["id"] == 2303
    assert group["logo"] == raw[0]["logo"], "a high-priority storefront tile must not replace the direct brand logo"
    assert group["providerIds"] == [582, 633, 1853, 2303, 2616]
    assert group["idsByType"] == {"movie": [582, 633, 2303], "tv_show": [633, 1853, 2303, 2616]}
    assert set(group["types"]) == {"movie", "tv_show"}
    assert {row["name"] for row in raw} <= set(group["aliases"])


def test_brand_aliases_do_not_swallow_unrelated_name_prefixes():
    unrelated = [
        _raw(9001, "Paramount Pictures"),
        _raw(9002, "Paramount Plus Sports"),
        _raw(9003, "Paramount+ Documentary Club"),
        _raw(9004, "Paramount Network"),
    ]
    groups = provider_catalog.group_providers(_paramount_rows() + unrelated)
    assert len(groups) == 5
    assert _group_for(groups, 2303)["providerIds"] == [582, 633, 1853, 2303, 2616]
    for row in unrelated:
        untouched = _group_for(groups, row["id"])
        assert untouched["providerIds"] == [row["id"]]
        assert untouched["name"] == row["name"]


def test_subscription_brands_do_not_absorb_stores_or_plain_cable_names():
    raw = [
        _raw(119, "Amazon Prime Video"), _raw(9, "Amazon Video"),
        _raw(350, "Apple TV+"), _raw(2, "Apple TV Store"),
        _raw(528, "AMC+"), _raw(9005, "AMC"),
    ]
    groups = provider_catalog.group_providers(raw)
    assert len(groups) == 6
    for row in raw:
        assert _group_for(groups, row["id"])["providerIds"] == [row["id"]]


def test_standalone_roku_service_keeps_its_complete_name_and_brand_identity():
    groups = provider_catalog.group_providers([
        _raw(207, "The Roku Channel"), _raw(633, "Paramount+ Roku Premium Channel")])
    roku = _group_for(groups, 207)
    assert roku["name"] == "The Roku Channel"
    assert roku["brandKey"] == "the roku channel"
    assert roku["providerIds"] == [207]
    assert _group_for(groups, 633)["name"] == "Paramount+"


def test_historical_direct_id_is_retained_when_present_in_the_regional_catalogue():
    groups = provider_catalog.group_providers([
        _raw(531, "Paramount Plus"), _raw(582, "Paramount+ Amazon Channel")])
    assert len(groups) == 1
    assert groups[0]["providerIds"] == [531, 582]
    assert groups[0]["name"] == "Paramount+"


def test_normalized_brand_key_survives_country_specific_case_and_unicode_display_names():
    first = provider_catalog.group_providers([_raw(446, "Ｒｅｔｒｏｃｒｕｓｈ")])[0]
    second = provider_catalog.group_providers([_raw(295, "RetroCrush Amazon Channel")])[0]
    assert first["brandKey"] == second["brandKey"] == "retrocrush"
    assert first["providerIds"] == [446]
    assert second["providerIds"] == [295]


def test_differing_movie_and_tv_names_for_one_raw_id_remain_searchable_aliases(client, mock_tmdb):
    names = {"watch/providers/movie": "Paramount Plus Premium", "watch/providers/tv": "Paramount+ Premium"}

    def responder(path, _request):
        if path in names:
            return {"results": [{"provider_id": 2303, "provider_name": names[path],
                                 "logo_path": "/paramount.png", "display_priorities": {"US": 1}}]}
        return None

    mock_tmdb(responder)
    groups = client.get("/api/discovery/providers").json()["providers"]
    assert len(groups) == 1
    assert groups[0]["providerIds"] == [2303]
    assert groups[0]["idsByType"] == {"movie": [2303], "tv_show": [2303]}
    assert set(names.values()) <= set(groups[0]["aliases"])


@pytest.fixture(autouse=True)
def _provider_state():
    config.settings.require_auth = False
    config.settings.tmdb_api_key = "synthetic-provider-group-key"
    discovery.reset_cache()
    yield
    discovery.reset_cache()


@pytest.fixture
def mock_tmdb(monkeypatch):
    clients = []

    def install(responder=None):
        calls = []

        def route(request):
            calls.append(request)
            path = request.url.path.removeprefix("/3/")
            if responder:
                payload = responder(path, request)
                if payload is not None:
                    return httpx.Response(200, json=payload)
            if path == "watch/providers/regions":
                payload = {"results": [
                    {"iso_3166_1": "US", "english_name": "United States"},
                    {"iso_3166_1": "GB", "english_name": "United Kingdom"}]}
            elif path in ("watch/providers/movie", "watch/providers/tv"):
                region = request.url.params["watch_region"]
                rows = _paramount_rows() if region == "US" else [
                    _raw(2303, "Paramount Plus Premium"),
                    _raw(1853, "Paramount Plus Apple TV channel", ("tv_show",))]
                media_type = "tv_show" if path.endswith("/tv") else "movie"
                payload = {"results": [{"provider_id": row["id"], "provider_name": row["name"],
                                        "logo_path": f"/logo-{row['id']}.png",
                                        "display_priorities": {region: row["displayPriority"]}}
                                       for row in rows if media_type in row["types"]]}
            elif path in ("discover/movie", "discover/tv"):
                payload = {"results": [_title(90, tv=path.endswith("/tv"))], "total_pages": 1}
            else:
                pytest.fail(f"Unexpected synthetic upstream request: {path}")
            return httpx.Response(200, json=payload)

        upstream = httpx.AsyncClient(transport=httpx.MockTransport(route))
        clients.append(upstream)
        monkeypatch.setattr(tmdb, "_get_client", lambda: upstream)
        return calls

    yield install
    for upstream in clients:
        asyncio.run(upstream.aclose())


def _title(identifier, *, tv=False):
    return {"id": identifier, "name" if tv else "title": "Synthetic grouped catalogue",
            "first_air_date" if tv else "release_date": "2026-01-01"}


def test_provider_groups_remain_country_scoped_even_after_another_country_is_cached(client, mock_tmdb):
    mock_tmdb()
    us = client.get("/api/discovery/providers", params={"region": "US"}).json()["providers"]
    gb = client.get("/api/discovery/providers", params={"region": "GB"}).json()["providers"]
    assert len(us) == len(gb) == 1
    assert us[0]["providerIds"] == [582, 633, 1853, 2303, 2616]
    assert gb[0]["providerIds"] == [1853, 2303]
    assert gb[0]["idsByType"] == {"movie": [2303], "tv_show": [1853, 2303]}
    unavailable = client.get("/api/discovery", params={"provider": 582, "region": "GB", "type": "movie"})
    assert unavailable.status_code == 400, "a US storefront ID must not become a GB group member"


@pytest.mark.parametrize(("legacy_id", "media_type", "expected_ids"), [
    (1853, "movie", {582, 633, 2303}),
    (582, "tv_show", {633, 1853, 2303, 2616}),
])
def test_legacy_variant_selection_uses_or_discovery_of_the_brand_media_ids(client, mock_tmdb, legacy_id, media_type, expected_ids):
    calls = mock_tmdb()
    response = client.get("/api/discovery", params={"provider": legacy_id, "type": media_type, "region": "US"})
    assert response.status_code == 200
    body = response.json()
    suffix = "/discover/tv" if media_type == "tv_show" else "/discover/movie"
    request = next(call for call in calls if call.url.path.endswith(suffix))
    encoded = request.url.params["with_watch_providers"]
    assert {int(value) for value in encoded.split("|")} == expected_ids, "a comma would require AND membership instead of the intended OR"
    assert request.url.params["watch_region"] == "US"
    assert body["providerName"] == "Paramount+"
    assert body["providerIds"] == sorted(expected_ids)
    assert body["items"][0]["type"] == media_type
    assert body["items"][0]["availability"] == "unchecked", "catalogue discovery does not prove indexed releases"


def test_all_media_group_feed_queries_each_scope_and_reports_the_regional_id_union(client, mock_tmdb):
    calls = mock_tmdb()
    body = client.get("/api/discovery", params={"provider": 1853, "type": "all", "region": "US"}).json()
    assert body["available"] is True
    assert body["providerName"] == "Paramount+"
    assert body["providerIds"] == [582, 633, 1853, 2303, 2616]
    assert {item["type"] for item in body["items"]} == {"movie", "tv_show"}
    for suffix, expected in (("/discover/movie", {582, 633, 2303}), ("/discover/tv", {633, 1853, 2303, 2616})):
        request = next(call for call in calls if call.url.path.endswith(suffix))
        assert {int(value) for value in request.url.params["with_watch_providers"].split("|")} == expected


def test_known_group_without_movies_returns_empty_instead_of_unfiltered_discovery(client, mock_tmdb):
    def responder(path, _request):
        if path == "watch/providers/movie":
            return {"results": []}
        if path == "watch/providers/tv":
            return {"results": [{"provider_id": 2616, "provider_name": "Paramount Plus Essential",
                                 "logo_path": "/essential.png", "display_priorities": {"US": 1}}]}
        return None

    calls = mock_tmdb(responder)
    body = client.get("/api/discovery", params={"provider": 2616, "type": "movie"}).json()
    assert body["available"] is True
    assert body["items"] == []
    assert body["providerIds"] == []
    assert not any(call.url.path.endswith("/discover/movie") for call in calls)


@pytest.mark.parametrize(("media_type", "expected_items"), [("movie", ["10", "11"]), ("tv_show", ["10", "12"])])
def test_group_search_requires_exact_country_and_media_membership_and_excludes_unknowns(client, mock_tmdb, media_type, expected_items):
    def responder(path, _request):
        if path in ("search/movie", "search/tv"):
            return {"results": [_title(i, tv=path.endswith("/tv")) for i in range(10, 18)], "total_pages": 2}
        if path.endswith("/watch/providers"):
            identifier = int(path.split("/")[1])
            if identifier == 14:
                return {"success": False}
            if identifier == 16:
                return {"results": {"US": {"flatrate": "malformed"}}}
            provider = {10: 2303, 11: 582, 12: 2616, 13: 9001, 15: 582, 17: "582"}[identifier]
            return {"results": {"GB" if identifier == 15 else "US": {"flatrate": [{"provider_id": provider}]}}}
        return None

    mock_tmdb(responder)
    response = client.get("/api/discovery", params={"provider": 633, "type": media_type, "region": "US", "q": "Synthetic"})
    assert response.status_code == 200
    body = response.json()
    assert [item["id"] for item in body["items"]] == expected_items
    assert body["partial"] is True, "failed or malformed provider checks must remain visible as uncertainty"
    assert body["hasNextPage"] is True, "filtering one search page cannot erase the upstream continuation"
    assert body["searchScope"] == "provider_checked_search_page"

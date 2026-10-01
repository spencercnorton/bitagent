"""Private Torznab contract through the ASGI boundary, with synthetic media."""
from __future__ import annotations

import hashlib
from xml.etree import ElementTree as ET

import httpx
import pytest
from fastapi import HTTPException, Response

import config
import private_indexer
import torznab


@pytest.fixture()
def private_feed(monkeypatch):
    monkeypatch.setattr(config.settings, "private_indexer_enabled", True)
    monkeypatch.setattr(config.settings, "torznab_rate_limit_per_min", 0)
    state = {"active": True, "searched": [], "downloads": [], "touched": [], "usage": []}
    key_row = {"id": 901, "user_id": "synthetic-member", "private_access": True}

    async def lookup(key_hash):
        return key_row if key_hash == hashlib.sha256(b"ba_test").hexdigest() else None

    async def member(user_id):
        assert user_id == key_row["user_id"]
        return state["active"]

    async def require_key(row, **kwargs):
        if not state["active"]:
            raise HTTPException(403, "Membership required")
        return row

    async def touch(key_id):
        state["touched"].append(key_id)

    async def usage(user_id):
        state["usage"].append(user_id)

    async def search(params):
        state["searched"].append(params)
        return {
            "offset": 2, "limit": 1, "total": 3,
            "items": [{
                "id": "release-demo", "source_id": "synthetic:movie:1", "title": "Example & Demo <Film>",
                "kind": "movie", "info_hash": "a" * 40, "size": 4096,
                "created_at": 1790726400, "seeders": None, "leechers": 0,
                "imdb_id": "tt0000001", "tmdb_id": "1", "tvdb_id": None,
                "season": None, "episodes": [],
            }],
        }

    async def torrent(release_id, request, row):
        state["downloads"].append((release_id, row["user_id"], request.method))
        return Response(b"synthetic-torrent", media_type="application/x-bittorrent")

    monkeypatch.setattr(torznab, "lookup_user_api_key", lookup)
    monkeypatch.setattr(torznab, "touch_user_api_key", touch)
    monkeypatch.setattr(torznab, "record_api_search", usage)
    monkeypatch.setattr(private_indexer, "member_active", member)
    monkeypatch.setattr(private_indexer, "require_private_key", require_key)
    monkeypatch.setattr(private_indexer, "search_releases", search)
    monkeypatch.setattr(private_indexer, "torrent_response", torrent)
    monkeypatch.setattr(private_indexer, "external_base", lambda request: "https://library.example.org")
    return state


def test_private_feed_disabled_and_requires_member(client, private_feed, monkeypatch):
    monkeypatch.setattr(config.settings, "private_indexer_enabled", False)
    assert client.get("/torznab/private/api?t=caps&apikey=ba_test").status_code == 404
    monkeypatch.setattr(config.settings, "private_indexer_enabled", True)
    assert client.get("/torznab/private/api?t=caps").status_code == 401
    assert client.get("/torznab/private/api?t=caps&apikey=invalid").status_code == 401
    private_feed["active"] = False
    assert client.get("/torznab/private/api?t=caps&apikey=ba_test").status_code == 403
    assert not private_feed["searched"]


def test_private_caps_are_limited_to_implemented_modes(client, private_feed):
    response = client.get("/torznab/private/api?t=caps&apikey=ba_test")
    assert response.status_code == 200
    root = ET.fromstring(response.content)
    assert root.tag == "caps"
    assert root.find("registration").get("open") == "no"
    assert [node.get("id") for node in root.findall("categories/category")] == ["2000", "5000"]
    assert root.find("searching/movie-search").get("supportedParams") == "q,imdbid,tmdbid"
    assert root.find("searching/music-search") is None
    assert root.find("limits").attrib == {"max": "100", "default": "100"}
    assert not private_feed["searched"]


def test_private_feed_torrent_download_and_pagination(client, private_feed):
    response = client.get("/torznab/private/api?t=movie&q=demo&offset=2&limit=1&apikey=ba_test")
    assert response.status_code == 200
    assert response.headers["cache-control"] == "no-store"
    root = ET.fromstring(response.content)
    item = root.find("channel/item")
    assert item.findtext("title") == "Example & Demo <Film>"
    assert item.findtext("guid") == "private:release-demo:" + hashlib.sha256(b"synthetic:movie:1").hexdigest()
    assert item.find("enclosure").get("url") == (
        "https://library.example.org/torznab/private/api?t=get&id=release-demo&apikey=ba_test"
    )
    ns = {"tz": "http://torznab.com/schemas/2015/feed"}
    paging = root.find("channel/tz:response", ns)
    assert paging.attrib == {"offset": "2", "total": "3"}
    attrs = {node.get("name"): node.get("value") for node in item.findall("tz:attr", ns)}
    assert attrs["category"] == "2000"
    assert attrs["size"] == "4096"
    assert attrs["leechers"] == "0"
    assert "seeders" not in attrs
    assert "infohash" not in attrs
    assert "magneturl" not in attrs
    assert b"magnet:" not in response.content
    assert not private_feed["downloads"]
    assert private_feed["usage"] == ["synthetic-member"]


def test_private_usage_failure_preserves_feed_and_redacts_log(client, private_feed, monkeypatch, caplog):
    async def failed_usage(user_id):
        raise RuntimeError("https://synthetic.example.org/?apikey=synthetic-secret")

    monkeypatch.setattr(torznab, "record_api_search", failed_usage)
    response = client.get("/torznab/private/api?t=search&apikey=ba_test")
    assert response.status_code == 200
    assert ET.fromstring(response.content).tag == "rss"
    assert "RuntimeError" in caplog.text
    assert "synthetic-secret" not in caplog.text


def test_private_alias_guids_distinguish_one_shared_swarm():
    release = {
        "id": "shared-release", "source_id": "synthetic:episode:1", "title": "Synthetic Episode",
        "kind": "episode", "size": 4096, "created_at": 1790726400,
    }
    result = {"offset": 0, "total": 2, "items": [release, {**release, "source_id": "synthetic:season:1", "title": "Synthetic Season", "kind": "season"}]}
    feed = ET.fromstring(torznab._private_feed(result, "https://library.example.org", "ba_test"))
    items = feed.findall("channel/item")
    assert items[0].findtext("guid") != items[1].findtext("guid")
    assert items[0].find("enclosure").get("url") == items[1].find("enclosure").get("url")


def test_private_download_uses_authenticated_identity(client, private_feed):
    response = client.get("/torznab/private/api?t=get&id=release-demo", headers={"Authorization": "Bearer ba_test"})
    assert response.status_code == 200
    assert response.content == b"synthetic-torrent"
    assert private_feed["downloads"] == [("release-demo", "synthetic-member", "GET")]
    assert client.get("/torznab/private/api?t=get&apikey=ba_test").status_code == 400


def test_private_feed_rejects_unsupported_or_invalid_search(client, private_feed, monkeypatch):
    response = client.get("/torznab/private/api?t=music&apikey=ba_test")
    assert response.status_code == 400
    assert ET.fromstring(response.content).get("code") == "202"

    async def invalid(params):
        raise HTTPException(422, "Invalid parameter: cat < 0 & unknown")

    monkeypatch.setattr(private_indexer, "search_releases", invalid)
    response = client.get("/torznab/private/api?t=search&cat=bad&apikey=ba_test")
    assert response.status_code == 400
    assert ET.fromstring(response.content).get("description") == "Invalid parameter: cat < 0 & unknown"


def test_private_feed_head_has_no_body(client, private_feed):
    response = client.head("/torznab/private/api?t=caps&apikey=ba_test")
    assert response.status_code == 200
    assert response.content == b""


def test_private_missing_torrent_returns_torznab_error(client, private_feed, monkeypatch):
    async def missing(*args):
        raise HTTPException(404, "Not found")

    monkeypatch.setattr(private_indexer, "torrent_response", missing)
    response = client.get("/torznab/private/api?t=get&id=absent&apikey=ba_test")
    assert response.status_code == 404
    assert ET.fromstring(response.content).get("code") == "300"


def test_dht_proxy_preserves_public_key_when_private_member_is_suspended(client, private_feed, monkeypatch):
    private_feed["active"] = False
    class Client:
        async def __aenter__(self):
            return self
        async def __aexit__(self, *args):
            return False
        async def request(self, *args, **kwargs):
            return httpx.Response(200, content=b"<caps/>", headers={"content-type": "application/xml"})
    monkeypatch.setattr(torznab.httpx, "AsyncClient", lambda **kwargs: Client())
    response = client.get("/torznab/api?t=caps&apikey=ba_test")
    assert response.status_code == 200
    assert client.get("/torznab/private/api?t=caps&apikey=ba_test").status_code == 403
    assert not private_feed["searched"]


def test_dht_proxy_failure_never_logs_upstream_credentials(client, private_feed, monkeypatch, caplog):
    monkeypatch.setattr(config.settings, "private_indexer_enabled", False)
    monkeypatch.setattr(config.settings, "torznab_api_key", "synthetic-core-secret")

    class Client:
        async def __aenter__(self):
            return self

        async def __aexit__(self, *args):
            return False

        async def request(self, *args, **kwargs):
            raise httpx.ConnectError("http://core:3333/torznab/api?apikey=synthetic-core-secret")

    monkeypatch.setattr(torznab.httpx, "AsyncClient", lambda **kwargs: Client())
    response = client.get("/torznab/api?t=caps&apikey=ba_test")
    assert response.status_code == 502
    assert "ConnectError" in caplog.text
    assert "synthetic-core-secret" not in caplog.text
    assert "ba_test" not in caplog.text

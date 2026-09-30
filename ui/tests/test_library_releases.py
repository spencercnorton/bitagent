"""Bounded public title-release paging, using a synthetic upstream catalog."""
from __future__ import annotations

import pytest

import app as app_module
import config
import graphql_client as gql


@pytest.fixture(autouse=True)
def _open_auth_and_empty_blocks(monkeypatch):
    config.settings.require_auth = False

    async def no_blocks():
        return []

    monkeypatch.setattr(app_module, "_load_block_phrases", no_blocks)


def _release(number, *, cid="123", content_type="tv_show", name="Synthetic Show"):
    return {
        "infoHash": f"{number:040x}", "title": name,
        "contentType": content_type, "contentSource": "tmdb", "contentId": cid,
        "seeders": number, "torrent": {"name": f"{name} S01E01", "size": 100},
    }


def _patch_query(monkeypatch, items, *, has_next=None, errors=None):
    captured = {}

    async def query(_query, variables=None, **_kwargs):
        captured.update((variables or {}).get("input", {}))
        if errors:
            return {"data": None, "errors": errors}
        block = {"items": items}
        if has_next is not None:
            block["hasNextPage"] = has_next
        return {"data": {"torrentContent": {"search": block}}}

    monkeypatch.setattr(gql, "query", query)
    return captured


def _params(**extra):
    return {"title": "Synthetic Show", "content_source": "tmdb",
            "content_id": "123", "content_type": "tv_show", **extra}


def test_release_paging_advances_raw_window_when_all_rows_are_filtered(client, monkeypatch):
    captured = _patch_query(monkeypatch, [_release(1, cid="456"), _release(2, cid="789")], has_next=True)
    response = client.get("/api/titles/releases", params=_params(limit=2, offset=4))
    assert response.status_code == 200
    body = response.json()
    assert body["items"] == []
    assert body["totalCount"] == -1
    assert body["scannedCount"] == 2
    assert body["offset"] == 4
    assert body["nextOffset"] == 6
    assert body["hasNextPage"] is True
    assert body["truncated"] is True
    assert captured["limit"] == 2
    assert captured["offset"] == 4
    assert captured["hasNextPage"] is True
    assert captured["totalCount"] is False


def test_release_complete_first_window_reports_exact_count_and_no_cursor(client, monkeypatch):
    _patch_query(monkeypatch, [_release(1), _release(2)], has_next=False)
    body = client.get("/api/titles/releases", params=_params()).json()
    assert body["totalCount"] == 2
    assert body["hasNextPage"] is False
    assert body["truncated"] is False
    assert body["nextOffset"] is None


def test_last_release_window_does_not_claim_page_count_is_full_title_count(client, monkeypatch):
    _patch_query(monkeypatch, [_release(1)], has_next=False)
    body = client.get("/api/titles/releases", params=_params(offset=500)).json()
    assert body["totalCount"] == -1
    assert body["hasNextPage"] is False


def test_release_full_window_on_older_core_is_explicitly_partial(client, monkeypatch):
    _patch_query(monkeypatch, [_release(1), _release(2)])
    body = client.get("/api/titles/releases", params=_params(limit=2)).json()
    assert body["truncated"] is True
    assert body["hasNextPage"] is True
    assert body["nextOffset"] == 2


def test_release_identity_includes_type_when_upstream_facet_is_ignored(client, monkeypatch):
    _patch_query(monkeypatch, [_release(1), _release(2, content_type="movie")], has_next=False)
    body = client.get("/api/titles/releases", params=_params()).json()
    assert [item["infoHash"] for item in body["items"]] == [f"{1:040x}"]


def test_release_upstream_failure_is_not_an_empty_success(client, monkeypatch):
    _patch_query(monkeypatch, [], errors=[{"message": "Synthetic upstream failure"}])
    response = client.get("/api/titles/releases", params=_params())
    assert response.status_code == 502
    assert response.json()["detail"] == "Could not load title releases"


@pytest.mark.parametrize("field,value", [("limit", 0), ("limit", 501), ("offset", -1), ("offset", 100001)])
def test_release_window_parameters_are_bounded(client, field, value):
    assert client.get("/api/titles/releases", params=_params(**{field: value})).status_code == 422

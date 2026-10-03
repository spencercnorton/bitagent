"""Synthetic leaf files through the real private SQLite/ASGI contract."""
import asyncio
import base64
import hashlib
import json
from xml.etree import ElementTree as ET

import pytest

import database
import media_publish as publisher
import private_indexer
import torrent_metainfo
from test_private_indexer import identity, mark_ready, private_config  # noqa: F401


ANNOUNCE = "https://tracker.example.org/announce"
KINDS = ("movie", "episode", "season", "show", "music", "generic")


def prepare(tmp_path, kinds=KINDS):
    source = tmp_path / "synthetic.bin"
    source.write_bytes(b"synthetic private leaf")
    inventory = {"version": 1, "releases": [
        {"source_id": "synthetic:" + kind, "title": "Example " + kind,
         "kind": kind, "files": [source.name]} for kind in kinds
    ]}
    catalog, seeding, _ = publisher.prepare_catalog(inventory, tmp_path, ANNOUNCE, 65536)
    return catalog, seeding


def test_leaf_round_trip_keeps_one_swarm_aliases_and_fresh_readiness(client, tmp_path):
    catalog, seeding = prepare(tmp_path)
    owner, member = identity("owner", "OWNER"), identity()
    assert len({r["info_hash"] for r in catalog["releases"]}) == 1
    assert all(not row["seed_verified"] for row in seeding["releases"])
    response = client.post("/api/private/catalog/import?ack=1", json=catalog, headers=owner)
    assert response.status_code == 200, response.text
    ack = response.json()
    release_id = ack["results"][0]["id"]
    assert len({row["id"] for row in ack["results"]}) == 1
    snapshot = ack["catalog_snapshot"]
    aliases = client.get("/api/private/catalog/page?view=aliases", headers=owner).json()
    assert aliases["snapshot"]["alias_total"] == 6
    assert aliases["snapshot"]["release_total"] == 1
    assert {row["kind"] for row in aliases["items"]} == set(KINDS)
    assert all(row["metadata"] == {} and row["ready"] == 0 for row in aliases["items"])
    assert str(tmp_path) not in json.dumps(aliases)
    assert "synthetic.bin" not in json.dumps(aliases)
    assert client.post("/api/private/catalog/import?ack=1", json=catalog, headers=owner).json()["catalog_snapshot"] == snapshot

    assert client.put("/api/private/members/member", json={"active": True}, headers=owner).status_code == 200
    assert client.get("/api/library/private", headers=member).json()["items"] == []
    asyncio.run(mark_ready(release_id))  # synthetic proof, never a production seed assertion
    assert {row["kind"] for row in client.get("/api/library/private", headers=member).json()["items"]} == set(KINDS)
    public_key = client.post("/api/account/api-key", headers=member, json={"expectedAccountId": "member"}).json()["apiKeySecret"]
    assert client.get("/torznab/private/api", params={"t": "music", "apikey": public_key}).status_code == 403
    private_key = client.post("/api/account/api-key", headers=member,
                              json={"expectedAccountId": "member", "privateAccess": True}).json()["apiKeySecret"]
    response = client.get("/torznab/private/api", params={"t": "music", "apikey": private_key})
    assert response.status_code == 200
    items = ET.fromstring(response.content).findall("channel/item")
    assert len(items) == 1 and items[0].findtext("title") == "Example music"
    attrs = {n.get("name"): n.get("value") for n in items[0].findall("{*}attr")}
    assert attrs["category"] == "3000"
    assert "magneturl" not in attrs and "infohash" not in attrs
    torrent = client.get(f"/private/torrents/{release_id}.torrent", params={"apikey": private_key})
    assert torrent.status_code == 200
    decoded = torrent_metainfo.bdecode(torrent.content)
    original = torrent_metainfo.bdecode(base64.b64decode(catalog["releases"][0]["metainfo_base64"]))
    assert decoded[b"info"] == original[b"info"]
    assert decoded[b"info"][b"private"] == 1
    assert decoded[b"announce"] != original[b"announce"]
    assert client.patch(f"/api/private/catalog/{release_id}", json={"published": False}, headers=owner).status_code == 200
    assert client.get("/api/library/private", headers=member).json()["items"] == []
    assert client.get(f"/private/torrents/{release_id}.torrent", params={"apikey": private_key}).status_code == 404


@pytest.mark.parametrize("params, expected", [
    ({"t": "movie"}, {"movie"}),
    ({"t": "tvsearch"}, {"episode", "season", "show"}),
    ({"t": "music"}, {"music"}),
    ({"cat": "2000"}, {"movie"}),
    ({"cat": "3000"}, {"music"}),
    ({"cat": "5000"}, {"episode", "season", "show"}),
    ({"cat": "8000"}, {"generic"}),
    ({"cat": "3000,8000"}, {"music", "generic"}),
    ({"cat": "7000"}, set()),
    ({"t": "tvsearch", "cat": "3000,8000"}, set()),
    ({"t": "music", "cat": "8000"}, set()),
    ({"q": "Example gen"}, {"generic"}),
])
def test_search_kinds_do_not_cross_video_audio_other_boundaries(client, tmp_path, params, expected):
    catalog, _ = prepare(tmp_path)
    assert client.post("/api/private/catalog/import", json=catalog, headers=identity("owner", "OWNER")).status_code == 200
    asyncio.run(mark_ready())
    result = asyncio.run(private_indexer.search_releases(params))
    assert {row["kind"] for row in result["items"]} == expected
    assert result["total"] == len(expected)


@pytest.mark.parametrize("kind", ["music", "generic"])
def test_new_canonical_kinds_enumerate_and_cannot_assert_readiness(client, tmp_path, kind):
    catalog, _ = prepare(tmp_path, [kind])
    catalog["releases"][0]["seed_verified"] = True
    owner = identity("owner", "OWNER")
    assert client.post("/api/private/catalog/import", json=catalog, headers=owner).status_code == 200
    result = client.get("/api/private/catalog/page?view=releases", headers=owner)
    assert result.status_code == 200
    assert [(row["kind"], row["ready"]) for row in result.json()["items"]] == [(kind, 0)]


def test_unknown_kind_rejects_whole_batch_without_changing_catalog(client, tmp_path):
    catalog, _ = prepare(tmp_path, ["music", "generic"])
    catalog["releases"][1]["kind"] = "unknown"
    response = client.post("/api/private/catalog/import", json=catalog, headers=identity("owner", "OWNER"))
    assert response.status_code == 422
    async def count():
        return (await (await database.get_db()).execute_fetchall("SELECT count(*) n FROM private_releases"))[0]["n"]
    assert asyncio.run(count()) == 0


@pytest.mark.parametrize("kind", KINDS)
def test_kind_does_not_change_legacy_torrent_info_bytes(tmp_path, kind):
    catalog, _ = prepare(tmp_path, [kind])
    release = catalog["releases"][0]
    info = {b"name": b"synthetic.bin", b"length": 22, b"private": 1,
            b"piece length": 65536, b"pieces": hashlib.sha1(b"synthetic private leaf").digest()}
    assert torrent_metainfo.bdecode(base64.b64decode(release["metainfo_base64"]))[b"info"] == info
    assert release["info_hash"] == hashlib.sha1(torrent_metainfo.bencode(info)).hexdigest()

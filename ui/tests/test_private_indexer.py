"""Real SQLite/ASGI authorization, torrent issuance and tracker accounting."""
import asyncio
import base64
import hashlib
import time
from urllib.parse import urlencode, urlsplit

import pytest

import config
import database
import private_indexer as private
import torznab


PROOF = "p" * 40


def identity(user="member", role="VIEWER"):
    return {"x-auth-user-id": user, "x-auth-priv": role, "x-bitagent-proxy-proof": PROOF}


@pytest.fixture(autouse=True)
def private_config():
    config.settings.private_indexer_enabled = True
    config.settings.private_indexer_secret = "s" * 40
    config.settings.private_indexer_url = "https://library.example.org"
    config.settings.require_auth = True
    config.settings.trust_npm_headers = True
    config.settings.proxy_auth_secret = PROOF
    config.settings.trusted_proxy_cidrs = "127.0.0.0/8"
    torznab._TZ_BUCKETS.clear()
    async def clear():
        db = await database.get_db()
        for table in ("private_members", "private_releases", "private_release_aliases", "private_peers", "private_tracker_keys", "private_transfer_totals", "user_api_keys"):
            await db.execute("DELETE FROM " + table)
        await db.commit()
    asyncio.run(clear())


def metainfo():
    from torrent_metainfo import bencode
    data = b"synthetic media"
    info = {b"name": b"Synthetic.mkv", b"length": len(data), b"private": 1,
            b"pieces": hashlib.sha1(data).digest(), b"piece length": 16384}
    torrent = bencode({b"info": info, b"announce": b"https://tracker.example.org/announce"})
    return {"source_id": "synthetic:movie:1", "title": "Synthetic Movie", "kind": "movie",
            "info_hash": hashlib.sha1(bencode(info)).hexdigest(), "size": len(data),
            "imdb_id": "tt1234567", "tmdb_id": 42, "metainfo_base64": base64.b64encode(torrent).decode(),
            "seed_verified": True}


def setup_release(client, ready=True):
    owner = identity("owner", "OWNER")
    assert client.put("/api/private/members/member", json={"active": True}, headers=owner).status_code == 200
    key = client.post("/api/account/api-key", headers=identity()).json()["apiKeySecret"]
    response = client.post("/api/private/catalog/import", json={"version": 1, "releases": [metainfo()]}, headers=owner)
    assert response.status_code == 200, response.text
    rows = client.get("/api/private/catalog", headers=owner).json()
    release_id = rows[0]["id"]
    assert rows[0]["ready"] == 0  # manifest cannot assert seeder readiness
    if ready:
        async def mark():
            db = await database.get_db()
            await db.execute("UPDATE private_releases SET ready=1,verified_at=? WHERE id=?", (time.time(), release_id))
            await db.commit()
        asyncio.run(mark())
    return key, release_id


def download(client, key, release_id):
    from torrent_metainfo import bdecode
    response = client.get(f"/private/torrents/{release_id}.torrent", params={"apikey": key})
    assert response.status_code == 200, response.text
    torrent = bdecode(response.content)
    assert torrent[b"info"][b"private"] == 1
    assert set(torrent) == {b"info", b"announce"}
    return urlsplit(torrent[b"announce"].decode()).path


def announce(client, path, **changes):
    params = {"info_hash": bytes.fromhex(metainfo()["info_hash"]), "peer_id": b"-TEST01-abcdefghijkl",
              "port": 6881, "uploaded": 0, "downloaded": 0, "left": metainfo()["size"], "event": "started", "compact": 1}
    params.update(changes)
    from torrent_metainfo import bdecode
    response = client.get(path + "?" + urlencode(params), headers={"x-bitagent-proxy-proof": PROOF, "x-bitagent-peer-ip": "192.0.2.1"})
    assert response.status_code == 200
    return bdecode(response.content)


def test_feature_off_closes_all_routes(client):
    config.settings.private_indexer_enabled = False
    assert client.get("/private/announce/bad").status_code == 404
    assert client.get("/private/torrents/anything.torrent").status_code == 404
    assert client.get("/api/library/private", headers=identity()).status_code == 404


def test_startup_requires_auth_strong_secret_and_https_origin():
    private.validate_settings()
    for key, value in (("require_auth", False), ("private_indexer_secret", "weak"),
                       ("private_indexer_url", "https://user:secret@library.example.org"),
                       ("private_indexer_url", "http://library.example.org"),
                       ("private_indexer_url", "https://library.example.org/path")):
        old = getattr(config.settings, key)
        setattr(config.settings, key, value)
        with pytest.raises(RuntimeError):
            private.validate_settings()
        setattr(config.settings, key, old)


def test_membership_restricts_site_account_mint_and_operator_management(client):
    assert client.get("/api/account/private", headers=identity()).json()["approved"] is False
    assert client.post("/api/account/api-key", headers=identity()).status_code == 403
    assert client.get("/library", headers={**identity(), "host": "library.example.org"}).status_code == 403
    assert client.get("/api/torrents", headers={**identity(), "host": "library.example.org"}).status_code == 403
    assert client.put("/api/private/members/member", json={"active": True}, headers=identity()).status_code == 403
    assert client.put("/api/private/members/member", json={"active": True}, headers={**identity("owner", "OWNER"), "host": "library.example.org"}).status_code == 404
    assert client.put("/api/private/members/api-client", json={"active": True}, headers=identity("owner", "OWNER")).status_code == 422


def test_private_catalog_unverified_hidden_and_search_filters(client):
    key, release_id = setup_release(client, ready=False)
    assert client.get("/api/library/private", headers=identity()).json()["total"] == 0
    assert client.get(f"/private/torrents/{release_id}.torrent", params={"apikey": key}).status_code == 404
    async def mark():
        db = await database.get_db()
        await db.execute("UPDATE private_releases SET ready=1,verified_at=?", (time.time(),))
        await db.commit()
    asyncio.run(mark())
    good = client.get("/api/library/private?t=movie&imdbid=1234567", headers=identity()).json()
    assert good["total"] == 1 and good["items"][0]["seeders"] == 0
    assert client.get("/api/library/private?t=tvsearch", headers=identity()).json()["total"] == 0
    assert client.get("/api/library/private?q=%", headers=identity()).json()["total"] == 0
    assert client.get("/api/library/private?cat=9999", headers=identity()).json()["total"] == 0
    assert client.get("/api/library/private?limit=101", headers=identity()).status_code == 422


def test_torrent_and_magnet_use_same_private_hash_and_revocable_credentials(client):
    from torrent_metainfo import bdecode, bencode
    key, release_id = setup_release(client)
    response = client.get(f"/private/torrents/{release_id}.torrent", params={"apikey": key})
    torrent = bdecode(response.content)
    assert hashlib.sha1(bencode(torrent[b"info"])).hexdigest() == metainfo()["info_hash"]
    assert response.headers["cache-control"] == "no-store"
    path = urlsplit(torrent[b"announce"].decode()).path
    magnet = client.get(f"/api/library/private/{release_id}/magnet", headers=identity()).json()
    assert "&tr=https%3A%2F%2Flibrary.example.org%2Fprivate%2Fannounce%2Fbt_" in magnet["magnetUri"]
    assert b"failure reason" not in announce(client, path)
    assert client.delete("/api/account/api-key", headers=identity()).status_code == 200
    assert b"failure reason" in announce(client, path)
    assert client.get(f"/private/torrents/{release_id}.torrent", params={"apikey": key}).status_code == 401


def test_suspend_removes_peers_and_invalidates_existing_download_and_announce(client):
    key, release_id = setup_release(client)
    path = download(client, key, release_id)
    assert b"failure reason" not in announce(client, path, left=0)
    assert client.put("/api/private/members/member", json={"active": False}, headers=identity("owner", "OWNER")).status_code == 200
    assert b"failure reason" in announce(client, path)
    assert client.get(f"/private/torrents/{release_id}.torrent", params={"apikey": key}).status_code == 401
    assert client.get("/torznab/api?t=caps", params={"apikey": key}).status_code == 401
    assert client.put("/api/private/members/member", json={"active": True}, headers=identity("owner", "OWNER")).status_code == 200
    assert b"failure reason" in announce(client, path)


def test_tracker_delta_accounting_baselines_and_self_only_metrics(client):
    key, release_id = setup_release(client)
    path = download(client, key, release_id)
    assert b"failure reason" not in announce(client, path, uploaded=1000, downloaded=1000)
    assert b"failure reason" not in announce(client, path, event="", uploaded=1010, downloaded=1005)
    assert b"failure reason" not in announce(client, path, event="completed", uploaded=1020, downloaded=1014, left=0)
    assert b"failure reason" not in announce(client, path, event="completed", uploaded=1020, downloaded=1014, left=0)
    metrics = client.get("/api/account/private/metrics", headers=identity()).json()["items"][0]
    assert (metrics["uploaded"], metrics["downloaded"], metrics["completed"], metrics["issued"]) == (20, 14, 1, 1)
    assert metrics["ratio"] == 20/14
    assert client.get("/api/private/metrics", headers=identity()).status_code == 403
    assert client.put("/api/private/members/other", json={"active": True}, headers=identity("owner", "OWNER")).status_code == 200
    assert client.get("/api/account/private/metrics", headers=identity("other")).json()["items"] == []
    assert client.get("/api/private/metrics", headers=identity("owner", "OWNER")).json()["items"][0]["user_id"] == "member"
    # A counter reset or new session does not credit the earlier lifetime count.
    announce(client, path, event="started", uploaded=0, downloaded=0)
    assert client.get("/api/account/private/metrics", headers=identity()).json()["items"][0]["uploaded"] == 20


def test_announce_binary_validation_and_no_peer_address_spoof(client):
    key, release_id = setup_release(client)
    path = download(client, key, release_id)
    for change in ({"peer_id": b"short"}, {"info_hash": b"short"}, {"port": 0},
                   {"uploaded": -1}, {"left": 100000}, {"event": "unknown"}, {"numwant": 201}):
        assert b"failure reason" in announce(client, path, **change)
    assert b"failure reason" not in announce(client, path, ip="198.51.100.99")
    async def peer():
        return dict((await (await database.get_db()).execute_fetchall("SELECT * FROM private_peers"))[0])
    assert asyncio.run(peer())["ip"] == "192.0.2.1"


def test_import_rejects_public_metainfo_and_preserves_readiness_on_same_hash(client):
    from torrent_metainfo import bdecode, bencode
    key, release_id = setup_release(client)
    item = metainfo()
    torrent = bdecode(base64.b64decode(item["metainfo_base64"]))
    torrent[b"announce-list"] = [[b"https://public.example.org/announce"]]
    item["metainfo_base64"] = base64.b64encode(bencode(torrent)).decode()
    assert client.post("/api/private/catalog/import", json={"releases": [item]}, headers=identity("owner", "OWNER")).status_code == 422
    item = metainfo()
    assert client.post("/api/private/catalog/import", json={"releases": [item]}, headers=identity("owner", "OWNER")).status_code == 200
    assert client.get(f"/private/torrents/{release_id}.torrent", params={"apikey": key}).status_code == 200


def test_seeder_verification_does_not_trust_manifest_or_network_failure(client, monkeypatch):
    key, release_id = setup_release(client, ready=False)
    assert client.post(f"/api/private/catalog/{release_id}/verify", headers=identity("owner", "OWNER")).status_code == 409
    config.settings.private_seeder_url = "http://seeder.example.org"
    class Result:
        status_code = 200
        text = "Ok."
        def raise_for_status(self):
            pass
        def json(self):
            return [{"hash": metainfo()["info_hash"], "size": metainfo()["size"], "progress": 1,
                     "amount_left": 0, "state": "stalledUP"}]
    class Client:
        def __init__(self, **kw):
            self.headers = {}
            assert kw["follow_redirects"] is False
        async def __aenter__(self):
            return self
        async def __aexit__(self, *args):
            pass
        async def post(self, *args, **kw):
            return Result()
        async def get(self, *args, **kw):
            return Result()
    monkeypatch.setattr(private.httpx, "AsyncClient", Client)
    assert client.post(f"/api/private/catalog/{release_id}/verify", headers=identity("owner", "OWNER")).json()["ready"] is True
    assert client.get(f"/private/torrents/{release_id}.torrent", params={"apikey": key}).status_code == 200


def test_same_swarm_aliases_and_immutable_versioned_sources(client):
    from torrent_metainfo import bdecode, bencode
    key, release_id = setup_release(client)
    alias = metainfo()
    alias.update(source_id="synthetic:show:1", title="Synthetic Show", kind="show", episodes=[1])
    assert client.post("/api/private/catalog/import", json={"releases": [alias]}, headers=identity("owner", "OWNER")).status_code == 200
    results = client.get("/api/library/private", headers=identity()).json()
    assert results["total"] == 2
    assert {row["id"] for row in results["items"]} == {release_id}
    assert client.get("/api/library/private?t=tvsearch&ep=1", headers=identity()).json()["total"] == 1
    changed = metainfo()
    torrent = bdecode(base64.b64decode(changed["metainfo_base64"]))
    torrent[b"info"][b"name"] = b"Changed.mkv"
    changed["metainfo_base64"] = base64.b64encode(bencode(torrent)).decode()
    changed["info_hash"] = hashlib.sha1(bencode(torrent[b"info"])).hexdigest()
    new = {**metainfo(), "source_id": "synthetic:new:alias"}
    # Whole batch rolls back; the earlier new alias must not survive a conflict.
    response = client.post("/api/private/catalog/import", json={"releases": [new, changed]}, headers=identity("owner", "OWNER"))
    assert response.status_code == 409
    assert client.get("/api/library/private", headers=identity()).json()["total"] == 2
    assert client.get(f"/private/torrents/{release_id}.torrent", params={"apikey": key}).status_code == 200


def test_expired_and_withdrawn_releases_fail_closed(client):
    key, release_id = setup_release(client)
    path = download(client, key, release_id)
    async def stale():
        db = await database.get_db()
        await db.execute("UPDATE private_releases SET verified_at=?", (time.time()-config.settings.private_seed_verification_ttl-1,))
        await db.commit()
    asyncio.run(stale())
    assert client.get("/api/library/private", headers=identity()).json()["total"] == 0
    assert client.get(f"/private/torrents/{release_id}.torrent", params={"apikey": key}).status_code == 404
    assert b"failure reason" in announce(client, path)
    assert client.patch(f"/api/private/catalog/{release_id}", json={"published": False}, headers=identity("owner", "OWNER")).status_code == 200
    assert client.post(f"/api/private/catalog/{release_id}/verify", headers=identity("owner", "OWNER")).status_code == 404
    assert client.get("/api/private/catalog", headers=identity("owner", "OWNER")).json()[0]["withdrawn"] == 1


@pytest.mark.parametrize("field,value", [("season", {}), ("tmdb_id", True), ("episodes", "12"), ("episodes", [1,1]), ("imdb_id", "not-imdb"), ("title", "bad\x00title")])
def test_import_validates_metadata_types(client, field, value):
    item = metainfo()
    item[field] = value
    assert client.post("/api/private/catalog/import", json={"releases": [item]}, headers=identity("owner", "OWNER")).status_code == 422


@pytest.mark.parametrize("info", [b"not a dictionary", 1, []])
def test_import_validates_metainfo_shape(client, info):
    from torrent_metainfo import bencode
    item = metainfo()
    item["metainfo_base64"] = base64.b64encode(bencode({b"info": info})).decode()
    assert client.post("/api/private/catalog/import", json={"releases": [item]}, headers=identity("owner", "OWNER")).status_code == 422


def test_import_bounds_body_before_json_parse_and_authenticates_first(client):
    huge = b" " * (8*1024*1024+1)
    assert client.post("/api/private/catalog/import", content=huge, headers={**identity("owner", "OWNER"), "content-type": "application/json"}).status_code == 413
    assert client.post("/api/private/catalog/import", content=huge, headers={**identity(), "content-type": "application/json"}).status_code == 403


def test_direct_download_and_search_rate_limits(client):
    key, release_id = setup_release(client)
    config.settings.torznab_rate_limit_per_min = 1
    torznab._TZ_BUCKETS.clear()
    assert client.get(f"/private/torrents/{release_id}.torrent", params={"apikey": key}).status_code == 200
    assert client.get(f"/private/torrents/{release_id}.torrent", params={"apikey": key}).status_code == 429
    assert client.get("/api/library/private", headers=identity()).status_code == 429


def test_private_pages_and_member_torrent_download(client):
    key, release_id = setup_release(client)
    assert client.get("/library/private", headers=identity()).status_code == 200
    assert client.get("/private-admin", headers=identity()).status_code == 403
    assert client.get("/private-admin", headers=identity("owner", "OWNER")).status_code == 200
    assert client.get(f"/api/library/private/{release_id}/torrent", headers=identity()).status_code == 200


def test_failed_background_seeder_probe_withdraws_readiness(client):
    key, release_id = setup_release(client)
    # No seed client configured; the known failure invalidates the old proof.
    asyncio.run(private.refresh_readiness())
    assert client.get("/api/library/private", headers=identity()).json()["total"] == 0
    assert client.get(f"/private/torrents/{release_id}.torrent", params={"apikey": key}).status_code == 404


def test_member_network_policy_rejects_outside_peers(client):
    key, release_id = setup_release(client)
    path = download(client, key, release_id)
    config.settings.private_peer_cidrs = "198.51.100.0/24"
    assert b"failure reason" in announce(client, path)
    config.settings.private_peer_cidrs = "192.0.2.0/24"
    assert b"failure reason" not in announce(client, path)


def test_operator_can_bootstrap_seed_before_advertising_to_members(client):
    from torrent_metainfo import bdecode
    key, release_id = setup_release(client, ready=False)
    url = f"/api/private/catalog/{release_id}/seed-torrent?seed_member_id=member"
    assert client.get(url, headers=identity()).status_code == 403
    response = client.get(url, headers=identity("owner", "OWNER"))
    assert response.status_code == 200
    assert b"/private/announce/bt_" in bdecode(response.content)[b"announce"]
    assert client.get(f"/private/torrents/{release_id}.torrent", params={"apikey": key}).status_code == 404
    assert client.get("/api/library/private", headers=identity()).json()["total"] == 0
    assert client.get("/api/account/private/metrics", headers=identity()).json()["items"] == []


def test_signing_secret_rotation_retires_old_tokens_and_issues_working_new_tokens(client):
    key, release_id = setup_release(client)
    old = download(client, key, release_id)
    assert b"failure reason" not in announce(client, old)
    config.settings.private_indexer_secret = "new-signing-secret-material-for-synthetic-test-123"
    assert b"failure reason" in announce(client, old)
    new = download(client, key, release_id)
    assert old != new
    assert b"failure reason" not in announce(client, new)
    assert b"failure reason" in announce(client, old)

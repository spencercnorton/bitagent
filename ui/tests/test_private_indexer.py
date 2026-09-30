"""Real SQLite/ASGI authorization, torrent issuance and tracker accounting."""
import asyncio
import base64
import hashlib
import time
from urllib.parse import urlencode, urlsplit

import pytest
from fastapi.testclient import TestClient

import app as app_module
import config
import database
import private_indexer as private
import torznab
from conftest import _with_transport_peer


PROOF = "p" * 40


def identity(user="member", role="VIEWER"):
    return {"x-auth-user-id": user, "x-auth-priv": role, "x-bitagent-proxy-proof": PROOF}


@pytest.fixture(autouse=True)
def private_config():
    config.settings.private_indexer_enabled = True
    config.settings.private_indexer_secret = "s" * 40
    config.settings.private_indexer_url = "https://library.example.org"
    config.settings.private_seeder_url = "http://seeder.example.org"
    config.settings.require_auth = True
    config.settings.trust_npm_headers = True
    config.settings.proxy_auth_secret = PROOF
    config.settings.trusted_proxy_cidrs = "127.0.0.0/8"
    torznab._TZ_BUCKETS.clear()
    async def clear():
        db = await database.get_db()
        for table in ("private_members", "private_releases", "private_release_aliases", "private_peers", "private_tracker_keys", "private_transfer_totals", "user_api_keys", "account_usage"):
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
                       ("private_seeder_url", ""),
                       ("private_indexer_url", "https://library.example.org/path")):
        old = getattr(config.settings, key)
        setattr(config.settings, key, value)
        with pytest.raises(RuntimeError):
            private.validate_settings()
        setattr(config.settings, key, old)


def test_startup_missing_seeder_url_refuses_enabled_application():
    config.settings.private_seeder_url = ""
    with pytest.raises(RuntimeError, match="PRIVATE_SEEDER_URL is required"):
        with TestClient(_with_transport_peer(app_module.app, "127.0.0.1")):
            pytest.fail("An enabled private application must not start without its seeder")


def test_startup_withdraws_cached_readiness_before_initial_probe(monkeypatch):
    from torrent_metainfo import bdecode, bencode

    async def pending_probe():
        await asyncio.Event().wait()

    # Both lifespans are hermetic, including the first cache preparation.
    monkeypatch.setattr(private, "refresh_readiness", pending_probe)
    with TestClient(_with_transport_peer(app_module.app, "127.0.0.1")) as first:
        key, release_id = setup_release(first)
        download(first, key, release_id)
        withdrawn = metainfo()
        info = bdecode(base64.b64decode(withdrawn["metainfo_base64"]))[b"info"]
        info[b"name"] = b"Withdrawn.mkv"
        withdrawn.update(source_id="synthetic:withdrawn:1", title="Withdrawn Synthetic Movie",
                         info_hash=hashlib.sha1(bencode(info)).hexdigest(),
                         metainfo_base64=base64.b64encode(bencode({b"info": info})).decode())
        assert first.post("/api/private/catalog/import", json={"releases": [withdrawn]}, headers=identity("owner", "OWNER")).status_code == 200

        async def mark_withdrawn():
            async with private.private_write() as db:
                await db.execute("UPDATE private_releases SET ready=1,verified_at=?,withdrawn=1 WHERE source_id=?",
                                 (time.time(), withdrawn["source_id"]))
        asyncio.run(mark_withdrawn())
        before = {row["id"]: row for row in first.get("/api/private/catalog", headers=identity("owner", "OWNER")).json()}
        assert all(row["ready"] == 1 for row in before.values())

    with TestClient(_with_transport_peer(app_module.app, "127.0.0.1")) as restarted:
        assert restarted.get("/api/library/private", headers=identity()).json()["total"] == 0
        assert restarted.get(f"/private/torrents/{release_id}.torrent", params={"apikey": key}).status_code == 404
        after = {row["id"]: row for row in restarted.get("/api/private/catalog", headers=identity("owner", "OWNER")).json()}
        assert after.keys() == before.keys()
        for release_id, row in after.items():
            assert row == {**before[release_id], "ready": 0, "verified_at": None}
        history = restarted.get("/api/account/private/metrics", headers=identity()).json()["items"]
        assert len(history) == 1 and history[0]["issued"] == 1


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


def test_suspended_public_member_can_revoke_only_their_key(client):
    key, release_id = setup_release(client)
    owner = identity("owner", "OWNER")
    assert client.put("/api/private/members/other", json={"active": True}, headers=owner).status_code == 200
    other_key = client.post("/api/account/api-key", headers=identity("other")).json()["apiKeySecret"]
    assert client.put("/api/private/members/member", json={"active": False}, headers=owner).status_code == 200
    public = {**identity(), "host": "library.example.org"}
    assert client.delete("/api/account/api-key", headers={"host": "library.example.org"}).status_code == 401
    assert client.delete("/api/account/api-key", headers={**public, "sec-fetch-site": "cross-site"}).status_code == 403
    assert client.post("/api/account/api-key", headers=public).status_code == 403
    response = client.delete("/api/account/api-key", headers=public)
    assert response.status_code == 200
    assert response.json()["apiKey"] is None
    assert client.post("/api/account/api-key", headers=public).status_code == 403
    assert client.get("/torznab/private/api", params={"t": "caps", "apikey": key}).status_code == 401
    assert client.get("/torznab/private/api", params={"t": "caps", "apikey": other_key}).status_code == 200


def test_private_searches_update_account_usage_only_for_successful_gets(client):
    key, release_id = setup_release(client)

    def count():
        return client.get("/api/account", headers=identity()).json()["usage"]["apiSearches"]

    assert count() == 0
    params = {"apikey": key}
    assert client.get("/torznab/private/api", params={**params, "t": "caps"}).status_code == 200
    assert client.get("/torznab/private/api", params={**params, "t": "get", "id": release_id}).status_code == 200
    assert client.head("/torznab/private/api", params={**params, "t": "search"}).status_code == 200
    assert client.get("/torznab/private/api", params={**params, "t": "search", "cat": "invalid"}).status_code == 400
    assert client.get("/torznab/private/api", params={**params, "t": "music"}).status_code == 400
    assert client.get("/torznab/private/api", params={"t": "search", "apikey": "invalid"}).status_code == 401
    assert count() == 0
    for function in ("search", "movie", "tvsearch"):
        assert client.get("/torznab/private/api", params={**params, "t": function}).status_code == 200
    assert count() == 3  # Empty successful TV feeds are search requests too.
    assert client.put("/api/private/members/member", json={"active": False}, headers=identity("owner", "OWNER")).status_code == 200
    assert client.get("/torznab/private/api", params={**params, "t": "search"}).status_code == 401
    assert count() == 3


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
    config.settings.private_seeder_url = ""
    assert client.post(f"/api/private/catalog/{release_id}/verify", headers=identity("owner", "OWNER")).status_code == 409
    config.settings.private_seeder_url = "http://seeder.example.org"
    import httpx
    async_client = httpx.AsyncClient
    def seeder(request):
        if request.url.path.endswith("auth/login"):
            return httpx.Response(200, content=b"Ok.", headers={"set-cookie": "SID=" + "a" * 32 + "; Path=/; HttpOnly"})
        assert request.headers["cookie"] == "SID=" + "a" * 32
        return httpx.Response(200, json=[{"hash": metainfo()["info_hash"], "size": metainfo()["size"], "progress": 1,
                                       "amount_left": 0, "state": "stalledUP"}])
    def seeder_client(**kwargs):
        assert kwargs["follow_redirects"] is False
        return async_client(transport=httpx.MockTransport(seeder), **kwargs)
    monkeypatch.setattr(private.httpx, "AsyncClient", seeder_client)
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
    config.settings.private_seeder_url = ""
    asyncio.run(private.refresh_readiness())
    assert client.get("/api/library/private", headers=identity()).json()["total"] == 0
    assert client.get(f"/private/torrents/{release_id}.torrent", params={"apikey": key}).status_code == 404


def test_enabled_worker_withdraws_cache_when_seeder_url_disappears(client, monkeypatch):
    key, release_id = setup_release(client)
    refresh = private.refresh_readiness

    async def restart_worker():
        await private.stop_readiness_worker()
        config.settings.private_seeder_url = ""
        finished = asyncio.Event()

        async def observed_refresh():
            await refresh()
            finished.set()

        monkeypatch.setattr(private, "refresh_readiness", observed_refresh)
        private.start_readiness_worker()
        try:
            assert private._READINESS_TASK is not None
            await asyncio.wait_for(finished.wait(), timeout=2)
        finally:
            await private.stop_readiness_worker()

    client.portal.call(restart_worker)
    assert client.get("/api/library/private", headers=identity()).json()["total"] == 0
    assert client.get(f"/private/torrents/{release_id}.torrent", params={"apikey": key}).status_code == 404


def test_unexpected_probe_failure_withdraws_cached_readiness(client, monkeypatch, caplog):
    key, release_id = setup_release(client)

    def failed_client(**kwargs):
        raise RuntimeError("https://synthetic-seeder.example.org/?secret=synthetic-secret")

    monkeypatch.setattr(private.httpx, "AsyncClient", failed_client)
    asyncio.run(private.refresh_readiness())
    assert client.get("/api/library/private", headers=identity()).json()["total"] == 0
    assert client.get(f"/private/torrents/{release_id}.torrent", params={"apikey": key}).status_code == 404
    assert "synthetic-secret" not in caplog.text


def test_probe_cancellation_propagates(client, monkeypatch):
    key, release_id = setup_release(client)

    def cancelled_client(**kwargs):
        raise asyncio.CancelledError

    monkeypatch.setattr(private.httpx, "AsyncClient", cancelled_client)
    with pytest.raises(asyncio.CancelledError):
        asyncio.run(private.refresh_readiness())


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


@pytest.mark.parametrize("probe", ["background", "operator"])
@pytest.mark.parametrize("status,body,cookie,accepted", [
    (200, b"Ok.", "SID=" + "a" * 32 + "; Path=/; HttpOnly", True),
    (204, b"", "QBT_SID_8080=" + "ab+/" * 8 + "; Path=/; HttpOnly", True),
    (204, b"", "SID=" + "b" * 32 + "; Path=/; HttpOnly", True),
    (200, b"Ok.", "QBT_SID_8080=" + "ab+/" * 8 + "; Path=/; HttpOnly", True),
    (201, b"Ok.", "SID=" + "a" * 32 + "; Path=/", False),
    (202, b"", "SID=" + "a" * 32 + "; Path=/", False),
    (204, b"Ok.", "SID=" + "a" * 32 + "; Path=/", False),
    (204, b" ", "SID=" + "a" * 32 + "; Path=/", False),
    (200, b"", "SID=" + "a" * 32 + "; Path=/", False),
    (200, b"Ok.\n", "SID=" + "a" * 32 + "; Path=/", False),
    (200, b"Fails.", "SID=" + "a" * 32 + "; Path=/", False),
    (403, b"Ok.", "SID=" + "a" * 32 + "; Path=/", False),
    (302, b"", "SID=" + "a" * 32 + "; Path=/", False),
    (204, b"", None, False),
    (200, b"Ok.", None, False),
    (204, b"", "SID=; Path=/", False),
    (204, b"", "SID=short; Path=/", False),
    (204, b"", "sid=" + "a" * 32 + "; Path=/", False),
    (204, b"", "unrelated=" + "a" * 32 + "; Path=/", False),
    (204, b"", "QBT_SID_65536=" + "a" * 32 + "; Path=/", False),
    (204, b"", "QBT_SID_0=" + "a" * 32 + "; Path=/", False),
    (204, b"", "QBT_SID_bad=" + "a" * 32 + "; Path=/", False),
    (204, b"", "SID=" + "a" * 32 + "; Domain=elsewhere.example.org; Path=/", False),
    (204, b"", "SID=" + "a" * 32 + "; Path=/api/v2/auth", False),
    (204, b"", "SID=" + "a" * 32 + "; Path=/; Secure", False),
    (204, b"", "SID=" + "a" * 32 + "; Path=/; Max-Age=0", False),
    (204, b"", [("set-cookie", "SID=" + "a" * 32 + "; Path=/"),
                 ("set-cookie", "SID=" + "b" * 32 + "; Path=/")], False),
    (204, b"", [("set-cookie", "SID=" + "a" * 32 + "; Path=/"),
                 ("set-cookie", "QBT_SID_8080=" + "b" * 32 + "; Path=/")], False),
])
def test_seeder_login_cookie_contract_for_both_readiness_paths(client, monkeypatch, probe, status, body, cookie, accepted):
    import httpx
    key, release_id = setup_release(client)
    async_client = httpx.AsyncClient
    get_calls = []
    def seeder(request):
        assert request.headers["referer"] == config.settings.private_seeder_url + "/"
        if request.url.path.endswith("auth/login"):
            return httpx.Response(status, content=body, headers=cookie if isinstance(cookie, list) else {"set-cookie": cookie} if cookie else {})
        get_calls.append(request)
        assert request.url.params["hashes"] == metainfo()["info_hash"]
        assert cookie.split(";", 1)[0] in request.headers["cookie"]
        return httpx.Response(200, json=[{"hash": metainfo()["info_hash"], "size": metainfo()["size"], "progress": 1,
                                       "amount_left": 0, "state": "stalledUP"}])
    monkeypatch.setattr(private.httpx, "AsyncClient", lambda **kwargs: async_client(transport=httpx.MockTransport(seeder), **kwargs))
    if probe == "background":
        asyncio.run(private.refresh_readiness())
    else:
        response = client.post(f"/api/private/catalog/{release_id}/verify", headers=identity("owner", "OWNER"))
        assert response.status_code == (200 if accepted else 409)
    assert len(get_calls) == int(accepted)
    assert client.get("/api/library/private", headers=identity()).json()["total"] == int(accepted)
    assert client.get(f"/private/torrents/{release_id}.torrent", params={"apikey": key}).status_code == (200 if accepted else 404)


@pytest.mark.parametrize("probe", ["background", "operator"])
@pytest.mark.parametrize("failure", ["forbidden", "redirect", "server-error", "malformed-json", "incomplete", "wrong-hash", "wrong-size", "paused"])
def test_successful_seeder_login_still_requires_verified_information(client, monkeypatch, probe, failure):
    import httpx
    key, release_id = setup_release(client)
    async_client = httpx.AsyncClient
    def seeder(request):
        if request.url.path.endswith("auth/login"):
            return httpx.Response(204, headers={"set-cookie": "QBT_SID_8080=" + "c" * 32 + "; Path=/; HttpOnly"})
        assert request.headers["cookie"] == "QBT_SID_8080=" + "c" * 32
        if failure in {"forbidden", "redirect", "server-error"}:
            return httpx.Response({"forbidden": 403, "redirect": 302, "server-error": 500}[failure])
        if failure == "malformed-json":
            return httpx.Response(200, content=b"not-json")
        torrent = {"hash": metainfo()["info_hash"], "size": metainfo()["size"], "progress": 1, "amount_left": 0, "state": "stalledUP"}
        torrent.update({"incomplete": {"progress": .5, "amount_left": 1}, "wrong-hash": {"hash": "f" * 40},
                        "wrong-size": {"size": 1}, "paused": {"state": "pausedUP"}}[failure])
        return httpx.Response(200, json=[torrent])
    monkeypatch.setattr(private.httpx, "AsyncClient", lambda **kwargs: async_client(transport=httpx.MockTransport(seeder), **kwargs))
    if probe == "background":
        asyncio.run(private.refresh_readiness())
    else:
        assert client.post(f"/api/private/catalog/{release_id}/verify", headers=identity("owner", "OWNER")).status_code == 409
    assert client.get("/api/library/private", headers=identity()).json()["total"] == 0
    assert client.get(f"/private/torrents/{release_id}.torrent", params={"apikey": key}).status_code == 404

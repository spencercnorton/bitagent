"""HTTP route surface — the `/api/*` endpoints in app.py.

These were previously untested: the existing suite covered version/asset/auth/
infisical/reshape but none of the FastAPI routes end-to-end. This file exercises
every route through the TestClient with the backend (GraphQL, /metrics, TMDB,
*arr) faked, so the suite stays hermetic and runs offline in CI.

Auth is disabled per-test (require_auth = False) so we test the route bodies, not
the auth gate (that lives in test_auth.py). The DB-backed routes (wants, settings
overrides, block-phrases, notifications) hit the real throwaway temp SQLite from
conftest, so their CRUD round-trips are real, not mocked.
"""
from __future__ import annotations

import asyncio

import httpx
import pytest

import app as app_module
import config
import graphql_client as gql
import tmdb


@pytest.fixture(autouse=True)
def _open_auth():
    """Most route tests want auth out of the way; auth itself is covered in
    test_auth.py. Restored by conftest's _restore_settings around each test."""
    config.settings.require_auth = False


@pytest.fixture()
def fresh_db():
    """Truncate the DB-backed tables so row-count / ordering assertions are
    deterministic regardless of test order. The connection is the process-wide
    singleton pointed at conftest's temp file."""
    async def _clear():
        db = await app_module.get_db()
        for tbl in ("wants", "settings_overrides", "audit_log",
                    "notifications", "block_phrases", "user_api_keys"):
            await db.execute(f"DELETE FROM {tbl}")
        await db.commit()
    asyncio.run(_clear())
    yield


# ── Helpers to fake the GraphQL / metrics client ──────────────────────────

def _patch_gql_query(monkeypatch, responder):
    async def fake_query(q, variables=None, **kwargs):
        return responder(q, variables)
    monkeypatch.setattr(gql, "query", fake_query)


def _patch_metrics(monkeypatch, text: str):
    async def fake_metrics():
        return text
    monkeypatch.setattr(gql, "fetch_metrics", fake_metrics)


# ── Health & identity ─────────────────────────────────────────────────────

def test_healthz_is_open_and_ok(client):
    config.settings.require_auth = True  # /healthz must work even with auth on
    r = client.get("/healthz")
    assert r.status_code == 200
    body = r.json()
    assert body["status"] == "ok"
    assert isinstance(body["ts"], (int, float))


def test_api_me_returns_open_identity(client):
    r = client.get("/api/me")
    assert r.status_code == 200
    body = r.json()
    assert body["id"] == "anonymous"
    assert body["method"] == "open"
    # Identity shape is stable — templates read these unconditionally.
    for k in ("display", "username", "email", "provider", "priv", "operator"):
        assert k in body


def test_api_me_401_when_auth_required_and_no_creds(client):
    config.settings.require_auth = True
    r = client.get("/api/me")
    assert r.status_code == 401


def test_dashboard_requires_auth_when_enabled(client):
    config.settings.require_auth = True
    r = client.get("/")
    assert r.status_code == 401


# ── /api/auth/tiers ───────────────────────────────────────────────────────

def test_auth_tiers_reports_booleans_only(client, fresh_db):
    # require_auth stays False (route reachable; the gate itself is tested in
    # test_auth.py). require_auth=False means SSO is reported inactive even with
    # a proxy tier trusted — that's the documented contract.
    config.settings.trust_npm_headers = True
    config.settings.trust_forwarded_user = False
    config.settings.trusted_proxy_cidrs = "127.0.0.0/8"
    config.settings.proxy_auth_secret = "test-proof-at-least-thirty-two-bytes"
    r = client.get("/api/auth/tiers")
    assert r.status_code == 200
    body = r.json()
    assert body["npmHeaders"] is True
    assert body["forwardedUser"] is False
    assert body["proxyProof"] is True
    assert body["sso"] is False  # require_auth is off
    # No secret values leak.
    assert "dashboard_api_key" not in str(body)


def test_auth_tiers_sso_active_when_enforced_and_proxy_trusted(client, fresh_db):
    # describe_active_tiers reads settings.require_auth directly, so SSO flips on
    # when auth is enforced AND a proxy tier is trusted — even though the route
    # itself is reached with auth disabled in this test harness... it isn't:
    # toggle require_auth on only for the duration of the body computation by
    # supplying the configured api key so the request authenticates.
    config.settings.require_auth = True
    config.settings.dashboard_api_key = "secret-key"
    config.settings.trust_npm_headers = True
    config.settings.trusted_proxy_cidrs = "127.0.0.0/8"
    config.settings.proxy_auth_secret = "test-proof-at-least-thirty-two-bytes"
    r = client.get("/api/auth/tiers", headers={"x-api-key": "secret-key"})
    assert r.status_code == 200
    assert r.json()["sso"] is True


def test_auth_trust_flags_are_not_runtime_mutable(client, fresh_db):
    config.settings.trust_npm_headers = False
    denied = client.put(
        "/api/settings/overrides/trust_npm_headers", json={"value": "true"}
    )
    assert denied.status_code == 403
    body = client.get("/api/auth/tiers").json()
    assert body["npmHeaders"] is False


# ── /api/stats ────────────────────────────────────────────────────────────

_METRICS_SAMPLE = """
# HELP whatever
bitagent_dht_client_request_concurrency 7
bitagent_dht_client_request_success_total 42
bitagent_dht_client_request_error_total{reason="timeout"} 3
bitagent_contentfilter_examined_total 1000
bitagent_classifier_llm_cache_hits_total 80
bitagent_classifier_llm_cache_misses_total 20
bitagent_liveness_observations_total{class="alive"} 90
bitagent_liveness_observations_total{class="suspect"} 10
bitagent_liveness_blacklist_size 5
bitagent_liveness_torznab_excluded_total 3
bitagent_liveness_revalidations_total{outcome="alive_again"} 6
bitagent_liveness_revalidations_total{outcome="still_dead"} 4
bitagent_dashstats_grab_success 68
bitagent_dashstats_grab_failure 32
bitagent_dashstats_grab_pending 2
bitagent_dashstats_match_video_total_30d 100
bitagent_dashstats_match_video_matched_30d 57
"""


def test_api_stats_aggregates_gql_and_metrics(client, monkeypatch):
    def responder(q, variables=None):
        if "version" in q:  # SYSTEM_STATS
            return {"data": {
                "version": "kleos-v1.23.0",
                "torrentContent": {"search": {
                    "totalCount": 1234,
                    "aggregations": {"contentType": [
                        {"value": "movie", "count": 800},
                        {"value": "tv_show", "count": 434},
                    ]},
                }},
            }}
        if "EvidenceList" in q or "evidence" in q:
            return {"data": {"evidence": {"list": {"totalCount": 9, "items": []}}}}
        return {"data": {}}
    _patch_gql_query(monkeypatch, responder)
    _patch_metrics(monkeypatch, _METRICS_SAMPLE)

    r = client.get("/api/stats")
    assert r.status_code == 200
    body = r.json()
    assert body["totalTorrents"] == 1234
    assert body["totalReleases"] == 1234
    assert body["totalEvidence"] == 9
    # This gauge is active request concurrency, not a routing-table peer count.
    # The misleading legacy field is intentionally null; the correctly named
    # compatibility field and envelope carry the measured value.
    assert body["dhtPeerCount"] is None
    assert body["dhtRequestConcurrency"] == 7
    assert body["version"] == "kleos-v1.23.0"
    # category breakdown reshaped value/count -> category/count
    cats = {c["category"]: c["count"] for c in body["categoryBreakdown"]}
    assert cats == {"movie": 800, "tv_show": 434}
    # cache hit ratio 80/(80+20)
    assert body["cacheHitRatio"] == pytest.approx(0.8)
    # grab success 68/(68+32)
    assert body["grabSuccess"]["rate"] == pytest.approx(0.68)
    assert body["grabSuccess"]["pending"] == 2
    # match rate 57/100
    assert body["matchRate"]["rate"] == pytest.approx(0.57)
    # liveness raw observation counts (the Liveness sub-tab surfaces these; the
    # old dashboard "Success Rate" ratio card was removed in v1.13.0)
    assert body["liveness"]["observationsAlive"] == 90
    assert body["liveness"]["observationsSuspect"] == 10
    assert "successRate" not in body["liveness"]
    assert body["liveness"]["blacklistSize"] == 5
    assert body["liveness"]["revalidations"]["recoveryRate"] == pytest.approx(0.6)
    assert body["uptimeSeconds"] >= 1
    # core reachability flags drive the dashboard health pills; db is the
    # sidecar SQLite (System tab pill) — live in tests via the temp DB
    assert body["core"] == {"graphql": True, "metrics": True, "db": True}

    observed_at = body["snapshot"]["observed_at"]
    assert body["snapshot"]["max_parallel_probes"] == 4
    required_envelope_fields = {
        "value", "status", "source", "observed_at", "stale", "error",
    }
    assert body["metrics"]
    for metric in body["metrics"].values():
        assert set(metric) == required_envelope_fields
        assert metric["observed_at"] == observed_at
        assert metric["stale"] is False
    dht = body["metrics"]["dhtRequestConcurrency"]
    assert dht == {
        "value": 7,
        "status": "ok",
        "source": "bitagent.prometheus:bitagent_dht_client_request_concurrency",
        "observed_at": observed_at,
        "stale": False,
        "error": None,
    }
    assert body["metrics"]["dhtPeerCount"]["value"] is None
    assert body["metrics"]["dhtPeerCount"]["status"] == "unavailable"
    assert body["metrics"]["totalEvidence"]["source"] == (
        "bitagent.graphql:evidence.list.totalCount"
    )
    successful_rate = body["metrics"]["successfulDhtRequestsPerMin"]
    assert successful_rate["value"] is None  # first sample has no rate baseline
    assert successful_rate["source"] == (
        "bitagent.prometheus:bitagent_dht_client_request_success_total"
    )
    assert "duration_seconds_count" not in successful_rate["source"]


def test_api_stats_backend_down_returns_null_unknowns_not_fabricated_zeroes(
    client, monkeypatch
):
    # GraphQL returns the unreachable envelope; metrics returns "". The local
    # process uptime and DB probe remain known, but upstream facts are null.
    async def dead_query(q, variables=None, **kwargs):
        return {"data": None, "errors": [{"message": "unreachable"}]}
    monkeypatch.setattr(gql, "query", dead_query)
    _patch_metrics(monkeypatch, "")
    r = client.get("/api/stats")
    assert r.status_code == 200
    body = r.json()
    assert body["totalTorrents"] is None
    assert body["totalEvidence"] is None
    assert body["dhtPeerCount"] is None
    assert body["dhtRequestConcurrency"] is None
    assert body["crawlRatePerMin"] is None
    assert body["cacheHitRatio"] is None
    assert body["categoryBreakdown"] is None
    assert body["grabSuccess"] == {
        "rate": None, "success": None, "failure": None, "pending": None,
    }
    assert body["liveness"]["blacklistSize"] is None
    assert body["uptimeSeconds"] >= 1
    for name in (
        "totalTorrents", "totalEvidence", "dhtRequestConcurrency",
        "cacheHitRatio", "livenessBlacklistSize",
    ):
        assert body["metrics"][name]["value"] is None
        assert body["metrics"][name]["status"] in {"unavailable", "error"}
    # health pills must show the truth: both core endpoints down (the
    # sidecar SQLite is still up — it's local, not part of the core)
    assert body["core"] == {"graphql": False, "metrics": False, "db": True}


def test_api_stats_preserves_source_emitted_zeroes(client, monkeypatch):
    def responder(q, variables=None):
        if "version" in q:
            return {"data": {
                "version": "kleos-v0",
                "torrentContent": {"search": {
                    "totalCount": 0,
                    "aggregations": {"contentType": []},
                }},
            }}
        if "evidence" in q:
            return {"data": {"evidence": {"list": {
                "totalCount": 0, "items": [],
            }}}}
        return {"data": {}}

    _patch_gql_query(monkeypatch, responder)
    _patch_metrics(
        monkeypatch,
        "\n".join((
            "bitagent_dht_client_request_concurrency 0",
            "bitagent_liveness_blacklist_size 0",
            "bitagent_classifier_llm_cache_hits_total 0",
            "bitagent_classifier_llm_cache_misses_total 0",
        )),
    )

    body = client.get("/api/stats").json()
    assert body["totalTorrents"] == 0
    assert body["totalEvidence"] == 0
    assert body["dhtRequestConcurrency"] == 0
    assert body["liveness"]["blacklistSize"] == 0
    assert body["categoryBreakdown"] == []
    for name in (
        "totalTorrents", "totalEvidence", "dhtRequestConcurrency",
        "livenessBlacklistSize", "categoryBreakdown",
    ):
        assert body["metrics"][name]["status"] == "ok"
        assert body["metrics"][name]["error"] is None
    # A 0/0 ratio and first-sample rate are unknown, not measured zeroes.
    assert body["cacheHitRatio"] is None
    assert body["metrics"]["cacheHitRatio"]["status"] == "unavailable"
    assert body["crawlRatePerMin"] is None


def test_api_stats_successful_dht_rate_excludes_errors_and_preserves_decimal(
    client, monkeypatch
):
    def responder(q, variables=None):
        if "version" in q:
            return {"data": {
                "version": "kleos-v1",
                "torrentContent": {"search": {
                    "totalCount": 1,
                    "aggregations": {"contentType": []},
                }},
            }}
        return {"data": {"evidence": {"list": {
            "totalCount": 0, "items": [],
        }}}}

    scrapes = iter((
        "\n".join((
            "bitagent_dht_client_request_success_total 0",
            'bitagent_dht_client_request_error_total{reason="timeout"} 0',
        )),
        "\n".join((
            "bitagent_dht_client_request_success_total 1",
            'bitagent_dht_client_request_error_total{reason="timeout"} 1000',
        )),
        "unrelated_metric 1",
    ))

    async def fake_metrics():
        return next(scrapes)

    clocks = {"history": 0.0, "cache": 0.0}
    _patch_gql_query(monkeypatch, responder)
    monkeypatch.setattr(gql, "fetch_metrics", fake_metrics)
    monkeypatch.setattr(
        app_module, "_metric_history_now", lambda: clocks["history"]
    )
    monkeypatch.setattr(app_module, "_stats_cache_now", lambda: clocks["cache"])

    first = client.get("/api/stats").json()
    assert first["successfulDhtRequestsPerMin"] is None

    clocks.update(history=40.0, cache=6.0)
    second = client.get("/api/stats").json()
    assert second["successfulDhtRequestsPerMin"] == pytest.approx(1.5)
    assert second["crawlRatePerMin"] == pytest.approx(1.5)  # legacy alias
    assert second["metrics"]["successfulDhtRequestsPerMin"]["value"] == pytest.approx(1.5)

    # The next successful scrape drops the family. The previous two samples
    # must not keep the old rate alive, and the null result itself is cached.
    clocks.update(history=50.0, cache=12.0)
    missing = client.get("/api/stats").json()
    assert missing["successfulDhtRequestsPerMin"] is None
    assert missing["metrics"]["successfulDhtRequestsPerMin"]["status"] == "unavailable"
    assert missing["metrics"]["successfulDhtRequestsPerMin"]["error"] == (
        "Successful DHT request counter not emitted"
    )
    assert missing["snapshot"]["cached"] is False

    clocks["cache"] = 13.0
    cached_missing = client.get("/api/stats").json()
    assert cached_missing["successfulDhtRequestsPerMin"] is None
    assert cached_missing["snapshot"]["cached"] is True


def test_api_stats_one_sided_labeled_series_remain_unknown(client, monkeypatch):
    def responder(q, variables=None):
        if "version" in q:
            return {"data": {
                "version": "kleos-v1",
                "torrentContent": {"search": {
                    "totalCount": 1,
                    "aggregations": {"contentType": []},
                }},
            }}
        return {"data": {"evidence": {"list": {
            "totalCount": 0, "items": [],
        }}}}

    _patch_gql_query(monkeypatch, responder)
    _patch_metrics(monkeypatch, "\n".join((
        'bitagent_liveness_observations_total{class="alive"} 5',
        'bitagent_liveness_revalidations_total{outcome="alive_again"} 2',
    )))

    body = client.get("/api/stats").json()
    liveness = body["liveness"]
    assert liveness["observationsAlive"] == 5
    assert liveness["observationsSuspect"] is None
    assert liveness["revalidations"] == {
        "aliveAgain": 2,
        "stillDead": None,
        "recoveryRate": None,
    }
    assert body["metrics"]["livenessObservationsAlive"]["status"] == "ok"
    assert body["metrics"]["livenessObservationsSuspect"]["status"] == "unavailable"
    assert body["metrics"]["livenessRevalidationsDead"]["status"] == "unavailable"


def test_api_stats_missing_metric_family_is_null_while_scrape_is_reachable(
    client, monkeypatch
):
    def responder(q, variables=None):
        if "version" in q:
            return {"data": {
                "version": "kleos-v1",
                "torrentContent": {"search": {
                    "totalCount": 1,
                    "aggregations": {"contentType": []},
                }},
            }}
        return {"data": {"evidence": {"list": {"totalCount": 0, "items": []}}}}

    _patch_gql_query(monkeypatch, responder)
    _patch_metrics(monkeypatch, "unrelated_metric 1\n")
    body = client.get("/api/stats").json()

    assert body["core"]["metrics"] is True
    assert body["metrics"]["metricsReachable"]["status"] == "ok"
    assert body["dhtRequestConcurrency"] is None
    assert body["metrics"]["dhtRequestConcurrency"]["status"] == "unavailable"
    assert body["metrics"]["dhtRequestConcurrency"]["error"] == (
        "DHT request concurrency metric not emitted"
    )


def test_api_stats_evidence_outage_is_not_reported_as_empty_corpus(
    client, monkeypatch
):
    def responder(q, variables=None):
        if "version" in q:
            return {"data": {
                "version": "kleos-v1",
                "torrentContent": {"search": {
                    "totalCount": 12,
                    "aggregations": {"contentType": []},
                }},
            }}
        return {"data": None, "errors": [{"message": "evidence unavailable"}]}

    _patch_gql_query(monkeypatch, responder)
    _patch_metrics(monkeypatch, "bitagent_dht_client_request_concurrency 1\n")
    body = client.get("/api/stats").json()

    assert body["core"]["graphql"] is True
    assert body["totalTorrents"] == 12
    assert body["totalEvidence"] is None
    evidence = body["metrics"]["totalEvidence"]
    assert evidence["value"] is None
    assert evidence["status"] == "unavailable"
    assert body["metrics"]["evidenceReachable"]["status"] == "unavailable"


# ── /api/metrics ──────────────────────────────────────────────────────────

def test_api_metrics_parses_lines_skipping_comments(client, monkeypatch):
    _patch_metrics(monkeypatch, "# a comment\nfoo_total 5\nbar{x=\"y\"} 9\n")
    r = client.get("/api/metrics")
    assert r.status_code == 200
    body = r.json()
    assert body["foo_total"] == "5"
    assert body['bar{x="y"}'] == "9"
    assert all(not k.startswith("#") for k in body)


# ── /api/indexer-stats ─────────────────────────────────────────────────────

def test_api_indexer_stats_passes_through_aggregate(client, monkeypatch):
    stats = {
        "totalGrabs": 100,
        "bitagentGrabs": 60,
        "bitagentWinRate": 0.6,
        "indexers": [{"indexer": "BitAgent (Local DHT)", "grabs": 60}],
        "days": [{"date": "2026-07-01T00:00:00Z", "totalGrabs": 10, "bitagentGrabs": 6}],
    }

    def responder(q, variables):
        # Days must be clamped server-side before reaching the core.
        assert 1 <= variables["input"]["days"] <= 365
        return {"data": {"evidence": {"indexerStats": stats}}}

    _patch_gql_query(monkeypatch, responder)
    r = client.get("/api/indexer-stats?days=99999")
    assert r.status_code == 200
    body = r.json()
    assert body["available"] is True
    assert body["bitagentWinRate"] == 0.6
    assert body["indexers"][0]["indexer"] == "BitAgent (Local DHT)"


def test_api_indexer_stats_unavailable_on_old_or_down_core(client, monkeypatch):
    # A core that predates evidence.indexerStats returns a GraphQL validation
    # error (data: None) — same shape as unreachable. Either way: available=False.
    _patch_gql_query(monkeypatch, lambda q, v: {"data": None, "errors": [{"message": "x"}]})
    r = client.get("/api/indexer-stats")
    assert r.status_code == 200
    assert r.json() == {"available": False}


# ── /api/filters/status ───────────────────────────────────────────────────

def test_api_filters_status_buckets_drops_and_extensions(client, monkeypatch):
    metrics = (
        "bitagent_contentfilter_examined_total 500\n"
        'bitagent_contentfilter_drop_total{reason="blocked_extension"} 12\n'
        'bitagent_contentfilter_drop_total{reason="nsfw_keyword"} 4\n'
        'bitagent_contentfilter_blocked_ext_total{ext="exe"} 7\n'
        'bitagent_contentfilter_blocked_ext_total{ext="lnk"} 3\n'
        "bitagent_csam_blocklist_entries 99\n"
    )
    _patch_metrics(monkeypatch, metrics)
    r = client.get("/api/filters/status")
    assert r.status_code == 200
    body = r.json()
    assert body["available"] is True
    assert body["status"]["key"] == "active"
    assert body["examined"] == 500
    assert body["liveDropTotal"] == 16
    assert body["wouldDropTotal"] == 0
    assert body["drops"]["blocked_extension"] == 12
    assert body["drops"]["nsfw_keyword"] == 4
    assert body["drops"]["non_latin_script"] == 0  # absent -> 0
    # blocked extensions sorted by count desc
    assert body["blockedExtensions"][0] == {"ext": "exe", "count": 7}
    assert body["csam"]["blocklistEntries"] == 99


def test_api_filters_status_reports_shadow_decisions_separately(client, monkeypatch):
    """The production core emits would_drop, not drop, while enforcement is off.
    Flattening both families made a filter with millions of decisions render
    as three zeroes and three 'no activity' badges."""
    _patch_metrics(monkeypatch, (
        "bitagent_contentfilter_examined_total 1000\n"
        "bitagent_contentfilter_keep_total 1000\n"
        'bitagent_contentfilter_would_drop_total{reason="blocked_extension"} 220\n'
        'bitagent_contentfilter_would_drop_total{reason="non_latin_script"} 330\n'
        'bitagent_contentfilter_would_drop_total{reason="nsfw_keyword"} 45\n'
        'bitagent_contentfilter_blocked_ext_total{ext="zip"} 150\n'
        'bitagent_contentfilter_blocked_ext_total{ext="rar"} 70\n'
    ))
    body = client.get("/api/filters/status").json()

    assert body["available"] is True
    assert body["status"]["key"] == "shadow"
    assert "retained" in body["status"]["detail"]
    assert body["drops"] == {
        "blocked_extension": 0, "non_latin_script": 0, "nsfw_keyword": 0,
    }
    assert body["wouldDrops"] == {
        "blocked_extension": 220, "non_latin_script": 330, "nsfw_keyword": 45,
    }
    assert body["wouldDropTotal"] == 595
    # blocked_ext_total deliberately increments for both enforced and shadow
    # decisions; the stage status tells the UI how to label this breakdown.
    assert body["blockedExtensions"] == [
        {"ext": "zip", "count": 150}, {"ext": "rar", "count": 70},
    ]


def test_api_filters_status_distinguishes_unreachable_from_zero(client, monkeypatch):
    _patch_metrics(monkeypatch, "")
    body = client.get("/api/filters/status").json()
    assert body["available"] is False
    assert body["status"]["key"] == "unavailable"


# ── /api/torrents (search + server-side filtering) ────────────────────────

def _search_block(items, total=None, aggregations=None):
    return {"data": {"torrentContent": {"search": {
        "totalCount": total if total is not None else len(items),
        "items": items,
        "aggregations": aggregations or {},
    }}}}


def _anime_item(info_hash, name, *, torrent_name=None, source=None, cid=None,
                ct="tv_show", seeders=0, original_language="", original_title="",
                release_group=""):
    return {
        "infoHash": info_hash,
        "title": name,
        "contentType": ct,
        "contentSource": source,
        "contentId": cid,
        "seeders": seeders,
        "leechers": 0,
        "createdAt": "t0",
        "updatedAt": "t1",
        "languages": [{"id": "en"}],
        "videoResolution": "V1080p",
        "videoSource": "WEB_DL",
        "releaseGroup": release_group,
        "content": {
            "originalLanguage": (
                {"id": original_language, "name": "Japanese"}
                if original_language else None
            ),
            "originalTitle": original_title,
        },
        "torrent": {"name": torrent_name or name, "size": 100, "filesCount": 1},
    }


def test_api_torrents_maps_items(client, monkeypatch, fresh_db):
    item = {
        "infoHash": "hash1", "title": "Cool Movie 2024",
        "contentType": "movie", "contentSource": "tmdb", "contentId": "tt1",
        "seeders": 50, "leechers": 5, "createdAt": "t0", "updatedAt": "t1",
        "languages": [{"id": "en"}], "videoResolution": "1080p",
        "videoSource": "bluray", "releaseGroup": "GRP",
        "torrent": {"name": "Cool.Movie.2024", "size": 999, "filesCount": 3},
    }
    _patch_gql_query(monkeypatch, lambda q, v=None: _search_block([item], total=1))
    r = client.get("/api/torrents", params={"q": "cool"})
    assert r.status_code == 200
    body = r.json()
    assert body["totalCount"] == 1
    out = body["items"][0]
    assert out["infoHash"] == "hash1"
    assert out["name"] == "Cool Movie 2024"
    assert out["isMapped"] is True
    assert out["languages"] == ["en"]
    assert out["size"] == 999


def test_api_torrents_block_phrase_excludes_and_counts_hit(client, monkeypatch, fresh_db):
    # Seed a block phrase that matches one of two results.
    client.post("/api/block-phrases", json={"pattern": "spam", "note": "junk"})
    items = [
        {"infoHash": "h1", "title": "Good Release", "contentSource": "x",
         "torrent": {"name": "Good"}},
        {"infoHash": "h2", "title": "SPAM Garbage", "contentSource": "x",
         "torrent": {"name": "Spam"}},
    ]
    _patch_gql_query(monkeypatch, lambda q, v=None: _search_block(items, total=2))
    r = client.get("/api/torrents")
    body = r.json()
    names = [it["name"] for it in body["items"]]
    assert "Good Release" in names
    assert "SPAM Garbage" not in names
    assert body["excludedByBlockPhrases"] == 1
    # totalCount discounts the excluded row.
    assert body["totalCount"] == 1
    # The hit counter was bumped.
    phrases = client.get("/api/block-phrases").json()["items"]
    assert phrases[0]["hits"] == 1


def test_api_torrents_block_phrase_hits_batched_across_items(client, monkeypatch, fresh_db):
    # One phrase matching several items in a request is flushed as a single
    # batched increment (count == matches), not one commit per item.
    client.post("/api/block-phrases", json={"pattern": "spam"})
    items = [
        {"infoHash": f"h{i}", "title": f"SPAM {i}", "contentSource": "x",
         "torrent": {"name": "s"}}
        for i in range(3)
    ]
    _patch_gql_query(monkeypatch, lambda q, v=None: _search_block(items, total=3))
    r = client.get("/api/torrents")
    assert r.json()["excludedByBlockPhrases"] == 3
    phrases = client.get("/api/block-phrases").json()["items"]
    assert phrases[0]["hits"] == 3


def test_api_torrents_response_cached_by_params(client, monkeypatch, fresh_db):
    calls = {"n": 0}
    item = {"infoHash": "h1", "title": "Cached", "contentSource": "tmdb",
            "contentId": "c1", "torrent": {"name": "c"}}

    def responder(q, v=None):
        calls["n"] += 1
        return _search_block([item], total=1)

    _patch_gql_query(monkeypatch, responder)
    r1 = client.get("/api/torrents", params={"q": "cached"})
    r2 = client.get("/api/torrents", params={"q": "cached"})
    assert r1.json() == r2.json()
    assert calls["n"] == 1  # second request served from the response cache
    # Different params are a cache miss.
    client.get("/api/torrents", params={"q": "other"})
    assert calls["n"] == 2


def test_api_torrents_cache_invalidated_on_block_phrase_change(client, monkeypatch, fresh_db):
    calls = {"n": 0}
    item = {"infoHash": "h1", "title": "Thing", "contentSource": "tmdb",
            "contentId": "c1", "torrent": {"name": "t"}}

    def responder(q, v=None):
        calls["n"] += 1
        return _search_block([item], total=1)

    _patch_gql_query(monkeypatch, responder)
    client.get("/api/torrents", params={"q": "x"})
    client.get("/api/torrents", params={"q": "x"})
    assert calls["n"] == 1
    # A block-phrase mutation must drop the cached responses (blocked set changed).
    client.post("/api/block-phrases", json={"pattern": "zzz"})
    client.get("/api/torrents", params={"q": "x"})
    assert calls["n"] == 2


def test_api_torrents_cache_key_disambiguates_separator(client, monkeypatch, fresh_db):
    # q="a" + content_types="|b" vs q="a|" + content_types="b" must NOT share a
    # cache entry (a naive '|'-join would collide across the field boundary).
    seen = []

    def responder(q, v=None):
        seen.append(v["input"].get("queryString"))
        return _search_block([{"infoHash": "h" + str(len(seen)), "title": "T",
                               "contentSource": "tmdb", "contentId": "c",
                               "torrent": {"name": "t"}}], total=1)

    _patch_gql_query(monkeypatch, responder)
    client.get("/api/torrents", params={"q": "a", "content_types": "|b"})
    client.get("/api/torrents", params={"q": "a|", "content_types": "b"})
    # Two distinct queries → two upstream calls (no false cache hit).
    assert seen == ["a", "a|"]


def test_api_torrents_unmapped_only_filter_sets_unknown_total(client, monkeypatch, fresh_db):
    items = [
        {"infoHash": "h1", "title": "Mapped", "contentSource": "tmdb",
         "torrent": {"name": "m"}},
        {"infoHash": "h2", "title": "Unmapped", "contentSource": None,
         "torrent": {"name": "u"}},
    ]
    _patch_gql_query(monkeypatch, lambda q, v=None: _search_block(items))
    r = client.get("/api/torrents", params={"unmapped_only": "true"})
    body = r.json()
    assert [it["name"] for it in body["items"]] == ["Unmapped"]
    # totalCount is unknown (-1) when any server-side filter is active.
    assert body["totalCount"] == -1


def test_api_torrents_limit_validation(client):
    # limit > 500 violates the Query(le=500) constraint.
    r = client.get("/api/torrents", params={"limit": 9999})
    assert r.status_code == 422


def test_api_torrents_passes_public_facets_and_returns_aggregations(client, monkeypatch, fresh_db):
    captured = {}

    def responder(q, variables=None):
        captured["input"] = variables["input"]
        return _search_block([], total=0, aggregations={
            "genre": [{"value": "Action", "count": 4}],
            "releaseYear": [{"value": 2024, "count": 2}],
            "videoResolution": [{"value": "V1080p", "count": 3}],
            "videoSource": [{"value": "WEB_DL", "count": 1}],
        })

    _patch_gql_query(monkeypatch, responder)
    r = client.get("/api/torrents", params={
        "content_types": "movie",
        "genres": "Action",
        "years": "2024",
        "qualities": "1080p",
        "sources": "WEB_DL",
        "aggregate_facets": "true",
    })
    assert r.status_code == 200
    facets = captured["input"]["facets"]
    assert facets["contentType"] == {"filter": ["movie"], "aggregate": True}
    assert facets["genre"] == {"filter": ["Action"], "aggregate": True}
    assert facets["releaseYear"] == {"filter": [2024], "aggregate": True}
    assert facets["videoResolution"] == {"filter": ["V1080p"], "aggregate": True}
    assert facets["videoSource"] == {"filter": ["WEB_DL"], "aggregate": True}
    assert r.json()["aggregations"]["genre"][0]["value"] == "Action"


def test_api_torrents_anime_discovers_release_markers_and_dedupes_series(client, monkeypatch, fresh_db):
    captured = []

    def responder(q, variables=None):
        input_ = variables["input"]
        captured.append(input_)
        query = input_.get("queryString")
        if query == "" and input_.get("offset") == 0:
            return _search_block([
                _anime_item(
                    "mapped-ja",
                    "Kaiju No. 8 S01E01",
                    source="tmdb",
                    cid="123",
                    seeders=20,
                    original_language="ja",
                    original_title="怪獣８号",
                ),
                _anime_item(
                    "western-animation",
                    "Spider-Man The Animated Series S01E01",
                    source="tmdb",
                    cid="456",
                    seeders=99,
                    original_language="en",
                ),
            ], aggregations={"genre": [{"value": "tmdb:16", "count": 2}]})
        if query == "subsplease":
            return _search_block([
                _anime_item(
                    "ep1",
                    "[SubsPlease] Frieren - Beyond Journey's End - 01 (1080p)",
                    torrent_name="[SubsPlease] Frieren - Beyond Journey's End - 01 (1080p).mkv",
                    source=None,
                    cid=None,
                    seeders=15,
                ),
                _anime_item(
                    "ep2",
                    "[SubsPlease] Frieren - Beyond Journey's End - 02 (1080p)",
                    torrent_name="[SubsPlease] Frieren - Beyond Journey's End - 02 (1080p).mkv",
                    source=None,
                    cid=None,
                    seeders=40,
                ),
                _anime_item("noise", "Die Hard 2", seeders=80),
            ])
        return _search_block([])

    _patch_gql_query(monkeypatch, responder)
    r = client.get("/api/torrents", params={"anime": "true", "limit": 10})

    assert r.status_code == 200
    body = r.json()
    names = [it["name"] for it in body["items"]]
    assert names == ["Frieren - Beyond Journey's End", "Kaiju No. 8 S01E01"]
    assert body["totalCount"] == 2
    frieren = body["items"][0]
    assert frieren["infoHash"] == "ep2"  # highest-seeded episode represents the series
    assert frieren["isMapped"] is False
    assert "Spider-Man" not in " ".join(names)
    assert "Die Hard" not in " ".join(names)
    queries = {c.get("queryString") for c in captured}
    assert "" in queries
    assert "subsplease" in queries
    assert captured[0]["facets"]["contentType"]["filter"] == ["movie", "tv_show"]


def test_api_torrents_anime_respects_hide_unmapped_after_auto_browse(client, monkeypatch, fresh_db):
    def responder(q, variables=None):
        query = variables["input"].get("queryString")
        if query == "" and variables["input"].get("offset") == 0:
            return _search_block([
                _anime_item("mapped", "Paprika", source="tmdb", cid="789",
                            original_language="ja", seeders=5),
            ])
        if query == "subsplease":
            return _search_block([
                _anime_item("unmapped", "[SubsPlease] Unmapped Anime - 01", seeders=50),
            ])
        return _search_block([])

    _patch_gql_query(monkeypatch, responder)
    r = client.get("/api/torrents", params={"anime": "true", "hide_unmapped": "true"})

    assert r.status_code == 200
    body = r.json()
    assert [it["name"] for it in body["items"]] == ["Paprika"]
    assert body["totalCount"] == 1


def test_anime_release_title_preserves_real_parenthetical_alias():
    raw = "[Judas] Kaifuku Jutsushi no Yarinaoshi (Redo of Healer) (Season 1) [BD 1080p]"
    assert app_module._anime_release_title(raw) == "Kaifuku Jutsushi no Yarinaoshi (Redo of Healer)"


# ── grouping-key season-boundary parity with the frontend normGroupTitle ───
# The two backend grouping-key normalizers — _norm_title/_NORM_STRIP (powers the
# /api/titles/releases unmapped-title match) and _anime_release_title — mirror
# the frontend static/js/norm-title.js normGroupTitle(). The frontend replaced the
# fragile trailing-`\b` season stripper with `(?!\d)(?!E)` on the frontend so a
# season token followed by a word char ("S02_complete" — `_` is a word char, so
# `\b` never fires) still strips and groups onto the base title. These assert
# both backend copies now match that behavior, and that a 3-digit token (S123)
# is not mistaken for a 1-2 digit season and over-stripped.

def test_norm_title_season_underscore_groups_with_bare_season():
    assert (
        app_module._norm_title("House of the Dragon S02_complete")
        == app_module._norm_title("House of the Dragon S01")
        == "house of the dragon"
    )


def test_norm_title_does_not_overstrip_three_digit_season_token():
    assert app_module._norm_title("Area S123") == "area s123"


def test_anime_release_title_season_underscore_groups_with_bare_season():
    # _anime_release_title preserves case (it is not lower-cased like _norm_title).
    assert (
        app_module._anime_release_title("Frieren S02_complete")
        == app_module._anime_release_title("Frieren S01")
        == "Frieren"
    )


def test_anime_release_title_does_not_overstrip_three_digit_season_token():
    assert app_module._anime_release_title("Case S123") == "Case S123"


# ── /api/titles/releases (all releases for one title) ─────────────────────

def _rel_item(info_hash, name, *, source="tmdb", cid="99", ct="movie",
              seeders=0, res=None):
    return {
        "infoHash": info_hash, "title": name, "contentType": ct,
        "contentSource": source, "contentId": cid, "seeders": seeders,
        "leechers": 0, "createdAt": "t0", "updatedAt": "t1",
        "languages": [{"id": "en"}], "videoResolution": res,
        "torrent": {"name": name, "size": 100, "filesCount": 1},
    }


def test_api_title_releases_requires_title(client):
    r = client.get("/api/titles/releases")
    assert r.status_code == 400


def test_api_title_releases_mapped_matches_by_content_id(client, monkeypatch, fresh_db):
    # Two releases share the requested tmdb id; a third is a different film that
    # merely matched the full-text queryString. Only the two must come back.
    items = [
        _rel_item("h1", "Dune 2021 1080p", cid="438631", seeders=30, res="1080p"),
        _rel_item("h2", "Dune 2021 2160p Directors Cut", cid="438631", seeders=12, res="2160p"),
        _rel_item("h3", "Dune Prophecy S01", cid="111111", seeders=99, ct="tv_show"),
    ]
    _patch_gql_query(monkeypatch, lambda q, v=None: _search_block(items))
    r = client.get("/api/titles/releases", params={
        "title": "Dune", "content_type": "movie",
        "content_source": "tmdb", "content_id": "438631",
    })
    assert r.status_code == 200
    body = r.json()
    hashes = {it["infoHash"] for it in body["items"]}
    assert hashes == {"h1", "h2"}
    assert body["totalCount"] == 2


def test_api_title_releases_unmapped_matches_by_normalised_title(client, monkeypatch, fresh_db):
    items = [
        {"infoHash": "u1", "title": "Some Indie Film 1080p BluRay",
         "contentSource": None, "contentId": None, "contentType": "movie",
         "torrent": {"name": "Some Indie Film 1080p BluRay", "size": 1}},
        {"infoHash": "u2", "title": "Some Indie Film 720p WEB-DL",
         "contentSource": None, "contentId": None, "contentType": "movie",
         "torrent": {"name": "Some Indie Film 720p WEB-DL", "size": 1}},
        {"infoHash": "u3", "title": "Totally Different Movie",
         "contentSource": None, "contentId": None, "contentType": "movie",
         "torrent": {"name": "Totally Different Movie", "size": 1}},
    ]
    _patch_gql_query(monkeypatch, lambda q, v=None: _search_block(items))
    r = client.get("/api/titles/releases", params={"title": "Some Indie Film"})
    body = r.json()
    assert {it["infoHash"] for it in body["items"]} == {"u1", "u2"}


def test_api_title_releases_unmapped_groups_underscore_season(client, monkeypatch, fresh_db):
    # End-to-end regrouping guard for the _norm_title season-boundary fix: an
    # unmapped TV title whose release name carries an underscore-joined season
    # token ("S02_complete") must normalise to the same key as the bare "S01"
    # release and come back for the base-title query. Before the fix the trailing
    # `\b` left "S02_complete" un-stripped, so this release was silently dropped.
    items = [
        {"infoHash": "s1", "title": "House of the Dragon S01",
         "contentSource": None, "contentId": None, "contentType": "tv_show",
         "torrent": {"name": "House of the Dragon S01", "size": 1}},
        {"infoHash": "s2", "title": "House of the Dragon S02_complete",
         "contentSource": None, "contentId": None, "contentType": "tv_show",
         "torrent": {"name": "House of the Dragon S02_complete", "size": 1}},
        {"infoHash": "s3", "title": "Some Other Show S01",
         "contentSource": None, "contentId": None, "contentType": "tv_show",
         "torrent": {"name": "Some Other Show S01", "size": 1}},
    ]
    _patch_gql_query(monkeypatch, lambda q, v=None: _search_block(items))
    r = client.get("/api/titles/releases", params={"title": "House of the Dragon"})
    body = r.json()
    assert {it["infoHash"] for it in body["items"]} == {"s1", "s2"}


def test_api_title_releases_honours_block_phrases(client, monkeypatch, fresh_db):
    client.post("/api/block-phrases", json={"pattern": "cam", "note": "junk"})
    items = [
        _rel_item("h1", "Dune 2021 1080p", cid="438631", seeders=30),
        _rel_item("h2", "Dune 2021 CAM", cid="438631", seeders=1),
    ]
    _patch_gql_query(monkeypatch, lambda q, v=None: _search_block(items))
    r = client.get("/api/titles/releases", params={
        "title": "Dune", "content_type": "movie",
        "content_source": "tmdb", "content_id": "438631",
    })
    body = r.json()
    assert {it["infoHash"] for it in body["items"]} == {"h1"}


# ── /api/torrents/{hash} (detail + 404 + error surface) ───────────────────

def test_api_torrent_detail_maps_full_record(client, monkeypatch):
    # Core-schema shape: matched metadata on `content` (list-of-links
    # externalLinks, LanguageInfo originalLanguage), episode structure on
    # `episodes`, file list nested under `torrent`. movie/tvShow/tvEpisode/
    # tvSeason fields DO NOT exist on TorrentContent.
    detail_item = {
        "infoHash": "abc", "title": "Movie X", "contentType": "movie",
        "contentSource": "tmdb", "contentId": "42",
        "seeders": 10, "leechers": 1,
        "createdAt": "t0", "updatedAt": "t1", "languages": [{"id": "en"}],
        "videoResolution": "2160p", "videoSource": "web", "videoCodec": "hevc",
        "releaseGroup": "RG",
        "episodes": None,
        "content": {
            "title": "Movie X", "releaseYear": 2020,
            "originalLanguage": {"id": "en"},
            "externalLinks": [
                {"metadataSource": {"key": "imdb"}, "url": "https://www.imdb.com/title/tt9/"},
                {"metadataSource": {"key": "tmdb"}, "url": "https://www.themoviedb.org/movie/42"},
            ],
        },
        "torrent": {"name": "Movie.X", "size": 100, "filesCount": 2,
                    "files": [{"path": "a.mkv", "size": 90}, {"path": "b.srt", "size": 10}]},
    }
    _patch_gql_query(monkeypatch, lambda q, v=None: _search_block([detail_item]))
    r = client.get("/api/torrents/abc")
    assert r.status_code == 200
    body = r.json()
    assert body["infoHash"] == "abc"
    assert body["imdbId"] == "tt9"
    assert body["tmdbId"] == "42"
    assert body["originalLanguage"] == "en"
    assert body["files"] == [{"path": "a.mkv", "size": 90}, {"path": "b.srt", "size": 10}]
    assert body["magnetUri"].startswith("magnet:?xt=urn:btih:abc")


def test_api_torrent_detail_query_uses_content_not_movie_tvshow(client, monkeypatch):
    # Same schema-drift guard as recent-matches: an invalid field fails the
    # whole query and the modal 404s for every torrent.
    captured = {}

    def responder(q, v=None):
        captured["query"] = q
        return _search_block([])

    _patch_gql_query(monkeypatch, responder)
    client.get("/api/torrents/abc")
    assert "content {" in captured["query"]
    assert "movie {" not in captured["query"]
    assert "tvShow {" not in captured["query"]
    assert "tvEpisode {" not in captured["query"]


def test_api_torrent_detail_maps_episode_structure(client, monkeypatch):
    detail_item = {
        "infoHash": "abc", "title": "Show S02E03", "contentType": "tv_show",
        "contentSource": "tmdb", "contentId": "7",
        "episodes": {"label": "S02E03", "seasons": [{"season": 2, "episodes": [3]}]},
        "content": {"title": "Show", "releaseYear": 2019,
                    "originalLanguage": None, "externalLinks": []},
        "torrent": {"name": "Show.S02E03", "size": 1, "filesCount": 1, "files": []},
    }
    _patch_gql_query(monkeypatch, lambda q, v=None: _search_block([detail_item]))
    body = client.get("/api/torrents/abc").json()
    assert body["seasonNumber"] == 2
    assert body["episodeNumber"] == 3
    assert body["episodeTitle"] == "S02E03"


def test_api_torrent_detail_404_when_not_found(client, monkeypatch):
    _patch_gql_query(monkeypatch, lambda q, v=None: _search_block([]))
    r = client.get("/api/torrents/nope")
    assert r.status_code == 404


def test_api_torrent_detail_502_on_core_query_error(client, monkeypatch):
    # A failed query must not masquerade as "not found" — that hid the
    # movie/tvShow schema drift behind blanket 404s.
    def responder(q, v=None):
        return {"data": None, "errors": [{"message": "boom"}]}
    _patch_gql_query(monkeypatch, responder)
    r = client.get("/api/torrents/abc")
    assert r.status_code == 502


# ── /api/evidence ─────────────────────────────────────────────────────────

def test_api_evidence_proxies_reshaped_list(client, monkeypatch):
    async def fake_list(limit, offset):
        return {"totalCount": 2, "items": [{"id": 1}, {"id": 2}]}
    monkeypatch.setattr(gql, "fetch_evidence_list", fake_list)
    r = client.get("/api/evidence", params={"limit": 10})
    assert r.status_code == 200
    assert r.json()["totalCount"] == 2


# ── /api/wants CRUD (real DB) ─────────────────────────────────────────────

# ── /api/wants/sync (Sonarr/Radarr) ───────────────────────────────────────

# ── /api/arr/push ─────────────────────────────────────────────────────────

def test_arr_push_rejects_unknown_arr(client):
    r = client.post("/api/arr/push", json={"arr": "plex", "info_hash": "h", "title": "t"})
    assert r.status_code == 400


def test_arr_push_400_when_not_configured(client, fresh_db):
    config.settings.sonarr_base_url = ""
    config.settings.sonarr_api_key = ""
    r = client.post("/api/arr/push", json={"arr": "sonarr", "info_hash": "h", "title": "t"})
    assert r.status_code == 400
    assert "not configured" in r.json()["detail"]


def test_arr_push_success(client, fresh_db, monkeypatch):
    # Private IP so the SSRF guard allows it (resolves offline); push is mocked.
    config.settings.radarr_base_url = "http://10.0.0.9:7878"
    config.settings.radarr_api_key = "k"

    class FakeResp:
        is_success = True
        status_code = 201

    class FakeClient:
        async def post(self, url, **kw):
            assert "/api/v3/release/push" in url
            return FakeResp()

    monkeypatch.setattr(gql, "_get_client", lambda: FakeClient())
    r = client.post("/api/arr/push",
                    json={"arr": "radarr", "info_hash": "abc", "title": "Movie"})
    assert r.status_code == 200
    assert r.json()["status"] == "pushed"


# ── /api/settings + overrides + audit (real DB) ───────────────────────────

def test_settings_lists_mutable_fields(client, fresh_db):
    r = client.get("/api/settings")
    assert r.status_code == 200
    body = r.json()
    assert "tmdb_api_key" in body["fields"]
    assert "tmdb_api_key" in body["mutable_keys"]
    secret_field = body["fields"]["tmdb_api_key"]
    assert secret_field["sensitive"] is True
    assert "current" not in secret_field
    assert "default" not in secret_field


def test_settings_override_round_trip_and_audit(client, fresh_db):
    # Set
    r = client.put("/api/settings/overrides/log_level", json={"value": "debug"})
    assert r.status_code == 200
    body = client.get("/api/settings").json()
    assert body["fields"]["log_level"]["current"] == "debug"
    assert body["fields"]["log_level"]["overridden"] is True
    # Audit recorded the change
    audit = client.get("/api/settings/audit").json()
    assert any(a["key"] == "log_level" and a["new"] == "debug" for a in audit)
    # Delete
    assert client.delete("/api/settings/overrides/log_level").status_code == 200
    body = client.get("/api/settings").json()
    assert body["fields"]["log_level"]["overridden"] is False


def test_sensitive_settings_and_audit_are_metadata_only(client, fresh_db):
    startup_value = "startup-value-never-return"
    replacement = "replacement-value-never-return"
    replacement_two = "second-replacement-never-return"
    config.settings.tmdb_api_key = startup_value

    saved = client.put(
        "/api/settings/overrides/tmdb_api_key", json={"value": replacement}
    )
    assert saved.status_code == 200
    assert saved.json()["configured"] is True
    assert saved.json()["source"] == "override"
    assert startup_value not in saved.text
    assert replacement not in saved.text

    settings_body = client.get("/api/settings")
    assert startup_value not in settings_body.text
    assert replacement not in settings_body.text
    field = settings_body.json()["fields"]["tmdb_api_key"]
    assert field == {
        "sensitive": True,
        "configured": True,
        "source": "override",
        "overridden": True,
    }

    # Replacement and deletion must redact the previous stored override too.
    replaced = client.put(
        "/api/settings/overrides/tmdb_api_key", json={"value": replacement_two}
    )
    deleted = client.delete("/api/settings/overrides/tmdb_api_key")
    assert replaced.status_code == 200
    assert deleted.status_code == 200
    assert replacement not in replaced.text
    assert replacement_two not in replaced.text

    audit = client.get("/api/settings/audit")
    assert startup_value not in audit.text
    assert replacement not in audit.text
    assert replacement_two not in audit.text
    events = [a for a in audit.json() if a["key"] == "tmdb_api_key"]
    assert len(events) == 3
    assert all(event["old"] in {None, "[redacted]"} for event in events)
    assert all(event["new"] in {None, "[redacted]"} for event in events)

    # Defense in depth is at persistence time, not only response serialization.
    async def _raw_audit():
        db = await app_module.get_db()
        return await db.execute_fetchall(
            "SELECT old_value, new_value FROM audit_log WHERE key = 'tmdb_api_key'"
        )
    raw = asyncio.run(_raw_audit())
    persisted_values = {value for row in raw for value in row}
    assert persisted_values <= {None, "[redacted]"}
    assert replacement not in persisted_values
    assert replacement_two not in persisted_values


def test_settings_audit_records_authenticated_actor_on_set_and_delete(client, fresh_db):
    config.settings.require_auth = True
    config.settings.trust_npm_headers = False
    config.settings.trust_forwarded_user = True
    config.settings.trusted_proxy_cidrs = "127.0.0.0/8"
    config.settings.proxy_auth_secret = "test-proof-at-least-thirty-two-bytes"
    config.settings.operator_roles = "OWNER"
    headers = {
        "X-Forwarded-User": "7000000001",
        "X-Auth-Priv": "OWNER",
        "X-BitAgent-Proxy-Proof": "test-proof-at-least-thirty-two-bytes",
    }

    assert client.put(
        "/api/settings/overrides/log_level",
        json={"value": "debug"}, headers=headers,
    ).status_code == 200
    assert client.delete(
        "/api/settings/overrides/log_level", headers=headers,
    ).status_code == 200
    events = [
        a for a in client.get("/api/settings/audit", headers=headers).json()
        if a["key"] == "log_level"
    ]
    assert len(events) == 2
    assert {event["actor"] for event in events} == {"7000000001"}


def test_settings_override_rejects_immutable_field(client, fresh_db):
    # dashboard_api_key is NOT in MUTABLE_FIELDS.
    r = client.put("/api/settings/overrides/dashboard_api_key", json={"value": "x"})
    assert r.status_code == 403
    r = client.delete("/api/settings/overrides/dashboard_api_key")
    assert r.status_code == 403


def test_settings_delete_missing_override_404(client, fresh_db):
    r = client.delete("/api/settings/overrides/log_level")
    assert r.status_code == 404


# ── /api/account + /torznab proxy (real DB, fake upstream) ─────────────────

def test_account_api_key_lifecycle_never_re_echoes_secret(client, fresh_db):
    r = client.get("/api/account")
    assert r.status_code == 200
    assert r.json()["apiKey"] is None

    created = client.post("/api/account/api-key", json={"name": "arr"}).json()
    secret = created["apiKeySecret"]
    assert secret.startswith("ba_")
    assert created["apiKey"]["prefix"].startswith("ba_")

    after = client.get("/api/account").json()
    assert after["apiKey"]["prefix"] == created["apiKey"]["prefix"]
    assert "apiKeySecret" not in after
    assert secret not in str(after)

    rotated = client.post("/api/account/api-key", json={"name": "arr"}).json()
    assert rotated["apiKeySecret"] != secret

    deleted = client.delete("/api/account/api-key").json()
    assert deleted["apiKey"] is None


def test_torznab_proxy_rejects_missing_or_bad_key(client, fresh_db):
    assert client.get("/torznab/api?t=caps").status_code == 401
    bad = client.get("/torznab/api?t=caps&apikey=bad")
    assert bad.status_code == 401
    assert "<error" in bad.text


def test_torznab_proxy_validates_user_key_and_swaps_core_key(client, monkeypatch, fresh_db):
    config.settings.bitagent_torznab_url = "http://core:3333/torznab"
    config.settings.torznab_api_key = "core-secret"
    user_key = client.post("/api/account/api-key", json={"name": "arr"}).json()["apiKeySecret"]
    captured = {}

    class _Resp:
        status_code = 200
        content = b"<caps/>"
        headers = {"content-type": "application/xml"}

    class _Client:
        async def __aenter__(self):
            return self
        async def __aexit__(self, *a):
            return False
        async def request(self, method, url, **kwargs):
            captured["method"] = method
            captured["url"] = url
            captured["params"] = kwargs["params"]
            return _Resp()

    monkeypatch.setattr(app_module.httpx, "AsyncClient", lambda *a, **k: _Client())
    r = client.get(f"/torznab/api?t=caps&cat=2000&apikey={user_key}")
    assert r.status_code == 200
    assert r.text == "<caps/>"
    assert captured["method"] == "GET"
    assert captured["url"] == "http://core:3333/torznab/api"
    assert ("apikey", "core-secret") in captured["params"]
    assert all(v != user_key for k, v in captured["params"] if k.lower() == "apikey")


def test_torznab_proxy_never_leaves_the_torznab_namespace(client, monkeypatch, fresh_db):
    config.settings.bitagent_torznab_url = "http://core:3333/torznab"
    user_key = client.post("/api/account/api-key", json={"name": "arr"}).json()["apiKeySecret"]
    calls = []

    class _Client:
        async def __aenter__(self):
            return self
        async def __aexit__(self, *a):
            return False
        async def request(self, method, url, **kwargs):
            calls.append(url)
            raise AssertionError("upstream must not be reached")

    monkeypatch.setattr(app_module.httpx, "AsyncClient", lambda *a, **k: _Client())
    for path in ("../graphql", "%2e%2e/graphql", "api/../../import", "evidence/arr/x", "api/extra"):
        r = client.get(f"/torznab/{path}?apikey={user_key}")
        assert r.status_code == 404, path
    assert calls == []
    assert client.post(f"/torznab/api?apikey={user_key}").status_code == 405


def test_torznab_proxy_ignores_legacy_frozen_upstream_overrides(
    client, monkeypatch, fresh_db
):
    # Simulate stale rows surviving in (or being reinserted into) the database
    # after startup. Both the explicit Torznab URL and GraphQL fallback must
    # come only from startup settings.
    config.settings.bitagent_torznab_url = ""
    config.settings.bitagent_graphql_url = "http://startup-core:3333/graphql"

    async def _seed_legacy_rows():
        db = await app_module.get_db()
        await db.executemany(
            "INSERT INTO settings_overrides (key, value, updated_at) VALUES (?, ?, ?)",
            [
                ("bitagent_torznab_url", "http://db-override/torznab", 1.0),
                ("bitagent_graphql_url", "http://db-override/graphql", 1.0),
            ],
        )
        await db.commit()

    asyncio.run(_seed_legacy_rows())
    user_key = client.post("/api/account/api-key", json={"name": "arr"}).json()[
        "apiKeySecret"
    ]
    captured = {}

    class _Resp:
        status_code = 200
        content = b"<caps/>"
        headers = {"content-type": "application/xml"}

    class _Client:
        async def __aenter__(self):
            return self

        async def __aexit__(self, *args):
            return False

        async def request(self, method, url, **kwargs):
            captured["url"] = url
            return _Resp()

    monkeypatch.setattr(
        app_module.httpx,
        "AsyncClient",
        lambda *args, **kwargs: _Client(),
    )
    response = client.get(f"/torznab/api?t=caps&apikey={user_key}")

    assert response.status_code == 200
    assert captured["url"] == "http://startup-core:3333/torznab/api"
    assert "db-override" not in captured["url"]


# ── /api/notifications (real DB) ──────────────────────────────────────────

def test_notifications_list_and_mark_read(client, fresh_db):
    async def _seed():
        db = await app_module.get_db()
        await db.execute(
            "INSERT INTO notifications (level, title, message, read, created_at) "
            "VALUES ('warn', 'Heads up', 'something', 0, 123.0)")
        await db.commit()
    asyncio.run(_seed())
    items = client.get("/api/notifications").json()
    assert len(items) == 1
    nid = items[0]["id"]
    assert items[0]["read"] is False
    assert client.put(f"/api/notifications/{nid}/read").json()["status"] == "ok"
    assert client.get("/api/notifications").json()[0]["read"] is True


# ── /api/block-phrases CRUD (real DB) ─────────────────────────────────────

def test_block_phrases_crud(client, fresh_db):
    r = client.post("/api/block-phrases", json={"pattern": "BadWord", "note": "n"})
    assert r.status_code == 200
    pid = r.json()["id"]
    # Stored lowercased? pattern is kept as-is on create; matching lowercases.
    listing = client.get("/api/block-phrases").json()["items"]
    assert any(p["id"] == pid for p in listing)
    # Delete
    assert client.delete(f"/api/block-phrases/{pid}").json()["deleted"] == pid
    assert client.delete(f"/api/block-phrases/{pid}").status_code == 404


def test_block_phrases_rejects_empty_and_too_long(client, fresh_db):
    assert client.post("/api/block-phrases", json={"pattern": "   "}).status_code == 400
    assert client.post("/api/block-phrases",
                       json={"pattern": "x" * 201}).status_code == 400


def test_block_phrases_duplicate_is_409(client, fresh_db):
    client.post("/api/block-phrases", json={"pattern": "dupe"})
    r = client.post("/api/block-phrases", json={"pattern": "dupe"})
    assert r.status_code == 409


# ── /api/graphql passthrough ──────────────────────────────────────────────

def test_graphql_proxy_passes_through(client, monkeypatch):
    captured = {}
    def responder(q, v=None):
        captured["q"], captured["v"] = q, v
        return {"data": {"echo": True}}
    _patch_gql_query(monkeypatch, responder)
    r = client.post("/api/graphql",
                    json={"query": "{ ping }", "variables": {"a": 1}})
    assert r.status_code == 200
    assert r.json()["data"]["echo"] is True
    assert captured["q"] == "{ ping }"
    assert captured["v"] == {"a": 1}


# ── /api/quarantine (proxies core REST) ───────────────────────────────────

class _FakeHttpResp:
    def __init__(self, status_code, payload):
        self.status_code = status_code
        self._payload = payload
    def json(self):
        return self._payload


class _FakeAsyncClient:
    """Stand-in for httpx.AsyncClient as an async context manager."""
    def __init__(self, resp=None, raise_exc=None):
        self._resp = resp
        self._raise = raise_exc
    async def __aenter__(self):
        return self
    async def __aexit__(self, *a):
        return False
    async def get(self, *a, **k):
        return self._dispatch()
    async def post(self, *a, **k):
        return self._dispatch()
    async def request(self, *a, **k):
        return self._dispatch()
    def _dispatch(self):
        if self._raise:
            raise self._raise
        return self._resp


def test_quarantine_list_proxies_core(client, monkeypatch):
    resp = _FakeHttpResp(200, {"items": [{"infoHash": "h"}],
                               "totalCount": 1, "windowDays": 7})
    monkeypatch.setattr(app_module.httpx, "AsyncClient",
                        lambda *a, **k: _FakeAsyncClient(resp=resp))
    r = client.get("/api/quarantine")
    assert r.status_code == 200
    assert r.json()["windowDays"] == 7


def test_quarantine_restore_proxies_status(client, monkeypatch):
    resp = _FakeHttpResp(200, {"status": "restored"})
    monkeypatch.setattr(app_module.httpx, "AsyncClient",
                        lambda *a, **k: _FakeAsyncClient(resp=resp))
    r = client.post("/api/quarantine/abc/restore")
    assert r.status_code == 200
    assert r.json()["status"] == "restored"


def test_quarantine_unreachable_core_is_502(client, monkeypatch):
    monkeypatch.setattr(
        app_module.httpx, "AsyncClient",
        lambda *a, **k: _FakeAsyncClient(raise_exc=httpx.ConnectError("down")))
    r = client.get("/api/quarantine")
    assert r.status_code == 502


# ── /api/poster + poster-proxy ────────────────────────────────────────────

def test_poster_404_when_no_data(client, monkeypatch):
    async def none_poster(*a, **k):
        return None
    monkeypatch.setattr(tmdb, "get_poster", none_poster)
    r = client.get("/api/poster/12345")
    assert r.status_code == 404


def test_poster_redirect_302_to_cover(client, monkeypatch):
    # The library grid renders <img src="/api/poster/{id}?redirect=1">; the route
    # must 302 to the resolved cover so the browser loads it natively.
    async def fake_poster(*a, **k):
        return {"poster_url": "https://image.tmdb.org/t/p/w300/x.jpg", "title": "X", "year": "2020"}
    monkeypatch.setattr(tmdb, "get_poster", fake_poster)
    r = client.get("/api/poster/99?redirect=1", follow_redirects=False)
    assert r.status_code == 302
    assert r.headers["location"] == "https://image.tmdb.org/t/p/w300/x.jpg"
    assert "max-age" in r.headers.get("cache-control", "")


def test_poster_redirect_blank_when_no_data(client, monkeypatch):
    # A poster miss in redirect mode serves a 1x1 transparent GIF (200) so the
    # grid <img> loads cleanly and the CSS placeholder shows through — instead of
    # a 404 logged for every unmatched / no-art card.
    async def none_poster(*a, **k):
        return None
    monkeypatch.setattr(tmdb, "get_poster", none_poster)
    r = client.get("/api/poster/99?redirect=1", follow_redirects=False)
    assert r.status_code == 200
    assert r.headers["content-type"] == "image/gif"


def test_poster_json_still_default(client, monkeypatch):
    # Without redirect=1 the endpoint returns JSON (used by the detail meta path).
    async def fake_poster(*a, **k):
        return {"poster_url": "https://image.tmdb.org/t/p/w300/x.jpg", "title": "X", "year": "2020"}
    monkeypatch.setattr(tmdb, "get_poster", fake_poster)
    r = client.get("/api/poster/99")
    assert r.status_code == 200
    assert r.json()["poster_url"].endswith("/x.jpg")


def test_poster_proxy_rejects_bad_scheme(client):
    r = client.get("/api/poster-proxy", params={"url": "ftp://evil/x.png"})
    assert r.status_code == 400


def test_poster_proxy_rejects_foreign_host(client, fresh_db):
    config.settings.lidarr_base_url = "http://lidarr:8686"
    r = client.get("/api/poster-proxy",
                   params={"url": "http://evil.example/cover.png"})
    assert r.status_code == 403


def test_poster_proxy_403_when_lidarr_unconfigured(client, fresh_db):
    config.settings.lidarr_base_url = ""
    r = client.get("/api/poster-proxy",
                   params={"url": "http://anything/x.png"})
    assert r.status_code == 403


# ── /api/ai/* — LLM TMDB-matcher observability ────────────────────────────

_AI_METRICS_SAMPLE = """
# HELP bitagent_classifier_llm_match_matches_total TMDB ids chosen by the matcher.
bitagent_classifier_llm_match_matches_total{media_type="movie",mode="live"} 35
bitagent_classifier_llm_match_matches_total{media_type="tv",mode="live"} 10
bitagent_classifier_llm_match_matches_total{media_type="movie",mode="shadow"} 5
bitagent_classifier_llm_match_extract_total{result="ok"} 80
bitagent_classifier_llm_match_extract_total{result="empty"} 15
bitagent_classifier_llm_match_extract_total{result="error"} 5
bitagent_classifier_llm_match_rerank_total{result="match"} 50
bitagent_classifier_llm_match_rerank_total{result="none"} 30
bitagent_classifier_llm_match_rerank_total{result="error"} 2
bitagent_classifier_llm_match_anime_total{english="dub",outcome="kept"} 7
bitagent_classifier_llm_match_anime_total{english="none",outcome="rejected"} 3
bitagent_classifier_llm_match_gate_rejects_total{gate="size"} 12
bitagent_classifier_llm_match_gate_rejects_total{gate="plausibility"} 4
bitagent_classifier_llm_match_cache_hits_total 90
bitagent_classifier_llm_match_cache_misses_total 10
bitagent_classifier_llm_match_call_errors_total{stage="extract",class="timeout"} 2
bitagent_classifier_llm_match_call_duration_seconds_bucket{stage="extract",le="+Inf"} 60
bitagent_classifier_llm_match_call_duration_seconds_sum{stage="extract"} 120
bitagent_classifier_llm_match_call_duration_seconds_count{stage="extract"} 60
bitagent_classifier_llm_match_call_duration_seconds_sum{stage="rerank"} 90
bitagent_classifier_llm_match_call_duration_seconds_count{stage="rerank"} 30
bitagent_dashstats_alt_title_content_total 1000
bitagent_dashstats_alt_title_content_checked 250
bitagent_dashstats_alt_title_content_with_alt 180
bitagent_unrelated_metric_total 999
bitagent_contentfilter_llm_calls_total{ok="true"} 190
bitagent_contentfilter_llm_calls_total{ok="false"} 10
bitagent_contentfilter_llm_cache_hits_total 4
bitagent_contentfilter_llm_cache_misses_total 196
bitagent_contentfilter_llm_deferred_total 3
bitagent_contentfilter_llm_budget_exhausted_total 0
bitagent_contentfilter_llm_call_duration_seconds_sum 400
bitagent_contentfilter_llm_call_duration_seconds_count 200
bitagent_contentfilter_drop_total{reason="llm_non_english"} 6
bitagent_contentfilter_drop_total{reason="non_latin_script"} 5994
bitagent_junkpurge_llm_requests_total{model="acme/judge-1",outcome="ok"} 999
bitagent_junkpurge_llm_requests_total{model="acme/judge-1",outcome="error"} 1
bitagent_junkpurge_llm_tokens_total{model="acme/judge-1",type="input"} 50000
bitagent_junkpurge_llm_tokens_total{model="acme/judge-1",type="output"} 2000
bitagent_junkpurge_judged_total{verdict="junk"} 250
bitagent_junkpurge_judged_total{verdict="real_mangled"} 740
bitagent_junkpurge_judged_total{verdict="real_absent"} 10
bitagent_junkpurge_quarantined_total 250
bitagent_junkpurge_expired_total 40
bitagent_junkpurge_cycles_total 4
bitagent_junkpurge_cycle_duration_seconds_sum 800
bitagent_junkpurge_cycle_duration_seconds_count 4
"""


def _stage(body, stage_id):
    return next(s for s in body["stages"] if s["id"] == stage_id)


def _metric(stage, label):
    return next(m for m in stage["metrics"] if m["label"] == label)


def test_ai_summary_scores_every_llm_stage_not_just_the_matcher(client, monkeypatch):
    _patch_metrics(monkeypatch, _AI_METRICS_SAMPLE)
    body = client.get("/api/ai/summary").json()
    assert [s["id"] for s in body["stages"]] == ["matcher", "contentfilter", "junkpurge"]

    matcher = _stage(body, "matcher")
    assert matcher["status"]["key"] == "mixed"
    # Only the junk-purge family carries a model label; claiming one for the
    # others would be a guess, and the matcher has silently swapped models
    # before with no metric changing.
    assert matcher["model"] is None
    assert "no model label" in matcher["modelNote"]
    # Attach yield spans BOTH modes because its denominator does: rerank_total
    # carries no mode label, so a live-only numerator over an all-mode
    # denominator understated the yield whenever a shadow rerank had run.
    # (45 live + 5 shadow) / 50 picks.
    assert _metric(matcher, "Attach yield")["value"] == pytest.approx(1.0)
    # No post-rerank gates in the sample (size/plausibility are pre-LLM), so
    # the rejected-picks proxy must read 0, not pick up the pre-LLM gates.
    assert _metric(matcher, "Rejected picks")["value"] == pytest.approx(0.0)
    assert _metric(matcher, "Extract success")["value"] == pytest.approx(0.8)
    assert _metric(matcher, "Calls")["value"] == 100 + 82
    # Call-weighted, not a mean of means: (120+90)/(60+30) = 2.333…
    assert _metric(matcher, "Avg latency")["value"] == pytest.approx(210 / 90)
    # The core emits no matcher error counter — null with a note, never a zero.
    errors = _metric(matcher, "Call errors")
    assert errors["value"] is None and errors["tone"] == "neutral"
    assert matcher["tokens"] is None

    cf = _stage(body, "contentfilter")
    assert cf["status"]["key"] == "active"
    assert _metric(cf, "Call success")["value"] == pytest.approx(0.95)
    assert _metric(cf, "Avg latency")["value"] == pytest.approx(2.0)
    assert _metric(cf, "Deferred")["value"] == 3
    assert _metric(cf, "Cache hit ratio")["value"] == pytest.approx(0.02)
    # 6 of 6000 drops — a real signal that this model is a rare tie-breaker.
    assert _metric(cf, "Share of all drops")["value"] == pytest.approx(0.001)

    jp = _stage(body, "junkpurge")
    assert jp["status"]["key"] == "active"
    assert jp["model"] == "acme/judge-1"
    assert jp["modelNote"] is None
    assert _metric(jp, "Request success")["value"] == pytest.approx(0.999)
    assert _metric(jp, "Judged junk")["value"] == pytest.approx(250 / 1000)
    assert _metric(jp, "Tokens")["value"] == 52000
    assert jp["tokens"] == {"input": 50000, "output": 2000}
    assert _metric(jp, "Avg cycle")["value"] == pytest.approx(200.0)


_AI_SHADOW_DRY_RUN_SAMPLE = """
bitagent_classifier_llm_match_cache_hits_total 0
bitagent_classifier_llm_match_cache_misses_total 0
bitagent_contentfilter_examined_total 1000
bitagent_contentfilter_keep_total 1000
bitagent_contentfilter_llm_deferred_total 0
bitagent_contentfilter_llm_budget_exhausted_total 0
bitagent_contentfilter_would_drop_total{reason="blocked_extension"} 220
bitagent_contentfilter_would_drop_total{reason="non_latin_script"} 330
bitagent_junkpurge_llm_requests_total{model="google/gemma-3-12b-it",outcome="ok"} 10
bitagent_junkpurge_llm_tokens_total{model="google/gemma-3-12b-it",type="input"} 5000
bitagent_junkpurge_llm_tokens_total{model="google/gemma-3-12b-it",type="output"} 500
bitagent_junkpurge_judged_total{verdict="junk"} 30
bitagent_junkpurge_quarantined_total 0
bitagent_junkpurge_would_delete_total 30
bitagent_junkpurge_cycles_total 1
"""


def test_ai_summary_names_inactive_shadow_and_dry_run_states(client, monkeypatch):
    """This is the live production operating shape: matcher quiet, deterministic
    content filtering in shadow, and junk judging active without quarantine."""
    _patch_metrics(monkeypatch, _AI_SHADOW_DRY_RUN_SAMPLE)
    body = client.get("/api/ai/summary").json()

    assert body["telemetryAvailable"] is True
    assert body["available"] is False  # matcher activity, retained for compatibility

    matcher = _stage(body, "matcher")
    assert matcher["status"]["key"] == "inactive"
    assert "disabled" in matcher["status"]["detail"]

    content_filter = _stage(body, "contentfilter")
    assert content_filter["available"] is False  # no LLM calls
    assert content_filter["status"]["key"] == "shadow"
    assert _metric(content_filter, "Would drop")["value"] == 550
    assert _metric(content_filter, "Share of all drops")["value"] is None

    junk_purge = _stage(body, "junkpurge")
    assert junk_purge["status"]["key"] == "dry_run"
    assert _metric(junk_purge, "Quarantined")["value"] == 0
    assert _metric(junk_purge, "Would delete")["value"] == 30
    assert junk_purge["spend"]["unitLabel"] == "per would-delete"


def test_ai_summary_marks_every_stage_unknown_when_metrics_are_unreachable(client, monkeypatch):
    _patch_metrics(monkeypatch, "")
    body = client.get("/api/ai/summary").json()
    assert body["telemetryAvailable"] is False
    assert {stage["status"]["key"] for stage in body["stages"]} == {"unavailable"}


# An ACTIVE stage whose optional counter families are absent: the stage is
# clearly running, so nothing here can be excused as "idle". Every absent
# family must still read null. Older cores that predate a counter land here.
_AI_ACTIVE_BUT_UNINSTRUMENTED = """
bitagent_contentfilter_llm_calls_total{ok="true"} 100
bitagent_junkpurge_llm_requests_total{model="acme/judge-1",outcome="ok"} 500
bitagent_junkpurge_judged_total{verdict="junk"} 500
"""

# A stage that IS instrumented and legitimately measured zero. This is the
# inverse failure and the more dangerous one: "we quarantined nothing" is
# exactly the fact an operator wants confirmed, and must not read as "no data".
_AI_INSTRUMENTED_REAL_ZEROES = """
bitagent_contentfilter_llm_calls_total{ok="true"} 100
bitagent_contentfilter_llm_deferred_total 0
bitagent_contentfilter_llm_budget_exhausted_total 0
bitagent_junkpurge_llm_requests_total{model="acme/judge-1",outcome="ok"} 500
bitagent_junkpurge_judged_total{verdict="junk"} 500
bitagent_junkpurge_quarantined_total 0
bitagent_junkpurge_expired_total 0
bitagent_junkpurge_llm_tokens_total{model="acme/judge-1",type="input"} 0
bitagent_junkpurge_llm_tokens_total{model="acme/judge-1",type="output"} 0
"""


def test_ai_summary_absent_family_on_an_active_stage_is_null_not_zero(client, monkeypatch):
    """An absent counter must never render as a healthy zero. Both stages here
    are demonstrably running, so 'idle' cannot explain the nulls."""
    _patch_metrics(monkeypatch, _AI_ACTIVE_BUT_UNINSTRUMENTED)
    body = client.get("/api/ai/summary").json()

    cf = _stage(body, "contentfilter")
    jp = _stage(body, "junkpurge")
    assert cf["available"] is True and jp["available"] is True

    for stage, label in ((cf, "Deferred"), (cf, "Budget exhausted"),
                         (jp, "Quarantined"), (jp, "Tokens")):
        m = _metric(stage, label)
        assert m["value"] is None, f"{stage['id']}.{label} fabricated a zero"
        assert m["detail"] == "not instrumented"
        assert m["tone"] == "neutral"
    # And the stage-level token object must not claim a measured zero spend.
    assert jp["tokens"] is None


def test_ai_summary_preserves_a_measured_zero(client, monkeypatch):
    """The inverse: an instrumented counter reading zero is a real finding and
    must survive as 0, not be flattened into 'unavailable'."""
    _patch_metrics(monkeypatch, _AI_INSTRUMENTED_REAL_ZEROES)
    body = client.get("/api/ai/summary").json()

    cf = _stage(body, "contentfilter")
    jp = _stage(body, "junkpurge")
    assert _metric(cf, "Deferred")["value"] == 0
    assert _metric(cf, "Budget exhausted")["value"] == 0
    assert _metric(jp, "Quarantined")["value"] == 0
    assert _metric(jp, "Tokens")["value"] == 0
    assert jp["tokens"] == {"input": 0, "output": 0}
    # A measured zero is good news for these two, and must be styled as such
    # rather than falling back to the null-neutral path.
    assert _metric(cf, "Deferred")["tone"] == "good"
    assert _metric(cf, "Budget exhausted")["tone"] == "good"
    assert _metric(jp, "Quarantined")["detail"] == "0 expired past the review window"


def test_ai_summary_stages_stay_null_not_zero_when_a_stage_is_idle(client, monkeypatch):
    """A stage the core never called must report unavailable, not a
    fabricated 0% — an idle judge and a judge answering everything wrong
    would otherwise render identically."""
    _patch_metrics(monkeypatch, "bitagent_classifier_llm_match_rerank_total{result=\"match\"} 5\n")
    body = client.get("/api/ai/summary").json()
    cf = _stage(body, "contentfilter")
    jp = _stage(body, "junkpurge")
    assert cf["available"] is False and jp["available"] is False
    assert all(m["value"] is None for m in cf["metrics"])
    assert all(m["value"] is None for m in jp["metrics"])
    assert jp["model"] is None
    assert _stage(body, "matcher")["available"] is True


# The realistic partial-family case: the content-filter LLM never ran, but its
# unlabelled optional counters are registered at core boot and scrape as 0
# anyway, and the deterministic rules kept dropping torrents the whole time.
_AI_IDLE_STAGE_WITH_REGISTERED_ZEROES = """
bitagent_classifier_llm_match_rerank_total{result="match"} 5
bitagent_contentfilter_llm_deferred_total 0
bitagent_contentfilter_llm_budget_exhausted_total 0
bitagent_contentfilter_drop_total{reason="non_latin_script"} 60000
bitagent_contentfilter_drop_total{reason="llm_non_english"} 0
"""


def test_ai_summary_idle_stage_never_renders_a_healthy_verdict(client, monkeypatch):
    """An idle stage keeps its measured zeroes but loses its colour. A green
    '0 budget exhausted' beside 'this stage is idle' reads as a healthy model,
    which is exactly what this tab promises not to do."""
    _patch_metrics(monkeypatch, _AI_IDLE_STAGE_WITH_REGISTERED_ZEROES)
    cf = _stage(client.get("/api/ai/summary").json(), "contentfilter")

    assert cf["available"] is False
    # The zeroes are real and stay — flattening them to null would undo the
    # measured-zero contract.
    assert _metric(cf, "Deferred")["value"] == 0
    assert _metric(cf, "Budget exhausted")["value"] == 0
    assert _metric(cf, "Share of all drops")["value"] == pytest.approx(0.0)
    assert _metric(cf, "Share of all drops")["detail"] == "0 of 60,000 enforced drops"
    # ...but nothing on an idle card may claim a verdict.
    assert {m["tone"] for m in cf["metrics"]} == {"neutral"}


def test_ai_summary_deferrals_mean_unreachable_not_idle(client, monkeypatch):
    """A deferral happens instead of a call. Reporting that stage as 'idle or
    disabled' points the operator at the config when the fault is upstream."""
    _patch_metrics(monkeypatch, (
        "bitagent_contentfilter_llm_deferred_total 4\n"
        "bitagent_junkpurge_llm_deferred_total 7\n"
    ))
    body = client.get("/api/ai/summary").json()

    cf = _stage(body, "contentfilter")
    assert cf["available"] is True
    assert _metric(cf, "Deferred")["value"] == 4
    assert _metric(cf, "Deferred")["tone"] == "warn"
    # No calls were made, so nothing derived from calls may fabricate a figure.
    assert _metric(cf, "Call success")["value"] is None
    assert _stage(body, "junkpurge")["available"] is True


def test_system_stats_falls_back_when_the_core_rejects_a_field(client, monkeypatch):
    """GraphQL validates the whole document, so one unknown field would take
    the total, the categories and the core version down with it."""
    assert "totalCountIsEstimate" not in gql.SYSTEM_STATS_LEGACY
    assert "totalCount" in gql.SYSTEM_STATS_LEGACY
    monkeypatch.setattr(app_module, "_stats_query_is_legacy", False)
    seen: list[str] = []

    def responder(q, variables=None):
        if "SystemStats" in q:
            seen.append(q)
            if "totalCountIsEstimate" in q:
                return {"data": None, "errors": [{
                    "message": 'Cannot query field "totalCountIsEstimate" on type '
                               '"TorrentContentSearchResult".',
                    "extensions": {"code": "GRAPHQL_VALIDATION_FAILED"},
                }]}
            return {"data": {
                "version": "kleos-v1.0.0",
                "torrentContent": {"search": {
                    "totalCount": 1234,
                    "aggregations": {"contentType": [{"value": "movie", "count": 1234}]},
                }},
            }}
        if "evidence" in q:
            return {"data": {"evidence": {"list": {"totalCount": 9, "items": []}}}}
        return {"data": {}}

    _patch_gql_query(monkeypatch, responder)
    _patch_metrics(monkeypatch, _METRICS_SAMPLE)

    body = client.get("/api/stats").json()
    assert len(seen) == 2, "expected one rejected query and one fallback"
    # Everything else in the panel survives...
    assert body["totalTorrents"] == 1234
    assert body["version"] == "kleos-v1.0.0"
    assert body["categoryBreakdown"] == [{"category": "movie", "count": 1234}]
    # ...and the one field the core cannot answer reports itself unavailable
    # rather than asserting the count is exact.
    env = body["metrics"]["totalTorrentsIsEstimate"]
    assert env["value"] is None and env["status"] == "unavailable"

    # The fallback latches, so a genuinely old core costs one extra round-trip
    # in total rather than one on every 30s poll.
    assert app_module._stats_query_is_legacy is True


def test_ai_summary_shapes_the_matcher_scorecard(client, monkeypatch):
    _patch_metrics(monkeypatch, _AI_METRICS_SAMPLE)
    r = client.get("/api/ai/summary")
    assert r.status_code == 200
    body = r.json()
    assert body["available"] is True

    # Live/shadow attaches split by media type
    assert body["matches"]["live"]["total"] == 45
    assert body["matches"]["live"]["byType"] == {"movie": 35, "tv": 10}
    assert body["matches"]["shadow"]["total"] == 5
    assert body["matches"]["shadow"]["byType"] == {"movie": 5}

    # Extract outcomes
    assert body["extract"] == {"ok": 80, "empty": 15, "error": 5, "total": 100}

    # Rerank: match rate over decided (match+none), error excluded
    assert body["rerank"]["match"] == 50
    assert body["rerank"]["none"] == 30
    assert body["rerank"]["error"] == 2
    assert body["rerank"]["matchRate"] == pytest.approx(50 / 80)

    # Pre-LLM gates
    assert body["gateRejects"] == {"size": 12, "plausibility": 4}

    # Anime English gate
    assert body["anime"]["kept"] == 7
    assert body["anime"]["rejected"] == 3
    assert body["anime"]["byEnglish"]["dub"] == {"kept": 7, "rejected": 0}
    assert body["anime"]["byEnglish"]["none"] == {"kept": 0, "rejected": 3}

    # Cache
    assert body["cache"] == {"hits": 90, "misses": 10, "hitRatio": pytest.approx(0.9)}

    # Call errors + latency (sum/count -> avg; _bucket lines must not pollute)
    assert body["callErrors"]["total"] == 2
    assert body["callErrors"]["byStage"] == {"extract": 2}
    assert body["latency"]["extract"]["avgSeconds"] == pytest.approx(2.0)
    assert body["latency"]["extract"]["count"] == 60
    assert body["latency"]["rerank"]["avgSeconds"] == pytest.approx(3.0)

    # Alt-title coverage (core v0.22.0 dashstats gauges)
    assert body["altTitles"]["available"] is True
    assert body["altTitles"]["total"] == 1000
    assert body["altTitles"]["checked"] == 250
    assert body["altTitles"]["withAlt"] == 180
    assert body["altTitles"]["checkedRatio"] == pytest.approx(0.25)


def test_ai_summary_reports_unavailable_when_core_lacks_matcher(client, monkeypatch):
    # Core reachable but no llm_match metric family (old/uninstrumented core).
    # A current disabled matcher still emits its boot-registered cache counters
    # and is therefore distinguishable as "no activity observed".
    _patch_metrics(monkeypatch, "bitagent_dht_client_request_concurrency 7\n")
    body = client.get("/api/ai/summary").json()
    assert body["available"] is False
    assert body["telemetryAvailable"] is True
    assert _stage(body, "matcher")["status"]["key"] == "unavailable"
    assert _stage(body, "matcher")["status"]["label"] == "Not instrumented"
    assert body["matches"]["live"]["total"] == 0
    assert body["extract"]["total"] == 0
    assert body["rerank"]["matchRate"] == 0.0
    assert body["cache"]["hitRatio"] == 0.0
    assert body["latency"]["extract"]["count"] == 0
    # No dashstats alt-title gauges either -> coverage marked unavailable
    assert body["altTitles"]["available"] is False
    assert body["altTitles"]["checkedRatio"] == 0.0


# ── Wants CRUD honesty (404 on missing id) ────────────────────────────────
def test_update_want_404_when_missing(client, fresh_db):
    # PUT/DELETE on a non-existent id must 404, not silently report success —
    # the frontend keys its toast on the result.
    assert client.put("/api/wants/999999", json={"status": "paused"}).status_code == 404


def test_delete_want_404_when_missing(client, fresh_db):
    assert client.delete("/api/wants/999999").status_code == 404


# ── Quarantine proxy tolerates a non-JSON core body ───────────────────────
def test_quarantine_proxy_survives_non_json(client, monkeypatch):
    # A wedged core (or proxy) can answer 502 with an HTML page; the proxy must
    # not raise an unhandled JSONDecodeError → 500.
    import httpx

    class _Resp:
        status_code = 502
        text = "<html>502 Bad Gateway</html>"
        def json(self):
            raise ValueError("not json")

    class _Client:
        def __init__(self, *a, **k): pass
        async def __aenter__(self): return self
        async def __aexit__(self, *a): return False
        async def get(self, *a, **k): return _Resp()

    monkeypatch.setattr(httpx, "AsyncClient", _Client)
    r = client.get("/api/quarantine")
    assert r.status_code == 502
    assert r.json()["error"]


# ── /api/torrents grouped pagination (core groupByContent, v1.29.0) ────────

def _grouped_block(items, *, total, estimate, has_next):
    return {"data": {"torrentContent": {"search": {
        "totalCount": total,
        "totalCountIsEstimate": estimate,
        "hasNextPage": has_next,
        "items": items,
        "aggregations": {},
    }}}}


def test_api_torrents_group_passes_grouped_input_and_pages(client, monkeypatch, fresh_db):
    captured = {}

    def responder(q, v=None):
        captured.update((v or {}).get("input") or {})
        item = {"infoHash": "h1", "title": "One Title", "contentSource": "tmdb",
                "contentId": "tt1", "contentType": "movie", "seeders": 9,
                "torrent": {"name": "One.Title.2024"}}
        return _grouped_block([item], total=143, estimate=True, has_next=True)

    _patch_gql_query(monkeypatch, responder)
    r = client.get("/api/torrents", params={
        "q": "batman", "group": "true", "limit": 60, "offset": 120, "order": "seeders",
    })
    assert r.status_code == 200
    # Grouped opt-ins reach the core, with the caller's real limit/offset
    # (no over-fetch) and the server-side order.
    assert captured["groupByContent"] is True
    assert captured["hasNextPage"] is True
    assert captured["totalCount"] is True
    assert captured["limit"] == 60
    assert captured["offset"] == 120
    assert captured["orderBy"] == [{"field": "seeders", "descending": True}]
    body = r.json()
    assert body["hasNextPage"] is True
    assert body["totalCountIsEstimate"] is True
    assert body["totalCount"] == 143
    assert body["items"][0]["name"] == "One Title"


def test_api_torrents_group_false_keeps_legacy_shape(client, monkeypatch, fresh_db):
    captured = {}

    def responder(q, v=None):
        captured.update((v or {}).get("input") or {})
        return _search_block([], total=0)

    _patch_gql_query(monkeypatch, responder)
    r = client.get("/api/torrents", params={"q": "x"})
    assert r.status_code == 200
    # Ungrouped requests never send the grouped opt-ins.
    assert "groupByContent" not in captured
    assert "hasNextPage" not in captured
    assert "hasNextPage" not in r.json()


def test_api_torrents_group_new_orders_map(client, monkeypatch, fresh_db):
    captured = {}

    def responder(q, v=None):
        captured.update((v or {}).get("input") or {})
        return _grouped_block([], total=0, estimate=False, has_next=False)

    _patch_gql_query(monkeypatch, responder)
    for order, expect in [
        ("size", [{"field": "size", "descending": True}]),
        ("name", [{"field": "name", "descending": False}]),
    ]:
        captured.clear()
        r = client.get("/api/torrents", params={"group": "true", "order": order, "q": order})
        assert r.status_code == 200
        assert captured["orderBy"] == expect


# ── LLM spend ─────────────────────────────────────────────────────────────
#
# Money is the one number on this page that must never be guessed. These cover
# the pricing arithmetic, the two stages' disagreeing token label names, and
# the failure that matters most: an unpriced model must never read as free.

_SPEND_METRICS = """
bitagent_contentfilter_llm_calls_total{ok="true"} 5665
bitagent_contentfilter_llm_calls_total{ok="false"} 63
bitagent_contentfilter_drop_total{reason="llm_non_english"} 170
bitagent_contentfilter_drop_total{reason="non_latin_script"} 262605
bitagent_contentfilter_llm_tokens_total{kind="prompt",model="meta-llama/llama-3.3-70b-instruct"} 2284642
bitagent_contentfilter_llm_tokens_total{kind="completion",model="meta-llama/llama-3.3-70b-instruct"} 134337
bitagent_junkpurge_llm_requests_total{model="google/gemma-3-12b-it",outcome="ok"} 10554
bitagent_junkpurge_llm_tokens_total{model="google/gemma-3-12b-it",processing="standard",type="input"} 6336052
bitagent_junkpurge_llm_tokens_total{model="google/gemma-3-12b-it",processing="standard",type="output"} 175075
bitagent_junkpurge_judged_total{verdict="junk"} 4367
bitagent_junkpurge_quarantined_total 4334
"""


def test_llm_spend_prices_both_stages_despite_different_token_labels():
    """The content filter labels kind=prompt|completion and junk purge labels
    type=input|output. A single mapping would silently price one at zero."""
    from prom_metrics import _parse_prometheus_snapshot, _llm_spend
    spend = _llm_spend(_parse_prometheus_snapshot(_SPEND_METRICS)["lines"])

    assert spend["available"] is True
    assert spend["totalIsPartial"] is False
    by_model = {r["model"]: r for r in spend["byModel"]}

    cf = by_model["meta-llama/llama-3.3-70b-instruct"]
    assert cf["stage"] == "contentfilter"
    assert (cf["inputTokens"], cf["outputTokens"]) == (2284642, 134337)
    # 2.284642M x $0.10 + 0.134337M x $0.32
    assert cf["usd"] == pytest.approx(0.2714520, abs=1e-6)

    jp = by_model["google/gemma-3-12b-it"]
    assert jp["stage"] == "junkpurge"
    assert (jp["inputTokens"], jp["outputTokens"]) == (6336052, 175075)
    assert jp["usd"] == pytest.approx(0.3430639, abs=1e-6)

    assert spend["totalUsd"] == pytest.approx(0.6145159, abs=1e-6)
    # Most expensive first — the panel ranks by spend.
    assert spend["byModel"][0]["model"] == "google/gemma-3-12b-it"


def test_llm_spend_reports_an_unpriced_model_as_a_floor_not_as_free():
    """The failure that actually costs money: a changed model pin. Tokens must
    still be counted, dollars must be null, and the total must be marked
    partial so the UI says "at least" rather than presenting a low bill."""
    from prom_metrics import _parse_prometheus_snapshot, _llm_spend
    text = _SPEND_METRICS.replace("google/gemma-3-12b-it", "acme/brand-new-model")
    spend = _llm_spend(_parse_prometheus_snapshot(text)["lines"])

    unknown = next(r for r in spend["byModel"] if r["model"] == "acme/brand-new-model")
    assert unknown["priced"] is False
    assert unknown["usd"] is None
    assert unknown["inputTokens"] == 6336052  # tokens still counted
    assert spend["totalIsPartial"] is True
    assert spend["unpricedModels"] == ["acme/brand-new-model"]
    # Only the priced stage contributes — the total is a floor, never a claim.
    assert spend["totalUsd"] == pytest.approx(0.2714520, abs=1e-6)


def test_matcher_spend_includes_nano_and_discounts_cached_input():
    from prom_metrics import _parse_prometheus_snapshot, _llm_spend
    text = '''
bitagent_classifier_llm_match_tokens_total{model="gpt-5.4-nano",stage="extract",kind="input"} 1000000
bitagent_classifier_llm_match_tokens_total{model="gpt-5.4-nano",stage="extract",kind="cached_input"} 500000
bitagent_classifier_llm_match_tokens_total{model="gpt-5.4-nano",stage="extract",kind="output"} 20000
bitagent_classifier_llm_match_tokens_total{model="gpt-5.4-nano",stage="extract",kind="reasoning"} 10000
bitagent_classifier_llm_match_tokens_total{model="gpt-5.4-nano",stage="rerank",kind="input"} 100000
bitagent_classifier_llm_match_tokens_total{model="gpt-5.4-nano",stage="rerank",kind="output"} 10000
'''
    spend = _llm_spend(_parse_prometheus_snapshot(text)["lines"])
    assert len(spend["byModel"]) == 1
    row = spend["byModel"][0]
    assert row["stage"] == "matcher"
    assert row["inputTokens"] == 1100000
    assert row["outputTokens"] == 30000
    assert row["usd"] == pytest.approx(0.1675)
    assert not spend["totalIsPartial"]
    partial = text + 'bitagent_classifier_llm_match_usage_missing_total{model="gpt-5.4-nano",stage="extract"} 1\n'
    spend = _llm_spend(_parse_prometheus_snapshot(partial)["lines"])
    assert spend["totalIsPartial"]
    assert spend["byModel"][0]["missingUsageResponses"] == 1

    routed = _llm_spend(_parse_prometheus_snapshot(text.replace("gpt-5.4-nano", "openai/gpt-5.4-nano-20260317"))["lines"])
    assert routed["totalUsd"] == pytest.approx(0.18425)


@pytest.mark.parametrize("enabled,live,label", [(0, 0, "Disabled"), (1, 0, "Shadow configured"), (1, 1, "Live configured")])
def test_matcher_config_is_visible_before_any_call(client, monkeypatch, enabled, live, label):
    text = f'''
bitagent_classifier_llm_match_config{{setting="enabled"}} {enabled}
bitagent_classifier_llm_match_config{{setting="live"}} {live}
bitagent_classifier_llm_match_config{{setting="require_source_title"}} 1
bitagent_classifier_llm_match_config{{setting="daily_call_limit"}} 100
bitagent_classifier_llm_match_config{{setting="monthly_call_limit"}} 3000
bitagent_classifier_llm_match_info{{model="gpt-5.4-nano",prompt_version="v4"}} 1
'''
    _patch_metrics(monkeypatch, text)
    body = client.get("/api/ai/summary").json()
    matcher = _stage(body, "matcher")
    assert matcher["status"]["label"] == label
    assert matcher["model"] == "gpt-5.4-nano"
    assert matcher["available"] is False
    assert matcher["tokens"] is None
    assert matcher["spend"] is None
    assert matcher["config"]["daily_call_limit"] == 100


def test_matcher_counts_outbound_calls_and_missing_usage(client, monkeypatch):
    text = _AI_METRICS_SAMPLE + '''
bitagent_classifier_llm_match_calls_total{model="gpt-5.4-nano",stage="extract"} 10
bitagent_classifier_llm_match_calls_total{model="gpt-5.4-nano",stage="rerank"} 5
bitagent_classifier_llm_match_tokens_total{model="gpt-5.4-nano",stage="extract",kind="input"} 1000
bitagent_classifier_llm_match_tokens_total{model="gpt-5.4-nano",stage="extract",kind="output"} 100
bitagent_classifier_llm_match_usage_missing_total{model="gpt-5.4-nano",stage="extract"} 1
bitagent_classifier_llm_match_budget_skips_total{reason="exhausted"} 2
'''
    _patch_metrics(monkeypatch, text)
    body = client.get("/api/ai/summary").json()
    matcher = _stage(body, "matcher")
    assert _metric(matcher, "Calls")["value"] == 15
    assert _metric(matcher, "Call errors")["value"] == pytest.approx(2/15)
    assert _metric(matcher, "Budget skips")["value"] == 2
    assert matcher["tokens"] == {"input": 1000, "output": 100}
    assert "partial" in matcher["tokensNote"]
    assert body["spend"]["totalIsPartial"]


def test_llm_spend_excludes_cached_and_reasoning_token_subsets():
    """junk purge's help text warns cached_input is a SUBSET of input and
    reasoning a subset of output. Summing them would inflate the bill."""
    from prom_metrics import _parse_prometheus_snapshot, _llm_spend
    text = _SPEND_METRICS + (
        'bitagent_junkpurge_llm_tokens_total{model="google/gemma-3-12b-it",'
        'processing="standard",type="cached_input"} 5000000\n'
        'bitagent_junkpurge_llm_tokens_total{model="google/gemma-3-12b-it",'
        'processing="standard",type="reasoning"} 90000\n'
    )
    spend = _llm_spend(_parse_prometheus_snapshot(text)["lines"])
    jp = next(r for r in spend["byModel"] if r["stage"] == "junkpurge")
    assert (jp["inputTokens"], jp["outputTokens"]) == (6336052, 175075)


def test_ai_summary_surfaces_spend_and_cost_per_unit_of_work(client, monkeypatch):
    _patch_metrics(monkeypatch, _SPEND_METRICS)
    body = client.get("/api/ai/summary").json()

    assert body["spend"]["totalUsd"] == pytest.approx(0.6145159, abs=1e-6)
    # No second sample yet, so a monthly figure cannot be measured. It must say
    # so rather than report 0 — the core publishes no process-start gauge, so
    # one scrape genuinely cannot know the window the counters cover.
    assert body["spend"]["monthlyUsd"] is None
    assert body["spend"]["monthlyStatus"] == "measuring"

    cf = _stage(body, "contentfilter")
    # Model label and tokens now ride the core's own counter; both were
    # previously reported as instrumentation gaps.
    assert cf["model"] == "meta-llama/llama-3.3-70b-instruct"
    assert cf["tokens"] == {"input": 2284642, "output": 134337}
    # $0.2715 over 170 drops — the comparison that ranks stages against each
    # other, and the one a raw token count cannot make.
    assert cf["spend"]["usdPerUnit"] == pytest.approx(0.2714520 / 170, rel=1e-4)
    assert cf["spend"]["unitLabel"] == "per drop"

    jp = _stage(body, "junkpurge")
    assert jp["spend"]["usdPerUnit"] == pytest.approx(0.3430639 / 4334, rel=1e-4)
    assert jp["spend"]["unitLabel"] == "per quarantine"
    # The matcher reports no tokens, so it gets no spend block at all — an
    # empty one would read as "this model is free".
    assert _stage(body, "matcher")["spend"] is None


def test_ai_summary_unavailable_when_only_boot_registered_counters_are_present(
    client, monkeypatch
):
    """The bug this replaces: `available` was `bool(fam)`, and the matcher's
    cache counters are registered at core boot with prometheus.NewCounter, so
    they scrape as a bare 0 with the matcher switched OFF. The family was never
    empty, so the "matcher metrics not present" banner was unreachable in
    exactly the situation it exists for — the page showed a wall of zeroes and
    called itself healthy. This is the live production state today
    (CLASSIFIER_LLM_MATCH_ENABLED=false)."""
    _patch_metrics(monkeypatch, (
        "bitagent_classifier_llm_match_cache_hits_total 0\n"
        "bitagent_classifier_llm_match_cache_misses_total 0\n"
    ))
    body = client.get("/api/ai/summary").json()
    assert body["available"] is False

    # And it flips back on the first real matcher work, not on a registration.
    _patch_metrics(monkeypatch, (
        "bitagent_classifier_llm_match_cache_hits_total 0\n"
        "bitagent_classifier_llm_match_cache_misses_total 0\n"
        'bitagent_classifier_llm_match_extract_total{result="ok"} 1\n'
    ))
    assert client.get("/api/ai/summary").json()["available"] is True


# ── Wantbridge & evidence sources ─────────────────────────────────────────

_WANTBRIDGE_METRICS = """
bitagent_wantbridge_wantlist_size{source="sonarr"} 2426
bitagent_wantbridge_wantlist_size{source="radarr"} 58
bitagent_wantbridge_matches_total{source="sonarr",tier="tier0"} 944
bitagent_wantbridge_matches_total{source="radarr",tier="tier0"} 11
bitagent_wantbridge_matches_total{source="",tier="tier1"} 145191
bitagent_wantbridge_fingerprint_keys 2894
bitagent_wantbridge_arr_poll_duration_seconds_sum{source="sonarr"} 594.78
bitagent_wantbridge_arr_poll_duration_seconds_count{source="sonarr"} 264
"""


def test_wantbridge_reports_what_it_measured_not_what_it_inferred(client, monkeypatch):
    """Tier 2 only occurs once matches change crawl priority, so its absence is
    CONSISTENT with WANTBRIDGE_ENFORCE=false — but these are since-boot
    counters and a core that restarted recently with enforcement on and nothing
    yet qualifying is indistinguishable. The field is therefore named for the
    observation (`tier2Observed`), not the conclusion: publishing the inference
    as a fact would send an operator to change a correct setting."""
    _patch_metrics(monkeypatch, _WANTBRIDGE_METRICS)
    body = client.get("/api/wantbridge").json()

    assert body["available"] is True
    assert body["tier2Observed"] is False
    assert "enforcing" not in body
    assert body["wantlistTotal"] == 2484
    assert body["matchesByTier"] == {"tier0": 955, "tier1": 145191}
    assert body["matchesBySource"] == {"sonarr": 944, "radarr": 11}
    assert body["polls"]["sonarr"]["cycles"] == 264
    assert body["polls"]["sonarr"]["avgSeconds"] == pytest.approx(594.78 / 264)

    _patch_metrics(monkeypatch, _WANTBRIDGE_METRICS
                   + 'bitagent_wantbridge_matches_total{source="",tier="tier2"} 7\n')
    assert client.get("/api/wantbridge").json()["tier2Observed"] is True


def test_wantbridge_unavailable_when_the_core_emits_nothing(client, monkeypatch):
    _patch_metrics(monkeypatch, "bitagent_unrelated_total 1\n")
    assert client.get("/api/wantbridge").json() == {"available": False}


def test_evidence_sources_counts_webhook_events_rather_than_judging_config(client, monkeypatch):
    """The question the event list cannot answer: ~99% of rows are qBittorrent
    polls and the core's evidence API takes only limit/offset, so no amount of
    paging finds an *arr that is not delivering webhooks.

    But the counters reset with the core, so zero webhook events proves only
    that none arrived since boot — a correctly wired *arr that has not grabbed
    anything reads the same as one with no webhook. The field is a COUNT, not a
    boolean verdict, so the UI can report the observation rather than tell an
    operator their config is broken on this evidence."""
    _patch_metrics(monkeypatch, """
bitagent_evidence_events_received_total{kind="qb_state_observation",source="qbittorrent"} 3706
bitagent_evidence_events_persisted_total{kind="qb_state_observation",source="qbittorrent"} 3632
bitagent_evidence_events_received_total{kind="poll_history",source="sonarr"} 18200
bitagent_evidence_events_persisted_total{kind="poll_history",source="sonarr"} 98
bitagent_evidence_events_duplicated_total{kind="poll_history",source="sonarr"} 17502
bitagent_evidence_events_received_total{kind="webhook_grab",source="sonarr"} 25
bitagent_evidence_events_received_total{kind="poll_history",source="radarr"} 18200
""")
    body = client.get("/api/evidence/sources").json()
    assert body["available"] is True
    by_source = {s["source"]: s for s in body["sources"]}

    assert by_source["sonarr"]["webhookEvents"] == 25
    # radarr polls but has delivered no webhook_* event since boot — worth
    # checking, not a verdict.
    assert by_source["radarr"]["webhookEvents"] == 0
    assert by_source["qbittorrent"]["webhookEvents"] == 0  # polled by design

    assert by_source["sonarr"]["received"] == 18225
    assert by_source["sonarr"]["persisted"] == 98
    assert by_source["sonarr"]["duplicated"] == 17502
    # Kinds ordered by volume so the dominant one reads first.
    assert [k["kind"] for k in by_source["sonarr"]["kinds"]] == ["poll_history", "webhook_grab"]
    # Sources ordered by volume, ties broken by name.
    assert [s["source"] for s in body["sources"]][0] == "sonarr"


def test_evidence_sources_unavailable_when_the_core_emits_nothing(client, monkeypatch):
    _patch_metrics(monkeypatch, "bitagent_unrelated_total 1\n")
    assert client.get("/api/evidence/sources").json() == {"available": False, "sources": []}


def test_spend_projection_never_prices_an_unmeasured_bursty_stage_at_zero(
    client, monkeypatch
):
    """The failure this guards is the one that matters: junk purge judges in
    cycles roughly an hour apart, so across a short sampling window its token
    counter does not move and its rate measures a clean, confident 0.00/mo.
    Summed naively that produced "$8.57/mo against a $10 budget" while the
    real combined run rate was ~$20/mo — a card that says "under budget" when
    spend is twice it is worse than no card. An unmeasured model must make the
    total a FLOOR, not vanish from it."""
    import app as app_module

    app_module._SPEND_HISTORY.clear()
    _patch_metrics(monkeypatch, _SPEND_METRICS)
    assert client.get("/api/ai/summary").json()["spend"]["monthlyStatus"] == "measuring"

    # Second sample 60s later: the content filter moved, junk purge did not —
    # exactly what a bursty stage looks like between cycles.
    moved = _SPEND_METRICS.replace(
        'bitagent_contentfilter_llm_tokens_total{kind="prompt",'
        'model="meta-llama/llama-3.3-70b-instruct"} 2284642',
        'bitagent_contentfilter_llm_tokens_total{kind="prompt",'
        'model="meta-llama/llama-3.3-70b-instruct"} 2294642',
    )
    _patch_metrics(monkeypatch, moved)
    base = app_module._SPEND_HISTORY[0][0]
    monkeypatch.setattr(app_module, "_metric_history_now", lambda: base + 60.0)

    spend = client.get("/api/ai/summary").json()["spend"]
    assert spend["monthlyStatus"] == "partial"
    assert spend["monthlyIsPartial"] is True
    # Named, so the UI can say WHAT is still being measured.
    assert spend["monthlyMeasuring"] == ["google/gemma-3-12b-it"]
    # 10,000 prompt tokens in 60s at $0.10/M = $0.001/min -> $43.20/mo.
    assert spend["monthlyUsd"] == pytest.approx(43.2, rel=1e-3)
    by_model = {r["model"]: r for r in spend["byModel"]}
    assert by_model["google/gemma-3-12b-it"]["monthlyUsd"] is None


def test_spend_projection_trusts_a_flat_counter_once_the_window_is_long_enough(
    client, monkeypatch
):
    """The inverse error: a stage that really is switched off must eventually
    read $0/mo rather than 'measuring…' forever."""
    import app as app_module

    app_module._SPEND_HISTORY.clear()
    _patch_metrics(monkeypatch, _SPEND_METRICS)
    client.get("/api/ai/summary")
    base = app_module._SPEND_HISTORY[0][0]
    monkeypatch.setattr(
        app_module, "_metric_history_now",
        lambda: base + app_module._SPEND_IDLE_TRUST_SECONDS + 1,
    )
    spend = client.get("/api/ai/summary").json()["spend"]
    assert spend["monthlyStatus"] == "ok"
    assert spend["monthlyIsPartial"] is False
    assert spend["monthlyUsd"] == pytest.approx(0.0)


def test_spend_history_window_outlives_the_idle_trust_threshold():
    """These two constants must stay ordered: if the history window is shorter
    than the idle-trust threshold, the retained baseline is pruned before the
    threshold is ever reached and an idle model reads 'measuring…' forever."""
    import app as app_module
    assert app_module._SPEND_HISTORY_WINDOW_SECONDS > app_module._SPEND_IDLE_TRUST_SECONDS


def test_llm_spend_marks_unlabelled_tokens_as_unpriced_too():
    """A token series with no `model` label is unpriceable for a different
    reason than an unrecognised model, but it fails the same way: real tokens,
    no dollars. Dropping it from the unpriced list because the label is empty
    let the priced subtotal render as the complete bill."""
    from prom_metrics import _parse_prometheus_snapshot, _llm_spend
    spend = _llm_spend(_parse_prometheus_snapshot(
        'bitagent_junkpurge_llm_tokens_total{processing="standard",type="input"} 500000\n'
        'bitagent_contentfilter_llm_tokens_total{kind="prompt",'
        'model="meta-llama/llama-3.3-70b-instruct"} 1000000\n'
    )["lines"])

    assert spend["totalIsPartial"] is True
    assert spend["unpricedModels"] == ["unlabelled"]
    # Only the priced series contributes; the total is a floor, not the bill.
    assert spend["totalUsd"] == pytest.approx(0.10, abs=1e-6)
    unlabelled = next(r for r in spend["byModel"] if r["model"] == "")
    assert unlabelled["priced"] is False and unlabelled["inputTokens"] == 500000


def test_operator_tab_panels_are_siblings_not_nested():
    """Guards the failure mode of editing this template with string surgery:
    an unbalanced <div> silently nests one tab panel inside another, and the
    nested one becomes unreachable because its inactive ancestor hides it.
    Tag counts can balance while the nesting is wrong, so this walks depth."""
    import pathlib
    import re

    html = (pathlib.Path(__file__).resolve().parent.parent
            / "templates" / "index.html").read_text()
    depth, depths = 0, {}
    for m in re.finditer(r"<div\b[^>]*>|</div>|<!--.*?-->", html, re.S):
        token = m.group(0)
        if token.startswith("<!--"):
            continue
        if token.startswith("</"):
            depth -= 1
            continue
        found = re.search(r'id="(tab-[a-z]+)"', token)
        if found:
            depths[found.group(1)] = depth
        depth += 1

    assert depth == 0, "unbalanced <div> in index.html"
    assert len(depths) >= 8, f"expected every tab panel, found {sorted(depths)}"
    assert len(set(depths.values())) == 1, f"tab panels at differing depths: {depths}"

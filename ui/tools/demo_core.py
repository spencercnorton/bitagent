#!/usr/bin/env python3
"""A stand-in BitAgent core: run the console with no crawler.

Serves the GraphQL documents and the Prometheus families the console reads,
from synthetic data (public-domain films, invented release groups, plausible
counters that move a little on every scrape). Standard library only.

    python tools/demo_core.py                # http://127.0.0.1:3333
    REQUIRE_AUTH=false OPERATOR_HOSTS=localhost,127.0.0.1 \\
      PUBLIC_LIBRARY_HOSTS=library.localhost python app.py

This is a demonstration state for screenshots and UI work, not a mock of the
core's contract: field shapes follow what the console asks for today.
"""
from __future__ import annotations

import hashlib
import json
import random
import time
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from urllib.parse import urlsplit

START = time.time()
RNG = random.Random(7)

# Public-domain features (title, year, type, genre) — nothing here is a
# current commercial release, so a poster grid looks real and stays clean.
FILMS = [
    ("Metropolis", 1927, "movie", "Science Fiction"), ("Nosferatu", 1922, "movie", "Horror"),
    ("The General", 1926, "movie", "Comedy"), ("Sherlock Jr.", 1924, "movie", "Comedy"),
    ("The Cabinet of Dr. Caligari", 1920, "movie", "Horror"), ("His Girl Friday", 1940, "movie", "Comedy"),
    ("Night of the Living Dead", 1968, "movie", "Horror"), ("Charade", 1963, "movie", "Thriller"),
    ("The Phantom of the Opera", 1925, "movie", "Horror"), ("Safety Last!", 1923, "movie", "Comedy"),
    ("Battleship Potemkin", 1925, "movie", "Drama"), ("A Trip to the Moon", 1902, "movie", "Science Fiction"),
    ("The Kid", 1921, "movie", "Comedy"), ("Häxan", 1922, "movie", "Documentary"),
    ("The Lost World", 1925, "movie", "Adventure"), ("Carnival of Souls", 1962, "movie", "Horror"),
    ("The Last Man on Earth", 1964, "movie", "Science Fiction"), ("Plan 9 from Outer Space", 1957, "movie", "Science Fiction"),
    ("Flash Gordon", 1936, "tv_show", "Adventure"), ("The Adventures of Sherlock Holmes", 1954, "tv_show", "Mystery"),
    ("Dragnet", 1951, "tv_show", "Crime"), ("The Beverly Hillbillies", 1962, "tv_show", "Comedy"),
    ("Sea Hunt", 1958, "tv_show", "Adventure"), ("The Lucy Show", 1962, "tv_show", "Comedy"),
]
GROUPS = ["ARCHIVE", "NITRATE", "REEL", "KINO", "SILENT", "TELECINE"]
RES = ["1080p", "720p", "2160p", "480p"]
SRC = ["BluRay", "WEB-DL", "DVDRip", "HDTV"]


def _iso(ts: float) -> str:
    return time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime(ts))


def _hash(seed: str) -> str:
    return hashlib.sha1(seed.encode()).hexdigest()[:40]


def _catalog() -> list[dict]:
    items = []
    for i, (title, year, kind, genre) in enumerate(FILMS):
        for n in range(3):
            res, src, group = RES[(i + n) % 4], SRC[(i * 2 + n) % 4], GROUPS[(i + n) % len(GROUPS)]
            season = f".S0{1 + n}" if kind == "tv_show" else ""
            name = f"{title.replace(' ', '.')}.{year}{season}.{res}.{src}.x264-{group}"
            created = START - 86400 * (i * 3 + n) - 3600 * n
            items.append({
                "infoHash": _hash(name), "title": title, "contentType": kind, "contentSource": "tmdb",
                "contentId": str(10000 + i), "seeders": 4 + (i * 7 + n * 13) % 90, "leechers": (i + n) % 12,
                "createdAt": _iso(created), "updatedAt": _iso(created + 7200), "languages": [{"id": "en"}],
                "videoResolution": f"V{res}", "videoSource": src, "videoCodec": "x264", "releaseGroup": group,
                "episodes": {"label": f"S0{1 + n}", "seasons": [{"season": 1 + n, "episodes": list(range(1, 9))}]} if kind == "tv_show" else None,
                "content": {"title": title, "releaseYear": year, "originalTitle": title,
                            "originalLanguage": {"id": "en", "name": "English"},
                            "externalLinks": [{"metadataSource": {"key": "tmdb"}, "url": f"https://www.themoviedb.org/{kind.replace('_show', '')}/{10000 + i}"}]},
                "torrent": {"name": name, "size": 700_000_000 + (i * 91 + n * 37) % 40 * 100_000_000,
                            "filesCount": 1 + n, "tagNames": [],
                            "files": [{"path": f"{name}.mkv", "size": 700_000_000}]},
                "_genre": genre,
            })
    return items


CATALOG = _catalog()
CONTENT_TYPES = [("movie", 1_022_170), ("tv_show", 980_768), ("music", 614_385), ("ebook", 394_801), ("audiobook", 54_692)]


def _agg(values: list[tuple[str, int]]) -> list[dict]:
    return [{"value": v, "count": c} for v, c in values]


def search(inp: dict) -> dict:
    items = list(CATALOG)
    hashes = inp.get("infoHashes")
    if hashes:
        items = [i for i in items if i["infoHash"] in hashes]
    q = (inp.get("queryString") or "").lower().strip()
    if q:
        items = [i for i in items if q in i["torrent"]["name"].lower() or q in i["title"].lower()]
    kinds = (inp.get("facets") or {}).get("contentType", {}).get("filter") or inp.get("contentType")
    if kinds:
        items = [i for i in items if i["contentType"] in kinds]
    total = sum(c for _, c in CONTENT_TYPES) if not (q or hashes or kinds) else len(items)
    limit, offset = int(inp.get("limit") or 20), int(inp.get("offset") or 0)
    page = items[offset:offset + limit]
    genres: dict[str, int] = {}
    for i in items:
        genres[i["_genre"]] = genres.get(i["_genre"], 0) + 1
    return {
        "totalCount": total, "totalCountIsEstimate": not (q or hashes or kinds), "hasNextPage": offset + limit < len(items),
        "aggregations": {
            "contentType": _agg(CONTENT_TYPES if not kinds else [(k, len(items)) for k in kinds]),
            "genre": _agg(sorted(genres.items(), key=lambda kv: -kv[1])),
            "releaseYear": _agg(sorted({(str(i["content"]["releaseYear"]), 3) for i in items})),
            "videoResolution": _agg([(f"V{r}", 18) for r in RES]),
            "videoSource": _agg([(s, 18) for s in SRC]),
        },
        "items": [{k: v for k, v in i.items() if not k.startswith("_")} for i in page],
    }


def graphql(doc: str, variables: dict) -> dict:
    tick = int(time.time() - START)
    if "query SystemStats" in doc:
        return {"data": {"version": "v2.7.3", "torrentContent": {"search": search({"queryString": "", "limit": 1})}}}
    if "query LibraryStats" in doc:
        return {"data": {"torrentContent": {"total": {"totalCount": 2_002_938, "totalCountIsEstimate": False},
                                            "recent": {"totalCount": 41_207, "totalCountIsEstimate": False}}}}
    if "query EvidenceList" in doc:
        inp = variables.get("input") or {}
        limit, offset = int(inp.get("limit") or 50), int(inp.get("offset") or 0)
        rows = []
        for n in range(offset, offset + limit):
            item = CATALOG[(n * 7) % len(CATALOG)]
            kind = ["webhook_grab", "webhook_import", "qbt_category", "arr_history"][n % 4]
            rows.append({"id": str(90_000 - n), "source": ["sonarr", "radarr", "qbittorrent", "lidarr"][n % 4], "kind": kind,
                         "infoHash": item["infoHash"], "title": item["torrent"]["name"], "mediaType": item["contentType"],
                         "observedAt": _iso(time.time() - 300 * n - tick)})
        return {"data": {"evidence": {"list": {"totalCount": 18_442, "items": rows}}}}
    if "query IndexerStats" in doc:
        days = int((variables.get("input") or {}).get("days") or 30)
        series = [{"date": time.strftime("%Y-%m-%d", time.gmtime(time.time() - 86400 * d)),
                   "totalGrabs": 18 + (d * 5) % 9, "bitagentGrabs": 9 + (d * 3) % 7} for d in range(days - 1, -1, -1)]
        total, ours = sum(s["totalGrabs"] for s in series), sum(s["bitagentGrabs"] for s in series)
        return {"data": {"evidence": {"indexerStats": {
            "totalGrabs": total, "bitagentGrabs": ours, "bitagentWinRate": round(ours / total, 4),
            "indexers": [{"indexer": "BitAgent", "grabs": ours}, {"indexer": "Public tracker A", "grabs": (total - ours) * 2 // 3},
                         {"indexer": "Public tracker B", "grabs": (total - ours) - (total - ours) * 2 // 3}],
            "days": series}}}}
    # Search, TorrentDetail, RecentMatches all read torrentContent.search
    inp = variables.get("input") or {}
    if "TorrentDetail" in doc:
        inp = {"infoHashes": [variables.get("infoHash")], "limit": 1}
    return {"data": {"torrentContent": {"search": search(inp)}}}


def metrics() -> str:
    t = int(time.time() - START)
    m = "gpt-5.4-nano"
    lines = [
        f"bitagent_dashstats_indexer_grabs_bitagent_30d {331}", f"bitagent_dashstats_indexer_grabs_total_30d {651}",
        f"bitagent_dashstats_grab_success {294 + t // 600}", "bitagent_dashstats_grab_failure 41", "bitagent_dashstats_grab_pending 6",
        "bitagent_dashstats_match_video_total_30d 1146826", "bitagent_dashstats_match_video_matched_30d 903841",
        "bitagent_dashstats_alt_title_content_total 191163", "bitagent_dashstats_alt_title_content_checked 173340",
        "bitagent_dashstats_alt_title_content_with_alt 143049",
        f"bitagent_contentfilter_examined_total {2_308_575 + t * 11}", f"bitagent_contentfilter_keep_total {2_251_910 + t * 11}",
        'bitagent_contentfilter_drop_total{reason="blocked_extension"} 26096', 'bitagent_contentfilter_drop_total{reason="lossy_audio_only"} 18211',
        'bitagent_contentfilter_drop_total{reason="non_latin_script"} 9917', 'bitagent_contentfilter_drop_total{reason="llm_non_english"} 2441',
        'bitagent_contentfilter_would_drop_total{reason="nsfw"} 1290',
        'bitagent_contentfilter_blocked_ext_total{ext="exe"} 14022', 'bitagent_contentfilter_blocked_ext_total{ext="apk"} 8071',
        'bitagent_contentfilter_blocked_ext_total{ext="deb"} 4003',
        f'bitagent_contentfilter_llm_calls_total{{ok="true"}} {1815 + t // 120}', "bitagent_contentfilter_llm_budget_exhausted_total 0",
        "bitagent_contentfilter_llm_deferred_total 0", "bitagent_contentfilter_llm_cache_hits_total 6120", "bitagent_contentfilter_llm_cache_misses_total 1815",
        f"bitagent_contentfilter_llm_call_duration_seconds_sum {1815 * 1.7:.1f}", "bitagent_contentfilter_llm_call_duration_seconds_count 1815",
        f'bitagent_contentfilter_llm_tokens_total{{kind="prompt",model="{m}"}} {688_400 + t * 9}', f'bitagent_contentfilter_llm_tokens_total{{kind="completion",model="{m}"}} {41_300 + t}',
        f'bitagent_classifier_llm_match_info{{model="{m}",prompt_version="v4"}} 1',
        'bitagent_classifier_llm_match_config{setting="enabled"} 1', 'bitagent_classifier_llm_match_config{setting="live"} 1',
        'bitagent_classifier_llm_match_config{setting="daily_call_limit"} 400', 'bitagent_classifier_llm_match_config{setting="min_confidence"} 0.75',
        f'bitagent_classifier_llm_match_calls_total{{model="{m}",stage="extract"}} {2_912 + t // 90}', f'bitagent_classifier_llm_match_calls_total{{model="{m}",stage="rerank"}} {2_640 + t // 100}',
        f'bitagent_classifier_llm_match_tokens_total{{kind="input",model="{m}",stage="extract"}} {1_204_000 + t * 8}',
        f'bitagent_classifier_llm_match_tokens_total{{kind="output",model="{m}",stage="extract"}} {96_300 + t}',
        f'bitagent_classifier_llm_match_tokens_total{{kind="input",model="{m}",stage="rerank"}} {2_011_000 + t * 8}',
        f'bitagent_classifier_llm_match_tokens_total{{kind="output",model="{m}",stage="rerank"}} {61_900 + t}',
        f'bitagent_classifier_llm_match_usage_missing_total{{model="{m}"}} 0', 'bitagent_classifier_llm_match_call_errors_total{stage="extract"} 3',
        'bitagent_classifier_llm_match_budget_skips_total{reason="cooldown"} 12', 'bitagent_classifier_llm_match_extract_total{result="ok"} 2811',
        'bitagent_classifier_llm_match_extract_total{result="empty"} 101', 'bitagent_classifier_llm_match_rerank_total{result="match"} 2402',
        'bitagent_classifier_llm_match_rerank_total{result="no_match"} 238', 'bitagent_classifier_llm_match_gate_rejects_total{gate="title_mismatch"} 141',
        'bitagent_classifier_llm_match_gate_rejects_total{gate="year_mismatch"} 63', 'bitagent_classifier_llm_match_gate_rejects_total{gate="ambiguous"} 37',
        'bitagent_classifier_llm_match_anime_total{english="dub",outcome="kept"} 84', 'bitagent_classifier_llm_match_anime_total{english="sub",outcome="kept"} 51',
        'bitagent_classifier_llm_match_anime_total{english="none",outcome="rejected"} 19',
        'bitagent_classifier_llm_match_call_duration_seconds_sum{stage="extract"} 4076.8', 'bitagent_classifier_llm_match_call_duration_seconds_count{stage="extract"} 2912',
        'bitagent_classifier_llm_match_call_duration_seconds_sum{stage="rerank"} 5016.0', 'bitagent_classifier_llm_match_call_duration_seconds_count{stage="rerank"} 2640',
        "bitagent_classifier_llm_match_cache_hits_total 18_402".replace("_", ""), "bitagent_classifier_llm_match_cache_misses_total 2912",
        "bitagent_classifier_llm_cache_hits_total 18402", "bitagent_classifier_llm_cache_misses_total 2912",
        f"bitagent_junkpurge_cycles_total {48 + t // 3600}", "bitagent_junkpurge_cycle_duration_seconds_sum 6116.4", "bitagent_junkpurge_cycle_duration_seconds_count 48",
        'bitagent_junkpurge_judged_total{verdict="junk"} 1204', 'bitagent_junkpurge_judged_total{verdict="keep"} 1096', 'bitagent_junkpurge_judged_total{verdict="unsure"} 100',
        "bitagent_junkpurge_quarantined_total 1204", "bitagent_junkpurge_would_delete_total 0", "bitagent_junkpurge_expired_total 611",
        f'bitagent_junkpurge_llm_requests_total{{model="{m}",outcome="ok",processing="grouped"}} {480 + t // 900}', "bitagent_junkpurge_llm_deferred_total 0",
        f'bitagent_junkpurge_llm_tokens_total{{model="{m}",processing="grouped",type="input"}} {425_400 + t * 3}',
        f'bitagent_junkpurge_llm_tokens_total{{model="{m}",processing="grouped",type="output"}} {38_100 + t}',
        'bitagent_wantbridge_wantlist_size{source="sonarr"} 312', 'bitagent_wantbridge_wantlist_size{source="radarr"} 87', 'bitagent_wantbridge_wantlist_size{source="lidarr"} 41',
        f'bitagent_wantbridge_matches_total{{source="sonarr",tier="0"}} {1_038 + t // 300}', 'bitagent_wantbridge_matches_total{source="radarr",tier="0"} 214',
        'bitagent_wantbridge_matches_total{source="lidarr",tier="0"} 26', 'bitagent_wantbridge_matches_total{source="sonarr",tier="2"} 140',
        "bitagent_wantbridge_fingerprint_keys 440",
        'bitagent_wantbridge_arr_poll_duration_seconds_sum{source="sonarr"} 91.2', 'bitagent_wantbridge_arr_poll_duration_seconds_count{source="sonarr"} 288',
        'bitagent_wantbridge_arr_poll_duration_seconds_sum{source="radarr"} 40.1', 'bitagent_wantbridge_arr_poll_duration_seconds_count{source="radarr"} 288',
        'bitagent_wantbridge_arr_poll_duration_seconds_sum{source="lidarr"} 22.6', 'bitagent_wantbridge_arr_poll_duration_seconds_count{source="lidarr"} 288',
        f'bitagent_evidence_events_received_total{{kind="webhook_grab",source="sonarr"}} {1_931 + t // 400}', 'bitagent_evidence_events_received_total{kind="webhook_import",source="sonarr"} 4370',
        'bitagent_evidence_events_received_total{kind="webhook_grab",source="radarr"} 402', 'bitagent_evidence_events_received_total{kind="webhook_import",source="radarr"} 388',
        f'bitagent_evidence_events_received_total{{kind="qbt_category",source="qbittorrent"}} {11_820 + t // 30}',
        'bitagent_evidence_events_persisted_total{kind="webhook_grab",source="sonarr"} 1931', 'bitagent_evidence_events_persisted_total{kind="webhook_import",source="sonarr"} 4361',
        'bitagent_evidence_events_persisted_total{kind="webhook_grab",source="radarr"} 402', 'bitagent_evidence_events_persisted_total{kind="webhook_import",source="radarr"} 388',
        'bitagent_evidence_events_persisted_total{kind="qbt_category",source="qbittorrent"} 11820',
        'bitagent_evidence_events_duplicated_total{kind="webhook_import",source="sonarr"} 9',
        f"bitagent_liveness_torznab_excluded_total {6_051 + t // 50}", "bitagent_liveness_blacklist_size 1288",
        # Same label contract as internal/evidence/liveness/metrics.go: class ∈ {alive, suspect}
        # + outcome per observation; revalidations by outcome ∈ {alive_again, still_dead, error}.
        f'bitagent_liveness_observations_total{{class="alive",outcome="upsert"}} {40_212 + t // 4}',
        f'bitagent_liveness_observations_total{{class="suspect",outcome="upsert"}} {9_804 + t // 20}',
        'bitagent_liveness_revalidations_total{outcome="alive_again"} 611', 'bitagent_liveness_revalidations_total{outcome="still_dead"} 2033',
        f'bitagent_dht_client_request_success_total{{query="get_peers"}} {8_480_211 + t * 310}', f'bitagent_dht_client_request_success_total{{query="find_node"}} {1_020_089 + t * 60}',
        'bitagent_dht_client_request_concurrency{query="get_peers"} 118', 'bitagent_dht_client_request_concurrency{query="find_node"} 47',
        "bitagent_csam_blocklist_entries 0", "bitagent_csam_blocklist_lookups_total 0", 'bitagent_csam_blocklist_export_total{outcome="local_ok"} 2065',
    ]
    return "\n".join(lines) + "\n"


def quarantine(offset: int, limit: int) -> dict:
    rows = []
    for n in range(offset, min(offset + limit, 1204)):
        name = f"{['sample','trailer','xvid','cam','promo'][n % 5]}-{['pack','rip','dupe','stub'][n % 4]}-{n:04d}.avi"
        rows.append({"infoHash": _hash(name), "name": name, "verdict": "junk", "reason": ["no metadata match", "trailer-length video", "duplicate of a kept release"][n % 3],
                     "quarantinedAt": _iso(time.time() - 3600 * (n + 2)), "expiresAt": _iso(time.time() + 86400 * (14 - n % 14)),
                     "daysLeft": 14 - n % 14, "size": 40_000_000 + n * 1_000_000, "seeders": n % 3})
    return {"total": 1204, "items": rows, "limit": limit, "offset": offset}


class Handler(BaseHTTPRequestHandler):
    def _send(self, body: bytes, ctype: str = "application/json", status: int = 200) -> None:
        self.send_response(status)
        self.send_header("Content-Type", ctype)
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def do_GET(self) -> None:  # noqa: N802
        url = urlsplit(self.path)
        if url.path == "/metrics":
            return self._send(metrics().encode(), "text/plain; version=0.0.4")
        if url.path == "/api/quarantine":
            q = dict(p.split("=", 1) for p in url.query.split("&") if "=" in p)
            return self._send(json.dumps(quarantine(int(q.get("offset", 0)), int(q.get("limit", 200)))).encode())
        if url.path.startswith("/torznab"):
            return self._send(b'<?xml version="1.0"?><rss><channel><title>BitAgent demo</title></channel></rss>', "application/xml")
        self._send(b"404 page not found\n", "text/plain", 404)

    def do_POST(self) -> None:  # noqa: N802
        url = urlsplit(self.path)
        raw = self.rfile.read(int(self.headers.get("Content-Length") or 0))
        if url.path == "/graphql":
            req = json.loads(raw or b"{}")
            return self._send(json.dumps(graphql(req.get("query", ""), req.get("variables") or {})).encode())
        if url.path.startswith("/api/quarantine/"):
            return self._send(b'{"ok": true}')
        self._send(b"404 page not found\n", "text/plain", 404)

    def do_DELETE(self) -> None:  # noqa: N802
        self._send(b'{"ok": true}')

    def log_message(self, fmt, *args):  # noqa: D102
        pass


if __name__ == "__main__":
    import sys
    port = int(sys.argv[1]) if len(sys.argv) > 1 else 3333
    print(f"demo core on http://127.0.0.1:{port}  (graphql, metrics, api/quarantine)")
    ThreadingHTTPServer(("127.0.0.1", port), Handler).serve_forever()

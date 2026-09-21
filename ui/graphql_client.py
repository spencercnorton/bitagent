"""GraphQL + Prometheus client for the BitAgent core.

The core's GraphQL schema (bitagent / bitmagnet) exposes:
    version() -> String
    workers()
    health()
    queue()
    torrent(infoHash)
    torrentContent { search(input: TorrentContentSearchQueryInput) }
    evidence { list(input: EvidenceListInput) }

This module wraps the real schema and derives the dashboard's stats from
torrentContent.search aggregations + the Prometheus /metrics scrape, and
proxies evidence queries to the upstream label_evidence table.
"""
from __future__ import annotations

import httpx
from config import settings

_client: httpx.AsyncClient | None = None


def _get_client() -> httpx.AsyncClient:
    global _client
    if _client is None:
        # 10s cap, not 30s: /api/stats fans out to several upstream calls and the
        # dashboard re-polls every 30s. A 30s connect+read on a wedged core let
        # requests pile up faster than they drained; 10s fails fast and the UI
        # degrades to its honest "Unreachable" state instead of hanging.
        _client = httpx.AsyncClient(timeout=httpx.Timeout(10.0, connect=5.0))
    return _client


async def query(q: str, variables: dict | None = None, *, timeout: float | None = None) -> dict:
    """POST a GraphQL query. `timeout` overrides the client's 10s default for
    the rare known-slow query (a cold unfiltered groupByContent browse scans
    the whole matched corpus, ~4-15s) without loosening every other call."""
    client = _get_client()
    try:
        resp = await client.post(
            settings.bitagent_graphql_url,
            json={"query": q, "variables": variables or {}},
            timeout=timeout if timeout is not None else httpx.USE_CLIENT_DEFAULT,
        )
        resp.raise_for_status()
        return resp.json()
    except httpx.HTTPStatusError as exc:
        # The core returns validation failures as HTTP 422 with a standard
        # GraphQL errors body — pass the real messages through instead of
        # reporting "unreachable" (which sends operators debugging the
        # network when the query itself is wrong).
        try:
            body = exc.response.json()
            if isinstance(body, dict) and body.get("errors"):
                return {"data": None, "errors": body["errors"]}
        except ValueError:
            pass
        return {"data": None, "errors": [{"message": f"GraphQL endpoint returned HTTP {exc.response.status_code}"}]}
    except httpx.HTTPError:
        return {"data": None, "errors": [{"message": "GraphQL endpoint unreachable"}]}


async def fetch_metrics() -> str:
    client = _get_client()
    try:
        resp = await client.get(settings.bitagent_metrics_url)
        resp.raise_for_status()
        return resp.text
    except httpx.HTTPError:
        return ""


# ── Real schema queries ──────────────────────────────────────────────────

SEARCH_TORRENTS = """
query Search($input: TorrentContentSearchQueryInput!) {
  torrentContent {
    search(input: $input) {
      totalCount
      totalCountIsEstimate
      hasNextPage
      aggregations {
        contentType { value count }
        genre { value count }
        releaseYear { value count }
        videoResolution { value count }
        videoSource { value count }
      }
      items {
        infoHash
        title
        contentType
        contentSource
        contentId
        seeders
        leechers
        createdAt
        updatedAt
        languages { id }
        videoResolution
        videoSource
        releaseGroup
        content {
          originalLanguage { id name }
          originalTitle
        }
        torrent {
          name
          size
          filesCount
        }
      }
    }
  }
}
"""

# Matched metadata lives on the generic `content` field (there are no
# movie/tvShow/tvEpisode/tvSeason fields on TorrentContent in the core
# schema); episode structure lives on `episodes`. Validated by introspection
# against the live core — an invalid field here fails the whole query and
# the modal 404s for every torrent.
TORRENT_DETAIL = """
query TorrentDetail($infoHash: Hash20!) {
  torrentContent {
    search(input: { infoHashes: [$infoHash], limit: 1 }) {
      items {
        infoHash
        title
        contentType
        contentSource
        contentId
        seeders
        leechers
        createdAt
        updatedAt
        languages { id }
        videoResolution
        videoSource
        videoCodec
        releaseGroup
        episodes { label seasons { season episodes } }
        content {
          title
          releaseYear
          originalLanguage { id }
          externalLinks { metadataSource { key } url }
        }
        torrent {
          name
          size
          filesCount
          files { path size }
        }
      }
    }
  }
}
"""

# Stats: totalCount + per-contentType aggregations in one round-trip.
# `facets:{contentType:{aggregate:true}}` triggers the contentType array.
SYSTEM_STATS = """
query SystemStats {
  version
  torrentContent {
    search(input: {
      queryString: "",
      limit: 1,
      totalCount: true,
      facets: { contentType: { aggregate: true } }
    }) {
      totalCount
      totalCountIsEstimate
      aggregations {
        contentType { value count }
      }
    }
  }
}
"""

# GraphQL validates the whole document before it executes anything, so one
# field a core does not know takes down every GraphQL-backed dashboard metric
# — total, categories, core version — not just that field. This is the same
# query with the newest field stripped, used as a one-shot fallback so the
# panel degrades to "no estimate flag" instead of "unreachable".
# Not aimed at totalCountIsEstimate specifically: that has been
# `Boolean!` on TorrentContentSearchResult since upstream v0.10.0-beta.7
# (2024-10-14) and SEARCH_TORRENTS has required it since bitagent-ui v1.29.0,
# so a core that rejects it would have broken library search first. It is the
# net for the next field added here.
SYSTEM_STATS_LEGACY = "\n".join(
    line for line in SYSTEM_STATS.splitlines()
    if line.strip() != "totalCountIsEstimate"
)

# Public-library headline counts. Both aliases count the same current
# torrentContent.search population. The rolling-window alias is constrained by
# the core's immutable torrents.created_at fields (core >= the release that
# added torrentCreatedAfter/torrentCreatedBefore), never source-row updated_at.
# The caller requests exact counts for both aliases and runs the document in the
# background (app._get_library_stats). Measured 2026-09-19 at the core: the
# all-time movie/TV count took 1.2 s, the seven-day range 7.1 s and this document
# 14.3 s, while the planner estimate for the range was 2x off (400,750 vs
# 194,253). The API still carries estimate flags defensively.
LIBRARY_STATS = """
query LibraryStats(
  $totalInput: TorrentContentSearchQueryInput!
  $recentInput: TorrentContentSearchQueryInput!
) {
  torrentContent {
    total: search(input: $totalInput) {
      totalCount
      totalCountIsEstimate
    }
    recent: search(input: $recentInput) {
      totalCount
      totalCountIsEstimate
    }
  }
}
"""

# Recent matched movie/TV torrents for the AI tab's review table: raw torrent
# name vs the matched TMDB title/year, ordered by updated_at desc (a classifier
# (re)match updates the torrent-content row). Field enum values confirmed
# against the BitAgent core's graphql/schema/enums.graphqls
# (TorrentContentOrderByField: relevance, published_at, updated_at, ...).
RECENT_MATCHES = """
query RecentMatches($input: TorrentContentSearchQueryInput!) {
  torrentContent {
    search(input: $input) {
      items {
        infoHash
        title
        contentType
        contentSource
        contentId
        seeders
        videoResolution
        createdAt
        updatedAt
        content { title releaseYear }
        torrent { name tagNames }
      }
    }
  }
}
"""

# (TORRENT_BY_HASH removed: Query.torrent is a namespace (files/listSources/
# suggestTags/metrics) in the core schema, not a by-hash record lookup — the
# old query never validated. torrentContent.search(infoHashes:) IS the lookup.)

# Evidence is sourced from the upstream label_evidence table — qBittorrent
# polls and *arr webhooks both feed it on the backend, so this is the single
# source of truth. The result shape mirrors the legacy local-SQLite contract
# (id, source, eventType, infoHash, torrentName, contentType, result,
# timestamp) so existing dashboard JS doesn't need rewiring.
EVIDENCE_LIST = """
query EvidenceList($input: EvidenceListInput!) {
  evidence {
    list(input: $input) {
      totalCount
      items {
        id
        source
        kind
        infoHash
        title
        mediaType
        observedAt
      }
    }
  }
}
"""

# North-star KPI: grab wins by indexer from label_evidence webhook_grab
# payloads. Aggregate counts only — the core never returns release names
# on this query. Requires core >= v0.40.0; older cores fail validation
# and fetch_indexer_stats reports available:false.
INDEXER_STATS = """
query IndexerStats($input: EvidenceIndexerStatsInput!) {
  evidence {
    indexerStats(input: $input) {
      totalGrabs
      bitagentGrabs
      bitagentWinRate
      indexers { indexer grabs }
      days { date totalGrabs bitagentGrabs }
    }
  }
}
"""


async def fetch_indexer_stats(days: int) -> dict:
    """Fetch the grab win-rate aggregate for the last `days` days.

    Returns {"available": bool, ...stats}. available:false covers both an
    unreachable core and a core too old to know the query — the dashboard
    renders the same "not available" state for either.
    """
    resp = await query(INDEXER_STATS, {"input": {"days": int(days)}})
    stats = (((resp or {}).get("data") or {}).get("evidence") or {}).get("indexerStats")
    if not stats:
        return {"available": False}
    return {"available": True, **stats}


# Upstream mediaType → UI contentType. The dashboard's typePill knows about
# movie/tv_show/music/ebook; book and audiobook fall through to a neutral
# pill, which is the right behaviour. unknown becomes empty so the pill
# renders "--".
_MEDIA_TYPE_TO_CONTENT_TYPE = {
    "movie": "movie",
    "tv": "tv_show",
    "music": "music",
    "book": "ebook",
    "audiobook": "audiobook",
    "unknown": "",
    "": "",
}


def _parse_iso8601_to_unix(value: str) -> float:
    """Best-effort ISO8601 → unix-seconds. Returns 0 on failure."""
    if not value:
        return 0.0
    from datetime import datetime
    try:
        return datetime.fromisoformat(value.replace("Z", "+00:00")).timestamp()
    except ValueError:
        return 0.0


async def fetch_evidence_list(limit: int, offset: int) -> dict:
    """Fetch a page of evidence rows and reshape to the dashboard contract.

    Returns {"totalCount": int, "items": [...]}. On upstream failure returns
    an empty page rather than raising — the dashboard widget already renders
    a "no events" state.
    """
    resp = await query(
        EVIDENCE_LIST,
        {"input": {"limit": int(limit), "offset": int(offset)}},
    )
    data = (resp or {}).get("data") or {}
    listed = ((data.get("evidence") or {}).get("list")) or {}
    upstream_items = listed.get("items") or []
    items = []
    for row in upstream_items:
        try:
            row_id = int(row.get("id") or 0)
        except (TypeError, ValueError):
            row_id = 0
        media_type = (row.get("mediaType") or "").lower()
        items.append({
            "id": row_id,
            "source": row.get("source") or "",
            # The real event kind (qb_state_observation, *arr grab/import/…).
            # The UI colours + humanizes this; there is no upstream success/fail
            # verdict, so we do NOT fabricate a `result` field (a prior version
            # hardcoded result:"success", making every row show a green pill).
            "eventType": row.get("kind") or "",
            "infoHash": row.get("infoHash") or None,
            "torrentName": row.get("title") or "",
            "contentType": _MEDIA_TYPE_TO_CONTENT_TYPE.get(media_type, media_type),
            "timestamp": _parse_iso8601_to_unix(row.get("observedAt") or ""),
        })
    return {"totalCount": int(listed.get("totalCount") or 0), "items": items}

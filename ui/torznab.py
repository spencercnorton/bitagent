"""Torznab proxy — a self-contained APIRouter, factored out of app.py.

Unlike the console/library routes this surface is NOT behind the SSO gate: it
authenticates with a per-user `ba_` key (Prowlarr/Sonarr poll it), rate-limits
per key, then forwards to the core's Torznab endpoint with the operator's core
key swapped in. Mounted via `app.include_router()`; the upstream destination is
frozen at startup (settings only), never runtime-mutable.
"""
from __future__ import annotations

import logging
import time
import hashlib
from datetime import datetime, timezone
from xml.etree import ElementTree as ET

import httpx
from fastapi import APIRouter, HTTPException, Request, Response

from config import settings
from account_usage import record_api_search
from database import (
    _hash_user_api_key,
    get_override,
    lookup_user_api_key,
    touch_user_api_key,
)

logger = logging.getLogger("bitagent-ui")

router = APIRouter()

# Per-key token bucket for the torznab proxy (the app is single-process). The
# proxy bypasses the SSO gate (ba_-key auth) and fans out to the core on every
# hit, so an abusive key could hammer it; legitimate Prowlarr/Sonarr polling
# stays well under the default. Set torznab_rate_limit_per_min=0 to disable.
_TZ_BUCKETS: dict = {}


def _torznab_rate_ok(key_id, *, rate=None) -> tuple[bool, int]:
    if rate is None:
        rate = getattr(settings, "torznab_rate_limit_per_min", 0) or 0
    if rate <= 0:
        return True, 0
    cap = float(rate)
    refill = rate / 60.0
    now = time.monotonic()
    tokens, last = _TZ_BUCKETS.get(key_id, (cap, now))
    tokens = min(cap, tokens + (now - last) * refill)
    if tokens < 1.0:
        _TZ_BUCKETS[key_id] = (tokens, now)
        return False, int((1.0 - tokens) / refill) + 1
    _TZ_BUCKETS[key_id] = (tokens - 1.0, now)
    return True, 0


def _extract_torznab_api_key(request: Request) -> str:
    query = {key.lower(): value for key, value in request.query_params.multi_items()}
    query_key = query.get("apikey") or query.get("api_key")
    if query_key:
        return query_key
    header_key = request.headers.get("x-api-key")
    if header_key:
        return header_key
    auth = request.headers.get("authorization") or ""
    if auth.lower().startswith("bearer "):
        return auth[7:].strip()
    return ""


def _torznab_error(status: int, code: int, description: str) -> Response:
    xml = ET.tostring(
        ET.Element("error", code=str(code), description=description),
        encoding="utf-8", xml_declaration=True,
    )
    return Response(xml, status_code=status, media_type="application/xml")


def _core_torznab_url(path: str) -> str:
    """Build the frozen Torznab destination from startup settings only."""
    base = getattr(settings, "bitagent_torznab_url", "") or ""
    if not base:
        gql_url = settings.bitagent_graphql_url
        gql_base = gql_url.rstrip("/")
        if gql_base.endswith("/graphql"):
            gql_base = gql_base[: -len("/graphql")]
        base = gql_base.rstrip("/") + "/torznab"
    return base.rstrip("/") + "/" + path.lstrip("/")


# The core answers every /torznab/* subpath identically (GET /torznab/*any); a
# Torznab client only ever asks for "/torznab/api" or "/torznab/". Anything else
# is refused before the upstream URL exists: httpx normalises dot segments, so
# a forwarded "../graphql" would have reached the core's other endpoints with
# the operator key attached.
_TORZNAB_PATHS = {"", "api"}
_SEARCH_FUNCTIONS = {"search", "movie", "tvsearch", "music", "book"}


class _SearchFeedTree(ET.TreeBuilder):
    def doctype(self, name, pubid, system):
        raise ValueError("Search feeds must not contain a DTD")


def _successful_search(request: Request, upstream) -> bool:
    """HTTP 200 may contain a Torznab <error>, so require a real search feed."""
    functions = request.query_params.getlist("t")
    if (
        request.method != "GET"
        or not functions
        or functions[0] not in _SEARCH_FUNCTIONS
        or upstream.status_code != 200
    ):
        return False
    body = upstream.content
    # The core emits plain RSS. Do not resolve DTD/entity declarations while
    # checking a response, and never treat malformed XML as a successful search.
    try:
        root = ET.fromstring(body, parser=ET.XMLParser(target=_SearchFeedTree()))
    except (ET.ParseError, ValueError):
        return False
    return (
        root.tag.rsplit("}", 1)[-1] == "rss"
        and any(child.tag.rsplit("}", 1)[-1] == "channel" for child in root)
        and not any(node.tag.rsplit("}", 1)[-1] == "error" for node in root.iter())
    )


def _private_caps() -> bytes:
    """Advertise only the categories and search modes implemented locally."""
    caps = ET.Element("caps")
    ET.SubElement(caps, "server", title="BitAgent Private Library")
    ET.SubElement(caps, "limits", max="100", default="100")
    ET.SubElement(caps, "registration", available="no", open="no")
    searching = ET.SubElement(caps, "searching")
    ET.SubElement(searching, "search", available="yes", supportedParams="q")
    ET.SubElement(searching, "movie-search", available="yes", supportedParams="q,imdbid,tmdbid")
    ET.SubElement(
        searching, "tv-search", available="yes",
        supportedParams="q,imdbid,tmdbid,tvdbid,season,ep",
    )
    categories = ET.SubElement(caps, "categories")
    ET.SubElement(categories, "category", id="2000", name="Movies")
    ET.SubElement(categories, "category", id="5000", name="TV")
    return ET.tostring(caps, encoding="utf-8", xml_declaration=True)


def _private_feed(result: dict, base_url: str, api_key: str) -> bytes:
    """Private releases use authenticated torrent URLs, never public magnets."""
    from urllib.parse import urlencode

    namespace = "http://torznab.com/schemas/2015/feed"
    ET.register_namespace("torznab", namespace)
    rss = ET.Element("rss", version="2.0")
    channel = ET.SubElement(rss, "channel")
    ET.SubElement(channel, "title").text = "BitAgent Private Library"
    ET.SubElement(channel, "link").text = base_url + "/library"
    ET.SubElement(channel, "description").text = "Private member media releases"
    ET.SubElement(
        channel, f"{{{namespace}}}response",
        offset=str(result["offset"]), total=str(result["total"]),
    )
    for release in result["items"]:
        item = ET.SubElement(channel, "item")
        ET.SubElement(item, "title").text = release["title"]
        alias = hashlib.sha256(release["source_id"].encode()).hexdigest()
        ET.SubElement(item, "guid", isPermaLink="false").text = "private:" + release["id"] + ":" + alias
        download = base_url + "/torznab/private/api?" + urlencode({
            "t": "get", "id": release["id"], "apikey": api_key,
        })
        ET.SubElement(item, "link").text = download
        created = datetime.fromtimestamp(float(release["created_at"]), timezone.utc)
        ET.SubElement(item, "pubDate").text = created.strftime("%a, %d %b %Y %H:%M:%S +0000")
        ET.SubElement(
            item, "enclosure", url=download,
            length=str(release["size"]), type="application/x-bittorrent",
        )

        def attr(name, value):
            if value is not None:
                ET.SubElement(item, f"{{{namespace}}}attr", name=name, value=str(value))

        attr("category", 2000 if release["kind"] == "movie" else 5000)
        attr("size", release["size"])
        # Unknown swarm counts stay absent; a download-capable release is not
        # evidence that someone is currently seeding it.
        attr("seeders", release.get("seeders"))
        attr("leechers", release.get("leechers"))
        attr("imdb", release.get("imdb_id"))
        attr("tmdbid", release.get("tmdb_id"))
        attr("tvdbid", release.get("tvdb_id"))
        attr("season", release.get("season"))
        for episode in release.get("episodes") or []:
            attr("episode", episode)
    return ET.tostring(rss, encoding="utf-8", xml_declaration=True)


@router.api_route("/torznab/private/api", methods=["GET", "HEAD"], include_in_schema=False)
async def private_torznab(request: Request):
    """A separate paginated indexer for authenticated private releases."""
    import private_indexer

    if not settings.private_indexer_enabled:
        return _torznab_error(404, 910, "Private indexer disabled")
    presented = _extract_torznab_api_key(request)
    row = await lookup_user_api_key(_hash_user_api_key(presented)) if presented else None
    if not row:
        return _torznab_error(401, 100, "Invalid API key")
    if not await private_indexer.member_active(row["user_id"]):
        return _torznab_error(403, 100, "Membership required")
    ok, retry_after = _torznab_rate_ok(row["id"])
    if not ok:
        response = _torznab_error(429, 500, "Rate limit exceeded")
        response.headers["Retry-After"] = str(retry_after)
        return response
    params = {
        key.lower(): value for key, value in request.query_params.multi_items()
        if key.lower() not in {"apikey", "api_key"}
    }
    function = params.get("t", "search").lower()
    if function == "caps":
        body = _private_caps()
    elif function == "get":
        if not params.get("id"):
            return _torznab_error(400, 200, "Missing parameter (id)")
        try:
            return await private_indexer.torrent_response(params["id"], request, row)
        except HTTPException as exc:
            code = 100 if exc.status_code in {401, 403} else 300 if exc.status_code == 404 else 203
            return _torznab_error(exc.status_code, code, str(exc.detail))
    elif function in {"search", "movie", "tvsearch"}:
        params["t"] = function
        try:
            result = await private_indexer.search_releases(params)
        except ValueError as exc:
            return _torznab_error(400, 201, str(exc))
        except HTTPException as exc:
            if exc.status_code in {400, 422}:
                return _torznab_error(400, 201, str(exc.detail))
            raise
        body = _private_feed(result, private_indexer.external_base(request), presented)
    else:
        return _torznab_error(400, 202, "Function not available")
    await touch_user_api_key(row["id"])
    return Response(
        b"" if request.method == "HEAD" else body,
        media_type="application/xml",
        headers={"Cache-Control": "no-store"},
    )


@router.api_route("/torznab/{path:path}", methods=["GET", "HEAD"], include_in_schema=False)
async def torznab_proxy(path: str, request: Request):
    if path.strip("/") not in _TORZNAB_PATHS:
        return _torznab_error(404, 900, "Not found")
    presented = _extract_torznab_api_key(request)
    if not presented:
        return _torznab_error(401, 100, "Missing API key")
    row = await lookup_user_api_key(_hash_user_api_key(presented))
    if not row:
        return _torznab_error(401, 100, "Invalid API key")
    if settings.private_indexer_enabled:
        import private_indexer

        if not await private_indexer.torznab_requires_member(row):
            return _torznab_error(403, 100, "Membership required")

    ok, retry_after = _torznab_rate_ok(row["id"])
    if not ok:
        resp = _torznab_error(429, 900, "Rate limit exceeded")
        resp.headers["Retry-After"] = str(retry_after)
        return resp

    # Only the credential is runtime-mutable. The upstream destination below
    # is derived exclusively from startup settings, even if a legacy frozen
    # row is somehow reinserted after the startup purge.
    core_key = await get_override("torznab_api_key") or settings.torznab_api_key
    params = [
        (k, v)
        for k, v in request.query_params.multi_items()
        if k.lower() not in {"apikey", "api_key"}
    ]
    if core_key:
        params.append(("apikey", core_key))

    headers = {}
    for name in ("accept", "user-agent"):
        value = request.headers.get(name)
        if value:
            headers[name] = value
    try:
        async with httpx.AsyncClient(timeout=30.0, follow_redirects=False) as client:
            upstream = await client.request(
                request.method,
                _core_torznab_url("api"),
                params=params,
                headers=headers,
            )
    except httpx.HTTPError as exc:
        # httpx exception text may include the operator key in the upstream
        # request URL. The failure class is enough to diagnose connectivity.
        logger.warning("torznab proxy failed (%s)", type(exc).__name__)
        return _torznab_error(502, 900, "Upstream Torznab request failed")

    await touch_user_api_key(row["id"])
    if _successful_search(request, upstream):
        try:
            await record_api_search(row["user_id"])
        except Exception as exc:
            # Usage accounting must not turn a successful indexer response into
            # a failed client request. No URLs, keys or search terms are logged.
            logger.warning("account usage recording failed (%s)", type(exc).__name__)
    out_headers = {}
    content_type = upstream.headers.get("content-type")
    if content_type:
        out_headers["content-type"] = content_type
    cache_control = upstream.headers.get("cache-control")
    if cache_control:
        out_headers["cache-control"] = cache_control
    body = b"" if request.method == "HEAD" else upstream.content
    return Response(body, status_code=upstream.status_code, headers=out_headers)

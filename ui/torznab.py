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

import httpx
from fastapi import APIRouter, Request, Response

from config import settings
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


def _torznab_rate_ok(key_id) -> tuple[bool, int]:
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
    query_key = request.query_params.get("apikey") or request.query_params.get("api_key")
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
    xml = (
        '<?xml version="1.0" encoding="UTF-8"?>'
        f'<error code="{code}" description="{description}"/>'
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
        logger.warning("torznab proxy failed: %s", exc)
        return _torznab_error(502, 900, "Upstream Torznab request failed")

    await touch_user_api_key(row["id"])
    out_headers = {}
    content_type = upstream.headers.get("content-type")
    if content_type:
        out_headers["content-type"] = content_type
    cache_control = upstream.headers.get("cache-control")
    if cache_control:
        out_headers["cache-control"] = cache_control
    body = b"" if request.method == "HEAD" else upstream.content
    return Response(body, status_code=upstream.status_code, headers=out_headers)

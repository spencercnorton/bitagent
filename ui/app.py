from __future__ import annotations
import asyncio
import base64
from collections import Counter, OrderedDict
import copy
from datetime import datetime, timedelta, timezone
import hashlib
import ipaddress
import json
import logging
import re as _re
import secrets
import socket
import time
from pathlib import Path
from urllib.parse import quote as _urlquote, urlparse as _urlparse_url
from contextlib import asynccontextmanager

from fastapi import FastAPI, Request, Depends, HTTPException, Query
from fastapi.staticfiles import StaticFiles
from fastapi.templating import Jinja2Templates
from fastapi.responses import JSONResponse, FileResponse, RedirectResponse, Response
from pydantic import BaseModel

import httpx
from version import __version__
from config import settings, MUTABLE_FIELDS, SENSITIVE_FIELDS, ARR_BASE_URL_FIELDS
from auth import (
    require_auth, describe_active_tiers, validate_auth_settings,
    proxy_provenance_valid,
)
from database import (
    get_db, get_all_overrides, set_override, get_audit_log,
    delete_override, get_user_api_key, create_user_api_key,
    revoke_user_api_key, _hash_user_api_key,
    bump_block_phrase_hits, close_all,
)
import graphql_client as gql
import tmdb
from prom_metrics import (
    _parse_prometheus_snapshot,
    _nonnegative_int,
    _nonnegative_rate,
    _labeled_metric_sum,
    _metric_family_present,
    _iter_labeled,
    _llm_spend,
    LLM_PRICES_AS_OF,
)
from search_input import (
    _csv_values,
    _year_values,
    _resolution_values,
    _content_type_values,
    _search_input,
    _api_item_from_search_item,
)
from deps import (
    _validate_host_settings,
    _host_scope,
    require_operator,
    require_public_library,
)
from torznab import router as torznab_router
from infisical import hydrate_settings
from telemetry import (
    MetricEnvelope,
    TelemetryUnavailable,
    collect_bounded_probes,
    snapshot_timestamp,
    unavailable_metric,
)


@asynccontextmanager
async def lifespan(app: FastAPI):
    from database import get_db
    _reset_stats_snapshot_cache()
    _reset_library_stats_cache()
    _reset_indexer_stats_cache()
    # Infisical, when configured, is the authoritative source for secrets
    # (e.g. TMDB_API_KEY). Hydrate before serving so a stale/typo'd deployment
    # env literal can't silently break poster fetches. No-op + fail-open when
    # Infisical is unconfigured or unreachable.
    hydrate_settings(settings)
    validate_auth_settings()
    _validate_host_settings()
    _app_switcher_origin()  # a malformed APP_SWITCHER_SCRIPT_URL fails startup, not every request
    await get_db()
    yield
    _reset_stats_snapshot_cache()
    _reset_library_stats_cache()
    _reset_indexer_stats_cache()
    await close_all()


logger = logging.getLogger("bitagent-ui")

app = FastAPI(title="BitAgent Console", version=__version__, lifespan=lifespan)

# Torznab proxy lives in its own module (ba_-key auth, not the SSO gate).
app.include_router(torznab_router)

# CSRF is otherwise mitigated only by our routes being JSON-only (a simple
# cross-site form can't set Content-Type: application/json). Sec-Fetch-Site adds
# a self-contained second layer that also covers the no-body POST routes and any
# future route that forgets the JSON gate — instead of depending on a SameSite
# cookie attribute set in the upstream auth portal (another repo).
_CSRF_SAFE_METHODS = frozenset({"GET", "HEAD", "OPTIONS", "TRACE"})
_CSRF_SAFE_FETCH_SITES = frozenset({"same-origin", "same-site"})


def _app_switcher_origin() -> str:
    """` https://host` for the CSP script-src when APP_SWITCHER_SCRIPT_URL is set."""
    url = settings.app_switcher_script_url.strip()
    if not url:
        return ""
    parts = _urlparse_url(url)
    if parts.scheme not in ("http", "https") or not parts.netloc:
        raise RuntimeError("APP_SWITCHER_SCRIPT_URL must be an absolute http(s) URL")
    return f" {parts.scheme}://{parts.netloc}"


@app.middleware("http")
async def _security_headers(request: Request, call_next):
    """Baseline security headers on every response. The UI is built with inline
    event handlers + a pre-paint inline theme script, so script/style-src still
    need 'unsafe-inline' (removing that needs the inline-handler refactor). Even
    so this restricts img/connect sources, forbids framing, and pins base/form."""
    # `/healthz` is the only host-agnostic endpoint so local container health
    # checks keep working. Every other path must name one of the two configured
    # surfaces; an unknown/malformed Host never falls through to operator.
    if request.scope.get("path") != "/healthz" and _host_scope(request) is None:
        resp = JSONResponse(status_code=421, content={"detail": "Misdirected request"})
    elif (
        request.method not in _CSRF_SAFE_METHODS
        # Browsers stamp Sec-Fetch-Site on every request and JS cannot forge it,
        # so a state-changing request a browser marks cross-site/none is a forged
        # (CSRF) call. Non-browser callers (Sonarr/Prowlarr on /torznab, api-key
        # clients, curl) omit the header and pass — we enforce only when present.
        and (fs := request.headers.get("sec-fetch-site")) is not None
        and fs not in _CSRF_SAFE_FETCH_SITES
    ):
        resp = JSONResponse(status_code=403, content={"detail": "Cross-site request blocked"})
    else:
        resp = await call_next(request)
    resp.headers.setdefault("X-Content-Type-Options", "nosniff")
    resp.headers.setdefault("X-Frame-Options", "DENY")
    resp.headers.setdefault("Referrer-Policy", "strict-origin-when-cross-origin")
    resp.headers.setdefault(
        "Content-Security-Policy",
        "default-src 'self'; "
        "img-src 'self' https://image.tmdb.org https://coverartarchive.org data:; "
        # An optional cross-origin app-switcher script (APP_SWITCHER_SCRIPT_URL)
        # needs its origin here or it is blocked outright. Its <style>
        # injection and inline style attributes are already covered by
        # style-src 'unsafe-inline'.
        f"script-src 'self' 'unsafe-inline'{_app_switcher_origin()}; "
        "style-src 'self' 'unsafe-inline'; "
        "font-src 'self'; connect-src 'self'; "
        "frame-ancestors 'none'; base-uri 'self'; form-action 'self'",
    )
    # Cache policy: versioned static assets (served with ?v=<content-hash>) are
    # safe to cache forever; everything else (HTML shells, and especially authed
    # /api responses like settings/account/api-key mint) must never be stored.
    # setdefault so routes that set their own (poster proxies) keep it.
    if request.scope.get("path", "").startswith("/static/"):
        resp.headers.setdefault("Cache-Control", "public, max-age=31536000, immutable")
    else:
        resp.headers.setdefault("Cache-Control", "no-store")
    return resp


# 1x1 transparent GIF: served for a poster miss in redirect mode so the grid's
# lazy <img> loads successfully (the CSS placeholder shows through) rather than
# logging a 404 for every unmatched / no-art card.
_BLANK_GIF = base64.b64decode("R0lGODlhAQABAIAAAAAAAP///yH5BAEAAAAALAAAAAABAAEAAAIBRAA7")
def _blank_poster() -> Response:
    return Response(_BLANK_GIF, media_type="image/gif",
                    headers={"Cache-Control": "public, max-age=3600"})


_START_TIME = time.time()

# Sliding-window snapshots of successful /metrics probes for rate calculation.
# Each entry is (monotonic_ts, dict[bare_metric_name -> sum_across_labels]).
# Time bounds, not an assumed browser polling cadence, define the window.
_METRIC_HISTORY: list[tuple[float, dict[str, float]]] = []
_METRIC_HISTORY_MAX = 64
_METRIC_HISTORY_WINDOW_SECONDS = 300.0
_METRIC_RATE_MIN_INTERVAL_SECONDS = 10.0

# /api/stats owns a fixed, bounded fan-out. These startup constants are not
# mutable operator settings: allowing runtime changes could turn one dashboard
# poll into an unbounded upstream load generator.
_STATS_PROBE_TIMEOUT_SECONDS = 8.0
_STATS_MAX_PARALLEL_PROBES = 4
_STATS_SNAPSHOT_TTL_SECONDS = 5.0
_STATS_SNAPSHOT_CACHE: tuple[float, dict] | None = None
_STATS_SNAPSHOT_INFLIGHT: asyncio.Task | None = None

# The public banner is identical for every authenticated library visitor and
# its exact GraphQL counts are slow: measured 2026-09-19 straight at the core,
# the all-time movie/TV count took 1.2 s, the seven-day window 7.1 s and the
# shipped two-alias document 14.3 s — past the client's 10 s default, so the
# collection never completed, nothing was ever cached, and every page load
# waited on it until the edge cut the request with a 502. Collect at most once
# per ten minutes with a timeout the query can meet, serve the last snapshot
# while a refresh runs behind it, and bound how long a cold-cache request may
# wait so the edge (10 s) never times out on this endpoint.
_LIBRARY_STATS_CACHE_TTL_SECONDS = 600.0
_LIBRARY_STATS_QUERY_TIMEOUT_SECONDS = 60.0
_LIBRARY_STATS_INLINE_WAIT_SECONDS = 5.0
# A failed collection waits before the next attempt: 60 s, doubling per
# consecutive failure, capped at the cadence — so an unhealthy core sees one
# bounded attempt per window, not one per request. A stale snapshot keeps being
# served meanwhile; with nothing cached the answer is `available: false`.
_LIBRARY_STATS_RETRY_BASE_SECONDS = 60.0
_LIBRARY_STATS_RETRY_DELAY = 0.0        # current backoff; 0 once a refresh succeeds
_LIBRARY_STATS_RETRY_NOT_BEFORE = 0.0   # monotonic deadline set by a failed refresh
_LIBRARY_STATS_CONTENT_TYPES = ("movie", "tv_show")
_LIBRARY_STATS_CACHE: tuple[float, dict] | None = None
_LIBRARY_STATS_INFLIGHT: asyncio.Task | None = None

# The indexer win-rate aggregate reads many evidence rows and is operator-
# identical, yet the dashboard re-polls it every 30s. Cache the usable snapshot
# per day-window and singleflight concurrent polls so the poll cadence stops
# amplifying into the core. Keyed by (clamped) days; an "available: False"
# result is NOT cached so a transient core outage self-heals on the next poll.
_INDEXER_STATS_CACHE_TTL_SECONDS = 45.0
_INDEXER_STATS_CACHE: dict[int, tuple[float, object]] = {}
_INDEXER_STATS_INFLIGHT: dict[int, asyncio.Task] = {}

BASE = Path(__file__).parent
app.mount("/static", StaticFiles(directory=BASE / "static"), name="static")
templates = Jinja2Templates(directory=BASE / "templates")


def _compute_asset_version() -> str:
    """Content hash of the front-end assets, used as the `?v=` cache-bust token.

    A hardcoded token never gets bumped, so returning browsers can run stale JS
    after a deploy (StaticFiles sends no Cache-Control, only ETag/Last-Modified,
    which permits heuristic caching). Hashing the actual asset bytes means the
    token changes iff the content changes — every real deploy busts the cache,
    and identical content stays cached.
    """
    h = hashlib.sha256()
    for rel in ("css/tokens.css", "css/app.css", "css/library.css",
                "js/torrent-kind.js", "js/norm-title.js", "js/app.js", "js/library.js"):
        try:
            h.update((BASE / "static" / rel).read_bytes())
        except OSError:
            h.update(rel.encode())  # missing asset → still a stable token
    return h.hexdigest()[:12]


# Computed once at import; the static bundle is immutable for the process life.
ASSET_VERSION = _compute_asset_version()


# ── Health ────────────────────────────────────────────────────────────

@app.get("/healthz")
async def healthz():
    return {"status": "ok", "ts": time.time()}


_LIBRARY_MANIFEST = {
    "name": f"Library — {settings.library_brand}",
    "short_name": "Library",
    "description": "Browse and search the BitAgent media library",
    "id": "/", "start_url": "/", "scope": "/",
    "display": "standalone",
    "background_color": "#0f1117", "theme_color": "#4f6ef7",
    "icons": [
        {"src": "/static/img/icon-192.png", "sizes": "192x192", "type": "image/png", "purpose": "any"},
        {"src": "/static/img/icon-512.png", "sizes": "512x512", "type": "image/png", "purpose": "any"},
        {"src": "/static/img/icon-maskable-192.png", "sizes": "192x192", "type": "image/png", "purpose": "maskable"},
        {"src": "/static/img/icon-maskable-512.png", "sizes": "512x512", "type": "image/png", "purpose": "maskable"},
    ],
}


@app.get("/manifest.webmanifest")
async def webmanifest(request: Request):
    """Web app manifest, served from the root so its default scope covers "/".

    Unauthenticated like /healthz: browsers fetch the manifest without
    credentials by default, and install eligibility dies on a 401/redirect.
    It contains only public branding — no data. Explicit operator hosts get
    console metadata, explicit public hosts get library metadata, and unknown
    hosts fail closed exactly like the page surfaces.
    """
    scope = _host_scope(request)
    if scope == "operator":
        return FileResponse(
            BASE / "static" / "manifest.webmanifest",
            media_type="application/manifest+json",
        )
    if scope == "public":
        return JSONResponse(_LIBRARY_MANIFEST, media_type="application/manifest+json")
    raise HTTPException(status_code=421, detail="Misdirected request")


# ── Auth ──────────────────────────────────────────────────────────────

@app.get("/api/me")
async def me(identity: dict = Depends(require_auth)):
    return identity


# ── API: Account + personal Torznab keys ─────────────────────────────────

class AccountApiKeyRequest(BaseModel):
    name: str = "default"


def _account_user_id(identity: dict) -> str:
    return str(identity.get("id") or identity.get("username") or identity.get("email") or "anonymous")


def _new_user_api_key() -> str:
    return "ba_" + secrets.token_urlsafe(32)




def _key_prefix(api_key: str) -> str:
    return api_key[:10] + "..."


def _torznab_public_url(request: Request) -> str:
    scheme = request.url.scheme
    if proxy_provenance_valid(request):
        forwarded_scheme = (request.headers.get("x-forwarded-proto") or "").lower()
        if forwarded_scheme in {"http", "https"}:
            scheme = forwarded_scheme
    # Host is already constrained by the global surface allowlist middleware.
    return f"{scheme}://{request.headers['host']}/torznab/api"


def _public_api_key(row: dict | None) -> dict | None:
    if not row:
        return None
    return {
        "id": row.get("id"),
        "name": row.get("name") or "default",
        "prefix": row.get("key_prefix"),
        "createdAt": row.get("created_at"),
        "lastUsedAt": row.get("last_used_at"),
    }


def _account_payload(
    request: Request,
    identity: dict,
    row: dict | None,
    *,
    secret: str | None = None,
) -> dict:
    torznab_url = _torznab_public_url(request)
    payload = {
        "identity": {
            "id": identity.get("id"),
            "display": identity.get("display"),
            "username": identity.get("username"),
            "email": identity.get("email"),
            "method": identity.get("method"),
        },
        "apiKey": _public_api_key(row),
        "torznabUrl": torznab_url,
    }
    if secret:
        payload["apiKeySecret"] = secret
        payload["sampleUrl"] = f"{torznab_url}?t=caps&apikey={_urlquote(secret)}"
    return payload


@app.get("/api/account")
async def api_account(request: Request, identity: dict = Depends(require_auth)):
    row = await get_user_api_key(_account_user_id(identity))
    return _account_payload(request, identity, row)


@app.post("/api/account/api-key")
async def api_account_create_key(
    request: Request,
    body: AccountApiKeyRequest | None = None,
    identity: dict = Depends(require_auth),
):
    api_key = _new_user_api_key()
    row = await create_user_api_key(
        _account_user_id(identity),
        _hash_user_api_key(api_key),
        _key_prefix(api_key),
        (body.name if body else "default") or "default",
    )
    return _account_payload(request, identity, row, secret=api_key)


@app.delete("/api/account/api-key")
async def api_account_revoke_key(request: Request, identity: dict = Depends(require_auth)):
    await revoke_user_api_key(_account_user_id(identity))
    return _account_payload(request, identity, None)


# ── Page surfaces ─────────────────────────────────────────────────────
#
# Two front-ends share this one app + API, chosen by request Host:
#   • operator console (index.html)   — explicit OPERATOR_HOSTS only
#   • public library    (library.html) — explicit PUBLIC_LIBRARY_HOSTS only
# Both are auth-gated. The public shell is available to any authenticated user;
# the operator shell additionally requires an explicit verified operator role.
# Unknown/malformed Hosts receive 421 before routing. `/library` serves the
# public shell on either recognized host.


def _library_response(request: Request, identity: dict):
    return templates.TemplateResponse(
        request=request,
        name="library.html",
        context={
            "identity": identity,
            "asset_version": ASSET_VERSION,
            "app_switcher_script_url": settings.app_switcher_script_url.strip(),
            "library_brand": settings.library_brand,
            "app_version": __version__,
            # `/library` remains a convenient library-shell link on the
            # operator host, but public-only data widgets must stay absent
            # there because their API deliberately returns 404.
            "show_library_stats": _host_scope(request) == "public",
        },
    )


def _dashboard_response(request: Request, identity: dict):
    return templates.TemplateResponse(
        request=request,
        name="index.html",
        context={
            "identity": identity,
            "active_tab": "dashboard",
            "asset_version": ASSET_VERSION,
            "app_switcher_script_url": settings.app_switcher_script_url.strip(),
            "app_version": __version__,
        },
    )


@app.get("/")
async def root(request: Request, identity: dict = Depends(require_auth)):
    scope = _host_scope(request)
    if scope == "public":
        return _library_response(request, identity)
    if scope != "operator":
        raise HTTPException(status_code=421, detail="Misdirected request")
    if not identity.get("operator"):
        raise HTTPException(status_code=403, detail="Operator grant required")
    return _dashboard_response(request, identity)


@app.get("/library")
async def library_page(request: Request, identity: dict = Depends(require_auth)):
    return _library_response(request, identity)


# ── API: Public library headline stats ───────────────────────────────

def _library_stats_now() -> datetime:
    return datetime.now(timezone.utc)


def _utc_iso(value: datetime) -> str:
    return value.astimezone(timezone.utc).isoformat().replace("+00:00", "Z")


async def _collect_library_stats() -> dict:
    """Collect one truthful, source-aligned public-library count snapshot.

    The all-time and rolling-window counts are aliases of the same core
    torrentContent.search population. The rolling predicate is the immutable
    underlying torrent creation time, so classifier reprocessing and source
    refreshes cannot move a torrent into or out of the seven-day window.
    """
    window_end = _library_stats_now()
    window_start = window_end - timedelta(days=7)
    content_type_facets = {
        "contentType": {"filter": list(_LIBRARY_STATS_CONTENT_TYPES)}
    }
    variables = {
        "totalInput": {
            "limit": 0,
            "totalCount": True,
            "aggregationBudget": 0,
            "facets": content_type_facets,
        },
        "recentInput": {
            "limit": 0,
            "totalCount": True,
            # Exact on purpose: the planner estimate for this window was 2x off
            # (400,750 vs 194,253, 2026-09-19). Zero disables the estimate
            # budget. This is the slow half of the document (~7 s live), which
            # is why the collection runs in the background.
            "aggregationBudget": 0,
            "torrentCreatedAfter": _utc_iso(window_start),
            "torrentCreatedBefore": _utc_iso(window_end),
            "facets": content_type_facets,
        },
    }
    try:
        result = await gql.query(
            gql.LIBRARY_STATS, variables, timeout=_LIBRARY_STATS_QUERY_TIMEOUT_SECONDS
        )
    except Exception as exc:
        logger.warning("library stats GraphQL request failed: %s", type(exc).__name__)
        raise HTTPException(502, "Library statistics unavailable") from exc

    observed_at = _library_stats_now()
    if not isinstance(result, dict) or result.get("errors"):
        logger.warning("library stats GraphQL response contained errors")
        raise HTTPException(502, "Library statistics unavailable")

    data = result.get("data")
    if not isinstance(data, dict):
        raise HTTPException(502, "Library statistics unavailable")
    torrent_content = data.get("torrentContent")
    if not isinstance(torrent_content, dict):
        raise HTTPException(502, "Library statistics unavailable")
    total = torrent_content.get("total")
    recent = torrent_content.get("recent")
    if not isinstance(total, dict) or not isinstance(recent, dict):
        raise HTTPException(502, "Library statistics unavailable")

    total_count = _nonnegative_int(total.get("totalCount"))
    recent_count = _nonnegative_int(recent.get("totalCount"))
    total_is_estimate = total.get("totalCountIsEstimate")
    recent_is_estimate = recent.get("totalCountIsEstimate")
    if (
        total_count is None
        or recent_count is None
        or not isinstance(total_is_estimate, bool)
        or not isinstance(recent_is_estimate, bool)
        or total_is_estimate
        or recent_is_estimate
    ):
        # An estimated value is not silently presented as a real count.
        raise HTTPException(502, "Library statistics unavailable")

    return {
        "available": True,
        "totalReleases": total_count,
        "totalReleasesIsEstimate": total_is_estimate,
        "releasesAddedLast7Days": recent_count,
        "windowStart": _utc_iso(window_start),
        "windowEnd": _utc_iso(window_end),
        "observedAt": _utc_iso(observed_at),
    }


def _reset_library_stats_cache() -> None:
    global _LIBRARY_STATS_CACHE, _LIBRARY_STATS_INFLIGHT
    global _LIBRARY_STATS_RETRY_DELAY, _LIBRARY_STATS_RETRY_NOT_BEFORE
    if _LIBRARY_STATS_INFLIGHT is not None and not _LIBRARY_STATS_INFLIGHT.done():
        _LIBRARY_STATS_INFLIGHT.cancel()
    _LIBRARY_STATS_CACHE = None
    _LIBRARY_STATS_INFLIGHT = None
    _LIBRARY_STATS_RETRY_DELAY = 0.0
    _LIBRARY_STATS_RETRY_NOT_BEFORE = 0.0


def _publish_library_stats(task: asyncio.Task) -> None:
    global _LIBRARY_STATS_CACHE, _LIBRARY_STATS_INFLIGHT
    global _LIBRARY_STATS_RETRY_DELAY, _LIBRARY_STATS_RETRY_NOT_BEFORE
    if _LIBRARY_STATS_INFLIGHT is not task:
        return
    _LIBRARY_STATS_INFLIGHT = None
    try:
        snapshot = task.result()
    except BaseException:
        # Keep whatever snapshot exists; the next attempt waits out the backoff.
        _LIBRARY_STATS_RETRY_DELAY = min(
            max(_LIBRARY_STATS_RETRY_DELAY * 2, _LIBRARY_STATS_RETRY_BASE_SECONDS),
            _LIBRARY_STATS_CACHE_TTL_SECONDS,
        )
        _LIBRARY_STATS_RETRY_NOT_BEFORE = time.monotonic() + _LIBRARY_STATS_RETRY_DELAY
        return
    _LIBRARY_STATS_CACHE = (time.monotonic(), snapshot)
    _LIBRARY_STATS_RETRY_DELAY = 0.0
    _LIBRARY_STATS_RETRY_NOT_BEFORE = 0.0


def _start_library_stats_refresh() -> asyncio.Task:
    """Reuse the running collection or start one (process-wide single flight)."""
    global _LIBRARY_STATS_INFLIGHT
    task = _LIBRARY_STATS_INFLIGHT
    if task is None:
        task = asyncio.create_task(_collect_library_stats())
        _LIBRARY_STATS_INFLIGHT = task
        task.add_done_callback(_publish_library_stats)
    return task


async def _get_library_stats() -> dict:
    if _LIBRARY_STATS_CACHE is not None:
        cached_at, snapshot = _LIBRARY_STATS_CACHE
        now = time.monotonic()
        if (now - cached_at > _LIBRARY_STATS_CACHE_TTL_SECONDS
                and now >= _LIBRARY_STATS_RETRY_NOT_BEFORE):
            # Serve the last snapshot now; the refresh publishes behind it. A
            # failed refresh keeps the snapshot and backs off before retrying.
            _start_library_stats_refresh()
        return copy.deepcopy(snapshot)

    if time.monotonic() < _LIBRARY_STATS_RETRY_NOT_BEFORE:
        # The last collection failed and there is nothing to serve: honour the
        # backoff rather than launching another collection per request.
        return {"available": False}
    task = _start_library_stats_refresh()
    try:
        snapshot = await asyncio.wait_for(
            asyncio.shield(task), _LIBRARY_STATS_INLINE_WAIT_SECONDS
        )
    except TimeoutError:
        # Cold cache and the count is still running: say so instead of holding
        # the request until the edge 502s. The collection continues and
        # publishes for the next call.
        return {"available": False}
    return copy.deepcopy(snapshot)


@app.get("/api/library/stats")
async def api_library_stats(identity: dict = Depends(require_public_library)):
    """Public-library banner counts from a process cache refreshed in the
    background. Cold cache: a bounded wait, then `{"available": false}`; a
    failed collection with nothing cached is still a 502, and requests inside
    its backoff window answer `{"available": false}` without a new collection."""
    return await _get_library_stats()


# ── API: System stats ────────────────────────────────────────────────

_stats_query_is_legacy = False


def _is_graphql_validation_failure(response: dict | None) -> bool:
    """True when the core rejected the *document*, not the data — i.e. it does
    not know a field we asked for. Distinct from an execution error, which
    still returns a usable `data`."""
    return any(
        isinstance(e, dict)
        and (e.get("extensions") or {}).get("code") == "GRAPHQL_VALIDATION_FAILED"
        for e in ((response or {}).get("errors") or [])
    )


async def _probe_stats_graphql() -> dict[str, object]:
    # A single unknown field fails the whole query, so a core one field behind
    # would black out totalCount, the category breakdown and the core version
    # too. Retry once without the newest field and latch it: an old core costs
    # one extra round-trip, not one on every 30s poll. The estimate flag then
    # falls out of the response and `totalTorrentsIsEstimate` reports its own
    # "core does not report this" envelope, which is the honest answer.
    global _stats_query_is_legacy
    response = await gql.query(
        gql.SYSTEM_STATS_LEGACY if _stats_query_is_legacy else gql.SYSTEM_STATS
    )
    if not _stats_query_is_legacy and _is_graphql_validation_failure(response):
        response = await gql.query(gql.SYSTEM_STATS_LEGACY)
        # Only latch if the fallback actually validated — if it failed too the
        # mismatch is something else and the next poll should retry in full.
        _stats_query_is_legacy = not _is_graphql_validation_failure(response)
    data = (response or {}).get("data")
    search = (((data or {}).get("torrentContent") or {}).get("search"))
    if not isinstance(data, dict) or not isinstance(search, dict):
        raise TelemetryUnavailable("GraphQL stats unavailable")
    return {"data": data, "search": search}


async def _probe_prometheus() -> dict[str, object]:
    raw = await gql.fetch_metrics()
    if not raw.strip():
        raise TelemetryUnavailable("Prometheus endpoint unavailable")
    snapshot = _parse_prometheus_snapshot(raw)
    if not snapshot["lines"]:
        raise TelemetryUnavailable("Prometheus response contained no samples")
    return snapshot


async def _probe_evidence_count() -> int:
    # fetch_evidence_list intentionally soft-fails to an empty page for the
    # activity table. The stats snapshot needs the raw response so an outage is
    # not mislabeled as a real zero-row evidence corpus.
    response = await gql.query(
        gql.EVIDENCE_LIST,
        {"input": {"limit": 1, "offset": 0}},
    )
    listed = (((((response or {}).get("data") or {}).get("evidence") or {})
               .get("list")))
    if not isinstance(listed, dict) or "totalCount" not in listed:
        raise TelemetryUnavailable("Evidence aggregate unavailable")
    count = _nonnegative_int(listed.get("totalCount"))
    if count is None:
        raise TelemetryUnavailable("Evidence aggregate was invalid")
    return count


async def _probe_sidecar_db() -> bool:
    db = await get_db()
    cursor = await db.execute("SELECT 1")
    try:
        row = await cursor.fetchone()
    finally:
        await cursor.close()
    if not row or row[0] != 1:
        raise TelemetryUnavailable("SQLite health check failed")
    return True


def _metric_from_probe(
    probe,
    value,
    *,
    source: str,
    missing_error: str,
    missing_status: str = "unavailable",
) -> MetricEnvelope:
    """Envelope one derived value.

    ``missing_status`` exists so the frontend can tell "this source will never
    have it" (unavailable) from "this is a measured rate and the in-process
    baseline is still filling" (partial). Both carry value=None, but only the
    second one resolves itself by waiting, and rendering both as an em-dash
    made every fresh page load look broken for its first poll.
    """
    if not probe.ok:
        return probe.envelope(source=source)
    if value is None:
        return MetricEnvelope(
            value=None,
            status=missing_status,
            source=source,
            observed_at=probe.observed_at,
            error=missing_error,
        )
    return probe.envelope(value, source=source)


# Spend-to-date behaves like a counter — monotonic, resets with the core — so a
# rate can be sampled from it. It gets its OWN history rather than riding
# _METRIC_HISTORY because the two have opposite requirements: the dashboard's
# rate cards measure continuous plumbing and want a short window that reacts
# fast, while LLM stages are BURSTY. Junk purge judges in cycles roughly an
# hour apart, so over a 5-minute window its token counter does not move at all
# and its measured rate is a clean, confident, wrong 0.00 — which would then be
# summed into a total that reads comfortably under budget while actual spend is
# twice it. Observed live: a 12s window reported $8.57/mo when the true
# combined run rate was ~$20/mo, because junk purge contributed nothing.
_SPEND_HISTORY: list[tuple[float, dict[str, float]]] = []
# Must stay LONGER than _SPEND_IDLE_TRUST_SECONDS: the retained baseline is
# what lets a genuinely idle model ever reach the trust point instead of
# reading "measuring…" forever.
_SPEND_HISTORY_WINDOW_SECONDS = 7200.0
_SPEND_HISTORY_MAX = 256
# Below this, a model whose counter has not moved is "still measuring", not
# "costing nothing" — comfortably longer than a junk-purge cycle gap (~1h).
_SPEND_IDLE_TRUST_SECONDS = 5400.0


def _record_spend_snapshot(now_mono: float, values: dict[str, float]) -> None:
    _SPEND_HISTORY.append((now_mono, dict(values)))
    cutoff = now_mono - _SPEND_HISTORY_WINDOW_SECONDS
    _SPEND_HISTORY[:] = [e for e in _SPEND_HISTORY if e[0] >= cutoff]
    if len(_SPEND_HISTORY) > _SPEND_HISTORY_MAX:
        del _SPEND_HISTORY[: len(_SPEND_HISTORY) - _SPEND_HISTORY_MAX]


def _spend_rate_per_min(key: str, now_mono: float) -> tuple[float | None, float]:
    """(USD per minute, window seconds). None until a usable baseline exists.

    Mirrors _counter_rate_per_min's reset handling, and additionally returns
    the window so the caller can decide whether a flat counter means "idle" or
    "not measured long enough yet" — the distinction that keeps a bursty stage
    from being priced at zero.
    """
    if not _SPEND_HISTORY:
        return None, 0.0
    latest_ts, latest = _SPEND_HISTORY[-1]
    if latest_ts != now_mono or key not in latest:
        return None, 0.0
    samples = [(ts, snap[key]) for ts, snap in _SPEND_HISTORY if key in snap]
    if len(samples) < 2:
        return None, 0.0
    baseline_index = 0
    for i in range(1, len(samples)):
        if samples[i][1] < samples[i - 1][1]:
            baseline_index = i          # counter reset -> start after it
    base_ts, base_value = samples[baseline_index]
    elapsed = samples[-1][0] - base_ts
    if elapsed < _METRIC_RATE_MIN_INTERVAL_SECONDS:
        return None, elapsed
    return max((samples[-1][1] - base_value) / elapsed * 60.0, 0.0), elapsed


def _record_metric_snapshot(now_mono: float, metric_sum: dict[str, float]) -> None:
    _METRIC_HISTORY.append((now_mono, dict(metric_sum)))
    cutoff = now_mono - _METRIC_HISTORY_WINDOW_SECONDS
    _METRIC_HISTORY[:] = [entry for entry in _METRIC_HISTORY if entry[0] >= cutoff]
    if len(_METRIC_HISTORY) > _METRIC_HISTORY_MAX:
        del _METRIC_HISTORY[: len(_METRIC_HISTORY) - _METRIC_HISTORY_MAX]


def _metric_history_now() -> float:
    return time.monotonic()


def _rate_missing_status(metric_sum: dict[str, float], metric_name: str) -> str:
    """"partial" when the counter is being scraped but the rate baseline is
    still filling; "unavailable" when the core does not emit the counter at
    all. Only the first one fixes itself by waiting one more poll."""
    return "partial" if metric_name in metric_sum else "unavailable"


def _counter_rate_per_min(metric_name: str, now_mono: float) -> float | None:
    """Return a measured counter rate, or None until a valid baseline exists."""
    # The newest successful scrape is authoritative for family presence. Do
    # not filter back to an older sample when a counter disappears: that would
    # publish a stale historical rate as if it were observed now.
    if not _METRIC_HISTORY:
        return None
    latest_ts, latest_snapshot = _METRIC_HISTORY[-1]
    if latest_ts != now_mono or metric_name not in latest_snapshot:
        return None
    samples = [
        (ts, snapshot[metric_name])
        for ts, snapshot in _METRIC_HISTORY
        if metric_name in snapshot
    ]
    if len(samples) < 2:
        return None

    # Start after the most recent counter reset. This avoids comparing a
    # recycled core's counter with any pre-restart value that happens to be
    # numerically smaller than the new counter.
    baseline_index = 0
    for index in range(1, len(samples)):
        if samples[index][1] < samples[index - 1][1]:
            baseline_index = index
    baseline_ts, baseline_value = samples[baseline_index]
    current_ts, current_value = samples[-1]
    if current_ts > now_mono:
        return None
    elapsed = current_ts - baseline_ts
    if elapsed < _METRIC_RATE_MIN_INTERVAL_SECONDS:
        return None
    return max((current_value - baseline_value) / elapsed * 60.0, 0.0)


def _category_breakdown(search: dict[str, object]) -> list[dict[str, object]] | None:
    aggregations = search.get("aggregations")
    if not isinstance(aggregations, dict):
        return None
    rows = aggregations.get("contentType")
    if not isinstance(rows, list):
        return None
    result: list[dict[str, object]] = []
    for row in rows:
        if not isinstance(row, dict) or not isinstance(row.get("value"), str):
            return None
        count = _nonnegative_int(row.get("count"))
        if count is None:
            return None
        result.append({"category": row["value"], "count": count})
    return result


async def _collect_stats_snapshot() -> dict:
    """Return one bounded, source-aware telemetry snapshot.

    Flat compatibility fields remain for existing callers, but every value is
    derived from ``metrics`` and unknown values are null rather than fabricated
    zeroes. ``dhtPeerCount`` is intentionally null: the source gauge measures
    active requests, not routing-table peers.
    """
    observed_at = snapshot_timestamp()
    probes = await collect_bounded_probes(
        {
            "graphql": ("bitagent.graphql:SystemStats", _probe_stats_graphql),
            "prometheus": ("bitagent.prometheus:/metrics", _probe_prometheus),
            "evidence": ("bitagent.graphql:evidence.list", _probe_evidence_count),
            "db": ("bitagent-ui:sqlite", _probe_sidecar_db),
        },
        observed_at=observed_at,
        timeout_seconds=_STATS_PROBE_TIMEOUT_SECONDS,
        max_concurrency=_STATS_MAX_PARALLEL_PROBES,
    )

    graphql_probe = probes["graphql"]
    prometheus_probe = probes["prometheus"]
    evidence_probe = probes["evidence"]
    db_probe = probes["db"]

    gql_payload = graphql_probe.value if graphql_probe.ok else {}
    gql_data = gql_payload.get("data", {}) if isinstance(gql_payload, dict) else {}
    search = gql_payload.get("search", {}) if isinstance(gql_payload, dict) else {}
    total_torrents = (
        _nonnegative_int(search.get("totalCount"))
        if isinstance(search, dict) and "totalCount" in search
        else None
    )
    # The core answers this count from pg_class.reltuples once the table is
    # large enough that an exact count would time out — 2,311,600 is a planner
    # estimate, not a census. Rendering it to the last digit claimed a
    # precision the number does not have.
    total_torrents_estimated = (
        bool(search.get("totalCountIsEstimate"))
        if isinstance(search, dict) and "totalCountIsEstimate" in search
        else None
    )
    categories = _category_breakdown(search) if isinstance(search, dict) else None
    core_version = gql_data.get("version") if isinstance(gql_data, dict) else None
    if not isinstance(core_version, str) or not core_version:
        core_version = None

    prom_payload = prometheus_probe.value if prometheus_probe.ok else {}
    metric_sum = prom_payload.get("sum", {}) if isinstance(prom_payload, dict) else {}
    metric_lines = prom_payload.get("lines", {}) if isinstance(prom_payload, dict) else {}
    if not isinstance(metric_sum, dict):
        metric_sum = {}
    if not isinstance(metric_lines, dict):
        metric_lines = {}

    def prom_value(name: str) -> float | None:
        return metric_sum.get(name) if name in metric_sum else None

    now_mono = _metric_history_now()
    if prometheus_probe.ok:
        _record_metric_snapshot(now_mono, metric_sum)

    dht_request_concurrency = _nonnegative_int(
        prom_value("bitagent_dht_client_request_concurrency")
    )
    successful_dht_rate = (
        _counter_rate_per_min(
            "bitagent_dht_client_request_success_total", now_mono
        )
        if prometheus_probe.ok
        else None
    )
    throughput = (
        _counter_rate_per_min("bitagent_contentfilter_examined_total", now_mono)
        if prometheus_probe.ok
        else None
    )

    hits = prom_value("bitagent_classifier_llm_cache_hits_total")
    misses = prom_value("bitagent_classifier_llm_cache_misses_total")
    cache_hit_ratio = (
        hits / (hits + misses)
        if hits is not None and misses is not None and (hits + misses) > 0
        else None
    )

    grab_success = _nonnegative_int(prom_value("bitagent_dashstats_grab_success"))
    grab_failure = _nonnegative_int(prom_value("bitagent_dashstats_grab_failure"))
    grab_pending = _nonnegative_int(prom_value("bitagent_dashstats_grab_pending"))
    grab_rate = (
        grab_success / (grab_success + grab_failure)
        if grab_success is not None
        and grab_failure is not None
        and (grab_success + grab_failure) > 0
        else None
    )

    match_total = _nonnegative_int(
        prom_value("bitagent_dashstats_match_video_total_30d")
    )
    match_matched = _nonnegative_int(
        prom_value("bitagent_dashstats_match_video_matched_30d")
    )
    match_rate = (
        match_matched / match_total
        if match_matched is not None and match_total is not None and match_total > 0
        else None
    )

    observations_alive = _nonnegative_int(
        _labeled_metric_sum(
            metric_lines, "bitagent_liveness_observations_total", **{"class": "alive"}
        )
    )
    observations_suspect = _nonnegative_int(
        _labeled_metric_sum(
            metric_lines, "bitagent_liveness_observations_total", **{"class": "suspect"}
        )
    )
    blacklist_size = _nonnegative_int(prom_value("bitagent_liveness_blacklist_size"))
    excluded_total = _nonnegative_int(
        prom_value("bitagent_liveness_torznab_excluded_total")
    )
    block_rate = (
        _counter_rate_per_min("bitagent_liveness_torznab_excluded_total", now_mono)
        if prometheus_probe.ok
        else None
    )
    revalid_alive = _nonnegative_int(
        _labeled_metric_sum(
            metric_lines,
            "bitagent_liveness_revalidations_total",
            outcome="alive_again",
        )
    )
    revalid_dead = _nonnegative_int(
        _labeled_metric_sum(
            metric_lines,
            "bitagent_liveness_revalidations_total",
            outcome="still_dead",
        )
    )
    revalidation_rate = (
        revalid_alive / (revalid_alive + revalid_dead)
        if revalid_alive is not None
        and revalid_dead is not None
        and (revalid_alive + revalid_dead) > 0
        else None
    )

    def prom_source(name: str) -> str:
        return f"bitagent.prometheus:{name}"

    metrics: dict[str, MetricEnvelope] = {
        "graphqlReachable": graphql_probe.envelope(True),
        "metricsReachable": prometheus_probe.envelope(True),
        "evidenceReachable": evidence_probe.envelope(True),
        "sidecarDbReachable": db_probe.envelope(True),
        "totalTorrents": _metric_from_probe(
            graphql_probe,
            total_torrents,
            source="bitagent.graphql:torrentContent.search.totalCount",
            missing_error="GraphQL totalCount missing or invalid",
        ),
        "totalTorrentsIsEstimate": _metric_from_probe(
            graphql_probe,
            total_torrents_estimated,
            source="bitagent.graphql:torrentContent.search.totalCountIsEstimate",
            missing_error="Core does not report whether totalCount is an estimate",
        ),
        "categoryBreakdown": _metric_from_probe(
            graphql_probe,
            categories,
            source="bitagent.graphql:torrentContent.search.aggregations.contentType",
            missing_error="GraphQL content-type aggregation missing or invalid",
        ),
        "coreVersion": _metric_from_probe(
            graphql_probe,
            core_version,
            source="bitagent.graphql:version",
            missing_error="Core version not reported",
        ),
        "totalEvidence": _metric_from_probe(
            evidence_probe,
            evidence_probe.value if evidence_probe.ok else None,
            source="bitagent.graphql:evidence.list.totalCount",
            missing_error="Evidence totalCount missing or invalid",
        ),
        "dhtRequestConcurrency": _metric_from_probe(
            prometheus_probe,
            dht_request_concurrency,
            source=prom_source("bitagent_dht_client_request_concurrency"),
            missing_error="DHT request concurrency metric not emitted",
        ),
        "dhtPeerCount": unavailable_metric(
            source="bitagent-core:unexposed",
            observed_at=observed_at,
            error="No routing-table peer count is exposed; request concurrency is not peers",
        ),
        "successfulDhtRequestsPerMin": _metric_from_probe(
            prometheus_probe,
            _nonnegative_rate(successful_dht_rate),
            source=prom_source("bitagent_dht_client_request_success_total"),
            missing_error=(
                "Successful DHT request counter not emitted"
                if "bitagent_dht_client_request_success_total" not in metric_sum
                else "Successful DHT request rate needs two valid samples at least 10s apart"
            ),
            missing_status=_rate_missing_status(
                metric_sum, "bitagent_dht_client_request_success_total"
            ),
        ),
        "indexerThroughput": _metric_from_probe(
            prometheus_probe,
            _nonnegative_rate(throughput),
            source=prom_source("bitagent_contentfilter_examined_total"),
            missing_error=(
                "Indexer throughput counter not emitted"
                if "bitagent_contentfilter_examined_total" not in metric_sum
                else "Indexer throughput needs two valid samples at least 10s apart"
            ),
            missing_status=_rate_missing_status(
                metric_sum, "bitagent_contentfilter_examined_total"
            ),
        ),
        "cacheHitRatio": _metric_from_probe(
            prometheus_probe,
            cache_hit_ratio,
            source="bitagent.prometheus:classifier_llm_cache_hits/misses_total",
            missing_error="Cache ratio metric family absent or has no observations",
        ),
        "grabSuccessRate": _metric_from_probe(
            prometheus_probe,
            grab_rate,
            source="bitagent.prometheus:bitagent_dashstats_grab_success/failure",
            missing_error="Grab success ratio absent or has no resolved grabs",
        ),
        "grabSuccessCount": _metric_from_probe(
            prometheus_probe,
            grab_success,
            source=prom_source("bitagent_dashstats_grab_success"),
            missing_error="Grab success metric not emitted",
        ),
        "grabFailureCount": _metric_from_probe(
            prometheus_probe,
            grab_failure,
            source=prom_source("bitagent_dashstats_grab_failure"),
            missing_error="Grab failure metric not emitted",
        ),
        "grabPendingCount": _metric_from_probe(
            prometheus_probe,
            grab_pending,
            source=prom_source("bitagent_dashstats_grab_pending"),
            missing_error="Grab pending metric not emitted",
        ),
        "matchRate30d": _metric_from_probe(
            prometheus_probe,
            match_rate,
            source="bitagent.prometheus:bitagent_dashstats_match_video_*_30d",
            missing_error="Match rate absent or has no eligible torrents",
        ),
        "matchMatched30d": _metric_from_probe(
            prometheus_probe,
            match_matched,
            source=prom_source("bitagent_dashstats_match_video_matched_30d"),
            missing_error="Matched-video metric not emitted",
        ),
        "matchTotal30d": _metric_from_probe(
            prometheus_probe,
            match_total,
            source=prom_source("bitagent_dashstats_match_video_total_30d"),
            missing_error="Match-total metric not emitted",
        ),
        "livenessBlacklistSize": _metric_from_probe(
            prometheus_probe,
            blacklist_size,
            source=prom_source("bitagent_liveness_blacklist_size"),
            missing_error="Liveness blacklist metric not emitted",
        ),
        "livenessBlockRatePerMin": _metric_from_probe(
            prometheus_probe,
            _nonnegative_rate(block_rate),
            source=prom_source("bitagent_liveness_torznab_excluded_total"),
            missing_error=(
                "Liveness exclusion counter not emitted"
                if "bitagent_liveness_torznab_excluded_total" not in metric_sum
                else "Block rate needs two valid samples at least 10s apart"
            ),
            missing_status=_rate_missing_status(
                metric_sum, "bitagent_liveness_torznab_excluded_total"
            ),
        ),
        "livenessTotalExcluded": _metric_from_probe(
            prometheus_probe,
            excluded_total,
            source=prom_source("bitagent_liveness_torznab_excluded_total"),
            missing_error="Liveness exclusion metric not emitted",
        ),
        "livenessObservationsAlive": _metric_from_probe(
            prometheus_probe,
            observations_alive,
            source=prom_source('bitagent_liveness_observations_total{class="alive"}'),
            missing_error="Alive-observation metric family not emitted",
        ),
        "livenessObservationsSuspect": _metric_from_probe(
            prometheus_probe,
            observations_suspect,
            source=prom_source('bitagent_liveness_observations_total{class="suspect"}'),
            missing_error="Suspect-observation metric family not emitted",
        ),
        "livenessRevalidationsAlive": _metric_from_probe(
            prometheus_probe,
            revalid_alive,
            source=prom_source(
                'bitagent_liveness_revalidations_total{outcome="alive_again"}'
            ),
            missing_error="Revalidation metric family not emitted",
        ),
        "livenessRevalidationsDead": _metric_from_probe(
            prometheus_probe,
            revalid_dead,
            source=prom_source(
                'bitagent_liveness_revalidations_total{outcome="still_dead"}'
            ),
            missing_error="Revalidation metric family not emitted",
        ),
        "livenessRecoveryRate": _metric_from_probe(
            prometheus_probe,
            revalidation_rate,
            source="bitagent.prometheus:bitagent_liveness_revalidations_total",
            missing_error="Recovery ratio absent or has no revalidations",
        ),
        "uptimeSeconds": MetricEnvelope(
            value=max(int(time.time() - _START_TIME), 1),
            status="ok",
            source="bitagent-ui:process",
            observed_at=observed_at,
        ),
        "lastCrawlAt": unavailable_metric(
            source="bitagent-core:unexposed",
            observed_at=observed_at,
            error="Core exposes no last-crawl timestamp",
        ),
    }

    def value(name: str):
        return metrics[name].value

    return {
        "snapshot": {
            "observed_at": observed_at,
            "stale": False,
            "cached": False,
            "age_seconds": 0.0,
            "cache_ttl_seconds": _STATS_SNAPSHOT_TTL_SECONDS,
            "probe_timeout_seconds": _STATS_PROBE_TIMEOUT_SECONDS,
            "max_parallel_probes": _STATS_MAX_PARALLEL_PROBES,
        },
        "metrics": {name: envelope.as_dict() for name, envelope in metrics.items()},
        # Compatibility fields are views of the typed envelopes. Null means
        # unavailable/unknown; zero appears only when the source emitted zero.
        "core": {
            "graphql": graphql_probe.ok,
            "metrics": prometheus_probe.ok,
            "db": db_probe.ok,
        },
        "totalTorrents": value("totalTorrents"),
        "totalReleases": value("totalTorrents"),
        "totalEvidence": value("totalEvidence"),
        "dhtPeerCount": value("dhtPeerCount"),
        "dhtRequestConcurrency": value("dhtRequestConcurrency"),
        "successfulDhtRequestsPerMin": value("successfulDhtRequestsPerMin"),
        "crawlRatePerMin": value("successfulDhtRequestsPerMin"),
        "indexerThroughput": value("indexerThroughput"),
        "cacheHitRatio": value("cacheHitRatio"),
        "uptimeSeconds": value("uptimeSeconds"),
        "lastCrawlAt": value("lastCrawlAt"),
        "categoryBreakdown": value("categoryBreakdown"),
        "version": value("coreVersion"),
        "grabSuccess": {
            "rate": value("grabSuccessRate"),
            "success": value("grabSuccessCount"),
            "failure": value("grabFailureCount"),
            "pending": value("grabPendingCount"),
        },
        "matchRate": {
            "rate": value("matchRate30d"),
            "matched": value("matchMatched30d"),
            "total": value("matchTotal30d"),
        },
        "liveness": {
            "blacklistSize": value("livenessBlacklistSize"),
            "blockRatePerMin": value("livenessBlockRatePerMin"),
            "totalExcluded": value("livenessTotalExcluded"),
            "observationsAlive": value("livenessObservationsAlive"),
            "observationsSuspect": value("livenessObservationsSuspect"),
            "revalidations": {
                "aliveAgain": value("livenessRevalidationsAlive"),
                "stillDead": value("livenessRevalidationsDead"),
                "recoveryRate": value("livenessRecoveryRate"),
            },
        },
    }


def _stats_cache_now() -> float:
    return time.monotonic()


def _reset_stats_snapshot_cache() -> None:
    """Reset process-local snapshot state (used by tests and app lifecycle)."""
    global _STATS_SNAPSHOT_CACHE, _STATS_SNAPSHOT_INFLIGHT
    if _STATS_SNAPSHOT_INFLIGHT is not None and not _STATS_SNAPSHOT_INFLIGHT.done():
        _STATS_SNAPSHOT_INFLIGHT.cancel()
    _STATS_SNAPSHOT_CACHE = None
    _STATS_SNAPSHOT_INFLIGHT = None


def _present_stats_snapshot(snapshot: dict, *, cached: bool, age_seconds: float) -> dict:
    """Return an isolated response with explicit cache/freshness metadata."""
    response = copy.deepcopy(snapshot)
    age = max(float(age_seconds), 0.0)
    stale = age > _STATS_SNAPSHOT_TTL_SECONDS
    metadata = response.setdefault("snapshot", {})
    metadata.update({
        "cached": bool(cached),
        "age_seconds": round(age, 3),
        "cache_ttl_seconds": _STATS_SNAPSHOT_TTL_SECONDS,
        "stale": stale,
    })
    for metric in response.get("metrics", {}).values():
        metric["stale"] = stale
        if stale and metric.get("status") == "ok":
            metric["status"] = "stale"
    return response


def _publish_stats_snapshot(task: asyncio.Task) -> None:
    """Publish one completed collection even if its initiating client left."""
    global _STATS_SNAPSHOT_CACHE, _STATS_SNAPSHOT_INFLIGHT
    if _STATS_SNAPSHOT_INFLIGHT is not task:
        return
    try:
        snapshot = task.result()
    except BaseException:
        _STATS_SNAPSHOT_INFLIGHT = None
        return
    _STATS_SNAPSHOT_CACHE = (_stats_cache_now(), snapshot)
    _STATS_SNAPSHOT_INFLIGHT = None


async def _get_stats_snapshot() -> dict:
    """Serve a short-lived process-wide snapshot with request singleflight."""
    global _STATS_SNAPSHOT_INFLIGHT
    now = _stats_cache_now()
    cached_entry = _STATS_SNAPSHOT_CACHE
    if cached_entry is not None:
        cached_at, snapshot = cached_entry
        age = max(now - cached_at, 0.0)
        if age <= _STATS_SNAPSHOT_TTL_SECONDS:
            return _present_stats_snapshot(snapshot, cached=True, age_seconds=age)

    task = _STATS_SNAPSHOT_INFLIGHT
    if task is None:
        task = asyncio.create_task(_collect_stats_snapshot())
        _STATS_SNAPSHOT_INFLIGHT = task
        task.add_done_callback(_publish_stats_snapshot)
    try:
        snapshot = await asyncio.shield(task)
    except Exception:
        # The done callback normally clears this first; this branch makes the
        # retry guarantee explicit even if callback scheduling changes.
        if _STATS_SNAPSHOT_INFLIGHT is task:
            _STATS_SNAPSHOT_INFLIGHT = None
        raise
    return _present_stats_snapshot(snapshot, cached=False, age_seconds=0.0)


@app.get("/api/stats")
async def api_stats(identity: dict = Depends(require_operator)):
    """Return one bounded, source-aware, process-singleflight snapshot."""
    return await _get_stats_snapshot()


@app.get("/api/metrics")
async def api_metrics(identity: dict = Depends(require_operator)):
    raw = await gql.fetch_metrics()
    lines = {}
    for line in raw.strip().splitlines():
        if line.startswith("#"):
            continue
        parts = line.split(" ", 1)
        if len(parts) == 2:
            lines[parts[0]] = parts[1]
    return lines


def _reset_indexer_stats_cache() -> None:
    global _INDEXER_STATS_CACHE, _INDEXER_STATS_INFLIGHT
    for task in _INDEXER_STATS_INFLIGHT.values():
        if not task.done():
            task.cancel()
    _INDEXER_STATS_CACHE = {}
    _INDEXER_STATS_INFLIGHT = {}


def _publish_indexer_stats(days: int, task: asyncio.Task) -> None:
    if _INDEXER_STATS_INFLIGHT.get(days) is not task:
        return
    _INDEXER_STATS_INFLIGHT.pop(days, None)
    try:
        result = task.result()
    except BaseException:
        return
    # Cache only a usable snapshot; a transient "available: False" self-heals on
    # the next poll instead of sticking for the whole TTL.
    if isinstance(result, dict) and result.get("available"):
        _INDEXER_STATS_CACHE[days] = (time.monotonic(), result)


async def _get_indexer_stats(days: int) -> object:
    """Short-TTL per-window snapshot with request singleflight (mirrors
    _get_library_stats). `days` is already clamped by the caller."""
    entry = _INDEXER_STATS_CACHE.get(days)
    if entry is not None:
        cached_at, result = entry
        if max(time.monotonic() - cached_at, 0.0) <= _INDEXER_STATS_CACHE_TTL_SECONDS:
            return copy.deepcopy(result)

    task = _INDEXER_STATS_INFLIGHT.get(days)
    if task is None:
        task = asyncio.create_task(gql.fetch_indexer_stats(days))
        _INDEXER_STATS_INFLIGHT[days] = task
        task.add_done_callback(lambda t, d=days: _publish_indexer_stats(d, t))
    return copy.deepcopy(await asyncio.shield(task))


@app.get("/api/indexer-stats")
async def api_indexer_stats(
    days: int = 30, identity: dict = Depends(require_operator)
):
    """North-star KPI: *arr grab win rate by indexer, proxied from the core's
    evidence.indexerStats GraphQL aggregate. Counts only — no release names."""
    return await _get_indexer_stats(max(1, min(days, 365)))


# ── API: AI matcher (LLM TMDB-matcher observability) ─────────────────
#
# The core's classifier has an LLM fallback matcher (BitAgent
# internal/classifier/llmmatch): stage-1 "extract" pulls a clean title/year
# from the raw torrent name, stage-2 "rerank" picks the right TMDB candidate.
# It runs in shadow or live mode and emits bitagent_classifier_llm_match_*
# Prometheus counters. This endpoint reshapes those into a scorecard.
#
# The endpoint also scores the other two LLM stages the core runs (content
# filter, junk purge), which live under entirely different metric prefixes —
# see _llm_stage_scorecards.
#
# Alt-title backfill coverage IS surfaced, via the core's
# bitagent_dashstats_alt_title_content_* gauges (core >= v0.22.0). The older
# note here said the opposite — it predates those gauges, when the count only
# existed in bitmagnet's Postgres content_attributes table and this UI, being
# stateless with no direct DB access, genuinely could not reach it.

import re as _label_re_mod

_LLM_MATCH_PREFIX = "bitagent_classifier_llm_match_"
_LABEL_RE = _label_re_mod.compile(r'(\w+)="([^"]*)"')


def _parse_prom_labels(label_blob: str) -> dict:
    """Parse the `k="v",k2="v2"}` tail of a Prometheus exposition key into a
    dict. Values in this exposition never contain escaped quotes, so a simple
    scan is sufficient."""
    return {m.group(1): m.group(2) for m in _LABEL_RE.finditer(label_blob)}


def _stage_metric(
    label: str,
    value: float | None,
    fmt: str,
    detail: str,
    *,
    tone: str = "neutral",
    help_text: str = "",
) -> dict:
    """One row in a stage scorecard. ``tone`` is decided here because the
    server is the only side that knows whether a high number is good."""
    return {
        "label": label,
        "value": value,
        "format": fmt,
        "detail": detail,
        "tone": tone if value is not None else "neutral",
        "help": help_text,
    }


def _stage_status(key: str, label: str, tone: str, detail: str) -> dict:
    """Operational state inferred from counters, with the inference named.

    Prefer resolved configuration gauges where available. Older core versions
    require counter-based inference; say when disabled and idle are ambiguous.
    """
    return {"key": key, "label": label, "tone": tone, "detail": detail}


def _tone_from(value: float | None, good: float, bad: float) -> str:
    """Tone for a ratio where higher is better. ``good``/``bad`` are the
    thresholds; pass them inverted (good < bad) for lower-is-better ratios."""
    if value is None:
        return "neutral"
    if good >= bad:
        return "good" if value >= good else "bad" if value < bad else "warn"
    return "good" if value <= good else "bad" if value > bad else "warn"


def _llm_stage_scorecards(
    *,
    spend,
    telemetry_available,
    all_fam,
    sum_all,
    labels_of,
    avg_latency,
    ratio,
    matcher_matches,
    matcher_extract,
    matcher_rerank,
    matcher_gate_rejects,
    matcher_latency,
) -> list[dict]:
    """One uniform scorecard per LLM stage BitAgent actually runs.

    Three distinct models do three distinct jobs and only one of them was ever
    on this page. Every figure below is derived from a metric the core emits —
    where an older core emits nothing, the field is null with a note, never
    a zero. New matcher configuration and token counters take precedence over
    historical inference, while older core versions remain readable.
    """
    def count(name: str, **want: str) -> float | None:
        """Absent family -> None. Present family -> its sum, INCLUDING a real
        zero.

        `sum_all` cannot tell those apart — both come back 0 — and collapsing
        them breaks this tab's contract in both directions: an uninstrumented
        counter renders as a healthy "0", and a genuine "0 quarantined" (which
        is exactly what an operator wants to confirm) renders as "no data".
        A label that is missing from a family that IS present is a true zero:
        the counter is registered, nothing has incremented that label yet.
        """
        if name not in all_fam:
            return None
        return sum_all(name, **want)

    stages: list[dict] = []

    # ── Stage 1: TMDB matcher ────────────────────────────────────────
    extract_calls = matcher_extract.get("total", 0)
    rerank_calls = matcher_rerank.get("total", 0)
    matcher_calls = extract_calls + rerank_calls
    dispatched_calls = count("bitagent_classifier_llm_match_calls_total")
    if dispatched_calls is not None:
        matcher_calls = int(dispatched_calls)
    matcher_models = sorted(set(
        labels_of("bitagent_classifier_llm_match_calls_total", "model")
        + labels_of("bitagent_classifier_llm_match_info", "model")
    ))
    matcher_tokens_present = "bitagent_classifier_llm_match_tokens_total" in all_fam
    matcher_in = count("bitagent_classifier_llm_match_tokens_total", kind="input")
    matcher_out = count("bitagent_classifier_llm_match_tokens_total", kind="output")
    matcher_config = {
        key: count("bitagent_classifier_llm_match_config", setting=key)
        for key in ("enabled", "live", "require_source_title", "daily_call_limit", "monthly_call_limit")
    }
    matcher_usage_missing = count("bitagent_classifier_llm_match_usage_missing_total")
    matcher_errors = count("bitagent_classifier_llm_match_call_errors_total")
    live_attached = (matcher_matches.get("live") or {}).get("total", 0)
    shadow_attached = (matcher_matches.get("shadow") or {}).get("total", 0)
    rerank_picks = matcher_rerank.get("match", 0)
    # `rerank_total` carries no mode label, so its picks span live AND shadow.
    # Dividing live-only attaches by that denominator understates the yield
    # whenever any shadow rerank has run. Both sides are therefore mode-
    # agnostic: every pick the model made, over every attachment decision it
    # produced, applied or merely recorded.
    attached_any_mode = live_attached + shadow_attached
    # More attaches than picks means the two counters disagree — they are
    # incremented on different code paths and only a core bug or a mid-process
    # counter reset can produce it. Render nothing rather than a yield above
    # 100%, which reads as a broken page and hides the real fault.
    attach_yield = (
        ratio(attached_any_mode, rerank_picks)
        if attached_any_mode <= rerank_picks else None
    )
    # The identity gate is the check ON the model, so its post-pick rejections
    # are the closest thing to a measured matcher error rate. Pre-LLM gates
    # (plausibility/privacy/size) reject before any call and are excluded.
    post_pick_rejects = sum(
        matcher_gate_rejects.get(g, 0)
        for g in ("candidate_title", "candidate_year", "candidate_ambiguous", "resolved_year", "source_title", "source_year")
    )
    matcher_latency_total = None
    lat_counts = sum(
        (matcher_latency.get(s) or {}).get("count", 0) for s in ("extract", "rerank")
    )
    if lat_counts > 0:
        matcher_latency_total = sum(
            (matcher_latency.get(s) or {}).get("avgSeconds", 0.0)
            * (matcher_latency.get(s) or {}).get("count", 0)
            for s in ("extract", "rerank")
        ) / lat_counts

    matcher_observed = matcher_calls > 0 or attached_any_mode > 0
    matcher_instrumented = any(
        name.startswith(_LLM_MATCH_PREFIX) for name in all_fam
    )
    if not telemetry_available:
        matcher_status = _stage_status(
            "unavailable", "Unavailable", "danger",
            "Core metrics could not be read, so matcher state is unknown.",
        )
    elif not matcher_instrumented:
        matcher_status = _stage_status(
            "unavailable", "Not instrumented", "neutral",
            "The core emitted no matcher metric family, so enabled state and activity are unknown.",
        )
    elif matcher_config["enabled"] == 0:
        matcher_status = _stage_status("inactive", "Disabled", "neutral", "The core reports matcher enabled=false.")
    elif matcher_config["enabled"] == 1:
        live = matcher_config["live"] == 1
        matcher_status = _stage_status(
            "active" if live else "shadow", "Live configured" if live else "Shadow configured",
            "success" if live and live_attached else "info" if live else "warning",
            f"{matcher_calls:,} outbound requests and {live_attached:,} live attachment decisions since core boot. "
            + ("Independent source-title checks required. " if matcher_config["require_source_title"] else "")
            + f"Request caps: {int(matcher_config['daily_call_limit'] or 0):,}/day, "
              f"{int(matcher_config['monthly_call_limit'] or 0):,}/month.",
        )
    elif live_attached > 0 and shadow_attached > 0:
        matcher_status = _stage_status(
            "mixed", "Mixed history", "warning",
            f"{live_attached:,} live and {shadow_attached:,} shadow attaches were recorded "
            "since core boot; telemetry does not expose the current configured mode.",
        )
    elif live_attached > 0:
        matcher_status = _stage_status(
            "active", "Live effects observed", "success",
            f"{live_attached:,} live attaches were recorded since core boot.",
        )
    elif shadow_attached > 0:
        matcher_status = _stage_status(
            "shadow", "Shadow observed", "warning",
            f"{shadow_attached:,} shadow attaches and no live attaches were recorded "
            "since core boot.",
        )
    elif matcher_calls > 0:
        matcher_status = _stage_status(
            "active", "Calls observed", "info",
            f"{matcher_calls:,} calls and no attachments were recorded since core boot.",
        )
    else:
        matcher_status = _stage_status(
            "inactive", "No activity observed", "neutral",
            "No calls or attachments were recorded since core boot. This is consistent "
            "with the matcher being disabled, but the core exposes no enabled-state gauge.",
        )

    stages.append({
        "id": "matcher",
        "name": "TMDB Matcher",
        "role": "Attaches TMDB ids to torrent names the heuristics cannot map",
        "available": matcher_observed,
        "status": matcher_status,
        "model": ", ".join(matcher_models) or None,
        "modelNote": None if matcher_models else "core emits no model label on matcher metrics",
        "config": matcher_config,
        "metrics": [
            _stage_metric(
                "Attach yield", attach_yield, "percent",
                f"{attached_any_mode:,} attached of {rerank_picks:,} picked"
                + (f" ({shadow_attached:,} shadow)" if shadow_attached else "")
                + ("" if attach_yield is not None or not rerank_picks
                   else " — counters disagree"),
                tone=_tone_from(attach_yield, 0.75, 0.5),
                help_text="Of the candidates the model confidently picked, the share that survived the identity gate and became an attachment. Counts live and shadow together because the rerank counter behind the denominator does not separate them; in live-only operation the two are the same number.",
            ),
            _stage_metric(
                "Rejected picks", ratio(post_pick_rejects, rerank_picks), "percent",
                f"{post_pick_rejects:,} of {rerank_picks:,} picks failed the identity gate",
                tone=_tone_from(ratio(post_pick_rejects, rerank_picks), 0.15, 0.3),
                help_text="The matcher's only measured quality signal: confident picks the post-rerank identity gate refused (title/year/ambiguity). Roughly half of these rejections are themselves wrong, so this is a recall cost, not pure precision.",
            ),
            _stage_metric(
                "Extract success", ratio(matcher_extract.get("ok", 0), extract_calls),
                "percent",
                f"{matcher_extract.get('ok', 0):,} ok / {matcher_extract.get('empty', 0):,} empty / {matcher_extract.get('error', 0):,} error",
                tone=_tone_from(ratio(matcher_extract.get("ok", 0), extract_calls), 0.6, 0.35),
                help_text="Stage-1: share of calls where the model pulled a usable title/year out of the raw torrent name.",
            ),
            _stage_metric(
                "Calls",
                float(matcher_calls)
                if (dispatched_calls is not None or "bitagent_classifier_llm_match_extract_total" in all_fam
                    or "bitagent_classifier_llm_match_rerank_total" in all_fam)
                else None,
                "number",
                "admitted outbound HTTP requests" if dispatched_calls is not None else f"{extract_calls:,} extract + {rerank_calls:,} rerank",
                help_text="Total LLM calls across both matcher stages since core boot.",
            ),
            _stage_metric(
                "Avg latency", matcher_latency_total, "seconds",
                " / ".join(
                    f"{s} {(matcher_latency.get(s) or {}).get('avgSeconds', 0.0):.1f}s"
                    for s in ("extract", "rerank")
                ),
                tone=_tone_from(matcher_latency_total, 2.0, 5.0),
                help_text="Call-weighted mean across both stages, from the histogram sum/count pair.",
            ),
            _stage_metric(
                "Call errors", ratio(matcher_errors, matcher_calls) if matcher_errors is not None and dispatched_calls is not None else None,
                "percent", f"{int(matcher_errors or 0):,} call/response errors" if dispatched_calls is not None else "outbound denominator not instrumented",
                help_text="Transport, HTTP, and response parsing errors divided by admitted requests. Withheld calls are counted separately.",
            ),
            _stage_metric("Budget skips", count("bitagent_classifier_llm_match_budget_skips_total"), "number",
                          "requests withheld by allowance or database availability"),
        ],
        "tokens": {"input": int(matcher_in or 0), "output": int(matcher_out or 0)} if matcher_tokens_present else None,
        "tokensNote": (f"Usage missing for {int(matcher_usage_missing):,} responses; spend is partial." if matcher_usage_missing
                       else None if matcher_tokens_present else "core emits no token counter for the matcher"),
        "unit": {"label": "live attachment decision", "count": live_attached},
    })

    # ── Stage 2: content filter ──────────────────────────────────────
    cf_ok = sum_all("bitagent_contentfilter_llm_calls_total", ok="true")
    cf_err = sum_all("bitagent_contentfilter_llm_calls_total", ok="false")
    cf_calls = cf_ok + cf_err
    cf_hits = sum_all("bitagent_contentfilter_llm_cache_hits_total")
    cf_misses = sum_all("bitagent_contentfilter_llm_cache_misses_total")
    cf_deferred = count("bitagent_contentfilter_llm_deferred_total")
    cf_budget = count("bitagent_contentfilter_llm_budget_exhausted_total")
    cf_llm_drops = sum_all("bitagent_contentfilter_drop_total", reason="llm_non_english")
    cf_live_drops = count("bitagent_contentfilter_drop_total")
    cf_drops_all = cf_live_drops or 0.0
    cf_would_drops = count("bitagent_contentfilter_would_drop_total")
    cf_examined = count("bitagent_contentfilter_examined_total")
    # Model label and token accounting landed in the core after this tab was
    # first written; both were previously reported as instrumentation gaps.
    cf_models = labels_of("bitagent_contentfilter_llm_tokens_total", "model")
    cf_tokens_present = "bitagent_contentfilter_llm_tokens_total" in all_fam
    cf_in = sum_all("bitagent_contentfilter_llm_tokens_total", kind="prompt")
    cf_out = sum_all("bitagent_contentfilter_llm_tokens_total", kind="completion")
    cf_latency = avg_latency(
        "bitagent_contentfilter_llm_call_duration_seconds_sum",
        "bitagent_contentfilter_llm_call_duration_seconds_count",
    )

    cf_instrumented = any(
        name.startswith("bitagent_contentfilter_") for name in all_fam
    )
    if not telemetry_available:
        cf_status = _stage_status(
            "unavailable", "Unavailable", "danger",
            "Core metrics could not be read, so content-filter state is unknown.",
        )
    elif not cf_instrumented:
        cf_status = _stage_status(
            "unavailable", "Not instrumented", "neutral",
            "The core emitted no content-filter metric family, so state is unknown.",
        )
    elif (cf_live_drops or 0) > 0 and (cf_would_drops or 0) > 0:
        cf_status = _stage_status(
            "mixed", "Mixed history", "warning",
            f"{int(cf_live_drops or 0):,} enforced drops and "
            f"{int(cf_would_drops or 0):,} shadow would-drops were recorded since boot; "
            "telemetry does not expose the current configured mode.",
        )
    elif (cf_live_drops or 0) > 0:
        cf_status = _stage_status(
            "active", "Live effects observed", "success",
            f"{int(cf_live_drops or 0):,} enforced drops were recorded since core boot.",
        )
    elif (cf_would_drops or 0) > 0:
        cf_status = _stage_status(
            "shadow", "Shadow", "warning",
            f"{int(cf_would_drops or 0):,} would-drop decisions and no enforced drops "
            f"were recorded since core boot; LLM calls: {int(cf_calls):,}.",
        )
    elif cf_calls > 0 or bool(cf_deferred):
        cf_status = _stage_status(
            "active", "LLM activity observed", "info",
            f"{int(cf_calls):,} calls and {int(cf_deferred or 0):,} deferrals were recorded; "
            "no filter effects were emitted.",
        )
    elif (cf_examined or 0) > 0:
        cf_status = _stage_status(
            "active", "Evaluating", "info",
            f"{int(cf_examined or 0):,} torrents were examined with no drop decision recorded.",
        )
    else:
        cf_status = _stage_status(
            "inactive", "No activity observed", "neutral",
            "No content-filter evaluations or LLM calls were recorded since core boot.",
        )

    stages.append({
        "id": "contentfilter",
        "name": "Content Filter",
        "role": "Last-resort English/NSFW verdict on torrents the deterministic rules cannot settle",
        # A deferral happens *instead of* a call: the stage is wired up and the
        # endpoint refused it. Calling that "idle or disabled" sends the
        # operator to the config when the fault is the provider.
        "available": cf_calls > 0 or bool(cf_deferred),
        "status": cf_status,
        "model": cf_models[0] if len(cf_models) == 1 else (", ".join(cf_models) or None),
        "modelNote": None if cf_models else "no live calls recorded yet — the model label rides the token counter",
        "metrics": [
            _stage_metric(
                "Call success", ratio(cf_ok, cf_calls), "percent",
                f"{int(cf_ok):,} ok / {int(cf_err):,} failed",
                tone=_tone_from(ratio(cf_ok, cf_calls), 0.98, 0.9),
                help_text="Share of live calls that returned at all. Failures here are timeouts, not wrong answers — this stage has no ground truth to score against.",
            ),
            _stage_metric(
                "Avg latency", cf_latency, "seconds",
                f"{int(cf_calls):,} timed calls",
                tone=_tone_from(cf_latency, 2.0, 6.0),
                help_text="This model was chosen specifically on latency. Sustained drift above ~4s is the failure mode to watch.",
            ),
            _stage_metric(
                "Deferred", cf_deferred, "number",
                "re-queued, endpoint unreachable" if cf_deferred is not None
                else "not instrumented",
                tone="good" if cf_deferred == 0 else "warn",
                help_text="Torrents neither kept nor dropped because the LLM endpoint was unreachable. These come back around; a rising number means the provider is flaky.",
            ),
            _stage_metric(
                "Cache hit ratio", ratio(cf_hits, cf_hits + cf_misses), "percent",
                f"{int(cf_hits):,} hits / {int(cf_misses):,} misses",
                tone=_tone_from(ratio(cf_hits, cf_hits + cf_misses), 0.3, 0.05),
                help_text="Verdicts served from the sha256 LRU instead of a paid call. A near-zero ratio means the cache is not earning its keep on this workload.",
            ),
            _stage_metric(
                "Would drop", cf_would_drops, "number",
                "counterfactual decisions while enforcement is off"
                if cf_would_drops is not None else "not instrumented",
                tone="neutral",
                help_text="Counterfactual deterministic-rule decisions emitted in shadow mode. These rows were retained, not deleted.",
            ),
            _stage_metric(
                "Share of all drops", ratio(cf_llm_drops, cf_drops_all), "percent",
                f"{int(cf_llm_drops):,} of {int(cf_drops_all):,} enforced drops"
                if cf_live_drops is not None else "no enforced-drop counter emitted",
                tone="neutral",
                help_text="How much filtering this LLM actually does versus the cheap deterministic rules. A tiny share means the model is an expensive tie-breaker, not the workhorse.",
            ),
            _stage_metric(
                "Budget exhausted", cf_budget, "number",
                "calls that hit the daily cap" if cf_budget is not None
                else "not instrumented",
                tone="good" if cf_budget == 0 else "warn",
                help_text="Consultations that fell through to keep because the daily spend cap was reached.",
            ),
            _stage_metric(
                "Tokens", (cf_in + cf_out) if cf_tokens_present else None, "number",
                f"{int(cf_in):,} in / {int(cf_out):,} out" if cf_tokens_present
                else "not instrumented",
                tone="neutral",
                help_text="Provider-reported, since core boot. Divided by the work this stage actually does, this is the number that decides whether the model is worth its bill.",
            ),
        ],
        "tokens": {"input": int(cf_in), "output": int(cf_out)} if cf_tokens_present else None,
        "tokensNote": None,
        # Spend / unit of work: this stage's only actionable output is a drop.
        "unit": {"label": "per drop", "count": int(cf_llm_drops)},
    })

    # ── Stage 3: junk purge ──────────────────────────────────────────
    jp_models = labels_of("bitagent_junkpurge_llm_requests_total", "model")
    jp_ok = sum_all("bitagent_junkpurge_llm_requests_total", outcome="ok")
    jp_err = sum_all("bitagent_junkpurge_llm_requests_total", outcome="error")
    jp_reqs = jp_ok + jp_err
    verdicts = {
        v: sum_all("bitagent_junkpurge_judged_total", verdict=v)
        for v in ("junk", "real_mangled", "real_absent", "unsure")
    }
    jp_judged = sum(verdicts.values())
    # Tokens are the one BitAgent LLM stage with provider accounting, so an
    # absent family here means "this core/provider does not report tokens",
    # never "zero tokens were spent on 1,500 requests".
    jp_tokens_present = "bitagent_junkpurge_llm_tokens_total" in all_fam
    jp_in = sum_all("bitagent_junkpurge_llm_tokens_total", type="input")
    jp_out = sum_all("bitagent_junkpurge_llm_tokens_total", type="output")
    jp_quarantined = count("bitagent_junkpurge_quarantined_total")
    jp_would_delete = count("bitagent_junkpurge_would_delete_total")
    jp_expired = count("bitagent_junkpurge_expired_total")
    jp_deferred = count("bitagent_junkpurge_llm_deferred_total")
    jp_cycles = sum_all("bitagent_junkpurge_cycles_total")
    jp_cycle_latency = avg_latency(
        "bitagent_junkpurge_cycle_duration_seconds_sum",
        "bitagent_junkpurge_cycle_duration_seconds_count",
    )

    jp_instrumented = any(
        name.startswith("bitagent_junkpurge_") for name in all_fam
    )
    if not telemetry_available:
        jp_status = _stage_status(
            "unavailable", "Unavailable", "danger",
            "Core metrics could not be read, so junk-purge state is unknown.",
        )
    elif not jp_instrumented:
        jp_status = _stage_status(
            "unavailable", "Not instrumented", "neutral",
            "The core emitted no junk-purge metric family, so state is unknown.",
        )
    elif (jp_quarantined or 0) > 0 and (jp_would_delete or 0) > 0:
        jp_status = _stage_status(
            "mixed", "Mixed history", "warning",
            f"{int(jp_quarantined or 0):,} quarantines and "
            f"{int(jp_would_delete or 0):,} dry-run would-deletes were recorded since boot; "
            "telemetry does not expose the current configured mode.",
        )
    elif (jp_quarantined or 0) > 0:
        jp_status = _stage_status(
            "active", "Live quarantine observed", "success",
            f"{int(jp_quarantined or 0):,} torrents were quarantined since core boot.",
        )
    elif (jp_would_delete or 0) > 0:
        jp_status = _stage_status(
            "dry_run", "Dry run", "warning",
            f"Judging is active: {int(jp_would_delete or 0):,} confident-junk rows were "
            "counted as would-delete and none were quarantined.",
        )
    elif jp_reqs > 0 or bool(jp_deferred):
        jp_status = _stage_status(
            "active", "Judging active", "info",
            f"{int(jp_reqs):,} requests were recorded; no quarantine or dry-run effect "
            "has been observed yet.",
        )
    else:
        jp_status = _stage_status(
            "inactive", "No activity observed", "neutral",
            "No junk-purge requests were recorded since core boot.",
        )

    jp_unit = (
        {"label": "per quarantine", "count": int(jp_quarantined or 0)}
        if (jp_quarantined or 0) > 0
        else {"label": "per would-delete", "count": int(jp_would_delete or 0)}
    )

    stages.append({
        "id": "junkpurge",
        "name": "Junk Purge",
        "role": "Judges unmatched movie/TV torrents as junk or real before quarantine",
        # Same as the content filter: candidates left unjudged because the LLM
        # was unavailable mean the stage ran and could not reach its model.
        "available": jp_reqs > 0 or bool(jp_deferred),
        "status": jp_status,
        "model": jp_models[0] if len(jp_models) == 1 else (", ".join(jp_models) or None),
        "modelNote": None if jp_models else "no requests recorded yet",
        "metrics": [
            _stage_metric(
                "Request success", ratio(jp_ok, jp_reqs), "percent",
                f"{int(jp_ok):,} ok / {int(jp_err):,} failed",
                tone=_tone_from(ratio(jp_ok, jp_reqs), 0.98, 0.9),
                help_text="Provider-level success. Says nothing about verdict correctness.",
            ),
            _stage_metric(
                "Judged junk", ratio(verdicts["junk"], jp_judged), "percent",
                " / ".join(f"{k.replace('_', ' ')} {int(v):,}" for k, v in verdicts.items() if v),
                tone="neutral",
                help_text="Verdict mix, not accuracy. The measured false-junk floor is ~0.43% against a 0.1% budget, and junk confidence is bimodal (0.95/1.00) so no threshold separates the false positives — treat this as a volume signal only.",
            ),
            _stage_metric(
                "Quarantined", jp_quarantined, "number",
                f"{int(jp_expired):,} expired past the review window"
                if jp_expired is not None else "not instrumented",
                tone="neutral",
                help_text="Confident-junk torrents moved out of the main DB. Expiry removes them from the active review queue; the current core retains the snapshot for restoration.",
            ),
            _stage_metric(
                "Would delete", jp_would_delete, "number",
                "confident-junk rows retained by dry run"
                if jp_would_delete is not None else "not instrumented",
                tone="neutral",
                help_text="Dry-run signal: rows the purge would have removed if live quarantine were enabled. They remain in the main corpus.",
            ),
            _stage_metric(
                "Requests", count("bitagent_junkpurge_llm_requests_total"), "number",
                f"{int(jp_judged):,} verdicts over {int(jp_cycles):,} cycles",
                help_text="Provider requests since core boot, batched per cycle.",
            ),
            _stage_metric(
                "Avg cycle", jp_cycle_latency, "seconds",
                f"{int(jp_cycles):,} completed cycles",
                tone="neutral",
                help_text="Wall-clock per judging cycle, not per call — this stage runs in batches.",
            ),
            _stage_metric(
                "Tokens", (jp_in + jp_out) if jp_tokens_present else None, "number",
                f"{int(jp_in):,} in / {int(jp_out):,} out" if jp_tokens_present
                else "not instrumented",
                tone="neutral",
                help_text="Provider-reported, since core boot. Total spend combines every instrumented stage; missing usage or an unknown price makes it partial.",
            ),
        ],
        "tokens": {"input": int(jp_in), "output": int(jp_out)} if jp_tokens_present else None,
        "tokensNote": None,
        # In dry run, would-delete is the only measured output; once live,
        # quarantine remains the unit that this stage is paid for.
        "unit": jp_unit,
    })

    # A stage that never ran has no verdict to render. Its unlabelled optional
    # counters (deferred, budget_exhausted, quarantined, expired) are
    # registered at core boot and so scrape as a true 0 whether the stage is
    # off or genuinely clean — see the live core, which exposes
    # `bitagent_contentfilter_llm_budget_exhausted_total 0`. Keep the measured
    # value, drop the colour: a green "0 budget exhausted" next to "this stage
    # is idle" paints a disabled model as a healthy one, which is the one
    # thing this tab promises not to do. Ratios over a live LLM numerator
    # (share of drops, verdict mix) stay visible with their own detail line —
    # nulling a measured fact would be the opposite error.
    for stage in stages:
        if not stage["available"]:
            for metric in stage["metrics"]:
                metric["tone"] = "neutral"

    # Attach spend by stage. A stage whose model has no price entry gets its
    # tokens and a null cost, never a zero — see prom_metrics._llm_spend.
    by_stage = {row["stage"]: row for row in (spend.get("byModel") or [])}
    for stage in stages:
        row = by_stage.get(stage["id"])
        unit = stage.get("unit")
        usd = row.get("usd") if row else None
        stage["spend"] = {
            "usd": usd,
            "monthlyUsd": row.get("monthlyUsd") if row else None,
            "priced": bool(row and row.get("priced")),
            # Cost per unit of work is the comparison that ranks stages against
            # each other; without it a big bill and a big workload look alike.
            "usdPerUnit": (usd / unit["count"])
            if (usd is not None and unit and unit["count"] > 0) else None,
            "unitLabel": unit["label"] if unit else None,
            "unitCount": unit["count"] if unit else None,
        } if row else None

    return stages


def _spend_snapshot(raw: str) -> dict:
    """Spend-to-date per model, plus a measured projection against the budget.

    Token counters are cumulative since core boot and the core publishes no
    process-start gauge, so a single scrape cannot say what a month costs. The
    projection is therefore measured by sampling spend-to-date across this
    app's own polls, and it is deliberately conservative about saying zero: a
    model whose counter has not moved yet is reported as still measuring, not
    as costing nothing. Under-reporting a bill as "comfortably under budget"
    is the one failure this card exists to prevent, so the total is published
    as a floor (`monthlyIsPartial`) whenever any model is still measuring.
    """
    lines = _parse_prometheus_snapshot(raw).get("lines") or {}
    result = _llm_spend(lines)

    now_mono = _metric_history_now()
    sample = {
        row["model"]: float(row["usd"])
        for row in result["byModel"] if row["priced"]
    }
    if sample:
        _record_spend_snapshot(now_mono, sample)

    monthly_total = 0.0
    measured_any = False
    still_measuring: list[str] = []
    window = 0.0
    for row in result["byModel"]:
        if not row["priced"]:
            row["monthlyUsd"] = None
            still_measuring.append(row["model"])
            continue
        per_min, elapsed = _spend_rate_per_min(row["model"], now_mono)
        window = max(window, elapsed)
        # A flat counter is only credible as "$0/mo" once the window is longer
        # than this workload's burst gap; before that it is unmeasured.
        if per_min == 0.0 and elapsed < _SPEND_IDLE_TRUST_SECONDS:
            per_min = None
        if per_min is None:
            row["monthlyUsd"] = None
            still_measuring.append(row["model"])
            continue
        row["monthlyUsd"] = per_min * 43_200.0   # 60 x 24 x 30
        monthly_total += row["monthlyUsd"]
        measured_any = True

    monthly = monthly_total if measured_any else None
    budget = float(settings.llm_monthly_budget_usd or 0.0)
    partial = bool(still_measuring) or result["totalIsPartial"]
    result["monthlyUsd"] = monthly
    result["monthlyIsPartial"] = partial
    result["monthlyMeasuring"] = still_measuring
    result["monthlyWindowSeconds"] = window
    result["monthlyStatus"] = (
        "unavailable" if not result["available"]
        else "measuring" if monthly is None
        else "partial" if partial
        else "ok"
    )
    result["budgetUsd"] = budget or None
    result["budgetRatio"] = (monthly / budget) if (monthly is not None and budget > 0) else None
    result["pricesAsOf"] = LLM_PRICES_AS_OF
    return result


@app.get("/api/ai/summary")
async def api_ai_summary(identity: dict = Depends(require_operator)):
    """Scorecard for the classifier's LLM TMDB-matcher.

    Parses the bitagent_classifier_llm_match_* family out of /metrics and
    returns pre-shaped aggregates so the frontend never has to parse
    Prometheus label strings. `available` remains the matcher-activity flag for
    compatibility; `telemetryAvailable` and each stage's `status` distinguish
    a quiet stage from an unreachable or uninstrumented metrics source.
    """
    raw = await gql.fetch_metrics()
    spend = _spend_snapshot(raw)
    # Full scrape parsed once: metric name -> list of (labels dict, value).
    # The matcher view keeps its prefix-stripped `fam` alias so the existing
    # detail panels are untouched; the per-stage scorecard needs the whole
    # scrape because the content filter and junk purge live under different
    # metric prefixes entirely.
    all_fam: dict[str, list[tuple[dict, float]]] = {}
    fam: dict[str, list[tuple[dict, float]]] = {}
    for line in raw.splitlines():
        if not line or line.startswith("#"):
            continue
        parts = line.split(" ", 1)
        if len(parts) != 2:
            continue
        key, val = parts
        bare, _, label_blob = key.partition("{")
        try:
            v = float(val)
        except ValueError:
            continue
        labels = _parse_prom_labels(label_blob)
        all_fam.setdefault(bare, []).append((labels, v))
        if bare.startswith(_LLM_MATCH_PREFIX):
            fam.setdefault(bare[len(_LLM_MATCH_PREFIX):], []).append((labels, v))

    def _sum(name: str, **want: str) -> float:
        return sum(
            v for labels, v in fam.get(name, [])
            if all(labels.get(k) == wv for k, wv in want.items())
        )

    def _all(name: str, **want: str) -> float:
        return sum(
            v for labels, v in all_fam.get(name, [])
            if all(labels.get(k) == wv for k, wv in want.items())
        )

    def _labels_of(name: str, label: str) -> list[str]:
        seen = {
            labels.get(label)
            for labels, _ in all_fam.get(name, [])
            if labels.get(label)
        }
        return sorted(seen)

    def _avg_latency(sum_metric: str, count_metric: str, **want: str) -> float | None:
        count = _all(count_metric, **want)
        return (_all(sum_metric, **want) / count) if count > 0 else None

    def _ratio(numerator: float, denominator: float) -> float | None:
        return (numerator / denominator) if denominator > 0 else None

    # Live/shadow attaches by media type (labels: mode=live|shadow,
    # media_type=movie|tv).
    matches: dict[str, dict] = {}
    for mode in ("live", "shadow"):
        by_type = {
            labels.get("media_type") or "unknown": int(v)
            for labels, v in fam.get("matches_total", [])
            if labels.get("mode") == mode
        }
        matches[mode] = {"byType": by_type, "total": sum(by_type.values())}

    extract = {r: int(_sum("extract_total", result=r)) for r in ("ok", "empty", "error")}
    extract["total"] = sum(extract.values())

    rerank = {r: int(_sum("rerank_total", result=r)) for r in ("match", "none", "error")}
    rerank["total"] = sum(rerank.values())
    decided = rerank["match"] + rerank["none"]
    rerank["matchRate"] = (rerank["match"] / decided) if decided > 0 else 0.0

    gate_rejects = {
        labels.get("gate") or "unknown": int(v)
        for labels, v in fam.get("gate_rejects_total", [])
    }

    # Anime English gate: english=dub|sub|none|unknown × outcome=kept|rejected
    anime_by_english: dict[str, dict] = {}
    for labels, v in fam.get("anime_total", []):
        eng = labels.get("english") or "unknown"
        outcome = labels.get("outcome") or "unknown"
        anime_by_english.setdefault(eng, {"kept": 0, "rejected": 0})
        if outcome in ("kept", "rejected"):
            anime_by_english[eng][outcome] += int(v)
    anime_kept = sum(e["kept"] for e in anime_by_english.values())
    anime_rejected = sum(e["rejected"] for e in anime_by_english.values())

    hits = _sum("cache_hits_total")
    misses = _sum("cache_misses_total")
    lookups = hits + misses

    call_errors_by_stage = {}
    for labels, v in fam.get("call_errors_total", []):
        stage = labels.get("stage") or "unknown"
        call_errors_by_stage[stage] = call_errors_by_stage.get(stage, 0) + int(v)

    # Avg call latency per stage from the histogram's _sum/_count pair.
    latency = {}
    for stage in ("extract", "rerank"):
        dur_sum = _sum("call_duration_seconds_sum", stage=stage)
        dur_count = _sum("call_duration_seconds_count", stage=stage)
        latency[stage] = {
            "avgSeconds": (dur_sum / dur_count) if dur_count > 0 else 0.0,
            "count": int(dur_count),
        }

    # Alt-title backfill coverage (core v0.22.0 dashstats gauges). "checked"
    # counts rows the refresh-alt-titles sweep has visited — including rows
    # with zero upstream alt titles — so checked/total is honest backfill
    # progress; withAlt/total is alt-title density.
    alt_gauges: dict[str, float] = {}
    for line in raw.splitlines():
        if not line.startswith("bitagent_dashstats_alt_title_content_"):
            continue
        parts = line.split(" ", 1)
        if len(parts) != 2:
            continue
        try:
            alt_gauges[parts[0]] = float(parts[1])
        except ValueError:
            continue
    alt_total = int(alt_gauges.get("bitagent_dashstats_alt_title_content_total", 0))
    alt_checked = int(alt_gauges.get("bitagent_dashstats_alt_title_content_checked", 0))
    alt_with = int(alt_gauges.get("bitagent_dashstats_alt_title_content_with_alt", 0))
    alt_titles = {
        "available": bool(alt_gauges),
        "total": alt_total,
        "checked": alt_checked,
        "withAlt": alt_with,
        "checkedRatio": (alt_checked / alt_total) if alt_total > 0 else 0.0,
    }

    return {
        # NOT `bool(fam)`. `cache_hits`/`cache_misses` are registered at core
        # boot with prometheus.NewCounter, so they scrape as a bare 0 with the
        # matcher switched off — the family is never empty and the "matcher
        # metrics not present" banner was therefore unreachable in exactly the
        # situation it exists for. Gate on a family that only appears once the
        # matcher has actually done work.
        "available": any(
            fam.get(name) for name in ("extract_total", "rerank_total", "matches_total")
        ),
        # Distinguish "the matcher did no work" from "the metrics request
        # failed". The old response flattened both into available=false, which
        # sent operators debugging configuration during a transport failure.
        "telemetryAvailable": bool(all_fam),
        "spend": spend,
        "stages": _llm_stage_scorecards(
            spend=spend,
            telemetry_available=bool(all_fam),
            all_fam=all_fam,
            sum_all=_all,
            labels_of=_labels_of,
            avg_latency=_avg_latency,
            ratio=_ratio,
            matcher_matches=matches,
            matcher_extract=extract,
            matcher_rerank=rerank,
            matcher_gate_rejects=gate_rejects,
            matcher_latency=latency,
        ),
        "matches": matches,
        "extract": extract,
        "rerank": rerank,
        "gateRejects": gate_rejects,
        "anime": {
            "kept": anime_kept,
            "rejected": anime_rejected,
            "byEnglish": anime_by_english,
        },
        "cache": {
            "hits": int(hits),
            "misses": int(misses),
            "hitRatio": (hits / lookups) if lookups > 0 else 0.0,
        },
        "callErrors": {
            "byStage": call_errors_by_stage,
            "total": sum(call_errors_by_stage.values()),
        },
        "latency": latency,
        "altTitles": alt_titles,
    }


_ANIME_DEFAULT_TYPES = ["movie", "tv_show"]
_ANIME_GENRE = "tmdb:16"
_ANIME_SCORE_THRESHOLD = 3
_ANIME_DISCOVERY_QUERIES = (
    "subsplease",
    "anime time",
    "erai raws",
    "yameii",
    "crunchyroll",
    "hidive",
    "horriblesubs",
    "asw",
    "cleo",
    "bonkai77",
    "judas",
    "ember",
    "lostyears",
    "tsundere",
)
_ANIME_ANIMATION_OFFSETS = (0, 500, 2000, 2500, 4500, 5000, 5500)
# Max concurrent upstream queries per anime browse (the fan-out is ~21 specs).
_ANIME_FANOUT_CONCURRENCY = 5
_ANIME_CJK_RX = _re.compile(r"[ぁ-ゟ゠-ヿ一-龯]")
_ANIME_MARKER_RX = _re.compile(
    r"anime[ ._-]?time|subsplease|erai[ ._-]?raws|horriblesubs|yameii|"
    r"kawaiika(?:[ ._-]?raws)?|vcb[ ._-]?studio|beatrice[ ._-]?raws|"
    r"crunchyroll|\bcr[ ._-]?web[ ._-]?dl\b|hidive|funimation|anime[ ._-]?rg|"
    r"(?:^|[\[\]\s._-])(?:asw|cleo|bonkai77|judas|ember|lostyears|tsundere)(?:[\]\s._-]|$)",
    _re.I,
)


def _blocked_phrase_id(item: dict, block_phrases: list[dict]) -> int | None:
    haystack = " ".join(
        str(v or "") for v in (
            item.get("name"),
            item.get("torrentName"),
            item.get("title"),
            item.get("releaseGroup"),
        )
    ).lower()
    for bp in block_phrases:
        if bp["pattern"] in haystack:
            return bp["id"]
    return None


async def _filter_search_items(
    raw_items: list[dict],
    block_phrases: list[dict],
    *,
    unmapped_only: bool,
    hide_unmapped: bool,
    hide_non_english: bool,
    limit: int | None = None,
    seen_hashes: set[str] | None = None,
    hit_counter: Counter | None = None,
) -> tuple[list[dict], int]:
    items: list[dict] = []
    excluded_count = 0
    seen = seen_hashes if seen_hashes is not None else set()
    # Accumulate block-phrase matches and flush once (one commit) instead of an
    # UPDATE+commit per matched item. A caller that invokes this in a loop (anime
    # fan-out) passes its own counter to batch across every subquery.
    counter = hit_counter if hit_counter is not None else Counter()
    for raw in raw_items:
        item = _api_item_from_search_item(raw)
        info_hash = item.get("infoHash") or ""
        if info_hash and info_hash in seen:
            continue
        matched_phrase_id = _blocked_phrase_id(item, block_phrases)
        if matched_phrase_id is not None:
            counter[matched_phrase_id] += 1
            excluded_count += 1
            continue
        is_mapped = bool(item.get("isMapped"))
        if unmapped_only and is_mapped:
            continue
        if hide_unmapped and not is_mapped:
            continue
        langs = item.get("languages") or []
        if hide_non_english and langs and not any(lang == "en" for lang in langs):
            continue
        if info_hash:
            seen.add(info_hash)
        items.append(item)
        if limit is not None and len(items) >= limit:
            break
    # If we own the counter (single-shot caller), flush our matches now. A caller
    # that passed its own counter flushes once after its loop.
    if hit_counter is None:
        await bump_block_phrase_hits(counter)
    return items, excluded_count


def _anime_score(item: dict) -> int:
    text = " ".join(
        str(v or "") for v in (
            item.get("torrentName"),
            item.get("name"),
            item.get("title"),
            item.get("originalTitle"),
            item.get("releaseGroup"),
        )
    )
    score = 0
    if str(item.get("originalLanguage") or "").lower() == "ja":
        score += 4
    if _ANIME_CJK_RX.search(text):
        score += 4
    if _ANIME_MARKER_RX.search(text):
        score += 4
    return score


def _anime_release_title(name: str) -> str:
    s = str(name or "")
    # Anime release names commonly start with one or more release-group tags and
    # use "Title - 01" rather than S01E01. Strip that shape before title-level
    # grouping so a season does not become 12 separate poster cards.
    s = _re.sub(r"^\s*(?:\[[^\]]{1,60}\]\s*)+", "", s)
    s = _re.sub(r"\.(?:mkv|mp4|avi|m4v)$", "", s, flags=_re.I)
    s = _re.sub(r"\s*-?\s*\((?=[^)]*(?:Season|S\d{1,2}|OVA|OAD|Specials|Movie|BD)\b)[^)]*\).*$", "", s, flags=_re.I)
    s = _re.sub(r"\s+-\s+(?:\d{1,4}|NC(?:OP|ED)\d*|OP\d*|ED\d*|OVA\d*|OAD\d*|PV\d*)(?:v\d+)?\b.*", "", s, flags=_re.I)
    s = _re.sub(r"\bS\d{1,2}E\d{1,3}(?:-E\d{1,3})?.*", "", s, flags=_re.I)
    # (?!\d)(?!E) rather than a trailing \b so a following "_" or letter
    # ("S02_complete") still strips and groups with S01 — mirrors the frontend.
    s = _re.sub(r"\bS\d{1,2}(?!\d)(?!E).*", "", s, flags=_re.I)
    s = _re.sub(r"\bSeason\s+\d+.*", "", s, flags=_re.I)
    s = _re.sub(r"\b(480|576|720|1080|2160)[pi]?.*", "", s, flags=_re.I)
    s = _re.sub(
        r"\b(BluRay|BDRip|WEBRip|WEB[-.]DL|HDTV|DVDRip|REMUX|HDR|SDR|IMAX|"
        r"FLAC|AAC|HEVC|AVC|AV1|x264|x265|H\.?264|H\.?265)\b.*",
        "",
        s,
        flags=_re.I,
    )
    s = _re.sub(r"[._]+", " ", s)
    clean = _re.sub(r"\s+", " ", s).strip(" -._[]")
    return _re.sub(r"\s+\($", "", clean).strip(" -._[]")


def _anime_group_key(item: dict) -> str:
    ct = item.get("contentType") or "unknown"
    source = item.get("contentSource") or ""
    content_id = item.get("contentId")
    if source and content_id:
        return f"{ct}:id:{source}:{content_id}"
    title = _anime_release_title(item.get("name") or item.get("torrentName") or "")
    norm = _re.sub(r"\s+", " ", title).strip().lower()
    if not norm:
        norm = _norm_title(item.get("name") or item.get("torrentName") or "")
    return f"{ct}:name:{norm}"


def _anime_rank(item: dict) -> tuple:
    mapped = 1 if item.get("isMapped") else 0
    return (item.get("seeders") or 0, mapped, item.get("size") or 0)


def _anime_representatives(items: list[dict]) -> list[dict]:
    groups: dict[str, dict] = {}
    for item in items:
        if _anime_score(item) < _ANIME_SCORE_THRESHOLD:
            continue
        key = _anime_group_key(item)
        rep = dict(item)
        if not rep.get("isMapped"):
            clean_title = _anime_release_title(rep.get("name") or rep.get("torrentName") or "")
            if clean_title:
                rep["name"] = clean_title
        if not key.strip().endswith(":name:") and (
            key not in groups or _anime_rank(rep) > _anime_rank(groups[key])
        ):
            groups[key] = rep
    return sorted(groups.values(), key=_anime_rank, reverse=True)


def _aggregations_from_items(items: list[dict], fallback: dict | None = None) -> dict:
    aggs = dict(fallback or {})
    fields = {
        "contentType": "contentType",
        "videoResolution": "videoResolution",
        "videoSource": "videoSource",
    }
    for agg_name, item_key in fields.items():
        counts = Counter(str(item.get(item_key) or "") for item in items)
        counts.pop("", None)
        if counts:
            aggs[agg_name] = [
                {"value": value, "count": count}
                for value, count in counts.most_common(50)
            ]
    return aggs


async def _api_torrents_anime(
    *,
    q: str,
    limit: int,
    offset: int,
    unmapped_only: bool,
    hide_unmapped: bool,
    hide_non_english: bool,
    order: str,
    types: list[str],
    genre_values: list[str],
    year_values: list[int],
    quality_values: list[str],
    source_values: list[str],
    aggregate_facets: bool,
    block_phrases: list[dict],
) -> dict:
    """High-recall anime browse mode.

    The core has no original-language facet and many anime releases are not
    TMDB-mapped yet, so querying only TMDB Animation returns a tiny, duplicate-
    heavy window. This mode combines mapped Animation scans with high-confidence
    release/source marker searches, scores candidates, and returns title-level
    representatives for the poster grid.
    """
    specs: list[dict] = []
    base_genres = list(genre_values)
    animation_genres = list(dict.fromkeys(base_genres or [_ANIME_GENRE]))
    if q.strip():
        specs.append(_search_input(
            q=q, limit=500, offset=0, order=order, types=types,
            genre_values=base_genres, year_values=year_values,
            quality_values=quality_values, source_values=source_values,
            aggregate_facets=aggregate_facets, total_count=False,
        ))
        specs.append(_search_input(
            q=q, limit=500, offset=0, order=order, types=types,
            genre_values=animation_genres, year_values=year_values,
            quality_values=quality_values, source_values=source_values,
            aggregate_facets=False, total_count=False,
        ))
    else:
        for i, anim_offset in enumerate(_ANIME_ANIMATION_OFFSETS):
            specs.append(_search_input(
                q="", limit=500, offset=anim_offset, order=order, types=types,
                genre_values=animation_genres, year_values=year_values,
                quality_values=quality_values, source_values=source_values,
                aggregate_facets=aggregate_facets and i == 0,
                total_count=False,
            ))
        for term in _ANIME_DISCOVERY_QUERIES:
            specs.append(_search_input(
                q=term, limit=200, offset=0, order=order, types=types,
                genre_values=base_genres, year_values=year_values,
                quality_values=quality_values, source_values=source_values,
                aggregate_facets=False, total_count=False,
            ))

    # The high-recall anime mode fans out ~21 upstream queries. Bound how many
    # hit the core at once so a single anime browse can't saturate it.
    sem = asyncio.Semaphore(_ANIME_FANOUT_CONCURRENCY)

    async def _run(spec: dict):
        async with sem:
            return await gql.query(gql.SEARCH_TORRENTS, {"input": spec})

    results = await asyncio.gather(
        *(_run(spec) for spec in specs),
        return_exceptions=True,
    )
    seen_hashes: set[str] = set()
    candidates: list[dict] = []
    excluded_count = 0
    fallback_aggs: dict = {}
    # One shared counter across every subquery → one batched hit flush.
    hit_counter: Counter = Counter()
    for result in results:
        if isinstance(result, Exception):
            logger.warning("Anime search subquery failed: %s", result)
            continue
        block = ((result.get("data") or {}).get("torrentContent") or {}).get("search") or {}
        if not fallback_aggs and block.get("aggregations"):
            fallback_aggs = block.get("aggregations") or {}
        shaped, excluded = await _filter_search_items(
            block.get("items") or [],
            block_phrases,
            unmapped_only=unmapped_only,
            hide_unmapped=hide_unmapped,
            hide_non_english=hide_non_english,
            limit=None,
            seen_hashes=seen_hashes,
            hit_counter=hit_counter,
        )
        candidates.extend(shaped)
        excluded_count += excluded
    await bump_block_phrase_hits(hit_counter)

    representatives = _anime_representatives(candidates)
    page = representatives[offset:offset + limit]
    return {
        "totalCount": len(representatives),
        "items": page,
        "aggregations": _aggregations_from_items(representatives, fallback_aggs),
        "excludedByBlockPhrases": excluded_count,
    }


@app.get("/api/torrents")
async def api_torrents(
    q: str = "",
    content_types: str = "",
    content_type: str = "",  # deprecated alias for content_types
    limit: int = Query(50, ge=1, le=500),
    offset: int = Query(0, ge=0),
    unmapped_only: bool = False,
    hide_non_english: bool = False,
    hide_unmapped: bool = False,
    order: str = "",
    genres: str = "",
    years: str = "",
    release_years: str = "",
    qualities: str = "",
    quality: str = "",
    sources: str = "",
    aggregate_facets: bool = False,
    anime: bool = False,
    group: bool = False,
    identity: dict = Depends(require_auth),
):
    """Wraps the core's torrentContent.search.

    Both `content_types` and the deprecated `content_type` are accepted for
    backwards compatibility. If both are present, `content_types` wins.

    Server-side filters (unmapped_only, hide_non_english, hide_unmapped) trigger
    4× over-fetch from GraphQL to fill the page after filtering. totalCount
    returns -1 (unknown) when any filter is active.

    `group=true` (core >= v0.43) returns one row per title — the highest-seeded
    representative — with a grouped totalCount and hasNextPage. Grouped mode is
    matched-only by construction, so unmapped filters are no-ops; limit/offset
    page over TITLE GROUPS. totalCount is usually a planner estimate
    (totalCountIsEstimate) — callers must page off hasNextPage, never
    ceil(totalCount/limit). Ignored in anime mode (its fan-out needs raw rows).

    `order` drives the library home rows: "seeders" (most-seeded first, the
    "Popular" row) or "newest" (published_at desc, the "Recently Added" row).
    Empty = the core's default relevance order.

    Responses are keyed purely on the request params (identity only gates access),
    so they are served from a short-TTL bounded cache, single-flighted per key and
    served stale while one refresh runs (see _torrents_singleflight) — this covers
    the fixed home shelves and the anime set, which repeat on every home visit.
    """
    # JSON-encode the param tuple so string values (q, genres, …) that contain a
    # separator can't collide across a field boundary (e.g. q="a|" vs "|b").
    cache_key = json.dumps([
        q, content_types, content_type, limit, offset, unmapped_only,
        hide_non_english, hide_unmapped, order, genres, years, release_years,
        qualities, quality, sources, aggregate_facets, anime, group,
    ])
    return await _torrents_singleflight(cache_key, lambda: _api_torrents_uncached(
        q=q, content_types=content_types, content_type=content_type, limit=limit,
        offset=offset, unmapped_only=unmapped_only, hide_non_english=hide_non_english,
        hide_unmapped=hide_unmapped, order=order, genres=genres, years=years,
        release_years=release_years, qualities=qualities, quality=quality,
        sources=sources, aggregate_facets=aggregate_facets, anime=anime, group=group,
    ))


async def _api_torrents_uncached(
    *, q: str, content_types: str, content_type: str, limit: int, offset: int,
    unmapped_only: bool, hide_non_english: bool, hide_unmapped: bool, order: str,
    genres: str, years: str, release_years: str, qualities: str, quality: str,
    sources: str, aggregate_facets: bool, anime: bool, group: bool,
) -> dict:
    """The uncached /api/torrents body; api_torrents documents the parameters."""
    block_phrases = await _load_block_phrases()
    types = _content_type_values(content_types, content_type, _ANIME_DEFAULT_TYPES if anime else None)
    genre_values = _csv_values(genres)
    year_values = _year_values(years or release_years)
    quality_values = _resolution_values(qualities or quality)
    source_values = _csv_values(sources)

    if anime:
        response = await _api_torrents_anime(
            q=q,
            limit=limit,
            offset=offset,
            unmapped_only=unmapped_only,
            hide_unmapped=hide_unmapped,
            hide_non_english=hide_non_english,
            order=order,
            types=types,
            genre_values=genre_values,
            year_values=year_values,
            quality_values=quality_values,
            source_values=source_values,
            aggregate_facets=aggregate_facets,
            block_phrases=block_phrases,
        )
        return response

    if group:
        # Server-side grouped pagination: one representative per title, real
        # limit/offset paging. No over-fetch — grouped mode is matched-only, so
        # unmapped filters are inherent; block phrases / hide_non_english drop
        # representatives post-hoc (a page may run slightly short — the client
        # pages off hasNextPage, not returned count).
        search_input = _search_input(
            q=q,
            limit=limit,
            offset=offset,
            order=order,
            types=types,
            genre_values=genre_values,
            year_values=year_values,
            quality_values=quality_values,
            source_values=source_values,
            aggregate_facets=aggregate_facets,
            total_count=True,
            group_by_content=True,
            has_next_page=True,
        )
        # A cold unfiltered grouped browse scans the whole matched corpus and
        # can exceed the client's 10s fail-fast default; give it room instead
        # of fail-opening to an empty page. Faceted/searched browses are fast.
        result = await gql.query(gql.SEARCH_TORRENTS, {"input": search_input}, timeout=30.0)
        block = ((result.get("data") or {}).get("torrentContent") or {}).get("search") or {}
        items, excluded_count = await _filter_search_items(
            block.get("items") or [],
            block_phrases,
            unmapped_only=False,
            hide_unmapped=False,
            hide_non_english=hide_non_english,
            limit=limit,
        )
        response = {
            "totalCount": block.get("totalCount") or 0,
            "totalCountIsEstimate": bool(block.get("totalCountIsEstimate")),
            "hasNextPage": bool(block.get("hasNextPage")),
            "items": items,
            "aggregations": block.get("aggregations") or {},
            "excludedByBlockPhrases": excluded_count,
        }
        return response

    server_filtering = unmapped_only or hide_non_english or hide_unmapped
    # When filtering, fetch 4× from the proportional GraphQL offset so each
    # UI page scans a distinct window. totalCount=-1 signals to the frontend
    # that pagination is approximate (show/hide Next based on returned count).
    gql_limit = limit * 4 if server_filtering else limit
    gql_offset = offset * 4 if server_filtering else offset
    search_input = _search_input(
        q=q,
        limit=gql_limit,
        offset=gql_offset,
        order=order,
        types=types,
        genre_values=genre_values,
        year_values=year_values,
        quality_values=quality_values,
        source_values=source_values,
        aggregate_facets=aggregate_facets,
        total_count=not server_filtering,
    )
    result = await gql.query(gql.SEARCH_TORRENTS, {"input": search_input})
    block = ((result.get("data") or {}).get("torrentContent") or {}).get("search") or {}

    items, excluded_count = await _filter_search_items(
        block.get("items") or [],
        block_phrases,
        unmapped_only=unmapped_only,
        hide_unmapped=hide_unmapped,
        hide_non_english=hide_non_english,
        limit=limit,
    )
    total = -1 if server_filtering else (block.get("totalCount") or 0) - excluded_count
    response = {
        "totalCount": total,
        "items": items,
        "aggregations": block.get("aggregations") or {},
        "excludedByBlockPhrases": excluded_count,
    }
    return response


# Block phrases change rarely but are read on every /api/torrents request (each
# home visit fires 4+). Cache the list with a short TTL and invalidate on any
# create/delete so filtering never hits SQLite in the hot path. The cached `hits`
# field goes stale, but filtering only uses id/pattern/scope; the operator list
# endpoint queries the live table for accurate counts.
_BLOCK_PHRASES_TTL = 60.0
_block_phrases_cache: dict = {"at": 0.0, "rows": None}


async def _load_block_phrases() -> list[dict]:
    """All operator block phrases, lowercased, ordered by id (TTL-cached)."""
    now = time.monotonic()
    rows = _block_phrases_cache["rows"]
    if rows is not None and (now - _block_phrases_cache["at"]) < _BLOCK_PHRASES_TTL:
        return rows
    db = await get_db()
    cur = await db.execute(
        "SELECT id, pattern, scope, note, hits FROM block_phrases ORDER BY id"
    )
    fetched = await cur.fetchall()
    await cur.close()
    result = [
        {"id": r[0], "pattern": (r[1] or "").lower(), "scope": r[2], "note": r[3] or "", "hits": r[4]}
        for r in fetched
    ]
    _block_phrases_cache["rows"] = result
    _block_phrases_cache["at"] = now
    return result


def _invalidate_block_phrases_cache() -> None:
    """Drop the block-phrase cache and any cached torrents responses — a changed
    block list means both the filter set and every cached result are stale."""
    _block_phrases_cache["rows"] = None
    _block_phrases_cache["at"] = 0.0
    _torrents_cache.clear()
    # In-flight results were computed against the old list: forget them so
    # _torrents_publish does not cache them when they land.
    _torrents_inflight.clear()


# /api/torrents responses depend only on the query params (identity just gates
# access), so the same params always yield the same result. The 4 home shelves
# and the anime representative set are fixed-param, user-independent, and hit on
# every home visit; caching them (bounded LRU + short TTL) skips the block-phrase
# load, the upstream gql fan-out, and the anime shaping. Invalidated whenever the
# block-phrase list changes (see _invalidate_block_phrases_cache).
#
# Tradeoff: a cache hit returns before the filter runs, so block_phrases.hits is
# only bumped on cache misses. The operator-facing hit counter is therefore a
# cache-miss sample, not an exact per-item total — acceptable for an insight
# metric, and re-counting on every hit would reintroduce the per-request write
# the cache exists to avoid.
#
# Measured 2026-09-19: a home shelf (400-row over-fetch for hide_unmapped) costs
# 0.5-0.8 s at the core and the four shelves land together, so a cold visit saw
# 3.2-3.6 s per row. Misses are single-flighted per key — concurrent viewers
# share one upstream call — and an expired entry is served immediately while
# one refresh runs behind it, so only the first visit after boot (or after a
# long idle) pays the upstream latency.
_TORRENTS_CACHE_TTL = 180.0
# ponytail: a stale entry is served up to this age while one refresh runs behind
# it; older ones compute inline as before. Widen it if visits are sparser than
# this; add a warmer for the fixed home shelves if widening is not enough.
_TORRENTS_CACHE_STALE_TTL = 900.0
_TORRENTS_CACHE_MAX = 128
_torrents_cache: "OrderedDict[str, tuple[float, dict]]" = OrderedDict()
_torrents_inflight: dict[str, asyncio.Task] = {}


def _torrents_cache_lookup(key: str) -> tuple[dict | None, bool]:
    """(value, fresh). A stale-but-servable value comes back with fresh=False;
    anything past the stale window is dropped."""
    hit = _torrents_cache.get(key)
    if hit is None:
        return None, False
    age = time.monotonic() - hit[0]
    if age >= _TORRENTS_CACHE_STALE_TTL:
        _torrents_cache.pop(key, None)
        return None, False
    _torrents_cache.move_to_end(key)
    return hit[1], age < _TORRENTS_CACHE_TTL


def _torrents_cache_put(key: str, value: dict) -> None:
    _torrents_cache[key] = (time.monotonic(), value)
    _torrents_cache.move_to_end(key)
    while len(_torrents_cache) > _TORRENTS_CACHE_MAX:
        _torrents_cache.popitem(last=False)


def _torrents_publish(key: str, task: asyncio.Task) -> None:
    if _torrents_inflight.get(key) is not task:
        return  # superseded by an invalidation: computed against a stale block list
    _torrents_inflight.pop(key, None)
    try:
        value = task.result()
    except BaseException:
        return  # the awaiting caller (if any) got the error; a stale hit retries later
    _torrents_cache_put(key, value)


async def _torrents_singleflight(key: str, compute) -> dict:
    """Fresh hit: return it. Otherwise share one in-flight `compute()` across
    every caller of this key; a stale hit is returned at once while that
    computation refreshes it."""
    value, fresh = _torrents_cache_lookup(key)
    if value is not None and fresh:
        return value
    task = _torrents_inflight.get(key)
    if task is None:
        task = asyncio.create_task(compute())
        _torrents_inflight[key] = task
        task.add_done_callback(lambda t, k=key: _torrents_publish(k, t))
    if value is not None:
        return value
    return await asyncio.shield(task)


@app.get("/api/torrents/{info_hash}")
async def api_torrent_detail(info_hash: str, identity: dict = Depends(require_auth)):
    """Detail view for a single torrent, via the infoHashes search filter —
    the only by-hash lookup the core schema offers (Query.torrent is a
    files/sources/tags namespace, not a record lookup)."""
    result = await gql.query(gql.TORRENT_DETAIL, {"infoHash": info_hash})
    gql_errors = (result.get("errors") or [])
    if gql_errors:
        logger.warning("TORRENT_DETAIL gql errors for %s: %s", info_hash, gql_errors)
        raise HTTPException(502, "Core query failed")
    items = (
        ((result.get("data") or {}).get("torrentContent") or {}).get("search") or {}
    ).get("items") or []

    if not items:
        raise HTTPException(404, "Torrent not found")

    c = items[0]
    t = c.get("torrent") or {}
    langs = [lang.get("id") for lang in (c.get("languages") or []) if lang.get("id")]
    content = c.get("content") or {}
    orig_lang_obj = content.get("originalLanguage") or {}
    # externalLinks is a list of {metadataSource{key}, url}; the modal wants
    # bare ids. tmdbId comes from the content mapping itself; imdbId is
    # extracted from the imdb link URL (tt\d+).
    ext_links = {
        ((link.get("metadataSource") or {}).get("key") or ""): (link.get("url") or "")
        for link in (content.get("externalLinks") or [])
    }
    imdb_m = _re.search(r"tt\d+", ext_links.get("imdb", ""))
    tmdb_id = c.get("contentId") if c.get("contentSource") == "tmdb" else None
    # Episode structure: {label, seasons:[{season, episodes:[..]}]}
    episodes = c.get("episodes") or {}
    seasons = episodes.get("seasons") or []
    ep_list = (seasons[0].get("episodes") if seasons else None) or []
    title = c.get("title") or t.get("name") or info_hash
    dn = _urlquote(title)
    return {
        "infoHash": info_hash,
        "name": title,
        "title": c.get("title"),
        "size": t.get("size") or 0,
        "filesCount": t.get("filesCount") or 0,
        "files": t.get("files") or [],
        "contentType": c.get("contentType"),
        "contentSource": c.get("contentSource"),
        "isMapped": bool(c.get("contentSource")),
        "seeders": c.get("seeders") or 0,
        "leechers": c.get("leechers") or 0,
        "createdAt": c.get("createdAt"),
        "updatedAt": c.get("updatedAt"),
        "languages": langs,
        "videoResolution": c.get("videoResolution"),
        "videoSource": c.get("videoSource"),
        "videoCodec": c.get("videoCodec"),
        "releaseGroup": c.get("releaseGroup"),
        "originalLanguage": orig_lang_obj.get("id"),
        "imdbId": imdb_m.group(0) if imdb_m else None,
        "tmdbId": str(tmdb_id) if tmdb_id else None,
        "tvdbId": None,
        "seasonNumber": seasons[0].get("season") if seasons else None,
        "episodeNumber": ep_list[0] if len(ep_list) == 1 else None,
        "episodeTitle": episodes.get("label"),
        "magnetUri": f"magnet:?xt=urn:btih:{info_hash}&dn={dn}",
    }


# Mirrors the frontend `normGroupTitle` (static/js/norm-title.js) — strips
# season/episode/quality/source
# suffixes so two raw torrent names for the same title collapse to one key. Used
# to match releases of an *unmapped* title (no contentId to join on).
# Season stripping uses (?!\d)(?!E) rather than a trailing \b so a following "_"
# or letter ("S02_complete") still matches and groups with S01 — mirrors the
# frontend fix in static/js/norm-title.js.
_NORM_STRIP = [
    _re.compile(r"\bS\d{1,2}E\d{1,3}(?:-E\d{1,3})?.*", _re.I),
    _re.compile(r"\bS\d{1,2}(?!\d)(?!E).*", _re.I),
    _re.compile(r"\bSeason\s+\d+.*", _re.I),
    _re.compile(r"\b(480|576|720|1080|2160)[pi]?.*", _re.I),
    _re.compile(r"\b(BluRay|BDRip|WEBRip|WEB[-.]DL|HDTV|DVDRip|REMUX|HDR|SDR|IMAX|FLAC|MP3|AAC)\b.*", _re.I),
]


def _norm_title(name: str) -> str:
    s = name or ""
    for rx in _NORM_STRIP:
        s = rx.sub("", s)
    return _re.sub(r"\s+", " ", s).strip().lower()


@app.get("/api/titles/releases")
async def api_title_releases(
    title: str = Query("", description="Display title to search for"),
    content_type: str = "",
    content_source: str = "",
    content_id: str = "",
    identity: dict = Depends(require_auth),
):
    """Every torrent release for a single library title.

    The library grid groups torrents client-side over a capped 500-item batch,
    so a poster's detail view only ever saw the releases that happened to be
    co-loaded in that page — usually one. This endpoint re-queries the backend
    for *all* releases of one title so the detail view can present the full set
    of editions / seasons / episodes.

    Match strategy:
      - mapped titles (content_source + content_id present): search by the title
        queryString + contentType facet, then keep items whose
        (contentSource, contentId) equal the requested identity. This captures
        every edition, season and episode of that exact title.
      - unmapped titles: keep items whose normalised name matches the request.

    Results are ordered by seeders desc so, if the 500-item cap is hit on a huge
    title, the best-seeded releases are the ones retained.
    """
    q = (title or "").strip()
    if not q:
        raise HTTPException(400, "title is required")

    search_input: dict = {
        "queryString": q,
        "limit": 500,
        "offset": 0,
        "totalCount": False,
        "orderBy": [{"field": "seeders", "descending": True}],
    }
    if content_type:
        search_input["facets"] = {"contentType": {"filter": [content_type]}}
    result = await gql.query(gql.SEARCH_TORRENTS, {"input": search_input})
    block = ((result.get("data") or {}).get("torrentContent") or {}).get("search") or {}

    want_id = str(content_id or "").strip()
    want_source = (content_source or "").strip()
    want_norm = _norm_title(q)
    mapped_match = bool(want_id and want_source)

    block_phrases = await _load_block_phrases()
    items: list[dict] = []
    seen: set[str] = set()
    hit_counter: Counter = Counter()
    for it in block.get("items") or []:
        torrent = it.get("torrent") or {}
        info_hash = it.get("infoHash")
        if not info_hash or info_hash in seen:
            continue
        name = (it.get("title") or torrent.get("name") or "")
        name_lower = name.lower()

        matched_phrase_id = None
        for bp in block_phrases:
            if bp["pattern"] in name_lower:
                matched_phrase_id = bp["id"]
                break
        if matched_phrase_id is not None:
            hit_counter[matched_phrase_id] += 1
            continue

        it_source = it.get("contentSource") or ""
        it_id = str(it.get("contentId") or "")
        if mapped_match:
            if not (it_source == want_source and it_id == want_id):
                continue
        else:
            if _norm_title(name) != want_norm:
                continue

        seen.add(info_hash)
        langs = [lang.get("id") for lang in (it.get("languages") or []) if lang.get("id")]
        items.append({
            "infoHash": info_hash,
            "name": name,
            # Raw torrent filename — carries the edition / quality / source
            # tokens (e.g. "Directors.Cut.2160p.REMUX") that the cleaned
            # metadata `title` strips out. The detail view parses this.
            "torrentName": torrent.get("name") or name,
            "title": it.get("title"),
            "size": torrent.get("size") or 0,
            "filesCount": torrent.get("filesCount") or 0,
            "seeders": it.get("seeders") or 0,
            "leechers": it.get("leechers") or 0,
            "contentType": it.get("contentType"),
            "contentSource": it.get("contentSource"),
            "contentId": it.get("contentId"),
            "isMapped": bool(it.get("contentSource")),
            "languages": langs,
            "videoResolution": it.get("videoResolution"),
            "videoSource": it.get("videoSource"),
            "releaseGroup": it.get("releaseGroup"),
            "discoveredAt": it.get("createdAt"),
            "updatedAt": it.get("updatedAt"),
        })

    await bump_block_phrase_hits(hit_counter)
    return {"totalCount": len(items), "items": items}


# ── API: Evidence ─────────────────────────────────────────────────────
#
# Evidence comes from the upstream BitAgent label_evidence table — both
# qBittorrent state polls and *arr webhook/poll history feed it on the
# backend. Configure *arr "Connect" webhooks against the backend's
# /evidence/arr/{sonarr,radarr} endpoint, NOT this UI.

async def _metric_lines() -> dict[str, float]:
    """One /metrics scrape as {full series key: value}."""
    raw = await gql.fetch_metrics()
    return _parse_prometheus_snapshot(raw).get("lines") or {}


def _group_by_labels(
    lines: dict[str, float], name: str, *label_names: str
) -> dict[tuple[str, ...], float]:
    """Sum one metric family into {(label values, ...): total}."""
    out: dict[tuple[str, ...], float] = {}
    for labels, value in _iter_labeled(lines, name):
        key = tuple(labels.get(n) or "" for n in label_names)
        out[key] = out.get(key, 0.0) + value
    return out


@app.get("/api/evidence/sources")
async def api_evidence_sources(identity: dict = Depends(require_operator)):
    """Per-source, per-kind evidence flow — the "is my webhook firing?" answer.

    The raw event list is 99% qBittorrent state polls and the core's
    EvidenceListInput takes only limit/offset, so no amount of paging finds the
    *arr rows that actually matter. These counters are labelled by source AND
    kind, so the question the log cannot answer is answered here directly.

    `duplicated` is not an error: an *arr history poll re-reads the same rows
    every cycle and the core dedupes them. A source whose `persisted` never
    moves while `received` climbs is working.

    `webhookEvents` counts webhook_* events received SINCE CORE BOOT, and that
    is all it can claim. Zero is consistent with no webhook configured against
    /evidence/arr/<instance>, but a correctly wired *arr that simply has not
    grabbed anything since the core restarted reads exactly the same. The UI
    reports the observation and names the check; it must not tell an operator
    their webhook is broken on this evidence alone.
    """
    lines = await _metric_lines()
    families = {
        "received": "bitagent_evidence_events_received_total",
        "persisted": "bitagent_evidence_events_persisted_total",
        "duplicated": "bitagent_evidence_events_duplicated_total",
    }
    if not any(_metric_family_present(lines, f) for f in families.values()):
        return {"available": False, "sources": []}

    rows: dict[tuple[str, str], dict] = {}
    for field, family in families.items():
        for (source, kind), value in _group_by_labels(lines, family, "source", "kind").items():
            row = rows.setdefault(
                (source, kind),
                {"source": source, "kind": kind, "received": 0, "persisted": 0, "duplicated": 0},
            )
            row[field] = int(value)

    by_source: dict[str, dict] = {}
    for row in rows.values():
        entry = by_source.setdefault(
            row["source"],
            {"source": row["source"], "received": 0, "persisted": 0, "duplicated": 0,
             "kinds": [], "webhookEvents": 0},
        )
        for field in ("received", "persisted", "duplicated"):
            entry[field] += row[field]
        entry["kinds"].append(row)
        if row["kind"].startswith("webhook_"):
            entry["webhookEvents"] += row["received"]

    for entry in by_source.values():
        entry["kinds"].sort(key=lambda k: (-k["received"], k["kind"]))
    return {
        "available": True,
        "sources": sorted(by_source.values(), key=lambda e: (-e["received"], e["source"])),
    }


@app.get("/api/wantbridge")
async def api_wantbridge(identity: dict = Depends(require_operator)):
    """Wantbridge status — the core's *arr wantlist bridge.

    This replaced a UI-local `wants` table that nothing downstream ever read:
    every reference to it was this app's own CRUD, so the tab's claim that
    "BitAgent actively routes queries to match these wants" described no code.
    Wantbridge is the mechanism that really does poll Sonarr/Radarr/Lidarr.

    `tier2Observed` reports only what was measured. Tier-2 outcomes occur once
    matches change crawl priority, so their absence is CONSISTENT with
    WANTBRIDGE_ENFORCE=false — but these are since-boot counters, and a core
    that booted recently with enforcement ON and nothing yet qualifying looks
    identical. The core's own config is not reachable from this app, so the
    field is named for what it measures and the UI says "consistent with",
    never "is". Publishing the inference as a fact would send an operator to
    change a setting that may already be correct.
    """
    lines = await _metric_lines()
    if not _metric_family_present(lines, "bitagent_wantbridge_matches_total") and not \
            _metric_family_present(lines, "bitagent_wantbridge_wantlist_size"):
        return {"available": False}

    wantlists = [
        {"source": source, "size": int(size)}
        for (source,), size in _group_by_labels(
            lines, "bitagent_wantbridge_wantlist_size", "source"
        ).items()
    ]
    wantlists.sort(key=lambda w: (-w["size"], w["source"]))

    tiers: dict[str, int] = {}
    by_source: dict[str, int] = {}
    for (source, tier), value in _group_by_labels(
        lines, "bitagent_wantbridge_matches_total", "source", "tier"
    ).items():
        tiers[tier] = tiers.get(tier, 0) + int(value)
        if source:
            by_source[source] = by_source.get(source, 0) + int(value)

    polls = {}
    for source in {w["source"] for w in wantlists}:
        count = _labeled_metric_sum(
            lines, "bitagent_wantbridge_arr_poll_duration_seconds_count", source=source
        )
        total = _labeled_metric_sum(
            lines, "bitagent_wantbridge_arr_poll_duration_seconds_sum", source=source
        )
        polls[source] = {
            "cycles": int(count or 0),
            "avgSeconds": (total / count) if (count and total is not None) else None,
        }

    return {
        "available": True,
        "wantlists": wantlists,
        "wantlistTotal": sum(w["size"] for w in wantlists),
        "fingerprintKeys": int(
            _labeled_metric_sum(lines, "bitagent_wantbridge_fingerprint_keys") or 0
        ),
        "matchesByTier": tiers,
        "matchesBySource": by_source,
        "polls": polls,
        # Tier 2 is the only outcome that exists when matches steer the
        # crawler. Measured, not inferred — see the docstring.
        "tier2Observed": tiers.get("tier2", 0) > 0,
    }


@app.get("/api/evidence")
async def api_evidence(
    limit: int = Query(50, ge=1, le=500),
    offset: int = Query(0, ge=0),
    identity: dict = Depends(require_operator),
):
    return await gql.fetch_evidence_list(limit, offset)


# ── API: Push release to *arr ─────────────────────────────────────────

_ARR_URL_REJECT = (
    "base URL must use http/https and resolve to a routable address "
    "(loopback, link-local, and unspecified addresses are refused)"
)


async def _assert_safe_arr_base_url(base_url: str) -> None:
    """Reject an *arr base URL that would turn our credential-bearing fetch into SSRF.

    The *arr API key is forwarded to whatever host ``base_url`` names, so a value
    like ``http://169.254.169.254/`` (cloud metadata) or ``http://127.0.0.1:9443/``
    (host-local Portainer/qBittorrent — this container runs with host networking)
    would leak the key and reflect the upstream response. The real Sonarr/Radarr/
    Lidarr live on the tailnet (100.64/10), so private/tailnet ranges are allowed;
    only addresses that are never a legitimate *arr here are refused: loopback,
    link-local, unspecified, multicast, reserved.

    ponytail: getaddrinfo here and httpx's own lookup at connect time are two
    resolutions, so a hostname that rebinds between them (TOCTOU) isn't fully
    closed — not built out because the value is operator-set and this already
    blocks the static internal targets. Upgrade to connect-to-resolved-IP pinning
    only if a rebind vector is ever demonstrated.
    """
    parsed = _urlparse_url(base_url)
    if parsed.scheme not in ("http", "https"):
        raise HTTPException(400, _ARR_URL_REJECT)
    host = parsed.hostname
    if not host:
        raise HTTPException(400, _ARR_URL_REJECT)
    port = parsed.port or (443 if parsed.scheme == "https" else 80)
    try:
        infos = await asyncio.get_running_loop().getaddrinfo(
            host, port, type=socket.SOCK_STREAM
        )
    except socket.gaierror:
        raise HTTPException(400, f"base URL host does not resolve: {host}")
    for info in infos:
        ip = ipaddress.ip_address(info[4][0])
        if (
            ip.is_loopback or ip.is_link_local or ip.is_unspecified
            or ip.is_multicast or ip.is_reserved
        ):
            raise HTTPException(400, _ARR_URL_REJECT)


class ArrPushRequest(BaseModel):
    arr: str
    info_hash: str
    title: str


@app.post("/api/arr/push")
async def api_arr_push(body: ArrPushRequest, identity: dict = Depends(require_operator)):
    overrides = await get_all_overrides()

    def _get(key: str) -> str:
        return overrides.get(key) or getattr(settings, key, "") or ""

    if body.arr == "sonarr":
        base_url, api_key = _get("sonarr_base_url"), _get("sonarr_api_key")
    elif body.arr == "radarr":
        base_url, api_key = _get("radarr_base_url"), _get("radarr_api_key")
    elif body.arr == "lidarr":
        base_url, api_key = _get("lidarr_base_url"), _get("lidarr_api_key")
    else:
        raise HTTPException(400, "arr must be 'sonarr', 'radarr', or 'lidarr'")

    if not base_url or not api_key:
        raise HTTPException(
            400,
            f"{body.arr} not configured — set {body.arr}_base_url and {body.arr}_api_key in Settings → Integrations",
        )
    await _assert_safe_arr_base_url(base_url)

    magnet = f"magnet:?xt=urn:btih:{body.info_hash}&dn={_urlquote(body.title)}"
    arr_api_path = "/api/v1/release/push" if body.arr == "lidarr" else "/api/v3/release/push"
    client = gql._get_client()
    try:
        resp = await client.post(
            f"{base_url.rstrip('/')}{arr_api_path}",
            headers={"X-Api-Key": api_key, "Content-Type": "application/json"},
            json={"title": body.title, "downloadUrl": magnet, "protocol": "Torrent", "indexerId": 0},
            follow_redirects=False,
        )
        if not resp.is_success:
            raise HTTPException(502, f"{body.arr} returned HTTP {resp.status_code}: {resp.text[:200]}")
        return {"status": "pushed", "arr": body.arr, "httpStatus": resp.status_code}
    except HTTPException:
        raise
    except Exception as e:
        raise HTTPException(502, f"Failed to reach {body.arr}: {e}")


# ── API: Wants ────────────────────────────────────────────────────────

def _bitagent_api_base() -> str:
    """bitmagnet REST base — sibling of the GraphQL endpoint."""
    return settings.bitagent_graphql_url.removesuffix("/graphql") + "/api"


def _proxy_json(r: httpx.Response) -> JSONResponse:
    """Relay an upstream core response, tolerating a non-JSON body.

    A wedged core (or a reverse proxy in front of it) can answer with an HTML
    502/504 page; r.json() would raise ValueError and surface as an unhandled
    500. Fall back to a structured error carrying the upstream status instead."""
    try:
        return JSONResponse(status_code=r.status_code, content=r.json())
    except ValueError:
        return JSONResponse(
            status_code=r.status_code if r.status_code >= 400 else 502,
            content={"error": "core returned a non-JSON response",
                     "upstreamStatus": r.status_code},
        )


@app.get("/api/quarantine")
async def api_quarantine_list(identity: dict = Depends(require_operator), limit: int = 200, offset: int = 0):
    """Proxy the core's junkpurge quarantine list (the review surface)."""
    try:
        async with httpx.AsyncClient(timeout=15.0) as client:
            r = await client.get(f"{_bitagent_api_base()}/quarantine", params={"limit": limit, "offset": offset})
        return _proxy_json(r)
    except httpx.HTTPError as e:
        raise HTTPException(502, f"quarantine API unreachable: {e}")


@app.post("/api/quarantine/{info_hash}/restore")
async def api_quarantine_restore(info_hash: str, identity: dict = Depends(require_operator)):
    """Restore (undo) a quarantined torrent — re-insert + re-classify."""
    try:
        async with httpx.AsyncClient(timeout=30.0) as client:
            r = await client.post(f"{_bitagent_api_base()}/quarantine/{info_hash}/restore")
        return _proxy_json(r)
    except httpx.HTTPError as e:
        raise HTTPException(502, f"quarantine API unreachable: {e}")


@app.delete("/api/quarantine/{info_hash}")
async def api_quarantine_delete(info_hash: str, identity: dict = Depends(require_operator)):
    """Permanently delete + blacklist a quarantined torrent now."""
    try:
        async with httpx.AsyncClient(timeout=15.0) as client:
            r = await client.request("DELETE", f"{_bitagent_api_base()}/quarantine/{info_hash}")
        return _proxy_json(r)
    except httpx.HTTPError as e:
        raise HTTPException(502, f"quarantine API unreachable: {e}")


# ── API: Settings ─────────────────────────────────────────────────────

class SettingUpdate(BaseModel):
    value: str


@app.get("/api/settings")
async def api_settings(identity: dict = Depends(require_operator)):
    overrides = await get_all_overrides()
    fields = {}
    for key in sorted(MUTABLE_FIELDS):
        default = str(getattr(settings, key, ""))
        overridden = key in overrides
        current = overrides.get(key, default)
        if key in SENSITIVE_FIELDS:
            # The browser gets state, never credential material (or lengths).
            fields[key] = {
                "sensitive": True,
                "configured": bool(current),
                "source": "override" if overridden else ("startup" if default else "unset"),
                "overridden": overridden,
            }
        else:
            fields[key] = {
                "sensitive": False,
                "default": default,
                "current": current,
                "source": "override" if overridden else "startup",
                "overridden": overridden,
            }
    return {"fields": fields, "mutable_keys": sorted(MUTABLE_FIELDS)}


@app.get("/api/auth/tiers")
async def api_auth_tiers(identity: dict = Depends(require_operator)):
    """Startup-only auth status — booleans only, no secrets/CIDRs/role names."""
    return describe_active_tiers()


@app.put("/api/settings/overrides/{key}")
async def api_set_override(key: str, body: SettingUpdate, identity: dict = Depends(require_operator)):
    if key not in MUTABLE_FIELDS:
        raise HTTPException(403, f"Field '{key}' is not mutable")
    # Reject an SSRF-shaped *arr base URL at config time (best UX); the fetch
    # sites re-check to blunt set-time -> fetch-time DNS rebinding.
    if key in ARR_BASE_URL_FIELDS and body.value.strip():
        await _assert_safe_arr_base_url(body.value)
    actor = str(identity.get("id") or "unknown")
    result = await set_override(key, body.value, actor=actor)
    if key in SENSITIVE_FIELDS:
        return {
            "key": key,
            "sensitive": True,
            "configured": bool(body.value),
            "source": "override",
            "overridden": True,
            "actor": actor,
            "at": result["at"],
        }
    return result


@app.delete("/api/settings/overrides/{key}")
async def api_delete_override(key: str, identity: dict = Depends(require_operator)):
    if key not in MUTABLE_FIELDS:
        raise HTTPException(403, f"Field '{key}' is not mutable")
    actor = str(identity.get("id") or "unknown")
    ok = await delete_override(key, actor=actor)
    if not ok:
        raise HTTPException(404, "Override not found")
    return {"status": "deleted"}


@app.get("/api/settings/audit")
async def api_audit(
    limit: int = Query(100, ge=1, le=1000),
    identity: dict = Depends(require_operator),
):
    return await get_audit_log(limit)


# ── API: Notifications ────────────────────────────────────────────────

@app.get("/api/notifications")
async def api_notifications(identity: dict = Depends(require_operator)):
    db = await get_db()
    rows = await db.execute_fetchall(
        "SELECT id, level, title, message, read, created_at FROM notifications ORDER BY created_at DESC LIMIT 50"
    )
    return [
        {"id": r[0], "level": r[1], "title": r[2], "message": r[3], "read": bool(r[4]), "at": r[5]}
        for r in rows
    ]


@app.put("/api/notifications/{nid}/read")
async def api_mark_read(nid: int, identity: dict = Depends(require_operator)):
    db = await get_db()
    await db.execute("UPDATE notifications SET read = 1 WHERE id = ?", (nid,))
    await db.commit()
    return {"status": "ok"}


# ── API: TMDB enriched metadata (public library detail page) ──────────

@app.get("/api/meta/{media_type}/{tmdb_id}")
async def api_meta_details(
    media_type: str, tmdb_id: str, identity: dict = Depends(require_auth)
):
    """Enriched TMDB details for the library detail hero: backdrop, overview,
    genres, rating, runtime, cast, and (for TV) the season catalog. Cached
    server-side. 404 when TMDB is unconfigured or the id is unknown."""
    if media_type not in ("movie", "tv", "tv_show"):
        raise HTTPException(400, "media_type must be 'movie' or 'tv'")
    data = await tmdb.get_details(tmdb_id, media_type)
    if not data:
        raise HTTPException(404, "Metadata not available")
    return data


@app.get("/api/meta/tv/{tmdb_id}/season/{season_number}")
async def api_meta_season(
    tmdb_id: str, season_number: int, identity: dict = Depends(require_auth)
):
    """Episode catalog for one TV season (title, air date, still, overview)."""
    data = await tmdb.get_season(tmdb_id, season_number)
    if not data:
        raise HTTPException(404, "Season metadata not available")
    return data


# ── API: TMDB poster ─────────────────────────────────────────────────

@app.get("/api/poster/{poster_id:path}")
async def api_poster(poster_id: str, media_type: str = "movie", source: str = "tmdb", redirect: bool = False, identity: dict = Depends(require_auth)):
    from urllib.parse import quote as _q
    if source == "mb":
        data = await tmdb.get_mb_cover(poster_id)
    elif source == "lidarr":
        overrides = await get_all_overrides()
        lidarr_url = overrides.get("lidarr_base_url") or getattr(settings, "lidarr_base_url", "") or ""
        lidarr_key = overrides.get("lidarr_api_key") or getattr(settings, "lidarr_api_key", "") or ""
        # Pass a proxy transformer so Lidarr's internal image URLs are rewritten to
        # /api/poster-proxy?url=... before being stored in the cache and returned.
        # This ensures browsers never receive a raw internal URL regardless of cache state.
        def _lidarr_proxy(url: str) -> str:
            return f"/api/poster-proxy?url={_q(url)}" if url.startswith("http") else url
        data = await tmdb.get_lidarr_cover(poster_id, lidarr_url, lidarr_key, url_transform=_lidarr_proxy) if lidarr_url and lidarr_key else None
    else:
        data = await tmdb.get_poster(poster_id, media_type)
    if data:
        # `redirect=1` is used by <img loading="lazy" src="/api/poster/…?redirect=1">
        # on the library grid: resolve the id (served from the poster cache after the
        # first hit) and 302 to the real cover so the browser loads it natively —
        # no client-side hydration. The redirect is cacheable for a day.
        if redirect:
            poster_url = data.get("poster_url")
            if poster_url:
                return RedirectResponse(
                    poster_url,
                    status_code=302,
                    headers={"Cache-Control": "public, max-age=86400"},
                )
            return _blank_poster()
        return data
    if redirect:
        return _blank_poster()
    raise HTTPException(404, "Poster not found")


from fastapi.responses import StreamingResponse
from urllib.parse import urlparse as _urlparse


_PROXY_MAX_BYTES = 10 * 1024 * 1024  # 10 MB ceiling for a cover-art image


@app.get("/api/poster-proxy")
async def api_poster_proxy(url: str, identity: dict = Depends(require_auth)):
    """Proxy a Lidarr media-cover image through bitagent.

    SSRF mitigations:
    - scheme must be http or https
    - host:port must match the configured Lidarr base URL exactly
    - redirects are disabled (follow_redirects=False)
    - only image/* content-type responses are returned
    - response body is capped at 10 MB
    """
    parsed = _urlparse(url)
    if parsed.scheme not in ("http", "https"):
        raise HTTPException(400, "Invalid URL scheme")

    overrides = await get_all_overrides()
    lidarr_base = overrides.get("lidarr_base_url") or getattr(settings, "lidarr_base_url", "") or ""
    if not lidarr_base:
        raise HTTPException(403, "Lidarr not configured")
    lidarr_netloc = _urlparse(lidarr_base).netloc
    # Strict host:port match — rejects any URL not from the configured Lidarr host.
    if not lidarr_netloc or parsed.netloc != lidarr_netloc:
        raise HTTPException(403, "URL not from configured Lidarr host")

    client = tmdb._get_client()
    try:
        resp = await client.get(url, follow_redirects=False)
        # Explicitly reject redirects; redirect targets are not validated.
        if resp.is_redirect:
            raise HTTPException(403, "Redirects not permitted")
        if not resp.is_success:
            raise HTTPException(404, "Image not found")
        ct = resp.headers.get("content-type", "")
        if not ct.startswith("image/"):
            raise HTTPException(415, "Unexpected content type")
        # Reject via Content-Length before reading the body to avoid loading
        # an oversized payload into memory.
        cl_hdr = resp.headers.get("content-length", "")
        if cl_hdr.isdigit() and int(cl_hdr) > _PROXY_MAX_BYTES:
            raise HTTPException(413, "Image exceeds size limit")
        body = resp.content
        if len(body) > _PROXY_MAX_BYTES:
            raise HTTPException(413, "Image exceeds size limit")
        return StreamingResponse(iter([body]), media_type=ct)
    except httpx.HTTPError:
        raise HTTPException(502, "Failed to fetch image")


# ── API: GraphQL passthrough (for explorer) ───────────────────────────

class GQLRequest(BaseModel):
    query: str
    variables: dict | None = None


@app.post("/api/graphql")
async def api_graphql_proxy(body: GQLRequest, identity: dict = Depends(require_operator)):
    return await gql.query(body.query, body.variables)


# ── API: Filters status (read-only view of bitagent core contentfilter) ──

@app.get("/api/filters/status")
async def api_filters_status(identity: dict = Depends(require_operator)):
    """Aggregate counts emitted by the bitagent core's contentfilter chain.
    Read-only — toggling/configuring lives in env vars on the core stack.
    Live drops and shadow-mode would-drops stay separate: collapsing them made
    a busy shadow filter look disabled. Transport failure is also explicit
    rather than being flattened into a page of zeroes."""
    raw = await gql.fetch_metrics()
    metric_sum: dict[str, float] = {}
    blocked_ext: dict[str, int] = {}
    drop_reason: dict[str, int] = {}
    would_drop_reason: dict[str, int] = {}
    families: set[str] = set()
    for line in raw.splitlines():
        if not line or line.startswith("#"):
            continue
        parts = line.split(" ", 1)
        if len(parts) != 2:
            continue
        key, val = parts
        try:
            v = float(val)
        except ValueError:
            continue
        bare = key.split("{", 1)[0]
        families.add(bare)
        metric_sum[bare] = metric_sum.get(bare, 0.0) + v
        if bare == "bitagent_contentfilter_blocked_ext_total" and "ext=\"" in key:
            ext = key.split("ext=\"", 1)[1].split("\"", 1)[0]
            blocked_ext[ext] = blocked_ext.get(ext, 0) + int(v)
        if bare == "bitagent_contentfilter_drop_total" and "reason=\"" in key:
            reason = key.split("reason=\"", 1)[1].split("\"", 1)[0]
            drop_reason[reason] = drop_reason.get(reason, 0) + int(v)
        if bare == "bitagent_contentfilter_would_drop_total" and "reason=\"" in key:
            reason = key.split("reason=\"", 1)[1].split("\"", 1)[0]
            would_drop_reason[reason] = would_drop_reason.get(reason, 0) + int(v)

    available = any(name.startswith("bitagent_contentfilter_") for name in families)
    examined = int(metric_sum.get("bitagent_contentfilter_examined_total", 0))
    live_total = sum(drop_reason.values())
    would_total = sum(would_drop_reason.values())
    if not available:
        status = _stage_status(
            "unavailable", "Unavailable", "danger",
            "Core content-filter metrics could not be read.",
        )
    elif live_total > 0 and would_total > 0:
        status = _stage_status(
            "mixed", "Mixed history", "warning",
            f"{live_total:,} enforced drops and {would_total:,} shadow would-drops were "
            "recorded since core boot; current configuration is not exposed.",
        )
    elif live_total > 0:
        status = _stage_status(
            "active", "Live effects observed", "success",
            f"{live_total:,} enforced drops were recorded since core boot.",
        )
    elif would_total > 0:
        status = _stage_status(
            "shadow", "Shadow", "warning",
            f"{would_total:,} would-drop decisions and no enforced drops were recorded "
            "since core boot; the evaluated rows were retained.",
        )
    elif examined > 0:
        status = _stage_status(
            "active", "Evaluating", "info",
            f"{examined:,} torrents were examined with no drop decision recorded.",
        )
    else:
        status = _stage_status(
            "inactive", "No activity observed", "neutral",
            "No content-filter evaluations were recorded since core boot.",
        )

    return {
        "available": available,
        "status": status,
        "examined": examined,
        "liveDropTotal": live_total,
        "wouldDropTotal": would_total,
        "drops": {
            "blocked_extension": drop_reason.get("blocked_extension", 0),
            "non_latin_script": drop_reason.get("non_latin_script", 0),
            "nsfw_keyword": drop_reason.get("nsfw_keyword", 0),
        },
        "wouldDrops": {
            "blocked_extension": would_drop_reason.get("blocked_extension", 0),
            "non_latin_script": would_drop_reason.get("non_latin_script", 0),
            "nsfw_keyword": would_drop_reason.get("nsfw_keyword", 0),
        },
        "blockedExtensions": [
            {"ext": ext, "count": blocked_ext[ext]}
            for ext in sorted(blocked_ext.keys(), key=lambda k: -blocked_ext[k])
        ],
        "csam": {
            "blocklistEntries": int(metric_sum.get("bitagent_csam_blocklist_entries", 0)),
            "lookups": int(metric_sum.get("bitagent_csam_blocklist_lookups_total", 0)),
            "exports": int(metric_sum.get("bitagent_csam_blocklist_export_total", 0)),
        },
    }


# ── API: Operator block phrases (CRUD) ───────────────────────────────

class BlockPhrasePayload(BaseModel):
    pattern: str
    note: str = ""


@app.get("/api/block-phrases")
async def api_block_phrases_list(identity: dict = Depends(require_operator)):
    db = await get_db()
    cur = await db.execute(
        "SELECT id, pattern, scope, note, hits, created_at "
        "FROM block_phrases ORDER BY hits DESC, id DESC"
    )
    rows = await cur.fetchall()
    await cur.close()
    return {
        "items": [
            {
                "id": r[0],
                "pattern": r[1],
                "scope": r[2],
                "note": r[3] or "",
                "hits": r[4],
                "createdAt": r[5],
            }
            for r in rows
        ]
    }


@app.post("/api/block-phrases")
async def api_block_phrases_create(
    payload: BlockPhrasePayload, identity: dict = Depends(require_operator)
):
    pattern = payload.pattern.strip()
    if not pattern:
        raise HTTPException(400, "pattern must be non-empty")
    if len(pattern) > 200:
        raise HTTPException(400, "pattern must be ≤200 chars")
    db = await get_db()
    try:
        cur = await db.execute(
            "INSERT INTO block_phrases (pattern, scope, note, hits, created_at) "
            "VALUES (?, 'title', ?, 0, ?)",
            (pattern, payload.note.strip(), time.time()),
        )
        await db.commit()
        _invalidate_block_phrases_cache()
        return {"id": cur.lastrowid, "pattern": pattern}
    except Exception as e:
        if "UNIQUE" in str(e):
            raise HTTPException(409, "pattern already exists")
        raise


@app.delete("/api/block-phrases/{phrase_id}")
async def api_block_phrases_delete(
    phrase_id: int, identity: dict = Depends(require_operator)
):
    db = await get_db()
    cur = await db.execute("DELETE FROM block_phrases WHERE id = ?", (phrase_id,))
    await db.commit()
    if cur.rowcount == 0:
        raise HTTPException(404, "phrase not found")
    _invalidate_block_phrases_cache()
    return {"deleted": phrase_id}



if __name__ == "__main__":
    import uvicorn
    uvicorn.run(
        "app:app", host=settings.host, port=settings.port,
        reload=True, proxy_headers=False,
    )

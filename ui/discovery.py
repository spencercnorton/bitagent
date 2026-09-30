"""Cached TMDB discovery and country-specific JustWatch provider catalogues.

Discovery metadata does not establish that a release exists in the index. The
library checks its exact content identity when a discovered title is opened.
"""
from __future__ import annotations

import asyncio
import hashlib
import math
import re
import time
from collections import OrderedDict
from datetime import datetime, timezone
from itertools import zip_longest
from typing import Literal

import httpx
from fastapi import APIRouter, Depends, HTTPException, Query

import tmdb
import network_catalog
from provider_catalog import group_providers
from auth import require_auth
from config import settings

router = APIRouter()
_CACHE_LIMIT = 512
_INFLIGHT_LIMIT = 128
_FEED_TTL = 600.0
_PROVIDER_TTL = 24 * 3600.0
_PROVIDER_CHECK_BUDGET = 7.0
_UPSTREAM_REQUEST_BUDGET = 7.0
_CACHE: OrderedDict[tuple, tuple[float, dict]] = OrderedDict()
_INFLIGHT: dict[tuple, asyncio.Task] = {}
_SEMAPHORE: asyncio.Semaphore | None = None
_NETWORK_PAGE_SIZE = 24
# Canonical identities from TMDB's network index; names/logos still come from
# current metadata. Other networks remain searchable in the full daily index.
_FEATURED_NETWORKS = (
    49, 67, 88, 174, 74, 64, 2, 16, 6, 19, 4, 71, 47, 41, 68, 56,
    80, 65, 43, 30, 33, 2076, 13, 77, 129, 210, 143,
)


class DiscoveryUnavailable(Exception):
    """A safe public error, without an upstream URL or authentication secret."""


def reset_cache() -> None:
    """Cancel process-owned requests and release cache state at app lifecycle boundaries."""
    global _SEMAPHORE
    for task in _INFLIGHT.values():
        task.cancel()
    _INFLIGHT.clear()
    _CACHE.clear()
    _SEMAPHORE = None
    network_catalog.reset_cache()


async def _cached_get(path: str, params: dict, ttl: float) -> dict:
    global _SEMAPHORE
    credential = settings.tmdb_api_key
    if not credential:
        raise DiscoveryUnavailable("not_configured")
    key = (hashlib.sha256(credential.encode()).hexdigest(), path, tuple(sorted(params.items())))
    cached = _CACHE.get(key)
    if cached and time.monotonic() - cached[0] < ttl:
        _CACHE.move_to_end(key)
        return cached[1]
    if key not in _INFLIGHT:
        if len(_INFLIGHT) >= _INFLIGHT_LIMIT:
            raise DiscoveryUnavailable("upstream_unavailable")
        if _SEMAPHORE is None:
            _SEMAPHORE = asyncio.Semaphore(4)
        semaphore = _SEMAPHORE

        async def request() -> dict:
            async with semaphore:
                try:
                    response = await tmdb._get_client().get(
                        f"https://api.themoviedb.org/3/{path}",
                        params={**params, "api_key": credential},
                        timeout=httpx.Timeout(5.0, connect=2.0),
                    )
                    response.raise_for_status()
                    payload = response.json()
                    if not isinstance(payload, dict) or payload.get("success") is False:
                        raise DiscoveryUnavailable("upstream_unavailable")
                except (httpx.HTTPError, ValueError, TypeError):
                    raise DiscoveryUnavailable("upstream_unavailable") from None
                _CACHE[key] = (time.monotonic(), payload)
                _CACHE.move_to_end(key)
                while len(_CACHE) > _CACHE_LIMIT:
                    _CACHE.popitem(last=False)
                return payload

        async def fetch() -> dict:
            try:
                # Include time waiting for the shared four-request limit.
                # A disconnected caller must not leave an unbounded queue.
                return await asyncio.wait_for(request(), timeout=_UPSTREAM_REQUEST_BUDGET)
            except TimeoutError:
                raise DiscoveryUnavailable("upstream_unavailable") from None

        task = asyncio.create_task(fetch())
        _INFLIGHT[key] = task

        def finished(done: asyncio.Task) -> None:
            if _INFLIGHT.get(key) is done:
                _INFLIGHT.pop(key, None)
            if not done.cancelled():
                done.exception()  # Retrieve errors even when a caller disconnected.

        task.add_done_callback(finished)
    return await asyncio.shield(_INFLIGHT[key])


def _number(value, default=0):
    return value if isinstance(value, (int, float)) and not isinstance(value, bool) and math.isfinite(value) else default


def _image(path, size: str) -> str | None:
    if not isinstance(path, str) or not re.fullmatch(r"/[a-zA-Z\d_-]+\.(?:jpg|png|webp|svg)", path):
        return None
    if path.endswith(".svg"):
        size = "original"
    return f"https://image.tmdb.org/t/p/{size}{path}"


def _items(payload: dict, media_type: str) -> list[dict]:
    results = payload.get("results")
    if not isinstance(results, list):
        raise DiscoveryUnavailable("upstream_unavailable")
    items = []
    seen = set()
    for row in results[:20]:
        if not isinstance(row, dict) or row.get("adult") is True:
            continue
        identifier = row.get("id")
        title = row.get("title") if media_type == "movie" else row.get("name")
        if not isinstance(identifier, int) or isinstance(identifier, bool) or identifier <= 0:
            continue
        if identifier in seen or not isinstance(title, str) or not title.strip():
            continue
        seen.add(identifier)
        date = row.get("release_date") if media_type == "movie" else row.get("first_air_date")
        date = date if isinstance(date, str) and re.fullmatch(r"\d{4}-\d{2}-\d{2}", date) else ""
        original = row.get("original_title") if media_type == "movie" else row.get("original_name")
        overview = row.get("overview")
        items.append({
            "id": str(identifier), "type": "movie" if media_type == "movie" else "tv_show",
            "title": title[:500], "originalTitle": original[:500] if isinstance(original, str) else "",
            "year": date[:4], "releaseDate": date,
            "overview": overview[:3000] if isinstance(overview, str) else "",
            "poster": _image(row.get("poster_path"), "w500"),
            "backdrop": _image(row.get("backdrop_path"), "w1280"),
            "popularity": _number(row.get("popularity")), "voteAverage": _number(row.get("vote_average")),
            "availability": "unchecked", "source": "tmdb",
        })
    return items


def _unavailable(reason: str, **fields) -> dict:
    return {"available": False, "reason": reason, "items": [], **fields}


async def get_providers(region: str = "US") -> dict:
    if not settings.tmdb_api_key:
        return _unavailable("not_configured", region=region, regions=[], providers=[])
    try:
        results = await asyncio.gather(*(
            _cached_get(f"watch/providers/{media_type}", {"watch_region": region, "language": "en-US"}, _PROVIDER_TTL)
            for media_type in ("movie", "tv")
        ))
        country_payload = await _cached_get("watch/providers/regions", {"language": "en-US"}, _PROVIDER_TTL)
        countries = country_payload.get("results")
        if not isinstance(countries, list):
            raise DiscoveryUnavailable("upstream_unavailable")
        regions = sorted([
            {"code": row["iso_3166_1"], "name": row["english_name"]}
            for row in countries if isinstance(row, dict)
            and isinstance(row.get("iso_3166_1"), str) and re.fullmatch(r"[A-Z]{2}", row["iso_3166_1"])
            and isinstance(row.get("english_name"), str)
        ], key=lambda row: row["name"])
        if not any(row["code"] == region for row in regions):
            raise HTTPException(400, "Unsupported provider region")
        providers: dict[int, dict] = {}
        for media_type, payload in zip(("movie", "tv_show"), results):
            rows = payload.get("results")
            if not isinstance(rows, list):
                raise DiscoveryUnavailable("upstream_unavailable")
            for row in rows:
                if not isinstance(row, dict):
                    continue
                identifier, name = row.get("provider_id"), row.get("provider_name")
                if not isinstance(identifier, int) or isinstance(identifier, bool) or identifier <= 0:
                    continue
                if not isinstance(name, str) or not name.strip():
                    continue
                priorities = row.get("display_priorities") or {}
                priority = priorities.get(region, row.get("display_priority", 999)) if isinstance(priorities, dict) else 999
                provider = providers.setdefault(identifier, {
                    "id": identifier, "name": name[:200], "logo": _image(row.get("logo_path"), "w92"),
                    "displayPriority": _number(priority, 999), "types": [], "logoAlternatives": [], "aliases": [],
                })
                if name[:200] not in provider["aliases"]:
                    provider["aliases"].append(name[:200])
                logo = _image(row.get("logo_path"), "w92")
                if not provider["logo"] and logo:
                    provider["logo"] = logo
                elif logo and logo != provider["logo"] and logo not in provider["logoAlternatives"]:
                    provider["logoAlternatives"].append(logo)
                provider["displayPriority"] = min(provider["displayPriority"], _number(priority, 999))
                if media_type not in provider["types"]:
                    provider["types"].append(media_type)
        ordered = group_providers(list(providers.values()))
        return {"available": True, "region": region, "regions": regions, "providers": ordered,
                "attribution": "Provider availability: JustWatch via TMDB"}
    except DiscoveryUnavailable as exc:
        return _unavailable(str(exc), region=region, regions=[], providers=[])


async def _network_details(identifier: int) -> dict:
    payload = await _cached_get(f"network/{identifier}", {}, _PROVIDER_TTL)
    if (not isinstance(payload.get("id"), int) or isinstance(payload.get("id"), bool)
            or payload["id"] != identifier
            or not isinstance(payload.get("name"), str) or not payload["name"].strip()):
        raise DiscoveryUnavailable("upstream_unavailable")
    logo = _image(payload.get("logo_path"), "w185")
    alternatives = []
    if not logo:
        try:
            images = await _cached_get(f"network/{identifier}/images", {}, _PROVIDER_TTL)
            for row in images.get("logos", []) if isinstance(images.get("logos"), list) else []:
                candidate = _image(row.get("file_path"), "w185") if isinstance(row, dict) else None
                if candidate and candidate not in alternatives:
                    alternatives.append(candidate)
            if alternatives:
                logo = alternatives.pop(0)
        except DiscoveryUnavailable:
            pass
    country = payload.get("origin_country")
    return {"id": identifier, "name": payload["name"].strip()[:200], "logo": logo,
            "logoAlternatives": alternatives[:4],
            "country": country if isinstance(country, str) and re.fullmatch(r"[A-Z]{2}", country) else ""}


async def get_networks(*, q: str = "", page: int = 1) -> dict:
    fields = {"q": q.strip(), "page": page, "hasNext": False, "hasNextPage": False,
              "total": 0, "partial": False, "networks": []}
    if not settings.tmdb_api_key:
        return _unavailable("not_configured", **fields)
    try:
        rows = await asyncio.wait_for(network_catalog.get_index(), timeout=_PROVIDER_CHECK_BUDGET)
        query = q.strip().casefold()
        if query:
            rows = [row for row in rows if query in row["name"].casefold()]
        else:
            rank = {identifier: index for index, identifier in enumerate(_FEATURED_NETWORKS)}
            rows = sorted(rows, key=lambda row: (rank.get(row["id"], len(rank)), row["name"].casefold(), row["id"]))
        fields["total"] = len(rows)
        fields["hasNext"] = fields["hasNextPage"] = page * _NETWORK_PAGE_SIZE < len(rows)
        window = rows[(page - 1) * _NETWORK_PAGE_SIZE:page * _NETWORK_PAGE_SIZE]

        async def resolve(row: dict) -> dict | None:
            try:
                return await asyncio.wait_for(_network_details(row["id"]), timeout=_PROVIDER_CHECK_BUDGET)
            except (DiscoveryUnavailable, TimeoutError):
                return None

        resolved = await asyncio.gather(*(resolve(row) for row in window))
        fields["networks"] = [row for row in resolved if row is not None]
        fields["partial"] = any(row is None for row in resolved)
        return {"available": True, **fields,
                "attribution": "TV network metadata: TMDB. Network association is not current streaming availability."}
    except (network_catalog.NetworkIndexUnavailable, DiscoveryUnavailable, TimeoutError):
        return _unavailable("upstream_unavailable", **fields)


async def _network_match(item: dict, network: int) -> bool | None:
    try:
        payload = await asyncio.wait_for(
            _cached_get(f"tv/{item['id']}", {}, _PROVIDER_TTL), timeout=_PROVIDER_CHECK_BUDGET)
        if (not isinstance(payload.get("id"), int) or isinstance(payload.get("id"), bool)
                or str(payload["id"]) != item["id"]):
            return None
        rows = payload.get("networks")
        if not isinstance(rows, list):
            return None
        return any(isinstance(row, dict) and isinstance(row.get("id"), int)
                   and not isinstance(row.get("id"), bool) and row["id"] == network for row in rows)
    except (DiscoveryUnavailable, TimeoutError):
        return None


async def _provider_match(item: dict, provider_ids: list[int], region: str) -> bool | None:
    media_type = "movie" if item["type"] == "movie" else "tv"
    try:
        payload = await asyncio.wait_for(
            _cached_get(f"{media_type}/{item['id']}/watch/providers", {}, _PROVIDER_TTL),
            timeout=_PROVIDER_CHECK_BUDGET,
        )
        countries = payload.get("results")
        if not isinstance(countries, dict):
            return None
        market = countries.get(region, {})
        if not isinstance(market, dict):
            return None
        found = False
        for kind in ("flatrate", "free", "ads", "rent", "buy"):
            rows = market.get(kind, [])
            if not isinstance(rows, list):
                return None
            found = found or any(isinstance(row, dict) and isinstance(row.get("provider_id"), int)
                                 and not isinstance(row["provider_id"], bool)
                                 and row["provider_id"] in provider_ids for row in rows)
        return found
    except (DiscoveryUnavailable, TimeoutError):
        return None


async def get_discovery(*, provider: int | None = None, network: int | None = None, region: str = "US",
                        type: str = "movie", q: str = "", page: int = 1, mode: str = "popular") -> dict:
    if provider and network:
        raise HTTPException(400, "Choose a streaming service or TV network")
    if network:
        if type == "movie":
            raise HTTPException(400, "TV networks can only filter series")
        type = "tv_show"
    fields = {"provider": provider, "network": network, "region": region, "type": type, "q": q.strip(),
              "page": page, "mode": mode, "hasNextPage": False, "partial": False}
    if not settings.tmdb_api_key:
        return _unavailable("not_configured", **fields)
    selected = None
    if provider:
        catalogue = await get_providers(region)
        if not catalogue["available"]:
            return _unavailable(catalogue["reason"], **fields)
        selected = next((row for row in catalogue["providers"] if provider in row["providerIds"]), None)
        if selected is None:
            raise HTTPException(400, "Provider is not available in this region")
        fields["providerName"] = selected["name"]
        fields["providerIds"] = selected["providerIds"] if type == "all" else selected["idsByType"].get(type, [])
    if type == "all":
        types = selected["types"] if selected else ["movie", "tv_show"]
        feeds = await asyncio.gather(*(
            get_discovery(provider=provider, region=region, type=media_type, q=q, page=page, mode=mode)
            for media_type in types
        ))
        fields["hasNextPage"] = any(feed["hasNextPage"] for feed in feeds)
        fields["partial"] = any(not feed["available"] or feed["partial"] for feed in feeds)
        items = [item for row in zip_longest(*(feed["items"] for feed in feeds)) for item in row if item]
        if not any(feed["available"] for feed in feeds):
            return _unavailable("upstream_unavailable", **fields)
        return {"available": True, "items": items, **fields,
                "attribution": "Provider availability: JustWatch via TMDB"}
    media_type = "tv" if type == "tv_show" else "movie"
    params = {"language": "en-US", "page": page, "include_adult": "false"}
    try:
        if network:
            selected_network = await asyncio.wait_for(
                _network_details(network), timeout=_PROVIDER_CHECK_BUDGET)
            fields["networkName"] = selected_network["name"]
        if provider:
            if not fields["providerIds"]:
                return {"available": True, "items": [], **fields,
                        "attribution": "Provider availability: JustWatch via TMDB"}
        if q.strip():
            params["query"] = q.strip()
            path = f"search/{media_type}"
        else:
            path = f"discover/{media_type}"
            params["sort_by"] = "popularity.desc" if mode == "popular" else (
                "first_air_date.desc" if media_type == "tv" else "primary_release_date.desc")
            if mode == "newest":
                date_field = "first_air_date.lte" if media_type == "tv" else "primary_release_date.lte"
                params[date_field] = datetime.now(timezone.utc).date().isoformat()
                params["vote_count.gte"] = 5
            if provider:
                params["with_watch_providers"] = "|".join(map(str, fields["providerIds"]))
                params["watch_region"] = region
            if network:
                params["with_networks"] = str(network)
        payload = await _cached_get(path, params, _FEED_TTL)
        items = _items(payload, media_type)
        pages = payload.get("total_pages")
        known_pages = isinstance(pages, int) and not isinstance(pages, bool) and pages >= 0
        fields["hasNextPage"] = page < min(pages, 50) if known_pages else len(items) == 20 and page < 50
        fields["partial"] = not known_pages or (pages > 50 and page == 50)
        if q.strip() and provider:
            checks = await asyncio.gather(*(_provider_match(item, fields["providerIds"], region) for item in items))
            fields["partial"] = fields["partial"] or any(check is None for check in checks)
            # Unknown attribution never passes a provider filter. One upstream
            # search page can be empty after filtering and still have a next.
            items = [item for item, match in zip(items, checks) if match is True]
            fields["searchScope"] = "provider_checked_search_page"
        if q.strip() and network:
            checks = await asyncio.gather(*(_network_match(item, network) for item in items))
            fields["partial"] = fields["partial"] or any(check is None for check in checks)
            items = [item for item, match in zip(items, checks) if match is True]
            fields["searchScope"] = "network_checked_search_page"
        return {"available": True, "items": items, **fields,
                "attribution": ("TV network metadata: TMDB. Network association is not current streaming availability."
                                if network else "Provider availability: JustWatch via TMDB")}
    except TimeoutError:
        return _unavailable("upstream_unavailable", **fields)
    except DiscoveryUnavailable as exc:
        return _unavailable(str(exc), **fields)


async def get_spotlight(region: str = "US") -> dict:
    if not settings.tmdb_api_key:
        return _unavailable("not_configured", region=region)
    feeds = await asyncio.gather(*(
        get_discovery(type=media_type, mode=mode, region=region)
        for mode in ("popular", "newest") for media_type in ("movie", "tv_show")
    ))
    items, seen = [], set()
    for row in zip_longest(*(feed["items"] for feed in feeds)):
        for item, feed in zip(row, feeds):
            if item is None:
                continue
            key = (item["type"], item["id"])
            if key in seen:
                continue
            seen.add(key)
            items.append({**item, "mode": feed["mode"]})
            if len(items) == 8:
                break
        if len(items) == 8:
            break
    return {"available": any(feed["available"] for feed in feeds), "items": items,
            "region": region, "partial": not all(feed["available"] for feed in feeds), "source": "tmdb"}


@router.get("/api/discovery/providers")
async def api_providers(region: str = Query("US", pattern=r"^[A-Z]{2}$"),
                        identity: dict = Depends(require_auth)):
    return await get_providers(region)


@router.get("/api/discovery/spotlight")
async def api_spotlight(region: str = Query("US", pattern=r"^[A-Z]{2}$"),
                        identity: dict = Depends(require_auth)):
    return await get_spotlight(region)


@router.get("/api/discovery/networks")
async def api_networks(q: str = Query("", max_length=200), page: int = Query(1, ge=1, le=1000),
                       identity: dict = Depends(require_auth)):
    return await get_networks(q=q, page=page)


@router.get("/api/discovery")
async def api_discovery(provider: int | None = Query(None, ge=1, le=2_147_483_647),
                        network: int | None = Query(None, ge=1, le=2_147_483_647),
                        region: str = Query("US", pattern=r"^[A-Z]{2}$"),
                        type: Literal["all", "movie", "tv_show"] = "movie",
                        q: str = Query("", max_length=200), page: int = Query(1, ge=1, le=50),
                        mode: Literal["popular", "newest"] = "popular",
                        identity: dict = Depends(require_auth)):
    return await get_discovery(provider=provider, network=network, region=region, type=type, q=q, page=page, mode=mode)

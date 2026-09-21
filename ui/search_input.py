"""Pure helpers for building the core's torrentContent.search input + shaping
its items back into the UI's API shape.

Extracted verbatim from app.py (behavior-identical). No app state, no I/O — just
parsing request params and constructing/reshaping dicts.
"""
from __future__ import annotations


def _csv_values(value: str) -> list[str]:
    return [v.strip() for v in (value or "").split(",") if v.strip()]


def _year_values(value: str) -> list[int]:
    years = []
    for raw in _csv_values(value):
        try:
            year = int(raw)
        except ValueError:
            continue
        if 1800 <= year <= 3000:
            years.append(year)
    return years


def _resolution_values(value: str) -> list[str]:
    out = []
    for raw in _csv_values(value):
        v = raw.strip()
        lower = v.lower()
        if lower in {"4k", "uhd"}:
            out.append("V2160p")
        elif lower and lower[0].isdigit():
            out.append("V" + lower)
        else:
            out.append(v)
    return out


def _add_search_facet(
    facets: dict,
    name: str,
    values: list | None,
    *,
    aggregate: bool = False,
) -> None:
    if not values and not aggregate:
        return
    entry = facets.setdefault(name, {})
    if values:
        entry["filter"] = values
    if aggregate:
        entry["aggregate"] = True


_ORDER_BY = {
    "seeders": [{"field": "seeders", "descending": True}],
    "newest": [{"field": "published_at", "descending": True}],
    # Grouped-pagination sorts must be server-side (the client only ever holds
    # one page); these two existed client-side only before v1.29.0.
    "size": [{"field": "size", "descending": True}],
    "name": [{"field": "name", "descending": False}],
}


def _content_type_values(content_types: str, content_type: str, default: list[str] | None = None) -> list[str]:
    effective = content_types or content_type  # content_type is deprecated alias
    values = [t.strip() for t in effective.split(",") if t.strip()]
    return values or list(default or [])


def _search_input(
    *,
    q: str,
    limit: int,
    offset: int,
    order: str,
    types: list[str],
    genre_values: list[str],
    year_values: list[int],
    quality_values: list[str],
    source_values: list[str],
    aggregate_facets: bool,
    total_count: bool,
    group_by_content: bool = False,
    has_next_page: bool = False,
) -> dict:
    search_input: dict = {
        "queryString": q,
        "limit": min(limit, 500),
        "offset": offset,
        "totalCount": total_count,
    }
    if group_by_content:
        # Core >= v0.43: one row per title (highest-seeded representative),
        # matched-only corpus, grouped totalCount (usually an estimate).
        search_input["groupByContent"] = True
    if has_next_page:
        search_input["hasNextPage"] = True
    if order in _ORDER_BY:
        search_input["orderBy"] = _ORDER_BY[order]
    facets: dict = {}
    _add_search_facet(facets, "contentType", types, aggregate=aggregate_facets)
    _add_search_facet(facets, "genre", genre_values, aggregate=aggregate_facets)
    _add_search_facet(facets, "releaseYear", year_values, aggregate=aggregate_facets)
    _add_search_facet(facets, "videoResolution", quality_values, aggregate=aggregate_facets)
    _add_search_facet(facets, "videoSource", source_values, aggregate=aggregate_facets)
    if facets:
        search_input["facets"] = facets
    return search_input


def _api_item_from_search_item(it: dict) -> dict:
    torrent = it.get("torrent") or {}
    content = it.get("content") or {}
    original_language = content.get("originalLanguage") or {}
    title = (it.get("title") or torrent.get("name") or "")
    langs = [lang.get("id") for lang in (it.get("languages") or []) if lang.get("id")]
    is_mapped = bool(it.get("contentSource"))
    return {
        "infoHash": it.get("infoHash"),
        "name": title,
        "torrentName": torrent.get("name") or title,
        "title": it.get("title"),
        "size": torrent.get("size") or 0,
        "filesCount": torrent.get("filesCount") or 0,
        "seeders": it.get("seeders") or 0,
        "leechers": it.get("leechers") or 0,
        "contentType": it.get("contentType"),
        "contentSource": it.get("contentSource"),
        "contentId": it.get("contentId"),
        "isMapped": is_mapped,
        "languages": langs,
        "videoResolution": it.get("videoResolution"),
        "videoSource": it.get("videoSource"),
        "releaseGroup": it.get("releaseGroup"),
        "originalLanguage": original_language.get("id") or "",
        "originalLanguageName": original_language.get("name") or "",
        "originalTitle": content.get("originalTitle") or "",
        "discoveredAt": it.get("createdAt"),
        "updatedAt": it.get("updatedAt"),
    }

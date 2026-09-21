from __future__ import annotations
import json
import time
import httpx
from config import settings
from database import get_db, read_conn

TMDB_IMG_BASE = "https://image.tmdb.org/t/p/w300"
# Per-asset image sizes for the rich library detail page. Posters go a notch
# larger than the grid thumbnail; backdrops fill the hero; stills/profiles are
# small inline thumbnails.
TMDB_POSTER_BASE = "https://image.tmdb.org/t/p/w500"
TMDB_BACKDROP_BASE = "https://image.tmdb.org/t/p/w1280"
TMDB_STILL_BASE = "https://image.tmdb.org/t/p/w300"
TMDB_PROFILE_BASE = "https://image.tmdb.org/t/p/w185"

# Enriched detail/season payloads are far more stable than a live torrent list,
# so cache them for a week. A missing backdrop today rarely appears tomorrow.
_META_TTL_SECONDS = 7 * 24 * 3600

_client: httpx.AsyncClient | None = None


def _img(base: str, path: str | None) -> str | None:
    return f"{base}{path}" if path else None


async def _meta_cache_get(cache_key: str) -> dict | None:
    async with read_conn() as db:
        rows = await db.execute_fetchall(
            "SELECT payload, fetched_at FROM tmdb_meta_cache WHERE cache_key = ?",
            (cache_key,),
        )
    if not rows:
        return None
    if (time.time() - (rows[0][1] or 0)) > _META_TTL_SECONDS:
        return None  # stale — let the caller refetch
    try:
        return json.loads(rows[0][0])
    except (ValueError, TypeError):
        return None


async def _meta_cache_put(cache_key: str, payload: dict) -> None:
    db = await get_db()
    await db.execute(
        "INSERT OR REPLACE INTO tmdb_meta_cache (cache_key, payload, fetched_at) VALUES (?, ?, ?)",
        (cache_key, json.dumps(payload), time.time()),
    )
    await db.commit()


async def get_details(tmdb_id: str, media_type: str = "movie") -> dict | None:
    """Full TMDB detail payload for the library hero + metadata block.

    Normalises the movie/tv shape difference (title vs name, release_date vs
    first_air_date, runtime vs episode_run_time) into one dict. For TV it also
    carries the season catalog so the frontend can render a season switcher
    without a second round-trip. Cached in SQLite for a week.
    """
    if not settings.tmdb_api_key or not tmdb_id:
        return None
    mt = "tv" if media_type in ("tv", "tv_show") else "movie"
    cache_key = f"details:{mt}:{tmdb_id}"
    cached = await _meta_cache_get(cache_key)
    if cached is not None:
        return cached

    client = _get_client()
    try:
        resp = await client.get(
            f"https://api.themoviedb.org/3/{mt}/{tmdb_id}",
            params={"api_key": settings.tmdb_api_key, "append_to_response": "credits"},
        )
        resp.raise_for_status()
        d = resp.json()
    except httpx.HTTPError:
        return None

    runtime = d.get("runtime")
    if not runtime:
        ert = d.get("episode_run_time") or []
        runtime = ert[0] if ert else None

    cast = [
        {
            "name": c.get("name"),
            "character": c.get("character"),
            "profile": _img(TMDB_PROFILE_BASE, c.get("profile_path")),
        }
        for c in ((d.get("credits") or {}).get("cast") or [])[:12]
        if c.get("name")
    ]

    seasons = []
    if mt == "tv":
        for s in d.get("seasons") or []:
            seasons.append({
                "seasonNumber": s.get("season_number"),
                "name": s.get("name"),
                "episodeCount": s.get("episode_count") or 0,
                "airDate": s.get("air_date"),
                "overview": s.get("overview") or "",
                "poster": _img(TMDB_POSTER_BASE, s.get("poster_path")),
            })

    payload = {
        "tmdbId": str(tmdb_id),
        "mediaType": mt,
        "title": d.get("title") or d.get("name") or "",
        "originalTitle": d.get("original_title") or d.get("original_name") or "",
        "tagline": d.get("tagline") or "",
        "overview": d.get("overview") or "",
        "year": (d.get("release_date") or d.get("first_air_date") or "")[:4],
        "releaseDate": d.get("release_date") or d.get("first_air_date") or "",
        "genres": [g.get("name") for g in (d.get("genres") or []) if g.get("name")],
        "runtime": runtime,
        "voteAverage": d.get("vote_average") or 0,
        "voteCount": d.get("vote_count") or 0,
        "status": d.get("status") or "",
        "poster": _img(TMDB_POSTER_BASE, d.get("poster_path")),
        "backdrop": _img(TMDB_BACKDROP_BASE, d.get("backdrop_path")),
        "homepage": d.get("homepage") or "",
        "numberOfSeasons": d.get("number_of_seasons"),
        "numberOfEpisodes": d.get("number_of_episodes"),
        "seasons": seasons,
        "cast": cast,
    }
    await _meta_cache_put(cache_key, payload)
    return payload


async def get_season(tmdb_id: str, season_number: int) -> dict | None:
    """Episode catalog for one TV season (episode title, air date, still,
    overview, rating). Cached in SQLite for a week."""
    if not settings.tmdb_api_key or not tmdb_id:
        return None
    cache_key = f"season:{tmdb_id}:{season_number}"
    cached = await _meta_cache_get(cache_key)
    if cached is not None:
        return cached

    client = _get_client()
    try:
        resp = await client.get(
            f"https://api.themoviedb.org/3/tv/{tmdb_id}/season/{season_number}",
            params={"api_key": settings.tmdb_api_key},
        )
        resp.raise_for_status()
        d = resp.json()
    except httpx.HTTPError:
        return None

    episodes = [
        {
            "episodeNumber": e.get("episode_number"),
            "seasonNumber": e.get("season_number", season_number),
            "name": e.get("name") or "",
            "overview": e.get("overview") or "",
            "airDate": e.get("air_date"),
            "runtime": e.get("runtime"),
            "voteAverage": e.get("vote_average") or 0,
            "still": _img(TMDB_STILL_BASE, e.get("still_path")),
        }
        for e in (d.get("episodes") or [])
    ]
    payload = {
        "tmdbId": str(tmdb_id),
        "seasonNumber": season_number,
        "name": d.get("name") or f"Season {season_number}",
        "overview": d.get("overview") or "",
        "airDate": d.get("air_date"),
        "poster": _img(TMDB_POSTER_BASE, d.get("poster_path")),
        "episodes": episodes,
    }
    await _meta_cache_put(cache_key, payload)
    return payload


def _get_client() -> httpx.AsyncClient:
    global _client
    if _client is None:
        _client = httpx.AsyncClient(timeout=10.0)
    return _client


async def get_poster(tmdb_id: str, media_type: str = "movie") -> dict | None:
    if not settings.tmdb_api_key or not tmdb_id:
        return None

    # TMDB movie and TV ids are separate number spaces, so the cache key carries
    # the media type like the mb:/lidarr: rows below; pre-namespace rows (bare
    # id) are simply misses that refetch once.
    cache_key = f"tmdb:{media_type}:{tmdb_id}"
    async with read_conn() as db:
        rows = await db.execute_fetchall(
            "SELECT poster_url, title, year FROM poster_cache WHERE tmdb_id = ?",
            (cache_key,),
        )
    if rows:
        return {"poster_url": rows[0][0], "title": rows[0][1], "year": rows[0][2]}

    client = _get_client()
    try:
        resp = await client.get(
            f"https://api.themoviedb.org/3/{media_type}/{tmdb_id}",
            params={"api_key": settings.tmdb_api_key},
        )
        resp.raise_for_status()
        data = resp.json()
        poster_path = data.get("poster_path")
        poster_url = f"{TMDB_IMG_BASE}{poster_path}" if poster_path else None
        title = data.get("title") or data.get("name", "")
        year = (data.get("release_date") or data.get("first_air_date") or "")[:4]

        db = await get_db()
        await db.execute(
            "INSERT OR REPLACE INTO poster_cache (tmdb_id, poster_url, title, year, fetched_at) VALUES (?, ?, ?, ?, ?)",
            (cache_key, poster_url, title, year, time.time()),
        )
        await db.commit()
        return {"poster_url": poster_url, "title": title, "year": year}
    except httpx.HTTPError:
        return None


async def get_mb_cover(mbid: str) -> dict | None:
    """Fetch album art from MusicBrainz Cover Art Archive, keyed as 'mb:{mbid}'."""
    if not mbid:
        return None
    cache_key = f"mb:{mbid}"
    async with read_conn() as db:
        rows = await db.execute_fetchall(
            "SELECT poster_url, title, year FROM poster_cache WHERE tmdb_id = ?", (cache_key,)
        )
    if rows:
        return {"poster_url": rows[0][0], "title": rows[0][1], "year": rows[0][2]}

    client = _get_client()
    for entity in ("release-group", "release"):
        try:
            resp = await client.get(
                f"https://coverartarchive.org/{entity}/{mbid}/front-250",
                follow_redirects=True,
                headers={"User-Agent": "BitAgent/1.0 (music-cover-art)"},
            )
            if resp.status_code == 200 and resp.headers.get("content-type", "").startswith("image/"):
                poster_url = str(resp.url)
                db = await get_db()
                await db.execute(
                    "INSERT OR REPLACE INTO poster_cache (tmdb_id, poster_url, title, year, fetched_at) VALUES (?, ?, ?, ?, ?)",
                    (cache_key, poster_url, "", "", time.time()),
                )
                await db.commit()
                return {"poster_url": poster_url, "title": "", "year": ""}
        except httpx.HTTPError:
            continue
    return None


import re as _re


def _lidarr_artist_name(torrent_name: str) -> str:
    """Normalize a torrent name to an artist name suitable for Lidarr matching."""
    name = torrent_name.split(" - ")[0].strip() if " - " in torrent_name else torrent_name
    return _re.sub(r"\s*[\(\[]\d{4}[\)\]].*$", "", name).strip()


async def get_lidarr_cover(
    torrent_name: str,
    lidarr_url: str,
    lidarr_key: str,
    url_transform=None,
) -> dict | None:
    """Search Lidarr for an artist matching the torrent name and return their poster art.

    url_transform: optional callable applied to the raw Lidarr image URL before caching.
    Pass a proxy transformer so cached URLs are browser-accessible from day one.
    """
    if not torrent_name or not lidarr_url or not lidarr_key:
        return None
    artist_name = _lidarr_artist_name(torrent_name)
    if not artist_name:
        return None

    cache_key = f"lidarr:{artist_name.lower()}"
    async with read_conn() as db:
        rows = await db.execute_fetchall(
            "SELECT poster_url, title, year FROM poster_cache WHERE tmdb_id = ?", (cache_key,)
        )
    if rows:
        return {"poster_url": rows[0][0], "title": rows[0][1], "year": rows[0][2]}

    client = _get_client()
    try:
        resp = await client.get(
            f"{lidarr_url.rstrip('/')}/api/v1/artist",
            headers={"X-Api-Key": lidarr_key},
        )
        resp.raise_for_status()
        artists = resp.json() or []
        name_lower = artist_name.lower()
        match = next(
            (a for a in artists if name_lower in (a.get("artistName") or "").lower()),
            None,
        )
        if not match:
            return None
        images = match.get("images") or []
        poster = next((img for img in images if img.get("coverType") == "poster"), None)
        if not poster:
            poster = images[0] if images else None
        if not poster:
            return None
        # Lidarr image URLs are relative; build the full URL
        img_url = poster.get("url") or poster.get("remoteUrl") or ""
        if img_url.startswith("/"):
            img_url = f"{lidarr_url.rstrip('/')}{img_url}"
        if not img_url:
            return None
        # Apply url_transform (e.g. a proxy rewrite) before caching so every
        # stored URL is browser-accessible without further post-processing.
        stored_url = url_transform(img_url) if url_transform else img_url
        title = match.get("artistName") or artist_name
        db = await get_db()
        await db.execute(
            "INSERT OR REPLACE INTO poster_cache (tmdb_id, poster_url, title, year, fetched_at) VALUES (?, ?, ?, ?, ?)",
            (cache_key, stored_url, title, "", time.time()),
        )
        await db.commit()
        return {"poster_url": stored_url, "title": title, "year": ""}
    except httpx.HTTPError:
        return None

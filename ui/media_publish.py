"""Prepare a private media catalog offline; never publish or seed production data.

Run ``python media_publish.py --manifest inventory.json --root /media/library
--announce https://tracker.example.org/announce --output prepared``. Input uses
version=1 and releases with source_id, title, kind and a relative files array.
Movies, episodes, music and generic releases have explicit file sets;
season/show releases are packs. Plex music discovery emits track versions.
The catalog contains portable metainfo for a protected importer. The separate
seeding map is local preparation data, never an HTTP response or public asset.
"""
from __future__ import annotations

import argparse
import base64
import fcntl
import json
import os
import re
import sqlite3
import stat
import uuid
from pathlib import Path, PurePosixPath
from urllib.parse import urlsplit
from urllib.request import HTTPRedirectHandler, ProxyHandler, Request, build_opener

from torrent_metainfo import (
    DEFAULT_PIECE_LENGTH, MAX_FILES, TorrentError, build_private_torrent,
    relative_media_path, safe_component, validate_announce_url, verify_torrent_files,
    BuiltTorrent, HashPacer, source_snapshot, parse_metainfo, info_hash, adaptive_piece_length,
)
from private_media import KINDS

MAX_MANIFEST_BYTES = 16 * 1024 * 1024
MAX_CATALOG_BYTES = 128 * 1024 * 1024
MAX_RELEASES = 1_000_000
MAX_IMPORT_RELEASES = 1000
MAX_IMPORT_BYTES = 4 * 1024 * 1024


def _inventory(value: dict) -> list[dict]:
    if not isinstance(value, dict) or value.get("version") != 1:
        raise TorrentError("inventory version must be 1")
    releases = value.get("releases")
    if not isinstance(releases, list) or not 1 <= len(releases) <= MAX_RELEASES:
        raise TorrentError("inventory release count is outside the supported limit")
    seen = set()
    for release in releases:
        if not isinstance(release, dict):
            raise TorrentError("invalid inventory release")
        source_id = release.get("source_id")
        if not isinstance(source_id, str) or not re.fullmatch(r"[A-Za-z0-9_.:-]{1,159}", source_id):
            raise TorrentError("release needs a bounded source_id")
        if source_id in seen:
            raise TorrentError("duplicate source_id")
        seen.add(source_id)
        title = release.get("title")
        if not isinstance(title, str) or not title or len(title) > 500 or any(ord(c) < 32 for c in title):
            raise TorrentError("release needs a bounded title")
        if release.get("kind") not in KINDS:
            raise TorrentError("unsupported media kind")
        files = release.get("files")
        if not isinstance(files, list) or not 1 <= len(files) <= MAX_FILES:
            raise TorrentError("release needs a bounded files array")
        for field in ("season", "tmdb_id", "tvdb_id"):
            if field in release and (type(release[field]) is not int or release[field] < 0):
                raise TorrentError("numeric media identifiers must be nonnegative integers")
        if "episodes" in release and (not isinstance(release["episodes"], list)
                or len(release["episodes"]) > MAX_FILES
                or any(type(n) is not int or n < 0 for n in release["episodes"])):
            raise TorrentError("episodes must be a bounded list of nonnegative integers")
        if "imdb_id" in release and (not isinstance(release["imdb_id"], str)
                or not release["imdb_id"].startswith("tt") or not release["imdb_id"][2:].isdigit()
                or len(release["imdb_id"]) > 20):
            raise TorrentError("invalid IMDb identifier")
    return releases



def inventory_from_plex(payloads: list[dict], root: Path, path_maps: list[tuple[str, str]] | None = None,
                        include_packs: bool = True) -> dict:
    """Map real Plex media parts to movie/episode/music leaves and TV packs.

    Path maps pair an absolute Plex/container prefix with a relative path under
    the configured root. With no maps, Plex paths must already be below root.
    Multiple encodings remain distinct releases. Packs use one deterministic
    version per episode, avoiding duplicate versions in the same download.
    Missing or unmapped parts fail the whole inventory instead of omitting media.
    """
    root = Path(root).resolve(strict=True)
    maps = path_maps or [(root.as_posix(), "")]
    normalized = []
    for source, destination in maps:
        prefix = PurePosixPath(source)
        if not prefix.is_absolute() or ".." in prefix.parts:
            raise TorrentError("Plex path mapping needs an absolute source prefix")
        dest = () if destination == "" else relative_media_path(destination)
        normalized.append((prefix, dest))
    normalized.sort(key=lambda pair: len(pair[0].parts), reverse=True)

    def source_path(filename):
        if not isinstance(filename, str):
            raise TorrentError("Plex part has no file path")
        actual = PurePosixPath(filename)
        if not actual.is_absolute() or ".." in actual.parts:
            raise TorrentError("Plex part needs an absolute file path")
        for prefix, destination in normalized:
            if actual.is_relative_to(prefix):
                mapped = "/".join((*destination, *actual.relative_to(prefix).parts))
                relative_media_path(mapped)
                return mapped
        raise TorrentError("Plex part is outside configured media mappings")

    def ids(metadata):
        identifiers = {}
        for guid in metadata.get("Guid", []):
            identifier = guid.get("id", "")
            if identifier.startswith("imdb://tt"):
                identifiers["imdb_id"] = identifier[7:]
            for provider in ("tmdb", "tvdb"):
                if identifier.startswith(provider + "://") and identifier[len(provider) + 3:].isdigit():
                    identifiers[provider + "_id"] = int(identifier[len(provider) + 3:])
        return identifiers

    show_ids = {str(metadata.get("ratingKey")): ids(metadata) for metadata in payloads if metadata.get("type") == "show"}
    releases = []
    preferred_episodes = {}
    seen = set()
    for metadata in payloads:
        plex_kind = metadata.get("type")
        if plex_kind not in {"movie", "episode", "track"}:
            continue
        kind = "music" if plex_kind == "track" else plex_kind
        rating_key = str(metadata.get("ratingKey") or "")
        if not rating_key:
            raise TorrentError("Plex media has no stable rating key")
        media_versions = metadata.get("Media")
        if not isinstance(media_versions, list) or not media_versions:
            raise TorrentError("Plex media has no downloadable parts")
        # Torznab tvdbid/tmdbid on TV searches names the series, not the episode.
        identifiers = {}
        if kind == "movie":
            identifiers = ids(metadata)
        elif kind == "episode":
            identifiers = show_ids.get(str(metadata.get("grandparentRatingKey")), {})
        for version_number, media in enumerate(media_versions):
            parts = media.get("Part")
            if not isinstance(parts, list) or not parts:
                raise TorrentError("Plex media version has no parts")
            files = [source_path(part.get("file")) for part in parts]
            source_id = "plex:" + rating_key + ":" + str(media.get("id", version_number))
            if source_id in seen:
                raise TorrentError("duplicate Plex media identity")
            seen.add(source_id)
            title = str(metadata.get("title") or "")
            if kind == "music":
                context = [metadata.get("originalTitle") or metadata.get("grandparentTitle"),
                           metadata.get("parentTitle"), title]
                if (not isinstance(metadata.get("title"), str) or not title.strip()
                        or any(value is not None and not isinstance(value, str) for value in context)):
                    raise TorrentError("Plex track has invalid title context")
                title = " - ".join(value.strip() for value in context if value and value.strip())
            release = {"source_id": source_id, "kind": kind, "title": title, "files": files, **identifiers}
            if kind == "episode":
                if type(metadata.get("parentIndex")) is not int or type(metadata.get("index")) is not int:
                    raise TorrentError("Plex episode lacks season or episode number")
                season = metadata["parentIndex"]
                episode = metadata["index"]
                show_key = str(metadata.get("grandparentRatingKey") or "")
                show_title = str(metadata.get("grandparentTitle") or "")
                if not show_key or not show_title:
                    raise TorrentError("Plex episode lacks a parent show identity")
                release.update(season=season, episodes=[episode], series_source_id=show_key, series_title=show_title, title=f"{show_title} S{season:02d}E{episode:02d} {title}".strip())
                preferred_episodes.setdefault((show_key, season, episode), (show_title, release))
            releases.append(release)
    if include_packs:
        seasons = {}
        shows = {}
        for (show_key, season, episode), (show_title, release) in sorted(preferred_episodes.items()):
            seasons.setdefault((show_key, season), (show_title, []))[1].append(release)
            shows.setdefault(show_key, (show_title, []))[1].append(release)
        for (show_key, season), (title, episodes) in seasons.items():
            releases.append({"source_id": f"plex:season:{show_key}:{season}", "title": f"{title} Season {season}",
                             "kind": "season", "season": season,
                             "episodes": sorted({n for release in episodes for n in release["episodes"]}),
                             "files": sorted({path for release in episodes for path in release["files"]}), **show_ids.get(show_key, {})})
        for show_key, (title, episodes) in shows.items():
            releases.append({"source_id": f"plex:show:{show_key}", "title": title, "kind": "show",
                             "files": sorted({path for release in episodes for path in release["files"]}), **show_ids.get(show_key, {})})
    inventory = {"version": 1, "releases": releases}
    _inventory(inventory)
    return inventory


class _NoRedirect(HTTPRedirectHandler):
    def redirect_request(self, request, fp, code, message, headers, new_url):
        raise TorrentError("Plex redirects are refused")


def _plex_page(result: dict, start: int, previous_total) -> tuple[list[dict], int | None, bool]:
    """Reject truncated or contradictory pages before any rows are emitted."""
    rows = result.get("Metadata", [])
    if not isinstance(rows, list) or any(not isinstance(row, dict) for row in rows) or len(rows) > 250:
        raise TorrentError("invalid Plex inventory page")
    total = result.get("totalSize")
    if "totalSize" in result and (type(total) is not int or total < 0):
        raise TorrentError("invalid Plex pagination total")
    if previous_total != "first" and total != previous_total:
        raise TorrentError("Plex pagination total changed during inventory")
    if "offset" in result and (type(result["offset"]) is not int or result["offset"] != start):
        raise TorrentError("Plex pagination offset differs from its request")
    if "size" in result and (type(result["size"]) is not int or result["size"] != len(rows)):
        raise TorrentError("Plex pagination size differs from returned rows")
    consumed = start + len(rows)
    if total is not None and (consumed > total or (not rows and start < total)):
        raise TorrentError("Plex pagination ended before its declared total or exceeded it")
    finished = consumed == total if total is not None else len(rows) < 250
    return rows, total, finished


def fetch_plex_inventory(base_url: str, token: str, root: Path,
                         path_maps: list[tuple[str, str]] | None = None,
                         include_packs: bool = True, include_music: bool = False) -> dict:
    """Read video/track inventory from Plex; no library or downloader mutations."""
    parsed = urlsplit(base_url)
    if (parsed.scheme not in {"http", "https"} or not parsed.hostname or parsed.username or parsed.password
            or parsed.query or parsed.fragment or parsed.path not in {"", "/"}
            or not token or any(ord(c) < 33 for c in base_url + token)):
        raise TorrentError("invalid Plex connection configuration")
    opener = build_opener(ProxyHandler({}), _NoRedirect())

    def get(path, start=0):
        request = Request(base_url.rstrip("/") + path, headers={"Accept": "application/json", "X-Plex-Token": token,
                          "X-Plex-Container-Start": str(start), "X-Plex-Container-Size": "250"})
        with opener.open(request, timeout=30) as response:
            body = response.read(MAX_MANIFEST_BYTES + 1)
            if len(body) > MAX_MANIFEST_BYTES:
                raise TorrentError("Plex response exceeds supported size")
        payload = json.loads(body).get("MediaContainer")
        if not isinstance(payload, dict):
            raise TorrentError("invalid Plex inventory response")
        return payload

    sections = get("/library/sections").get("Directory", [])
    payloads = []
    for section in sections:
        if section.get("type") not in ({"movie", "show", "artist"} if include_music else {"movie", "show"}):
            continue
        key = str(section.get("key") or "")
        if not key.isdigit():
            raise TorrentError("invalid Plex section identity")
        for media_type in {"movie": (1,), "show": (2, 4), "artist": (10,)}[section["type"]]:
            start = 0
            previous_page = None
            previous_total = "first"
            for _ in range(400):
                page = get(f"/library/sections/{key}/all?type={media_type}&includeGuids=1", start)
                rows, total, finished = _plex_page(page, start, previous_total)
                if section["type"] == "artist" and any(row.get("type") != "track" for row in rows):
                    raise TorrentError("Plex music section returned a non-track asset")
                previous_total = total
                page_identity = tuple(str(row.get("ratingKey")) for row in rows)
                if rows and page_identity == previous_page:
                    raise TorrentError("Plex pagination did not advance")
                previous_page = page_identity
                payloads.extend(rows)
                start += len(rows)
                if len(payloads) > MAX_RELEASES:
                    raise TorrentError("Plex library exceeds supported batch size")
                if finished:
                    break
            else:
                raise TorrentError("Plex pagination exceeded supported batch size")
    return inventory_from_plex(payloads, root, path_maps, include_packs)



def iter_plex_releases(base_url: str, token: str, root: Path,
                       path_maps: list[tuple[str, str]] | None = None,
                       include_packs: bool = True, section_ids: set[str] | None = None,
                       include_music: bool = False):
    """Page Plex metadata and audit every unmapped/empty asset explicitly.

    Media blobs are bounded to a page. TV pack aggregation retains only titles,
    identifiers and distinct file paths for one section, never encoded torrents.
    A uniform approved root permits genuine packs across its mounted subtrees.
    Music sections require explicit opt-in; existing video discovery stays scoped.
    """
    parsed = urlsplit(base_url)
    if (parsed.scheme not in {"http", "https"} or not parsed.hostname or parsed.username or parsed.password
            or parsed.query or parsed.fragment or parsed.path not in {"", "/"}
            or not token or any(ord(c) < 33 for c in base_url + token)):
        raise TorrentError("invalid Plex connection configuration")
    if section_ids is not None and (not section_ids or any(not value.isdigit() for value in section_ids)):
        raise TorrentError("invalid requested Plex sections")
    opener = build_opener(ProxyHandler({}), _NoRedirect())

    def get(path, start=0):
        request = Request(base_url.rstrip("/") + path, headers={"Accept": "application/json", "X-Plex-Token": token,
                          "X-Plex-Container-Start": str(start), "X-Plex-Container-Size": "250"})
        with opener.open(request, timeout=30) as response:
            body = response.read(MAX_MANIFEST_BYTES + 1)
            if len(body) > MAX_MANIFEST_BYTES:
                raise TorrentError("Plex response exceeds supported size")
        result = json.loads(body).get("MediaContainer")
        if not isinstance(result, dict):
            raise TorrentError("invalid Plex inventory response")
        return result

    def pages(section, media_type):
        start = 0
        previous = None
        previous_total = "first"
        for _ in range(4000):
            result = get(f"/library/sections/{section}/all?type={media_type}&includeGuids=1", start)
            rows, total, finished = _plex_page(result, start, previous_total)
            previous_total = total
            identity = tuple(str(row.get("ratingKey")) for row in rows)
            if rows and identity == previous:
                raise TorrentError("Plex pagination did not advance")
            previous = identity
            yield rows
            start += len(rows)
            if start > MAX_RELEASES:
                raise TorrentError("Plex section exceeds supported streaming limit")
            if finished:
                return
        raise TorrentError("Plex pagination exceeded supported streaming limit")

    def reject(source_id, kind, reason):
        return {"_rejection": True, "source_id": source_id, "kind": kind, "reason": reason}

    def identifiers(metadata):
        result = {}
        for guid in metadata.get("Guid", []):
            value = guid.get("id", "")
            if value.startswith("imdb://tt"):
                result["imdb_id"] = value[7:]
            for provider in ("tmdb", "tvdb"):
                prefix = provider + "://"
                if value.startswith(prefix) and value[len(prefix):].isdigit():
                    result[provider + "_id"] = int(value[len(prefix):])
        return result

    sections = get("/library/sections").get("Directory", [])
    section_types = {"movie", "show", "artist"} if include_music else {"movie", "show"}
    available = {str(section.get("key")) for section in sections if section.get("type") in section_types}
    if section_ids is not None and not section_ids <= available:
        raise TorrentError("requested Plex media section is missing")
    yield {"_inventory_event": True, "counts": {}, "scope": {"source": "plex", "include_packs": include_packs,
           "requested_sections": sorted(section_ids) if section_ids else "all video and music sections" if include_music else "all video sections"}}
    for section in sections:
        section_id = str(section.get("key", ""))
        if section.get("type") not in section_types or (section_ids is not None and section_id not in section_ids):
            continue
        if not section_id.isdigit():
            raise TorrentError("invalid Plex section identity")
        show_metadata = {}
        season_metadata = {}
        season_files = {}
        show_files = {}
        preferred = set()
        missing_episodes = set()
        if section["type"] == "show":
            for rows in pages(section_id, 2):
                yield {"_inventory_event": True, "section_id": section_id, "counts": {"show": len(rows)}}
                for row in rows:
                    key = str(row.get("ratingKey") or "")
                    if not key:
                        yield reject(f"plex:missing-show:{section_id}", "show", "Plex show has no stable identity")
                    elif key in show_metadata:
                        yield reject("plex:show:" + key, "show", "duplicate Plex show identity")
                    else:
                        show_metadata[key] = row
            for rows in pages(section_id, 3):
                yield {"_inventory_event": True, "section_id": section_id, "counts": {"season": len(rows)}}
                for row in rows:
                    key = (str(row.get("parentRatingKey") or ""), row.get("index"))
                    if not key[0] or type(key[1]) is not int:
                        yield reject("plex:season:" + str(row.get("ratingKey") or section_id), "season", "Plex season lacks its parent identity or number")
                    elif key in season_metadata:
                        yield reject(f"plex:season:{key[0]}:{key[1]}", "season", "duplicate Plex season identity")
                    else:
                        season_metadata[key] = row
        media_type, kind = {"movie": (1, "movie"), "show": (4, "episode"), "artist": (10, "music")}[section["type"]]
        for rows in pages(section_id, media_type):
            yield {"_inventory_event": True, "section_id": section_id, "counts": {kind: len(rows)}}
            for row in rows:
                rating_key = str(row.get("ratingKey") or "missing")
                if kind == "music" and row.get("type") != "track":
                    yield reject("plex:" + rating_key, kind, "Plex music section returned a non-track asset")
                    continue
                versions = row.get("Media")
                valid = False
                if not isinstance(versions, list) or not versions:
                    yield reject("plex:" + rating_key, kind, "Plex asset has no materialized media version")
                else:
                    for version_number, media in enumerate(versions):
                        try:
                            if not isinstance(media, dict):
                                raise TorrentError("invalid Plex media version")
                            version = {**media, "id": media.get("id", version_number)}
                            leaf = {**row, "Media": [version]}
                            parent = show_metadata.get(str(row.get("grandparentRatingKey") or "")) if kind == "episode" else None
                            metadata = [parent, leaf] if parent is not None else [leaf]
                            release = inventory_from_plex(metadata, root, path_maps, False)["releases"][0]
                            valid = True
                            yield release
                            if kind == "episode":
                                show_key = release["series_source_id"]
                                season = release["season"]
                                episode = release["episodes"][0]
                                identity = (show_key, season, episode)
                                if identity not in preferred:
                                    preferred.add(identity)
                                    season_files.setdefault((show_key, season), {"files": set(), "episodes": set()})["files"].update(release["files"])
                                    season_files[(show_key, season)]["episodes"].add(episode)
                                    show_files.setdefault(show_key, set()).update(release["files"])
                        except (TorrentError, KeyError, TypeError) as exc:
                            yield reject(f"plex:{rating_key}:{version_number}", kind, str(exc) if isinstance(exc, TorrentError) else "invalid Plex part metadata")
                if kind == "episode" and not valid:
                    missing_episodes.add((str(row.get("grandparentRatingKey") or ""), row.get("parentIndex")))
        if not include_packs and section["type"] == "show":
            for show_key, season in season_metadata:
                yield {"_exclusion": True, "source_id": f"plex:season:{show_key}:{season}", "kind": "season", "reason": "packs disabled by operator"}
            for show_key in show_metadata:
                yield {"_exclusion": True, "source_id": "plex:show:" + show_key, "kind": "show", "reason": "packs disabled by operator"}
        if include_packs and section["type"] == "show":
            for show_key, season in sorted(set(season_metadata) | set(season_files)):
                title = str(show_metadata.get(show_key, {}).get("title") or season_metadata.get((show_key, season), {}).get("parentTitle") or "")
                source_id = f"plex:season:{show_key}:{season}"
                pack = season_files.get((show_key, season))
                if not pack or (show_key, season) in missing_episodes:
                    yield reject(source_id, "season", "season has missing or no materialized episode files")
                else:
                    yield {"source_id": source_id, "title": f"{title} Season {season}", "kind": "season", "season": season,
                           "episodes": sorted(pack["episodes"]), "files": sorted(pack["files"]), **identifiers(show_metadata.get(show_key, {}))}
            for show_key, metadata in show_metadata.items():
                source_id = "plex:show:" + show_key
                files = show_files.get(show_key)
                if not files or any(key[0] == show_key for key in missing_episodes):
                    yield reject(source_id, "show", "show has missing or no materialized episode files")
                else:
                    yield {"source_id": source_id, "title": str(metadata.get("title") or ""), "kind": "show", "files": sorted(files), **identifiers(metadata)}


def _release_layout(root: Path, release: dict) -> tuple[str, tuple[str, ...], str, bool]:
    paths = sorted(relative_media_path(path) for path in release["files"])
    if len(paths) == 1:
        name = paths[0][-1]
        prefix = ()
        save_path = "/".join(paths[0][:-1])
        save_parent = False
    else:
        common = list(paths[0][:-1])
        for path in paths[1:]:
            common = common[:min(len(common), len(path) - 1)]
            while tuple(common) != path[:len(common)]:
                common.pop()
        prefix = tuple(common)
        name = prefix[-1] if prefix else root.resolve(strict=True).name
        save_path = "/".join(prefix[:-1])
        save_parent = not prefix
    if release.get("torrent_name", name) != name:
        raise TorrentError("torrent_name must match the existing file or shared directory")
    safe_component(name)
    return name, prefix, save_path, save_parent


def prepare_catalog(inventory: dict, root: Path, announce_url: str,
                    piece_length: int = DEFAULT_PIECE_LENGTH) -> tuple[dict, dict, dict[str, bytes]]:
    """Prepare a bounded in-memory fixture/small catalog; CLI uses publish_stream."""
    validate_announce_url(announce_url)
    catalog = {"version": 1, "releases": []}
    seeding = {"version": 1, "releases": []}
    artifacts = {}
    encoded_size = 0
    for release in _inventory(inventory):
        files = release["files"]
        name, prefix, save_path, save_parent = _release_layout(root, release)
        built = build_private_torrent(root, files, name, announce_url, piece_length, prefix)
        artifact = built.info_hash + ".torrent"
        artifacts[artifact] = built.metainfo
        item = {key: release[key] for key in ("source_id", "title", "kind", "imdb_id", "tmdb_id", "tvdb_id", "season", "episodes") if key in release}
        item.update(source_id=release["source_id"] + ":" + built.info_hash, info_hash=built.info_hash, size=built.size, torrent_file=artifact,
                    metainfo_base64=base64.b64encode(built.metainfo).decode("ascii"), seed_verified=False)
        encoded_size += len(json.dumps(item).encode("utf-8"))
        if encoded_size > MAX_CATALOG_BYTES:
            raise TorrentError("catalog exceeds supported size; prepare smaller batches")
        catalog["releases"].append(item)
        seeding["releases"].append({"source_id": release["source_id"], "info_hash": built.info_hash,
                                    "torrent_file": artifact, "files": list(built.files),
                                    "save_path": save_path, "save_parent_of_root": save_parent,
                                    "seed_verified": False})
    return catalog, seeding, artifacts


def _write_private(path: Path, data: bytes) -> None:
    descriptor = os.open(path, os.O_WRONLY | os.O_CREAT | os.O_EXCL | os.O_NOFOLLOW, 0o600)
    with os.fdopen(descriptor, "wb") as stream:
        stream.write(data)
        stream.flush()
        os.fsync(stream.fileno())


def catalog_batches(catalog: dict) -> list[dict]:
    """Materialize bounded import batches for small callers and synthetic fixtures."""
    return list(iter_catalog_batches(catalog["releases"]))



def iter_catalog_batches(items):
    """Yield import shards with bounded memory, even for million-item inventories."""
    current = []
    current_bytes = 64
    for item in items:
        size = len(json.dumps(item, ensure_ascii=False).encode("utf-8")) + 2
        if size + 64 > MAX_IMPORT_BYTES:
            raise TorrentError("release metainfo is too large for an import batch")
        if current and (len(current) >= MAX_IMPORT_RELEASES or current_bytes + size > MAX_IMPORT_BYTES):
            yield {"version": 1, "releases": current}
            current = []
            current_bytes = 64
        current.append(item)
        current_bytes += size
    if current:
        yield {"version": 1, "releases": current}


def _read_bounded(path: Path, limit: int) -> bytes:
    descriptor = os.open(path, os.O_RDONLY | os.O_NOFOLLOW | os.O_NONBLOCK)
    with os.fdopen(descriptor, "rb") as stream:
        value = os.fstat(stream.fileno())
        if not stat.S_ISREG(value.st_mode) or value.st_size > limit:
            raise TorrentError("prepared file exceeds the supported limit or is not regular")
        data = stream.read(limit + 1)
        if len(data) > limit:
            raise TorrentError("prepared file exceeds the supported limit")
        return data


def _checkpoint(output: Path, resume: bool) -> tuple[sqlite3.Connection, int]:
    """Lock before schema work, retaining completed generations across resumes."""
    if not output.exists():
        output.mkdir(mode=0o700, parents=False)
    elif not resume:
        raise FileExistsError("prepared output already exists; use resume")
    value = output.lstat()
    if not stat.S_ISDIR(value.st_mode) or value.st_uid != os.getuid() or value.st_mode & 0o077:
        raise TorrentError("output must be an owner-only directory")
    lock_fd = os.open(output / ".publisher.lock", os.O_RDWR | os.O_CREAT | os.O_NOFOLLOW, 0o600)
    db = None
    try:
        value = os.fstat(lock_fd)
        if not stat.S_ISREG(value.st_mode) or value.st_uid != os.getuid() or value.st_mode & 0o077:
            raise TorrentError("publisher lock must be an owner-only regular file")
        try:
            fcntl.flock(lock_fd, fcntl.LOCK_EX | fcntl.LOCK_NB)
        except OSError:
            raise TorrentError("another publisher holds the output lock") from None
        filename = output / "publisher-checkpoint.sqlite3"
        if filename.exists():
            value = filename.lstat()
            if not stat.S_ISREG(value.st_mode) or value.st_uid != os.getuid() or value.st_mode & 0o077:
                raise TorrentError("checkpoint must be an owner-only regular file")
        else:
            descriptor = os.open(filename, os.O_WRONLY | os.O_CREAT | os.O_EXCL | os.O_NOFOLLOW, 0o600)
            os.close(descriptor)
        db = sqlite3.connect(filename)
        db.row_factory = sqlite3.Row
        db.execute("PRAGMA journal_mode=WAL")
        with db:
            db.execute("BEGIN IMMEDIATE")
            db.execute("""CREATE TABLE IF NOT EXISTS hash_cache (
                context TEXT PRIMARY KEY, fingerprint TEXT NOT NULL,
                info_hash TEXT NOT NULL, size INTEGER NOT NULL, files TEXT NOT NULL
            )""")
            columns = db.execute("PRAGMA table_info(published_releases)").fetchall()
            primary = tuple(row["name"] for row in sorted(columns, key=lambda row: row["pk"]) if row["pk"])
            if columns and primary == ("source_id",):
                # Preserve every available legacy generation row; never repurpose its run_id.
                db.execute("ALTER TABLE published_releases RENAME TO publisher_legacy_releases")
            elif columns and primary != ("run_id", "source_id"):
                raise TorrentError("unsupported publisher checkpoint schema")
            db.execute("""CREATE TABLE IF NOT EXISTS published_releases (
                source_id TEXT NOT NULL, input_source_id TEXT NOT NULL,
                inventory TEXT NOT NULL, run_id TEXT NOT NULL,
                PRIMARY KEY(run_id, source_id)
            )""")
            if columns and primary == ("source_id",):
                db.execute("""INSERT INTO published_releases (source_id,input_source_id,inventory,run_id)
                    SELECT source_id,input_source_id,inventory,run_id FROM publisher_legacy_releases""")
                db.execute("DROP TABLE publisher_legacy_releases")
            db.execute("CREATE INDEX IF NOT EXISTS published_run ON published_releases(run_id,input_source_id)")
        return db, lock_fd
    except BaseException:
        if db is not None:
            db.close()
        os.close(lock_fd)
        raise


def _cached_build(db, root, release, name, prefix, announce_url, piece_length, output, pacer=None):
    snapshot = source_snapshot(root, release["files"])
    piece_length = adaptive_piece_length(sum(file["stat"][2] for file in snapshot["files"]), piece_length)
    context = json.dumps({"root": str(Path(root).resolve(strict=True)), "files": sorted(release["files"]),
                          "name": name, "prefix": prefix, "announce": announce_url,
                          "piece_length": piece_length}, sort_keys=True)
    row = db.execute("SELECT * FROM hash_cache WHERE context=?", (context,)).fetchone()
    if row and json.loads(row["fingerprint"]) == snapshot:
        data = _read_bounded(output / (row["info_hash"] + ".torrent"), 4 * 1024 * 1024)
        meta = parse_metainfo(data)
        if (set(meta) != {"announce", "info"} or meta["announce"] != announce_url.encode("ascii")
                or meta["info"].get("private") != 1 or info_hash(meta) != row["info_hash"]
                or sum(file["stat"][2] for file in snapshot["files"]) != row["size"]):
            raise TorrentError("checkpoint artifact does not match its verified cache")
        return BuiltTorrent(data, row["info_hash"], row["size"], tuple(json.loads(row["files"])), snapshot), True
    built = build_private_torrent(root, release["files"], name, announce_url, piece_length, prefix, pacer)
    if source_snapshot(root, release["files"]) != built.source_fingerprint:
        raise TorrentError("source changed after its hash was prepared")
    filename = output / (built.info_hash + ".torrent")
    if filename.exists():
        if _read_bounded(filename, 4 * 1024 * 1024) != built.metainfo:
            raise TorrentError("existing artifact differs from prepared metainfo")
    else:
        _install_private(filename, built.metainfo)
    db.execute("INSERT INTO hash_cache VALUES (?,?,?,?,?) ON CONFLICT(context) DO UPDATE SET fingerprint=excluded.fingerprint,info_hash=excluded.info_hash,size=excluded.size,files=excluded.files",
               (context, json.dumps(built.source_fingerprint), built.info_hash, built.size, json.dumps(built.files)))
    db.commit()
    return built, False


def _catalog_item(release, built):
    item = {key: release[key] for key in ("source_id", "title", "kind", "imdb_id", "tmdb_id", "tvdb_id", "season", "episodes") if key in release}
    item.update(source_id=release["source_id"] + ":" + built.info_hash,
                info_hash=built.info_hash, size=built.size, torrent_file=built.info_hash + ".torrent",
                metainfo_base64=base64.b64encode(built.metainfo).decode("ascii"), seed_verified=False)
    return item


def _atomic_private(path, data):
    temporary = path.with_name(path.name + "." + uuid.uuid4().hex + ".tmp")
    try:
        _write_private(temporary, data)
        os.replace(temporary, path)
    finally:
        temporary.unlink(missing_ok=True)


def _install_private(path, data):
    """Expose a torrent only after its complete private temporary write succeeds."""
    temporary = path.with_name(path.name + "." + uuid.uuid4().hex + ".tmp")
    try:
        _write_private(temporary, data)
        # Hard-link installation is atomic and refuses to replace an existing artifact.
        os.link(temporary, path, follow_symlinks=False)
    finally:
        temporary.unlink(missing_ok=True)


def publish_stream(releases, root: Path, announce_url: str, output: Path,
                   piece_length: int = DEFAULT_PIECE_LENGTH, resume: bool = False,
                   full_catalog: bool = False, pacer: HashPacer | None = None) -> dict:
    """Checkpoint hashes and emit bounded shards without retaining torrent blobs.

    Unchanged cache reuse requires safe regular-file descriptor fingerprints.
    Changed files are hashed again. Checkpoints survive interrupted inventories;
    a completed index is atomically replaced only after the input is exhausted.
    Rejections are explicit in a private audit, and mark completeness incomplete.
    """
    validate_announce_url(announce_url)
    output = Path(output)
    db, lock_fd = _checkpoint(output, resume)
    run_id = uuid.uuid4().hex
    report = {"version": 2, "run_id": run_id, "release_count": 0, "hashes_built": 0, "cache_reused": 0,
              "rejected": 0, "excluded": 0, "metadata_counts": {}, "emitted_counts": {}, "rejected_counts": {}, "excluded_counts": {}, "inventory_sections": [], "inventory_scope": {},
              "shards": [], "seeding_map": f"seeding-map-{run_id}.jsonl", "audit": f"audit-{run_id}.jsonl",
              "seed_verified": False, "completeness": "incomplete"}
    full = {"version": 1, "releases": []} if full_catalog else None
    full_size = 64
    try:
        os.ftruncate(lock_fd, 0)
        os.write(lock_fd, (str(os.getpid()) + "\n").encode("ascii"))
        with os.fdopen(os.open(output / report["seeding_map"], os.O_WRONLY | os.O_CREAT | os.O_EXCL | os.O_NOFOLLOW, 0o600), "w", encoding="utf-8") as seeding, os.fdopen(os.open(output / report["audit"], os.O_WRONLY | os.O_CREAT | os.O_EXCL | os.O_NOFOLLOW, 0o600), "w", encoding="utf-8") as audit:
            def prepared():
                nonlocal full_size
                for release in releases:
                    if not isinstance(release, dict):
                        release = {"_rejection": True, "kind": "unknown", "reason": "invalid release record"}
                    if release.get("_inventory_event"):
                        if release.get("scope"):
                            report["inventory_scope"].update(release["scope"])
                        section = release.get("section_id")
                        if section and section not in report["inventory_sections"]:
                            report["inventory_sections"].append(section)
                        for kind, count in release["counts"].items():
                            report["metadata_counts"][kind] = report["metadata_counts"].get(kind, 0) + count
                        audit.write(json.dumps(release, ensure_ascii=False) + "\n")
                        continue
                    if release.get("_exclusion"):
                        report["excluded"] += 1
                        kind = release.get("kind", "unknown")
                        report["excluded_counts"][kind] = report["excluded_counts"].get(kind, 0) + 1
                        audit.write(json.dumps({"source_id": release.get("source_id"), "kind": kind, "status": "excluded", "reason": release.get("reason")}, ensure_ascii=False) + "\n")
                        continue
                    try:
                        if release.get("_rejection"):
                            raise TorrentError(release.get("reason", "unpublishable Plex asset"))
                        _inventory({"version": 1, "releases": [release]})
                        if report["release_count"] + report["rejected"] >= MAX_RELEASES:
                            raise TorrentError("inventory exceeds the supported streaming release limit")
                        if db.execute("SELECT 1 FROM published_releases WHERE input_source_id=? AND run_id=?", (release["source_id"], run_id)).fetchone():
                            raise TorrentError("duplicate source_id in current inventory")
                        name, prefix, save_path, save_parent = _release_layout(root, release)
                        built, reused = _cached_build(db, root, release, name, prefix, announce_url, piece_length, output, pacer)
                        item = _catalog_item(release, built)
                        if len(json.dumps(item, ensure_ascii=False).encode("utf-8")) + 64 > MAX_IMPORT_BYTES:
                            raise TorrentError("release metainfo is too large for an import batch")
                        if full is not None:
                            full_size += len(json.dumps(item, ensure_ascii=False).encode("utf-8")) + 2
                            if full_size > MAX_CATALOG_BYTES:
                                raise TorrentError("optional full catalog exceeds its memory bound; use shards")
                            full["releases"].append(item)
                        db.execute("INSERT INTO published_releases VALUES (?,?,?,?) ON CONFLICT(run_id,source_id) DO UPDATE SET inventory=excluded.inventory",
                                   (item["source_id"], release["source_id"], json.dumps(release), run_id))
                        db.commit()
                        mapping = {"source_id": release["source_id"], "catalog_source_id": item["source_id"],
                                   "info_hash": built.info_hash, "torrent_file": item["torrent_file"], "files": list(built.files),
                                   "save_path": save_path, "save_parent_of_root": save_parent, "seed_verified": False}
                        seeding.write(json.dumps(mapping, ensure_ascii=False) + "\n")
                        report["release_count"] += 1
                        report["cache_reused" if reused else "hashes_built"] += 1
                        kind = release["kind"]
                        report["emitted_counts"][kind] = report["emitted_counts"].get(kind, 0) + 1
                        yield item
                    except (TorrentError, OSError) as exc:
                        report["rejected"] += 1
                        kind = release.get("kind", "unknown")
                        report["rejected_counts"][kind] = report["rejected_counts"].get(kind, 0) + 1
                        audit.write(json.dumps({"source_id": release.get("source_id"), "kind": kind,
                                               "status": "rejected", "reason": str(exc) if isinstance(exc, TorrentError) else "source unavailable"}, ensure_ascii=False) + "\n")
                seeding.flush()
                audit.flush()
            for number, batch in enumerate(iter_catalog_batches(prepared()), 1):
                filename = f"catalog-{run_id}-{number:06d}.json"
                data = (json.dumps(batch, ensure_ascii=False) + "\n").encode("utf-8")
                if len(data) > MAX_IMPORT_BYTES:
                    raise TorrentError("catalog shard exceeds its byte bound")
                _write_private(output / filename, data)
                report["shards"].append({"file": filename, "count": len(batch["releases"])})
        if full is not None:
            _atomic_private(output / "catalog.json", (json.dumps(full, ensure_ascii=False) + "\n").encode("utf-8"))
        report["completeness"] = "complete" if report["rejected"] == 0 else "incomplete"
        _atomic_private(output / "catalog-index.json", (json.dumps(report, indent=2) + "\n").encode("utf-8"))
        return report
    finally:
        os.close(lock_fd)
        db.close()


def _manifest_records(inventory):
    releases = _inventory(inventory)
    counts = {}
    for release in releases:
        counts[release["kind"]] = counts.get(release["kind"], 0) + 1
    yield {"_inventory_event": True, "source": "manifest", "counts": counts, "scope": {"source": "manifest", "coverage": "listed releases"}}
    yield from releases


def publish_offline(inventory: dict, root: Path, announce_url: str, output: Path,
                    piece_length: int = DEFAULT_PIECE_LENGTH, resume: bool = False,
                    full_catalog: bool = False, pacer: HashPacer | None = None) -> dict:
    """Stream a version-one manifest into resumable private preparation artifacts."""
    return publish_stream(_manifest_records(inventory), root, announce_url, output, piece_length, resume, full_catalog, pacer)


def verify_offline(inventory: dict | None, root: Path, output: Path, pacer: HashPacer | None = None) -> int:
    """Rehash indexed shards against sources, independent of cache change detection."""
    index = json.loads(_read_bounded(output / "catalog-index.json", MAX_MANIFEST_BYTES))
    filename = output / "publisher-checkpoint.sqlite3"
    if filename.is_symlink():
        raise TorrentError("invalid checkpoint path")
    db = sqlite3.connect(f"file:{filename}?mode=ro", uri=True)
    db.row_factory = sqlite3.Row
    expected = {item["source_id"]: item for item in _inventory(inventory)} if inventory is not None else None
    count = 0
    seen = set()
    try:
        for shard in index["shards"]:
            name = safe_component(shard["file"])
            catalog = json.loads(_read_bounded(output / name, MAX_IMPORT_BYTES))
            if len(catalog["releases"]) != shard["count"]:
                raise TorrentError("catalog shard count differs from its index")
            for item in catalog["releases"]:
                if item["source_id"] in seen:
                    raise TorrentError("duplicate source_id in prepared catalog")
                seen.add(item["source_id"])
                row = db.execute("SELECT * FROM published_releases WHERE source_id=? AND run_id=?", (item["source_id"], index["run_id"])).fetchone()
                if row is None:
                    raise TorrentError("catalog source is missing from its checkpoint")
                stored = json.loads(row["inventory"])
                release = expected.pop(stored["source_id"], None) if expected is not None else stored
                if release is None:
                    raise TorrentError("prepared catalog and inventory releases differ")
                artifact = safe_component(item["torrent_file"])
                data = _read_bounded(output / artifact, 4 * 1024 * 1024)
                if base64.b64decode(item["metainfo_base64"], validate=True) != data:
                    raise TorrentError("catalog metainfo differs from its artifact")
                _, prefix, _, _ = _release_layout(root, release)
                built = verify_torrent_files(root, release["files"], data, prefix, pacer)
                if (built.info_hash != item["info_hash"] or built.size != item["size"]
                        or item["source_id"] != release["source_id"] + ":" + built.info_hash):
                    raise TorrentError("catalog hash or size differs from its artifact")
                if item != _catalog_item(stored, built):
                    raise TorrentError("catalog metadata differs from its source checkpoint")
                count += 1
        checkpoint_count = db.execute("SELECT count(*) FROM published_releases WHERE run_id=?", (index["run_id"],)).fetchone()[0]
        if count != checkpoint_count or count != index["release_count"] or (expected is not None and expected):
            raise TorrentError("prepared catalog and inventory releases differ")
    finally:
        db.close()
    return count



def main(argv: list[str] | None = None) -> int:
    """Prepare/resume or rehash private artifacts, logging aggregate counts only."""
    parser = argparse.ArgumentParser(description=__doc__)
    source = parser.add_mutually_exclusive_group()
    source.add_argument("--manifest", type=Path)
    source.add_argument("--plex-url")
    parser.add_argument("--plex-token-env", default="PLEX_TOKEN")
    parser.add_argument("--path-map", action="append", default=[], help="Plex absolute prefix=relative prefix below --root")
    parser.add_argument("--sections", help="Optional comma-separated Plex video or music section IDs")
    parser.add_argument("--include-music", action="store_true", help="Explicitly include Plex track versions from music sections")
    parser.add_argument("--no-packs", action="store_true")
    parser.add_argument("--root", type=Path, required=True)
    parser.add_argument("--announce")
    parser.add_argument("--output", type=Path, required=True)
    parser.add_argument("--piece-length", type=int, default=DEFAULT_PIECE_LENGTH)
    parser.add_argument("--hash-mib-per-second", type=float,
                        help="Optional source hashing read limit in MiB/s, shared across releases (default: unlimited)")
    parser.add_argument("--verify", action="store_true")
    parser.add_argument("--resume", action="store_true")
    parser.add_argument("--full-catalog", action="store_true", help="Optional bounded monolithic catalog for small exports")
    args = parser.parse_args(argv)
    try:
        pacer = HashPacer(args.hash_mib_per_second) if args.hash_mib_per_second is not None else None
        if args.verify:
            inventory = json.loads(_read_bounded(args.manifest, MAX_MANIFEST_BYTES)) if args.manifest else None
            count = verify_offline(inventory, args.root, args.output, pacer)
            index = json.loads(_read_bounded(args.output / "catalog-index.json", MAX_MANIFEST_BYTES))
            print(f"Verified {count} private release artifacts; catalog {index['completeness']}; live seeding remains unverified.")
            return 0 if index["completeness"] == "complete" else 3
        if not args.announce or not (args.manifest or args.plex_url):
            raise TorrentError("preparation needs an announce URL and inventory source")
        if args.manifest:
            inventory = json.loads(_read_bounded(args.manifest, MAX_MANIFEST_BYTES))
            releases = _manifest_records(inventory)
        else:
            maps = []
            for mapping in args.path_map:
                if "=" not in mapping:
                    raise TorrentError("path map requires source=destination")
                maps.append(tuple(mapping.split("=", 1)))
            releases = iter_plex_releases(args.plex_url, os.environ.get(args.plex_token_env, ""), args.root,
                                          maps or None, not args.no_packs, set(args.sections.split(",")) if args.sections else None,
                                          include_music=args.include_music)
        report = publish_stream(releases, args.root, args.announce, args.output, args.piece_length, args.resume, args.full_catalog, pacer)
        print(f"Prepared {report['release_count']} private releases; reused {report['cache_reused']} verified hash checkpoints; rejected {report['rejected']}. Live seeding remains unverified.")
        return 0 if report["completeness"] == "complete" else 3
    except (OSError, ValueError, KeyError, TypeError, sqlite3.Error):
        parser.exit(2, "Private media preparation failed: invalid input, unavailable file, changed content, or interrupted source.\n")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())

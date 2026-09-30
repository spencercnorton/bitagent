"""Prepare a private media catalog offline; never publish or seed production data.

Run ``python media_publish.py --manifest inventory.json --root /media/library
--announce https://tracker.example.org/announce --output prepared``. Input uses
version=1 and releases with source_id, title, kind and a relative files array.
Movies and episodes have one or more parts; season/show releases are packs.
The catalog contains portable metainfo for a protected importer. The separate
seeding map is local preparation data, never an HTTP response or public asset.
"""
from __future__ import annotations

import argparse
import base64
import json
import os
import re
from pathlib import Path, PurePosixPath
from urllib.parse import urlsplit
from urllib.request import HTTPRedirectHandler, ProxyHandler, Request, build_opener

from torrent_metainfo import (
    DEFAULT_PIECE_LENGTH, MAX_FILES, TorrentError, build_private_torrent,
    relative_media_path, safe_component, validate_announce_url, verify_torrent_files,
)

MAX_MANIFEST_BYTES = 16 * 1024 * 1024
MAX_CATALOG_BYTES = 128 * 1024 * 1024
MAX_RELEASES = 100_000
MAX_IMPORT_RELEASES = 1000
MAX_IMPORT_BYTES = 4 * 1024 * 1024
KINDS = {"movie", "episode", "season", "show"}


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
    """Map real Plex media parts to movie/episode releases and optional packs.

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
        kind = metadata.get("type")
        if kind not in {"movie", "episode"}:
            continue
        rating_key = str(metadata.get("ratingKey") or "")
        if not rating_key:
            raise TorrentError("Plex media has no stable rating key")
        media_versions = metadata.get("Media")
        if not isinstance(media_versions, list) or not media_versions:
            raise TorrentError("Plex media has no downloadable parts")
        # Torznab tvdbid/tmdbid on TV searches names the series, not the episode.
        identifiers = ids(metadata) if kind == "movie" else show_ids.get(str(metadata.get("grandparentRatingKey")), {})
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
                release.update(season=season, episodes=[episode], title=f"{show_title} S{season:02d}E{episode:02d} {title}".strip())
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


def fetch_plex_inventory(base_url: str, token: str, root: Path,
                         path_maps: list[tuple[str, str]] | None = None,
                         include_packs: bool = True) -> dict:
    """Read movie/episode inventory from Plex; no library or downloader mutations."""
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
        if section.get("type") not in {"movie", "show"}:
            continue
        key = str(section.get("key") or "")
        if not key.isdigit():
            raise TorrentError("invalid Plex section identity")
        for media_type in ((1,) if section["type"] == "movie" else (2, 4)):
            start = 0
            previous_page = None
            for _ in range(400):
                page = get(f"/library/sections/{key}/all?type={media_type}&includeGuids=1", start)
                rows = page.get("Metadata", [])
                if not isinstance(rows, list):
                    raise TorrentError("invalid Plex inventory page")
                page_identity = tuple(str(row.get("ratingKey")) for row in rows)
                if rows and page_identity == previous_page:
                    raise TorrentError("Plex pagination did not advance")
                previous_page = page_identity
                payloads.extend(rows)
                start += len(rows)
                if len(payloads) > MAX_RELEASES:
                    raise TorrentError("Plex library exceeds supported batch size")
                total = page.get("totalSize")
                if not rows or (type(total) is int and start >= total) or (total is None and len(rows) < 250):
                    break
            else:
                raise TorrentError("Plex pagination exceeded supported batch size")
    return inventory_from_plex(payloads, root, path_maps, include_packs)


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
    """Hash media inventory without network calls or crawler corpus writes."""
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


def catalog_batches(catalog: dict) -> list[dict]:
    """Split prepared releases into bounded operator-import requests."""
    batches = []
    current = []
    current_bytes = 64
    for item in catalog["releases"]:
        size = len(json.dumps(item, ensure_ascii=False).encode("utf-8")) + 2
        if size + 64 > MAX_IMPORT_BYTES:
            raise TorrentError("release metainfo is too large for an import batch")
        if current and (len(current) >= MAX_IMPORT_RELEASES or current_bytes + size > MAX_IMPORT_BYTES):
            batches.append({"version": 1, "releases": current})
            current = []
            current_bytes = 64
        current.append(item)
        current_bytes += size
    if current:
        batches.append({"version": 1, "releases": current})
    return batches


def publish_offline(inventory: dict, root: Path, announce_url: str, output: Path,
                    piece_length: int = DEFAULT_PIECE_LENGTH) -> dict:
    """Write artifacts to a new owner-only directory after all hashes pass."""
    catalog, seeding, artifacts = prepare_catalog(inventory, root, announce_url, piece_length)
    batches = catalog_batches(catalog)
    output.mkdir(mode=0o700, parents=False, exist_ok=False)
    for name, data in artifacts.items():
        _write_private(output / name, data)
    _write_private(output / "catalog.json", (json.dumps(catalog, ensure_ascii=False) + "\n").encode("utf-8"))
    for number, batch in enumerate(batches, 1):
        _write_private(output / f"catalog-{number:04d}.json", (json.dumps(batch, ensure_ascii=False) + "\n").encode("utf-8"))
    _write_private(output / "seeding_map.json", (json.dumps(seeding, ensure_ascii=False, indent=2) + "\n").encode("utf-8"))
    return catalog


def _read_bounded(path: Path, limit: int) -> bytes:
    descriptor = os.open(path, os.O_RDONLY | os.O_NOFOLLOW | os.O_NONBLOCK)
    with os.fdopen(descriptor, "rb") as stream:
        if os.fstat(stream.fileno()).st_size > limit:
            raise TorrentError("prepared file exceeds the supported limit")
        data = stream.read(limit + 1)
        if len(data) > limit:
            raise TorrentError("prepared file exceeds the supported limit")
        return data


def verify_offline(inventory: dict, root: Path, output: Path) -> int:
    """Rehash prepared artifacts; successful checks do not prove a live seed."""
    catalog = json.loads(_read_bounded(output / "catalog.json", MAX_CATALOG_BYTES))
    prepared = {item["source_id"][:-41]: item for item in catalog["releases"]}
    releases = _inventory(inventory)
    if set(prepared) != {item["source_id"] for item in releases} or len(prepared) != len(catalog["releases"]):
        raise TorrentError("prepared catalog and inventory releases differ")
    for release in releases:
        item = prepared[release["source_id"]]
        filename = safe_component(item["torrent_file"])
        data = _read_bounded(output / filename, 4 * 1024 * 1024)
        if base64.b64decode(item["metainfo_base64"], validate=True) != data:
            raise TorrentError("catalog metainfo differs from its artifact")
        _, prefix, _, _ = _release_layout(root, release)
        built = verify_torrent_files(root, release["files"], data, prefix)
        if (built.info_hash != item["info_hash"] or built.size != item["size"]
                or item["source_id"] != release["source_id"] + ":" + built.info_hash):
            raise TorrentError("catalog hash or size differs from its artifact")
    return len(releases)


def main(argv: list[str] | None = None) -> int:
    """Prepare or recheck a private catalog, printing aggregate counts only."""
    parser = argparse.ArgumentParser(description=__doc__)
    source = parser.add_mutually_exclusive_group(required=True)
    source.add_argument("--manifest", type=Path)
    source.add_argument("--plex-url")
    parser.add_argument("--plex-token-env", default="PLEX_TOKEN")
    parser.add_argument("--path-map", action="append", default=[], help="Plex absolute prefix=relative prefix below --root")
    parser.add_argument("--no-packs", action="store_true")
    parser.add_argument("--root", type=Path, required=True)
    parser.add_argument("--announce")
    parser.add_argument("--output", type=Path, required=True)
    parser.add_argument("--piece-length", type=int, default=DEFAULT_PIECE_LENGTH)
    parser.add_argument("--verify", action="store_true")
    args = parser.parse_args(argv)
    try:
        if args.manifest:
            inventory = json.loads(_read_bounded(args.manifest, MAX_MANIFEST_BYTES))
        else:
            maps = []
            for mapping in args.path_map:
                if "=" not in mapping:
                    raise TorrentError("path map requires source=destination")
                maps.append(tuple(mapping.split("=", 1)))
            inventory = fetch_plex_inventory(args.plex_url, os.environ.get(args.plex_token_env, ""),
                                             args.root, maps or None, not args.no_packs)
        if args.verify:
            count = verify_offline(inventory, args.root, args.output)
            print(f"Verified {count} private release artifacts; live seeding remains unverified.")
        else:
            if not args.announce:
                raise TorrentError("--announce is required for preparation")
            catalog = publish_offline(inventory, args.root, args.announce, args.output, args.piece_length)
            print(f"Prepared {len(catalog['releases'])} private releases; live seeding remains unverified.")
    except (OSError, ValueError, KeyError, TypeError):
        parser.exit(2, "Private media preparation failed: invalid input, unavailable file, or changed content.\n")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())

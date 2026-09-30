"""Offline v1 private metainfo: strict bencoding and bounded streaming hashes.

This module never announces, serves, or seeds a torrent. A private flag controls
peer discovery in conforming clients; it is not encryption or a sharing barrier.
"""
from __future__ import annotations

import hashlib
import os
import stat
from dataclasses import dataclass, field
from pathlib import Path, PurePosixPath
from urllib.parse import urlsplit

MAX_METAINFO_BYTES = 4 * 1024 * 1024
MAX_FILES = 10_000
MIN_PIECE_LENGTH = 64 * 1024
MAX_PIECE_LENGTH = 16 * 1024 * 1024
DEFAULT_PIECE_LENGTH = 4 * 1024 * 1024


class TorrentError(ValueError):
    """An unsafe input or unstable media file prevented metainfo generation."""


def bencode(value) -> bytes:
    """Encode supported bencoding values canonically, sorting raw dictionary keys."""
    if isinstance(value, bool):
        raise TorrentError("booleans are not bencoding integers")
    if isinstance(value, int):
        return b"i" + str(value).encode("ascii") + b"e"
    if isinstance(value, str):
        value = value.encode("utf-8")
    if isinstance(value, bytes):
        return str(len(value)).encode("ascii") + b":" + value
    if isinstance(value, (list, tuple)):
        return b"l" + b"".join(bencode(item) for item in value) + b"e"
    if isinstance(value, dict):
        items = []
        seen = set()
        for key, item in value.items():
            raw = key.encode("utf-8") if isinstance(key, str) else key
            if not isinstance(raw, bytes) or raw in seen:
                raise TorrentError("dictionary keys must be unique strings")
            seen.add(raw)
            items.append((raw, item))
        return b"d" + b"".join(bencode(k) + bencode(v) for k, v in sorted(items)) + b"e"
    raise TorrentError("unsupported bencoding value")


def bdecode(data: bytes):
    """Decode bounded canonical bencoding; keys and byte-string values stay bytes."""
    if not isinstance(data, bytes) or not data or len(data) > MAX_METAINFO_BYTES:
        raise TorrentError("metainfo size is outside the supported limit")
    index = 0
    nodes = 0

    def read(depth=0):
        nonlocal index, nodes
        nodes += 1
        if depth >= 16 or nodes > 100_000 or index >= len(data):
            raise TorrentError("metainfo nesting or item limit exceeded")
        token = data[index:index + 1]
        if token == b"i":
            end = data.find(b"e", index + 1)
            raw = data[index + 1:end] if end >= 0 else b""
            if not raw or len(raw) > 32:
                raise TorrentError("invalid integer")
            digits = raw[1:] if raw.startswith(b"-") else raw
            if not digits.isdigit() or raw == b"-0" or (len(digits) > 1 and digits[:1] == b"0"):
                raise TorrentError("noncanonical integer")
            index = end + 1
            return int(raw)
        if token in (b"l", b"d"):
            index += 1
            result = [] if token == b"l" else {}
            previous = None
            while index < len(data) and data[index:index + 1] != b"e":
                item = read(depth + 1)
                if token == b"l":
                    result.append(item)
                else:
                    if not isinstance(item, bytes) or (previous is not None and item <= previous):
                        raise TorrentError("dictionary keys must be sorted and unique")
                    previous = item
                    result[item] = read(depth + 1)
            if index >= len(data):
                raise TorrentError("unterminated collection")
            index += 1
            return result
        if token.isdigit():
            end = data.find(b":", index)
            raw = data[index:end] if end >= 0 else b""
            if not raw.isdigit() or len(raw) > 10 or (len(raw) > 1 and raw[:1] == b"0"):
                raise TorrentError("invalid string length")
            length = int(raw)
            start = end + 1
            if start + length > len(data):
                raise TorrentError("truncated string")
            index = start + length
            return data[start:index]
        raise TorrentError("invalid bencoding token")

    result = read()
    if index != len(data):
        raise TorrentError("trailing metainfo bytes")
    return result


def parse_metainfo(data: bytes) -> dict:
    """Decode canonical metainfo with ASCII dictionary keys and binary values."""
    def keys(value):
        if isinstance(value, dict):
            try:
                return {key.decode("ascii"): keys(item) for key, item in value.items()}
            except UnicodeDecodeError as exc:
                raise TorrentError("metainfo dictionary keys must be ASCII") from exc
        if isinstance(value, list):
            return [keys(item) for item in value]
        return value

    result = keys(bdecode(data))
    if not isinstance(result, dict) or not isinstance(result.get("info"), dict):
        raise TorrentError("metainfo must contain an info dictionary")
    return result


def encode_metainfo(value: dict) -> bytes:
    """Encode metainfo, preserving canonical info during announce rewrites."""
    encoded = bencode(value)
    parse_metainfo(encoded)
    return encoded


def info_hash(metainfo: dict) -> str:
    """Return the v1 hash of the canonical info dictionary."""
    return hashlib.sha1(bencode(metainfo["info"]), usedforsecurity=False).hexdigest()


def safe_component(value: str) -> str:
    """Validate a portable torrent path component without silently renaming it."""
    if (not isinstance(value, str) or not value or value in {".", ".."}
            or len(value.encode("utf-8")) > 255 or value[-1:] in {" ", "."}
            or any(ord(char) < 32 or ord(char) == 127 or char in '/\\:<>"|?*' for char in value)):
        raise TorrentError("invalid torrent path component")
    return value


def relative_media_path(value: str) -> tuple[str, ...]:
    """Reject absolute, traversal, backslash and noncanonical relative paths."""
    if not isinstance(value, str) or len(value) > 4096:
        raise TorrentError("invalid relative media path")
    path = PurePosixPath(value)
    parts = value.split("/")
    if path.is_absolute() or not parts or len(parts) > 32:
        raise TorrentError("media paths must stay below the configured root")
    return tuple(safe_component(part) for part in parts)


def validate_announce_url(value: str) -> str:
    """Accept one token-free HTTPS tracker URL; tokens belong in issued copies."""
    if not isinstance(value, str) or len(value) > 2048 or any(ord(c) < 33 for c in value):
        raise TorrentError("invalid announce URL")
    parsed = urlsplit(value)
    try:
        if parsed.port is not None and parsed.port < 1:
            raise ValueError("invalid port")
        value.encode("ascii")
    except (ValueError, UnicodeEncodeError) as exc:
        raise TorrentError("invalid announce URL or port") from exc
    if (parsed.scheme != "https" or not parsed.hostname or parsed.username or parsed.password
            or parsed.query or parsed.fragment or parsed.path != "/announce"):
        raise TorrentError("announce must be a credential-free HTTPS /announce URL")
    return value


def _open_media(root_fd: int, parts: tuple[str, ...]) -> int:
    directory = os.dup(root_fd)
    try:
        for component in parts[:-1]:
            child = os.open(component, os.O_RDONLY | os.O_DIRECTORY | os.O_NOFOLLOW, dir_fd=directory)
            os.close(directory)
            directory = child
        return os.open(parts[-1], os.O_RDONLY | os.O_NOFOLLOW | os.O_NONBLOCK, dir_fd=directory)
    except OSError as exc:
        raise TorrentError("media file is unavailable or contains a symlink") from exc
    finally:
        os.close(directory)


def _fingerprint(value: os.stat_result) -> tuple:
    return (value.st_dev, value.st_ino, value.st_size, value.st_mtime_ns, value.st_ctime_ns)


@dataclass(frozen=True)
class BuiltTorrent:
    """Verified offline metainfo and source mapping; no seed readiness is implied."""
    metainfo: bytes
    info_hash: str
    size: int
    files: tuple[dict, ...]
    source_fingerprint: dict = field(default_factory=dict)


def build_private_torrent(root: Path, files: list[str], name: str, announce_url: str,
                          piece_length: int = DEFAULT_PIECE_LENGTH, path_prefix: tuple[str, ...] = ()) -> BuiltTorrent:
    """Stream file pieces across boundaries, refusing symlinks and mutation.

    The configured root is resolved once. Descendants are opened relative to
    its descriptor with O_NOFOLLOW. All files are rechecked after release hashing.
    """
    safe_component(name)
    validate_announce_url(announce_url)
    if (type(piece_length) is not int or piece_length < MIN_PIECE_LENGTH
            or piece_length > MAX_PIECE_LENGTH or piece_length & (piece_length - 1)):
        raise TorrentError("piece length must be a supported power of two")
    if not isinstance(files, list) or not 1 <= len(files) <= MAX_FILES:
        raise TorrentError("release file count is outside the supported limit")
    paths = sorted(relative_media_path(path) for path in files)
    if len(set(paths)) != len(paths):
        raise TorrentError("duplicate media path")
    if not isinstance(path_prefix, tuple) or any(safe_component(p) != p for p in path_prefix):
        raise TorrentError("invalid pack directory prefix")
    if any(parts[:len(path_prefix)] != path_prefix or len(parts) <= len(path_prefix) for parts in paths):
        raise TorrentError("pack files must stay below their shared directory")
    root_fd = os.open(Path(root).resolve(strict=True), os.O_RDONLY | os.O_DIRECTORY | os.O_NOFOLLOW)
    root_identity = os.fstat(root_fd)
    mappings = []
    fingerprints = []
    piece_hashes = bytearray()
    current = hashlib.sha1(usedforsecurity=False)
    remaining = piece_length
    try:
        for parts in paths:
            file_fd = _open_media(root_fd, parts)
            with os.fdopen(file_fd, "rb") as stream:
                before = os.fstat(stream.fileno())
                if not stat.S_ISREG(before.st_mode):
                    raise TorrentError("only regular media files can be published")
                read_bytes = 0
                while chunk := stream.read(min(1024 * 1024, remaining)):
                    current.update(chunk)
                    remaining -= len(chunk)
                    read_bytes += len(chunk)
                    if remaining == 0:
                        piece_hashes.extend(current.digest())
                        if len(piece_hashes) > MAX_METAINFO_BYTES // 2:
                            raise TorrentError("piece table exceeds the supported limit")
                        current = hashlib.sha1(usedforsecurity=False)
                        remaining = piece_length
                if read_bytes != before.st_size or _fingerprint(before) != _fingerprint(os.fstat(stream.fileno())):
                    raise TorrentError("media file changed during hashing")
                fingerprints.append((parts, _fingerprint(before)))
                mappings.append({"source_path": "/".join(parts), "path": list(parts[len(path_prefix):]), "length": before.st_size})
        for parts, expected in fingerprints:
            file_fd = _open_media(root_fd, parts)
            try:
                if _fingerprint(os.fstat(file_fd)) != expected:
                    raise TorrentError("media file changed during release hashing")
            finally:
                os.close(file_fd)
    finally:
        os.close(root_fd)
    size = sum(item["length"] for item in mappings)
    if size == 0:
        raise TorrentError("empty releases cannot be published")
    if remaining != piece_length:
        piece_hashes.extend(current.digest())
    info = {"name": name, "piece length": piece_length, "pieces": bytes(piece_hashes), "private": 1}
    if len(mappings) == 1:
        info["length"] = size
        mappings[0]["torrent_path"] = [name]
    else:
        info["files"] = [{"length": item["length"], "path": item["path"]} for item in mappings]
        for item in mappings:
            item["torrent_path"] = [name, *item["path"]]
    metainfo = {"announce": announce_url, "info": info}
    encoded = encode_metainfo(metainfo)
    return BuiltTorrent(encoded, info_hash(metainfo), size, tuple(mappings),
                        {"root": [root_identity.st_dev, root_identity.st_ino],
                         "files": [{"path": "/".join(parts), "stat": list(values)} for parts, values in fingerprints]})




def adaptive_piece_length(size: int, minimum: int = DEFAULT_PIECE_LENGTH) -> int:
    """Choose a bounded power-of-two piece length for large genuine packs."""
    if (type(size) is not int or size <= 0 or type(minimum) is not int
            or minimum < MIN_PIECE_LENGTH or minimum > MAX_PIECE_LENGTH
            or minimum & (minimum - 1)):
        raise TorrentError("invalid adaptive piece layout")
    length = minimum
    while 20 * ((size + length - 1) // length) > MAX_METAINFO_BYTES // 2 and length < MAX_PIECE_LENGTH:
        length *= 2
    if 20 * ((size + length - 1) // length) > MAX_METAINFO_BYTES // 2:
        raise TorrentError("release exceeds supported private metainfo size even at maximum piece length")
    return length


def source_snapshot(root: Path, files: list[str]) -> dict:
    """Inspect safe file descriptors for cache reuse without reading media bytes.

    Device/inode/size/mtime/ctime must all match the original successful hash.
    This is a filesystem change detector, not a live seeder hash verification.
    """
    if not isinstance(files, list) or not 1 <= len(files) <= MAX_FILES:
        raise TorrentError("release file count is outside the supported limit")
    paths = sorted(relative_media_path(path) for path in files)
    if len(set(paths)) != len(paths):
        raise TorrentError("duplicate media path")
    root_fd = os.open(Path(root).resolve(strict=True), os.O_RDONLY | os.O_DIRECTORY | os.O_NOFOLLOW)
    result = {"root": [], "files": []}
    try:
        identity = os.fstat(root_fd)
        result["root"] = [identity.st_dev, identity.st_ino]
        for parts in paths:
            file_fd = _open_media(root_fd, parts)
            try:
                info = os.fstat(file_fd)
                if not stat.S_ISREG(info.st_mode):
                    raise TorrentError("only regular media files can be published")
                result["files"].append({"path": "/".join(parts), "stat": list(_fingerprint(info))})
            finally:
                os.close(file_fd)
    finally:
        os.close(root_fd)
    return result


def verify_torrent_files(root: Path, files: list[str], data: bytes, path_prefix: tuple[str, ...] = ()) -> BuiltTorrent:
    """Rehash source files and compare every info byte to an offline artifact."""
    meta = parse_metainfo(data)
    if set(meta) != {"announce", "info"} or meta["info"].get("private") != 1:
        raise TorrentError("verification accepts only controlled private artifacts")
    try:
        rebuilt = build_private_torrent(root, files, meta["info"]["name"].decode("utf-8"),
                                        meta["announce"].decode("ascii"), meta["info"]["piece length"], path_prefix)
    except (KeyError, AttributeError, UnicodeDecodeError) as exc:
        raise TorrentError("invalid private metainfo fields") from exc
    if bencode(parse_metainfo(rebuilt.metainfo)["info"]) != bencode(meta["info"]):
        raise TorrentError("source files no longer match the torrent")
    return rebuilt

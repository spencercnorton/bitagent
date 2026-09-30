"""Optional, short-lived address ownership for private tracker announces.

The seed management boundary supplies approved address-to-SSO mappings over
its existing authenticated TLS session. This cache is independent of torrent
readiness and never treats client transfer counters as measured byte delivery.
"""
from __future__ import annotations

import asyncio
from dataclasses import dataclass
from http.cookies import SimpleCookie
import ipaddress
import json
import logging
import math
import re
import time
from types import MappingProxyType
from urllib.parse import urlsplit

import httpx

from config import settings

_PATH = "/_bitagent/peer-bindings"
_MAX_BYTES = 256 * 1024
_MAX_BINDINGS = 1000
_MAX_AGE = 30
_REFRESH_SECONDS = 3
_REQUEST_SECONDS = 5
_TASK = None
_SNAPSHOT = None
_HIGH_WATER = -1.0
_LAST_CONTENT = None
logger = logging.getLogger("bitagent-ui")


@dataclass(frozen=True)
class _Snapshot:
    issued_at: float
    deadline: float
    owners: MappingProxyType


def validate_settings():
    if not settings.private_peer_bindings_required:
        return
    if not settings.private_indexer_enabled:
        raise RuntimeError("PRIVATE_PEER_BINDINGS_REQUIRED requires the private indexer")
    parts = urlsplit(settings.private_seeder_url)
    if (parts.scheme != "https" or not parts.hostname or parts.username is not None or parts.password is not None
            or "?" in settings.private_seeder_url or "#" in settings.private_seeder_url or parts.path not in {"", "/"}):
        raise RuntimeError("Peer bindings require PRIVATE_SEEDER_URL to be a fixed HTTPS origin")
    try:
        if parts.port is not None and not 1 <= parts.port <= 65535:
            raise ValueError("Invalid port")
    except ValueError:
        raise RuntimeError("Peer bindings require a valid HTTPS port") from None
    if not settings.private_seeder_username or not settings.private_seeder_password:
        raise RuntimeError("Peer bindings require seed session credentials")


def invalidate():
    """Withdraw trust but retain the replay watermark until process restart."""
    global _SNAPSHOT
    _SNAPSHOT = None


def reset():
    global _HIGH_WATER, _LAST_CONTENT
    invalidate()
    _HIGH_WATER, _LAST_CONTENT = -1.0, None


def matches(address: str, user_id: str) -> bool:
    if not settings.private_peer_bindings_required:
        return True
    snapshot = _SNAPSHOT
    return bool(snapshot and time.monotonic() < snapshot.deadline
                and snapshot.owners.get(address) == user_id)


def require_match(address: str, user_id: str):
    if not matches(address, user_id):
        raise ValueError("Peer identity unavailable")


def _unique_object(pairs):
    result = {}
    for key, value in pairs:
        if key in result:
            raise ValueError("Duplicate snapshot field")
        result[key] = value
    return result


def _invalid_constant(_value):
    raise ValueError("Invalid snapshot number")


def _install(content: bytes, wall_now: float, monotonic_now: float):
    """Publish a validated immutable mapping without extending replayed leases."""
    global _SNAPSHOT, _HIGH_WATER, _LAST_CONTENT
    if len(content) > _MAX_BYTES:
        raise ValueError("Snapshot too large")
    body = json.loads(content, object_pairs_hook=_unique_object, parse_constant=_invalid_constant)
    if not isinstance(body, dict) or set(body) != {"version", "issued_at", "expires_at", "bindings"}:
        raise ValueError("Invalid snapshot envelope")
    if type(body["version"]) is not int or body["version"] != 1:
        raise ValueError("Unsupported snapshot version")
    issued, expires = body["issued_at"], body["expires_at"]
    if any(type(n) not in {int, float} or not math.isfinite(n) or n < 0 for n in (issued, expires)):
        raise ValueError("Invalid snapshot time")
    if not 0 < expires - issued <= _MAX_AGE or issued > wall_now or expires <= wall_now:
        raise ValueError("Snapshot not fresh")
    rows = body["bindings"]
    if not isinstance(rows, list) or len(rows) > _MAX_BINDINGS:
        raise ValueError("Invalid snapshot bindings")
    owners = {}
    for row in rows:
        if not isinstance(row, dict) or set(row) != {"user_id", "address"}:
            raise ValueError("Invalid binding")
        user, address = row["user_id"], row["address"]
        if (not isinstance(user, str) or not 1 <= len(user) <= 200 or user.strip() != user
                or user in {"anonymous", "api-client"} or any(ord(c) < 32 or ord(c) == 127 for c in user)):
            raise ValueError("Invalid binding identity")
        if not isinstance(address, str):
            raise ValueError("Invalid binding address")
        ip = ipaddress.IPv4Address(address)
        if (str(ip) != address or ip.is_unspecified or ip.is_multicast or ip.is_loopback
                or address == "255.255.255.255" or address in owners):
            raise ValueError("Invalid binding address")
        owners[address] = user  # One identity may own several approved devices.
    canonical = (float(expires), tuple(sorted(owners.items())))
    deadline = monotonic_now + (expires - wall_now)
    if issued < _HIGH_WATER:
        raise ValueError("Retired snapshot")
    if issued == _HIGH_WATER:
        if (canonical != _LAST_CONTENT or _SNAPSHOT is None
                or monotonic_now >= _SNAPSHOT.deadline):
            raise ValueError("Replayed or conflicting snapshot")
        deadline = min(deadline, _SNAPSHOT.deadline)
    _SNAPSHOT = _Snapshot(float(issued), deadline, MappingProxyType(owners))
    _HIGH_WATER, _LAST_CONTENT = float(issued), canonical


async def _read_bounded(response, limit):
    # Require an uncompressed wire body so decompression cannot allocate an
    # unbounded chunk before the decoded-size check sees it.
    if response.headers.get("Content-Encoding", "identity").lower() != "identity":
        raise ValueError("Encoded response not supported")
    if response.headers.get("Content-Length"):
        if not 0 <= int(response.headers["Content-Length"]) <= limit:
            raise ValueError("Response too large")
    body = bytearray()
    async for chunk in response.aiter_bytes():
        if len(body) + len(chunk) > limit:
            raise ValueError("Response too large")
        body.extend(chunk)
    return bytes(body)


async def _fetch():
    # Reuse the qBittorrent login predicate used for readiness. A fresh client
    # cannot authenticate using a session retained from an earlier observation.
    from private_indexer import _seeder_login_ok
    validate_settings()
    base = settings.private_seeder_url.rstrip("/")
    async with httpx.AsyncClient(timeout=_REQUEST_SECONDS, follow_redirects=False, trust_env=False) as client:
        client.headers["Referer"] = base + "/"
        client.headers["Accept-Encoding"] = "identity"
        async with client.stream("POST", base + "/api/v2/auth/login", data={
                "username": settings.private_seeder_username, "password": settings.private_seeder_password}) as response:
            login = httpx.Response(response.status_code, headers=response.headers,
                                   content=await _read_bounded(response, 1024), request=response.request)
        if not _seeder_login_ok(login, client, base):
            raise ValueError("Seeder authentication failed")
        # The snapshot lives outside /api: a session scoped only to the qBt
        # API path must not count as authentication for the reserved endpoint.
        sent = SimpleCookie()
        sent.load(client.build_request("GET", base + _PATH).headers.get("Cookie", ""))
        if not any((c.name == "SID" or re.fullmatch(r"QBT_SID_[1-9][0-9]{0,4}", c.name))
                   and c.name in sent and sent[c.name].value == c.value for c in login.cookies.jar):
            raise ValueError("Session does not cover peer bindings")
        async with client.stream("GET", base + _PATH) as response:
            if response.status_code != 200 or response.headers.get("Content-Type", "").split(";", 1)[0].strip().lower() != "application/json":
                raise ValueError("Peer bindings unavailable")
            return await _read_bounded(response, _MAX_BYTES)


async def refresh():
    if not settings.private_indexer_enabled or not settings.private_peer_bindings_required:
        invalidate()
        return
    started_wall, started_mono = time.time(), time.monotonic()
    try:
        # httpx timeouts limit idle I/O; this also bounds the whole observation.
        async with asyncio.timeout(_REQUEST_SECONDS):
            content = await _fetch()
        now = time.monotonic()
        wall = max(time.time(), started_wall + (now - started_mono))
        _install(content, wall, now)
    except BaseException:
        invalidate()
        raise


def start():
    global _TASK
    if not settings.private_indexer_enabled or not settings.private_peer_bindings_required or _TASK is not None:
        return
    async def loop():
        try:
            while True:
                try:
                    await refresh()
                except Exception as exc:
                    # Never include a URL, response body, cookie or identity.
                    logger.warning("private peer binding refresh failed (%s)", type(exc).__name__)
                await asyncio.sleep(_REFRESH_SECONDS)
        finally:
            invalidate()
    _TASK = asyncio.create_task(loop())


async def stop():
    global _TASK
    invalidate()
    if _TASK is not None:
        _TASK.cancel()
        try:
            await _TASK
        except asyncio.CancelledError:
            pass
        finally:
            _TASK = None

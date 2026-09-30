"""Bounded, shared cache of TMDB's public daily TV-network ID/name export.

The index supports complete name lookup without fetching thousands of logos.
Discovery resolves metadata only for the selected page of network identities.
"""
from __future__ import annotations

import asyncio
import gzip
import io
import json
import time
import zlib
from datetime import datetime, timedelta, timezone

import httpx

import tmdb

_TTL = 6 * 3600.0
_DOWNLOAD_BUDGET = 6.0
_MAX_COMPRESSED = 512 * 1024
_MAX_DECOMPRESSED = 4 * 1024 * 1024
_MAX_NETWORKS = 20_000
_CACHE: tuple[float, list[dict]] | None = None
_TASK: asyncio.Task | None = None


class NetworkIndexUnavailable(Exception):
    """A safe export failure with no upstream URL or credentials."""


def reset_cache() -> None:
    global _CACHE, _TASK
    if _TASK is not None:
        _TASK.cancel()
    _TASK = None
    _CACHE = None


def _parse_export(content: bytes) -> list[dict]:
    try:
        if len(content) > _MAX_COMPRESSED:
            raise NetworkIndexUnavailable()
        with gzip.GzipFile(fileobj=io.BytesIO(content)) as archive:
            raw = archive.read(_MAX_DECOMPRESSED + 1)
        if len(raw) > _MAX_DECOMPRESSED:
            raise NetworkIndexUnavailable()
        networks = {}
        for line in raw.decode("utf-8").splitlines():
            row = json.loads(line)
            if not isinstance(row, dict):
                raise NetworkIndexUnavailable()
            identifier, name = row.get("id"), row.get("name")
            if (not isinstance(identifier, int) or isinstance(identifier, bool)
                    or not 0 < identifier <= 2_147_483_647
                    or not isinstance(name, str) or not name.strip()):
                continue
            networks[identifier] = {"id": identifier, "name": name.strip()[:200]}
            if len(networks) > _MAX_NETWORKS:
                raise NetworkIndexUnavailable()
        if not networks:
            raise NetworkIndexUnavailable()
        return sorted(networks.values(), key=lambda item: (item["name"].casefold(), item["id"]))
    except (OSError, EOFError, UnicodeError, ValueError, TypeError, zlib.error):
        raise NetworkIndexUnavailable() from None


async def _download_export() -> list[dict]:
    # Daily exports can be absent before publication, so try yesterday too.
    today = datetime.now(timezone.utc).date()
    for date in (today, today - timedelta(days=1)):
        url = f"https://files.tmdb.org/p/exports/tv_network_ids_{date:%m_%d_%Y}.json.gz"
        try:
            async with tmdb._get_client().stream("GET", url, timeout=httpx.Timeout(5.0, connect=2.0)) as response:
                if response.status_code == 404:
                    continue
                response.raise_for_status()
                content = bytearray()
                async for part in response.aiter_bytes():
                    content.extend(part)
                    if len(content) > _MAX_COMPRESSED:
                        raise NetworkIndexUnavailable()
            return _parse_export(bytes(content))
        except (httpx.HTTPError, ValueError, TypeError):
            raise NetworkIndexUnavailable() from None
    raise NetworkIndexUnavailable()


async def get_index() -> list[dict]:
    global _CACHE, _TASK
    if _CACHE is not None and time.monotonic() - _CACHE[0] < _TTL:
        return _CACHE[1]
    if _TASK is None:
        async def fetch() -> list[dict]:
            global _CACHE
            try:
                # Bound the entire shared stream, including slowly arriving
                # bytes and the previous-day publication fallback.
                rows = await asyncio.wait_for(_download_export(), timeout=_DOWNLOAD_BUDGET)
            except TimeoutError:
                raise NetworkIndexUnavailable() from None
            _CACHE = (time.monotonic(), rows)
            return rows

        task = asyncio.create_task(fetch())
        _TASK = task

        def finished(done: asyncio.Task) -> None:
            global _TASK
            if _TASK is done:
                _TASK = None
            if not done.cancelled():
                done.exception()

        task.add_done_callback(finished)
    return await asyncio.shield(_TASK)

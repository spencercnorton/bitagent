"""Bounded operator catalog snapshots and restart-safe continuation cursors."""

from __future__ import annotations

import base64
import hashlib
import hmac
import json
import math
import re
import secrets
import time

import aiosqlite
from fastapi import HTTPException

from config import settings
import private_readiness as readiness
from private_media import KINDS

PAGE_LIMIT = 100
PAGE_BYTES = 2 * 1024 * 1024
ACK_BYTES = 512 * 1024
CURSOR_BYTES = 1024
CURSOR_TTL = 3600
_PURPOSE = "operator-catalog-v1"
_HEX = re.compile(r"[0-9a-f]{32}")
_SOURCE = re.compile(r"[A-Za-z0-9_.:-]{1,200}")
_CURSOR_FIELDS = {
    "version", "purpose", "instance_id", "revision_token", "view", "limit",
    "after", "issued_at", "expires_at", "alias_total", "release_total",
}


def _pairs(pairs):
    result = {}
    for key, value in pairs:
        if key in result:
            raise ValueError("Duplicate JSON field")
        result[key] = value
    return result


def _constant(_value):
    raise ValueError("Nonfinite JSON value")


def _json(raw):
    return json.loads(raw, object_pairs_hook=_pairs, parse_constant=_constant)


def bounded_body(value, maximum):
    try:
        raw = json.dumps(value, ensure_ascii=True, separators=(",", ":"), allow_nan=False).encode()
    except (TypeError, ValueError, UnicodeError):
        raise HTTPException(409, "Invalid stored catalog data") from None
    if len(raw) > maximum:
        raise HTTPException(409, "Catalog response exceeds limit")


async def initialize():
    """Initialize identity once; all new schema changes commit atomically."""
    async with aiosqlite.connect(settings.db_path, timeout=30) as db:
        db.row_factory = aiosqlite.Row
        await db.execute("BEGIN IMMEDIATE")
        try:
            await db.execute("""CREATE TABLE IF NOT EXISTS private_catalog_state (
                singleton INTEGER PRIMARY KEY CHECK(singleton=1),
                instance_id TEXT NOT NULL CHECK(length(instance_id)=32),
                revision_token TEXT NOT NULL CHECK(length(revision_token)=32)
            )""")
            await db.execute("INSERT OR IGNORE INTO private_catalog_state VALUES (1,?,?)",
                             (secrets.token_hex(16), secrets.token_hex(16)))
            await state(db)
            await db.commit()
        except BaseException:
            await db.rollback()
            raise


async def state(db):
    rows = await db.execute_fetchall("SELECT instance_id,revision_token FROM private_catalog_state WHERE singleton=1")
    if (len(rows) != 1 or any(not isinstance(rows[0][key], str) or not _HEX.fullmatch(rows[0][key])
                              for key in ("instance_id", "revision_token"))):
        raise HTTPException(409, "Invalid catalog identity")
    return dict(rows[0])


async def changed(db):
    """Replace the revision in the caller's metadata write transaction."""
    await state(db)
    await db.execute("UPDATE private_catalog_state SET revision_token=? WHERE singleton=1", (secrets.token_hex(16),))


def acknowledgment_requested(params):
    values = params.getlist("ack")
    if not values:
        return False
    if len(values) != 1 or values[0] not in {"0", "1"}:
        raise HTTPException(422, "Invalid acknowledgment parameter")
    return values[0] == "1"


async def acknowledgment(db, releases):
    results = []
    for item in releases:
        rows = await db.execute_fetchall("""SELECT a.source_id,r.id,r.info_hash,r.size
            FROM private_release_aliases a LEFT JOIN private_releases r ON r.id=a.release_id
            WHERE a.source_id=?""", (item["source_id"],))
        if (len(rows) != 1 or not isinstance(rows[0]["id"], str) or not _HEX.fullmatch(rows[0]["id"])
                or rows[0]["info_hash"] != item["info_hash"] or rows[0]["size"] != item["size"]):
            raise HTTPException(409, "Catalog identity conflicts with an existing release")
        results.append(dict(rows[0]))
    return {"catalog_snapshot": await state(db), "results": results}


def _b64(raw):
    return base64.urlsafe_b64encode(raw).rstrip(b"=").decode("ascii")


def _sign(raw):
    return hmac.new(settings.private_indexer_secret.encode(), _PURPOSE.encode() + b"\0" + raw, hashlib.sha256).hexdigest()


def _encode_cursor(value):
    raw = json.dumps(value, ensure_ascii=True, separators=(",", ":"), allow_nan=False).encode()
    result = _b64(raw) + "." + _sign(raw)
    if len(result) > CURSOR_BYTES:
        raise HTTPException(409, "Catalog cursor exceeds limit")
    return result


def _decode_cursor(token):
    try:
        if not isinstance(token, str) or len(token) > CURSOR_BYTES:
            raise ValueError()
        encoded, signature = token.split(".")
        if not re.fullmatch(r"[0-9a-f]{64}", signature):
            raise ValueError()
        raw = base64.b64decode(encoded + "=" * (-len(encoded) % 4), altchars=b"-_", validate=True)
        if _b64(raw) != encoded or not hmac.compare_digest(_sign(raw), signature):
            raise ValueError()
        result = _json(raw)
        if not isinstance(result, dict) or set(result) != _CURSOR_FIELDS:
            raise ValueError()
        if (type(result["version"]) is not int or result["version"] != 1 or result["purpose"] != _PURPOSE
                or result["view"] not in {"aliases", "releases"}
                or any(not isinstance(result[k], str) or not _HEX.fullmatch(result[k]) for k in ("instance_id", "revision_token"))
                or type(result["limit"]) is not int or not 1 <= result["limit"] <= PAGE_LIMIT
                or any(type(result[k]) is not int or not 0 <= result[k] <= 2**63-1
                       for k in ("issued_at", "expires_at", "alias_total", "release_total"))
                or result["expires_at"] != result["issued_at"] + CURSOR_TTL
                or result["issued_at"] > time.time() + 5
                or not isinstance(result["after"], str)
                or not (_SOURCE if result["view"] == "aliases" else _HEX).fullmatch(result["after"])):
            raise ValueError()
    except (ValueError, TypeError, UnicodeError, KeyError):
        raise HTTPException(422, "Invalid catalog cursor") from None
    if time.time() >= result["expires_at"]:
        raise HTTPException(410, "Catalog cursor expired; restart enumeration")
    return result


def _options(params):
    pairs = list(params.multi_items())
    if any(key not in {"cursor", "view", "limit"} for key, _ in pairs) or len({key for key, _ in pairs}) != len(pairs):
        raise HTTPException(422, "Invalid catalog page parameter")
    cursor = _decode_cursor(params["cursor"]) if "cursor" in params else None
    view = params.get("view", cursor["view"] if cursor else "aliases")
    raw_limit = params.get("limit", str(cursor["limit"] if cursor else PAGE_LIMIT))
    if view not in {"aliases", "releases"} or not re.fullmatch(r"[0-9]{1,3}", raw_limit) or not 1 <= int(raw_limit) <= PAGE_LIMIT:
        raise HTTPException(422, "Invalid catalog page parameter")
    limit = int(raw_limit)
    if cursor and (view != cursor["view"] or limit != cursor["limit"]):
        raise HTTPException(422, "Catalog cursor parameters changed")
    return cursor, view, limit


def _metadata(raw):
    try:
        if not isinstance(raw, str) or len(raw) > 8000:
            raise ValueError()
        value = _json(raw)
        if not isinstance(value, dict) or set(value) - {"imdb_id", "tmdb_id", "tvdb_id", "season", "episodes"}:
            raise ValueError()
        for key, field in value.items():
            if key == "imdb_id":
                if not isinstance(field, str) or not re.fullmatch(r"tt[0-9]{1,12}", field):
                    raise ValueError()
            elif key == "episodes":
                if (not isinstance(field, list) or len(field) > 10000
                        or any(type(ep) is not int or not 0 <= ep <= 100000 for ep in field)
                        or len(set(field)) != len(field)):
                    raise ValueError()
            elif type(field) is not int or not 0 <= field <= 10**12:
                raise ValueError()
        return value
    except (ValueError, TypeError, UnicodeError):
        raise HTTPException(409, "Invalid stored catalog metadata") from None


def _item(row, view):
    item = dict(row)
    if (not isinstance(item["id"], str) or not _HEX.fullmatch(item["id"])
            or not isinstance(item["source_id"], str) or not _SOURCE.fullmatch(item["source_id"])
            or not isinstance(item["info_hash"], str) or not re.fullmatch(r"[0-9a-f]{40}", item["info_hash"])
            or not isinstance(item["title"], str) or not item["title"].strip() or len(item["title"]) > 500
            or item["kind"] not in KINDS
            or type(item["size"]) is not int or not 0 < item["size"] <= 2**63-1
            or any(type(item[k]) is not int or item[k] not in {0, 1} for k in ("ready", "withdrawn"))
            or (item["verified_at"] is not None and (type(item["verified_at"]) not in {int, float} or not math.isfinite(item["verified_at"])))):
        raise HTTPException(409, "Invalid stored catalog data")
    if view == "aliases":
        item["metadata"] = _metadata(item["metadata"])
    return item


async def page(params):
    cursor, view, limit = _options(params)
    gate = readiness.capture()
    # Never borrow the shared writer for a multi-query read snapshot.
    async with aiosqlite.connect(settings.db_path, timeout=5) as db:
        db.row_factory = aiosqlite.Row
        await db.execute("PRAGMA query_only=ON")
        await db.execute("BEGIN")
        try:
            snapshot = await state(db)  # First SELECT anchors this WAL snapshot.
            if cursor and any(cursor[key] != snapshot[key] for key in snapshot):
                raise HTTPException(409, "Catalog changed; restart enumeration")
            if cursor:
                counts = {key: cursor[key] for key in ("alias_total", "release_total")}
            else:
                counts = dict((await db.execute_fetchall("""SELECT
                    (SELECT count(*) FROM private_release_aliases) AS alias_total,
                    (SELECT count(*) FROM private_releases) AS release_total"""))[0])
            after = cursor["after"] if cursor else ""
            if view == "aliases":
                rows = await db.execute_fetchall("""SELECT r.id,a.source_id,a.title,a.kind,r.info_hash,r.size,
                    r.ready,r.verified_at,r.withdrawn,r.ready_epoch,r.ready_until_mono,r.proof_id,r.publication_nonce,substr(a.metadata,1,8001) AS metadata
                    FROM private_release_aliases a LEFT JOIN private_releases r ON r.id=a.release_id
                    WHERE a.source_id COLLATE BINARY>? ORDER BY a.source_id COLLATE BINARY LIMIT ?""", (after, limit+1))
            else:
                rows = await db.execute_fetchall("""SELECT id,source_id,title,kind,info_hash,size,ready,verified_at,withdrawn,ready_epoch,ready_until_mono,proof_id,publication_nonce
                    FROM private_releases WHERE id COLLATE BINARY>? ORDER BY id COLLATE BINARY LIMIT ?""", (after, limit+1))
            items = [_item(row, view) for row in rows[:limit]]
            await db.commit()
            matches = await readiness.matching_proofs(db, items, gate)
            for item in items:
                if (item["id"], item["proof_id"]) not in matches:
                    item.update(ready=0, verified_at=None)
        except BaseException:
            await db.rollback()
            raise
    items = readiness.display(items, gate)
    issued = cursor["issued_at"] if cursor else int(time.time())
    result = {"version": 1, "view": view, "snapshot": {**snapshot, **counts}, "items": items, "next_cursor": None}
    if len(rows) > limit:
        result["next_cursor"] = _encode_cursor({
            "version": 1, "purpose": _PURPOSE, **snapshot, **counts, "view": view, "limit": limit,
            "after": items[-1]["source_id" if view == "aliases" else "id"],
            "issued_at": issued, "expires_at": issued + CURSOR_TTL,
        })
    bounded_body(result, PAGE_BYTES)
    return result

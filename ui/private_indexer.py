"""Opt-in member catalog and HTTP tracker, isolated from the DHT corpus.

Membership is an authorization grant for an existing SSO identity, not a new
login system. Tracker counters are client-reported observations, never proof
of delivery. Download issuance is measured separately.
"""
from __future__ import annotations

import asyncio
import base64
import hashlib
import hmac
import ipaddress
import json
import logging
import re
import secrets
import struct
import time
from contextlib import asynccontextmanager
from http.cookies import CookieError, SimpleCookie
from urllib.parse import parse_qsl, quote, urlsplit

import aiosqlite
from fastapi import APIRouter, Depends, HTTPException, Request, Response
from fastapi.responses import JSONResponse
from fastapi.templating import Jinja2Templates
from pathlib import Path
from pydantic import BaseModel, Field

from auth import require_auth, proxy_provenance_valid
from config import settings
from database import get_db, get_user_api_key, lookup_user_api_key, _hash_user_api_key
from deps import require_operator, _configured_hosts
import privatebindings
import privatecatalog
import private_readiness as readiness

router = APIRouter()
_WRITE_LOCK = asyncio.Lock()
_PEER_TTL = 1800
_MAX_TORRENT_BYTES = 4 * 1024 * 1024
_MAX_CATALOG_RELEASES = 1000
_MAX_COUNTER = 2**63 - 1
_TEMPLATES = Jinja2Templates(directory=Path(__file__).parent / "templates")
_READINESS_TASK = None
logger = logging.getLogger("bitagent-ui")


async def init_schema(db):
    await db.executescript("""
        CREATE TABLE IF NOT EXISTS private_members (
            user_id TEXT PRIMARY KEY, active INTEGER NOT NULL,
            actor TEXT NOT NULL, updated_at REAL NOT NULL
        );
        CREATE TABLE IF NOT EXISTS private_releases (
            id TEXT PRIMARY KEY, source_id TEXT NOT NULL UNIQUE,
            title TEXT NOT NULL, kind TEXT NOT NULL, info_hash TEXT NOT NULL UNIQUE,
            size INTEGER NOT NULL, metadata TEXT NOT NULL, metainfo BLOB NOT NULL,
            ready INTEGER NOT NULL DEFAULT 0, verified_at REAL, created_at REAL NOT NULL,
            withdrawn INTEGER NOT NULL DEFAULT 0
        );
        CREATE TABLE IF NOT EXISTS private_tracker_keys (
            key_hash TEXT PRIMARY KEY, api_key_id INTEGER NOT NULL UNIQUE
        );
        CREATE TABLE IF NOT EXISTS private_release_aliases (
            source_id TEXT PRIMARY KEY, release_id TEXT NOT NULL,
            title TEXT NOT NULL, kind TEXT NOT NULL, metadata TEXT NOT NULL
        );
        CREATE TABLE IF NOT EXISTS private_peers (
            release_id TEXT NOT NULL, user_id TEXT NOT NULL, api_key_id INTEGER NOT NULL,
            peer_id BLOB NOT NULL, ip TEXT NOT NULL, port INTEGER NOT NULL,
            uploaded INTEGER NOT NULL, downloaded INTEGER NOT NULL, remaining INTEGER NOT NULL,
            completed INTEGER NOT NULL DEFAULT 0, updated_at REAL NOT NULL,
            PRIMARY KEY(release_id, user_id, peer_id)
        );
        CREATE TABLE IF NOT EXISTS private_transfer_totals (
            user_id TEXT NOT NULL, release_id TEXT NOT NULL,
            uploaded INTEGER NOT NULL DEFAULT 0, downloaded INTEGER NOT NULL DEFAULT 0,
            completed INTEGER NOT NULL DEFAULT 0, issued INTEGER NOT NULL DEFAULT 0,
            PRIMARY KEY(user_id, release_id)
        );
    """)
    await readiness.initialize(db)
    await privatecatalog.initialize()


@asynccontextmanager
async def private_write(*, deadline=None, failure_epoch=None):
    # Readiness has bounded lock/busy/SQL budgets. Ordinary writers retain their
    # existing transaction semantics; no shared connection owns this BEGIN.
    acquired = False
    try:
        if deadline is None:
            await _WRITE_LOCK.acquire()
        else:
            async with asyncio.timeout_at(deadline):
                await _WRITE_LOCK.acquire()
        acquired = True
        connection = aiosqlite.connect(settings.db_path, timeout=.25 if deadline else 30)
        if failure_epoch is not None:
            connection = readiness.fenced_context(connection, lambda: failure_epoch)
        async with connection as db:
            db.row_factory = aiosqlite.Row
            if deadline is not None:
                await db.set_progress_handler(lambda: int(time.monotonic() >= deadline), 1000)
            try:
                async with asyncio.timeout_at(deadline):
                    await db.execute("BEGIN IMMEDIATE")
                    yield db
                    await db.commit()
            except BaseException:
                readiness.withdraw_epoch(failure_epoch)
                # Interrupt queued work before rollback; busy waits are <=250ms
                # for readiness. A failed cleanup never opens the local gate.
                await db.interrupt()
                await db.set_progress_handler(None, 0)
                await db.rollback()
                raise
    finally:
        if acquired:
            _WRITE_LOCK.release()


def validate_settings():
    privatebindings.validate_settings()
    if not settings.private_indexer_enabled:
        return
    if not settings.require_auth:
        raise RuntimeError("Private indexer requires authentication")
    if len(settings.private_indexer_secret.encode()) < 32:
        raise RuntimeError("PRIVATE_INDEXER_SECRET must be at least 32 bytes")
    if not 60 <= settings.private_seed_verification_ttl <= 86400:
        raise RuntimeError("PRIVATE_SEED_VERIFICATION_TTL must be between 60 and 86400 seconds")
    if settings.private_tracker_rate_limit_per_min < 1:
        raise RuntimeError("PRIVATE_TRACKER_RATE_LIMIT_PER_MIN must be positive")
    try:
        networks = [ipaddress.ip_network(c.strip()) for c in settings.private_peer_cidrs.split(",") if c.strip()]
        if any(n.prefixlen == 0 for n in networks):
            raise ValueError("Open peer network")
    except ValueError as exc:
        raise RuntimeError("PRIVATE_PEER_CIDRS must contain explicit member networks") from exc
    parts = urlsplit(settings.private_indexer_url)
    if (parts.scheme != "https" or not parts.hostname or parts.username or parts.password
            or parts.query or parts.fragment or parts.path not in {"", "/"}):
        raise RuntimeError("PRIVATE_INDEXER_URL must be a canonical HTTPS origin")
    if parts.hostname not in _configured_hosts(settings.public_library_hosts, "PUBLIC_LIBRARY_HOSTS"):
        raise RuntimeError("PRIVATE_INDEXER_URL must name a PUBLIC_LIBRARY_HOSTS entry")
    if not settings.private_seeder_url:
        raise RuntimeError("PRIVATE_SEEDER_URL is required when the private indexer is enabled")
    parts = urlsplit(settings.private_seeder_url)
    if (parts.scheme not in {"http", "https"} or not parts.hostname
            or parts.username or parts.password or parts.query or parts.fragment):
        raise RuntimeError("PRIVATE_SEEDER_URL must be a fixed HTTP(S) URL")


def enabled():
    if not settings.private_indexer_enabled:
        raise HTTPException(404, "Not found")


def external_base(request: Request) -> str:
    # Fixed origin avoids Host/forwarding poisoning and makes indexer responses
    # correct even when a TLS proxy talks HTTP to this app.
    enabled()
    return settings.private_indexer_url.rstrip("/")


async def member_active(user_id: str) -> bool:
    rows = await (await get_db()).execute_fetchall(
        "SELECT active FROM private_members WHERE user_id = ?", (user_id,)
    )
    return bool(rows and rows[0]["active"])


async def require_member(identity: dict = Depends(require_auth)) -> dict:
    enabled()
    if not await member_active(str(identity["id"])):
        raise HTTPException(403, "Approved membership required")
    return identity


async def torznab_requires_member(row: dict) -> bool:
    return not settings.private_indexer_enabled or await member_active(str(row["user_id"]))


class MemberGrant(BaseModel):
    active: bool


@router.put("/api/private/members/{user_id}")
async def grant_member(user_id: str, body: MemberGrant, identity=Depends(require_operator)):
    enabled()
    if not user_id or len(user_id) > 200 or user_id in {"anonymous", "api-client"}:
        raise HTTPException(422, "An existing SSO identity is required")
    async with private_write() as db:
        await db.execute(
            """INSERT INTO private_members VALUES (?, ?, ?, ?)
            ON CONFLICT(user_id) DO UPDATE SET active=excluded.active,
                actor=excluded.actor, updated_at=excluded.updated_at""",
            (user_id, int(body.active), str(identity["id"]), time.time()),
        )
        if not body.active:
            await db.execute("DELETE FROM private_peers WHERE user_id=?", (user_id,))
            await db.execute("UPDATE user_api_keys SET revoked_at=? WHERE user_id=? AND revoked_at IS NULL", (time.time(), user_id))
    return {"userId": user_id, "active": body.active}


@router.get("/api/private/members")
async def list_members(identity=Depends(require_operator)):
    enabled()
    return [dict(row) for row in await (await get_db()).execute_fetchall(
        "SELECT user_id, active, actor, updated_at FROM private_members ORDER BY user_id"
    )]


@router.get("/api/account/private")
async def private_account(identity=Depends(require_auth)):
    enabled()
    return {"approved": await member_active(str(identity["id"])),
            "privateTorznabUrl": settings.private_indexer_url.rstrip("/") + "/torznab/private/api",
            "metrics": "client-reported tracker observations"}


class CatalogImport(BaseModel):
    version: int = 1
    releases: list[dict] = Field(max_length=_MAX_CATALOG_RELEASES)


def _validate_release(item: dict) -> dict:
    from torrent_metainfo import bdecode, bencode
    try:
        payload = item["metainfo_base64"]
        if len(payload) > _MAX_TORRENT_BYTES * 2:
            raise ValueError("Torrent too large")
        metainfo_bytes = base64.b64decode(payload, validate=True)
        if len(metainfo_bytes) > _MAX_TORRENT_BYTES:
            raise ValueError("Torrent too large")
        torrent = bdecode(metainfo_bytes)
        if not isinstance(torrent, dict) or not isinstance(torrent.get(b"info"), dict):
            raise ValueError("Invalid metainfo dictionary")
        info = torrent[b"info"]
        # The builder performs file/path validation; repeat it at import, because
        # operator API input is independent of the offline builder.
        if info.get(b"private") != 1 or set(torrent) - {b"info", b"announce", b"created by", b"creation date"}:
            raise ValueError("Only private torrents with one tracker are supported")
        if set(info) - {b"name", b"files", b"length", b"piece length", b"pieces", b"private"}:
            raise ValueError("Unsupported info fields")
        def safe_part(value):
            if not isinstance(value, bytes):
                raise ValueError("Invalid path")
            part = value.decode("utf-8")
            if not part or part in {".", ".."} or any(c in part for c in "/\\\x00"):
                raise ValueError("Invalid path")
        safe_part(info[b"name"])
        if b"files" in info:
            if b"length" in info or not info[b"files"] or len(info[b"files"]) > 10000:
                raise ValueError("Invalid files")
            paths = set()
            size = 0
            for entry in info[b"files"]:
                if not isinstance(entry, dict) or set(entry) != {b"length", b"path"} or not isinstance(entry[b"path"], list) or not entry[b"path"]:
                    raise ValueError("Invalid file")
                for part in entry[b"path"]:
                    safe_part(part)
                path = tuple(entry[b"path"])
                if path in paths:
                    raise ValueError("Duplicate path")
                paths.add(path)
                if type(entry[b"length"]) is not int or entry[b"length"] < 0:
                    raise ValueError("Invalid length")
                size += entry[b"length"]
        else:
            size = info[b"length"]
        piece_length = info[b"piece length"]
        if (type(size) is not int or not 0 < size <= _MAX_COUNTER
                or type(piece_length) is not int or not 16384 <= piece_length <= 16*1024*1024
                or not isinstance(info[b"pieces"], bytes)
                or len(info[b"pieces"]) != 20 * ((size + piece_length - 1)//piece_length)):
            raise ValueError("Invalid piece layout")
        info_hash = hashlib.sha1(bencode(info)).hexdigest()
        if item["info_hash"] != info_hash or item["size"] != size:
            raise ValueError("Manifest does not match torrent")
        title, source_id, kind = item["title"], item["source_id"], item["kind"]
        if (not isinstance(title, str) or not title.strip() or len(title) > 500
                or any((ord(c) < 32 and c not in "\t\n\r") or 0xD800 <= ord(c) <= 0xDFFF or ord(c) in {0xFFFE,0xFFFF} for c in title)
                or not isinstance(source_id, str) or not re.fullmatch(r"[A-Za-z0-9_.:-]{1,200}", source_id)
                or kind not in {"movie", "episode", "season", "show"}):
            raise ValueError("Invalid release metadata")
        metadata = {}
        for key in ("imdb_id", "tmdb_id", "tvdb_id", "season"):
            value = item.get(key)
            if value is None:
                continue
            if key == "imdb_id":
                if not isinstance(value, str) or not re.fullmatch(r"tt\d{1,12}", value):
                    raise ValueError("Invalid IMDb ID")
            else:
                if isinstance(value, str) and re.fullmatch(r"\d{1,12}", value):
                    value = int(value)
                if type(value) is not int or not 0 <= value <= 10**12:
                    raise ValueError("Invalid metadata identifier")
            metadata[key] = value
        if item.get("episodes") is not None:
            episodes = item["episodes"]
            if (not isinstance(episodes, list) or len(episodes) > 10000
                    or any(type(ep) is not int or not 0 <= ep <= 100000 for ep in episodes)
                    or len(set(episodes)) != len(episodes)):
                raise ValueError("Invalid episode list")
            metadata["episodes"] = sorted(episodes)
        if len(json.dumps(metadata)) > 8000:
            raise ValueError("Metadata too large")
        # Discard upstream announce rather than trusting an offline URL. It is
        # personalized when downloaded, outside info, preserving the hash.
        return {"source_id": source_id, "title": title.strip(), "kind": kind,
                "info_hash": info_hash, "size": size, "metadata": json.dumps(metadata),
                "metainfo": bencode({b"info": info})}
    except (KeyError, ValueError, TypeError, UnicodeError, OverflowError) as exc:
        raise HTTPException(422, "Invalid private torrent manifest") from exc


@router.post("/api/private/catalog/import")
async def import_catalog(body: CatalogImport, request: Request, identity=Depends(require_operator)):
    enabled()
    acknowledge = privatecatalog.acknowledgment_requested(request.query_params)
    if body.version != 1:
        raise HTTPException(422, "Unsupported manifest version")
    releases = [_validate_release(item) for item in body.releases]
    if len({r["source_id"] for r in releases}) != len(releases):
        raise HTTPException(422, "Duplicate release")
    response = {"imported": len(releases), "readiness": "Seeder verification required"}
    async with private_write() as db:
        # Aliases share one immutable swarm; an episode/season/show pointing to
        # the same bytes must not manufacture new torrent hashes.
        try:
            changed = False
            for item in releases:
                existing = await db.execute_fetchall("""SELECT a.title,a.kind,a.metadata,r.id,r.info_hash,r.size
                    FROM private_release_aliases a LEFT JOIN private_releases r ON r.id=a.release_id
                    WHERE a.source_id=?""", (item["source_id"],))
                if existing and (existing[0]["id"] is None or existing[0]["info_hash"] != item["info_hash"]
                                 or existing[0]["size"] != item["size"]):
                    raise ValueError("Release IDs are immutable; use a versioned source ID")
                swarm = await db.execute_fetchall("SELECT id,size FROM private_releases WHERE info_hash=?", (item["info_hash"],))
                if swarm and swarm[0]["size"] != item["size"]:
                    raise ValueError("Canonical torrent size conflicts")
                release_id = swarm[0]["id"] if swarm else secrets.token_hex(16)
                if not swarm:
                    changed = True
                    await db.execute("""INSERT INTO private_releases
                        (id,source_id,title,kind,info_hash,size,metadata,metainfo,created_at)
                        VALUES (?,?,?,?,?,?,?,?,?)""",
                        (release_id, item["source_id"], item["title"], item["kind"],
                     item["info_hash"], item["size"], item["metadata"], item["metainfo"], time.time()))
                await readiness.publish_order(db, release_id)
                changed = changed or not existing or any(existing[0][key] != item[key] for key in ("title", "kind", "metadata"))
                await db.execute("""INSERT INTO private_release_aliases VALUES (?,?,?,?,?)
                    ON CONFLICT(source_id) DO UPDATE SET title=excluded.title,kind=excluded.kind,metadata=excluded.metadata""",
                    (item["source_id"], release_id, item["title"], item["kind"], item["metadata"]))
        except (ValueError, aiosqlite.IntegrityError):
            raise HTTPException(409, "Catalog conflicts with an existing release") from None
        if changed:
            await privatecatalog.changed(db)
        if acknowledge:
            response.update(await privatecatalog.acknowledgment(db, releases))
            privatecatalog.bounded_body(response, privatecatalog.ACK_BYTES)
    if acknowledge:
        return JSONResponse(response, headers={"Cache-Control": "no-store"})
    return response


def _ready_cutoff():
    return time.time() - settings.private_seed_verification_ttl


async def reset_readiness():
    """Replace the proof epoch before every enabled application startup."""
    privatebindings.reset()
    if settings.private_indexer_enabled:
        await readiness.reset(private_write)


def _seeder_login_ok(login, client, base):
    """Require a recognized login response and a usable qBittorrent session.

    qBittorrent 5.2 uses an empty 204 response and a QBT_SID_<WebUI port>
    cookie; older releases return 200/Ok. with SID. Let the cookie jar enforce
    origin, path, expiry and Secure rules for the subsequent information GET.
    """
    if (login.status_code, login.content) not in {(200, b"Ok."), (204, b"")}:
        return False
    def session_name(name):
        match = re.fullmatch(r"QBT_SID_([1-9][0-9]{0,4})", name)
        return name == "SID" or bool(match and int(match[1]) <= 65535)
    # Reject duplicate or conflicting session issuance before the jar can
    # collapse same-name Set-Cookie fields into an apparently valid session.
    issued = []
    sent = SimpleCookie()
    try:
        for header in login.headers.get_list("Set-Cookie"):
            parsed = SimpleCookie()
            parsed.load(header)
            issued.extend(name for name in parsed if session_name(name))
        sent.load(client.build_request("GET", base + "/api/v2/torrents/info").headers.get("Cookie", ""))
    except CookieError:
        return False
    if len(issued) != 1:
        return False
    for cookie in login.cookies.jar:
        if cookie.name != issued[0]:
            continue
        # Both legacy hexadecimal IDs and current 24-byte Base64 IDs fit.
        if (re.fullmatch(r"[A-Za-z0-9+/]{32}", cookie.value or "")
                and cookie.name in sent and sent[cookie.name].value == cookie.value):
            return True
    return False


async def refresh_readiness():
    """Refresh a fixed canonical cohort through bounded short transactions."""
    await get_db()
    return await readiness.refresh(private_write, _seeder_login_ok)


def start_readiness_worker():
    global _READINESS_TASK
    privatebindings.start()
    if not settings.private_indexer_enabled:
        return
    async def refresh_loop():
        while True:
            started = time.monotonic()
            try:
                await refresh_readiness()
            except Exception as exc:
                # Never log exception text: upstream request URLs may contain
                # secrets. Stale timestamps still withdraw catalog availability.
                logger.warning("private seeder refresh failed (%s)", type(exc).__name__)
            # Anchor to the sweep start; a slow sweep cannot add another full
            # period to every observation's age. Overruns retain a small backoff.
            await asyncio.sleep(max(1, started + readiness.period() - time.monotonic()))
    _READINESS_TASK = asyncio.create_task(refresh_loop())


async def stop_readiness_worker():
    global _READINESS_TASK
    readiness.withdraw_local()
    await privatebindings.stop()
    if _READINESS_TASK:
        _READINESS_TASK.cancel()
        try:
            await _READINESS_TASK
        except asyncio.CancelledError:
            pass
        _READINESS_TASK = None


@router.get("/api/private/catalog")
async def operator_catalog(identity=Depends(require_operator)):
    enabled()
    gate = readiness.capture()
    rows = await (await get_db()).execute_fetchall(
        "SELECT id,source_id,title,kind,info_hash,size,ready,verified_at,withdrawn," + readiness._COLUMNS +
        " FROM private_releases ORDER BY created_at DESC LIMIT 1000")
    matches = await readiness.matching_proofs(await get_db(), rows, gate)
    rows = [dict(row) for row in rows]
    for row in rows:
        if (row["id"], row["proof_id"]) not in matches:
            row.update(ready=0, verified_at=None)
    return readiness.display(rows, gate)



@router.get("/api/private/catalog/page")
async def operator_catalog_page(request: Request, identity=Depends(require_operator)):
    enabled()
    await get_db()
    return JSONResponse(await privatecatalog.page(request.query_params), headers={"Cache-Control": "no-store"})


@router.post("/api/private/catalog/{release_id}/verify")
async def verify_release(release_id: str, identity=Depends(require_operator)):
    enabled()
    ready = await readiness.verify(await get_db(), release_id, private_write, _seeder_login_ok)
    if ready is None:
        raise HTTPException(404, "Not found")
    if not ready:
        raise HTTPException(409, "Seeder has not verified a complete active copy")
    return {"id": release_id, "ready": True}



class CatalogVisibility(BaseModel):
    published: bool


@router.patch("/api/private/catalog/{release_id}")
async def catalog_visibility(release_id: str, body: CatalogVisibility, identity=Depends(require_operator)):
    enabled()
    async with private_write() as db:
        prior = await db.execute_fetchall("SELECT withdrawn FROM private_releases WHERE id=?", (release_id,))
        cur = await db.execute("UPDATE private_releases SET withdrawn=?,ready=0,verified_at=NULL,ready_until_mono=NULL,proof_id='',publication_nonce=? WHERE id=?", (int(not body.published), secrets.token_hex(16), release_id))
        if not cur.rowcount:
            raise HTTPException(404, "Not found")
        if prior[0]["withdrawn"] != int(not body.published):
            await privatecatalog.changed(db)
    return {"id": release_id, "published": body.published, "ready": False}


def _number(params, name, default, maximum):
    raw = params.get(name)
    if raw is None or raw == "":
        return default
    if not re.fullmatch(r"\d{1,10}", str(raw)) or int(raw) > maximum:
        raise HTTPException(422, "Invalid search parameter")
    return int(raw)


async def search_releases(params) -> dict:
    enabled()
    offset = _number(params, "offset", 0, 1000000)
    limit = _number(params, "limit", 100, 100)
    if not limit:
        raise HTTPException(422, "Invalid search limit")
    gate = readiness.capture()
    ready_clause, ready_args = readiness.predicate("r", gate)
    where, args = [ready_clause], list(ready_args)
    query = str(params.get("q") or "").strip()
    if len(query) > 300:
        raise HTTPException(422, "Query too long")
    if query:
        where.append("a.title LIKE ? ESCAPE '\\'")
        args.append("%" + query.replace("\\", "\\\\").replace("%", "\\%").replace("_", "\\_") + "%")
    mode = params.get("t", "search")
    cats = str(params.get("cat") or "")
    if cats:
        if not re.fullmatch(r"\d+(,\d+)*", cats) or len(cats) > 100:
            raise HTTPException(422, "Invalid category")
        kinds = set()
        for cat in map(int, cats.split(",")):
            if 2000 <= cat < 3000:
                kinds.add("movie")
            if 5000 <= cat < 6000:
                kinds.update({"episode", "season", "show"})
        if not kinds:
            where.append("0")
        else:
            where.append("a.kind IN (" + ",".join("?" for _ in kinds) + ")")
            args.extend(sorted(kinds))
    if mode == "movie":
        where.append("a.kind='movie'")
    if mode == "tvsearch":
        where.append("a.kind!='movie'")
    for field, param in (("imdb_id", "imdbid"), ("tmdb_id", "tmdbid"), ("tvdb_id", "tvdbid"), ("season", "season")):
        value = params.get(param)
        if value is not None and value != "":
            value = str(value)
            if field == "imdb_id":
                value = "tt" + value.removeprefix("tt")
                if not re.fullmatch(r"tt\d{1,12}", value):
                    raise HTTPException(422, "Invalid IMDb ID")
            elif not re.fullmatch(r"\d{1,12}", value):
                raise HTTPException(422, "Invalid identifier")
            where.append("CAST(json_extract(a.metadata, ?) AS TEXT)=?")
            args.extend(("$." + field, value))
    if params.get("ep") is not None and params.get("ep") != "":
        ep = _number(params, "ep", 0, 100000)
        where.append("EXISTS (SELECT 1 FROM json_each(json_extract(a.metadata,'$.episodes')) WHERE CAST(value AS INTEGER)=?)")
        args.append(ep)
    clause = " AND ".join(where)
    db = await get_db()
    join = " FROM private_releases r JOIN private_release_aliases a ON a.release_id=r.id WHERE "
    total = (await db.execute_fetchall("SELECT count(*) n" + join + clause, args))[0]["n"]
    rows = await db.execute_fetchall("SELECT r.id,r.info_hash,r.size,r.created_at,a.source_id,a.title,a.kind,a.metadata,r.ready,r.verified_at,r.withdrawn,r." + readiness._COLUMNS.replace(",", ",r.") + join + clause + " ORDER BY r.created_at DESC,r.id,a.source_id LIMIT ? OFFSET ?", (*args, limit, offset))
    items = []
    for row in rows:
        item = {k: row[k] for k in ("id", "source_id", "title", "kind", "info_hash", "size", "created_at")}
        item.update(json.loads(row["metadata"]))
        peers = await db.execute_fetchall("""SELECT p.remaining,p.ip,p.user_id FROM private_peers p
            JOIN user_api_keys k ON k.id=p.api_key_id AND k.revoked_at IS NULL
            JOIN private_members m ON m.user_id=p.user_id AND m.active=1
            WHERE p.release_id=? AND p.updated_at>?""", (row["id"], time.time()-_PEER_TTL))
        peers = [p for p in peers if privatebindings.matches(p["ip"], p["user_id"])]
        item["seeders"] = sum(p["remaining"] == 0 for p in peers)
        item["leechers"] = sum(p["remaining"] > 0 for p in peers)
        items.append(item)
    # Last awaited peer lookup may cross a source/visibility withdrawal. Keep
    # only the original exact proofs; recovery cannot authorize an old result.
    proofs = await readiness.matching_proofs(db, rows, gate)
    valid = [item for row, item in zip(rows, items) if (row["id"], row["proof_id"]) in proofs]
    if not readiness.live(gate):
        valid, total = [], 0
    return {"items": valid, "total": total, "offset": offset, "limit": limit}


@router.get("/api/library/private")
async def private_library(request: Request, identity=Depends(require_member)):
    _member_rate(str(identity["id"]))
    return await search_releases(request.query_params)


@router.get("/library/private")
async def private_library_page(request: Request, identity=Depends(require_member)):
    return _TEMPLATES.TemplateResponse(request=request, name="private-library.html", context={"admin": False})


@router.get("/private-admin")
async def private_admin_page(request: Request, identity=Depends(require_operator)):
    enabled()
    return _TEMPLATES.TemplateResponse(request=request, name="private-library.html", context={"admin": True})


async def _tracker_key(key_row: dict) -> str:
    db = await get_db()
    rows = await db.execute_fetchall("SELECT key_hash FROM user_api_keys WHERE id=? AND revoked_at IS NULL", (key_row["id"],))
    if not rows or not await member_active(str(key_row["user_id"])):
        raise HTTPException(403, "Approved membership required")
    token = _derive_tracker_key(key_row["id"], rows[0]["key_hash"])
    async with private_write() as db:
        await db.execute("INSERT INTO private_tracker_keys VALUES (?,?) ON CONFLICT(api_key_id) DO UPDATE SET key_hash=excluded.key_hash", (_hash_user_api_key(token), key_row["id"]))
    return token


def _derive_tracker_key(key_id: int, source_key_hash: str) -> str:
    return "bt_" + hmac.new(settings.private_indexer_secret.encode(),
        f"tracker-v1:{key_id}:{source_key_hash}".encode(), hashlib.sha256).hexdigest()


async def _download_release(release_id: str, row: dict, *, seeding=False) -> dict:
    enabled()
    if not await member_active(str(row["user_id"])):
        raise HTTPException(403, "Approved membership required")
    gate = readiness.capture()
    clause, args = ("1", ()) if seeding else readiness.predicate(gate=gate)
    rows = await (await get_db()).execute_fetchall("SELECT * FROM private_releases WHERE id=? AND withdrawn=0 AND " + clause,
                                                  (release_id, *args))
    if not rows:
        raise HTTPException(404, "Not found")
    release = dict(rows[0])
    release["_gate"] = gate
    return release


async def _require_release_proof(release):
    if not await readiness.recheck(await get_db(), release, release["_gate"]):
        raise HTTPException(404, "Not found")



async def _record_issue(row: dict, release_id: str, release):
    async with private_write() as db:
        if not await readiness.recheck(db, release, release["_gate"]):
            raise HTTPException(404, "Not found")
        await db.execute("""INSERT INTO private_transfer_totals(user_id,release_id,issued)
            VALUES (?,?,1) ON CONFLICT(user_id,release_id) DO UPDATE SET issued=issued+1""", (str(row["user_id"]), release_id))
        if not await readiness.recheck(db, release, release["_gate"]):
            raise HTTPException(404, "Not found")


async def torrent_response(release_id: str, request: Request, key_row: dict, *, seeding=False) -> Response:
    from torrent_metainfo import bdecode, bencode
    release = await _download_release(release_id, key_row, seeding=seeding)
    token = await _tracker_key(key_row)
    torrent = bdecode(release["metainfo"])
    torrent[b"announce"] = (external_base(request) + "/private/announce/" + token).encode()
    if request.method != "HEAD" and not seeding:
        await _record_issue(key_row, release_id, release)
    if not seeding:
        await _require_release_proof(release)
    return Response(b"" if request.method == "HEAD" else bencode(torrent), media_type="application/x-bittorrent",
        headers={"Cache-Control": "no-store", "Content-Disposition": f'attachment; filename="{release_id}.torrent"'})


@router.get("/api/private/catalog/{release_id}/seed-torrent")
async def seed_torrent(release_id: str, request: Request, seed_member_id: str, identity=Depends(require_operator)):
    enabled()
    # The operator provisions the original seed before availability exists.
    # It uses an approved member's existing key; it never creates an operator
    # bypass passkey or grants a new identity implicitly.
    row = await get_user_api_key(seed_member_id)
    if not row:
        raise HTTPException(409, "Seed member needs a personal indexer key")
    return await torrent_response(release_id, request, row, seeding=True)


async def _request_key(request: Request):
    from torznab import _extract_torznab_api_key
    key = _extract_torznab_api_key(request)
    row = await lookup_user_api_key(_hash_user_api_key(key)) if key else None
    if not row:
        raise HTTPException(401, "Invalid API key")
    return row


@router.api_route("/private/torrents/{release_id}.torrent", methods=["GET", "HEAD"], name="private_torrent", include_in_schema=False)
async def private_torrent(release_id: str, request: Request):
    enabled()
    row = await _request_key(request)
    _member_rate(str(row["user_id"]))
    return await torrent_response(release_id, request, row)


@router.get("/api/library/private/{release_id}/magnet")
async def private_magnet(release_id: str, request: Request, identity=Depends(require_member)):
    _member_rate(str(identity["id"]))
    row = await get_user_api_key(str(identity["id"]))
    if not row:
        raise HTTPException(409, "Create a personal indexer key first")
    release = await _download_release(release_id, row)
    token = await _tracker_key(row)
    announce = external_base(request) + "/private/announce/" + token
    await _record_issue(row, release_id, release)
    await _require_release_proof(release)
    return {"magnetUri": "magnet:?xt=urn:btih:" + release["info_hash"] + "&dn=" + quote(release["title"], safe="") + "&tr=" + quote(announce, safe=""),
            "private": True, "credentialNotice": "Contains your revocable tracker credential. Keep it private."}


@router.get("/api/library/private/{release_id}/torrent")
async def browser_torrent(release_id: str, request: Request, identity=Depends(require_member)):
    _member_rate(str(identity["id"]))
    row = await get_user_api_key(str(identity["id"]))
    if not row:
        raise HTTPException(409, "Create a personal indexer key first")
    return await torrent_response(release_id, request, row)


def _member_rate(user_id):
    from torznab import _torznab_rate_ok
    ok, retry = _torznab_rate_ok(("private-member", user_id))
    if not ok:
        raise HTTPException(429, "Rate limit exceeded", headers={"Retry-After": str(retry)})


def _announce_params(request: Request):
    raw = request.scope.get("query_string", b"")
    if len(raw) > 4096:
        raise ValueError("Invalid announce")
    pairs = parse_qsl(raw.decode("ascii"), encoding="latin-1", errors="strict", keep_blank_values=True)
    params = dict(pairs)
    if len(params) != len(pairs):
        raise ValueError("Duplicate announce parameter")
    info_hash = params["info_hash"].encode("latin-1")
    peer_id = params["peer_id"].encode("latin-1")
    if len(info_hash) != 20 or len(peer_id) != 20:
        raise ValueError("Invalid announce identifiers")
    values = {}
    for name in ("port", "uploaded", "downloaded", "left"):
        value = params[name]
        if not re.fullmatch(r"\d{1,19}", value) or int(value) > _MAX_COUNTER:
            raise ValueError("Invalid announce counter")
        values[name] = int(value)
    if not 1 <= values["port"] <= 65535:
        raise ValueError("Invalid peer port")
    event = params.get("event", "")
    if event not in {"", "started", "completed", "stopped"}:
        raise ValueError("Invalid event")
    numwant = int(params.get("numwant", "50"))
    if not 0 <= numwant <= 200:
        raise ValueError("Invalid peer count")
    return info_hash.hex(), peer_id, values, event, numwant


def _peer_ip(request: Request):
    raw = request.client.host if request.client else ""
    if proxy_provenance_valid(request):
        # Dedicated proxy location must overwrite this with $remote_addr.
        # Never trust caller-supplied ip or an arbitrary forwarded chain.
        raw = request.headers.get("x-bitagent-peer-ip", "")
    ip = ipaddress.ip_address(raw)
    if ip.is_unspecified or ip.is_multicast:
        raise ValueError("Invalid peer address")
    networks = [ipaddress.ip_network(c.strip()) for c in settings.private_peer_cidrs.split(",") if c.strip()]
    if networks and not any(ip in n for n in networks):
        raise ValueError("Peer must use the approved member network")
    return str(ip)


@router.get("/private/announce/{passkey}")
async def announce(passkey: str, request: Request):
    from torrent_metainfo import bencode
    enabled()
    try:
        if not re.fullmatch(r"bt_[0-9a-f]{64}", passkey):
            raise ValueError("Invalid tracker credential")
        db = await get_db()
        rows = await db.execute_fetchall("""SELECT k.id,k.user_id,k.key_hash AS source_key_hash FROM private_tracker_keys t
            JOIN user_api_keys k ON t.api_key_id=k.id AND k.revoked_at IS NULL
            JOIN private_members m ON m.user_id=k.user_id AND m.active=1 WHERE t.key_hash=?""", (_hash_user_api_key(passkey),))
        if not rows:
            raise ValueError("Invalid tracker credential")
        row = dict(rows[0])
        if not hmac.compare_digest(passkey, _derive_tracker_key(row["id"], row["source_key_hash"])):
            raise ValueError("Retired tracker credential")
        info_hash, peer_id, counters, event, numwant = _announce_params(request)
        ip = _peer_ip(request)
        privatebindings.require_match(ip, row["user_id"])
        gate = readiness.capture()
        clause, args = readiness.predicate(gate=gate)
        releases = await db.execute_fetchall("SELECT id,size,ready,verified_at,withdrawn," + readiness._COLUMNS +
                                             " FROM private_releases WHERE info_hash=? AND " + clause, (info_hash, *args))
        if not releases:
            raise ValueError("Torrent unavailable")
        release = dict(releases[0])
        release_id = release["id"]
        if counters["left"] > releases[0]["size"] or (event == "completed" and counters["left"] != 0):
            raise ValueError("Invalid remaining bytes")
        # Per-key limiter is shared with Torznab. Each peer announces no more
        # than every 15min, leaving room for large legitimate libraries.
        from torznab import _torznab_rate_ok
        if not _torznab_rate_ok(("tracker", row["id"]), rate=settings.private_tracker_rate_limit_per_min)[0]:
            raise ValueError("Rate limit exceeded")
        now = time.time()
        async with private_write() as db:
            # A queued writer may outlive the snapshot checked above. Recheck
            # after taking the write transaction and before any observation.
            privatebindings.require_match(ip, row["user_id"])
            if settings.private_peer_bindings_required:
                current = await db.execute_fetchall("""SELECT k.id FROM user_api_keys k
                    JOIN private_members m ON m.user_id=k.user_id AND m.active=1
                    WHERE k.id=? AND k.user_id=? AND k.key_hash=? AND k.revoked_at IS NULL""",
                    (row["id"], row["user_id"], row["source_key_hash"]))
                if not current:
                    raise ValueError("Retired tracker credential")
            if not await readiness.recheck(db, release, gate):
                raise ValueError("Torrent unavailable")
            # Bound persistent peer rows even for an approved member rotating
            # peer IDs. Normal clients use one ID per torrent.
            active = await db.execute_fetchall("SELECT count(*) n FROM private_peers WHERE release_id=? AND user_id=? AND updated_at>?", (release_id, row["user_id"], now-_PEER_TTL))
            previous = await db.execute_fetchall("SELECT * FROM private_peers WHERE release_id=? AND user_id=? AND peer_id=?",
                                                (release_id, row["user_id"], peer_id))
            if not previous and active[0]["n"] >= 10:
                raise ValueError("Peer limit exceeded")
            prior = dict(previous[0]) if previous else None
            # First/new-key/expired observations establish a baseline. The same
            # active peer keeps a high-water mark even for repeated started
            # events: a delayed report cannot lower it and re-credit old bytes.
            # A stop, new peer ID/key, or expiry opens a new counter epoch.
            continuing = bool(prior and prior["api_key_id"] == row["id"] and prior["updated_at"] > now-_PEER_TTL)
            high_uploaded = max(counters["uploaded"], prior["uploaded"]) if continuing else counters["uploaded"]
            high_downloaded = max(counters["downloaded"], prior["downloaded"]) if continuing else counters["downloaded"]
            uploaded = high_uploaded-prior["uploaded"] if continuing else 0
            downloaded = high_downloaded-prior["downloaded"] if continuing else 0
            completed = int(bool(continuing and event == "completed" and prior["remaining"] > 0 and not prior["completed"]))
            privatebindings.require_match(ip, row["user_id"])
            await db.execute("""INSERT INTO private_transfer_totals(user_id,release_id,uploaded,downloaded,completed)
                VALUES (?,?,?,?,?) ON CONFLICT(user_id,release_id) DO UPDATE SET
                uploaded=min(9223372036854775807,uploaded+excluded.uploaded),
                downloaded=min(9223372036854775807,downloaded+excluded.downloaded),completed=completed+excluded.completed""",
                (row["user_id"], release_id, uploaded, downloaded, completed))
            if event == "stopped":
                await db.execute("DELETE FROM private_peers WHERE release_id=? AND user_id=? AND peer_id=?", (release_id, row["user_id"], peer_id))
            else:
                await db.execute("""INSERT OR REPLACE INTO private_peers VALUES (?,?,?,?,?,?,?,?,?,?,?)""",
                    (release_id,row["user_id"],row["id"],peer_id,ip,counters["port"],high_uploaded,high_downloaded,counters["left"],
                     int(completed or (continuing and prior["completed"])), now))
            await db.execute("DELETE FROM private_peers WHERE updated_at<?", (now-_PEER_TTL,))
            peers = await db.execute_fetchall("""SELECT p.* FROM private_peers p
                JOIN user_api_keys k ON k.id=p.api_key_id AND k.revoked_at IS NULL
                JOIN private_members m ON m.user_id=p.user_id AND m.active=1
                WHERE p.release_id=? AND p.updated_at>? LIMIT 10000""", (release_id, now-_PEER_TTL))
            # Expiry/revocation during awaited database I/O rolls the whole
            # observation back instead of committing a partially stale claim.
            privatebindings.require_match(ip, row["user_id"])
            if not await readiness.recheck(db, release, gate):
                raise ValueError("Torrent unavailable")
        if not await readiness.recheck(await get_db(), release, gate):
            raise ValueError("Torrent unavailable")
        peers = [p for p in peers if privatebindings.matches(p["ip"], p["user_id"])]
        peers4, peers6, count = bytearray(), bytearray(), 0
        for peer in peers:
            if (peer["user_id"] == row["user_id"] and peer["peer_id"] == peer_id) or count >= numwant:
                continue
            address = ipaddress.ip_address(peer["ip"])
            (peers4 if address.version == 4 else peers6).extend(address.packed + struct.pack("!H", peer["port"]))
            count += 1
        result = {b"interval": 900, b"min interval": 60,
                  b"complete": sum(p["remaining"] == 0 for p in peers),
                  b"incomplete": sum(p["remaining"] > 0 for p in peers), b"peers": bytes(peers4), b"peers6": bytes(peers6)}
    except (ValueError, KeyError, UnicodeError, OverflowError):
        # Stable generic failure avoids exposing credentials, titles or account
        # membership through error messages. Tracker failures use bencode/200.
        result = {b"failure reason": b"Unauthorized or invalid announce"}
    return Response(bencode(result), media_type="text/plain", headers={"Cache-Control": "no-store"})


async def _transfer_summary(db, where, args):
    fields = ("uploaded", "downloaded", "completed", "issued")
    aggregates = ",".join(f"coalesce(sum({name}),0) AS {name}" for name in fields)
    try:
        rows = await db.execute_fetchall(
            "SELECT count(*) AS total_items," + aggregates + " FROM private_transfer_totals t" + where, args,
        )
        return dict(rows[0])
    except aiosqlite.OperationalError as error:
        if str(error) != "integer overflow":
            raise
    # Individual stored counters fit SQLite integers, but their account sum
    # may not. Preserve exact bytes with Python integers and bounded batches.
    totals = {name: 0 for name in (*fields, "total_items")}
    async with db.execute("SELECT " + ",".join(fields) + " FROM private_transfer_totals t" + where, args) as cursor:
        while batch := await cursor.fetchmany(1000):
            totals["total_items"] += len(batch)
            for row in batch:
                for name in fields:
                    totals[name] += row[name]
    return totals


async def _metrics(user_id: str | None):
    args = () if user_id is None else (user_id,)
    where = "" if user_id is None else " WHERE t.user_id=?"
    # Initialize the schema/WAL first. A dedicated read transaction pins both
    # queries (including aggregate overflow fallback) to the same snapshot,
    # without placing BEGIN on the app's shared writer connection.
    await get_db()
    async with aiosqlite.connect(settings.db_path, timeout=5) as db:
        db.row_factory = aiosqlite.Row
        await db.execute("PRAGMA query_only=ON")
        await db.execute("BEGIN")
        try:
            summary = await _transfer_summary(db, where, args)
            rows = await db.execute_fetchall("""SELECT t.*,r.title,r.info_hash FROM private_transfer_totals t
                JOIN private_releases r ON r.id=t.release_id""" + where + " ORDER BY t.user_id,r.title LIMIT 1000", args)
        finally:
            await db.rollback()
    items = []
    for row in rows:
        item = dict(row)
        item["ratio"] = row["uploaded"] / row["downloaded"] if row["downloaded"] else None
        items.append(item)
    total_items = summary.pop("total_items")
    summary["ratio"] = summary["uploaded"] / summary["downloaded"] if summary["downloaded"] else None
    return {"source": "client-reported tracker observations", "issuedMeaning": "credentialed torrent/magnet requests, not completed downloads",
            "items": items, "limit": 1000, "totals": summary, "totalItems": total_items,
            "itemsTruncated": total_items > len(items)}


@router.get("/api/account/private/metrics")
async def own_metrics(identity=Depends(require_member)):
    _member_rate(str(identity["id"]))
    return await _metrics(str(identity["id"]))


@router.get("/api/private/metrics")
async def all_metrics(identity=Depends(require_operator)):
    enabled()
    return await _metrics(None)

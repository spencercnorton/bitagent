"""Optional purpose-signed invitation bootstrap for a verified SSO service.

The service must reserve a stable provider principal before redemption and
activate its project capability only after fresh membership authority. This
bridge grants no browser identity, operator role or peer-network admission.
"""
from __future__ import annotations

import asyncio
import hashlib
import hmac
import json
import os
import re
import secrets
import stat
import time
from urllib.parse import urlsplit

from fastapi import APIRouter, HTTPException, Request
from fastapi.responses import Response

from auth import proxy_provenance_valid
from config import settings
from database import get_db
from deps import _host_scope, _host_sets, _normalise_hostname
import invitations
from private_indexer import private_write

router = APIRouter()
PATH = "/api/invitations/bootstrap"
CLIENT = "bitagent-auth"
PURPOSE = "bitagent-invitation-bootstrap"
_HEX_ID = re.compile(r"[0-9a-f]{32}")
_HASH = re.compile(r"[0-9a-f]{64}")
_NONCE = re.compile(r"[A-Za-z0-9_-]{32}")
_PROVIDERS = frozenset({"google", "microsoft", "discord", "plex"})
_KEY: bytes | None = None
_CONFIG: tuple[str, str, str] | None = None
_BODY_LIMIT = 4096
_ACTIVE_LIMIT = 1000
_NONCE_LIMIT = 4096


async def init_schema(db):
    await db.executescript("""
        CREATE TABLE IF NOT EXISTS invitation_bootstrap_enrollments (
            id TEXT PRIMARY KEY, invite_id TEXT NOT NULL,
            auth_request_id TEXT NOT NULL UNIQUE,
            browser_binding_hash TEXT NOT NULL, created_at REAL NOT NULL,
            expires_at REAL NOT NULL, principal_id INTEGER,
            provider TEXT, subject_binding_hash TEXT, redeemed_at REAL
        );
        CREATE INDEX IF NOT EXISTS idx_invitation_bootstrap_expiry
            ON invitation_bootstrap_enrollments(expires_at);
        CREATE TABLE IF NOT EXISTS invitation_bootstrap_nonces (
            client TEXT NOT NULL, nonce TEXT NOT NULL, expires_at REAL NOT NULL,
            PRIMARY KEY(client,nonce)
        );
        CREATE INDEX IF NOT EXISTS idx_invitation_bootstrap_nonce_expiry
            ON invitation_bootstrap_nonces(expires_at);
    """)


def _https_url(value, path, *, operator=False):
    try:
        p = urlsplit(value)
        _, operators = _host_sets()
        valid = (isinstance(value, str) and p.scheme == "https" and p.hostname
                 and p.hostname == p.netloc and p.path == path
                 and _normalise_hostname(p.netloc) == p.netloc
                 and not p.query and not p.fragment and not p.username and not p.password
                 and len(value) <= 2048 and not any(c.isspace() for c in value)
                 and value == f"https://{p.hostname}{path}"
                 and (not operator or p.hostname in operators))
    except (TypeError, ValueError):
        valid = False
    if not valid:
        raise RuntimeError("Invitation bridge requires a canonical HTTPS endpoint")
    return f"https://{p.hostname}"


def validate_settings():
    """Read the distinct protected key once, after trusted startup hydration."""
    global _KEY, _CONFIG
    _KEY, _CONFIG = None, None
    if not settings.private_invitation_bridge_enabled:
        return
    if not (settings.private_invitations_enabled and settings.private_indexer_enabled
            and settings.require_auth and (settings.trust_npm_headers or settings.trust_forwarded_user)):
        raise RuntimeError("Invitation bridge requires verified private invitation authentication")
    _https_url(settings.invitation_bridge_url, PATH, operator=True)
    _https_url(settings.invitation_sign_in_url, "/invitations/start")
    if not hasattr(os, "O_NOFOLLOW") or not hasattr(os, "geteuid"):
        raise RuntimeError("Invitation bridge requires protected POSIX key files")
    path = settings.invitation_bridge_secret_file
    try:
        if not path or os.path.realpath(path) != os.path.abspath(path):
            raise ValueError("Unsafe key path")
        fd = os.open(path, os.O_RDONLY | os.O_NOFOLLOW | os.O_NONBLOCK)
        try:
            s = os.fstat(fd)
            if (not stat.S_ISREG(s.st_mode) or s.st_nlink != 1
                    or s.st_uid not in {0, os.geteuid()} or s.st_mode & 0o077
                    or not 32 <= s.st_size <= 4096):
                raise ValueError("Unsafe key metadata")
            key = os.read(fd, 4097)
            if len(key) != s.st_size or not 32 <= len(key) <= 4096:
                raise ValueError("Unsafe key length")
        finally:
            os.close(fd)
        if any(hmac.compare_digest(key.strip(), str(value).encode().strip()) for value in (
            settings.proxy_auth_secret, settings.dashboard_api_key, settings.private_indexer_secret
        ) if value):
            raise ValueError("Reused key")
    except (OSError, ValueError):
        raise RuntimeError("Invitation bridge requires a distinct protected regular key file") from None
    _KEY = key
    _CONFIG = (settings.invitation_bridge_url, path, settings.invitation_sign_in_url)


def _enabled():
    invitations.enabled()
    if not settings.private_invitation_bridge_enabled:
        raise HTTPException(404, "Not found")
    if _KEY is None or _CONFIG != (
        settings.invitation_bridge_url, settings.invitation_bridge_secret_file, settings.invitation_sign_in_url
    ):
        raise HTTPException(503, "Invitation bridge unavailable")


def sign_in_url():
    """A landing-only form target; disabled/stale configuration emits none."""
    try:
        _enabled()
    except HTTPException:
        return ""
    return settings.invitation_sign_in_url


def check_ingress(request):
    _enabled()
    if (_host_scope(request) != "operator" or request.method != "POST"
            or request.scope.get("raw_path", b"") != PATH.encode()
            or request.scope.get("query_string")
            or request.headers.getlist("host") != [urlsplit(settings.invitation_bridge_url).netloc]):
        raise HTTPException(404, "Not found")
    # The machine location must clear every browser identity/credential. A
    # human/operator key never substitutes for the distinct purpose HMAC.
    forbidden = {"cookie", "authorization", "x-api-key", "origin", "sec-fetch-site",
                 "x-forwarded-user", "remote-user"}
    if (any(k in request.headers for k in forbidden)
            or any(k.startswith("x-auth-") for k in request.headers)
            or not proxy_provenance_valid(request)):
        raise HTTPException(401, "Invitation bridge authentication required")
    if (len(request.headers.getlist("content-type")) != 1
            or request.headers.get("content-type", "").split(";", 1)[0].strip().lower() != "application/json"
            or len(request.headers.getlist("content-encoding")) > 1
            or request.headers.get("content-encoding", "identity") != "identity"):
        raise HTTPException(415, "JSON required")


def canonical(*, direction, origin, operation, timestamp, nonce, body_hash,
              request_hash=None, status=None):
    lines = ["v1", f"purpose={PURPOSE}", f"direction={direction}", f"client={CLIENT}",
             f"origin={origin}", "method=POST", f"path={PATH}", f"operation={operation}",
             f"timestamp={timestamp}", f"nonce={nonce}", f"body_sha256={body_hash}"]
    if direction == "response":
        lines += [f"request_body_sha256={request_hash}", f"http_status={status}"]
    return "\n".join(lines).encode("ascii")


def _strict_json(raw):
    def pairs(values):
        out = {}
        for k, v in values:
            if k in out:
                raise ValueError("Duplicate field")
            out[k] = v
        return out
    def reject(value):
        raise ValueError("Non-JSON constant")
    try:
        body = json.loads(raw, object_pairs_hook=pairs, parse_constant=reject)
    except (ValueError, UnicodeDecodeError, RecursionError):
        raise HTTPException(422, "Invalid invitation bridge request") from None
    operation = body.get("operation") if isinstance(body, dict) else None
    if not isinstance(operation, str):
        raise HTTPException(422, "Invalid invitation bridge request")
    fields = {"version", "operation", "authRequestId"}
    if operation == "start":
        fields |= {"token", "browserBindingHash"}
    elif operation in {"redeem", "status"}:
        fields |= {"enrollmentId", "principalId", "provider", "subjectBindingHash"}
    else:
        raise HTTPException(422, "Invalid invitation bridge request")
    if (set(body) != fields or type(body["version"]) is not int or body["version"] != 1
            or not isinstance(body["authRequestId"], str) or not _HEX_ID.fullmatch(body["authRequestId"])):
        raise HTTPException(422, "Invalid invitation bridge request")
    if operation == "start":
        valid = (invitations._hash(body["token"]) is not None
                 and isinstance(body["browserBindingHash"], str) and _HASH.fullmatch(body["browserBindingHash"]))
    else:
        valid = (isinstance(body["enrollmentId"], str) and _HEX_ID.fullmatch(body["enrollmentId"])
                 and type(body["principalId"]) is int and 0 < body["principalId"] <= 2**63 - 1
                 and isinstance(body["provider"], str) and body["provider"] in _PROVIDERS
                 and isinstance(body["subjectBindingHash"], str) and _HASH.fullmatch(body["subjectBindingHash"]))
    if not valid:
        raise HTTPException(422, "Invalid invitation bridge request")
    return body


def _header(request, name):
    values = request.headers.getlist(name)
    if len(values) != 1:
        raise HTTPException(401, "Invitation bridge authentication required")
    return values[0]


async def _authenticate(request):
    check_ingress(request)
    raw = bytearray()
    async for chunk in request.stream():
        if len(raw) + len(chunk) > _BODY_LIMIT:
            raise HTTPException(413, "Invitation bridge request too large")
        raw.extend(chunk)
    body = _strict_json(raw)
    timestamp = _header(request, "x-invitation-time")
    nonce = _header(request, "x-invitation-nonce")
    signature = _header(request, "x-invitation-signature")
    if (_header(request, "x-invitation-client") != CLIENT
            or not re.fullmatch(r"[0-9]{1,12}", timestamp) or str(int(timestamp)) != timestamp
            or abs(time.time() - int(timestamp)) > 60
            or not _NONCE.fullmatch(nonce) or not _HASH.fullmatch(signature)):
        raise HTTPException(401, "Invitation bridge authentication required")
    digest = hashlib.sha256(raw).hexdigest()
    origin = settings.invitation_bridge_url.removesuffix(PATH)
    expected = hmac.new(_KEY, canonical(direction="request", origin=origin,
        operation=body["operation"], timestamp=timestamp, nonce=nonce, body_hash=digest), hashlib.sha256).hexdigest()
    if not hmac.compare_digest(signature, expected):
        raise HTTPException(401, "Invitation bridge authentication required")
    invitations._rate(("bootstrap", CLIENT), 30)
    return body, digest, nonce


def _unavailable():
    raise HTTPException(409, "Invitation unavailable")


def _authority_window(now, row, invite):
    issued = int(now)
    expiry = min(issued + 30, int(row["expires_at"]), int(invite["expires_at"]))
    # Integer wire timestamps must contain a full remaining second. A nearly
    # expired flow must not commit a member that its response cannot authorize.
    if expiry <= issued or expiry - now < 1:
        _unavailable()
    return issued, expiry


async def _valid_invite(db, invite_id, now):
    rows = await db.execute_fetchall("SELECT * FROM membership_invitations WHERE id=?", (invite_id,))
    if not rows or rows[0]["revoked_at"] is not None or rows[0]["expires_at"] <= now:
        _unavailable()
    row = rows[0]
    if not await invitations._active(db, row["issuer_id"]):
        _unavailable()
    return row


async def _start(db, body, now):
    rows = await db.execute_fetchall("SELECT id FROM membership_invitations WHERE token_hash=?", (invitations._hash(body["token"]),))
    if not rows:
        _unavailable()
    invite = await _valid_invite(db, rows[0]["id"], now)
    if invite["redeemed_at"] is not None:
        _unavailable()
    existing = await db.execute_fetchall("SELECT * FROM invitation_bootstrap_enrollments WHERE auth_request_id=?", (body["authRequestId"],))
    if existing:
        row = existing[0]
        if (row["invite_id"] != invite["id"] or row["browser_binding_hash"] != body["browserBindingHash"]
                or row["expires_at"] <= now):
            _unavailable()
        return {"version": 1, "enrollmentId": row["id"], "expiresAt": int(row["expires_at"])}
    active = (await db.execute_fetchall("SELECT COUNT(*) AS n FROM invitation_bootstrap_enrollments WHERE expires_at>?", (now,)))[0]["n"]
    if active >= _ACTIVE_LIMIT:
        raise HTTPException(429, "Invitation enrollment limit")
    enrollment_id = secrets.token_hex(16)
    expiry = min(int(invite["expires_at"]), int(now) + 600)
    if expiry <= now:
        _unavailable()
    await db.execute("INSERT INTO invitation_bootstrap_enrollments VALUES (?,?,?,?,?,?,NULL,NULL,NULL,NULL)",
        (enrollment_id, invite["id"], body["authRequestId"], body["browserBindingHash"], now, expiry))
    return {"version": 1, "enrollmentId": enrollment_id, "expiresAt": expiry}


async def _bound(db, body, now):
    rows = await db.execute_fetchall("SELECT * FROM invitation_bootstrap_enrollments WHERE id=? AND auth_request_id=?",
                                     (body["enrollmentId"], body["authRequestId"]))
    if not rows or rows[0]["expires_at"] <= now:
        _unavailable()
    row = rows[0]
    invite = await _valid_invite(db, row["invite_id"], now)
    if row["principal_id"] is not None and any(row[k] != body[field] for k, field in (
        ("principal_id", "principalId"), ("provider", "provider"), ("subject_binding_hash", "subjectBindingHash")
    )):
        _unavailable()
    return row, invite


async def _redeem(db, body, now):
    row, invite = await _bound(db, body, now)
    uid = str(-body["principalId"])
    if invite["redeemed_at"] is not None:
        if (row["redeemed_at"] is None or invite["redeemed_by"] != uid
                or not await invitations._active(db, uid)):
            _unavailable()
    already = await invitations._redeem_for_subject(db, invite, uid, now)
    if not already:
        await db.execute("""UPDATE invitation_bootstrap_enrollments
            SET principal_id=?,provider=?,subject_binding_hash=?,redeemed_at=? WHERE id=?""",
            (body["principalId"], body["provider"], body["subjectBindingHash"], now, row["id"]))
    now = time.time()
    issued, expiry = _authority_window(now, row, invite)
    return {"version": 1, "enrollmentId": row["id"], "principalId": body["principalId"],
            "accountId": uid, "approved": True, "alreadyRedeemed": already,
            "issuedAt": issued, "expiresAt": expiry}


async def _status(db, body, now):
    try:
        row, invite = await _bound(db, body, now)
        # Status is reconciliation, never a provider-principal reservation or
        # binding operation. Before redemption an unbound row is unavailable.
        approved = (row["principal_id"] is not None and row["redeemed_at"] is not None
                    and invite["redeemed_by"] == str(-body["principalId"])
                    and await invitations._active(db, str(-body["principalId"])))
        state = "redeemed" if approved else "unavailable"
        now = time.time()
        issued, expiry = _authority_window(now, row, invite)
    except HTTPException as exc:
        if exc.status_code != 409:
            raise
        approved, state, issued, expiry = False, "unavailable", int(now), int(now) + 30
    return {"version": 1, "enrollmentId": body["enrollmentId"], "principalId": body["principalId"],
            "accountId": str(-body["principalId"]), "state": state, "approved": bool(approved),
            "issuedAt": issued, "expiresAt": expiry}


@router.post(PATH)
async def bootstrap(request: Request):
    try:
        async with asyncio.timeout(10):
            return await _bootstrap(request, time.monotonic() + 10)
    except TimeoutError:
        raise HTTPException(503, "Invitation bridge unavailable") from None


async def _bootstrap(request, deadline):
    body, request_hash, nonce = await _authenticate(request)
    await get_db()
    status = 200
    async with private_write(deadline=deadline) as db:
        _enabled()
        now = time.time()
        if abs(now - int(request.headers["x-invitation-time"])) > 60:
            raise HTTPException(401, "Invitation bridge authentication required")
        await db.execute("DELETE FROM invitation_bootstrap_nonces WHERE expires_at<=?", (now,))
        await db.execute("DELETE FROM invitation_bootstrap_enrollments WHERE expires_at<=?", (now,))
        prior = await db.execute_fetchall("SELECT 1 FROM invitation_bootstrap_nonces WHERE client=? AND nonce=?", (CLIENT, nonce))
        if prior:
            raise HTTPException(401, "Invitation bridge replay rejected")
        count = (await db.execute_fetchall("SELECT COUNT(*) AS n FROM invitation_bootstrap_nonces"))[0]["n"]
        if count >= _NONCE_LIMIT:
            raise HTTPException(429, "Invitation bridge request limit")
        await db.execute("INSERT INTO invitation_bootstrap_nonces VALUES (?,?,?)", (CLIENT, nonce, now + 120))
        await db.execute("SAVEPOINT enrollment_operation")
        try:
            content = await {"start": _start, "redeem": _redeem, "status": _status}[body["operation"]](db, body, now)
        except HTTPException as error:
            await db.execute("ROLLBACK TO enrollment_operation")
            status, content = error.status_code, {"detail": error.detail}
        await db.execute("RELEASE enrollment_operation")
    # Membership authority is linearized by this transaction. A subsequent
    # suspension can invalidate it immediately; the recipient must still
    # enforce current membership for every protected action and network lease.
    raw = json.dumps(content, separators=(",", ":"), ensure_ascii=True).encode()
    timestamp = str(int(time.time()))
    signature = hmac.new(_KEY, canonical(direction="response",
        origin=settings.invitation_bridge_url.removesuffix(PATH), operation=body["operation"],
        timestamp=timestamp, nonce=nonce, body_hash=hashlib.sha256(raw).hexdigest(),
        request_hash=request_hash, status=status), hashlib.sha256).hexdigest()
    return Response(raw, status_code=status, media_type="application/json", headers={
        "X-Invitation-Client": CLIENT, "X-Invitation-Time": timestamp,
        "X-Invitation-Nonce": nonce, "X-Invitation-Signature": signature,
        "Cache-Control": "no-store", "Referrer-Policy": "no-referrer"})

"""Single-use invitations for existing, verified SSO identities.

Invitations grant application membership, never peer-network enrollment or
content readiness. Generated invitations permanently consume their allowance.
Payment verification is handled by a separate, disabled-by-default adapter.
"""
from __future__ import annotations

from collections import OrderedDict
from datetime import datetime, timezone
import hashlib
import json
import re
import secrets
import time

import aiosqlite
import asyncio
from fastapi import APIRouter, Depends, HTTPException, Request
from fastapi.responses import JSONResponse

from auth import require_human_sso
from config import settings
from database import get_db
from deps import _host_scope
from private_indexer import private_write

router = APIRouter()
_TOKEN = re.compile(r"bi_[A-Za-z0-9_-]{43}")
_ID = re.compile(r"[0-9a-f]{32}")
_TTL = 7 * 86400
_BODY_LIMIT = 8192
BODY_TIMEOUT = 10
_RATE_BUCKETS: OrderedDict = OrderedDict()
_RATE_BUCKET_LIMIT = 4096


async def init_schema(db):
    """Add invitation ledgers without changing existing identity or usage IDs."""
    await db.executescript("""
        CREATE TABLE IF NOT EXISTS membership_invitations (
            id TEXT PRIMARY KEY, token_hash TEXT NOT NULL UNIQUE,
            issuer_id TEXT NOT NULL, allowance_year INTEGER NOT NULL,
            credit_source TEXT NOT NULL CHECK(credit_source IN ('annual','paid')),
            created_at REAL NOT NULL, expires_at REAL NOT NULL,
            revoked_at REAL, redeemed_at REAL, redeemed_by TEXT
        );
        CREATE INDEX IF NOT EXISTS idx_membership_invites_issuer
            ON membership_invitations(issuer_id, created_at DESC, id);
        CREATE INDEX IF NOT EXISTS idx_membership_invites_allowance
            ON membership_invitations(issuer_id, allowance_year, credit_source);
        CREATE TABLE IF NOT EXISTS invitation_payments (
            payment_id TEXT PRIMARY KEY, event_id TEXT NOT NULL UNIQUE,
            user_id TEXT NOT NULL, amount INTEGER NOT NULL,
            currency TEXT NOT NULL, credited_at REAL NOT NULL
        );
        CREATE INDEX IF NOT EXISTS idx_invitation_payments_user
            ON invitation_payments(user_id);
        CREATE TABLE IF NOT EXISTS invitation_payment_events (
            event_id TEXT PRIMARY KEY, payment_id TEXT NOT NULL
        );
        CREATE TABLE IF NOT EXISTS invitation_credit_uses (
            invitation_id TEXT PRIMARY KEY, payment_id TEXT NOT NULL UNIQUE
        );
        CREATE TABLE IF NOT EXISTS invitation_payment_tombstones (
            payment_id TEXT PRIMARY KEY, reason TEXT NOT NULL, invalidated_at REAL NOT NULL
        );
    """)


def _subject(value):
    return (isinstance(value, str) and 1 <= len(value) <= 200
            and value == value.strip() and value not in {"anonymous", "api-client"}
            and not any(ord(c) < 32 or ord(c) == 127 for c in value))


def _owners():
    return frozenset(x.strip() for x in settings.invitation_owner_ids.split(",") if x.strip())


def validate_settings():
    if not settings.private_invitations_enabled:
        return
    if not settings.private_indexer_enabled or not settings.require_auth:
        raise RuntimeError("Private invitations require an authenticated private indexer")
    if not (settings.trust_npm_headers or settings.trust_forwarded_user):
        raise RuntimeError("Private invitations require a verified SSO proxy")
    if not 0 <= settings.invitation_annual_allowance <= 1000:
        raise RuntimeError("INVITATION_ANNUAL_ALLOWANCE must be between 0 and 1000")
    if not 1 <= settings.invitation_price_usd_cents <= 1000000:
        raise RuntimeError("INVITATION_PRICE_USD_CENTS must be between 1 and 1000000")
    if any(not _subject(uid) for uid in _owners()):
        raise RuntimeError("INVITATION_OWNER_IDS must contain exact existing SSO subjects")
    # The private indexer separately validates this fixed HTTPS origin against
    # PUBLIC_LIBRARY_HOSTS. Never derive a share URL from request Host headers.
    if not settings.private_indexer_url.startswith("https://"):
        raise RuntimeError("Invitations require a configured HTTPS library origin")


def enabled():
    if not (settings.private_invitations_enabled and settings.private_indexer_enabled):
        raise HTTPException(404, "Not found")


def _now():
    return time.time()


def _stamp(now):
    return datetime.fromtimestamp(now, timezone.utc).isoformat().replace("+00:00", "Z")


def _year(now):
    return datetime.fromtimestamp(now, timezone.utc).year


async def _active(db, uid):
    rows = await db.execute_fetchall("SELECT active FROM private_members WHERE user_id=?", (uid,))
    return bool(rows and rows[0]["active"] == 1)


async def revoke_pending(db, issuer_id, now):
    """Withdraw all pending links in the same transaction as member suspension."""
    await db.execute("""UPDATE membership_invitations SET revoked_at=?
        WHERE issuer_id=? AND redeemed_at IS NULL AND revoked_at IS NULL""", (now, issuer_id))


def _rate(key, maximum):
    """Bound both request rate and the number of retained transport/SSO buckets."""
    now = time.monotonic()
    while _RATE_BUCKETS and next(iter(_RATE_BUCKETS.values()))[1] <= now - 60:
        _RATE_BUCKETS.popitem(last=False)
    if key not in _RATE_BUCKETS and len(_RATE_BUCKETS) >= _RATE_BUCKET_LIMIT:
        raise HTTPException(429, "Invitation request limit", headers={"Retry-After": "60"})
    tokens, previous = _RATE_BUCKETS.pop(key, (float(maximum), now))
    tokens = min(float(maximum), tokens + max(0, now - previous) * maximum / 60)
    _RATE_BUCKETS[key] = (max(0, tokens - 1), now)
    if tokens < 1:
        raise HTTPException(429, "Invitation request limit", headers={"Retry-After": "60"})


def check_request(request):
    """Keep state changes JSON-only and same-origin under the verified proxy."""
    if request.headers.get("content-type", "").split(";", 1)[0].strip().lower() != "application/json":
        raise HTTPException(415, "JSON required")
    origin = request.headers.get("origin")
    if (origin is not None and origin != settings.private_indexer_url.rstrip("/")) or (
        request.headers.get("sec-fetch-site") not in {None, "same-origin"}
    ):
        raise HTTPException(403, "Same-origin request required")


async def _body(request, allowed):
    check_request(request)
    raw = bytearray()
    try:
        async with asyncio.timeout(BODY_TIMEOUT):
            async for chunk in request.stream():
                if len(raw) + len(chunk) > _BODY_LIMIT:
                    raise HTTPException(413, "Invitation request too large")
                raw.extend(chunk)
    except TimeoutError:
        raise HTTPException(408, "Invitation request deadline exceeded") from None
    def pairs(items):
        value = {}
        for key, item in items:
            if key in value:
                raise ValueError("Duplicate field")
            value[key] = item
        return value
    def invalid_constant(value):
        raise ValueError("Non-JSON constant")
    try:
        value = json.loads(raw, object_pairs_hook=pairs, parse_constant=invalid_constant)
    except (ValueError, UnicodeDecodeError, RecursionError):
        raise HTTPException(422, "Invalid invitation request") from None
    if not isinstance(value, dict) or set(value) - allowed:
        raise HTTPException(422, "Invalid invitation request")
    return value


def _account(identity, expected):
    if not isinstance(expected, str) or expected != identity["id"]:
        raise HTTPException(409, "Account changed; refresh before retrying")
    return identity["id"]


def _hash(token):
    if not isinstance(token, str) or not _TOKEN.fullmatch(token):
        return None
    return hashlib.sha256(token.encode("ascii")).hexdigest()


def _public(request):
    if _host_scope(request) != "public":
        raise HTTPException(404, "Not found")


async def _balance(db, uid):
    # Older unmapped paid invitations remain spent; never refund them on upgrade.
    available = (await db.execute_fetchall("""SELECT COUNT(*) AS n FROM invitation_payments p
        WHERE p.user_id=? AND NOT EXISTS(SELECT 1 FROM invitation_credit_uses u WHERE u.payment_id=p.payment_id)
        AND NOT EXISTS(SELECT 1 FROM invitation_payment_tombstones t WHERE t.payment_id=p.payment_id)""", (uid,)))[0]["n"]
    legacy = (await db.execute_fetchall("""SELECT COUNT(*) AS n FROM membership_invitations i
        WHERE i.issuer_id=? AND i.credit_source='paid'
        AND NOT EXISTS(SELECT 1 FROM invitation_credit_uses u WHERE u.invitation_id=i.id)""", (uid,)))[0]["n"]
    return max(0, available - legacy)


async def _paid_credit(db, uid):
    if await _balance(db, uid) <= 0:
        raise HTTPException(409, "No paid invitation credits")
    legacy = (await db.execute_fetchall("""SELECT COUNT(*) AS n FROM membership_invitations i
        WHERE i.issuer_id=? AND i.credit_source='paid'
        AND NOT EXISTS(SELECT 1 FROM invitation_credit_uses u WHERE u.invitation_id=i.id)""", (uid,)))[0]["n"]
    rows = await db.execute_fetchall("""SELECT p.payment_id FROM invitation_payments p
        WHERE p.user_id=? AND NOT EXISTS(SELECT 1 FROM invitation_credit_uses u WHERE u.payment_id=p.payment_id)
        AND NOT EXISTS(SELECT 1 FROM invitation_payment_tombstones t WHERE t.payment_id=p.payment_id)
        ORDER BY p.credited_at,p.payment_id LIMIT 1 OFFSET ?""", (uid, legacy))
    if not rows:
        raise HTTPException(409, "No paid invitation credits")
    return rows[0]["payment_id"]


def _metadata(row, now):
    status = "redeemed" if row["redeemed_at"] is not None else "revoked" if row["revoked_at"] is not None else "expired" if row["expires_at"] <= now else "pending"
    return {"id": row["id"], "createdAt": _stamp(row["created_at"]),
            "expiresAt": _stamp(row["expires_at"]), "status": status}


@router.get("/api/account/invitations")
async def account_invitations(request: Request, identity=Depends(require_human_sso)):
    _public(request)
    uid = identity["id"]
    if not settings.private_invitations_enabled:
        return {"enabled": False, "accountId": uid, "checkoutAvailable": False}
    enabled()
    _rate(("account", uid), 60)
    await get_db()
    async with aiosqlite.connect(settings.db_path, timeout=5) as db:
        db.row_factory = aiosqlite.Row
        await db.execute("PRAGMA query_only=ON")
        await db.execute("BEGIN")
        try:
            if not await _active(db, uid):
                raise HTTPException(403, "Approved membership required")
            now = _now()
            used = (await db.execute_fetchall("""SELECT COUNT(*) AS n FROM membership_invitations
                WHERE issuer_id=? AND allowance_year=? AND credit_source='annual'""", (uid, _year(now))))[0]["n"]
            balance = await _balance(db, uid)
            rows = await db.execute_fetchall("""SELECT id,created_at,expires_at,revoked_at,redeemed_at
                FROM membership_invitations WHERE issuer_id=? ORDER BY created_at DESC,id DESC LIMIT 100""", (uid,))
        finally:
            await db.rollback()
    unlimited = uid in _owners()
    from invitation_payments import checkout_available
    return {"schemaVersion": 1, "enabled": True, "accountId": uid, "unlimited": unlimited,
            "year": _year(now), "annualLimit": settings.invitation_annual_allowance, "annualUsed": used,
            "annualRemaining": None if unlimited else max(0, settings.invitation_annual_allowance - used),
            "paidCredits": balance, "price": {"amountMinor": settings.invitation_price_usd_cents, "currency": "USD"},
            "checkoutAvailable": checkout_available(uid), "invitations": [_metadata(row, now) for row in rows]}


@router.post("/api/account/invitations")
async def mint_invitation(request: Request, identity=Depends(require_human_sso)):
    enabled()
    _public(request)
    body = await _body(request, {"expectedAccountId", "creditSource"})
    uid = _account(identity, body.get("expectedAccountId"))
    source = body.get("creditSource", "annual")
    if not isinstance(source, str) or source not in {"annual", "paid"}:
        raise HTTPException(422, "Invalid invitation request")
    _rate(("mint", uid), 30)
    await get_db()
    async with private_write() as db:
        enabled()
        if not await _active(db, uid):
            raise HTTPException(403, "Approved membership required")
        now = _now()
        if source == "annual":
            used = (await db.execute_fetchall("""SELECT COUNT(*) AS n FROM membership_invitations
                WHERE issuer_id=? AND allowance_year=? AND credit_source='annual'""", (uid, _year(now))))[0]["n"]
            if uid not in _owners() and used >= settings.invitation_annual_allowance:
                raise HTTPException(409, "Annual invitation allowance exhausted")
        else:
            payment_id = await _paid_credit(db, uid)
        token, invite_id = "bi_" + secrets.token_urlsafe(32), secrets.token_hex(16)
        await db.execute("INSERT INTO membership_invitations VALUES (?,?,?,?,?,?,?,NULL,NULL,NULL)",
                         (invite_id, _hash(token), uid, _year(now), source, now, now + _TTL))
        if source == "paid":
            await db.execute("INSERT INTO invitation_credit_uses VALUES (?,?)", (invite_id, payment_id))
    return JSONResponse(status_code=201, content={"id": invite_id, "token": token,
        "createdAt": _stamp(now), "expiresAt": _stamp(now + _TTL), "creditSource": source,
        "shareUrl": settings.private_indexer_url.rstrip("/") + "/invite#" + token})


@router.delete("/api/account/invitations/{invite_id}")
async def revoke_invitation(invite_id: str, request: Request, identity=Depends(require_human_sso)):
    enabled()
    _public(request)
    body = await _body(request, {"expectedAccountId"})
    uid = _account(identity, body.get("expectedAccountId"))
    if not _ID.fullmatch(invite_id):
        raise HTTPException(404, "Not found")
    _rate(("revoke", uid), 60)
    await get_db()
    async with private_write() as db:
        rows = await db.execute_fetchall("SELECT redeemed_at FROM membership_invitations WHERE id=? AND issuer_id=?", (invite_id, uid))
        if not rows:
            raise HTTPException(404, "Not found")
        if rows[0]["redeemed_at"] is not None:
            raise HTTPException(409, "Invitation already redeemed")
        await db.execute("UPDATE membership_invitations SET revoked_at=COALESCE(revoked_at,?) WHERE id=? AND issuer_id=?", (_now(), invite_id, uid))
    return {"id": invite_id, "status": "revoked"}


@router.post("/api/invitations/preview")
async def preview_invitation(request: Request):
    enabled()
    _public(request)
    _rate(("preview", request.client.host if request.client else "unknown"), 30)
    try:
        body = await _body(request, {"token"})
    except HTTPException as error:
        if error.status_code != 422:
            raise
        return {"available": False}
    digest = _hash(body.get("token"))
    if digest is None:
        return {"available": False}
    rows = await (await get_db()).execute_fetchall("""SELECT i.expires_at FROM membership_invitations i
        JOIN private_members m ON m.user_id=i.issuer_id AND m.active=1
        WHERE i.token_hash=? AND i.revoked_at IS NULL AND i.redeemed_at IS NULL AND i.expires_at>?""", (digest, _now()))
    return {"available": True, "expiresAt": _stamp(rows[0]["expires_at"])} if rows else {"available": False}


@router.post("/api/invitations/redeem")
async def redeem_invitation(request: Request, identity=Depends(require_human_sso)):
    enabled()
    _public(request)
    body = await _body(request, {"token", "expectedAccountId"})
    uid = _account(identity, body.get("expectedAccountId"))
    digest = _hash(body.get("token"))
    if digest is None:
        raise HTTPException(409, "Invitation unavailable")
    _rate(("redeem", uid), 30)
    await get_db()
    async with private_write() as db:
        enabled()
        rows = await db.execute_fetchall("SELECT * FROM membership_invitations WHERE token_hash=?", (digest,))
        if not rows:
            raise HTTPException(409, "Invitation unavailable")
        row = rows[0]
        now = _now()
        if row["redeemed_at"] is None and (row["revoked_at"] is not None or row["expires_at"] <= now or not await _active(db, row["issuer_id"])):
            raise HTTPException(409, "Invitation unavailable")
        already = await _redeem_for_subject(db, row, uid, now)
    return {"approved": True, "alreadyRedeemed": already, "accountId": uid}


async def _redeem_for_subject(db, row, uid, now):
    """Consume under an owned transaction after the caller verifies identity.

    Human SSO derives uid from the verified proxy. The separate machine bridge
    derives it only from its authenticated reserved principal; it additionally
    rechecks the issuer and enrollment on every recovery acknowledgment.
    """
    if row["redeemed_at"] is not None:
        if row["redeemed_by"] == uid and await _active(db, uid):
            return True
        raise HTTPException(409, "Invitation unavailable")
    existing = await db.execute_fetchall("SELECT active FROM private_members WHERE user_id=?", (uid,))
    if existing:
        raise HTTPException(409 if existing[0]["active"] == 1 else 403, "Membership already managed")
    await db.execute("INSERT INTO private_members VALUES (?,1,?,?)", (uid, "invitation:" + row["id"], now))
    await db.execute("UPDATE membership_invitations SET redeemed_at=?,redeemed_by=? WHERE id=?", (now, uid, row["id"]))
    return False


async def fulfill_paid_credit(*, user_id, payment_id, event_id, amount, currency):
    """Record one credit after a future adapter verifies a paid provider event.

    This function is not an HTTP/payment verification boundary. A trusted
    adapter must verify signature, payment state and bound account beforehand.
    Both provider object and event IDs are unique; retries cannot credit twice.
    """
    enabled()
    if (not _subject(user_id) or type(amount) is not int or amount != settings.invitation_price_usd_cents
            or currency != "USD" or any(not isinstance(x, str) or not re.fullmatch(r"[A-Za-z0-9_-]{1,200}", x) for x in (payment_id, event_id))):
        raise ValueError("Invalid verified invitation payment")
    await get_db()
    async with private_write() as db:
        event_rows = await db.execute_fetchall("SELECT payment_id FROM invitation_payment_events WHERE event_id=?", (event_id,))
        if event_rows and event_rows[0]["payment_id"] != payment_id:
            raise ValueError("Invitation payment receipt conflict")
        rows = await db.execute_fetchall("SELECT * FROM invitation_payments WHERE payment_id=? OR event_id=?", (payment_id, event_id))
        if rows:
            if len(rows) != 1 or any(rows[0][key] != value for key, value in (
                ("payment_id", payment_id), ("user_id", user_id), ("amount", amount), ("currency", currency)
            )):
                raise ValueError("Invitation payment receipt conflict")
            await db.execute("INSERT OR IGNORE INTO invitation_payment_events VALUES (?,?)", (event_id, payment_id))
            return {"credited": False, "duplicate": True}
        await db.execute("INSERT INTO invitation_payments VALUES (?,?,?,?,?,?)", (payment_id, event_id, user_id, amount, currency, _now()))
        await db.execute("INSERT INTO invitation_payment_events VALUES (?,?)", (event_id, payment_id))
    return {"credited": True, "duplicate": False}

"""Default-off hosted Checkout with durable, verified invitation credits.

A browser return never grants credit. Unknown provider outcomes retain the
original immutable order and idempotency key. Refund/dispute tombstones prevent
late events from restoring a credit or a pending invitation.
"""
from __future__ import annotations

import asyncio
from contextlib import asynccontextmanager
import hashlib
import hmac
import json
import os
import re
import secrets
import stat
import time
import uuid
from urllib.parse import urlsplit

import aiosqlite
from fastapi import APIRouter, Depends, HTTPException, Request
import httpx

from auth import require_human_sso
from config import settings
from database import get_db
import invitations as invites
from private_indexer import private_write

router = APIRouter()
WEBHOOK = "/api/invitations/stripe/webhook"
API_ORIGIN = "https://api.stripe.com"
API_VERSION = "2025-08-27.basil"
AMOUNT = 5000
PURPOSE = "bitagent-invitation-credit"
BODY_LIMIT = 256 * 1024
PROVIDER_TIMEOUT = 10
_ID = re.compile(r"[A-Za-z0-9_]{1,200}\Z")
_ORDER = re.compile(r"[0-9a-f]{32}\Z")
_EVENT_TYPES = frozenset({"checkout.session.completed", "checkout.session.async_payment_succeeded",
                        "charge.refunded", "charge.dispute.created", "charge.dispute.funds_withdrawn"})
_CONFIG = None
_API_KEY = None
_WEBHOOK_KEY = None
_PROOF_UNTIL = 0.0
_TASK = None
_SLOTS = asyncio.Semaphore(4)
_WAITERS = 0


class PaymentUnavailable(Exception):
    """Never expose provider response bodies, URLs or credential-bearing errors."""


class UnrelatedPayment(Exception):
    """Authenticated provider object has no invitation-credit purpose."""


class PaymentManualHold(Exception):
    """Our object is malformed or cannot be bound to a durable order."""


def _require(condition):
    if not condition:
        raise PaymentUnavailable()


def _config():
    return (settings.invitation_stripe_api_key_file, settings.invitation_stripe_webhook_secret_file,
            settings.invitation_stripe_account_id, settings.invitation_stripe_price_id,
            settings.invitation_stripe_live_mode, settings.private_indexer_url)


def _read_secret(path):
    _require(isinstance(path, str) and path and os.path.realpath(path) == os.path.abspath(path))
    fd = os.open(path, os.O_RDONLY | os.O_NOFOLLOW | os.O_NONBLOCK)
    with os.fdopen(fd, "rb") as file:
        info = os.fstat(file.fileno())
        _require(stat.S_ISREG(info.st_mode) and info.st_nlink == 1 and info.st_uid in {0, os.getuid()}
                 and stat.S_IMODE(info.st_mode) in {0o400, 0o600})
        raw = file.read(4097)
    _require(32 <= len(raw) <= 4096 and raw.isascii() and not any(chr(x).isspace() for x in raw))
    return raw


def validate_settings():
    global _CONFIG, _API_KEY, _WEBHOOK_KEY, _PROOF_UNTIL
    _CONFIG, _API_KEY, _WEBHOOK_KEY, _PROOF_UNTIL = None, None, None, 0.0
    if not settings.private_invitation_checkout_enabled:
        return
    try:
        invites.validate_settings()
        _require(settings.private_invitations_enabled and settings.invitation_price_usd_cents == AMOUNT)
        _require(re.fullmatch(r"acct_[A-Za-z0-9]{1,100}", settings.invitation_stripe_account_id))
        _require(re.fullmatch(r"price_[A-Za-z0-9]{1,100}", settings.invitation_stripe_price_id))
        origin = urlsplit(settings.private_indexer_url)
        _require(origin.scheme == "https" and origin.netloc == origin.hostname and not origin.path
                 and not origin.query and not origin.fragment and not origin.username)
        api = _read_secret(settings.invitation_stripe_api_key_file)
        webhook = _read_secret(settings.invitation_stripe_webhook_secret_file)
        prefixes = (b"sk_live_", b"rk_live_") if settings.invitation_stripe_live_mode else (b"sk_test_", b"rk_test_")
        _require(api.startswith(prefixes) and webhook.startswith(b"whsec_") and api != webhook)
        _API_KEY, _WEBHOOK_KEY, _CONFIG = api, webhook, _config()
    except (OSError, ValueError, PaymentUnavailable):
        raise RuntimeError("Invitation Checkout requires protected keys, a fixed account and one USD price") from None


def enabled():
    invites.enabled()
    if not settings.private_invitation_checkout_enabled:
        raise HTTPException(404, "Not found")
    if _CONFIG is None or _CONFIG != _config():
        raise HTTPException(503, "Invitation payments unavailable")


def checkout_available(uid):
    return bool(settings.private_invitation_checkout_enabled and _CONFIG is not None
                and _CONFIG == _config() and time.monotonic() < _PROOF_UNTIL and uid not in invites._owners())


async def init_schema(db):
    await db.executescript("""
        CREATE TABLE IF NOT EXISTS invitation_checkout_orders (
            id TEXT PRIMARY KEY,user_id TEXT NOT NULL,request_id TEXT NOT NULL,
            account_id TEXT NOT NULL,price_id TEXT NOT NULL,amount INTEGER NOT NULL,
            currency TEXT NOT NULL,live_mode INTEGER NOT NULL,api_version TEXT NOT NULL,return_origin TEXT NOT NULL,
            created_at REAL NOT NULL,expires_at INTEGER NOT NULL,idempotency_key TEXT NOT NULL UNIQUE,
            state TEXT NOT NULL,session_id TEXT UNIQUE,payment_id TEXT UNIQUE,
            checkout_url TEXT,updated_at REAL NOT NULL,UNIQUE(user_id,request_id)
        );
        CREATE INDEX IF NOT EXISTS idx_invitation_orders_user
            ON invitation_checkout_orders(user_id,created_at DESC,id);
        CREATE TABLE IF NOT EXISTS invitation_stripe_events (
            id TEXT PRIMARY KEY,event_type TEXT NOT NULL,object_id TEXT NOT NULL,
            body_hash TEXT NOT NULL,state TEXT NOT NULL,received_at REAL NOT NULL,
            attempts INTEGER NOT NULL DEFAULT 0,next_attempt REAL NOT NULL DEFAULT 0
        );
        CREATE INDEX IF NOT EXISTS idx_invitation_events_pending
            ON invitation_stripe_events(state,next_attempt,received_at);
    """)


def _json(raw):
    def pairs(items):
        out = {}
        for key, value in items:
            _require(key not in out)
            out[key] = value
        return out
    try:
        value = json.loads(raw, object_pairs_hook=pairs, parse_constant=lambda _: _require(False))
    except (ValueError, TypeError, UnicodeDecodeError, RecursionError):
        raise PaymentUnavailable() from None
    _require(type(value) is dict)
    return value


def _identifier(value, prefix):
    return isinstance(value, str) and bool(_ID.fullmatch(value)) and value.startswith(prefix)


class StripeAPI:
    def __init__(self, client):
        self.client = client

    async def request(self, method, path, *, params=None, idempotency=None):
        # Every caller constructs a fixed route from a validated provider ID;
        # this method rejects absolute/encoded paths as an additional boundary.
        _require(len(path) <= 512 and re.fullmatch(r"/v1/(account|prices/price_[A-Za-z0-9_]+|checkout/sessions(?:/cs_[A-Za-z0-9_]+(?:/line_items)?)?|payment_intents/pi_[A-Za-z0-9_]+|charges/ch_[A-Za-z0-9_]+|disputes/dp_[A-Za-z0-9_]+)", path))
        headers = {"Authorization": "Bearer " + _API_KEY.decode("ascii"),
                   "Stripe-Version": API_VERSION, "Accept-Encoding": "identity"}
        if idempotency is not None:
            headers["Idempotency-Key"] = idempotency
        kwargs = {"data" if method == "POST" else "params": params} if params is not None else {}
        async with self.client.stream(method, API_ORIGIN + path, headers=headers, **kwargs) as response:
            _require(response.status_code == 200 and response.headers.get("content-encoding", "identity") == "identity")
            raw = bytearray()
            async for chunk in response.aiter_raw():
                _require(len(raw) + len(chunk) <= BODY_LIMIT)
                raw.extend(chunk)
            return _json(raw)


@asynccontextmanager
async def provider():
    global _WAITERS, _PROOF_UNTIL
    enabled()
    _require(_WAITERS < 16)
    _WAITERS += 1
    acquired = False
    try:
        async with asyncio.timeout(PROVIDER_TIMEOUT):
            await _SLOTS.acquire()
            acquired = True
            async with httpx.AsyncClient(verify=True, trust_env=False, follow_redirects=False, timeout=10) as client:
                try:
                    yield StripeAPI(client)
                except (UnrelatedPayment, PaymentManualHold):
                    # Object classification is not an account/Price outage.
                    raise
                except BaseException:
                    _PROOF_UNTIL = 0.0
                    raise
    except (UnrelatedPayment, PaymentManualHold):
        raise
    except BaseException:
        _PROOF_UNTIL = 0.0
        raise
    finally:
        _WAITERS -= 1
        if acquired:
            _SLOTS.release()


async def _verify_account(api):
    account = await api.request("GET", "/v1/account")
    _require(account.get("id") == settings.invitation_stripe_account_id and account.get("object") == "account")


async def _verify_configuration(api):
    global _PROOF_UNTIL
    _PROOF_UNTIL = 0.0
    await _verify_account(api)
    price = await api.request("GET", "/v1/prices/" + settings.invitation_stripe_price_id)
    _require(price.get("object") == "price" and price.get("id") == settings.invitation_stripe_price_id
             and price.get("active") is True and price.get("type") == "one_time"
             and price.get("recurring") is None and price.get("currency") == "usd"
             and type(price.get("unit_amount")) is int and price["unit_amount"] == AMOUNT
             and price.get("livemode") is settings.invitation_stripe_live_mode)
    _PROOF_UNTIL = time.monotonic() + 300


def _order_params(row):
    return {"mode": "payment", "line_items[0][price]": row["price_id"], "line_items[0][quantity]": "1",
            "payment_method_types[0]": "card", "client_reference_id": row["id"],
            "metadata[purpose]": PURPOSE, "metadata[order_id]": row["id"],
            "payment_intent_data[metadata][purpose]": PURPOSE,
            "payment_intent_data[metadata][order_id]": row["id"],
            "success_url": row["return_origin"] + "/invitations",
            "cancel_url": row["return_origin"] + "/invitations",
            "expires_at": str(row["expires_at"]), "allow_promotion_codes": "false",
            "automatic_tax[enabled]": "false", "adaptive_pricing[enabled]": "false"}


def _metadata(value, order_id):
    return value.get("metadata") == {"purpose": PURPOSE, "order_id": order_id}


def _object(value):
    _require(type(value) is dict)
    return value


def _money(value, expected):
    return type(value) is int and value == expected


def _session(session, row):
    _require(session.get("object") == "checkout.session" and _identifier(session.get("id"), "cs_")
             and session.get("livemode") is bool(row["live_mode"])
             and session.get("mode") == "payment" and session.get("client_reference_id") == row["id"]
             and _metadata(session, row["id"]) and session.get("currency") == "usd"
             and _money(session.get("amount_total"), AMOUNT)
             and _money(session.get("amount_subtotal"), AMOUNT)
             and type(session.get("expires_at")) is int and session["expires_at"] == row["expires_at"]
             and session.get("payment_method_types") == ["card"]
             and session.get("allow_promotion_codes") is False
             and _object(session.get("automatic_tax")).get("enabled") is False
             and _object(session.get("adaptive_pricing")).get("enabled") is False
             and not session.get("shipping_cost") and not session.get("shipping_options")
             and set(_object(session.get("total_details"))) == {"amount_discount", "amount_shipping", "amount_tax"}
             and all(_money(session["total_details"][key], 0) for key in session["total_details"]))
    if row["session_id"] is not None:
        _require(row["session_id"] == session["id"])


def _checkout_url(session):
    value = session.get("url")
    _require(isinstance(value, str) and len(value) <= 4096 and value.isascii() and not any(c.isspace() for c in value))
    url = urlsplit(value)
    _require(url.scheme == "https" and url.netloc == "checkout.stripe.com"
             and url.path == "/c/pay/" + session["id"] and not url.query and not url.username)
    return value


@router.post("/api/account/invitations/checkout")
async def create_checkout(request: Request, identity=Depends(require_human_sso)):
    enabled()
    invites._public(request)
    body = await invites._body(request, {"expectedAccountId", "requestId"})
    uid = invites._account(identity, body.get("expectedAccountId"))
    try:
        request_id = body.get("requestId")
        if not isinstance(request_id, str) or str(uuid.UUID(request_id)) != request_id:
            raise ValueError()
    except (ValueError, TypeError, AttributeError):
        raise HTTPException(422, "Invalid payment request") from None
    invites._rate(("checkout", uid), 10)
    await get_db()
    async with private_write() as db:
        if not await invites._active(db, uid):
            raise HTTPException(403, "Approved membership required")
        if uid in invites._owners():
            raise HTTPException(409, "Unlimited invitation allowance does not require payment")
        # Prepared means no Session POST was dispatched. Unlike unknown, these
        # rows can expire locally without risking a second charge.
        await db.execute("UPDATE invitation_checkout_orders SET state='expired',updated_at=? WHERE user_id=? AND state='prepared' AND expires_at<=?", (time.time(), uid, time.time() + 1800))
        rows = await db.execute_fetchall("SELECT * FROM invitation_checkout_orders WHERE user_id=? AND request_id=?", (uid, request_id))
        if rows:
            row = dict(rows[0])
        else:
            unresolved = await db.execute_fetchall("SELECT id FROM invitation_checkout_orders WHERE user_id=? AND state IN ('prepared','unknown','open','manual_hold') LIMIT 1", (uid,))
            if unresolved:
                raise HTTPException(409, "An invitation payment is still pending")
            now, order_id = time.time(), secrets.token_hex(16)
            await db.execute("""INSERT INTO invitation_checkout_orders VALUES
                (?,?,?,?,?,?,'usd',?,?,?,?,?,?,'prepared',NULL,NULL,NULL,?)""",
                (order_id, uid, request_id, settings.invitation_stripe_account_id,
                 settings.invitation_stripe_price_id, AMOUNT, int(settings.invitation_stripe_live_mode), API_VERSION,
                 settings.private_indexer_url, now, int(now) + 3600, PURPOSE + ":" + order_id, now))
            row = dict((await db.execute_fetchall("SELECT * FROM invitation_checkout_orders WHERE id=?", (order_id,)))[0])
    if row["state"] not in {"prepared", "unknown", "open"}:
        return {"accountId": uid, "requestId": request_id, "orderId": row["id"], "status": row["state"], "checkoutUrl": None}
    # A provider may delete idempotency entries after 24 hours. Never send the
    # same key beyond a conservative 23 hours, and never silently replace it.
    if time.time() - row["created_at"] >= 23 * 3600:
        raise HTTPException(409, "Payment outcome requires reconciliation")
    try:
        async with provider() as api:
            await _verify_configuration(api)
            async with private_write(deadline=time.monotonic() + 2) as db:
                enabled()
                if not await invites._active(db, uid) or uid in invites._owners():
                    raise HTTPException(403, "Payment authorization changed")
                current = await _order(db, row["id"])
                _require(current["state"] in {"prepared", "unknown", "open"})
                await db.execute("UPDATE invitation_checkout_orders SET state='unknown',updated_at=? WHERE id=? AND state IN ('prepared','unknown','open')", (time.time(), row["id"]))
            # The committed unknown state survives cancellation or lost response.
            session = await api.request("POST", "/v1/checkout/sessions", params=_order_params(row), idempotency=row["idempotency_key"])
            _session(session, row)
            _require(session.get("status") == "open" and session.get("payment_status") == "unpaid")
            url = _checkout_url(session)
            async with private_write(deadline=time.monotonic() + 2) as db:
                current = (await db.execute_fetchall("SELECT * FROM invitation_checkout_orders WHERE id=?", (row["id"],)))[0]
                _require(current["state"] in {"prepared", "unknown", "open"}
                         and current["session_id"] in {None, session["id"]})
                await db.execute("UPDATE invitation_checkout_orders SET state='open',session_id=?,checkout_url=?,updated_at=? WHERE id=?", (session["id"], url, time.time(), row["id"]))
        return {"accountId": uid, "requestId": request_id, "orderId": row["id"], "status": "open", "checkoutUrl": url}
    except (PaymentUnavailable, httpx.HTTPError, TimeoutError, aiosqlite.Error):
        # Do not pretend the charge failed, or expose its provider response.
        return {"accountId": uid, "requestId": request_id, "orderId": row["id"], "status": "pending", "checkoutUrl": None}


@router.get("/api/account/invitations/orders")
async def list_orders(request: Request, identity=Depends(require_human_sso)):
    enabled()
    invites._public(request)
    uid = identity["id"]
    invites._rate(("orders", uid), 60)
    db = await get_db()
    if not await invites._active(db, uid):
        raise HTTPException(403, "Approved membership required")
    rows = await db.execute_fetchall("SELECT id,request_id,state,created_at FROM invitation_checkout_orders WHERE user_id=? ORDER BY created_at DESC,id DESC LIMIT 20", (uid,))
    return {"schemaVersion": 1, "accountId": uid, "orders": [{"orderId": r["id"], "requestId": r["request_id"], "status": r["state"], "createdAt": invites._stamp(r["created_at"]),
        "retryAllowed": r["state"] in {"prepared", "unknown", "open"} and time.time() - r["created_at"] < 23 * 3600 and uid not in invites._owners()} for r in rows]}


def check_webhook_ingress(request):
    enabled()
    invites._public(request)
    if request.method != "POST" or request.scope.get("raw_path", b"") != WEBHOOK.encode() or request.url.query:
        raise HTTPException(404, "Not found")
    if len(request.headers.getlist("content-type")) != 1 or len(request.headers.getlist("content-encoding")) > 1 or len(request.headers.getlist("content-length")) > 1:
        raise HTTPException(400, "Invalid payment receipt")
    if request.headers.get("content-type", "").split(";", 1)[0].strip().lower() != "application/json":
        raise HTTPException(415, "JSON required")
    if request.headers.get("content-encoding", "identity") != "identity" or any(request.headers.get(x) for x in ("origin", "cookie", "authorization")):
        raise HTTPException(400, "Invalid payment receipt")
    if len(request.headers.getlist("stripe-signature")) != 1:
        raise HTTPException(400, "Invalid payment receipt")


def _signature(raw, header):
    _require(isinstance(header, str) and len(header) <= 1024)
    parts = header.split(",")
    _require(len(parts) <= 16)
    stamps, signatures = [], []
    for part in parts:
        key, separator, value = part.partition("=")
        _require(separator and key in {"t", "v1", "v0"})
        if key == "t":
            stamps.append(value)
        elif key == "v1":
            _require(re.fullmatch(r"[0-9a-f]{64}", value))
            signatures.append(value)
    _require(len(stamps) == 1 and re.fullmatch(r"0|[1-9][0-9]{0,11}", stamps[0])
             and 1 <= len(signatures) <= 8 and abs(time.time() - int(stamps[0])) <= 300)
    expected = hmac.new(_WEBHOOK_KEY, stamps[0].encode() + b"." + raw, hashlib.sha256).hexdigest()
    _require(any(hmac.compare_digest(expected, value) for value in signatures))


@router.post(WEBHOOK)
async def stripe_webhook(request: Request):
    check_webhook_ingress(request)
    invites._rate(("stripe-webhook", request.client.host if request.client else "unknown"), 120)
    try:
        async with asyncio.timeout(10):
            raw = bytearray()
            async for chunk in request.stream():
                _require(len(raw) + len(chunk) <= BODY_LIMIT)
                raw.extend(chunk)
            _signature(bytes(raw), request.headers["stripe-signature"])
            event = _json(raw)
            _require(event.get("object") == "event" and _identifier(event.get("id"), "evt_")
                     and event.get("livemode") is settings.invitation_stripe_live_mode
                     and event.get("api_version") == API_VERSION and event.get("account") is None)
            kind = event.get("type")
            _require(isinstance(kind, str) and len(kind) <= 100)
            obj = _object(_object(event.get("data")).get("object"))
            _require(isinstance(obj.get("id"), str) and _ID.fullmatch(obj["id"]))
            if kind in _EVENT_TYPES:
                prefix = "cs_" if kind.startswith("checkout.") else "ch_" if kind == "charge.refunded" else "dp_"
                _require(_identifier(obj["id"], prefix))
            digest = hashlib.sha256(raw).hexdigest()
            await get_db()
            async with private_write(deadline=time.monotonic() + 2) as db:
                prior = await db.execute_fetchall("SELECT body_hash FROM invitation_stripe_events WHERE id=?", (event["id"],))
                if prior:
                    _require(prior[0]["body_hash"] == digest)
                else:
                    queued = (await db.execute_fetchall("SELECT COUNT(*) AS n FROM invitation_stripe_events WHERE state='pending'"))[0]["n"]
                    _require(queued < 1000)
                    await db.execute("INSERT INTO invitation_stripe_events VALUES (?,?,?,?,?,?,0,0)",
                                     (event["id"], kind, obj["id"], digest, "pending" if kind in _EVENT_TYPES else "ignored", time.time()))
        return {"received": True}
    except (PaymentUnavailable, TimeoutError, aiosqlite.Error):
        raise HTTPException(400, "Invalid payment receipt") from None


async def _order(db, order_id):
    if not isinstance(order_id, str) or not _ORDER.fullmatch(order_id):
        raise PaymentManualHold()
    rows = await db.execute_fetchall("SELECT * FROM invitation_checkout_orders WHERE id=?", (order_id,))
    if len(rows) != 1:
        raise PaymentManualHold()
    row = dict(rows[0])
    _require(row["account_id"] == settings.invitation_stripe_account_id
             and row["price_id"] == settings.invitation_stripe_price_id and row["amount"] == AMOUNT
             and row["currency"] == "usd" and bool(row["live_mode"]) is settings.invitation_stripe_live_mode
             and row["api_version"] == API_VERSION and row["return_origin"] == settings.private_indexer_url)
    return row


def _purpose_order(value):
    metadata = value.get("metadata")
    if metadata is None or (type(metadata) is dict and metadata.get("purpose") != PURPOSE):
        raise UnrelatedPayment()
    if type(metadata) is not dict or not isinstance(metadata.get("order_id"), str) or not _ORDER.fullmatch(metadata["order_id"]):
        raise PaymentManualHold()
    return metadata["order_id"]


def _intent(pi, row, payment_id):
    _require(pi.get("object") == "payment_intent" and pi.get("id") == payment_id
             and _identifier(payment_id, "pi_") and pi.get("livemode") is bool(row["live_mode"])
             and _metadata(pi, row["id"]) and pi.get("currency") == "usd"
             and _money(pi.get("amount"), AMOUNT)
             and pi.get("status") == "succeeded" and _money(pi.get("amount_received"), AMOUNT))


def _charge(charge, row, payment_id):
    _require(charge.get("object") == "charge" and _identifier(charge.get("id"), "ch_")
             and charge.get("payment_intent") == payment_id and charge.get("paid") is True
             and charge.get("status") == "succeeded" and charge.get("currency") == "usd"
             and type(charge.get("amount")) is int and charge["amount"] == AMOUNT
             and charge.get("livemode") is bool(row["live_mode"])
             and type(charge.get("amount_refunded")) is int and 0 <= charge["amount_refunded"] <= AMOUNT
             and type(charge.get("disputed")) is bool)


async def _invalidate(db, row, payment_id, reason):
    await db.execute("INSERT OR IGNORE INTO invitation_payment_tombstones VALUES (?,?,?)", (payment_id, reason, time.time()))
    await db.execute("""UPDATE membership_invitations SET revoked_at=COALESCE(revoked_at,?)
        WHERE redeemed_at IS NULL AND id IN
        (SELECT invitation_id FROM invitation_credit_uses WHERE payment_id=?)""", (time.time(), payment_id))
    await db.execute("UPDATE invitation_checkout_orders SET state='invalidated',payment_id=COALESCE(payment_id,?),checkout_url=NULL,updated_at=? WHERE id=?", (payment_id, time.time(), row["id"]))


async def _credit(db, row, payment_id, event_id):
    existing = await db.execute_fetchall("SELECT * FROM invitation_payments WHERE payment_id=? OR event_id=?", (payment_id, event_id))
    if existing:
        _require(len(existing) == 1 and existing[0]["payment_id"] == payment_id
                 and existing[0]["user_id"] == row["user_id"] and existing[0]["amount"] == AMOUNT
                 and existing[0]["currency"] == "USD")
    else:
        await db.execute("INSERT INTO invitation_payments VALUES (?,?,?,?,?,?)", (payment_id, event_id, row["user_id"], AMOUNT, "USD", time.time()))
    prior = await db.execute_fetchall("SELECT payment_id FROM invitation_payment_events WHERE event_id=?", (event_id,))
    _require(not prior or prior[0]["payment_id"] == payment_id)
    await db.execute("INSERT OR IGNORE INTO invitation_payment_events VALUES (?,?)", (event_id, payment_id))


async def reconcile_event(event_id):
    """Verify one persisted signed receipt; all remote work precedes short writes."""
    enabled()
    await get_db()
    db = await get_db()
    events = await db.execute_fetchall("SELECT * FROM invitation_stripe_events WHERE id=? AND state='pending'", (event_id,))
    if not events:
        return
    event = dict(events[0])
    owned = False
    row = None
    try:
        async with provider() as api:
            await _verify_account(api)
            session = None
            reason = None
            if event["event_type"].startswith("checkout."):
                session = await api.request("GET", "/v1/checkout/sessions/" + event["object_id"])
                _require(session.get("object") == "checkout.session" and session.get("id") == event["object_id"]
                         and session.get("livemode") is settings.invitation_stripe_live_mode)
                order_id = _purpose_order(session)
                owned = True
                row = await _order(db, order_id)
                _session(session, row)
                _require(session["id"] == event["object_id"] and session.get("status") == "complete" and session.get("payment_status") == "paid")
                lines = await api.request("GET", "/v1/checkout/sessions/" + session["id"] + "/line_items", params={"limit": "2"})
                _require(lines.get("object") == "list" and lines.get("has_more") is False and type(lines.get("data")) is list and len(lines["data"]) == 1)
                item = lines["data"][0]
                _require(type(item) is dict and type(item.get("quantity")) is int and item["quantity"] == 1
                         and _object(item.get("price")).get("id") == row["price_id"]
                         and _money(item.get("amount_total"), AMOUNT) and _money(item.get("amount_subtotal"), AMOUNT)
                         and _money(item.get("amount_discount"), 0) and _money(item.get("amount_tax"), 0)
                         and item.get("currency") == "usd")
                payment_id = session.get("payment_intent")
                _require(_identifier(payment_id, "pi_"))
                pi = await api.request("GET", "/v1/payment_intents/" + payment_id, params={"expand[]": "latest_charge"})
                _intent(pi, row, payment_id)
                charge = pi.get("latest_charge")
                _require(type(charge) is dict)
                _charge(charge, row, payment_id)
                if charge["amount_refunded"] > 0 or charge["disputed"]:
                    reason = "refund" if charge["amount_refunded"] > 0 else "dispute"
            else:
                if event["event_type"] == "charge.refunded":
                    charge = await api.request("GET", "/v1/charges/" + event["object_id"])
                    _require(charge.get("id") == event["object_id"] and type(charge.get("amount_refunded")) is int and charge["amount_refunded"] > 0)
                    reason = "refund"
                else:
                    dispute = await api.request("GET", "/v1/disputes/" + event["object_id"])
                    _require(dispute.get("object") == "dispute" and dispute.get("id") == event["object_id"]
                             and dispute.get("livemode") is settings.invitation_stripe_live_mode and _identifier(dispute.get("charge"), "ch_"))
                    charge = await api.request("GET", "/v1/charges/" + dispute["charge"])
                    _require(charge.get("id") == dispute["charge"])
                    reason = "dispute"
                payment_id = charge.get("payment_intent")
                _require(_identifier(payment_id, "pi_"))
                pi = await api.request("GET", "/v1/payment_intents/" + payment_id)
                _require(pi.get("object") == "payment_intent" and pi.get("id") == payment_id
                         and pi.get("livemode") is settings.invitation_stripe_live_mode)
                order_id = _purpose_order(pi)
                owned = True
                row = await _order(db, order_id)
                _intent(pi, row, payment_id)
                _charge(charge, row, payment_id)
            async with private_write(deadline=time.monotonic() + 2) as write:
                enabled()
                current = await _order(write, row["id"])
                _require(current["payment_id"] in {None, payment_id})
                if session is not None:
                    _require(current["session_id"] in {None, session["id"]})
                    await write.execute("UPDATE invitation_checkout_orders SET session_id=? WHERE id=?", (session["id"], row["id"]))
                tombstones = await write.execute_fetchall("SELECT payment_id FROM invitation_payment_tombstones WHERE payment_id=?", (payment_id,))
                if reason is not None:
                    await _invalidate(write, current, payment_id, reason)
                elif tombstones or current["state"] == "invalidated":
                    await _invalidate(write, current, payment_id, "prior_invalidation")
                else:
                    await _credit(write, current, payment_id, event_id)
                    await write.execute("UPDATE invitation_checkout_orders SET state='credited',payment_id=?,checkout_url=NULL,updated_at=? WHERE id=?", (payment_id, time.time(), row["id"]))
                await write.execute("UPDATE invitation_stripe_events SET state='done' WHERE id=?", (event_id,))
    except UnrelatedPayment:
        async with private_write(deadline=time.monotonic() + 2) as write:
            await write.execute("UPDATE invitation_stripe_events SET state='ignored' WHERE id=? AND state='pending'", (event_id,))
    except PaymentManualHold:
        async with private_write(deadline=time.monotonic() + 2) as write:
            await write.execute("UPDATE invitation_stripe_events SET state='manual_hold' WHERE id=? AND state='pending'", (event_id,))
            if row is not None:
                await write.execute("UPDATE invitation_checkout_orders SET state='manual_hold',checkout_url=NULL,updated_at=? WHERE id=? AND state IN ('prepared','unknown','open')", (time.time(), row["id"]))
    except PaymentUnavailable:
        async with private_write(deadline=time.monotonic() + 2) as write:
            if owned:
                await write.execute("UPDATE invitation_stripe_events SET state='manual_hold' WHERE id=? AND state='pending'", (event_id,))
                if row is not None:
                    await write.execute("UPDATE invitation_checkout_orders SET state='manual_hold',checkout_url=NULL,updated_at=? WHERE id=? AND state IN ('prepared','unknown','open')", (time.time(), row["id"]))
            else:
                await write.execute("UPDATE invitation_stripe_events SET attempts=attempts+1,next_attempt=? WHERE id=? AND state='pending'", (time.time() + 60, event_id))
    except (httpx.HTTPError, TimeoutError, aiosqlite.Error):
        async with private_write(deadline=time.monotonic() + 2) as write:
            await write.execute("UPDATE invitation_stripe_events SET attempts=attempts+1,next_attempt=? WHERE id=? AND state='pending'", (time.time() + 60, event_id))


async def process_pending():
    enabled()
    db = await get_db()
    rows = await db.execute_fetchall("SELECT id FROM invitation_stripe_events WHERE state='pending' AND next_attempt<=? ORDER BY next_attempt,received_at,id LIMIT 100", (time.time(),))
    queue = asyncio.Queue()
    for row in rows:
        queue.put_nowait(row["id"])
    async def work():
        while not queue.empty():
            await reconcile_event(queue.get_nowait())
    # Four workers consume at most 100 fixed rows. Retried transient failures
    # move behind never-attempted rows; ignored/held events leave this queue.
    async with asyncio.TaskGroup() as group:
        for _ in range(4):
            group.create_task(work())


async def expire_orders():
    """Close only provider-confirmed expired/unpaid sessions, never grant credit."""
    enabled()
    db = await get_db()
    rows = await db.execute_fetchall("SELECT id,session_id FROM invitation_checkout_orders WHERE state='open' AND expires_at<=? ORDER BY expires_at,id LIMIT 10", (time.time(),))
    for item in rows:
        try:
            async with provider() as api:
                await _verify_account(api)
                row = await _order(db, item["id"])
                _require(_identifier(row["session_id"], "cs_"))
                session = await api.request("GET", "/v1/checkout/sessions/" + row["session_id"])
                _session(session, row)
                _require(session.get("status") == "expired" and session.get("payment_status") == "unpaid")
                async with private_write(deadline=time.monotonic() + 2) as write:
                    await write.execute("UPDATE invitation_checkout_orders SET state='expired',checkout_url=NULL,updated_at=? WHERE id=? AND state='open' AND session_id=?", (time.time(), row["id"], row["session_id"]))
        except (PaymentUnavailable, httpx.HTTPError, TimeoutError, aiosqlite.Error):
            pass


def start_worker():
    global _TASK
    if not settings.private_invitation_checkout_enabled:
        return
    async def loop():
        while True:
            try:
                async with provider() as api:
                    if time.monotonic() >= _PROOF_UNTIL:
                        await _verify_configuration(api)
                await process_pending()
                await expire_orders()
            except Exception:
                # No provider payloads or credential-bearing exception text.
                pass
            await asyncio.sleep(30)
    _TASK = asyncio.create_task(loop())


async def stop_worker():
    global _TASK, _PROOF_UNTIL
    _PROOF_UNTIL = 0.0
    task, _TASK = _TASK, None
    if task is not None:
        task.cancel()
        try:
            await task
        except asyncio.CancelledError:
            pass

"""Explicit owner-only invitation issuer admission, separate from private APIs."""
import asyncio
import json
import sqlite3
import time
from urllib.parse import urlsplit

from fastapi import APIRouter, Depends, HTTPException, Request

from auth import PROXY_PROOF_HEADER, require_human_sso
from config import settings
from database import get_db
from deps import _configured_hosts, _host_scope, _normalise_hostname, require_operator
from memberships import write_membership
from private_indexer import private_write

router = APIRouter()
PATH = "/api/registration/issuer"
PAGE = "/registration-admin"
BODY_LIMIT = 2048
DEADLINE = 5
IDENTITY_HEADERS = ("x-auth-user-id", "x-auth-user", "x-forwarded-user", "remote-user")


def operator_origin():
    raw = settings.operator_ui_url
    try:
        parts = urlsplit(raw)
        host = _normalise_hostname(parts.netloc)
        valid = (
            isinstance(raw, str) and host is not None and parts.scheme == "https"
            and raw == f"https://{host}" and parts.netloc == host
            and host in _configured_hosts(settings.operator_hosts, "OPERATOR_HOSTS")
        )
    except (TypeError, ValueError):
        valid = False
    if not valid:
        raise RuntimeError("Issuer admission requires an exact HTTPS OPERATOR_UI_URL origin")
    return raw


def validate_settings():
    if not settings.site_registration_issuer_admission_enabled:
        return
    if (not settings.site_registration_enabled or not settings.private_invitations_enabled
            or not settings.private_invitation_bridge_enabled or settings.private_indexer_enabled
            or settings.private_invitation_checkout_enabled or not settings.require_auth
            or not (settings.trust_npm_headers or settings.trust_forwarded_user)
            or "OWNER" not in {value.strip().upper() for value in settings.operator_roles.split(",")}):
        raise RuntimeError("Issuer admission requires owner-authenticated site registration with private indexing off")
    operator_origin()


def enabled():
    if not settings.site_registration_issuer_admission_enabled:
        raise HTTPException(404, "Not found")
    try:
        validate_settings()
    except RuntimeError:
        raise HTTPException(404, "Not found") from None


def require_owner(request: Request):
    enabled()
    scope = _host_scope(request)
    if scope == "public":
        raise HTTPException(404, "Not found")
    if scope != "operator":
        raise HTTPException(421, "Misdirected request")
    if (request.headers.getlist("host") != [urlsplit(operator_origin()).netloc]
            or request.scope.get("query_string")):
        raise HTTPException(404, "Not found")
    # The ordinary proxy resolver accepts aliases for compatibility. This narrow
    # mutation refuses duplicate or disagreeing claims rather than selecting one.
    for name in (*IDENTITY_HEADERS, "x-auth-priv", "x-auth-provider", PROXY_PROOF_HEADER):
        values = request.headers.getlist(name)
        if len(values) > 1:
            raise HTTPException(403, "Verified owner required")
    if (len(request.headers.getlist(PROXY_PROOF_HEADER)) != 1
            or request.headers.getlist("x-auth-priv") != ["OWNER"]):
        raise HTTPException(403, "Verified owner required")
    operator = require_operator(request)
    human = require_human_sso(request)
    if (not operator.get("operator") or operator.get("priv") != "OWNER"
            or human["id"] != operator["id"]
            or any(request.headers.get(name) != human["id"]
                   for name in IDENTITY_HEADERS if request.headers.get(name) is not None)):
        raise HTTPException(403, "Verified owner required")
    return human


def check_mutation(request):
    if (len(request.headers.getlist("content-type")) != 1
            or request.headers["content-type"].split(";", 1)[0].strip().lower() != "application/json"):
        raise HTTPException(415, "JSON required")
    if (request.headers.getlist("origin") != [operator_origin()]
            or request.headers.getlist("sec-fetch-site") not in ([], ["same-origin"])):
        raise HTTPException(403, "Same-origin request required")


def _object(pairs):
    value = {}
    for key, item in pairs:
        if key in value:
            raise ValueError("duplicate field")
        value[key] = item
    return value


async def _body(request):
    check_mutation(request)
    raw = bytearray()
    try:
        async with asyncio.timeout(DEADLINE):
            async for chunk in request.stream():
                if len(raw) + len(chunk) > BODY_LIMIT:
                    raise HTTPException(413, "Issuer request too large")
                raw.extend(chunk)
    except TimeoutError:
        raise HTTPException(408, "Issuer request deadline exceeded") from None
    try:
        value = json.loads(raw, object_pairs_hook=_object)
    except (UnicodeDecodeError, TypeError, ValueError):
        raise HTTPException(422, "Invalid issuer request") from None
    if not isinstance(value, dict) or set(value) != {"expectedAccountId"}:
        raise HTTPException(422, "Invalid issuer request")
    return value


def _state(user_id, row):
    return {"accountId": user_id, "active": bool(row and row["active"] == 1),
            "suspended": bool(row and row["active"] != 1)}


@router.get(PATH)
async def issuer_state(identity=Depends(require_owner)):
    db = await get_db()
    rows = await db.execute_fetchall("SELECT active FROM private_members WHERE user_id=?", (identity["id"],))
    return _state(identity["id"], rows[0] if rows else None)


@router.put(PATH)
async def admit_issuer(request: Request, identity=Depends(require_owner)):
    body = await _body(request)
    if not isinstance(body["expectedAccountId"], str) or body["expectedAccountId"] != identity["id"]:
        raise HTTPException(403, "Account changed; reload before continuing")
    await get_db()
    deadline = time.monotonic() + DEADLINE
    try:
        async with private_write(deadline=deadline) as db:
            # Recheck the immutable request identity and current feature boundary
            # after waiting for the writer. Never manufacture a target or actor.
            current = require_owner(request)
            if current["id"] != identity["id"]:
                raise HTTPException(403, "Account changed; reload before continuing")
            rows = await db.execute_fetchall("SELECT active FROM private_members WHERE user_id=?", (current["id"],))
            if rows and rows[0]["active"] != 1:
                raise HTTPException(409, "Suspended membership requires separate operator review")
            if not rows:
                await write_membership(db, current["id"], True, current["id"], time.time())
    except (TimeoutError, sqlite3.OperationalError):
        raise HTTPException(503, "Issuer admission unavailable; reload to check status") from None
    return {"accountId": identity["id"], "active": True, "suspended": False}

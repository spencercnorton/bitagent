from __future__ import annotations
import hmac
import ipaddress
from functools import lru_cache
from fastapi import Request, HTTPException
from config import settings

PROXY_PROOF_HEADER = "x-bitagent-proxy-proof"

# Identity dicts always carry the same shape so templates / /api/me can read a
# field without guarding for its absence. The proxy tiers fill these from the
# gate's X-Auth-* headers; the api-key and open tiers leave them blank.
_IDENTITY_EXTRA = {
    "username": "", "email": "", "provider": "", "priv": "", "operator": False,
}


def _identity(id: str, method: str, display: str, **extra) -> dict:
    return {"id": id, "method": method, "display": display, **_IDENTITY_EXTRA, **extra}


def _operator_roles() -> frozenset[str]:
    return frozenset(
        role.strip().upper()
        for role in (settings.operator_roles or "").split(",")
        if role.strip()
    )


@lru_cache(maxsize=16)
def _parse_proxy_networks(raw: str) -> tuple[ipaddress.IPv4Network | ipaddress.IPv6Network, ...]:
    networks = []
    for value in raw.split(","):
        value = value.strip()
        if value:
            networks.append(ipaddress.ip_network(value, strict=False))
    return tuple(networks)


def _trusted_proxy_networks():
    return _parse_proxy_networks(settings.trusted_proxy_cidrs or "")


def proxy_provenance_valid(request: Request) -> bool:
    """Verify the transport peer, then the mandatory proxy-shared proof.

    This intentionally uses ``request.client.host`` rather than any forwarding
    header. The container starts uvicorn with proxy-header rewriting disabled so
    that value remains the TCP peer and cannot be replaced by X-Forwarded-For.
    """
    peer = request.client.host if request.client else ""
    try:
        peer_ip = ipaddress.ip_address(peer)
        source_ok = any(peer_ip in network for network in _trusted_proxy_networks())
    except ValueError:
        source_ok = False
    if not source_ok:
        return False

    expected = settings.proxy_auth_secret or ""
    if not expected:
        return False
    presented = request.headers.get(PROXY_PROOF_HEADER) or ""
    return hmac.compare_digest(
        presented.encode("utf-8"),
        expected.encode("utf-8"),
    )


def validate_auth_settings() -> None:
    """Fail startup when proxy/role configuration would be unsafe or inert."""
    try:
        networks = _trusted_proxy_networks()
    except ValueError as exc:
        raise RuntimeError("TRUSTED_PROXY_CIDRS contains an invalid network") from exc

    proxy_headers_enabled = settings.trust_npm_headers or settings.trust_forwarded_user
    if proxy_headers_enabled and not networks:
        raise RuntimeError(
            "TRUSTED_PROXY_CIDRS is required when proxy identity trust is enabled"
        )
    if proxy_headers_enabled and not settings.proxy_auth_secret:
        raise RuntimeError(
            "PROXY_AUTH_SECRET is required when proxy identity trust is enabled"
        )
    if proxy_headers_enabled and len(settings.proxy_auth_secret.encode("utf-8")) < 32:
        raise RuntimeError("PROXY_AUTH_SECRET must be at least 32 bytes")
    if proxy_headers_enabled and any(network.prefixlen == 0 for network in networks):
        raise RuntimeError("TRUSTED_PROXY_CIDRS must not trust every address")
    if not _operator_roles():
        raise RuntimeError("OPERATOR_ROLES must contain at least one explicit role")


def _proxy_identity(request: Request, uid: str, method: str) -> dict:
    """Build a rich identity from the auth-portal gate's X-Auth-* headers.

    The auth *decision* has already been made by the caller — a trusted primary
    header (X-Forwarded-User / X-Auth-User-Id) was present under an enabled
    trust flag. These sibling headers are injected by the SAME upstream gate on
    the SAME request, so reading them adds no new trust surface; they only
    enrich what we display (the primary header is just the numeric telegram id).

    nginx's ``proxy_set_header`` silently drops empty-value headers, so any of
    these may be absent even behind the gate — hence the display fallback chain
    (display-name → username → uid).
    """
    h = request.headers
    username = h.get("x-auth-username") or ""
    display_name = h.get("x-auth-display-name") or ""
    # The gate sends "UNASSIGNED" for cookies minted before the priv claim
    # landed; treat that (and empty) as "no tier" so the UI shows no badge.
    priv = (h.get("x-auth-priv") or "").strip().upper()
    if priv == "UNASSIGNED":
        priv = ""
    return _identity(
        uid, method, display_name or username or uid,
        username=username,
        email=h.get("x-auth-email") or "",
        provider=h.get("x-auth-provider") or "",
        priv=priv,
        operator=priv in _operator_roles(),
    )


def resolve_identity(request: Request) -> dict:
    if not settings.require_auth:
        # REQUIRE_AUTH=false is an explicit local-development bypass and must
        # never be enabled in production. It grants the operator surface so the
        # dashboard remains usable in hermetic/local development.
        return _identity("anonymous", "open", "Anonymous", operator=True)

    api_key = (
        request.query_params.get("apikey")
        or request.headers.get("x-api-key")
        or request.headers.get("authorization", "").removeprefix("Bearer ").strip()
    )
    # Tier 1 — API key. When a dashboard key is configured, a SUPPLIED key must
    # match: a wrong key is a hard 401 with NO fall-through to the proxy tiers
    # (otherwise an attacker could pair a bogus key with a forged X-Auth-User to
    # slip past). A request with NO key falls through so the proxy/SSO path works.
    if settings.dashboard_api_key:
        if api_key:
            if hmac.compare_digest(api_key, settings.dashboard_api_key):
                # The dashboard key is an explicit operator credential.
                return _identity("api-client", "api-key", "API Client", operator=True)
            raise HTTPException(status_code=401, detail="Invalid API key")

    # Tier 2 — NPM auth_request header (only honored when explicitly trusted).
    # The forward-auth gate injects the telegram user id as X-Auth-User-Id;
    # X-Auth-User is accepted as a legacy alias.
    if settings.trust_npm_headers:
        npm_user = request.headers.get("x-auth-user-id") or request.headers.get("x-auth-user")
        if npm_user:
            if not proxy_provenance_valid(request):
                raise HTTPException(status_code=401, detail="Unverified proxy identity")
            return _proxy_identity(request, npm_user, "npm-header")

    # Tier 3 — generic forwarded-user (only honored when explicitly trusted).
    if settings.trust_forwarded_user:
        fwd_user = request.headers.get("x-forwarded-user") or request.headers.get("remote-user")
        if fwd_user:
            if not proxy_provenance_valid(request):
                raise HTTPException(status_code=401, detail="Unverified proxy identity")
            return _proxy_identity(request, fwd_user, "forwarded-user")

    # NOTE: there is intentionally no in-app "SSO cookie present" tier. The
    # ge_sso/bitagent_session cookie is an opaque token minted by the
    # telegram-auth-portal and can only be validated upstream by the NPM
    # forward-auth gate, which does so before the request reaches us and then
    # injects the identity header consumed by tier 2/3. Trusting mere cookie
    # *presence* here would let anyone able to reach the app directly (e.g. on
    # the tailnet :8081) forge a session, so we don't. SSO is enforced by the
    # gate and surfaced through the proxy tiers above.
    raise HTTPException(status_code=401, detail="Authentication required")


def require_auth(request: Request) -> dict:
    return resolve_identity(request)


def describe_active_tiers() -> dict:
    """Return which auth tiers are actually configured/active.

    Booleans only — never leaks API keys, proof values, CIDRs, or role names.
    Security trust settings are startup-only and never sourced from DB overrides.
    """
    try:
        proxy_source_configured = bool(_trusted_proxy_networks())
    except ValueError:
        proxy_source_configured = False
    proxy_boundary_configured = bool(
        proxy_source_configured and settings.proxy_auth_secret
    )
    npm_active = bool(settings.trust_npm_headers and proxy_boundary_configured)
    forwarded_active = bool(settings.trust_forwarded_user and proxy_boundary_configured)
    return {
        # Tier 1 — api-key: active when a dashboard key is configured. Not a
        # mutable field, so always read straight from settings (never echoed).
        "apiKey": bool(settings.dashboard_api_key),
        # Proxy tiers are active only when both their trust flag and a transport
        # peer allowlist and shared proof are configured.
        "npmHeaders": npm_active,
        "forwardedUser": forwarded_active,
        "proxyProof": bool(settings.proxy_auth_secret),
        # SSO: there is no in-app cookie tier (see resolve_identity). SSO is
        # validated upstream by the forward-auth gate and surfaced via a trusted
        # proxy tier — so report it active when auth is enforced AND at least one
        # proxy tier is trusted (i.e. a gate is in front doing the validation).
        "sso": bool(settings.require_auth and (npm_active or forwarded_active)),
    }

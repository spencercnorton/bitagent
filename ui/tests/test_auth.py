"""Auth resolver behaviour — the security-sensitive part of the dashboard.

These pin the security boundary: transport-peer CIDRs plus shared proxy proof,
startup validation, API-key behavior, role enrichment, cookie non-trust, and
the booleans-only `describe_active_tiers` contract.
"""
import auth
import config
import pytest
from fastapi.testclient import TestClient
import app as app_module


PROXY_SECRET = "unit-test-proxy-proof-at-least-32-bytes"
PROXY_PROOF = {"X-BitAgent-Proxy-Proof": PROXY_SECRET}


def _enable_proxy(*, npm: bool = False, forwarded: bool = False):
    config.settings.trust_npm_headers = npm
    config.settings.trust_forwarded_user = forwarded
    config.settings.trusted_proxy_cidrs = "127.0.0.0/8"
    config.settings.proxy_auth_secret = PROXY_SECRET


def test_healthz_is_open(client):
    r = client.get("/healthz")
    assert r.status_code == 200
    assert r.json()["status"] == "ok"


def test_me_requires_auth_by_default(client):
    config.settings.require_auth = True
    assert client.get("/api/me").status_code == 401


def test_require_auth_false_yields_open_user(client):
    config.settings.require_auth = False
    r = client.get("/api/me")
    assert r.status_code == 200
    body = r.json()
    assert body["method"] == "open"
    assert body["id"] == "anonymous"
    assert body["operator"] is True


def test_api_key_matches_all_three_carriers(client):
    config.settings.require_auth = True
    config.settings.dashboard_api_key = "s3cret"
    assert client.get("/api/me", params={"apikey": "s3cret"}).status_code == 200
    r = client.get("/api/me", headers={"Authorization": "Bearer s3cret"})
    assert r.status_code == 200 and r.json()["method"] == "api-key"
    assert r.json()["operator"] is True
    assert client.get("/api/me", headers={"X-Api-Key": "s3cret"}).status_code == 200


def test_npm_header_ignored_when_untrusted(client):
    # THE spoofing guard: a forged X-Auth-User must NOT authenticate while
    # trust_npm_headers is off (the default).
    config.settings.require_auth = True
    config.settings.trust_npm_headers = False
    assert client.get("/api/me", headers={"X-Auth-User": "attacker"}).status_code == 401


def test_npm_header_honored_when_trusted(client):
    config.settings.require_auth = True
    _enable_proxy(npm=True)
    r = client.get("/api/me", headers={"X-Auth-User": "alice", **PROXY_PROOF})
    assert r.status_code == 200
    body = r.json()
    assert body["id"] == "alice" and body["method"] == "npm-header"


def test_forwarded_user_ignored_when_untrusted(client):
    config.settings.require_auth = True
    config.settings.trust_forwarded_user = False
    assert client.get("/api/me", headers={"X-Forwarded-User": "attacker"}).status_code == 401


def test_forwarded_user_honored_when_trusted(client):
    config.settings.require_auth = True
    _enable_proxy(forwarded=True)
    r = client.get("/api/me", headers={"X-Forwarded-User": "bob", **PROXY_PROOF})
    assert r.status_code == 200
    assert r.json()["method"] == "forwarded-user"


def test_wrong_api_key_is_hard_401_no_fallthrough(client):
    # A configured key + a WRONG supplied key must hard-401, even when a
    # (forgeable) proxy header is also present — no silent fall-through.
    config.settings.require_auth = True
    config.settings.dashboard_api_key = "s3cret"
    _enable_proxy(npm=True)
    r = client.get(
        "/api/me", params={"apikey": "wrong"},
        headers={"X-Auth-User": "alice", **PROXY_PROOF},
    )
    assert r.status_code == 401


def test_no_api_key_supplied_still_allows_proxy_tier(client):
    # A configured key but NO key supplied should still permit the proxy tier
    # (so adding an api key doesn't lock out the NPM-gated browser flow).
    config.settings.require_auth = True
    config.settings.dashboard_api_key = "s3cret"
    _enable_proxy(npm=True)
    r = client.get("/api/me", headers={"X-Auth-User": "alice", **PROXY_PROOF})
    assert r.status_code == 200 and r.json()["method"] == "npm-header"


def test_cookie_presence_does_not_authenticate(client):
    # Removed tier-4: a bare session cookie must NOT authenticate. SSO is
    # validated upstream by the gate, never by cookie presence here.
    config.settings.require_auth = True
    config.settings.trust_npm_headers = False
    config.settings.trust_forwarded_user = False
    client.cookies.set(config.settings.sso_cookie_name, "forged-or-stale")
    r = client.get("/api/me")
    assert r.status_code == 401


def test_describe_active_tiers_booleans_only_no_key_leak():
    config.settings.dashboard_api_key = "supersecret"
    config.settings.require_auth = True
    _enable_proxy(npm=True)
    tiers = auth.describe_active_tiers()
    assert tiers == {
        "apiKey": True, "npmHeaders": True, "forwardedUser": False,
        "proxyProof": True, "sso": True,
    }
    assert "supersecret" not in str(tiers)
    assert PROXY_SECRET not in str(tiers)


def test_proxy_identity_requires_shared_proof(client):
    config.settings.require_auth = True
    _enable_proxy(forwarded=True)
    assert client.get(
        "/api/me", headers={"X-Forwarded-User": "spoofed"},
    ).status_code == 401
    assert client.get(
        "/api/me",
        headers={"X-Forwarded-User": "spoofed", "X-BitAgent-Proxy-Proof": "wrong"},
    ).status_code == 401


def test_proxy_identity_requires_trusted_transport_peer():
    config.settings.require_auth = True
    config.settings.operator_hosts = "console.example.org,testserver"
    _enable_proxy(forwarded=True)
    async def untrusted_peer(scope, receive, send):
        if scope["type"] == "http":
            scope = {**scope, "client": ("198.51.100.10", 50000)}
        await app_module.app(scope, receive, send)

    with TestClient(untrusted_peer) as untrusted:
        r = untrusted.get(
            "/api/me",
            headers={"X-Forwarded-User": "spoofed", **PROXY_PROOF},
        )
    assert r.status_code == 401


def test_startup_validation_rejects_incomplete_proxy_boundary():
    config.settings.trust_forwarded_user = True
    config.settings.trusted_proxy_cidrs = ""
    config.settings.proxy_auth_secret = ""
    with pytest.raises(RuntimeError, match="TRUSTED_PROXY_CIDRS"):
        auth.validate_auth_settings()

    config.settings.trusted_proxy_cidrs = "127.0.0.1/32"
    with pytest.raises(RuntimeError, match="PROXY_AUTH_SECRET"):
        auth.validate_auth_settings()

    config.settings.proxy_auth_secret = "too-short"
    with pytest.raises(RuntimeError, match="at least 32 bytes"):
        auth.validate_auth_settings()

    config.settings.proxy_auth_secret = PROXY_SECRET
    config.settings.trusted_proxy_cidrs = "0.0.0.0/0"
    with pytest.raises(RuntimeError, match="must not trust every address"):
        auth.validate_auth_settings()


def test_app_lifespan_fails_closed_without_proxy_proof():
    config.settings.operator_hosts = "console.example.org,testserver"
    config.settings.trust_forwarded_user = True
    config.settings.trusted_proxy_cidrs = "127.0.0.1/32"
    config.settings.proxy_auth_secret = ""
    with pytest.raises(RuntimeError, match="PROXY_AUTH_SECRET"):
        with TestClient(app_module.app):
            pass


def test_startup_validation_rejects_invalid_cidr_and_empty_roles():
    config.settings.trusted_proxy_cidrs = "not-a-network"
    with pytest.raises(RuntimeError, match="invalid network"):
        auth.validate_auth_settings()

    config.settings.trusted_proxy_cidrs = ""
    config.settings.operator_roles = ""
    with pytest.raises(RuntimeError, match="OPERATOR_ROLES"):
        auth.validate_auth_settings()


# ── Identity enrichment from the gate's X-Auth-* headers ──────────────────
# The gate sets X-Forwarded-User / X-Auth-User-Id to the (numeric) telegram id
# and ships the friendly identity in sibling headers. The resolver surfaces
# those for display without widening the trust surface (same gated request).

def test_forwarded_user_enriched_from_gate_headers(client):
    config.settings.require_auth = True
    _enable_proxy(forwarded=True)
    r = client.get("/api/me", headers={
        "X-Forwarded-User": "7000000001",
        "X-Auth-Username": "spencer",
        "X-Auth-Display-Name": "Spencer Norton",
        "X-Auth-Email": "spencer@example.com",
        "X-Auth-Provider": "telegram",
        "X-Auth-Priv": "OWNER",
        **PROXY_PROOF,
    })
    assert r.status_code == 200
    body = r.json()
    assert body["id"] == "7000000001"            # decision header = numeric id
    assert body["display"] == "Spencer Norton"  # friendly name preferred
    assert body["username"] == "spencer"
    assert body["email"] == "spencer@example.com"
    assert body["provider"] == "telegram"
    assert body["priv"] == "OWNER"
    assert body["operator"] is True
    assert body["method"] == "forwarded-user"


def test_display_falls_back_to_username_then_id(client):
    config.settings.require_auth = True
    _enable_proxy(forwarded=True)
    # nginx drops empty-value headers, so display-name can be absent.
    r = client.get(
        "/api/me",
        headers={"X-Forwarded-User": "42", "X-Auth-Username": "ada", **PROXY_PROOF},
    )
    assert r.json()["display"] == "ada"
    # username absent too → fall all the way back to the id.
    r2 = client.get("/api/me", headers={"X-Forwarded-User": "42", **PROXY_PROOF})
    assert r2.json()["display"] == "42"


def test_priv_unassigned_normalized_to_blank(client):
    config.settings.require_auth = True
    _enable_proxy(forwarded=True)
    r = client.get(
        "/api/me",
        headers={"X-Forwarded-User": "42", "X-Auth-Priv": "UNASSIGNED", **PROXY_PROOF},
    )
    assert r.json()["priv"] == ""
    assert r.json()["operator"] is False


def test_npm_tier_reads_x_auth_user_id(client):
    # The gate's NPM-style header is X-Auth-User-Id (X-Auth-User stays a legacy
    # alias, covered by test_npm_header_honored_when_trusted).
    config.settings.require_auth = True
    _enable_proxy(npm=True)
    r = client.get(
        "/api/me",
        headers={"X-Auth-User-Id": "99", "X-Auth-Display-Name": "Ada L", **PROXY_PROOF},
    )
    assert r.status_code == 200
    body = r.json()
    assert body["id"] == "99" and body["method"] == "npm-header"
    assert body["display"] == "Ada L"


def test_enrichment_headers_alone_do_not_authenticate(client):
    # The display headers carry no authority: without a trusted PRIMARY header
    # (id), a request full of X-Auth-* must still 401.
    config.settings.require_auth = True
    config.settings.trust_npm_headers = False
    config.settings.trust_forwarded_user = True
    r = client.get("/api/me", headers={
        "X-Auth-Username": "attacker", "X-Auth-Display-Name": "Totally Admin",
        "X-Auth-Priv": "OWNER",
    })
    assert r.status_code == 401

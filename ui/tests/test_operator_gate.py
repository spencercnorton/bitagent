"""Operator-only authorization gate (v1.22.0).

The public library and the operator console are the SAME FastAPI app behind
the SAME SSO gate. Before
v1.22.0 every endpoint used a flat `require_auth`, so any SSO-approved user who
could open the public library could also reach operator mutations/config. The
`require_operator` requires both the explicit operator Host and a verified
operator grant. Unknown hosts fail closed before routing.

Baseline surface tests run with `require_auth=False` to isolate Host behavior;
the role tests enable the complete CIDR + proof + X-Auth-Priv production path.
"""
from __future__ import annotations

import pytest

import config
import app as app_module

PUBLIC_HOST = {"host": "library.example.org"}
OPERATOR_HOST = {"host": "console.example.org"}
UNKNOWN_HOST = {"host": "unmapped.example.com"}
PROXY_SECRET = "unit-test-proxy-proof-at-least-32-bytes"


def _proxy_headers(priv: str, host: str = "console.example.org") -> dict[str, str]:
    return {
        "host": host,
        "X-Forwarded-User": "7000000001",
        "X-Auth-Priv": priv,
        "X-BitAgent-Proxy-Proof": PROXY_SECRET,
    }


def _enable_proxy_auth():
    config.settings.require_auth = True
    config.settings.trust_npm_headers = False
    config.settings.trust_forwarded_user = True
    config.settings.trusted_proxy_cidrs = "127.0.0.0/8"
    config.settings.proxy_auth_secret = PROXY_SECRET
    config.settings.operator_roles = "OWNER"

# Operator/admin surface — must 404 on a public-library host. All are GETs that
# short-circuit at the gate before touching upstream, so no mock is needed.
OPERATOR_GET = [
    "/api/settings",
    "/api/auth/tiers",
    "/api/settings/audit",
    "/api/stats",
    "/api/metrics",
    "/api/wants",
    "/api/quarantine",
    "/api/notifications",
    "/api/filters/status",
    "/api/block-phrases",
    "/api/evidence",
    "/api/ai/summary",
    "/api/indexer-stats",
]

# Public library + personal-account surface — must stay reachable on the public
# host (local DB only, no upstream needed).
PUBLIC_GET = ["/api/account", "/api/me"]


@pytest.fixture(autouse=True)
def _open_auth():
    config.settings.require_auth = False
    yield


@pytest.mark.parametrize("path", OPERATOR_GET)
def test_operator_endpoint_is_404_on_public_host(client, path):
    r = client.get(path, headers=PUBLIC_HOST)
    assert r.status_code == 404, f"{path} leaked on the public host ({r.status_code})"


def test_public_host_gets_library_manifest(client):
    r = client.get("/manifest.webmanifest", headers=PUBLIC_HOST)
    assert r.status_code == 200
    assert r.json()["name"] == "Library — BitAgent"


def test_operator_host_gets_console_manifest(client):
    r = client.get("/manifest.webmanifest", headers=OPERATOR_HOST)
    assert r.status_code == 200
    assert r.json()["name"] == "BitAgent Console"


@pytest.mark.parametrize("path", ["/api/settings", "/api/auth/tiers"])
def test_operator_endpoint_reachable_on_operator_host(client, path):
    # The gate must NOT fire on the operator host (these two need only local DB).
    r = client.get(path, headers=OPERATOR_HOST)
    assert r.status_code == 200, f"{path} should be reachable on the operator host"


@pytest.mark.parametrize("path", PUBLIC_GET)
def test_public_endpoint_still_reachable_on_public_host(client, path):
    r = client.get(path, headers=PUBLIC_HOST)
    assert r.status_code == 200, f"{path} should stay public ({r.status_code})"


@pytest.mark.parametrize(
    "path",
    ["/", "/api/me", "/api/settings", "/static/js/app.js", "/manifest.webmanifest"],
)
def test_unknown_host_fails_closed(client, path):
    r = client.get(path, headers=UNKNOWN_HOST)
    assert r.status_code == 421


def test_configured_hosts_are_case_and_port_normalized(client):
    assert client.get("/api/me", headers={"host": "CONSOLE.EXAMPLE.ORG:443"}).status_code == 200
    assert client.get("/api/me", headers={"host": "LIBRARY.EXAMPLE.ORG:443"}).status_code == 200


@pytest.mark.parametrize(
    "host",
    ["console.example.org", "console-alt.example.org", "localhost", "127.0.0.1"],
)
def test_v130_default_operator_hosts_keep_console_branding(client, host):
    r = client.get("/", headers={"host": host})
    assert r.status_code == 200
    assert "BitAgent Console" in r.text
    assert "library.js" not in r.text


def test_duplicate_host_header_is_rejected(client):
    r = client.get(
        "/api/me",
        headers=[("host", "console.example.org"), ("host", "attacker.invalid")],
    )
    assert r.status_code == 421


def test_host_lists_must_be_nonempty_disjoint_and_valid():
    config.settings.public_library_hosts = ""
    with pytest.raises(RuntimeError, match="PUBLIC_LIBRARY_HOSTS"):
        app_module._validate_host_settings()

    config.settings.public_library_hosts = "library.example.org"
    config.settings.operator_hosts = "library.example.org"
    with pytest.raises(RuntimeError, match="must not overlap"):
        app_module._validate_host_settings()

    config.settings.operator_hosts = "*.example.org"
    with pytest.raises(RuntimeError, match="invalid hostname"):
        app_module._validate_host_settings()


def test_owner_claim_reaches_operator_surface(client):
    _enable_proxy_auth()
    r = client.get("/api/settings", headers=_proxy_headers("owner"))
    assert r.status_code == 200


def test_non_operator_claim_is_forbidden_on_operator_host(client):
    _enable_proxy_auth()
    r = client.get("/api/settings", headers=_proxy_headers("VIEWER"))
    assert r.status_code == 403
    assert r.json()["detail"] == "Operator grant required"


def test_owner_claim_does_not_expose_operator_api_on_public_host(client):
    _enable_proxy_auth()
    r = client.get(
        "/api/settings",
        headers=_proxy_headers("OWNER", host="library.example.org"),
    )
    assert r.status_code == 404


def test_operator_shell_requires_role_but_public_shell_does_not(client):
    _enable_proxy_auth()
    assert client.get("/", headers=_proxy_headers("VIEWER")).status_code == 403
    public = client.get(
        "/", headers=_proxy_headers("VIEWER", host="library.example.org")
    )
    assert public.status_code == 200
    assert "Library — BitAgent" in public.text


def test_setting_override_mutation_is_404_on_public_host(client):
    # A media-consumer must not be able to reach the override mutation at all.
    r = client.put(
        "/api/settings/overrides/tmdb_api_key",
        json={"value": "attacker"},
        headers=PUBLIC_HOST,
    )
    assert r.status_code == 404


def test_graphql_passthrough_is_404_on_public_host(client):
    r = client.post(
        "/api/graphql",
        json={"query": "{ __typename }"},
        headers=PUBLIC_HOST,
    )
    assert r.status_code == 404


def test_infra_upstream_urls_are_frozen():
    # The persistent-SSRF vector: these upstream URLs must no longer be runtime
    # mutable, so /api/settings/overrides can never repoint the torznab proxy.
    from config import MUTABLE_FIELDS

    for frozen in ("bitagent_graphql_url", "bitagent_torznab_url", "bitagent_metrics_url"):
        assert frozen not in MUTABLE_FIELDS, f"{frozen} must be frozen (SSRF vector)"
    # *arr integration fields stay mutable (operator Integrations tab needs them).
    for kept in ("sonarr_base_url", "lidarr_api_key", "tmdb_api_key"):
        assert kept in MUTABLE_FIELDS

    # Trust-boundary controls are startup-only. A DB override can never widen
    # identity sources, hosts, or operator roles at runtime.
    for security_field in (
        "trust_npm_headers", "trust_forwarded_user", "trusted_proxy_cidrs",
        "proxy_auth_secret", "public_library_hosts", "operator_hosts",
        "operator_roles", "sso_cookie_name",
    ):
        assert security_field not in MUTABLE_FIELDS


def test_frozen_field_override_rejected_on_operator_host(client):
    # Even an operator can't repoint the torznab upstream URL now.
    r = client.put(
        "/api/settings/overrides/bitagent_torznab_url",
        json={"value": "http://169.254.169.254/latest/meta-data/"},
        headers=OPERATOR_HOST,
    )
    assert r.status_code == 403

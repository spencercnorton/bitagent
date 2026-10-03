"""Site links reuse verified roles and existing host gates; they grant no access."""
from __future__ import annotations

import pytest
from fastapi import Request
from fastapi.testclient import TestClient

import app as app_module
import config
import navigation


PROXY_SECRET = "navigation-test-proxy-proof-at-least-32-bytes"


def _headers(role: str = "OWNER", host: str = "library.example.org") -> dict[str, str]:
    return {
        "host": host,
        "X-Forwarded-User": "navigation-test-owner",
        "X-Auth-Priv": role,
        "X-BitAgent-Proxy-Proof": PROXY_SECRET,
    }


@pytest.fixture(autouse=True)
def _navigation_settings(monkeypatch):
    for key, value in {
        "require_auth": True,
        "dashboard_api_key": "",
        "trust_npm_headers": False,
        "trust_forwarded_user": True,
        "trusted_proxy_cidrs": "127.0.0.0/8",
        "proxy_auth_secret": PROXY_SECRET,
        "operator_roles": "OWNER",
        "operator_ui_url": "",
        "operator_ingress_host": "",
        "library_ui_url": "",
    }.items():
        monkeypatch.setattr(config.settings, key, value)


@pytest.mark.parametrize("path", ["/", "/library"])
def test_verified_operator_gets_public_admin_destination(client, path):
    config.settings.operator_ui_url = "https://console.example.org/admin/"
    response = client.get(path, headers=_headers())
    assert response.status_code == 200
    assert response.context["admin_url"] == "https://console.example.org/admin/"
    assert response.context["identity"]["operator"] is True
    assert 'id="libAdminLink" href="https://console.example.org/admin/"' in response.text


@pytest.mark.parametrize("role", ["VIEWER", "ELITE", "UNASSIGNED", ""])
def test_non_operator_has_no_admin_destination(client, role):
    config.settings.operator_ui_url = "https://console.example.org/"
    response = client.get("/", headers=_headers(role))
    assert response.status_code == 200
    assert response.context["admin_url"] == ""
    assert response.context["identity"]["operator"] is False
    assert 'id="libAdminLink"' not in response.text


def test_custom_verified_operator_role_uses_existing_grant(client):
    config.settings.operator_roles = "OWNER,MAINTAINER"
    response = client.get("/", headers=_headers("maintainer"))
    assert response.status_code == 200
    assert response.context["admin_url"] == "//console.example.org/"


@pytest.mark.parametrize("headers", [
    {"host": "library.example.org"},
    {"host": "library.example.org", "X-Forwarded-User": "spoofed", "X-Auth-Priv": "OWNER"},
    {"host": "library.example.org", "X-Auth-Priv": "OWNER", "X-BitAgent-Proxy-Proof": PROXY_SECRET},
])
def test_no_identity_or_unverified_headers_cannot_open_library(client, headers):
    assert client.get("/", headers=headers).status_code == 401


def test_untrusted_transport_peer_cannot_use_owner_headers():
    async def untrusted_peer(scope, receive, send):
        if scope["type"] == "http":
            scope = {**scope, "client": ("198.51.100.20", 50000)}
        await app_module.app(scope, receive, send)

    with TestClient(untrusted_peer) as client:
        assert client.get("/", headers=_headers()).status_code == 401


def test_link_does_not_expand_host_or_operator_gates(client):
    assert client.get("/api/settings", headers=_headers()).status_code == 404
    assert client.get("/", headers=_headers("VIEWER", "console.example.org")).status_code == 403
    assert client.get("/api/settings", headers=_headers("VIEWER", "console.example.org")).status_code == 403
    assert client.get("/", headers=_headers(host="unmapped.example.org")).status_code == 421


@pytest.mark.parametrize("suffix", ["/admin", "/admin/"])
def test_canonical_admin_proxy_prefix_preserves_role_and_host_gates(client, suffix):
    config.settings.operator_ingress_host = "console.example.org"
    config.settings.operator_ui_url = "https://library.example.org" + suffix
    public = client.get("/", headers=_headers())
    assert public.status_code == 200
    assert public.context["admin_url"] == config.settings.operator_ui_url
    operator = client.get("/", headers=_headers(host="console.example.org"))
    assert operator.status_code == 200
    assert operator.context["operator_path"] == "/admin"
    assert 'data-operator-path="/admin"' in operator.text
    assert 'href="/admin/manifest.webmanifest"' in operator.text
    manifest = client.get("/manifest.webmanifest", headers={"host": "console.example.org"})
    assert manifest.json()["start_url"] == "/admin/"
    assert manifest.json()["scope"] == "/admin/"
    public_manifest = client.get("/manifest.webmanifest", headers={"host": "library.example.org"})
    assert public_manifest.json()["start_url"] == "/"
    # The proxy must strip the prefix and set its fixed operator Host. Merely
    # adding a path or forwarded host never changes the application host gate.
    assert client.get("/admin/", headers=_headers()).status_code == 404
    assert client.get("/api/settings", headers={**_headers(), "X-Forwarded-Host": "console.example.org"}).status_code == 404
    assert client.get("/", headers=_headers("VIEWER", "console.example.org")).status_code == 403
    assert client.get("/api/settings", headers=_headers("VIEWER", "console.example.org")).status_code == 403
    viewer = client.get("/", headers=_headers("VIEWER"))
    assert viewer.context["admin_url"] == ""
    assert 'id="libAdminLink"' not in viewer.text


def test_canonical_admin_proxy_library_return_links_use_configured_prefix(client):
    config.settings.operator_ingress_host = "console.example.org"
    config.settings.operator_ui_url = "https://library.example.org/admin"
    response = client.get("/library", headers=_headers(host="console.example.org"))
    assert response.context["admin_url"] == "/admin/"


@pytest.mark.parametrize("url", [
    "http://library.example.org/admin", "https://library.example.org/",
    "https://library.example.org/admin/other", "https://library.example.org/%61dmin",
    "https://library.example.org//admin", "https://library.example.org/admin/../",
])
def test_canonical_public_operator_destination_only_accepts_reserved_https_path(url):
    config.settings.operator_ui_url = url
    with pytest.raises(RuntimeError, match="OPERATOR_UI_URL"):
        navigation.validate_navigation_settings()


def test_operator_host_library_links_to_its_own_root(client):
    config.settings.operator_ui_url = "https://console-alt.example.org/another-root/"
    response = client.get("/library", headers=_headers(host="console.example.org:8443"))
    assert response.status_code == 200
    assert response.context["admin_url"] == "/"
    assert response.context["show_library_stats"] is False
    assert client.get("/", headers=_headers(host="console.example.org:8443")).status_code == 200


def test_canonical_library_destination_preserves_scheme_port_and_path(client):
    config.settings.library_ui_url = "https://library.example.org:9443/browse/"
    response = client.get("/", headers=_headers(host="console.example.org"))
    assert response.status_code == 200
    assert response.context["library_url"] == "https://library.example.org:9443/browse/"
    assert 'id="operatorLibraryLink" href="https://library.example.org:9443/browse/"' in response.text


def test_fallbacks_keep_csv_order_and_never_copy_production_port(client):
    config.settings.operator_hosts = "localhost,127.0.0.1,CONSOLE-ALT.EXAMPLE.ORG.,console.example.org"
    response = client.get("/", headers=_headers(host="library.example.org:8080"))
    assert response.status_code == 200
    assert response.context["admin_url"] == "//console-alt.example.org/"

    config.settings.public_library_hosts = "library.localhost,LIBRARY.EXAMPLE.ORG.,library-alt.example.org"
    response = client.get("/", headers=_headers(host="console.example.org:8443"))
    assert response.status_code == 200
    assert response.context["library_url"] == "//library.example.org/"


def test_auth_enabled_local_only_destinations_do_not_guess_a_public_admin_site(client):
    config.settings.operator_hosts = "localhost,127.0.0.1"
    public = client.get("/", headers=_headers())
    assert public.status_code == 200
    assert public.context["admin_url"] == ""
    assert 'id="libAdminLink"' not in public.text

    config.settings.public_library_hosts = "library.localhost"
    operator = client.get("/", headers=_headers(host="localhost:8080"))
    assert operator.status_code == 200
    assert operator.context["library_url"] == "/library"


@pytest.mark.parametrize("host", ["console.localhost", "127.0.0.2", "127.1", "2130706433", "0x7f000001"])
def test_loopback_aliases_are_excluded_from_authenticated_fallback(client, host):
    config.settings.operator_hosts = host
    response = client.get("/", headers=_headers())
    assert response.status_code == 200
    assert response.context["admin_url"] == ""


def test_explicit_configured_local_url_is_respected(client):
    config.settings.operator_hosts = "localhost"
    config.settings.operator_ui_url = "http://localhost:8765/console/"
    response = client.get("/", headers=_headers())
    assert response.status_code == 200
    assert response.context["admin_url"] == "http://localhost:8765/console/"


def test_open_local_development_roundtrip_preserves_explicit_port(client):
    config.settings.require_auth = False
    config.settings.public_library_hosts = "library.localhost"
    config.settings.operator_hosts = "localhost,127.0.0.1"
    public = client.get("/", headers={"host": "library.localhost:8765"})
    assert public.status_code == 200
    assert public.context["admin_url"] == "//localhost:8765/"
    operator = client.get("/", headers={"host": "localhost:8765"})
    assert operator.status_code == 200
    assert operator.context["library_url"] == "//library.localhost:8765/"
    assert client.get("/", headers={"host": "unmapped.localhost:8765"}).status_code == 421


@pytest.mark.parametrize("port", ["0", "65536"])
def test_local_fallback_does_not_copy_an_invalid_port(port):
    config.settings.require_auth = False
    config.settings.public_library_hosts = "library.localhost"
    config.settings.operator_hosts = "localhost"
    request = Request({"type": "http", "headers": [(b"host", f"library.localhost:{port}".encode())]})
    assert navigation.operator_url(request, {"operator": True}) == ""


def test_open_mode_does_not_copy_remote_port_to_local_destination(client):
    config.settings.require_auth = False
    config.settings.operator_hosts = "localhost,console.example.org"
    response = client.get("/", headers={"host": "library.example.org:8765"})
    assert response.context["admin_url"] == "//console.example.org/"


@pytest.mark.parametrize("field,url", [
    ("operator_ui_url", "/admin"),
    ("operator_ui_url", "//console.example.org/"),
    ("operator_ui_url", "javascript:alert(1)"),
    ("operator_ui_url", "https://library.example.org/"),
    ("operator_ui_url", "https://console.example.org.attacker.invalid/"),
    ("operator_ui_url", "https://user:secret@console.example.org/"),
    ("operator_ui_url", "https://console.example.org/?apikey=secret"),
    ("operator_ui_url", "https://console.example.org/?"),
    ("operator_ui_url", "https://console.example.org/#"),
    ("operator_ui_url", "https://console.example.org/#dashboard"),
    ("operator_ui_url", "https://console.example.org/\n"),
    ("operator_ui_url", "https://console.example.org/with space"),
    ("operator_ui_url", "https://console.example.org/\\attacker.invalid"),
    ("operator_ui_url", "https://console.example.org/%5cattacker.invalid"),
    ("operator_ui_url", "https://console.example.org/%0a"),
    ("operator_ui_url", "https://console.example.org:65536/"),
    ("operator_ui_url", "https://console.example.org:0/"),
    ("operator_ui_url", "https://console.example.org:wrong/"),
    ("library_ui_url", "https://console.example.org/"),
    ("library_ui_url", "https://attacker.invalid/"),
])
def test_invalid_navigation_destination_fails_startup_without_echoing_url(field, url):
    setattr(config.settings, field, url)
    with pytest.raises(RuntimeError, match=field.upper()) as error:
        with TestClient(app_module.app):
            pass
    assert url not in str(error.value)


@pytest.mark.parametrize("url", [
    "http://console.example.org:8080/base-path",
    "https://CONSOLE.EXAMPLE.ORG./base-path/",
    "https://console.example.org/base%20path/",
])
def test_valid_canonical_urls_keep_the_configured_base_path(url):
    config.settings.operator_ui_url = url
    with TestClient(app_module.app) as client:
        # This client has no trusted IP, so use the explicit dashboard-key tier.
        config.settings.dashboard_api_key = "synthetic-navigation-key"
        response = client.get("/", headers={"host": "library.example.org", "X-Api-Key": "synthetic-navigation-key"})
        assert response.status_code == 200
        assert response.context["admin_url"] == url


@pytest.mark.parametrize("field", ["operator_ui_url", "library_ui_url"])
def test_navigation_urls_cannot_be_mutated_through_settings(client, field):
    assert field not in config.MUTABLE_FIELDS
    headers = _headers(host="console.example.org")
    assert client.put(f"/api/settings/overrides/{field}", json={"value": "https://attacker.invalid/"}, headers=headers).status_code == 403
    assert client.delete(f"/api/settings/overrides/{field}", headers=headers).status_code == 403


def test_navigation_never_copies_authentication_or_return_parameters(client):
    config.settings.dashboard_api_key = "synthetic-navigation-key"
    response = client.get(
        "/?apikey=synthetic-navigation-key&return=https://attacker.invalid/",
        headers={"host": "library.example.org"},
    )
    assert response.status_code == 200
    assert response.context["admin_url"] == "//console.example.org/"
    assert response.context["identity"]["method"] == "api-key"
    assert client.get(
        "/?apikey=wrong", headers=_headers(),
    ).status_code == 401


def test_unassigned_provider_subject_is_unchanged_and_has_no_operator_link(client):
    headers = {
        **_headers("UNASSIGNED"),
        "X-Forwarded-User": "navigation-test-local-account",
        "X-Auth-Provider": "test-local-provider",
    }
    public = client.get("/", headers=headers)
    assert public.status_code == 200
    assert public.context["admin_url"] == ""
    for host in ("library.example.org", "console.example.org"):
        identity = client.get("/api/me", headers={**headers, "host": host}).json()
        assert identity["id"] == "navigation-test-local-account"
        assert identity["operator"] is False
    assert client.get("/", headers={**headers, "host": "console.example.org"}).status_code == 403


def test_cookie_presence_does_not_gain_a_navigation_identity(client):
    client.cookies.set(config.settings.sso_cookie_name, "synthetic-unverified-session")
    assert client.get("/", headers={"host": "library.example.org"}).status_code == 401

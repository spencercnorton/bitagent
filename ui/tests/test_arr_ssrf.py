"""SSRF guard on operator-set *arr base URLs.

The *arr API key is forwarded to base_url on every push/sync, so base_url must
not resolve to host-local or link-local space (cloud metadata endpoint, loopback
services reachable under this container's host networking). Tailnet (100.64/10)
and RFC1918 ARE allowed — the real Sonarr/Radarr/Lidarr live on the tailnet.

Literal-IP cases need no DNS: getaddrinfo resolves numeric addresses offline, so
the suite stays hermetic. One monkeypatched case covers name->link-local (the
SSRF-by-DNS shape).
"""
import asyncio
import socket

import pytest
from fastapi import HTTPException

import app as app_module


def _check(url: str) -> None:
    asyncio.run(app_module._assert_safe_arr_base_url(url))


@pytest.mark.parametrize("url", [
    "http://100.64.0.1:8989",        # CGNAT / tailnet (100.64/10) — a typical Sonarr
    "https://100.64.0.1",            # CGNAT / tailscale range
    "http://192.168.0.5:7878",       # RFC1918 LAN
    "http://10.0.0.9:8686",          # RFC1918
])
def test_allows_private_and_tailnet(url):
    _check(url)  # must not raise


@pytest.mark.parametrize("url", [
    "http://169.254.169.254/latest/meta-data/",  # cloud metadata (link-local)
    "http://127.0.0.1:9443/",                     # loopback -> host-local Portainer
    "http://[::1]:8989/",                          # ipv6 loopback
    "http://0.0.0.0:8080/",                        # unspecified
    "http://224.0.0.1/",                           # multicast
])
def test_blocks_internal_targets(url):
    with pytest.raises(HTTPException) as ei:
        _check(url)
    assert ei.value.status_code == 400


@pytest.mark.parametrize("url", [
    "ftp://100.64.0.1/",       # non-http scheme
    "file:///etc/passwd",
    "gopher://100.64.0.1/",
    "http:///no-host",               # empty host
])
def test_blocks_bad_scheme_or_host(url):
    with pytest.raises(HTTPException) as ei:
        _check(url)
    assert ei.value.status_code == 400


def test_hostname_resolving_to_metadata_is_blocked(monkeypatch):
    """A name that resolves into link-local space (SSRF-by-DNS) is refused."""
    def fake(host, port, *a, **k):
        return [(socket.AF_INET, socket.SOCK_STREAM, 6, "", ("169.254.169.254", port or 80))]
    monkeypatch.setattr(socket, "getaddrinfo", fake)
    with pytest.raises(HTTPException) as ei:
        _check("http://sonarr.evil.example/")
    assert ei.value.status_code == 400


def test_set_override_route_rejects_ssrf_url(client):
    """End-to-end: the settings PUT refuses a metadata URL and accepts a tailnet one."""
    import config
    config.settings.require_auth = False  # restored by conftest _restore_settings
    bad = client.put("/api/settings/overrides/sonarr_base_url",
                     json={"value": "http://169.254.169.254/"})
    assert bad.status_code == 400
    ok = client.put("/api/settings/overrides/sonarr_base_url",
                    json={"value": "http://100.64.0.1:8989"})
    assert ok.status_code == 200

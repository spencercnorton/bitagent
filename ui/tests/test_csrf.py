"""CSRF defense-in-depth: Sec-Fetch-Site gate on state-changing requests.

The gate is self-contained (no dependency on an upstream SameSite cookie) and
covers even the no-body POST routes. It enforces only when the browser-set
Sec-Fetch-Site header is present, so non-browser callers (Sonarr/Prowlarr,
api-key clients, curl) are never blocked.
"""
import config


def _no_auth():
    # Restored by conftest _restore_settings; lets the mutation route return 200
    # so "not blocked" is distinguishable from an auth 401.
    config.settings.require_auth = False


def test_cross_site_mutation_blocked(client):
    _no_auth()
    r = client.post("/api/wants/sync", headers={"Sec-Fetch-Site": "cross-site"})
    assert r.status_code == 403


def test_none_fetch_site_mutation_blocked(client):
    _no_auth()
    r = client.post("/api/wants/sync", headers={"Sec-Fetch-Site": "none"})
    assert r.status_code == 403


def test_same_origin_mutation_allowed(client):
    _no_auth()
    r = client.post("/api/wants/sync", headers={"Sec-Fetch-Site": "same-origin"})
    assert r.status_code != 403


def test_missing_header_mutation_allowed(client):
    _no_auth()
    r = client.post("/api/wants/sync")
    assert r.status_code != 403


def test_safe_method_cross_site_allowed(client):
    _no_auth()
    r = client.get("/api/wants", headers={"Sec-Fetch-Site": "cross-site"})
    assert r.status_code != 403

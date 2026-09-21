"""Shared pytest fixtures.

The whole suite is hermetic: the SQLite DB is redirected to a throwaway temp
file before the app opens it, and auth-relevant settings are snapshotted and
restored around every test so order can't leak state.
"""
from __future__ import annotations

import pathlib
import sys
import tempfile

# Make the flat-layout modules importable even when the suite is run from the
# repo root without `pip install -e .`.
_ROOT = pathlib.Path(__file__).resolve().parent.parent
if str(_ROOT) not in sys.path:
    sys.path.insert(0, str(_ROOT))

import pytest  # noqa: E402
import config  # noqa: E402

# Point the SQLite DB at a throwaway file BEFORE app/database open a connection.
_TMP_DB = tempfile.NamedTemporaryFile(prefix="bitagent-ui-test-", suffix=".db", delete=False)
_TMP_DB.close()
config.settings.db_path = _TMP_DB.name

# Both surfaces are selected by Host, so the suite names them explicitly (the
# shipped defaults are local-development names). TestClient's default Host is
# "testserver" — treat it as an operator host so route tests exercise the
# operator surface by default. Must happen BEFORE app import (the allowlist is
# frozen at import). The fail-closed unknown-host behavior is tested explicitly
# in test_operator_gate.py with a host that is NOT in this list.
config.settings.public_library_hosts = "library.example.org"
config.settings.operator_hosts = "console.example.org,console-alt.example.org,localhost,127.0.0.1,testserver"

from fastapi.testclient import TestClient  # noqa: E402
import app as app_module  # noqa: E402


def _with_transport_peer(asgi_app, peer: str):
    """Wrap ASGI HTTP scopes with a deterministic transport peer IP.

    Starlette's TestClient uses the non-IP string ``testclient`` by default;
    production authorization correctly rejects that. This wrapper keeps the
    shipped dependency version while making CIDR behavior explicit in tests.
    """
    async def wrapped(scope, receive, send):
        if scope["type"] == "http":
            scope = {**scope, "client": (peer, 50000)}
        await asgi_app(scope, receive, send)
    return wrapped


@pytest.fixture()
def client():
    # Give the ASGI transport a real IP so proxy-CIDR checks are testable, and
    # explicitly add TestClient's synthetic Host to the operator allowlist.
    configured_operator_hosts = config.settings.operator_hosts
    config.settings.operator_hosts = f"{configured_operator_hosts},testserver"
    config.settings.trusted_proxy_cidrs = "127.0.0.0/8"
    with TestClient(_with_transport_peer(app_module.app, "127.0.0.1")) as c:
        yield c


@pytest.fixture(autouse=True)
def _reset_app_caches():
    """Clear the process-wide /api/torrents response cache and block-phrase cache
    before each test. Both key on request params (which repeat across tests), so a
    cached result would otherwise leak into a later test with a different backend."""
    app_module._invalidate_block_phrases_cache()
    app_module._METRIC_HISTORY.clear()
    app_module._reset_stats_snapshot_cache()
    app_module._reset_library_stats_cache()
    app_module._reset_indexer_stats_cache()
    yield
    app_module._invalidate_block_phrases_cache()
    app_module._METRIC_HISTORY.clear()
    app_module._reset_stats_snapshot_cache()
    app_module._reset_library_stats_cache()
    app_module._reset_indexer_stats_cache()


@pytest.fixture(autouse=True)
def _restore_settings():
    """Snapshot + restore the mutable auth knobs around each test."""
    s = config.settings
    keys = (
        "require_auth", "dashboard_api_key", "trust_npm_headers",
        "trust_forwarded_user", "trusted_proxy_cidrs", "proxy_auth_secret",
        "operator_roles", "operator_hosts", "public_library_hosts",
        "sso_cookie_name", "tmdb_api_key", "app_switcher_script_url",
        "torznab_api_key", "bitagent_graphql_url", "bitagent_torznab_url",
        "bitagent_metrics_url",
        # Integration creds — route tests mutate these to exercise the
        # configured/unconfigured branches; restore so order can't leak state.
        "sonarr_base_url", "sonarr_api_key",
        "radarr_base_url", "radarr_api_key",
        "lidarr_base_url", "lidarr_api_key",
    )
    saved = {k: getattr(s, k) for k in keys}
    try:
        yield
    finally:
        for k, v in saved.items():
            setattr(s, k, v)

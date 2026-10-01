"""Real SDK batches reach startup atomically before authorization and workers."""
from __future__ import annotations

import asyncio
import socket
from unittest.mock import AsyncMock

import httpx

import app as app_module
import auth
import config
import database
import infisical


PROOF = "synthetic-hydration-proof-at-least-32-bytes"


class _Response:
    def __init__(self, data):
        self.data = data

    def raise_for_status(self):
        pass

    def json(self):
        return self.data


class _Provider:
    def __init__(self, batches):
        self.batches = iter(batches)
        self.calls = 0

    def __call__(self, *args, **kwargs):
        return self

    def __enter__(self):
        return self

    def __exit__(self, *args):
        pass

    def post(self, url, **kwargs):
        assert url == "https://secrets.example.org/api/v1/auth/universal-auth/login"
        return _Response({"accessToken": "synthetic-service-token"})

    def get(self, url, **kwargs):
        assert url == "https://secrets.example.org/api/v3/secrets/raw"
        self.calls += 1
        return _Response({"secrets": next(self.batches)})


def _prepare(monkeypatch, child_batch):
    for key, value in {
        "require_auth": True, "dashboard_api_key": "", "operator_roles": "OWNER",
        "trust_npm_headers": True, "trust_forwarded_user": False,
        "trusted_proxy_cidrs": "127.0.0.1/32", "proxy_auth_secret": PROOF,
        "operator_hosts": "console.example.org", "public_library_hosts": "library.example.org",
        "operator_ui_url": "", "library_ui_url": "", "private_indexer_enabled": False,
        "private_invitations_enabled": False, "private_invitation_bridge_enabled": False,
        "private_invitation_checkout_enabled": False, "tmdb_api_key": "initial-synthetic-secret",
    }.items():
        monkeypatch.setattr(config.settings, key, value)
    for key, value in {
        "INFISICAL_CLIENT_ID": "synthetic-id", "INFISICAL_CLIENT_SECRET": "synthetic-secret",
        "INFISICAL_URL": "https://secrets.example.org", "INFISICAL_ENV": "fixture",
        "INFISICAL_PROJECT_ID": "fixture-project", "INFISICAL_PATH": "/example",
    }.items():
        monkeypatch.setenv(key, value)

    provider = _Provider([
        [{"secretKey": "OPERATOR_ROLES", "secretValue": "OWNER"}], child_batch,
    ])
    monkeypatch.setattr(infisical.httpx, "Client", provider)
    assert infisical.hydrate_settings(config.settings) == ["operator_roles"]
    auth.validate_auth_settings()
    return provider


def _lifespan(monkeypatch):
    effects = []
    hydrated = []
    states = []

    def record(effect):
        effects.append(effect)
        states.append(config.settings.model_dump())

    def loader(settings):
        result = infisical.hydrate_settings(settings)
        hydrated.append(result)
        return result

    async def get_db():
        record("database")

    def network_forbidden(*args, **kwargs):
        raise AssertionError("Real network transport is forbidden in this test")

    monkeypatch.setattr(socket.socket, "connect", network_forbidden)
    monkeypatch.setattr(socket.socket, "connect_ex", network_forbidden)
    monkeypatch.setattr(httpx.HTTPTransport, "handle_request", network_forbidden)
    monkeypatch.setattr(httpx.AsyncHTTPTransport, "handle_async_request", network_forbidden)
    monkeypatch.setattr(app_module, "hydrate_settings", loader)
    monkeypatch.setattr(database, "get_db", get_db)
    monkeypatch.setattr(app_module, "get_all_overrides", AsyncMock(return_value={}))
    monkeypatch.setattr(app_module, "close_all", AsyncMock())
    monkeypatch.setattr(app_module.private_indexer.readiness, "acquire_owner", lambda: record("owner"))
    monkeypatch.setattr(app_module.private_indexer.readiness, "release_owner", lambda: None)
    monkeypatch.setattr(app_module.private_indexer, "reset_readiness", AsyncMock())
    monkeypatch.setattr(app_module.private_indexer, "start_readiness_worker", lambda: record("private-worker"))
    monkeypatch.setattr(app_module.private_indexer, "stop_readiness_worker", AsyncMock())
    monkeypatch.setattr(app_module.invitation_payments, "start_worker", lambda: record("payment-worker"))
    monkeypatch.setattr(app_module.invitation_payments, "stop_worker", AsyncMock())

    async def run():
        responses = {}
        async with app_module.lifespan(app_module.app):
            record("serving")
            transport = httpx.ASGITransport(app=app_module.app, client=("127.0.0.1", 60000))
            async with httpx.AsyncClient(transport=transport, base_url="http://console.example.org") as client:
                for role in ("MEMBER", "MAINTAINER", "OWNER"):
                    headers = {"X-Auth-User-Id": "synthetic-account", "X-Auth-Priv": role,
                               "X-BitAgent-Proxy-Proof": PROOF}
                    responses[role] = {
                        "settings": (await client.get("/api/settings", headers=headers)).status_code,
                        "operator": (await client.get("/api/me", headers=headers)).json()["operator"],
                    }
        return responses

    return asyncio.run(run()), effects, hydrated, states


def test_failed_child_batch_retains_all_settings_before_authority_and_lifespan_effects(monkeypatch, caplog):
    provider = _prepare(monkeypatch, [
        {"secretKey": "TMDB_API_KEY", "secretValue": "staged-synthetic-secret-never-log"},
        {"secretKey": "OPERATOR_ROLES", "secretValue": "MEMBER"},
        {"secretKey": "REQUIRE_AUTH", "secretValue": "not-a-boolean"},
    ])
    settings_id = id(config.settings)
    original = config.settings.model_dump()
    fields_set = set(config.settings.model_fields_set)
    responses, effects, hydrated, states = _lifespan(monkeypatch)
    assert provider.calls == 2
    assert hydrated == [[]]
    assert id(config.settings) == settings_id
    assert config.settings.model_dump() == original
    assert config.settings.model_fields_set == fields_set
    assert effects == ["owner", "database", "private-worker", "payment-worker", "serving"]
    assert all(state == original for state in states)
    assert responses["MEMBER"] == {"settings": 403, "operator": False}
    assert responses["OWNER"] == {"settings": 200, "operator": True}
    assert "staged-synthetic-secret-never-log" not in caplog.text
    assert "not-a-boolean" not in caplog.text


def test_successful_child_batch_retains_intentional_custom_role_and_secret_precedence(monkeypatch):
    provider = _prepare(monkeypatch, [
        {"secretKey": "TMDB_API_KEY", "secretValue": "valid-synthetic-secret"},
        {"secretKey": "OPERATOR_ROLES", "secretValue": "OWNER,MAINTAINER"},
    ])
    settings_id = id(config.settings)
    responses, effects, hydrated, states = _lifespan(monkeypatch)
    assert provider.calls == 2
    assert hydrated == [["tmdb_api_key", "operator_roles"]]
    assert id(config.settings) == settings_id
    assert config.settings.tmdb_api_key == "valid-synthetic-secret"
    assert config.settings.operator_roles == "OWNER,MAINTAINER"
    assert all(state["operator_roles"] == "OWNER,MAINTAINER" for state in states)
    assert effects[-1] == "serving"
    assert responses["MAINTAINER"] == {"settings": 200, "operator": True}
    assert responses["MEMBER"] == {"settings": 403, "operator": False}

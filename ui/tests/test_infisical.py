"""Infisical hydration: precedence, field-matching, and fail-open contract.

These pin the behaviours that matter for the 2026-06-24 fix: Infisical
overrides the env-derived value for matching fields, ignores unknown keys,
and NEVER raises on a secret-store error. Application startup validation is a
separate gate and is not bypassed by hydration's env-value fallback.
"""
import infisical
import config


def _real_settings():
    return config.Settings(_env_file=None, require_auth=True, operator_roles="OWNER", tmdb_api_key="initial-synthetic-key")


class _Settings:
    """Minimal stand-in for the pydantic Settings instance."""

    def __init__(self):
        self.tmdb_api_key = "env-literal-30char"
        self.log_level = "info"


class _Resp:
    def __init__(self, data):
        self._data = data

    def raise_for_status(self):
        return self

    def json(self):
        return self._data


class _FakeClient:
    """Stubs httpx.Client: login -> token, secrets -> fixed payload."""

    def __init__(self, secrets):
        self._secrets = secrets

    def __call__(self, *a, **k):  # httpx.Client(...) constructor
        return self

    def __enter__(self):
        return self

    def __exit__(self, *a):
        return False

    def post(self, url, json=None):
        assert url == "https://secrets.example.org/api/v1/auth/universal-auth/login"
        assert json == {"clientId": "cid", "clientSecret": "csec"}
        return _Resp({"accessToken": "tok"})

    def get(self, url, params=None, headers=None):
        assert url == "https://secrets.example.org/api/v3/secrets/raw"
        assert params == {
            "workspaceId": "proj-123", "environment": "fixture", "secretPath": "/example",
        }
        assert headers["Authorization"] == "Bearer tok"
        return _Resp({"secrets": self._secrets})


def _set_creds(monkeypatch, project="proj-123"):
    monkeypatch.setenv("INFISICAL_URL", "https://secrets.example.org/")
    monkeypatch.setenv("INFISICAL_ENV", "fixture")
    monkeypatch.setenv("INFISICAL_PATH", "/example")
    monkeypatch.setenv("INFISICAL_CLIENT_ID", "cid")
    monkeypatch.setenv("INFISICAL_CLIENT_SECRET", "csec")
    if project is None:
        monkeypatch.delenv("INFISICAL_PROJECT_ID", raising=False)
    else:
        monkeypatch.setenv("INFISICAL_PROJECT_ID", project)


def test_no_creds_is_noop(monkeypatch):
    monkeypatch.delenv("INFISICAL_CLIENT_ID", raising=False)
    monkeypatch.delenv("INFISICAL_CLIENT_SECRET", raising=False)
    s = _Settings()
    assert infisical.hydrate_settings(s) == []
    assert s.tmdb_api_key == "env-literal-30char"  # untouched


def test_missing_project_id_is_noop(monkeypatch):
    _set_creds(monkeypatch, project=None)
    s = _Settings()
    assert infisical.hydrate_settings(s) == []
    assert s.tmdb_api_key == "env-literal-30char"


def test_hydrate_overrides_matching_field_only(monkeypatch):
    _set_creds(monkeypatch)
    monkeypatch.setattr(
        infisical.httpx, "Client",
        _FakeClient([
            {"secretKey": "TMDB_API_KEY", "secretValue": "f" * 32},
            {"secretKey": "TOTALLY_UNKNOWN", "secretValue": "ignored"},
        ]),
    )
    s = _Settings()
    applied = infisical.hydrate_settings(s)
    assert applied == ["tmdb_api_key"]
    assert s.tmdb_api_key == "f" * 32  # Infisical wins with a synthetic value
    assert not hasattr(s, "totally_unknown")  # unknown keys skipped


def test_empty_value_does_not_clobber(monkeypatch):
    _set_creds(monkeypatch)
    monkeypatch.setattr(
        infisical.httpx, "Client",
        _FakeClient([{"secretKey": "TMDB_API_KEY", "secretValue": ""}]),
    )
    s = _Settings()
    assert infisical.hydrate_settings(s) == []
    assert s.tmdb_api_key == "env-literal-30char"  # blank secret ignored


def test_fail_open_on_network_error(monkeypatch):
    _set_creds(monkeypatch)

    class _Boom:
        def __call__(self, *a, **k):
            return self

        def __enter__(self):
            return self

        def __exit__(self, *a):
            return False

        def post(self, *a, **k):
            raise RuntimeError("infisical unreachable")

    monkeypatch.setattr(infisical.httpx, "Client", _Boom())
    s = _Settings()
    assert infisical.hydrate_settings(s) == []  # never raises
    assert s.tmdb_api_key == "env-literal-30char"


def test_invalid_late_assignment_rolls_back_all_fields_and_model_metadata(monkeypatch, caplog):
    _set_creds(monkeypatch)
    monkeypatch.setattr(infisical.httpx, "Client", _FakeClient([
        {"secretKey": "TMDB_API_KEY", "secretValue": "new-synthetic-secret-never-log"},
        {"secretKey": "OPERATOR_ROLES", "secretValue": "MEMBER"},
        {"secretKey": "REQUIRE_AUTH", "secretValue": "not-a-boolean"},
    ]))
    settings = _real_settings()
    identity = id(settings)
    original = settings.model_dump()
    fields_set = set(settings.model_fields_set)
    values = settings.__dict__
    assert infisical.hydrate_settings(settings) == []
    assert settings.model_dump() == original
    assert settings.model_fields_set == fields_set
    assert settings.__dict__ is values
    assert id(settings) == identity
    for sensitive in ("initial-synthetic-key", "new-synthetic-secret-never-log", "not-a-boolean"):
        assert sensitive not in caplog.text


def test_malformed_late_record_rolls_back_a_simple_settings_fixture(monkeypatch):
    _set_creds(monkeypatch)
    monkeypatch.setattr(infisical.httpx, "Client", _FakeClient([
        {"secretKey": "TMDB_API_KEY", "secretValue": "staged-synthetic-secret"},
        {"secretKey": "LOG_LEVEL", "secretValue": "debug"},
        None,
    ]))
    settings = _Settings()
    before = dict(vars(settings))
    identity = id(settings)
    assert infisical.hydrate_settings(settings) == []
    assert vars(settings) == before
    assert id(settings) == identity


def test_client_cleanup_failure_does_not_publish_staged_fields(monkeypatch):
    _set_creds(monkeypatch)
    class FailedCleanup(_FakeClient):
        def __exit__(self, *args):
            raise RuntimeError("synthetic cleanup failure")
    monkeypatch.setattr(infisical.httpx, "Client", FailedCleanup([
        {"secretKey": "TMDB_API_KEY", "secretValue": "staged-synthetic-secret"},
    ]))
    settings = _real_settings()
    original = settings.model_dump()
    assert infisical.hydrate_settings(settings) == []
    assert settings.model_dump() == original


def test_successful_batch_keeps_vault_precedence_custom_roles_and_typed_values(monkeypatch):
    _set_creds(monkeypatch)
    monkeypatch.setattr(infisical.httpx, "Client", _FakeClient([
        {"secretKey": "TMDB_API_KEY", "secretValue": "new-synthetic-secret"},
        {"secretKey": "OPERATOR_ROLES", "secretValue": "OWNER,MAINTAINER"},
        {"secretKey": "REQUIRE_AUTH", "secretValue": "false"},
        {"secretKey": "TORZNAB_RATE_LIMIT_PER_MIN", "secretValue": "240"},
        {"secretKey": "NOT_A_SETTING", "secretValue": "ignored"},
    ]))
    settings = _real_settings()
    identity = id(settings)
    assert infisical.hydrate_settings(settings) == [
        "tmdb_api_key", "operator_roles", "require_auth", "torznab_rate_limit_per_min",
    ]
    assert id(settings) == identity
    assert settings.tmdb_api_key == "new-synthetic-secret"
    assert settings.operator_roles == "OWNER,MAINTAINER"
    assert settings.require_auth is False
    assert settings.torznab_rate_limit_per_min == 240
    assert "torznab_rate_limit_per_min" in settings.model_fields_set
    assert not hasattr(settings, "not_a_setting")

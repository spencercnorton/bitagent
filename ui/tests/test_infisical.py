"""Infisical hydration: precedence, field-matching, and fail-open contract.

These pin the behaviours that matter for the 2026-06-24 fix: Infisical
overrides the env-derived value for matching fields, ignores unknown keys,
and NEVER raises on a secret-store error. Application startup validation is a
separate gate and is not bypassed by hydration's env-value fallback.
"""
import infisical


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

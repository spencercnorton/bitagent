"""Bounded authenticated snapshot transport, expiry and replay lifecycle."""
import asyncio
import json
from types import SimpleNamespace

import httpx
import pytest

import config
import privatebindings as bindings


def envelope(**changes):
    body = {"version": 1, "issued_at": 999, "expires_at": 1029,
            "bindings": [{"user_id": "member", "address": "192.0.2.1"}]}
    body.update(changes)
    return json.dumps(body).encode()


@pytest.fixture(autouse=True)
def state(monkeypatch):
    config.settings.private_indexer_enabled = True
    config.settings.private_peer_bindings_required = True
    config.settings.private_seeder_url = "https://seeder.example.org"
    config.settings.private_seeder_username = "synthetic-user"
    config.settings.private_seeder_password = "synthetic-password"
    clock = {"wall": 1000.0, "mono": 100.0}
    monkeypatch.setattr(bindings, "time", SimpleNamespace(time=lambda: clock["wall"], monotonic=lambda: clock["mono"]))
    bindings.reset()
    yield clock
    bindings.reset()


def install(content=None):
    bindings._install(content or envelope(), bindings.time.time(), bindings.time.monotonic())


def test_exact_identity_multiple_addresses_and_empty_withdrawal(state):
    install(envelope(bindings=[{"user_id": "member", "address": "192.0.2.1"},
                               {"user_id": "member", "address": "192.0.2.2"}]))
    assert bindings.matches("192.0.2.1", "member")
    assert bindings.matches("192.0.2.2", "member")
    assert not bindings.matches("192.0.2.1", "other")
    assert not bindings.matches("192.0.2.3", "member")
    with pytest.raises(TypeError):
        bindings._SNAPSHOT.owners["192.0.2.3"] = "member"
    install(envelope(issued_at=1000, expires_at=1030, bindings=[]))
    assert not bindings.matches("192.0.2.1", "member")


def test_identical_replay_cannot_extend_monotonic_deadline(state):
    install()
    state.update(wall=995, mono=120)  # Wall clock correction cannot bank time.
    bindings._install(envelope(), 1000, 120)
    assert bindings._SNAPSHOT.deadline == 129
    state["mono"] = 129
    assert not bindings.matches("192.0.2.1", "member")
    with pytest.raises(ValueError, match="Replayed"):
        bindings._install(envelope(), 1000, 129)


def test_failed_observation_requires_a_new_source_generation(state):
    install()
    bindings.invalidate()
    with pytest.raises(ValueError, match="Replayed"):
        install()
    assert not bindings.matches("192.0.2.1", "member")
    install(envelope(issued_at=1000, expires_at=1030))
    assert bindings.matches("192.0.2.1", "member")
    with pytest.raises(ValueError, match="Retired"):
        install(envelope())
    with pytest.raises(ValueError, match="conflicting"):
        install(envelope(issued_at=1000, expires_at=1030, bindings=[]))


@pytest.mark.parametrize("changes", [
    {"version": True}, {"version": 2}, {"extra": "unknown"},
    {"issued_at": True}, {"issued_at": float("nan")}, {"expires_at": float("inf")},
    {"issued_at": 1001}, {"expires_at": 1000}, {"expires_at": 1030},
    {"bindings": {}}, {"bindings": [{}]},
    {"bindings": [{"user_id": "member", "address": "192.0.2.1", "extra": 1}]},
    {"bindings": [{"user_id": "", "address": "192.0.2.1"}]},
    {"bindings": [{"user_id": "anonymous", "address": "192.0.2.1"}]},
    {"bindings": [{"user_id": "member\n", "address": "192.0.2.1"}]},
    {"bindings": [{"user_id": 1, "address": "192.0.2.1"}]},
    {"bindings": [{"user_id": "member", "address": "::ffff:192.0.2.1"}]},
    {"bindings": [{"user_id": "member", "address": "192.000.2.1"}]},
    {"bindings": [{"user_id": "member", "address": "0.0.0.0"}]},
    {"bindings": [{"user_id": "member", "address": "127.0.0.1"}]},
    {"bindings": [{"user_id": "member", "address": "224.0.0.1"}]},
    {"bindings": [{"user_id": "member", "address": "255.255.255.255"}]},
    {"bindings": [{"user_id": "member", "address": "192.0.2.1"},
                  {"user_id": "other", "address": "192.0.2.1"}]},
    {"bindings": [{"user_id": "member", "address": "192.0.2.1"}] * 1001},
])
def test_rejects_malformed_or_unfresh_snapshot(changes):
    with pytest.raises(ValueError):
        install(envelope(**changes))
    assert not bindings.matches("192.0.2.1", "member")


@pytest.mark.parametrize("content", [b'{"version":1,"version":1}', b"not-json", b"\xff", b"x" * (256 * 1024 + 1)])
def test_rejects_ambiguous_invalid_or_oversized_json(content):
    with pytest.raises(ValueError):
        install(content)


def mock_client(monkeypatch, handler):
    actual_client = httpx.AsyncClient
    def client(**kwargs):
        assert kwargs["trust_env"] is False
        assert kwargs["follow_redirects"] is False
        return actual_client(transport=httpx.MockTransport(handler), **kwargs)
    monkeypatch.setattr(bindings.httpx, "AsyncClient", client)


@pytest.mark.parametrize("status,body,cookie", [(200, b"Ok.", "SID"), (204, b"", "QBT_SID_8080")])
def test_fresh_authenticated_fixed_snapshot_request(monkeypatch, status, body, cookie):
    calls = []
    def handler(request):
        calls.append(request.url.path)
        assert request.url.host == "seeder.example.org"
        assert request.url.scheme == "https"
        if request.url.path == "/api/v2/auth/login":
            return httpx.Response(status, content=body, headers={"set-cookie": cookie + "=" + "a" * 32 + "; Path=/; Secure; HttpOnly"})
        assert request.url.path == "/_bitagent/peer-bindings"
        assert not request.url.query
        assert request.headers["cookie"] == cookie + "=" + "a" * 32
        return httpx.Response(200, content=envelope(), headers={"content-type": "application/json"})
    mock_client(monkeypatch, handler)
    asyncio.run(bindings.refresh())
    assert calls == ["/api/v2/auth/login", "/_bitagent/peer-bindings"]
    assert bindings.matches("192.0.2.1", "member")


@pytest.mark.parametrize("failure", ["no-cookie", "api-only-cookie", "wrong-domain", "bad-login", "redirect", "forbidden", "unavailable", "wrong-type", "oversize", "compressed", "invalid-json", "expired", "unexpected"])
def test_failed_source_immediately_withdraws_cached_bindings(monkeypatch, failure):
    install()
    gets = []
    def handler(request):
        if request.url.path == "/api/v2/auth/login":
            if failure == "unexpected":
                raise RuntimeError("secret upstream detail must not escape into logs")
            cookie = "SID=" + "b" * 32 + "; Path=/"
            if failure == "api-only-cookie":
                cookie = "SID=" + "b" * 32 + "; Path=/api"
            if failure == "wrong-domain":
                cookie += "; Domain=elsewhere.example.org"
            return httpx.Response(200, content=b"wrong" if failure == "bad-login" else b"Ok.",
                                  headers={} if failure == "no-cookie" else {"set-cookie": cookie})
        gets.append(request)
        if failure in {"redirect", "forbidden", "unavailable"}:
            return httpx.Response({"redirect": 302, "forbidden": 401, "unavailable": 503}[failure], headers={"location": "https://elsewhere.example.org"})
        content = {"oversize": b"x" * (256 * 1024 + 1), "invalid-json": b"invalid", "expired": envelope(expires_at=1000)}.get(failure, envelope())
        headers = {"content-type": "text/html" if failure == "wrong-type" else "application/json"}
        if failure == "compressed":
            import gzip
            content = gzip.compress(content)
            headers["content-encoding"] = "gzip"
        return httpx.Response(200, stream=httpx.ByteStream(content), headers=headers)
    mock_client(monkeypatch, handler)
    with pytest.raises((ValueError, RuntimeError)):
        asyncio.run(bindings.refresh())
    assert not bindings.matches("192.0.2.1", "member")
    if failure in {"no-cookie", "api-only-cookie", "wrong-domain", "bad-login", "unexpected"}:
        assert not gets
    with pytest.raises(ValueError, match="Replayed"):
        install()


def test_cancellation_withdraws_and_worker_shutdown_clears_cache(monkeypatch):
    install()
    async def cancelled():
        raise asyncio.CancelledError
    monkeypatch.setattr(bindings, "_fetch", cancelled)
    with pytest.raises(asyncio.CancelledError):
        asyncio.run(bindings.refresh())
    assert not bindings.matches("192.0.2.1", "member")

    async def scenario():
        bindings.reset()
        install()
        entered = asyncio.Event()
        async def pending():
            entered.set()
            await asyncio.Event().wait()
        monkeypatch.setattr(bindings, "_fetch", pending)
        bindings.start()
        await entered.wait()
        await bindings.stop()
        assert bindings._TASK is None
        assert not bindings.matches("192.0.2.1", "member")
    asyncio.run(scenario())


def test_total_deadline_bounds_slow_source(monkeypatch):
    install()
    monkeypatch.setattr(bindings, "_REQUEST_SECONDS", .01)
    async def slow():
        await asyncio.Event().wait()
    monkeypatch.setattr(bindings, "_fetch", slow)
    with pytest.raises(TimeoutError):
        asyncio.run(bindings.refresh())
    assert not bindings.matches("192.0.2.1", "member")


def test_disabled_mode_needs_no_snapshot_and_starts_no_worker():
    config.settings.private_peer_bindings_required = False
    config.settings.private_seeder_url = "http://seeder.example.org"
    bindings.validate_settings()
    bindings.start()
    assert bindings._TASK is None
    assert bindings.matches("192.0.2.1", "member")
    asyncio.run(bindings.refresh())
    assert bindings._SNAPSHOT is None


@pytest.mark.parametrize("field,value", [("private_indexer_enabled", False), ("private_seeder_url", "http://seeder.example.org"),
    ("private_seeder_url", "https://seeder.example.org/api"), ("private_seeder_url", "https://user:password@seeder.example.org"),
    ("private_seeder_url", "https://@seeder.example.org"), ("private_seeder_url", "https://seeder.example.org#"),
    ("private_seeder_url", "https://seeder.example.org:0"), ("private_seeder_url", "https://seeder.example.org:99999"),
    ("private_seeder_username", ""), ("private_seeder_password", "")])
def test_strict_mode_requires_authenticated_fixed_https_origin(field, value):
    setattr(config.settings, field, value)
    with pytest.raises(RuntimeError):
        bindings.validate_settings()

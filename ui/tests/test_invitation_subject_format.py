"""Explicit bridge namespaces never migrate or reactivate account aliases."""
import asyncio
import hashlib
import time

import aiosqlite
import pytest
from pydantic import ValidationError

import account_preferences
import account_usage
import config
import database
import invitation_bridge as bridge
import private_indexer as private
from test_invitation_bridge import bridge_config as _bridge_config, redemption, rpc, start_body
from test_invitations import headers, invitation_config as _invitation_config, member, mint

bridge_config = _bridge_config
invitation_config = _invitation_config


@pytest.fixture
def owned_account_rows():
    async def clear():
        db = await database.get_db()
        for table in ("account_usage", "account_usage_events", "account_usage_daily",
                      "account_preferences", "user_api_keys", "private_transfer_totals"):
            await db.execute(f"DELETE FROM {table} WHERE user_id IN (?,?)",
                             ("-42424", "principal:42424"))
        await db.commit()
    asyncio.run(clear())
    yield
    asyncio.run(clear())


def set_format(monkeypatch, value):
    monkeypatch.setattr(config.settings, "invitation_bridge_subject_format", value)
    bridge.validate_settings()


def test_default_format_is_legacy_and_policy_is_not_mutable(monkeypatch):
    monkeypatch.delenv("INVITATION_BRIDGE_SUBJECT_FORMAT", raising=False)
    assert config.Settings(_env_file=None).invitation_bridge_subject_format == "legacy-negative"
    assert "invitation_bridge_subject_format" in config.FROZEN_OVERRIDE_FIELDS
    assert "invitation_bridge_subject_format" not in config.MUTABLE_FIELDS


@pytest.mark.parametrize("value", ["", "negative", "PRINCIPAL", " principal", "principal ",
                                  "provider", "false", "0"])
def test_invalid_configured_format_fails_settings_construction(monkeypatch, value):
    monkeypatch.setenv("INVITATION_BRIDGE_SUBJECT_FORMAT", value)
    with pytest.raises(ValidationError):
        config.Settings(_env_file=None)


@pytest.mark.parametrize("principal", [True, False, 0, -1, 1.0, "1", None, 2**63])
def test_subject_helper_requires_exact_positive_bounded_integer(principal):
    for mode in ("legacy-negative", "principal"):
        with pytest.raises(ValueError):
            bridge.account_subject(principal, mode)


def test_subject_format_has_no_provider_mapping_or_fallback():
    for mode in (None, [], "", "PRINCIPAL", " provider "):
        with pytest.raises(ValueError):
            bridge.account_subject(42, mode)
    assert bridge.account_subject(2**63 - 1, "legacy-negative") == "-9223372036854775807"
    assert bridge.account_subject(2**63 - 1, "principal") == "principal:9223372036854775807"


@pytest.mark.parametrize("mode,uid", [("legacy-negative", "-12345"), ("principal", "principal:12345")])
def test_actual_redemption_recovery_and_human_membership_use_one_subject(client, monkeypatch, mode, uid):
    set_format(monkeypatch, mode)
    begin = start_body(mint(client).json()["token"])
    eid = rpc(client, begin).json()["enrollmentId"]
    body = redemption(begin, eid)
    assert rpc(client, {**body, "operation": "status"}).json()["approved"] is False
    result = rpc(client, body)
    assert result.status_code == 200 and result.json()["accountId"] == uid
    assert result.json()["alreadyRedeemed"] is False
    again = rpc(client, body)
    assert again.status_code == 200 and again.json()["alreadyRedeemed"] is True
    status = rpc(client, {**body, "operation": "status"}).json()
    assert status["accountId"] == uid and status["approved"] is True
    assert client.get("/api/account/private", headers=headers(uid)).json()["approved"] is True
    other = "principal:12345" if mode == "legacy-negative" else "-12345"
    assert client.get("/api/account/private", headers=headers(other)).json()["approved"] is False
    asyncio.run(member(uid, False))
    assert rpc(client, body).status_code == 409
    assert rpc(client, {**body, "operation": "status"}).json()["approved"] is False


@pytest.mark.parametrize("mode,other", [("principal", "-12345"), ("legacy-negative", "principal:12345")])
@pytest.mark.parametrize("state", ["active", "suspended", "revoked"])
def test_other_format_membership_or_suspension_blocks_new_consumption(client, monkeypatch, mode, other, state):
    set_format(monkeypatch, mode)
    active = state == "active"
    asyncio.run(member(other, active))
    async def annotate():
        db = await database.get_db()
        await db.execute("UPDATE private_members SET actor=? WHERE user_id=?", (state, other))
        await db.commit()
    asyncio.run(annotate())
    begin = start_body(mint(client).json()["token"])
    eid = rpc(client, begin).json()["enrollmentId"]
    body = redemption(begin, eid)
    assert rpc(client, body).status_code == 409
    assert rpc(client, {**body, "operation": "status"}).json()["approved"] is False
    async def unchanged():
        db = await database.get_db()
        rows = await db.execute_fetchall("SELECT user_id,active FROM private_members WHERE user_id IN (?,?)",
            ("-12345", "principal:12345"))
        assert [dict(row) for row in rows] == [{"user_id": other, "active": int(active)}]
        assert not await db.execute_fetchall("SELECT 1 FROM membership_invitations WHERE redeemed_at IS NOT NULL")
    asyncio.run(unchanged())


@pytest.mark.parametrize("mode,uid,other", [
    ("legacy-negative", "-12345", "principal:12345"),
    ("principal", "principal:12345", "-12345"),
])
def test_same_subject_existing_active_is_still_managed_without_consumption(client, monkeypatch, mode, uid, other):
    set_format(monkeypatch, mode)
    asyncio.run(member(uid))
    begin = start_body(mint(client).json()["token"])
    eid = rpc(client, begin).json()["enrollmentId"]
    body = redemption(begin, eid)
    result = rpc(client, body)
    assert result.status_code == 409
    async def unchanged():
        db = await database.get_db()
        assert not await db.execute_fetchall("SELECT 1 FROM membership_invitations WHERE redeemed_at IS NOT NULL")
        assert not await db.execute_fetchall("SELECT 1 FROM private_members WHERE user_id=?", (other,))
        assert (await db.execute_fetchall("SELECT active,actor FROM private_members WHERE user_id=?", (uid,)))[0]["active"] == 1
    asyncio.run(unchanged())


@pytest.mark.parametrize("mode,uid,other", [
    ("legacy-negative", "-12345", "principal:12345"),
    ("principal", "principal:12345", "-12345"),
])
def test_consumed_same_subject_recovery_does_not_rebind_other_namespace(client, monkeypatch, mode, uid, other):
    set_format(monkeypatch, mode)
    begin = start_body(mint(client).json()["token"])
    eid = rpc(client, begin).json()["enrollmentId"]
    body = redemption(begin, eid)
    assert rpc(client, body).json()["accountId"] == uid
    # An unrelated later tombstone does not rebind an already consumed flow.
    asyncio.run(member(other, False))
    assert rpc(client, body).json()["alreadyRedeemed"] is True
    status = rpc(client, {**body, "operation": "status"}).json()
    assert status["approved"] is True and status["accountId"] == uid


@pytest.mark.parametrize("principal", [True, False, 0, -1, 1.0, "1", None, 2**63])
@pytest.mark.parametrize("operation", ["redeem", "status"])
def test_principal_mode_wire_rejects_noncanonical_integer_before_consumption(client, monkeypatch, principal, operation):
    set_format(monkeypatch, "principal")
    begin = start_body(mint(client).json()["token"])
    eid = rpc(client, begin).json()["enrollmentId"]
    assert rpc(client, redemption(begin, eid, principalId=principal, operation=operation)).status_code == 422
    async def unchanged():
        db = await database.get_db()
        assert not await db.execute_fetchall("SELECT 1 FROM membership_invitations WHERE redeemed_at IS NOT NULL")
        row = (await db.execute_fetchall("SELECT principal_id,subject_format FROM invitation_bootstrap_enrollments WHERE id=?", (eid,)))[0]
        assert row["principal_id"] is None and row["subject_format"] == "principal"
    asyncio.run(unchanged())


def test_disabled_bridge_never_fetches_database_in_principal_mode(client, monkeypatch):
    set_format(monkeypatch, "principal")
    monkeypatch.setattr(config.settings, "private_invitation_bridge_enabled", False)
    bridge.validate_settings()
    async def forbidden_db():
        raise AssertionError("Disabled bridge touched the database")
    monkeypatch.setattr(bridge, "get_db", forbidden_db)
    assert rpc(client, start_body("bi_" + "a" * 43)).status_code == 404
    assert bridge.sign_in_url() == ""


def test_mid_transaction_format_change_aborts_without_membership_or_consumption(client, monkeypatch):
    begin = start_body(mint(client).json()["token"])
    eid = rpc(client, begin).json()["enrollmentId"]
    original = bridge._valid_invite
    async def change_after_admission(*args):
        row = await original(*args)
        monkeypatch.setattr(config.settings, "invitation_bridge_subject_format", "principal")
        return row
    monkeypatch.setattr(bridge, "_valid_invite", change_after_admission)
    assert rpc(client, redemption(begin, eid)).status_code == 503
    async def unchanged():
        db = await database.get_db()
        assert not await db.execute_fetchall("SELECT 1 FROM private_members WHERE user_id IN ('-12345','principal:12345')")
        assert not await db.execute_fetchall("SELECT 1 FROM membership_invitations WHERE redeemed_at IS NOT NULL")
    asyncio.run(unchanged())


def test_runtime_change_or_restart_cannot_reinterpret_pending_enrollment(client, monkeypatch):
    begin = start_body(mint(client).json()["token"])
    eid = rpc(client, begin).json()["enrollmentId"]
    body = redemption(begin, eid)
    monkeypatch.setattr(config.settings, "invitation_bridge_subject_format", "principal")
    assert rpc(client, body).status_code == 503
    assert bridge.sign_in_url() == ""
    bridge.validate_settings()  # Simulate a deliberate restart after configuration review.
    assert rpc(client, begin).status_code == 409
    assert rpc(client, body).status_code == 409
    status = rpc(client, {**body, "operation": "status"}).json()
    assert status["approved"] is False and status["accountId"] == "principal:12345"
    async def unchanged():
        db = await database.get_db()
        assert not await db.execute_fetchall("SELECT 1 FROM private_members WHERE user_id IN ('-12345','principal:12345')")
        assert not await db.execute_fetchall("SELECT 1 FROM membership_invitations WHERE redeemed_at IS NOT NULL")
        row = (await db.execute_fetchall("SELECT subject_format FROM invitation_bootstrap_enrollments WHERE id=?", (eid,)))[0]
        assert row["subject_format"] == "legacy-negative"
    asyncio.run(unchanged())


def test_restart_cannot_rebind_consumed_legacy_enrollment_or_grant_alias(client, monkeypatch):
    begin = start_body(mint(client).json()["token"])
    eid = rpc(client, begin).json()["enrollmentId"]
    body = redemption(begin, eid)
    assert rpc(client, body).json()["accountId"] == "-12345"
    set_format(monkeypatch, "principal")
    assert rpc(client, body).status_code == 409
    assert rpc(client, {**body, "operation": "status"}).json()["approved"] is False
    async def unchanged():
        db = await database.get_db()
        assert not await db.execute_fetchall("SELECT 1 FROM private_members WHERE user_id='principal:12345'")
        assert (await db.execute_fetchall("SELECT active FROM private_members WHERE user_id='-12345'"))[0]["active"] == 1
        assert (await db.execute_fetchall("SELECT redeemed_by FROM membership_invitations WHERE redeemed_at IS NOT NULL"))[0]["redeemed_by"] == "-12345"
    asyncio.run(unchanged())


def test_existing_enrollment_schema_defaults_to_legacy_without_row_rewrite(tmp_path):
    async def run():
        async with aiosqlite.connect(tmp_path / "old.db") as db:
            db.row_factory = aiosqlite.Row
            await db.execute("""CREATE TABLE invitation_bootstrap_enrollments (
                id TEXT PRIMARY KEY, invite_id TEXT NOT NULL, auth_request_id TEXT NOT NULL UNIQUE,
                browser_binding_hash TEXT NOT NULL, created_at REAL NOT NULL, expires_at REAL NOT NULL,
                principal_id INTEGER, provider TEXT, subject_binding_hash TEXT, redeemed_at REAL)""")
            values = ("a"*32, "b"*32, "c"*32, "d"*64, time.time(), time.time()+500, 42, "google", "e"*64, time.time())
            await db.execute("INSERT INTO invitation_bootstrap_enrollments VALUES (?,?,?,?,?,?,?,?,?,?)", values)
            await bridge.init_schema(db)
            await bridge.init_schema(db)
            row = (await db.execute_fetchall("SELECT * FROM invitation_bootstrap_enrollments"))[0]
            assert tuple(row)[:10] == values and row["subject_format"] == "legacy-negative"
    asyncio.run(run())


def test_canonical_human_account_never_reads_or_rotates_legacy_owned_data(client, monkeypatch, owned_account_rows):
    set_format(monkeypatch, "principal")
    async def legacy_data():
        await account_usage.record_magnet_grab("-42424", 7, "copy", "9b10902e-9edf-4ce7-a2c2-c36f8192bffc")
        await account_preferences.patch_account_preferences("-42424", {"theme": "dark"})
        await database.create_user_api_key("-42424", hashlib.sha256(b"synthetic-legacy-key").hexdigest(), "synthetic...")
        db = await database.get_db()
        await db.execute("INSERT INTO private_transfer_totals VALUES (?,?,?,?,?,?)",
                         ("-42424", "invented-release", 7, 3, 1, 2))
        await db.execute("INSERT INTO invitation_payments VALUES (?,?,?,?,?,?)",
                         ("invented-payment", "invented-event", "-42424", 5000, "USD", time.time()))
        await db.commit()
    asyncio.run(legacy_data())
    begin = start_body(mint(client).json()["token"])
    eid = rpc(client, begin).json()["enrollmentId"]
    assert rpc(client, redemption(begin, eid, principalId=42424)).json()["accountId"] == "principal:42424"
    account = client.get("/api/account", headers=headers("principal:42424")).json()
    assert account["identity"]["id"] == "principal:42424"
    assert account["usage"]["grabs"] == 0 and account["preferences"]["settings"]["theme"] == "system"
    assert account["apiKey"] is None
    conflict = client.patch("/api/account/preferences", headers=headers("principal:42424"),
        json={"expectedAccountId": "-42424", "changes": {"theme": "light"}})
    assert conflict.status_code == 409
    metrics = client.get("/api/account/private/metrics", headers=headers("principal:42424")).json()
    assert metrics["totals"]["uploaded"] == 0 and metrics["totals"]["downloaded"] == 0
    invitation = client.get("/api/account/invitations", headers=headers("principal:42424")).json()
    assert invitation["annualUsed"] == 0 and invitation["paidCredits"] == 0
    async def unchanged():
        assert (await account_usage.get_account_usage("-42424"))["grabs"] == 7
        assert (await account_preferences.get_account_preferences("-42424"))["settings"]["theme"] == "dark"
        assert await database.get_user_api_key("-42424") is not None
        legacy_metrics = await private._metrics("-42424")
        assert legacy_metrics["totals"]["uploaded"] == 7 and legacy_metrics["totals"]["downloaded"] == 3
        db = await database.get_db()
        assert (await db.execute_fetchall("SELECT user_id FROM invitation_payments WHERE payment_id=?",
                                        ("invented-payment",)))[0]["user_id"] == "-42424"
    asyncio.run(unchanged())

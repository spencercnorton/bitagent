"""Sparse, account-scoped preference updates; no secrets or search history."""
from __future__ import annotations

import json
import time
from typing import Literal

from pydantic import BaseModel, ConfigDict, Field, StrictBool, StrictStr, field_validator

from account_usage import account_write, personal_account, utc_timestamp
from database import read_conn

DEFAULT_SETTINGS = {
    "theme": "system", "motion": "system", "density": "comfortable", "region": "US",
    "contentType": "", "sort": "seeders", "quality": "", "matchedOnly": True,
    "englishOnly": False, "magnetMode": "best",
}


class PreferenceChanges(BaseModel):
    model_config = ConfigDict(extra="forbid")
    theme: Literal["system", "light", "dark"] | None = None
    motion: Literal["system", "reduced"] | None = None
    density: Literal["comfortable", "compact"] | None = None
    region: StrictStr | None = Field(default=None, pattern=r"^[A-Z]{2}$")
    contentType: Literal["", "movie", "tv_show"] | None = None
    sort: Literal["seeders", "newest", "name", "size"] | None = None
    quality: Literal["", "V2160p", "V1080p", "V720p"] | None = None
    matchedOnly: StrictBool | None = None
    englishOnly: StrictBool | None = None
    magnetMode: Literal["best", "all"] | None = None

    @field_validator("*", mode="before")
    @classmethod
    def reject_null(cls, value):
        if value is None:
            raise ValueError("Preference values cannot be null")
        return value


def _stored_settings(row) -> dict:
    stored = json.loads(row["settings"]) if row else {}
    validated = PreferenceChanges.model_validate(stored).model_dump(exclude_unset=True)
    return {**DEFAULT_SETTINGS, **validated}


def _payload(user_id: str, row=None) -> dict:
    return {
        "schemaVersion": 1, "accountId": user_id, "available": personal_account(user_id),
        "revision": row["revision"] if row else 0,
        "updatedAt": utc_timestamp(row["updated_at"]) if row and row["updated_at"] is not None else None,
        "settings": _stored_settings(row),
    }


async def get_account_preferences(user_id: str) -> dict:
    if not personal_account(user_id):
        return _payload(user_id)
    async with read_conn() as db:
        rows = await db.execute_fetchall("SELECT * FROM account_preferences WHERE user_id=?", (user_id,))
    return _payload(user_id, rows[0] if rows else None)


async def patch_account_preferences(user_id: str, changes: dict) -> dict:
    if not personal_account(user_id):
        raise ValueError("A named authenticated account is required")
    changes = PreferenceChanges.model_validate(changes).model_dump(exclude_unset=True)
    async with account_write() as db:
        rows = await db.execute_fetchall("SELECT * FROM account_preferences WHERE user_id=?", (user_id,))
        prior = rows[0] if rows else None
        previous_settings = _stored_settings(prior)
        merged = {**previous_settings, **changes}
        if merged == previous_settings:
            return _payload(user_id, prior)
        await db.execute(
            "INSERT INTO account_preferences (user_id,revision,updated_at,settings) VALUES (?,?,?,?) "
            "ON CONFLICT(user_id) DO UPDATE SET revision=excluded.revision,updated_at=excluded.updated_at,settings=excluded.settings",
            (user_id, prior["revision"] + 1 if prior else 1, time.time(), json.dumps(merged, sort_keys=True)),
        )
        row = (await db.execute_fetchall("SELECT * FROM account_preferences WHERE user_id=?", (user_id,)))[0]
        return _payload(user_id, row)

"""Infisical secret hydration for an explicitly configured installation.

If ``INFISICAL_CLIENT_ID`` / ``INFISICAL_CLIENT_SECRET`` are present (a
Universal-Auth machine identity), fetch every secret at the configured path and
override the matching ``Settings`` field. The operator then manages a key in
exactly one place — Infisical — instead of retaining a stale or mistyped
environment literal. Set the intended origin, project, environment and path
explicitly; legacy fallback values are installation-specific.

Fails OPEN: if Infisical is unconfigured or unreachable, the existing
env-derived values are kept untouched. The application's later auth/Host
validation still rejects unsafe or incomplete effective settings. Secret
VALUES are never logged — only field names and the configured location.
"""
from __future__ import annotations

import logging
import os

import httpx

# uvicorn.error is reliably visible in container logs at INFO level.
log = logging.getLogger("uvicorn.error")


def hydrate_settings(settings) -> list[str]:
    """Override matching ``settings`` fields with secrets from Infisical.

    Returns the list of field names that were hydrated (empty when Infisical is
    not configured or unreachable).
    """
    client_id = os.environ.get("INFISICAL_CLIENT_ID")
    client_secret = os.environ.get("INFISICAL_CLIENT_SECRET")
    if not (client_id and client_secret):
        return []

    base = os.environ.get("INFISICAL_URL", "").rstrip("/")
    project_id = os.environ.get("INFISICAL_PROJECT_ID", "")
    environment = os.environ.get("INFISICAL_ENV", "")
    secret_path = os.environ.get("INFISICAL_PATH", "/bitagent")
    if not (base and project_id and environment):
        log.warning("infisical: INFISICAL_URL, INFISICAL_PROJECT_ID and INFISICAL_ENV are all required — skipping hydration")
        return []

    applied: list[str] = []
    try:
        with httpx.Client(timeout=6.0) as client:
            login = client.post(
                f"{base}/api/v1/auth/universal-auth/login",
                json={"clientId": client_id, "clientSecret": client_secret},
            )
            login.raise_for_status()
            token = login.json()["accessToken"]

            resp = client.get(
                f"{base}/api/v3/secrets/raw",
                params={
                    "workspaceId": project_id,
                    "environment": environment,
                    "secretPath": secret_path,
                },
                headers={"Authorization": f"Bearer {token}"},
            )
            resp.raise_for_status()
            for secret in resp.json().get("secrets", []):
                field = secret.get("secretKey", "").lower()
                value = secret.get("secretValue", "")
                if field and value and hasattr(settings, field):
                    setattr(settings, field, value)
                    applied.append(field)
    except Exception as exc:  # noqa: BLE001 — fail open on any error
        log.warning(
            "infisical: hydration failed (%s) — keeping env values",
            exc.__class__.__name__,
        )
        return []

    if applied:
        log.info(
            "infisical: hydrated %d setting(s) from %s%s -> %s",
            len(applied), environment, secret_path, ", ".join(sorted(applied)),
        )
    else:
        log.info(
            "infisical: connected but no matching secrets at %s%s",
            environment, secret_path,
        )
    return applied

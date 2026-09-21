"""Host-routing + auth-gate dependencies, factored out of app.py.

These sit between the request and every route: they classify the Host header
into a surface (public library vs operator console vs 421 for anything else),
and the operator/public gates layer identity on top. Kept in their own module —
depending only on config + auth + fastapi — so route modules can import the
gates without importing app.py (which would be a circular import).
"""
from __future__ import annotations

import re as _re

from fastapi import Request, HTTPException, Depends

from config import settings
from auth import require_auth, resolve_identity


def _normalise_hostname(raw: str) -> str | None:
    """Return a canonical host without a port, rejecting malformed/wildcard input."""
    value = (raw or "").strip().lower()
    if not value or any(ch.isspace() for ch in value) or any(ch in value for ch in "/\\@,#?"):
        return None

    # Deployment hostnames are DNS names/IPv4. Reject unbracketed IPv6 rather
    # than parsing it ambiguously as host:port; no current BitAgent surface uses
    # an IPv6-literal Host.
    if value.count(":") == 1:
        host, port = value.rsplit(":", 1)
        if not port.isdigit():
            return None
        value = host
    elif ":" in value:
        return None

    value = value.rstrip(".")
    if not value or len(value) > 253 or "*" in value:
        return None
    labels = value.split(".")
    if any(
        not label
        or len(label) > 63
        or not _re.fullmatch(r"[a-z0-9](?:[a-z0-9-]*[a-z0-9])?", label)
        for label in labels
    ):
        return None
    return value


def _configured_hosts(raw: str, setting_name: str) -> frozenset[str]:
    hosts: set[str] = set()
    for item in (raw or "").split(","):
        if not item.strip():
            continue
        host = _normalise_hostname(item)
        if host is None:
            raise RuntimeError(f"{setting_name} contains an invalid hostname")
        hosts.add(host)
    return frozenset(hosts)


def _host_sets() -> tuple[frozenset[str], frozenset[str]]:
    return (
        _configured_hosts(settings.public_library_hosts, "PUBLIC_LIBRARY_HOSTS"),
        _configured_hosts(settings.operator_hosts, "OPERATOR_HOSTS"),
    )


def _validate_host_settings() -> None:
    public_hosts, operator_hosts = _host_sets()
    if not public_hosts:
        raise RuntimeError("PUBLIC_LIBRARY_HOSTS must contain at least one host")
    if not operator_hosts:
        raise RuntimeError("OPERATOR_HOSTS must contain at least one host")
    if public_hosts & operator_hosts:
        raise RuntimeError("PUBLIC_LIBRARY_HOSTS and OPERATOR_HOSTS must not overlap")


def _host_scope(request: Request) -> str | None:
    host_headers = request.headers.getlist("host")
    if len(host_headers) != 1:
        return None
    host = _normalise_hostname(host_headers[0])
    if host is None:
        return None
    public_hosts, operator_hosts = _host_sets()
    if host in public_hosts:
        return "public"
    if host in operator_hosts:
        return "operator"
    return None


def require_operator(request: Request) -> dict:
    """Operator-only gate for the operator-console surface.

    Host selects a surface; it does not grant authority. Public hosts hide these
    routes, unknown hosts fail closed, and the operator host still requires an
    identity carrying an explicit operator grant from a verified auth tier.
    """
    scope = _host_scope(request)
    if scope == "public":
        # Don't reveal that the endpoint exists on the public surface.
        raise HTTPException(status_code=404, detail="Not found")
    if scope != "operator":
        raise HTTPException(status_code=421, detail="Misdirected request")
    identity = resolve_identity(request)
    if not identity.get("operator"):
        raise HTTPException(status_code=403, detail="Operator grant required")
    return identity


def require_public_library(
    request: Request,
    identity: dict = Depends(require_auth),
) -> dict:
    """Public-library-only gate for endpoints that do not belong to console."""
    scope = _host_scope(request)
    if scope == "operator":
        raise HTTPException(status_code=404, detail="Not found")
    if scope != "public":
        raise HTTPException(status_code=421, detail="Misdirected request")
    return identity

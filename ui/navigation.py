"""Links between the two existing, independently authorized UI surfaces."""
from __future__ import annotations

import ipaddress
import socket
from urllib.parse import unquote, urlsplit

from fastapi import Request

from config import settings
from deps import _configured_hosts, _host_scope, _normalise_hostname


def _validated_url(raw: str, hosts: str, setting_name: str, host_setting: str) -> str:
    if not raw:
        return ""
    # URL parsers can silently discard control characters. Validate before
    # parsing, including encoded controls/backslashes, and never echo a bad
    # URL (which might contain credentials) in a startup error.
    decoded = unquote(raw)
    if (
        any(ch.isspace() for ch in raw)
        or any(ord(ch) < 32 or 127 <= ord(ch) <= 159 for ch in decoded)
        or "\\" in decoded
        or "?" in raw
        or "#" in raw
    ):
        raise RuntimeError(f"{setting_name} must be a plain absolute http(s) URL without query or fragment")
    try:
        parts = urlsplit(raw)
        port = parts.port
        host = _normalise_hostname(parts.netloc)
        valid = (
            parts.scheme in {"http", "https"}
            and host is not None
            and parts.username is None
            and parts.password is None
            and (port is None or 1 <= port <= 65535)
            and not parts.query
            and not parts.fragment
        )
    except ValueError:
        valid = False
        host = None
    if not valid:
        raise RuntimeError(f"{setting_name} must be an absolute http(s) URL without userinfo, query, or fragment")
    if host not in _configured_hosts(hosts, host_setting):
        raise RuntimeError(f"{setting_name} must name a host in {host_setting}")
    return raw


def validate_navigation_settings() -> None:
    _validated_url(settings.operator_ui_url, settings.operator_hosts, "OPERATOR_UI_URL", "OPERATOR_HOSTS")
    _validated_url(settings.library_ui_url, settings.public_library_hosts, "LIBRARY_UI_URL", "PUBLIC_LIBRARY_HOSTS")


def _local_host(host: str) -> bool:
    if host == "localhost" or host.endswith(".localhost"):
        return True
    try:
        return ipaddress.ip_address(host).is_loopback
    except ValueError:
        # Browsers also recognize legacy IPv4 spellings such as 127.1 and
        # 2130706433. inet_aton parses these locally without a DNS lookup.
        try:
            return ipaddress.ip_address(socket.inet_aton(host)).is_loopback
        except OSError:
            return False


def _ordered_hosts(raw: str) -> list[str]:
    # The routing gate treats its allowlists as sets. Navigation alone retains
    # their configured order so a fallback is deterministic.
    return list(dict.fromkeys(
        host for item in raw.split(",") if (host := _normalise_hostname(item))
    ))


def _fallback_url(request: Request, target_hosts: str) -> str:
    targets = _ordered_hosts(target_hosts)
    source = _normalise_hostname(request.headers.get("host", ""))
    if not settings.require_auth and source and _local_host(source):
        local_target = next((host for host in targets if _local_host(host)), None)
        if local_target:
            # Only open local development copies a port. A production proxy's
            # listener/upstream port says nothing about the other site's port.
            raw_host = request.headers.get("host", "")
            port_suffix = ""
            if ":" in raw_host:
                raw_port = raw_host.rsplit(":", 1)[1]
                if not raw_port.isdigit() or not 1 <= int(raw_port) <= 65535:
                    return ""
                port_suffix = f":{int(raw_port)}"
            return f"//{local_target}{port_suffix}/"
    target = next((host for host in targets if not _local_host(host)), None)
    return f"//{target}/" if target else ""


def operator_url(request: Request, identity: dict) -> str:
    if not identity.get("operator"):
        return ""
    scope = _host_scope(request)
    if scope == "operator":
        return "/"
    if scope != "public":
        return ""
    return (
        _validated_url(settings.operator_ui_url, settings.operator_hosts, "OPERATOR_UI_URL", "OPERATOR_HOSTS")
        or _fallback_url(request, settings.operator_hosts)
    )


def library_url(request: Request) -> str:
    return (
        _validated_url(settings.library_ui_url, settings.public_library_hosts, "LIBRARY_UI_URL", "PUBLIC_LIBRARY_HOSTS")
        or _fallback_url(request, settings.public_library_hosts)
        or "/library"
    )

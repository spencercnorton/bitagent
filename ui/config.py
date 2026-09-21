from __future__ import annotations
from pathlib import Path
from pydantic_settings import BaseSettings

DATA_DIR = Path("/data") if Path("/data").exists() else Path(__file__).parent / "data"
DATA_DIR.mkdir(exist_ok=True)


class Settings(BaseSettings):
    bitagent_graphql_url: str = "http://localhost:3333/graphql"
    bitagent_torznab_url: str = ""
    bitagent_metrics_url: str = "http://localhost:3333/metrics"
    require_auth: bool = True
    # Both HTTP surfaces are explicit allowlists. Requests for every other Host
    # are rejected before routing; a typo must never fall through to operator.
    # Values are comma-separated, case-insensitive, and compared without ports.
    # The defaults are local-development names only; every deployment sets both
    # lists explicitly (they must be non-empty and disjoint or startup fails).
    public_library_hosts: str = "library.localhost"
    operator_hosts: str = "localhost,127.0.0.1"
    # Optional cross-origin script for a shared app switcher on both shells
    # (`data-current="bitagent" data-me="/api/me"`). Empty = no tag and no
    # third-party origin in the CSP script-src.
    app_switcher_script_url: str = ""
    # Name the public library carries in its page title and web-app manifest.
    library_brand: str = "BitAgent"
    dashboard_api_key: str = ""
    torznab_api_key: str = ""
    torznab_rate_limit_per_min: int = 120
    # Proxy identity is accepted only from these transport-peer CIDRs. Uvicorn
    # proxy-header rewriting must stay disabled so Request.client is the peer,
    # not a client-controlled X-Forwarded-For value.
    trusted_proxy_cidrs: str = ""
    # Required second factor when either forwarded-identity tier is enabled.
    # The trusted proxy must overwrite X-BitAgent-Proxy-Proof on every request.
    proxy_auth_secret: str = ""
    trust_npm_headers: bool = False
    trust_forwarded_user: bool = False
    # Verified X-Auth-Priv values that grant the operator surface. Host alone is
    # never authorization. The dashboard API key and REQUIRE_AUTH=false dev mode
    # are explicit non-proxy operator grants.
    operator_roles: str = "OWNER"
    sso_cookie_name: str = "bitagent_session"
    tmdb_api_key: str = ""
    log_level: str = "info"
    # Monthly OpenRouter/provider budget the AI tab measures spend against.
    # Startup-only: it mirrors a cap that is actually set at the provider, so
    # it is deployment config, not a knob an operator flips in the browser.
    llm_monthly_budget_usd: float = 10.0
    db_path: str = str(DATA_DIR / "bitagent-ui.db")
    host: str = "0.0.0.0"
    port: int = 8080
    sonarr_base_url: str = ""
    sonarr_api_key: str = ""
    radarr_base_url: str = ""
    radarr_api_key: str = ""
    lidarr_base_url: str = ""
    lidarr_api_key: str = ""

    # Infisical hydrates selected fields after construction; assignment
    # validation preserves bool/int types instead of leaving security flags as
    # truthy strings such as "false".
    model_config = {
        "env_prefix": "", "env_file": ".env", "extra": "ignore",
        "validate_assignment": True,
    }


settings = Settings()

# These values are credentials. They may be changed through the operator API,
# but their plaintext must never be returned by a settings or audit response.
SENSITIVE_FIELDS: frozenset[str] = frozenset({
    "tmdb_api_key",
    "torznab_api_key",
    "sonarr_api_key",
    "radarr_api_key",
    "lidarr_api_key",
})


MUTABLE_FIELDS: set[str] = {
    "tmdb_api_key",
    "log_level",
    "torznab_api_key",
    "sonarr_base_url",
    "sonarr_api_key",
    "radarr_base_url",
    "radarr_api_key",
    "lidarr_base_url",
    "lidarr_api_key",
}

# Mutable *arr base URLs receive a forwarded API key on every fetch, so they are
# an SSRF surface and get resolved-address validation before use (see app.py
# _assert_safe_arr_base_url). Derived from MUTABLE_FIELDS so adding a new *arr
# never silently skips the guard.
ARR_BASE_URL_FIELDS: frozenset[str] = frozenset(
    f for f in MUTABLE_FIELDS if f.endswith("_base_url")
)

# Database overrides are deny-by-default. Every declared setting that is not
# deliberately exposed through MUTABLE_FIELDS is startup-only, including core
# upstream destinations and the complete host/proxy/auth trust boundary. This
# set also drives the startup migration that removes rows written by older
# builds before those fields were frozen.
FROZEN_OVERRIDE_FIELDS: frozenset[str] = (
    frozenset(Settings.model_fields) - frozenset(MUTABLE_FIELDS)
)

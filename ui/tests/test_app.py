"""App wiring: version single-sourcing + OpenAPI surface + asset cache-busting."""
import re

import app as app_module
import config
import version


def test_app_version_is_single_sourced():
    # The contract is that the running app, version.py, and pyproject all agree.
    # version.py is the single source; asserting a hardcoded literal here would
    # itself be a second source (and breaks on every release bump), so we don't.
    assert app_module.app.version == version.__version__


def test_openapi_reports_the_single_version(client):
    r = client.get("/openapi.json")
    assert r.status_code == 200
    assert r.json()["info"]["version"] == version.__version__


def test_asset_version_is_a_content_hash():
    """The cache-bust token is a non-trivial hex hash, not the stale literal."""
    av = app_module.ASSET_VERSION
    assert re.fullmatch(r"[0-9a-f]{12}", av)
    assert av != "20260501"
    # Deterministic: recomputing from the same bytes yields the same token.
    assert app_module._compute_asset_version() == av


def test_dashboard_html_stamps_assets_with_the_asset_version(client):
    """Served HTML must reference assets with the dynamic ?v= token so a deploy
    that changes the bundle forces returning browsers to re-fetch."""
    config.settings.require_auth = False
    r = client.get("/")
    assert r.status_code == 200
    av = app_module.ASSET_VERSION
    assert f"/static/js/app.js?v={av}" in r.text
    assert f"/static/css/app.css?v={av}" in r.text
    assert "?v=20260501" not in r.text


def test_operator_copy_does_not_mislabel_counts_or_torznab_health(client):
    config.settings.require_auth = False
    html = client.get("/").text
    javascript = (app_module.BASE / "static/js/app.js").read_text()

    # The DHT concurrency gauge was never a peer count; the card is gone but
    # the mislabel must not come back with it.
    assert "DHT Peers" not in html
    assert "DHT Requests In Flight" not in html

    # torrentContent.totalCount is a Postgres planner estimate at this scale.
    # The card must say so and must not render a to-the-digit figure.
    assert "planner" in html.lower()
    assert "totalTorrentsIsEstimate" in javascript
    assert "function fmtApproxNum(n)" in javascript

    # Grab success excludes unresolved attempts from the ratio, so the pending
    # count has to be shown rather than silently dropped.
    assert "grabPendingCount" in javascript

    assert "GraphQL success does not prove the Torznab route" in html
    assert "Not probed" in javascript
    assert "setPill('sysTz', !!core.graphql" not in javascript


def test_dashboard_health_card_is_uptime_only(client):
    """Per-endpoint reachability belongs to System -> Health. The dashboard
    card keeps uptime and nothing else, and the JS must not keep painting
    pills into elements that no longer exist."""
    config.settings.require_auth = False
    html = client.get("/").text
    javascript = (app_module.BASE / "static/js/app.js").read_text()

    assert 'id="statUptime"' in html
    for gone in ('id="gqlStatus"', 'id="metricsStatus"', 'id="dashApiStatus"',
                 'id="coreHealthPill"', 'id="statLastCrawl"'):
        assert gone not in html, gone
    assert "_paintHealthPills" not in javascript
    # The System tab keeps its own pills.
    assert 'id="sysGql"' in html
    assert 'id="sysMetrics"' in html


def test_operator_telemetry_js_guards_stale_responses_and_titles():
    javascript = (app_module.BASE / "static/js/app.js").read_text()

    assert "let _dashboardLoadSeq = 0;" in javascript
    assert "const loadSeq = ++_dashboardLoadSeq;" in javascript
    assert javascript.count("if (loadSeq !== _dashboardLoadSeq) return;") >= 3
    assert "loadRecentActivity(loadSeq);" in javascript
    assert "loadWinRate(loadSeq);" in javascript
    assert "else if (el) {\n    el.removeAttribute('title');" in javascript
    assert "function fmtRatePerMin(n)" in javascript
    assert "if (value > 0 && value < 1) return '<1';" in javascript
    assert "indexerThroughput', fmtRatePerMin" in javascript
    # A rate whose baseline is still filling must read as measuring, not as an
    # em-dash — the em-dash means "this source will never provide it".
    assert "'measuring…'" in javascript


def test_operator_requests_are_bounded_and_ai_modes_are_rendered():
    """A wedged fetch must become a visible failure before the 30s refresh can
    overlap it, and stage cards must render the backend's observed mode rather
    than reduce every quiet model to the same 'idle or disabled' sentence."""
    javascript = (app_module.BASE / "static/js/app.js").read_text()
    html = (app_module.BASE / "templates/index.html").read_text()

    assert "const API_TIMEOUT_MS = 15_000;" in javascript
    assert "const controller = new AbortController();" in javascript
    assert "setTimeout(() => controller.abort(), timeoutMs)" in javascript
    assert "clearTimeout(timeoutId)" in javascript
    # The known-slow full-corpus operator search keeps its backend's 30s budget
    # while every ordinary telemetry request remains below the refresh period.
    assert javascript.count("{ timeoutMs: 35_000 }") >= 3

    assert "let _aiLoadSeq = 0;" in javascript
    assert "if (loadSeq !== _aiLoadSeq) return;" in javascript
    assert "stage.status" in javascript
    assert "data.wouldDrops" in javascript
    assert "LLM not observed" in javascript
    assert "idle or disabled" not in javascript
    assert 'id="aiUnavailableNote" class="callout' in html
    assert 'id="filterNsfwCountLabel"' in html

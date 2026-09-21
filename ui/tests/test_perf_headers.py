"""P3: security headers, torznab rate limit, poster-miss pixel (v1.24.0)."""
from __future__ import annotations

import config
import torznab


def test_security_headers_present(client):
    r = client.get("/healthz")
    assert r.headers.get("X-Content-Type-Options") == "nosniff"
    assert r.headers.get("X-Frame-Options") == "DENY"
    assert r.headers.get("Referrer-Policy") == "strict-origin-when-cross-origin"
    csp = r.headers.get("Content-Security-Policy", "")
    assert "frame-ancestors 'none'" in csp
    # posters/backdrops resolve to image.tmdb.org, so img-src must allow it
    assert "image.tmdb.org" in csp
    # No third-party script origin unless an app switcher is configured.
    assert "script-src 'self' 'unsafe-inline'; " in csp


def test_app_switcher_origin_is_allowed_and_tag_rendered(client):
    """A configured cross-origin app switcher must be allowed by script-src or
    the widget is blocked outright and its mount renders as an empty div — a
    silent failure with no console error the user would ever see."""
    import config
    config.settings.app_switcher_script_url = "https://switcher.example.org/switcher.js"
    config.settings.require_auth = False
    csp = client.get("/healthz").headers.get("Content-Security-Policy", "")
    assert "script-src 'self' 'unsafe-inline' https://switcher.example.org; " in csp
    page = client.get("/", headers={"host": "console.example.org"}).text
    assert 'src="https://switcher.example.org/switcher.js"' in page
    config.settings.app_switcher_script_url = ""
    assert 'switcher.example.org' not in client.get("/", headers={"host": "console.example.org"}).text


def test_dynamic_responses_are_no_store(client):
    # Authed/dynamic responses (settings, account, api-key mint) must never be
    # cached; /healthz stands in for the non-static default.
    r = client.get("/healthz")
    assert r.headers.get("Cache-Control") == "no-store"


def test_versioned_static_assets_are_immutable(client):
    # Assets are served with ?v=<content-hash>, so a long immutable TTL is safe.
    r = client.get("/static/js/app.js")
    assert r.status_code == 200
    cc = r.headers.get("Cache-Control", "")
    assert "immutable" in cc and "max-age=31536000" in cc


def test_torznab_rate_bucket_trips_then_recovers(monkeypatch):
    # Exercise the bucket in isolation (the full proxy needs a stored user key).
    monkeypatch.setattr(config.settings, "torznab_rate_limit_per_min", 2)
    torznab._TZ_BUCKETS.clear()
    ok1, _ = torznab._torznab_rate_ok("k")
    ok2, _ = torznab._torznab_rate_ok("k")
    ok3, retry = torznab._torznab_rate_ok("k")
    assert ok1 and ok2, "first requests within budget must pass"
    assert not ok3, "third request should trip the 2/min budget"
    assert retry >= 1, "a Retry-After hint should be returned"


def test_torznab_rate_disabled_when_zero(monkeypatch):
    monkeypatch.setattr(config.settings, "torznab_rate_limit_per_min", 0)
    torznab._TZ_BUCKETS.clear()
    assert all(torznab._torznab_rate_ok("z")[0] for _ in range(50))


def _csp_directives(csp: str) -> dict:
    """`"default-src 'self'; img-src 'self' https://x"` -> {name: {sources}}."""
    out = {}
    for part in csp.split(";"):
        tokens = part.split()
        if tokens:
            out[tokens[0].lower()] = set(tokens[1:])
    return out


def _csp_allows(directives: dict, name: str, origin: str) -> bool:
    """Whether `origin` is permitted by `name`, falling back to default-src.

    Fetch directives that are absent fall back to default-src; that fallback is
    the whole reason a directive-blind check is wrong, because a host listed
    under img-src does not fall back to anything for style-src.
    """
    sources = directives.get(name)
    if sources is None:
        sources = directives.get("default-src", set())
    return origin in sources


def test_shipped_css_requests_no_host_the_csp_forbids(client):
    """A stylesheet may not reference a remote origin the CSP blocks IT from.

    `app.css` opened with `@import url(https://fonts.googleapis.com/...)` for
    Inter + JetBrains Mono while the CSP has always been `style-src 'self'` and
    `font-src 'self'`. Both the stylesheet AND its font files were blocked, so
    the faces never loaded and every page load paid for a render-blocking
    request that could not succeed — silently, because a blocked subresource is
    a console warning, not an error the app sees.

    The check is per-directive on purpose. Collecting every host that appears
    anywhere in the CSP and calling it "allowed" would pass an
    `@import url(https://image.tmdb.org/...)` — that host is real, but it is
    listed under `img-src`, and `style-src 'self'` still blocks the import.
    So each reference is matched to the directive that actually governs it:
    `@import` -> style-src, `url()` inside `@font-face` -> font-src, any other
    `url()` -> img-src, each with the default-src fallback.

    Each reference is also classified EXACTLY ONCE. Imports and @font-face URLs
    are collected into sets and skipped by the generic image pass; matching them
    by reconstructing a CSS substring silently mis-classified every quoted
    import as an image too, which double-reports today and would spuriously
    fail a stylesheet host that style-src legitimately allowed.

    Comments are stripped first: a doc comment naming a URL is not a request,
    and scanning it would both mask real references and fail spuriously on
    prose (this file's own tokens.css comment discusses fonts.googleapis.com).
    """
    import pathlib
    import re

    csp = client.get("/healthz").headers.get("Content-Security-Policy", "")
    directives = _csp_directives(csp)
    assert directives, "no CSP on the response — the guard would be vacuous"

    def origin_of(url: str) -> str:
        return "/".join(url.split("/")[:3])          # https://host[:port]

    url_re = re.compile(r"url\(\s*['\"]?(https://[^'\")\s]+)")
    css_dir = pathlib.Path(__file__).resolve().parent.parent / "static" / "css"
    sheets = sorted(css_dir.glob("*.css"))
    assert sheets, "no stylesheets found — check the path"

    offenders = []
    for sheet in sheets:
        css = re.sub(r"/\*.*?\*/", "", sheet.read_text(), flags=re.S)

        # @import pulls a STYLESHEET -> style-src. Collect the URLs into a set
        # rather than reconstructing a substring to skip later: the obvious
        # `f"@import url({url}" in css` test fails on a QUOTED import, so every
        # real import fell through to the image pass and was judged twice.
        import_urls = set()
        for m in re.finditer(r"@import\s+url\(\s*['\"]?(https://[^'\")\s]+)", css):
            import_urls.add(m.group(1))
            if not _csp_allows(directives, "style-src", origin_of(m.group(1))):
                offenders.append(
                    f"{sheet.name}: style-src blocks @import {m.group(1)}"
                )

        # url() inside @font-face pulls a FONT -> font-src. Everything else is
        # an image (background-image, mask, cursor) -> img-src.
        font_urls = set()
        for block in re.finditer(r"@font-face\s*\{(.*?)\}", css, re.S):
            for m in url_re.finditer(block.group(1)):
                font_urls.add(m.group(1))
                if not _csp_allows(directives, "font-src", origin_of(m.group(1))):
                    offenders.append(
                        f"{sheet.name}: font-src blocks @font-face {m.group(1)}"
                    )
        for m in url_re.finditer(css):
            url = m.group(1)
            if url in font_urls or url in import_urls:
                continue
            if not _csp_allows(directives, "img-src", origin_of(url)):
                offenders.append(f"{sheet.name}: img-src blocks url({url})")

    assert not offenders, "; ".join(offenders) + f" | CSP: {csp}"

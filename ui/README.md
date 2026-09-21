# BitAgent UI

The operator console and public library for [BitAgent](../README.md), shipped
inside the BitAgent image and run by the Go core as the worker `ui`. It is off
by default — `UI_ENABLED=true` starts it with the other workers, and with that
flag `bitagent worker run --keys ui` runs it alone — and the core supervises it: starts the
uvicorn child, restarts it with backoff if it dies, stops it on shutdown and
folds its stdout/stderr into the core's own log stream under the logger name `ui`. The
core-side keys (`UI_ENABLED`, `UI_LISTEN_ADDRESS`, `UI_DIR`, `UI_PYTHON`,
`UI_RESTART_BACKOFF`) are in
[`docs/configuration.md`](../docs/configuration.md); everything below is what
the app itself reads.

One small FastAPI app, two surfaces selected by hostname:

- **Operator console** — health, crawl throughput, the `*arr` want-bridge, evidence
  feeds, the junk-purge quarantine, LLM stage scorecards with measured spend, and a
  read-only view of the configuration the running container actually sees.
- **Public library** — a poster grid with search and facets, rich title pages, and
  a per-user account panel for personal Torznab API keys.

It reads the crawler over its GraphQL and Prometheus endpoints — `127.0.0.1:3333`
when co-located, which is the shipped default — and never writes to the
crawler's corpus. Its own state — settings overrides, block phrases,
notifications, hashed user API keys — lives in one SQLite file under `/data`;
mount a volume there.

![The operator dashboard: indexer win rate, match rate, grab liveness, crawl throughput, category breakdown and recent activity](docs/screenshots/dashboard.png)

## Running it

The quickstart stack, [`examples/docker-compose.public.yml`](../examples/docker-compose.public.yml),
turns the worker on with `REQUIRE_AUTH=false` and binds it to
`http://localhost:8080` (console) and `http://library.localhost:8080` (library).
`REQUIRE_AUTH=false` is the development bypass. Never set it on anything
reachable by someone else — see *Authentication* below for how a real
deployment identifies users.

For development, run it standalone from this directory against a core you
already have (or the demo core, below), with Python 3.12:

```bash
cd ui
python3.12 -m venv .venv && source .venv/bin/activate
python -m pip install --require-hashes -r requirements.lock
REQUIRE_AUTH=false \
  OPERATOR_HOSTS=localhost,127.0.0.1 PUBLIC_LIBRARY_HOSTS=library.localhost \
  BITAGENT_GRAPHQL_URL=http://localhost:3333/graphql \
  BITAGENT_METRICS_URL=http://localhost:3333/metrics \
  python -m uvicorn app:app --no-proxy-headers --reload --port 8080
# console: http://localhost:8080   library: http://library.localhost:8080
```

Keep `--no-proxy-headers`: the proxy auth tiers check the real transport peer,
and this is exactly the command the core spawns (minus `--reload`).

## What the console shows

| Tab | What it is |
|---|---|
| **Dashboard** | The north-star **Indexer Win Rate (30d)**, then **Match Rate**, **Grab Liveness** (whether the swarm was alive, not whether the \*arr grab succeeded), **Crawl Throughput**, **Indexed Torrents**, and **Dead Blocked**; a category breakdown, uptime, and the recent-activity feed. Every headline is a source-aware metric: an unavailable value renders as *unknown*, never as zero, and a rate whose baseline is still filling says `measuring…`. |
| **Library** | Sortable, searchable table of recently crawled torrents with posters from TMDB. |
| **Wants** | The crawler's *wantbridge*: what Sonarr/Radarr/Lidarr are still missing, exact matches found, fingerprint keys and poll health — reported as observations, never as inferences about configuration the console cannot see. |
| **Evidence** | Per-source received/persisted counters first, then the raw evidence stream. |
| **Quarantine** | The junk-purge hold: days left, **Restore** (re-insert and re-queue a rematch) or **Delete now**, and **Spot-check 25** — a random offset, the honest way to sample a queue nobody will read through. |
| **AI** | One scorecard per LLM stage the crawler runs (TMDB matcher, content filter, junk judge) on shared axes: model, throughput, latency, a quality *proxy* (there is no ground truth, so nothing is called accuracy), and a spend row. Money is never guessed: an unpriced model makes the total a floor (`≥ $x`), and an idle stage is *still measuring*, not free. |
| **Settings** | Read-only: the configuration the running container sees, split into Configuration / Auth & Security / Integrations / Retention / Classifier / Liveness / Filters / Block Lists / Audit Log. Secrets render as `🔒 •••••• <n> chars`. |
| **System** | Per-endpoint reachability and the process view. |

Every explainable thing — stat cards, section headers, table columns — carries a
small ⓘ with a plain-English, code-grounded explanation of what it measures and
where the number comes from.

![The AI tab: projected monthly spend against the budget, tokens, spend by model, and one scorecard per LLM stage](docs/screenshots/ai.png)

![The public library: poster grid with search and facets](docs/screenshots/library.png)

## Configuration

Settings are environment variables named after the fields in [`config.py`](config.py)
(pydantic-settings, no prefix). The ones that matter:

| Variable | Default | Notes |
|---|---|---|
| `BITAGENT_GRAPHQL_URL` | `http://localhost:3333/graphql` | The crawler's GraphQL endpoint (the core sets `127.0.0.1:3333` when it spawns the worker) |
| `BITAGENT_METRICS_URL` | `http://localhost:3333/metrics` | The crawler's Prometheus endpoint (same) |
| `BITAGENT_TORZNAB_URL` | derived | Torznab base for the `/torznab/*` proxy; defaults to the GraphQL URL with `/graphql` → `/torznab` |
| `TORZNAB_API_KEY` | empty | The crawler's Torznab key (same name as the core's — set once). Users get personal keys, stored hashed, validated by this app's proxy |
| `OPERATOR_HOSTS` / `PUBLIC_LIBRARY_HOSTS` | `localhost,127.0.0.1` / `library.localhost` | Explicit, disjoint, non-empty host allowlists. Any other `Host` is answered `421` before routing |
| `REQUIRE_AUTH` | `true` | `false` is the development bypass |
| `DASHBOARD_API_KEY` | empty | An explicit operator credential for scripts: `?apikey=`, `X-Api-Key` or `Authorization: Bearer` |
| `TRUST_FORWARDED_USER` / `TRUST_NPM_HEADERS` | `false` | Accept an identity header from a reverse proxy (`X-Forwarded-User`/`Remote-User`, or `X-Auth-User-Id`) |
| `TRUSTED_PROXY_CIDRS` | empty | Required with either trust flag: the proxy's transport peer, as narrow as possible |
| `PROXY_AUTH_SECRET` | empty | Required with either trust flag: the proxy must overwrite `X-BitAgent-Proxy-Proof` with this value on every request |
| `OPERATOR_ROLES` | `OWNER` | Verified `X-Auth-Priv` values that grant the operator surface |
| `TMDB_API_KEY` | empty | Posters and title metadata. Same name as the core's key — set once |
| `SONARR_*`, `RADARR_*`, `LIDARR_*` | empty | `_BASE_URL` + `_API_KEY` enable *push to* that \*arr from the library |
| `LLM_MONTHLY_BUDGET_USD` | `10.0` | The provider cap the AI tab measures spend against |
| `LIBRARY_BRAND` | `BitAgent` | The name in the public library's title and web-app manifest |
| `APP_SWITCHER_SCRIPT_URL` | empty | Optional cross-origin script for a shared app switcher; its origin is added to the CSP |
| `INFISICAL_*` | empty | Optional secret hydration at startup from an [Infisical](https://infisical.com) project; `_URL`, `_ENV`, `_PROJECT_ID`, `_CLIENT_ID`, `_CLIENT_SECRET` and `_PATH` are all explicit. Fails open |
| `DB_PATH` | `/data/bitagent-ui.db` | The SQLite file |
| `HOST` / `PORT` | `0.0.0.0` / `8080` | Bind address when run standalone; the core passes `UI_LISTEN_ADDRESS` as `--host`/`--port` when it spawns the worker |

Host lists, proxy trust, proxy CIDRs, the proof, operator roles and the upstream
URLs are **startup-only**; they are never editable from the UI. Integration
credentials can be overridden from the Settings tab, are never echoed back, and
their audit rows store `[redacted]`.

## Authentication

The app validates no passwords and no session cookies itself. It identifies a
request through one of three tiers, tried in order; the first that yields a user
wins:

```
request → REQUIRE_AUTH=false? ─yes→ synthetic open user (development only)
             │ no
             ▼
          tier 1  DASHBOARD_API_KEY   key supplied and correct → api client
                                      key supplied and wrong   → 401, no fall-through
                                      no key                   → fall through
             ▼
          tier 2  X-Auth-User-Id      (TRUST_NPM_HEADERS + trusted peer + proof)
             ▼
          tier 3  X-Forwarded-User    (TRUST_FORWARDED_USER + trusted peer + proof)
             ▼
          401 authentication_required
```

The proxy tiers exist for a reverse proxy that performs login itself (an
`auth_request` gate, an OIDC proxy, a forward-auth service) and then injects the
verified identity. They are honoured only when **both** the transport peer is
inside `TRUSTED_PROXY_CIDRS` and `X-BitAgent-Proxy-Proof` constant-time-matches
`PROXY_AUTH_SECRET`; enabling a tier without both makes startup fail, and the
proxy must overwrite the identity, privilege and proof headers on every request.
Uvicorn runs with proxy-header rewriting disabled so the peer check sees the real
transport peer, not `X-Forwarded-For`.

Hostname selects a surface; it never grants authority. A valid non-operator
identity gets `403` on the operator host, operator APIs are `404` on the public
host, and unknown hosts get `421`. `tests/test_auth.py` and
`tests/test_operator_gate.py` pin all of this.

## Development

```bash
python -m pip install --require-hashes -r requirements.lock -r requirements-test.lock
python -m pip install --no-deps --no-build-isolation -e .
python -m pytest -W error        # the suite, warnings as errors (what CI runs)
ruff check .                     # lint
node --test tests/library_state.test.js
```

No crawler at hand? `python tools/demo_core.py` serves the GraphQL documents and
metric families the console reads from synthetic data (public-domain films,
invented release groups), on `:3333` — the quick-start command above then runs
the whole console against it. The screenshots on this page were taken that way.

The layout is flat — the Python modules live in this directory and the image
copies them, with `static/` and `templates/`, to `/app/ui`, where the core runs
`python -m uvicorn app:app --no-proxy-headers`. The version in `version.py`
follows the repo's tag series (one tag covers the core and the UI). `requirements.lock` and `requirements-test.lock`
are complete, hash-locked resolutions for Python 3.12; regenerate them with
`pip-compile --generate-hashes` after changing a direct pin in
`requirements.txt` or `requirements-test.in`, and `tests/test_dependency_contract.py`
fails when they drift. The image installs only the runtime lock with
`--require-hashes` and then removes `pip`, `setuptools` and `wheel`.

Contributions are welcome — read [CONTRIBUTING.md](../CONTRIBUTING.md) first.

## Support

- Bugs and feature requests: [open an issue](https://github.com/spencercnorton/bitagent/issues/new/choose).
- Security: [private vulnerability reporting](https://github.com/spencercnorton/bitagent/security/advisories/new) — see [SECURITY.md](../SECURITY.md).
- If it earns a place in your stack, you can [support its development](https://buy.stripe.com/8x26oH2U44f65TRe574wM04).

## Licence

[MIT](../LICENSE). BitAgent itself descends from
[bitmagnet-io/bitmagnet](https://github.com/bitmagnet-io/bitmagnet); this
console is original code.

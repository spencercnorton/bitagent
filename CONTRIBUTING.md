# Contributing to BitAgent

BitAgent's public scope is the reusable Go backend: DHT discovery, metadata
processing, title matching, content filtering, cleanup, evidence and machine
APIs. The hosted product and its browser interface are developed separately.

## Development workflow

Branch from `main` and open a GitHub pull request. Build, tests, integration,
privacy and distribution checks must pass before merge. Keep unreleased
changes in `CHANGELOG.md`; see [releasing](docs/RELEASING.md) for tagged builds.

Use a GitHub noreply address and sign off commits (`git commit -s`) under the
[Developer Certificate of Origin](https://developercertificate.org). Review
public diffs, logs and screenshots for credentials, personal paths and real
user data. Report vulnerabilities through [SECURITY.md](SECURITY.md).

## Local setup

Use the Go version in `go.mod` (currently 1.26.8), PostgreSQL 14 or later
(16 recommended), and optionally Docker Compose and [Task](https://taskfile.dev).
Python 3 is used only for repository policy checks. It is not a runtime
requirement for the backend.

```sh
git clone https://github.com/spencercnorton/bitagent.git
cd bitagent
cp examples/.env.example examples/.env.public
# Set POSTGRES_PASSWORD and TORZNAB_API_KEY in the ignored environment file.
docker compose -f examples/docker-compose.public.yml --env-file examples/.env.public up -d --build
```

For a native binary, `task build` reads the release version from `VERSION`.
See [development setup](docs/development/setup.md) for configuration and
schema generation.

## Validation

```sh
go vet ./...
go test -race -count=1 -timeout=10m ./...
python3 scripts/test_public_content.py
python3 scripts/test_headless_boundary.py
python3 scripts/check_headless_boundary.py
```

CI also runs PostgreSQL regressions for purge, matching, LLM-stage and capture
packages against a disposable database, scans called Go dependencies, and
builds the supported `linux/amd64` image. Do not attach production state to CI.
Run the affected package while iterating, then the required checks before
submitting.

## Pull requests

Use one logical change per pull request, conventional commit subjects and
the repository's PR template. Explain the problem, resulting behavior,
compatibility impact and validation. Version bumps edit root `VERSION` and
the changelog together; published tags remain immutable.

## Code and tests

- Format Go with `gofmt`; document public types and functions.
- Pass `context.Context` first to operations that perform I/O.
- Wrap errors with `%w` and a useful prefix.
- Cover changed behavior with meaningful success and failure regressions.
- Use disposable PostgreSQL instances or the existing test helpers for
  integration tests.
- Preserve two-stage opt-in and dry-run behavior for destructive or paid
  processing features.

The distribution guard prevents browser code, hosted account settings and
private deployment files from returning to this repository. Generic adapters
for configurable third-party services belong here when they improve backend
processing and do not depend on a particular deployment.

## License and attribution

BitAgent and its upstream [bitmagnet](https://github.com/bitmagnet-io/bitmagnet)
are MIT-licensed. Preserve attribution in [NOTICE](NOTICE). Contributions are
released under the same terms; no CLA is required.

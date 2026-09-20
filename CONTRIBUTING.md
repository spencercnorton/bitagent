# Contributing to BitAgent

Thanks for your interest. BitAgent is a small, focused project with a
narrow scope: stay accurate and non-rotting as a DHT crawler / content
indexer / torznab adapter / `*arr`-evidence pipeline. We deliberately do
not chase generic self-hosting features that exist upstream in
[`bitmagnet-io/bitmagnet`](https://github.com/bitmagnet-io/bitmagnet) or
in adjacent tools.

If you are about to file a security issue, **stop** and read
[`SECURITY.md`](SECURITY.md) instead — public issues are not the right
channel for those.

## How changes land

This GitHub repository is a **release mirror**: every commit on `main` is
a tagged release built from the maintainer's development tree, and `main`
only ever moves forward by a release. Pull requests are reviewed **here**
but not merged here: an accepted change is applied to the development
tree and ships in the next tagged release, after which the pull request is
closed with a reference to that release and you keep the credit in the
release notes. Please branch from `main` and do not rebase onto anything
else.

## Local development

### Prerequisites

- Go 1.22+
- Postgres 14+ (16 recommended)
- Docker + Docker Compose (for the local stack)
- `task` (https://taskfile.dev) — used as the build entrypoint
- For the dashboard companion: Python 3.12+ (separate repo:
  `bitagent-ui`)

### Getting the code running

```bash
git clone https://github.com/spencercnorton/bitagent.git
cd bitagent
cp examples/.env.example examples/.env.public   # set POSTGRES_PASSWORD
docker compose -f examples/docker-compose.public.yml --env-file examples/.env.public up -d --build
```

The public compose stack is described in [`examples/README.md`](examples/README.md).
The Go binary's entrypoint is `main.go`; subcommands live under
`internal/app/cmd/`.

### Running the test suite

```bash
go test ./... -race -count=1
```

CI runs `go vet` and the test suite on every pull request; the
maintainer's pipeline additionally runs `golangci-lint run`, the race
suite against Postgres, and the image build. All of it must pass before a
change ships.

For touched code, run only the relevant package to iterate fast:

```bash
go test ./internal/torznab/... -race -count=1 -v
```

## Branch + commit conventions

- Branch from `main`. Branch names: `feat/<topic>`, `fix/<topic>`,
  `docs/<topic>`.
- One logical change per pull request. If you find a drive-by fix, file
  it separately or call it out in the description.
- Conventional Commits subject line (`feat(torznab): …`, `fix(dht): …`,
  `docs(security): …`). Body wraps at ~72 cols.
- **DCO sign-off required.** Use `git commit --signoff` (or
  `git commit -s`). We do **not** require a CLA. By signing off you
  certify the [Developer Certificate of Origin](https://developercertificate.org).
- Commit messages explain *why*, not *what* the diff already shows.

## Pull request description

The pull request template asks for these sections:

```markdown
## What changed
<concise summary of the diff>

## Why
<motivation: which issue, what symptom, what's the user story>

## Impact
<who's affected, breaking changes, migration steps>

## Version
<current> → <new> (<patch | minor | major>)
Reason: <why this bump level>

## Test results
<paste relevant `go test` output or attach evidence>
```

Pull requests missing them are sent back for revision.

## Code style

- `gofmt -s` — enforced by CI.
- `golangci-lint run` clean — enforced by CI. The config is at
  `.golangci.yml`; do not silence findings without a comment explaining
  why.
- Public types and functions get doc comments. Private helpers get a
  comment when their purpose isn't obvious from the name.
- `context.Context` is the first arg of any function that does I/O.
- Errors are wrapped with `%w` and a stable string prefix; no
  bare-string returns.

## Test discipline

- Bug fixes ship with a regression test.
- New behaviour ships with at least one happy-path and one error-path
  test.
- Table-driven tests are preferred for branchy logic.
- Integration tests that need a Postgres should use `testcontainers-go`
  or the existing `testdb` helper; do not assume a DB is running.
- Don't mock what you don't own — prefer the real thing in a container.

## Code review

- CI must be green. Address findings by pushing a fix commit to your
  branch; force-pushing your own branch after a rebase is fine.
- The maintainer reviews every pull request personally. Accepted changes
  are applied to the development tree as described above; the release
  commit that carries them credits the author.

## Communication

- GitHub issues for bugs and feature requests — use the forms, they ask
  for what a maintainer needs first.
- For broader discussion (design, roadmap), open a GitHub Discussion
  rather than a chat channel — we want decisions written down. A
  Discord/Matrix room may open after sustained interest; it does not
  exist yet.

## Licensing

BitAgent is licensed under the MIT License (see [`LICENSE`](LICENSE)).
The original work it derives from — `bitmagnet-io/bitmagnet` — is also
MIT, with attribution preserved in [`NOTICE`](NOTICE). By contributing, you agree your work is
released under the same MIT terms.

## Out-of-scope contributions

Some changes we will close without merging. Save yourself the round
trip:

- A bundled web UI inside this repo — the operator dashboard lives in
  the separate `bitagent-ui` repo on purpose.
- New social features, account systems, multi-tenant auth.
- Speculative LLM-everywhere refactors. The LLM stage is opt-in,
  shadow-first, and bounded.
- Code-style sweeps that mix many files — they create review pain.

If you are unsure whether a contribution is in scope, open a small
issue describing the idea before writing the code.

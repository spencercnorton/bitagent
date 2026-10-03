# Local backend development

Use the Go version in `go.mod`, PostgreSQL 14+ (16 recommended), and optionally
[Task](https://taskfile.dev) and Docker Compose. Python 3 runs the public policy
checks; the backend executable and container do not depend on Python.

## Build and test

```sh
git clone https://github.com/spencercnorton/bitagent.git
cd bitagent
task build
./bitagent --help
./bitagent worker list
go vet ./...
go test -race -count=1 -timeout=10m ./...
python3 scripts/test_headless_boundary.py
python3 scripts/check_headless_boundary.py
```

`task build` reads root `VERSION`. Use affected Go packages for a quick edit
loop. CI's PostgreSQL integration job defines the supported disposable test
database and `BITAGENT_TEST_POSTGRES_DSN`; production state is never test data.

## Run a disposable local stack

```sh
cp examples/.env.example examples/.env.public
# Set a unique POSTGRES_PASSWORD and TORZNAB_API_KEY.
docker compose -f examples/docker-compose.public.yml --env-file examples/.env.public up -d --build
```

For a native process, export `POSTGRES_HOST`, `POSTGRES_NAME`, `POSTGRES_USER`
and `POSTGRES_PASSWORD` for your local database, then run:

```sh
./bitagent worker run --all
```

`config show` prints resolved values and their source; it can contain secrets,
so inspect it locally. Backend settings live in [configuration](../configuration.md).

## GraphQL and migrations

Schema inputs are in `graphql/schema/`, generated bindings in
`internal/gql/gql.gen.go`, and generation settings in `internal/gql/gqlgen.yml`.
After a schema change run `task gen-gql` and test the affected resolvers.
Use `POST /graphql` for inspection with a separate client or curl.

Goose migrations live in `migrations/`. Startup applies pending migrations;
`task migrate` runs the developer migration helper. Use disposable databases
for migration tests and keep a matching backup for upgrade recovery.

## Classifier changes

CEL rules are bundled in the executable, so rebuild after changes. Use
`bitagent classifier show` to inspect the workflow, and the evaluation CLI
commands to replay frozen synthetic or privately retained evidence.

See [CONTRIBUTING.md](../../CONTRIBUTING.md) for code conventions, PR requirements
and validation. Site development and account features use their own repository.

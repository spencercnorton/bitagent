# Synthetic reference processors

These independent services demonstrate the public contract without inference.
They are conformance examples, not production classification or policy models.
The Go reference proposes `movie` only for the synthetic `Synthetic.Amber.`
prefix. The Python service proposes publication exclusion only for
`Synthetic.Exclude.`. Both abstain outside their fixture cohort.

```sh
docker compose -f examples/processors/compose.yml up -d --build
```

The Compose services have separate CPU/RAM/PID limits, read-only filesystems,
no capabilities and no database mount or credential. Choose a restricted
network appropriate to deployment; loopback publication here is for local tests.
Core enforces concurrency, deadlines, message sizes, retries and queue bounds
regardless of whether a service cooperates. Configure authentication before
exposing a processor beyond an isolated test network.

Register each using `PUT /api/processors/v1/registry/<id>` with the admin bearer,
`expected_generation: 0`, its manifest's `expected_processor`, endpoint,
`mode: "shadow"`, `state: "active"`, `priority: 0`, `configuration: {}` and
`auth_env: ""`. Core in a container must use a reachable service hostname,
not another container's loopback. Change to apply only in a disposable synthetic
catalogue. No core rebuild/restart is needed when a compatible service changes.

Go imports only `processorapi/v1`. Python implements the same JSON protocol
using the standard library. `TestPostgresIndependentProcessesAndOperatorAPI`
builds/starts both independently, exercises real PostgreSQL processing and
checks status, metrics and cursor pagination. `TestTransportRejectsHostileResponsesAndContract`
checks malformed/oversized/duplicate fields, incompatible binding and redirects.
`TestExternalServiceUpgradeRollback` accepts separately running A/B services
through environment variables, so another plugin can run that test unchanged.
See the [contract](../../docs/processors/v1.md) and its portable fixtures.

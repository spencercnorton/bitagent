# Backend troubleshooting

Use metrics, machine APIs and container logs to distinguish network, worker,
classification and client failures. The backend does not serve browser pages.

## No DHT progress

Inspect `bitagent_dht_crawler_persisted_total`, DHT peer/routing metrics and
`docker compose logs bitagent`. Verify outbound UDP/TCP, peer-port routing and
that the crawler worker is enabled. Startup can take time; a healthy HTTP probe
alone does not establish that peers or metadata are arriving.

```sh
docker exec bitagent bitagent worker list
curl --fail http://localhost:3333/metrics
```

`worker list` lists available workers, rather than proving that each is running.
Use startup logs and subsystem observations to verify their actual activity.

## HTTP or GraphQL unavailable

Check the container is running, API port mapping and
`HTTP_SERVER_LOCAL_ADDRESS`. Probe from the same network as the client:

```sh
curl --fail -H 'Content-Type: application/json'   --data '{"query":"{ __typename }"}' http://localhost:3333/graphql
```

Use JSON POST. `GET /graphql` does not serve a playground in 4.x. If the request
fails from another container, `localhost` refers to that container rather than
the backend host. Review reverse-proxy authentication and routing independently.

## Torznab returns 401

Privately compare the caller's key with `TORZNAB_API_KEY`. Rotate it by updating
backend configuration and every client, then restarting the backend. This key
protects Torznab; it does not grant access to GraphQL/import/metrics.

## Client test succeeds but searches are empty

Check Torznab capabilities, request type/category parameters, crawl progress and
classification metrics. Query the [GraphQL catalog](reference/graphql-api.md)
to distinguish an empty corpus from search filtering. Review
[classification](concepts/classification.md), seeder/unknown-age filters and
provider configuration before loosening admission policies.

## Evidence never arrives

The webhook endpoint is `POST /evidence/arr/<instance>`. Set the client's
`X-Evidence-Token` custom header to the backend's `EVIDENCE_WEBHOOK_SECRET`.
An empty secret disables the webhook gate and is suitable only for isolated
local development. Inspect rejection metrics and logs for auth, schema or
payload failures. See [evidence](evidence.md) for the contract and polling
backstop; a client's test request is not proof of a real acquisition event.

## Health probe fails or database restarts

Inspect recent probe output, PostgreSQL logs, disk availability, memory and
connection limits. The public probe checks `/metrics` on the core API port.
Verify that it matches any changed in-container binding. Do not treat a probe
as a complete database/query/crawl capacity test.

## Optional processing appears inactive

Use `config show` locally to verify resolved modes, source and provider
configuration. It prints secret values, so do not paste its output into an
issue. Shadow/dry-run mode produces observations without applying a verdict;
paid/destructive stages require separate enable/apply gates. Check budgets,
provider errors and subsystem metrics before changing those gates.

## Report a problem

Include the release version, safe reproduction and relevant redacted metric/log
samples. Exclude credentials, personal paths, real media rows and deployment
identities. Security findings use [private reporting](../SECURITY.md).

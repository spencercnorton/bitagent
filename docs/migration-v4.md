# Migrating to the standalone 4.0 backend

Version 4.0 makes the public distribution a headless Go service. Crawling,
classification, title matching, filtering, evidence, retention, scrubbing and
machine APIs remain in this repository. Site code and its account/library state
now have a separate source, release and deployment lifecycle.

## Compatibility changes

- The public image no longer includes a browser console or library service.
  The former supervised site worker and Python runtime are absent.
- Backend HTTP remains on port 3333. GraphQL accepts `POST /graphql`; it no
  longer serves an HTML playground on `GET /graphql`.
- Site ports, host routing, identity headers, membership/invitation settings,
  SQLite state and account keys are not backend configuration.
- Root `VERSION` replaces the former site module as the backend release version.
- PostgreSQL migrations and core processing behavior are retained. The split
  does not migrate database rows, enroll users or enable optional processing.

## Upgrade a 3.x combined installation

1. Record the exact image and configuration, and back up PostgreSQL plus all
   site and optional backend volumes. Test recovery before upgrading.
2. Give the site its own deployment before replacing a combined image. Preserve
   its existing state and account configuration in that deployment. Point its
   backend client at the core's GraphQL and metrics endpoints over a protected
   service network. Do not put the core APIs behind the site's human login flow.
3. Deploy the 4.x backend with PostgreSQL and backend configuration. Remove site
   settings, its old HTTP port and its SQLite volume from this backend service.
   Preserve existing backend configuration/data volume paths to avoid orphaning
   optional persisted state.
4. Verify database health, `POST /graphql`, `/metrics`, Torznab capabilities and
   crawler progress. Confirm no browser service listens on the old site port.
5. Verify the independently deployed site's own version, state and integration.
   Backend release checks do not establish acceptance of a separate site.

Consumers that already ran the Go crawler headless keep the same processing
APIs. Retain the previous artifacts and backups: schema changes can make a
binary-only rollback insufficient.

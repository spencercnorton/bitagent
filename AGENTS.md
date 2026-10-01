# Working on bitagent

- GitHub is the development home; branch from `main` and use a pull request.
- Read `CONTRIBUTING.md`, `docs/OPERATIONS.md` and `docs/RELEASING.md` before substantial changes.
- Use the public build and test workflow. Keep CI read-only for pull requests.
- Never include personal paths, private deployment details, credentials or real user data in source, commits, issues, logs or media.
- Capture demos in a disposable environment with synthetic data. Review every animation frame.
- Keep unreleased changes in `CHANGELOG.md`; never rewrite published tags.
- Use a GitHub noreply commit identity. Document behavior and relevant validation in the pull request.

- Keep internal journals and deployment handoffs outside this public repository; record behavior and validation in the pull request.

## Session notes

- The SSO enrollment bridge uses one startup-sealed account-subject format.
  Preserve the legacy default, per-enrollment namespace and exact account
  ownership. Format changes must not migrate data, grant an alias membership
  or reactivate a suspension; cover restart mismatches and cross-account
  isolation with actual SQLite/ASGI tests.

- Invitations use existing verified SSO subjects and atomic SQLite ledgers.
  Keep the feature disabled by default, tokens out of URLs/logs/storage except
  one-time fragment links, and payment fulfillment separate from client claims.
  Optional SSO bootstrap uses a distinct protected machine key and bounded
  signed enrollments; direct human redemption retains its trusted proxy gate.
  Hosted payment integration stays disabled by default; preserve immutable
  payment orders, raw signature validation and terminal refund/dispute proofs.
  Mocked provider tests do not establish sandbox or live billing acceptance.

- Optional private tracker address ownership uses a separate bounded,
  authenticated seed snapshot. Keep transfer metrics labeled client-reported,
  preserve the disabled default, and cover authorization and expiry with real
  ASGI/SQLite regressions. Public-content checks exclude operation journals;
  record only generic implementation and validation here.

- Private readiness uses a single lifespan owner and bounded observations.
  Preserve epoch/visibility fencing, default portability and original proof
  timestamps; functional SQLite tests do not establish deployment capacity.

- Personal indexer keys default to public-only access. Private permission must
  be an explicit human-member rotation; never infer it from membership, roles,
  subject changes or old tracker records. Preserve exact-owner checks inside
  queued writers and final responses, plus key-owner equality for peer joins.

- The explicit per-key private permission is a breaking release change.
  Existing credentials default to public-only; document the required human
  rotation and client reconfiguration without migrating owners or enabling
  private deployment flags. Human-member catalogue browsing is unchanged.

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

- Optional private tracker address ownership uses a separate bounded,
  authenticated seed snapshot. Keep transfer metrics labeled client-reported,
  preserve the disabled default, and cover authorization and expiry with real
  ASGI/SQLite regressions. Public-content checks exclude operation journals;
  record only generic implementation and validation here.

- Private readiness uses a single lifespan owner and bounded observations.
  Preserve epoch/visibility fencing, default portability and original proof
  timestamps; functional SQLite tests do not establish deployment capacity.

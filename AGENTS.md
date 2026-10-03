# Working on the BitAgent backend

- GitHub is the development home; branch from `main` and use a pull request.
- Read `CONTRIBUTING.md`, `docs/OPERATIONS.md` and `docs/RELEASING.md` before substantial changes.
- This repository contains the headless crawler, processing pipeline, APIs and CLI.
  Browser pages, site authentication, membership, private media catalogues and
  deployment-specific adapters belong in a separate application.
- Keep the backend independently buildable and usable with its public examples.
  Never require a private service, site repository or particular operator's hardware.
- Run the public build/test workflow and headless boundary check. CI stays read-only
  for pull requests. Keep existing processing schemas and migration history intact.
- Never include personal paths, private deployment details, credentials or real
  user data in source, commits, issues, logs or media.
- Use synthetic data for demonstrations, tests and reproducible evaluation.
- Keep unreleased changes in `CHANGELOG.md`; `VERSION` is the backend release
  version. Never rewrite published tags.
- Use a GitHub noreply commit identity and DCO sign-off. Document behavior,
  compatibility and relevant validation in the pull request.
- Keep internal journals and deployment handoffs outside this public repository.

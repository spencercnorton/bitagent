# Changelog

## 2.10.9 — 2026-09-27

- Establish GitHub pull requests as the development workflow, with privacy checks.
- Add deployment, configuration, security, upgrade and recovery documentation.
- Remove the inherited shared metadata key and automatic fallback; enrichment
  requires the operator's own TMDB_API_KEY.

- Fail closed when retained content-filter evidence loses its privacy, expiry
  or source-identity admission, with PostgreSQL regression coverage.

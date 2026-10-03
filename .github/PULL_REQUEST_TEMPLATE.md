## What changed

<!-- Concise summary of the diff. Link the issue if there is one: "Fixes #12". -->

## Why

<!-- Motivation: which issue, what symptom, what's the user story. -->

## Impact

<!-- Who's affected, breaking changes, migration steps. -->

## Version

<!-- Current → new version (patch | minor | major), and the reason for the bump.
     Release-version changes update root VERSION and CHANGELOG.md together. -->

## Test results

<!-- Summarize relevant Go, PostgreSQL, API, image and policy validation.
     State what was verified and any remaining acceptance limits. -->

## Checklist

- [ ] `gofmt -s` clean, `go vet ./...` clean, tests pass
- [ ] Backend distribution boundary and privacy checks pass
- [ ] Commits are signed off (`git commit -s`, Developer Certificate of Origin)
- [ ] No secrets, hostnames, tracker credentials or personal paths in the diff
- [ ] Docs updated if behaviour changed (`docs/`, README)

<!--
How this lands: branch from main and open a pull request into main. After review
and required checks pass, accepted changes merge here and ship in a tagged
backend release. See CONTRIBUTING.md and docs/RELEASING.md.
-->

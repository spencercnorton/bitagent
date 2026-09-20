## What changed

<!-- Concise summary of the diff. Link the issue if there is one: "Fixes #12". -->

## Why

<!-- Motivation: which issue, what symptom, what's the user story. -->

## Impact

<!-- Who's affected, breaking changes, migration steps. -->

## Test results

<!-- Paste the relevant `go test ./... -race -count=1` output. -->

## Checklist

- [ ] `gofmt -s` clean, `go vet ./...` clean, tests pass
- [ ] Commits are signed off (`git commit -s`, Developer Certificate of Origin)
- [ ] No secrets, hostnames, tracker credentials or personal paths in the diff
- [ ] Docs updated if behaviour changed (`docs/`, README)

<!--
How this lands: this repository is a release mirror. A maintainer reviews the
pull request here, applies accepted changes to the development tree, and the
change ships in the next tagged release — the pull request is then closed
with a reference to that release. See CONTRIBUTING.md.
-->

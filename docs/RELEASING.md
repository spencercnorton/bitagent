# Releasing the backend

GitHub is the development home of the public Go backend. Branch from `main`
and merge after the required tests, PostgreSQL integration, container and
privacy checks pass. Keep pending changes in `CHANGELOG.md` under
`## Unreleased`.

## Release checklist

1. Update root `VERSION` and changelog together in a reviewed pull request.
   `VERSION` contains a stable semantic version without its `v` prefix.
2. Confirm the release commit is on `main` and required checks are green.
   Test database changes against disposable state and review public artifacts
   for secrets and identifying deployment data.
3. Create an immutable `vX.Y.Z` tag on that exact checked commit and push it.
4. The tag workflow re-runs public CI/privacy, proves main ancestry and version
   equality, then publishes a versioned `linux/amd64` backend image to
   `ghcr.io/spencercnorton/bitagent`.
5. Download and qualify the release assets before deployment. Never mutate
   a published tag or include private configuration/data in an artifact.

## Published artifacts

`source.tar.gz` is `git archive` of the checked tag. `IMAGE_DIGEST.txt` identifies
the immutable backend image. `SHA256SUMS.txt` covers both assets. The public
container contains the static Go binary, CA certificates and runtime tools.
Browser pages, Python services and site/account state are separate products.

Verify the asset checksums, pull the exact digest, and inspect these OCI labels:
`org.opencontainers.image.version`, `revision`, `created`, and `source`.
They must match the tag, checked commit, commit timestamp and public repository.
Then run the image qualification:

```sh
sh scripts/check_backend_image.sh '<exact-image-reference>' vX.Y.Z
```

This runs the CLI and worker registry with network disabled and rootfs read-only,
checks version identity, required backend workers, absent site runtimes and
CA certificates. It does not replace a database/API deployment health check.

## Local candidate builds

Build from a clean checkout or an exact `git archive`, using `--platform
linux/amd64`. Pass Docker arguments `VERSION` (with `v` prefix), `COMMIT`,
`BUILD_DATE` (the source commit timestamp) and `SOURCE` (the public repository
URL), as the release workflow does. Context exclusions keep local state out
of the image. An independently rebuilt image can differ as package mirrors
change; deploy the qualified published digest.

## Deployment and recovery

Each consumer manages its own secrets, configuration and deployment. Record
version/digest before upgrading, back up state, rehearse restoration and retain
the previous artifact. Review [4.0 migration](migration-v4.md) when upgrading
from a combined 3.x image. Database migrations can require restoration of a
matching backup rather than a binary-only downgrade.

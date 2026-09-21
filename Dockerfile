# One image: the Go core plus the Python operator console / public library
# under ui/, which the core supervises as the `ui` worker (off by default;
# UI_ENABLED=true, with `worker run --all` or `--keys ui`).
#
# The runtime base is the UI's digest-pinned python:slim so its hash-locked
# wheels stay exactly what requirements.lock verified. The core is a static
# binary, so it does not care which libc the runtime ships.

FROM golang:1.23.6-alpine3.20 AS build

# git: `git describe` below, and Go's -buildvcs=auto errors when .git is
# present but no git binary is.
RUN apk --no-cache add git

WORKDIR /build

COPY go.mod go.sum ./
RUN go mod download

COPY . .

# CI passes VERSION (.gitlab-ci.yml publish-image); local builds fall back to
# git describe as before.
ARG VERSION
# CGO_ENABLED=0 is load-bearing: the alpine toolchain defaults to 1 and links
# the binary against musl, which the Debian runtime does not have.
RUN CGO_ENABLED=0 go build \
    -ldflags "-s -w -X github.com/spencercnorton/bitagent/internal/version.GitTag=${VERSION:-$(git describe --tags --always --dirty)}" \
    -o /build/bitagent .


FROM python:3.12.13-slim-bookworm@sha256:d50fb7611f86d04a3b0471b46d7557818d88983fc3136726336b2a4c657aa30b AS ui-deps
COPY ui/requirements.txt ui/requirements.lock ./
RUN python -m pip install --no-cache-dir --require-hashes -r requirements.lock \
    && python -m pip check \
    && rm -rf \
        /usr/local/lib/python3.12/site-packages/_distutils_hack \
        /usr/local/lib/python3.12/site-packages/distutils-precedence.pth \
        /usr/local/lib/python3.12/site-packages/pip \
        /usr/local/lib/python3.12/site-packages/pip-*.dist-info \
        /usr/local/lib/python3.12/site-packages/pkg_resources \
        /usr/local/lib/python3.12/site-packages/setuptools \
        /usr/local/lib/python3.12/site-packages/setuptools-*.dist-info \
        /usr/local/lib/python3.12/site-packages/wheel \
        /usr/local/lib/python3.12/site-packages/wheel-*.dist-info


FROM python:3.12.13-slim-bookworm@sha256:d50fb7611f86d04a3b0471b46d7557818d88983fc3136726336b2a4c657aa30b

# PYTHONUNBUFFERED: the core reads uvicorn's stdout/stderr line by line into
# its own logger, so the child must not block-buffer.
ENV PYTHONDONTWRITEBYTECODE=1 \
    PYTHONUNBUFFERED=1

# curl: compose healthchecks. libpcre2/libssh2 are pulled explicitly to get
# past the base image's CVE'd versions (unpinned: the archive only serves the
# fixed version or newer, and a point release drops the old one).
RUN apt-get update \
    && apt-get install -y --no-install-recommends curl libpcre2-8-0 libssh2-1 \
    && rm -rf /var/lib/apt/lists/* \
        /usr/local/lib/python3.12/ensurepip \
        /usr/local/lib/python3.12/site-packages/_distutils_hack \
        /usr/local/lib/python3.12/site-packages/distutils-precedence.pth \
        /usr/local/lib/python3.12/site-packages/pip \
        /usr/local/lib/python3.12/site-packages/pip-*.dist-info \
        /usr/local/lib/python3.12/site-packages/pkg_resources \
        /usr/local/lib/python3.12/site-packages/setuptools \
        /usr/local/lib/python3.12/site-packages/setuptools-*.dist-info \
        /usr/local/lib/python3.12/site-packages/wheel \
        /usr/local/lib/python3.12/site-packages/wheel-*.dist-info \
    && rm -f /usr/local/bin/pip /usr/local/bin/pip3 \
        /usr/local/bin/pip3.12 /usr/local/bin/wheel

# No USER: the core always ran as root and the estate and quickstart composes
# mount /root/.config/bitmagnet and /root/.local/share/bitmagnet, which a USER
# would silently orphan. The core spawns the web-facing UI child as appuser
# (internal/ui/worker.go) when it runs as root, so /data is appuser-owned; a
# UI-only deployment can also run the whole container non-root with
# `user: "10001:10001"` in compose, as the old bitagent-ui stack did. /data
# existing is what makes ui/config.py put its SQLite file there instead of
# under /app/ui.
RUN useradd -m -u 10001 appuser \
    && mkdir -p /data \
    && chown appuser:appuser /data

COPY --from=ui-deps /usr/local/lib/python3.12/site-packages/ /usr/local/lib/python3.12/site-packages/
COPY --from=build /build/bitagent /usr/bin/bitagent

# Flat-layout app: every root-level module plus assets. Glob so a new module
# cannot be silently left out of the image (crash-looped bitagent-ui v1.2.0).
COPY ui/*.py /app/ui/
COPY ui/static /app/ui/static
COPY ui/templates /app/ui/templates

ENTRYPOINT ["bitagent"]

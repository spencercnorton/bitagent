# The public image contains only the headless Go indexing backend.
FROM golang:1.26.8-alpine3.23@sha256:a8fa79c5bd40d880b52bd3b6d7669ecdcfd00e85facdd427d279efb5ddd79cb1 AS build

RUN apk --no-cache add git
WORKDIR /build
COPY go.mod go.sum ./
RUN go mod download
COPY . .

# VERSION is the release source of truth, including source archives without Git.
# CI and release builds pass the same version explicitly for OCI labels.
ARG VERSION
RUN CGO_ENABLED=0 go build \
    -ldflags "-s -w -X github.com/spencercnorton/bitagent/internal/version.GitTag=${VERSION:-v$(cat VERSION)}" \
    -o /build/bitagent .

FROM alpine:3.23@sha256:85fe1e81d6758c208f3e1eed4338a1997e19d4be002d4dd32d3100c9a8c010a0

ARG VERSION=dev
ARG COMMIT=unknown
ARG BUILD_DATE=1970-01-01T00:00:00Z
ARG SOURCE=https://github.com/spencercnorton/bitagent
LABEL org.opencontainers.image.version="${VERSION}" \
      org.opencontainers.image.revision="${COMMIT}" \
      org.opencontainers.image.created="${BUILD_DATE}" \
      org.opencontainers.image.source="${SOURCE}"

# TLS roots for metadata/LLM APIs, time zones, and the Compose health probe.
RUN apk --no-cache add ca-certificates curl tzdata
# Preserve existing /root/.config/bitmagnet and /root/.local/share/bitmagnet
# volume paths. The site runs independently with its own user and state.
COPY --from=build /build/bitagent /usr/bin/bitagent
ENTRYPOINT ["bitagent"]

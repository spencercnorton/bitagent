#!/bin/sh
# Verify the released runtime without a database, network or writable rootfs.
set -eu
image=$1
version=$2
help=$(docker run --rm --network none --read-only --entrypoint bitagent "$image" --help)
printf '%s\n' "$help" | grep -F "bitagent $version" >/dev/null
workers=$(docker run --rm --network none --read-only --entrypoint bitagent "$image" worker list)
printf '%s\n' "$workers" | grep -Fx http_server >/dev/null
printf '%s\n' "$workers" | grep -Fx dht_crawler >/dev/null
if printf '%s\n' "$workers" | grep -Fx ui >/dev/null; then
  echo 'Unexpected site worker in backend image' >&2
  exit 1
fi
docker run --rm --network none --read-only --entrypoint /bin/sh "$image" -ec '
  test ! -e /app/ui
  test ! -e /data/bitagent-ui.db
  for command in python python3 uvicorn pip node npm go; do
    if command -v "$command" >/dev/null 2>&1; then
      echo "Unexpected runtime command: $command" >&2
      exit 1
    fi
  done
  test -s /etc/ssl/certs/ca-certificates.crt
'

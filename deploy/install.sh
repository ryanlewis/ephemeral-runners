#!/usr/bin/env bash
# Build ephemeral-runners and install it as a systemd service. Run as root from
# a checkout. It does not write config.json, or enable or start the service.
#
# Usage: install.sh
set -euo pipefail

if [[ $EUID -ne 0 ]]; then
  echo "run this as root" >&2
  exit 1
fi

here=$(cd "$(dirname "$0")" && pwd)
root=$(cd "$here/.." && pwd)

if ! command -v go >/dev/null; then
  echo "go is not installed or not on PATH" >&2
  exit 1
fi

build=$(mktemp -d)
trap 'rm -rf "$build"' EXIT

(cd "$root" && CGO_ENABLED=0 go build -trimpath -buildvcs=false \
  -o "$build/ephemeral-runners" ./cmd/ephemeral-runners)

install -D -m 0755 "$build/ephemeral-runners" /usr/local/bin/ephemeral-runners
install -D -m 0644 "$here/ephemeral-runners.service" /etc/systemd/system/ephemeral-runners.service
install -D -m 0644 "$here/sysusers.conf" /etc/sysusers.d/ephemeral-runners.conf
install -D -m 0644 "$here/tmpfiles.conf" /etc/tmpfiles.d/ephemeral-runners.conf

systemd-sysusers /etc/sysusers.d/ephemeral-runners.conf
systemd-tmpfiles --create /etc/tmpfiles.d/ephemeral-runners.conf
systemctl daemon-reload

echo "installed; write /etc/ephemeral-runners/config.json, then: systemctl enable --now ephemeral-runners"

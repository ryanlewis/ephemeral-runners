#!/usr/bin/env bash
# Install ephemeral-runners as a systemd service. Run as root from a checkout.
# It does not write config.json, or enable or start the service.
#
# Usage: install.sh [binary]
#
# With a binary (built beforehand with go build), it installs that. Without
# one, it builds the program with the go on root's PATH.
set -euo pipefail

if [[ $EUID -ne 0 ]]; then
  echo "run this as root" >&2
  exit 1
fi

here=$(cd "$(dirname "$0")" && pwd)
root=$(cd "$here/.." && pwd)

binary=${1:-}
if [[ -z $binary ]]; then
  if ! command -v go >/dev/null; then
    echo "go is not on root's PATH: build the binary first and pass its path" >&2
    exit 1
  fi
  build=$(mktemp -d)
  trap 'rm -rf "$build"' EXIT
  binary=$build/ephemeral-runners
  (cd "$root" && CGO_ENABLED=0 go build -trimpath -buildvcs=false -o "$binary" ./cmd/ephemeral-runners)
elif [[ ! -f $binary || ! -x $binary ]]; then
  echo "$binary is not an executable file" >&2
  exit 1
fi

install -D -m 0755 "$binary" /usr/local/bin/ephemeral-runners
install -D -m 0644 "$here/ephemeral-runners.service" /etc/systemd/system/ephemeral-runners.service
install -D -m 0644 "$here/sysusers.conf" /etc/sysusers.d/ephemeral-runners.conf
install -D -m 0644 "$here/tmpfiles.conf" /etc/tmpfiles.d/ephemeral-runners.conf

systemd-sysusers /etc/sysusers.d/ephemeral-runners.conf
systemd-tmpfiles --create /etc/tmpfiles.d/ephemeral-runners.conf
systemctl daemon-reload

echo "installed; write /etc/ephemeral-runners/config.json, then: systemctl enable --now ephemeral-runners"

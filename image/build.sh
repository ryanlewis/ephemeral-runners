#!/usr/bin/env bash
# Build the runner VM image: the current Debian 13 cloud image, upgraded, with
# the GitHub Actions runner and whatever the site config adds. Run as root.
#
# Usage: build.sh [config]    (default: /etc/ephemeral-runners/image.env)
set -euo pipefail

here=$(cd "$(dirname "$0")" && pwd)
# shellcheck source=versions.env
source "$here/versions.env"

# Defaults, overridden by the site config
EXTRA_PACKAGES=""
RUST_TOOLCHAINS=""
RUST_TARGETS=""
DISK_SIZE=40G
OUT_DIR=/var/lib/ephemeral-runners/images
KEEP_IMAGES=3

config=${1:-/etc/ephemeral-runners/image.env}
if [[ -f $config ]]; then
  # shellcheck source=/dev/null
  source "$config"
else
  echo "no config at $config, so building with the defaults" >&2
fi

packages=(
  # Tools most jobs expect, as on GitHub's hosted Ubuntu runners
  build-essential pkg-config libssl-dev git curl jq python3 unzip xz-utils
  zstd ca-certificates sudo iproute2 procps
  # Actions runner (.NET) dependencies
  libicu76 libkrb5-3 zlib1g
)
read -ra extra <<< "$EXTRA_PACKAGES"
packages+=("${extra[@]}")

work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT

debian=https://cloud.debian.org/images/cloud/trixie/latest
image="debian-13-genericcloud-amd64.qcow2"
curl -fsSL -o "$work/$image" "$debian/$image"
curl -fsSL -o "$work/SHA512SUMS" "$debian/SHA512SUMS"
(cd "$work" && grep " $image\$" SHA512SUMS | sha512sum -c --quiet -)

curl -fsSL -o "$work/runner.tar.gz" \
  "https://github.com/actions/runner/releases/download/v$RUNNER_VERSION/actions-runner-linux-x64-$RUNNER_VERSION.tar.gz"
echo "$RUNNER_SHA256  $work/runner.tar.gz" | sha256sum -c --quiet -

rust=()
if [[ -n $RUST_TOOLCHAINS ]]; then
  curl -fsSL -o "$work/rustup-init" \
    "https://static.rust-lang.org/rustup/archive/$RUSTUP_VERSION/x86_64-unknown-linux-gnu/rustup-init"
  echo "$RUSTUP_SHA256  $work/rustup-init" | sha256sum -c --quiet -
  rust=(--upload "$work/rustup-init:/tmp/rustup-init")
fi

qemu-img resize -q "$work/$image" "$DISK_SIZE"

virt-customize -a "$work/$image" \
  --run-command 'growpart /dev/sda 1 && resize2fs /dev/sda1' \
  --update \
  --install "$(IFS=,; echo "${packages[*]}")" \
  --run-command 'DEBIAN_FRONTEND=noninteractive apt-get purge -y cloud-init netplan.io openssh-server && apt-get autoremove -y --purge' \
  --upload "$work/runner.tar.gz:/tmp/runner.tar.gz" \
  "${rust[@]}" \
  --upload "$here/setup.sh:/tmp/setup.sh" \
  --copy-in "$here/files/etc:/" \
  --copy-in "$here/files/usr:/" \
  --run-command "RUST_TOOLCHAINS='$RUST_TOOLCHAINS' RUST_TARGETS='$RUST_TARGETS' bash /tmp/setup.sh"

install -d -m 0755 "$OUT_DIR"
name="runner-$(date -u +%Y%m%d-%H%M%S).qcow2"
qemu-img convert -q -O qcow2 "$work/$image" "$OUT_DIR/$name"
ln -sfn "$name" "$OUT_DIR/current.qcow2.new"
mv -T "$OUT_DIR/current.qcow2.new" "$OUT_DIR/current.qcow2"
echo "built $OUT_DIR/$name"

# Drop old images. Running VMs keep theirs open, and new overlays are made
# against the resolved current image, so only the oldest go.
ls -1t "$OUT_DIR"/runner-*.qcow2 | tail -n +$((KEEP_IMAGES + 1)) | xargs -r rm -f

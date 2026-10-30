#!/usr/bin/env bash
# Build the runner VM image: the pinned Debian 13 cloud image, upgraded, with
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
METRICS_FILE=""

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

# Records the build's outcome in METRICS_FILE, if set, in the Prometheus text
# format. A failed build keeps the time of the last one that succeeded.
write_metrics() {
  local rc=$1 last=0
  [[ -n $METRICS_FILE ]] || return 0
  if [[ $rc -eq 0 ]]; then
    last=$(date +%s)
  elif [[ -f $METRICS_FILE ]]; then
    last=$(awk '$1 == "ephemeral_runners_image_build_last_success_seconds" {print $2}' "$METRICS_FILE")
  fi
  {
    echo "# HELP ephemeral_runners_image_build_last_success_seconds Unix time of the last image build that succeeded, or 0."
    echo "# TYPE ephemeral_runners_image_build_last_success_seconds gauge"
    echo "ephemeral_runners_image_build_last_success_seconds ${last:-0}"
    echo "# HELP ephemeral_runners_image_build_last_exit_code Exit code of the last image build."
    echo "# TYPE ephemeral_runners_image_build_last_exit_code gauge"
    echo "ephemeral_runners_image_build_last_exit_code $rc"
  } > "$METRICS_FILE.tmp"
  chmod 0644 "$METRICS_FILE.tmp"
  mv -f "$METRICS_FILE.tmp" "$METRICS_FILE"
}

work=$(mktemp -d)
on_exit() {
  local rc=$?
  rm -rf "$work"
  write_metrics "$rc"
}
trap on_exit EXIT

debian=https://cloud.debian.org/images/cloud/trixie/$DEBIAN_IMAGE_VERSION
image="debian-13-genericcloud-amd64-$DEBIAN_IMAGE_VERSION.qcow2"
if ! curl -fsSL -o "$work/$image" "$debian/$image"; then
  echo "can't download $debian/$image; if that's a 404, the pinned build is gone, so bump DEBIAN_IMAGE_VERSION and DEBIAN_IMAGE_SHA512 in $here/versions.env" >&2
  exit 1
fi
if ! echo "$DEBIAN_IMAGE_SHA512  $work/$image" | sha512sum -c --quiet -; then
  echo "$image doesn't match DEBIAN_IMAGE_SHA512 in $here/versions.env; check the pin against $debian/SHA512SUMS, and don't change it to match this download" >&2
  exit 1
fi

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

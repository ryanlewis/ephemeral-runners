#!/usr/bin/env bash
# Runs inside the image during the build (virt-customize).
set -euo pipefail

# Files copied in from files/ keep the builder's uid
chown root:root /etc/default/grub.d/99-runner.cfg /etc/systemd/network/10-runner.network \
  /etc/systemd/system/actions-runner.service /usr/local/sbin/actions-runner-start
chmod 0755 /usr/local/sbin/actions-runner-start

# A runner user with passwordless sudo, as on the hosted runners
useradd --create-home --shell /bin/bash runner
echo 'runner ALL=(ALL) NOPASSWD:ALL' > /etc/sudoers.d/runner
chmod 0440 /etc/sudoers.d/runner

install -d -o runner -g runner /home/runner/actions-runner
tar -xzf /tmp/runner.tar.gz -C /home/runner/actions-runner
chown -R runner:runner /home/runner/actions-runner

# Rust, when the site config asks for it. The first toolchain is the default.
if [[ -n $RUST_TOOLCHAINS ]]; then
  rustup=/home/runner/.cargo/bin/rustup
  targets=()
  [[ -n $RUST_TARGETS ]] && targets=(-t "${RUST_TARGETS// /,}")
  install -m 0755 /tmp/rustup-init /usr/local/bin/rustup-init
  runuser -u runner -- /usr/local/bin/rustup-init -y --no-modify-path --profile minimal --default-toolchain none
  for tc in $RUST_TOOLCHAINS; do
    runuser -u runner -- "$rustup" toolchain install "$tc" --profile minimal -c rustfmt,clippy "${targets[@]}"
  done
  read -r default _ <<< "$RUST_TOOLCHAINS"
  runuser -u runner -- "$rustup" default "$default"
  rm /usr/local/bin/rustup-init /tmp/rustup-init
fi

# Boot straight past GRUB
update-grub

systemctl enable systemd-networkd systemd-resolved actions-runner.service
ln -sf ../run/systemd/resolve/stub-resolv.conf /etc/resolv.conf

# Each VM gets its own machine ID, so DHCP leases don't collide
truncate -s 0 /etc/machine-id
rm -f /var/lib/dbus/machine-id

apt-get clean
rm -f /tmp/runner.tar.gz /tmp/setup.sh

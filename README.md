# ephemeral-runners

Self-hosted GitHub Actions runners on a Linux host with KVM. Each job gets a
fresh VM, which is destroyed when the job ends. The repos, labels, VM sizes
and image contents all come from configuration on the host.

## Image

`image/build.sh` builds the VM image. Run it as root on the host:

```sh
sudo image/build.sh [config]
```

The config defaults to `/etc/ephemeral-runners/image.env`; see
[`examples/image.env`](examples/image.env) for the settings. Without one, the
build uses the defaults: no extra packages and no Rust.

It needs `libguestfs-tools` and `qemu-utils`. It:

1. downloads the current Debian 13 genericcloud image and checks it against
   Debian's `SHA512SUMS`;
2. downloads the Actions runner, and `rustup-init` if the config lists Rust
   toolchains, at the versions in `image/versions.env`, and checks their
   SHA-256;
3. upgrades the packages, installs the base tools and the config's extra
   packages, removes cloud-init and the SSH server, and installs the Rust
   toolchains and targets from the config;
4. writes `runner-<timestamp>.qcow2` to `OUT_DIR`, points `current.qcow2` at
   it, and keeps the newest `KEEP_IMAGES`.

To pick up a new runner or rustup release, change the version and checksum in
`image/versions.env` together.

## How a VM gets its job

The VM runs `actions-runner.service` at boot. It reads a JIT runner config from
a small raw disk with serial `jitconfig`, then runs `run.sh --jitconfig` as the
`runner` user. When the job finishes, the VM powers off.

The host either attaches the disk when it starts the VM, or starts the VM
without it and hot-plugs the disk later. Until the disk appears, the VM waits
for as long as it takes, so a booted VM can wait as a warm spare. The service
writes its progress to the serial console: `RUNNER-WAIT` once if the disk was
not there at boot, and `RUNNER-START` when it reads the config.

Inside the VM, the `runner` user has passwordless sudo, as on the hosted runners.
Nothing in the VM survives the job.

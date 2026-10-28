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

## Running the service

```sh
sudo ephemeral-runners run -config /etc/ephemeral-runners/config.json
```

See [`examples/config.json`](examples/config.json) for the settings.
`ephemeral-runners check -config …` tests the GitHub App setup without
starting any VMs.

Every `poll_interval`, the service:

1. forgets VMs that have powered off: it deletes their files and their
   runners on GitHub;
2. lists the queued jobs in each repo whose labels are all in `labels` or
   are a size name. A job without a size label gets `default_size`. Jobs
   in runs that are already in progress are checked every 30 seconds
   instead, since each check costs API requests;
3. checks the runners it started that have not taken a job yet, and
   destroys any VM whose runner is not online within `timeouts.online`, is
   online without a job for `timeouts.idle`, or has had its job for longer
   than `timeouts.job`;
4. for each queued job that no starting or idle runner of its repo and size
   will take, registers a single-use runner named
   `<name_prefix>-<repo>-<size>-<4 hex>` with `labels` and the size, and
   gives its config to a VM of that size: to a warm spare if the job is
   of the spare size and one is free, otherwise to a new VM. Jobs start
   oldest first while the sizes of the VMs that have jobs fit in the
   `budget`; spares are destroyed if a new VM needs their room;
5. while jobs have been seen within `spares.idle_after`, keeps
   `spares.count` spares booted (`<name_prefix>-spare-<4 hex>`), and
   replaces any that booted from an older image. After that, it destroys
   them;
6. writes the metrics to `metrics_file`, if set, in the Prometheus text
   format.

If GitHub rate-limits it, it waits until the limit resets; after other
GitHub errors it waits 5 seconds, doubling up to a minute. Running VMs
carry on meanwhile.

On start, it destroys every VM whose name starts with `<name_prefix>-`,
deletes their files from `vm.state_dir`, and deletes that prefix's runners
in each repo. A job that was running in one of those VMs fails and can be
re-run.

On SIGINT or SIGTERM, it stops launching, destroys its VMs, deletes their
runners and files, and exits.

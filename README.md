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

`deploy/install.sh` also installs a copy of `image/` in
`/usr/local/lib/ephemeral-runners/image` and a timer that rebuilds the image
there once a week, which picks up Debian's security updates. Turn it on with
`systemctl enable --now ephemeral-runners-image.timer`. The service replaces
its warm spares once a new image is current. Set `METRICS_FILE` in the config
to record each build's outcome for Prometheus's node_exporter textfile
collector: `ephemeral_runners_image_build_last_success_seconds` and
`ephemeral_runners_image_build_last_exit_code`.

## Install

The service runs under systemd as its own user, `ephemeral-runners`. That user
is the only one that can read the GitHub App key.

**Prerequisites.** A Linux host with KVM (`/dev/kvm` exists), libvirt with the
`qemu:///system` connection and a network for the VMs, `libguestfs-tools` and
`qemu-utils` for the image build, and Go. On Debian:

```sh
sudo apt install libvirt-daemon-system libvirt-clients qemu-system-x86 \
  qemu-utils ovmf libguestfs-tools golang
```

Each VM's network port is isolated, so VMs on the same bridge or libvirt
network can't reach each other. Keeping them away from the host and the LAN is
up to the network: a bridge with no host address on a network that only routes
to the internet, for example.

**1. Install the program.** From a checkout, build the binary as a normal user
(or cross-compile it on another machine), then install it as root:

```sh
CGO_ENABLED=0 go build -trimpath -o ephemeral-runners ./cmd/ephemeral-runners
sudo deploy/install.sh ./ephemeral-runners
```

This installs the binary to `/usr/local/bin/ephemeral-runners`. (Without an
argument, `install.sh` builds with the `go` on root's PATH.) It also installs
the systemd unit, creates the `ephemeral-runners` user (in the `libvirt` and `kvm` groups) and creates
these directories:

| Path | Owner | Mode | Holds |
| --- | --- | --- | --- |
| `/etc/ephemeral-runners` | `root:ephemeral-runners` | 0750 | config, key |
| `/var/lib/ephemeral-runners` | `root` | 0755 | images |
| `/var/lib/ephemeral-runners/vms` | `ephemeral-runners` | 0755 | per-VM state |

It does not write the config or start the service.

**2. Create the GitHub App.** Create an App, install it on the repos the
runners will serve, and give it these repository permissions: Actions
(read), Administration (read and write) and Metadata (read). Note the App ID and
the installation ID, and generate a private key.

**3. Install the key.** The key must be readable by the service user and nobody
else:

```sh
sudo install -m 0440 -o root -g ephemeral-runners app.pem /etc/ephemeral-runners/app.pem
```

**4. Write the config.** Copy [`examples/config.json`](examples/config.json) to
`/etc/ephemeral-runners/config.json` and edit it: the App and installation IDs,
the repos, the labels, the VM sizes, the budget and the network. Check it as the service user, which
can read the key:

```sh
sudo -u ephemeral-runners ephemeral-runners check
```

Two hosts must never share a `name_prefix` on the same repo: at startup, the
service deletes every runner whose name starts with `<name_prefix>-`. For the
same reason, prefixes used on one repo must not start with each other, as
`example` and `example-x` do.

Every job's `runs-on` should carry an explicit size label (one of the names in
`sizes`). A job with no size label can be taken by a runner of any size.

**5. Build the image.** See [Image](#image). The config's `vm.image` points at
the result.

**6. Start the service.**

```sh
sudo systemctl enable --now ephemeral-runners
journalctl -u ephemeral-runners -f
```

Restarting or stopping the service destroys its VMs and deletes their runners.
This can take up to two minutes, and a job running at the time fails.

The unit makes the system read-only for the service, except
`/var/lib/ephemeral-runners`. If `metrics_file` is set, give the service write
access to its directory with a drop-in (`sudo systemctl edit ephemeral-runners`):

```ini
[Service]
ReadWritePaths=/var/lib/node_exporter/textfile
```

The directory must also be writable by the `ephemeral-runners` user.

To update, pull, build again, run `sudo deploy/install.sh ./ephemeral-runners`
and restart the service.

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

The VM's memory size is a ceiling: the host only backs pages the guest
touches. A virtio balloon with free page reporting hands pages the guest frees
back to the host while the job runs. Page cache still counts as in use, so set
`budget.memory_gib` a few GiB below the host's memory.

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

If GitHub rate-limits it, it waits as long as GitHub's `Retry-After`
asks, or else until the limit resets; after other GitHub errors it waits
5 seconds, doubling up to a minute. Running VMs carry on meanwhile.

On start, it destroys every VM whose name starts with `<name_prefix>-`,
deletes their files from `vm.state_dir`, and deletes that prefix's runners
in each repo. GitHub refuses to delete a runner that it still shows running
a job: the job fails once GitHub notices its runner has gone, and GitHub
then removes the runner itself. The job can be re-run.

On SIGINT or SIGTERM, it stops launching, destroys its VMs, deletes their
runners and files, and exits. A runner that was running a job is left to
GitHub, as on start.

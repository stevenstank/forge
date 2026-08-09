# Forge

A Docker-inspired container runtime, built from scratch in Go — to learn how
containers actually work, not to replace Docker.

> Forge is an educational systems project. It is **not** production-hardened
> and should not be used to run untrusted workloads. See [Non-goals](#non-goals).

---

## What is Forge?

Forge is a container runtime implemented from first principles: raw Linux
namespace syscalls, cgroups v2, `pivot_root`, veth/bridge networking, and OCI
image unpacking — no shelling out to `docker`, `runc`, or `containerd`.

If you've ever wondered what actually happens when you run `docker run
alpine echo hello`, Forge is the answer, built incrementally and explained
along the way.

## Motivation

Container tooling has become so good that it's easy to use containers for
years without understanding the four or five kernel mechanisms that make
them possible. Forge exists to make those mechanisms visible: each stage of
the project implements exactly one concept — process isolation, filesystem
isolation, resource limits, networking, images — as a small, readable, tested
Go package.

Forge deliberately does **not** aim for feature parity with Docker. It aims
for conceptual completeness with production-quality engineering discipline:
clean architecture, TDD, and minimal dependencies.

## Features

- **Process isolation** — PID, UTS, and mount namespaces via direct `clone(2)`
  syscalls.
- **Filesystem isolation** — `pivot_root`-based rootfs isolation with bind
  mounts.
- **Resource limits** — cgroups v2 for memory, CPU, and process-count limits.
- **Networking** — per-container network namespaces, veth pairs, a Forge-managed
  bridge, and NAT for outbound connectivity, spoken to the kernel over raw
  netlink — no `ip`, no `iptables`, no netlink library.
- **OCI images** — pull, verify, cache, and unpack standard OCI images from
  any compatible registry.
- **A real CLI** — `forge run`, `forge ps`, `forge exec`, `forge stop`,
  `forge logs`, `forge rm`, over a crash-safe on-disk state store.

All six stages are complete. Forge runs `forge run alpine:3.20 /bin/sh` and
lands you in an isolated, resource-limited, networked shell that leaves nothing
behind when it exits.

## Project Goals

- Prioritize **correctness, readability, and testability** over feature
  breadth.
- Use the **Go standard library** wherever possible; keep third-party
  dependencies minimal and justified.
- Build in **small, sequential stages**, where every stage is a complete,
  working, tested system.
- Make the mapping from **kernel concept → Go package** obvious.

See [docs/PRD.md](./docs/PRD.md) for the full product requirements and
[docs/SSOT.md](./docs/SSOT.md) for the engineering architecture reference.

## Non-goals

Forge intentionally does not implement: Dockerfile-style image building,
multi-host orchestration, rootless containers, seccomp/AppArmor/SELinux
integration, or a plugin system. See
[docs/PRD.md §5](./docs/PRD.md#5-non-goals) for the complete list and
rationale.

---

## Requirements

- Linux kernel **5.10+** with cgroups v2 (unified hierarchy) enabled.
- Go **1.24+**.
- Root privileges (or equivalent capabilities) to create namespaces, cgroups,
  and network interfaces.
- x86_64 or arm64. macOS/Windows users should develop inside a Linux VM.

## Installation

```bash
git clone https://github.com/stevenstank/forge.git
cd forge
make build
```

This produces a `forge` binary in `./bin/forge`.

## Building

```bash
make build        # build the forge binary
make test         # run unit tests (no root required)
make test-integration   # run integration tests (requires root, Linux)
make lint         # run golangci-lint
```

## Running

Forge must run as root (or with the necessary Linux capabilities:
`CAP_SYS_ADMIN`, `CAP_NET_ADMIN`, among others):

```bash
sudo ./bin/forge run /bin/echo "hello from forge"
```

## Example Commands

> **All six stages are implemented**, and every example below works today.
> `forge -h` always lists exactly what is implemented.
>
> `forge run` accepts three grammars, decided without a lookahead flag: an
> image reference (`alpine:3.20 /bin/sh`), a `-rootfs` directory plus a command,
> or — with neither — an absolute path run against the host's filesystem, which
> is Stage 1's behaviour and still valid. An argument beginning with `/`, `./`
> or `../` is a command; anything else in first position is an image.

### Stage 1 — process isolation

```bash
# Run a command in an isolated container
sudo forge run /bin/echo "hello from forge"

# The container's init process is PID 1 in its own namespace
sudo forge run /bin/sh -c 'echo "I am pid $$"'      # → I am pid 1

# Give the container a hostname (default: the container ID)
sudo forge run -hostname sandbox /bin/hostname

# Mounts made inside the container never reach the host
sudo forge run /bin/sh -c 'mount -t tmpfs tmpfs /mnt && ls /mnt'

# forge exits with the container's exit status
sudo forge run /bin/sh -c 'exit 42'; echo $?        # → 42

# Turn up the detail
sudo forge --log-level debug run /bin/true
```

### Stage 2 — filesystem isolation

`-rootfs` takes any unpacked root filesystem tree. Until Stage 5 pulls images,
the quickest way to get one:

```bash
curl -sL https://dl-cdn.alpinelinux.org/alpine/v3.20/releases/x86_64/alpine-minirootfs-3.20.3-x86_64.tar.gz \
  | sudo tar -xz -C /srv/alpine
```

```bash
# The container's / is the rootfs, not the host's
sudo forge run -rootfs /srv/alpine /bin/ls /

# The host is gone: no /home, no /srv, nothing to climb back to
sudo forge run -rootfs /srv/alpine /bin/ls /home     # → no such file or directory

# /proc is the container's own, so ps finally tells the truth
sudo forge run -rootfs /srv/alpine /bin/ps           # → one process, PID 1

# Bind-mount a host directory in
sudo forge run -rootfs /srv/alpine -mount /srv/data:/data /bin/ls /data

# ...read-only, so the container cannot write back to the host
sudo forge run -rootfs /srv/alpine -mount /srv/data:/data:ro \
  /bin/sh -c 'echo nope > /data/file'                # → Read-only file system

# An immutable root with one writable directory
sudo forge run -rootfs /srv/alpine -read-only -mount /tmp/scratch:/scratch /bin/sh

# Start somewhere other than /
sudo forge run -rootfs /srv/alpine -workdir /etc /bin/pwd    # → /etc

# Nothing is left behind: no mounts, no directories
sudo forge run -rootfs /srv/alpine /bin/true
ls /var/lib/forge/containers                         # → empty
```

> **Containers started from the same `-rootfs` share it, and writes go back to
> the source tree.** Stage 2 bind-mounts the tree rather than copying it;
> per-container copy-on-write arrives with image layering in Stage 5. Use
> `-read-only` if that matters to you.

### Stage 3 — resource limits

Every container gets its own cgroup v2 leaf at `/sys/fs/cgroup/forge/<id>`, so
it is accounted for whether or not you cap anything. Four flags turn accounting
into enforcement. Flags may be written `-memory` or `--memory`; both are the
same flag.

```bash
# Cap memory. The container lives in RAM or it is OOM-killed — swap is capped
# alongside it, so a container cannot escape the limit by paging out.
sudo forge run --memory 128m /bin/sh

# Suffixes are 1024-based: 512m, 1g, 1gib and a bare byte count all work
sudo forge run --memory 1g --rootfs /srv/alpine /bin/sh

# An allocator that overruns its limit is killed, not slowed down
sudo forge run --memory 32m /bin/sh -c 'a=; while :; do a="$a$a x"; done'
echo $?                                              # → 137 (128 + SIGKILL)

# Hard CPU ceiling, in cores. 0.5 is half a core, 1.5 is one and a half.
sudo forge run --cpus 0.5 /bin/sh -c 'while :; do :; done'

# A relative share instead of a ceiling: this container gets twice the CPU of a
# sibling left at the default 100 — but only when both are competing, and
# neither is capped.
sudo forge run --cpu-weight 200 /bin/sh

# The two are orthogonal: a ceiling and a share of what is left under it
sudo forge run --cpus 1.5 --cpu-weight 512 /bin/sh

# Cap the process count, which is what stops a fork bomb taking the host with it
sudo forge run --pids 64 /bin/sh -c 'while :; do sleep 30 & done'
                                                     # → fork fails, host survives

# Combine them
sudo forge run --rootfs /srv/alpine --memory 256m --cpus 1 --pids 128 /bin/sh

# "max" asks for no limit explicitly, which is not the same as saying nothing:
# an unset flag inherits, "max" writes an unlimited value.
sudo forge run --memory max --pids max /bin/sh

# The leaf is removed with the container, whatever the container exits with
sudo forge run --memory 128m /bin/false
ls /sys/fs/cgroup/forge                              # → empty
```

> **A limit you asked for is never silently dropped.** On a host without a
> cgroup v2 unified hierarchy, a container with limits refuses to start rather
> than starting uncapped; a container that asked for nothing still runs, losing
> only accounting.

### Stage 4 — networking

Every container gets its own network namespace and, by default, a veth pair
plugged into a Forge-managed bridge (`forge0`) with an address from
`10.99.0.0/16` and NAT to the outside world. The bridge and its NAT rules are
created on first use, over raw netlink and nf_tables.

```bash
# The default: a private netns with a real address and a route out
sudo forge run --rootfs /srv/alpine /sbin/ip addr

# The container can reach the internet through the host
sudo forge run --rootfs /srv/alpine /bin/ping -c1 1.1.1.1

# Isolated instead of connected: a netns with nothing in it but loopback
sudo forge run --network none --rootfs /srv/alpine /sbin/ip addr   # → lo only

# Share the host's network stack, which is what Stages 1-3 did
sudo forge run --network host --rootfs /srv/alpine /sbin/ip addr

# Cap the interface MTU (bridge mode only)
sudo forge run --mtu 1400 --rootfs /srv/alpine /sbin/ip link

# The veth and the IP lease go with the container, whatever it exits with
sudo forge run --rootfs /srv/alpine /bin/true
ip link | grep fh                                    # → nothing
ls /var/lib/forge/network/leases                     # → empty
```

> **An address is claimed before the container exists** and the lease records
> the claiming PID, so a `forge` killed with SIGKILL cannot leak an address: the
> next allocation reclaims any lease whose veth *and* whose owner are both gone.

### Stage 5 — OCI images

```bash
# Pull, verify, unpack and run — the first positional is now an image
sudo forge run alpine:3.20 /bin/sh -c 'cat /etc/alpine-release'

# The image supplies the command, the environment and the working directory
sudo forge run alpine:3.20                           # → the image's own CMD

# Fully-qualified references work too
sudo forge run docker.io/library/busybox:latest /bin/echo hi

# Layers are cached by digest: the second run downloads nothing
sudo forge run alpine:3.20 /bin/true
du -sh /var/lib/forge/images

# Flags compose with everything below them
sudo forge run --memory 256m --cpus 1 --network none alpine:3.20 /bin/sh
```

> **Every byte is verified.** The manifest, the config, and each layer are
> checked against their digests — the layer as it is decompressed, so a corrupt
> cached blob is caught, named, quarantined, and re-downloaded rather than
> unpacked. Layer extraction refuses absolute and `../` entry names, rebases
> absolute symlinks against the image root rather than following them onto the
> host, and bounds both the uncompressed size and the entry count of a layer.

### Stage 6 — the runtime

Containers are recorded in a crash-safe on-disk state store, so a second
terminal can see, enter, and stop them. `forge run` is attached and blocks
until the container exits; `--keep` is what leaves a record behind for
`forge ps -a` and `forge rm` afterwards.

```bash
# In one terminal: a container to work with
sudo forge run --keep alpine:3.20 /bin/sh -c 'while :; do date; sleep 1; done'

# In another: what is running
sudo forge ps
# CONTAINER ID   IMAGE          COMMAND        STATUS    CREATED         PID
# 7f3c9a1b2d04   alpine:3.20    /bin/sh -c …   running   12 seconds ago  48213

sudo forge ps -a                     # include containers that have finished
sudo forge ps -q                     # IDs only, for scripting

# Run a second process inside the container's namespaces
sudo forge exec 7f3c9a1b2d04 /bin/ps          # → the container's processes, not the host's
sudo forge exec -workdir /etc 7f3c9a1b2d04 /bin/pwd
sudo forge exec -env FOO=bar 7f3c9a1b2d04 /bin/sh -c 'echo $FOO'

# Everything the container wrote, captured whether or not you were attached
sudo forge logs 7f3c9a1b2d04
sudo forge logs -f 7f3c9a1b2d04               # follow until it exits
sudo forge logs -n 20 -t 7f3c9a1b2d04         # last 20 entries, with timestamps

# SIGTERM, then SIGKILL after the timeout
sudo forge stop 7f3c9a1b2d04
sudo forge stop -t 30 7f3c9a1b2d04
sudo forge stop -rm 7f3c9a1b2d04              # ...and remove it once it is gone

# Remove a stopped container and everything still held for it
sudo forge rm 7f3c9a1b2d04
sudo forge rm -f 7f3c9a1b2d04                 # stop it first if it is still running
```

> **`forge exec` never moves Forge itself.** `setns(2)` changes the namespace of
> the calling *thread*, so the join happens on a thread locked to one goroutine,
> never unlocked, and destroyed when that goroutine returns — with an explicit
> guard against the process's initial thread, which the Go runtime parks rather
> than terminates and whose namespaces are the ones `/proc/self` reports.

### Where Forge keeps things

| Path | Contents |
|---|---|
| `/var/lib/forge/state/containers/<id>/` | `metadata.json` — the container record, plus its lock |
| `/var/lib/forge/containers/<id>/rootfs` | the container's root filesystem |
| `/var/lib/forge/images/` | the content-addressed layer blob cache |
| `/var/lib/forge/logs/<id>` | captured stdout/stderr, one JSON entry per line |
| `/var/lib/forge/network/leases/<ip>` | IP leases, one file per claimed address |
| `/sys/fs/cgroup/forge/<id>/` | the container's cgroup v2 leaf |

`--state-dir`, `--root` and `--image-root` move the first four; the cgroup root
follows the host's unified hierarchy.

---

## Roadmap

Forge is built in six stages. Each stage is a complete, working system in
its own right. **All six are complete.**

| Stage | Focus | Key Mechanisms | Status |
|---|---|---|---|
| 1 | Process Isolation | PID / UTS / mount namespaces | ✅ Complete |
| 2 | Filesystem Isolation | `pivot_root`, bind mounts | ✅ Complete |
| 3 | Resource Limits | cgroups v2 (memory, CPU, pids) | ✅ Complete |
| 4 | Networking | network namespaces, veth, bridge, NAT | ✅ Complete |
| 5 | Images | OCI image pull, unpack, layer cache | ✅ Complete |
| 6 | Runtime | `ps` / `exec` / `stop` / `logs` / `rm`, full lifecycle | ✅ Complete |

Full detail on each stage is in
[docs/PRD.md §13](./docs/PRD.md#13-stage-breakdown), with per-stage design
notes in [docs/stages/](./docs/stages).

## Project Structure

```
forge/
├── cmd/forge/            # CLI entrypoint (wiring only)
├── internal/
│   ├── process/           # process creation & lifecycle
│   ├── namespace/         # namespace creation (clone flags) and entry (setns)
│   ├── rootfs/            # per-container root filesystem directories
│   ├── mount/             # pivot_root, bind mounts, mount cleanup
│   ├── cgroup/            # cgroups v2 management
│   ├── network/           # netns, veth, bridge, NAT, IP leases
│   ├── image/             # OCI registry client, blob cache, layer unpack
│   ├── runtime/           # container orchestration — the conductor
│   ├── state/             # on-disk container records
│   ├── logs/              # captured stdout/stderr
│   ├── cli/               # CLI command implementations
│   └── logging/           # structured logging helpers
├── test/integration/      # privileged integration tests (build tag)
├── docs/
│   ├── adr/               # architecture decision records
│   ├── stages/            # per-stage design notes
│   ├── PRD.md
│   └── SSOT.md
├── Makefile
└── README.md
```

There is no `internal/registry`: the registry client, the blob cache and the
unpacker are one package, because the seam between them was never a seam
anybody crossed ([ADR-0020](./docs/adr/0020-single-image-package.md)).

See [SSOT.md §2](./docs/SSOT.md#2-package-layout--responsibilities) for a full
description of each package's responsibilities and boundaries.

## Development Philosophy

- **Standard library first.** New dependencies require an ADR justifying
  them (see [SSOT.md §10](./docs/SSOT.md#10-dependency-rules)). Forge has
  exactly one: `golang.org/x/sys/unix`, for syscalls the frozen `syscall`
  package will not grow
  ([ADR-0013](./docs/adr/0013-golang-x-sys-dependency.md)).
- **Test-driven.** Tests are written alongside — ideally before —
  implementation. See [SSOT.md §7](./docs/SSOT.md#7-testing-strategy).
- **One package, one kernel mechanism.** The package layout mirrors the
  underlying Linux primitives so the codebase doubles as documentation.
- **No shortcuts through existing runtimes.** Forge never shells out to
  `docker`, `runc`, or `containerd` — that would defeat the project's
  purpose.
- **Clean up after yourself.** Every kernel resource Forge creates
  (namespace, cgroup, mount, veth) has a corresponding, idempotent teardown
  path, and this is tested.

The full set of engineering invariants is documented in
[SSOT.md §13](./docs/SSOT.md#13-engineering-invariants) and should not be
violated without an explicit ADR.

## Known limitations

Forge is conceptually complete, not production-hardened. The gaps that matter
most, all of them deliberate:

- **No user namespaces.** A container's `root` is the host's `root`. This is
  the single largest difference between Forge and a runtime you would trust
  with someone else's code.
- **No seccomp, AppArmor or SELinux.** A container may make any syscall its
  uid allows.
- **Device nodes come from the image.** A layer declaring a block device gets
  one, with the major/minor it asked for. Docker restricts this; Forge does
  not.
- **IPv4 only**, single host, one bridge.
- **`forge run` is attached only.** There is no `-d`; a container's lifetime is
  its `forge run`. `--keep` is what leaves a record for `forge ps -a` and
  `forge rm` after it exits.
- **`forge ps` reports what the records say** and does not reconcile them
  against the kernel, so a container whose `forge run` was killed still reads
  `running` until `forge stop` or `forge rm` meets it.

## Contributing

Contributions are welcome. Before opening a PR:

1. Read [docs/SSOT.md](./docs/SSOT.md) — it is the authoritative architecture
   reference and PR reviews will be checked against it.
2. If your change affects package boundaries, architecture, or the
   engineering invariants, add or update an ADR in `docs/adr/`.
3. Run `make test lint` locally before pushing.
4. Keep PRs scoped to a single stage or a single well-defined improvement —
   large, cross-cutting PRs are hard to review against the SSOT.

## License

[MIT](./LICENSE)
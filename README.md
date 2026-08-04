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
  bridge, and NAT for outbound connectivity.
- **OCI images** — pull, verify, cache, and unpack standard OCI images from
  any compatible registry.
- **A real CLI** — `forge run`, `forge ps`, `forge exec`, `forge stop`, `forge logs`, `forge rm`.

## Project Goals

- Prioritize **correctness, readability, and testability** over feature
  breadth.
- Use the **Go standard library** wherever possible; keep third-party
  dependencies minimal and justified.
- Build in **small, sequential stages**, where every stage is a complete,
  working, tested system.
- Make the mapping from **kernel concept → Go package** obvious.

See [PRD.md](./PRD.md) for the full product requirements and
[SSOT.md](./SSOT.md) for the engineering architecture reference.

## Non-goals

Forge intentionally does not implement: Dockerfile-style image building,
multi-host orchestration, rootless containers, seccomp/AppArmor/SELinux
integration, or a plugin system. See [PRD.md §5](./PRD.md#5-non-goals) for the
complete list and rationale.

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

> **Stage 3 is the current stage.** `forge run` takes a **path to a binary**,
> not an image reference — images arrive in Stage 5. Without `-rootfs` that path
> is on the host and the container shares the host's filesystem, exactly as in
> Stage 1. With `-rootfs` it is a path *inside* the container's own root
> filesystem. The commands listed under later stages below do not exist yet;
> `forge -h` always lists exactly what is implemented.

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

Later stages will add:

```bash
sudo forge run alpine:3.20 /bin/sh        # Stage 5: OCI images
sudo forge ps                             # Stage 6
sudo forge exec <container-id> /bin/ps    # Stage 6
sudo forge logs -f <container-id>         # Stage 6
sudo forge stop <container-id>            # Stage 6
sudo forge rm <container-id>              # Stage 6
```

---

## Roadmap

Forge is built in six stages. Each stage is a complete, working system in
its own right.

| Stage | Focus | Key Mechanisms | Status |
|---|---|---|---|
| 1 | Process Isolation | PID / UTS / mount namespaces | ✅ Complete |
| 2 | Filesystem Isolation | `pivot_root`, bind mounts | ✅ Complete |
| 3 | Resource Limits | cgroups v2 (memory, CPU, pids) | ✅ Complete |
| 4 | Networking | network namespaces, veth, bridge, NAT | Not started |
| 5 | Images | OCI image pull, unpack, layer cache | Not started |
| 6 | Runtime | `ps` / `exec` / `stop` / `logs`, full lifecycle | Not started |

Full detail on each stage is in [PRD.md §13](./PRD.md#13-stage-breakdown).

## Project Structure

```
forge/
├── cmd/forge/          # CLI entrypoint
├── internal/
│   ├── process/         # process creation & lifecycle
│   ├── namespace/       # namespace setup, in-child namespace configuration
│   ├── rootfs/          # root filesystem management
│   ├── mount/           # pivot_root, bind mounts
│   ├── cgroup/          # cgroups v2 management
│   ├── network/         # netns, veth, bridge, NAT
│   ├── image/           # OCI image unpack & layering
│   ├── registry/        # OCI registry client
│   ├── runtime/         # container orchestration
│   ├── state/           # on-disk state store
│   └── cli/             # CLI command implementations
├── test/integration/    # privileged integration tests
├── docs/adr/             # architecture decision records
├── PRD.md
├── SSOT.md
└── README.md
```

See [SSOT.md §2](./SSOT.md#2-package-layout--responsibilities) for a full
description of each package's responsibilities and boundaries.

## Development Philosophy

- **Standard library first.** New dependencies require an ADR justifying
  them (see [SSOT.md §10](./SSOT.md#10-dependency-rules)). Forge has exactly
  one: `golang.org/x/sys/unix`, for syscalls the frozen `syscall` package will
  not grow ([ADR-0013](./docs/adr/0013-golang-x-sys-dependency.md)).
- **Test-driven.** Tests are written alongside — ideally before —
  implementation. See [SSOT.md §7](./SSOT.md#7-testing-strategy).
- **One package, one kernel mechanism.** The package layout mirrors the
  underlying Linux primitives so the codebase doubles as documentation.
- **No shortcuts through existing runtimes.** Forge never shells out to
  `docker`, `runc`, or `containerd` — that would defeat the project's
  purpose.
- **Clean up after yourself.** Every kernel resource Forge creates
  (namespace, cgroup, mount, veth) has a corresponding, idempotent teardown
  path, and this is tested.

The full set of engineering invariants is documented in
[SSOT.md §13](./SSOT.md#13-engineering-invariants) and should not be
violated without an explicit ADR.

## Contributing

Contributions are welcome. Before opening a PR:

1. Read [SSOT.md](./SSOT.md) — it is the authoritative architecture
   reference and PR reviews will be checked against it.
2. If your change affects package boundaries, architecture, or the
   engineering invariants, add or update an ADR in `docs/adr/`.
3. Run `make test lint` locally before pushing.
4. Keep PRs scoped to a single stage or a single well-defined improvement —
   large, cross-cutting PRs are hard to review against the SSOT.

## License

[MIT](./LICENSE)
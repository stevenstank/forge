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
git clone https://github.com/<org>/forge.git
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

> **Stage 1 is the current stage.** `forge run` takes a **path to a binary on
> the host**, not an image reference — images arrive in Stage 5. The command
> runs as PID 1 inside new PID, UTS and mount namespaces. The commands listed
> under later stages below do not exist yet; `forge -h` always lists exactly
> what is implemented.

```bash
# Run a command in an isolated container
sudo forge run /bin/echo "hello from forge"

# The container's init process is PID 1 in its own namespace
sudo forge run /bin/sh -c 'echo "I am pid $$"'      # → I am pid 1

# Give the container a hostname (default: the container ID)
sudo forge run --hostname sandbox /bin/hostname

# Mounts made inside the container never reach the host
sudo forge run /bin/sh -c 'mount -t tmpfs tmpfs /mnt && ls /mnt'

# forge exits with the container's exit status
sudo forge run /bin/sh -c 'exit 42'; echo $?        # → 42

# Turn up the detail
sudo forge --log-level debug run /bin/true
```

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
| 2 | Filesystem Isolation | `pivot_root`, bind mounts | Not started |
| 3 | Resource Limits | cgroups v2 (memory, CPU, pids) | Not started |
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
  them (see [SSOT.md §10](./SSOT.md#10-dependency-rules)).
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
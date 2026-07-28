# Forge — Product Requirements Document (PRD)

**Status:** Draft v1.0
**Owner:** Systems Engineering
**Audience:** Contributors, maintainers, reviewers

---

## 1. Executive Summary

Forge is a Docker-inspired container runtime built from scratch in Go. It is an
educational systems project whose purpose is to demonstrate — through working,
tested code — how Linux containers actually work: namespaces, cgroups,
filesystem isolation, networking, and the OCI image format.

Forge is not a Docker replacement and does not target feature parity with
Docker, containerd, or Podman. It targets **conceptual completeness**: every
major mechanism that makes a "container" possible should exist in Forge, in a
minimal, readable, correct form.

The project is developed in six sequential stages, each producing a working,
testable system that is a strict superset of the previous stage.

---

## 2. Vision

Most engineers use containers daily without understanding the primitives
underneath them — namespaces, cgroups, pivot_root, veth pairs, OCI layers.
Existing container runtimes (Docker, containerd, runc, Podman) are excellent
production software but poor learning artifacts: their codebases are large,
layered with backward-compatibility concerns, plugin systems, and years of
edge-case handling.

Forge exists to close that gap. It is the container runtime a systems
engineer would build to teach themselves — and others — how containers work,
written with production-quality software engineering practices (clean
architecture, TDD, strong typing, minimal dependencies) but without
production-scope ambitions (no swarm mode, no plugin ecosystem, no Windows
support, no multi-tenant orchestration).

Forge succeeds if a reader can go from `forge run` to a full understanding of
PID namespaces, cgroups v2, pivot_root, veth/bridge networking, and OCI image
unpacking, by reading the source code stage by stage.

---

## 3. Problem Statement

Container internals are conceptually simple but operationally scattered
across the Linux kernel, cli tools (`iproute2`, `unshare`, `nsenter`), and
large codebases. There is no small, sequentially-staged, test-driven Go
project that:

1. Builds each isolation primitive independently and explains why it exists.
2. Uses only the Go standard library and minimal, well-justified dependencies.
3. Is structured so each stage is a complete, runnable, testable system.
4. Follows real engineering discipline (interfaces, error handling, logging,
   testing) rather than being a collection of disconnected scripts or gists.

Forge fills this gap.

---

## 4. Goals

- Implement real Linux process isolation using namespaces (PID, UTS, mount,
  network) via direct syscalls, not by shelling out to `docker` or `runc`.
- Implement real filesystem isolation using `pivot_root` and bind mounts.
- Implement real resource control using cgroups v2.
- Implement real container networking using network namespaces, veth pairs,
  a Linux bridge, and NAT via iptables/nftables rules.
- Support pulling and unpacking OCI-compliant container images.
- Provide a coherent CLI (`forge run`, `forge ps`, `forge exec`, `forge stop`,
  `forge logs`) that ties every stage together into a usable runtime.
- Maintain a codebase that is idiomatic Go, has strong test coverage, and is
  approachable to a mid-level engineer with basic Linux knowledge.
- Document every architectural decision so the project can be extended
  without drifting from its original design intent.

## 5. Non-Goals

- **Feature parity with Docker/containerd/Podman.** No Dockerfile builder,
  no BuildKit, no Docker Compose equivalent, no Swarm/Kubernetes integration.
- **Multi-host orchestration.** Forge manages containers on a single host.
- **Windows or macOS native containers.** Forge targets Linux only (x86_64
  and arm64). Development on macOS/Windows must use a Linux VM.
- **Rootless containers** in early stages. Rootless support may be explored
  as a future enhancement (see §14) but is out of scope for Stages 1–6.
- **A plugin/extension system.** Forge is a monolith by design; extensibility
  is achieved by reading and modifying the source, not by runtime plugins.
- **Security hardening to production standards.** Forge does not implement
  seccomp profiles, AppArmor/SELinux integration, or user namespace remapping
  in its initial scope. This is explicitly called out as a limitation.
- **Image building.** Forge consumes existing OCI images; it does not build
  new images from a Dockerfile-like spec.
- **High availability / restart policies / health checks.** These are
  orchestration-layer concerns and out of scope.

---

## 6. Target Audience

- **Primary:** Systems/infrastructure engineers who want to deeply understand
  container internals by building one.
- **Secondary:** Go engineers looking for a substantial, well-architected
  systems project to study or contribute to.
- **Tertiary:** Educators/students using Forge as teaching material for
  operating systems, Linux internals, or distributed systems courses.

Forge assumes the reader/contributor is comfortable with Go, has basic Linux
command-line experience, and is willing to work in a Linux environment
(native, VM, or cloud instance) since kernel namespace/cgroup features are
not available on macOS or Windows directly.

---

## 7. Core Concepts

| Concept | Definition in Forge's context |
|---|---|
| **Container** | A process tree isolated via Linux namespaces, constrained via cgroups, and rooted at an isolated filesystem. |
| **Namespace** | A kernel mechanism that scopes a global system resource (PIDs, hostnames, mounts, network stacks) so a process sees an isolated view of it. |
| **cgroup** | A kernel mechanism (v2) used to limit, account for, and isolate resource usage (CPU, memory, PIDs) of a process group. |
| **Root filesystem (rootfs)** | The filesystem tree a container process sees as `/`, constructed from an unpacked OCI image and made the process's root via `pivot_root`. |
| **OCI Image** | A filesystem + config bundle following the [OCI Image Format Specification](https://github.com/opencontainers/image-spec), consisting of a manifest, config, and one or more layer tarballs. |
| **Container Runtime** | The userspace program (Forge) responsible for creating, running, monitoring, and destroying containers by orchestrating the above primitives. |
| **Bridge Network** | A virtual Linux switch (`bridge` device) connecting container veth interfaces to each other and, via NAT, to the host's network. |

---

## 8. Functional Requirements

### 8.1 Stage 1 — Process Isolation
- FR-1.1: `forge run <path> [args...]` executes a binary inside a new process
  tree isolated via `CLONE_NEWPID`.
- FR-1.2: The container process has an isolated hostname via `CLONE_NEWUTS`.
- FR-1.3: The container process has an isolated mount table via
  `CLONE_NEWNS`, such that mounts inside the container do not propagate to
  the host.
- FR-1.4: Forge tracks the lifecycle of the container process (start, running,
  exited) and reports its exit code.
- FR-1.5: Forge cleans up all resources (namespaces are released by the
  kernel automatically once unreferenced) on process exit.

### 8.2 Stage 2 — Filesystem Isolation
- FR-2.1: Each container has its own root filesystem, established via
  `pivot_root`, not `chroot` (chroot is documented as insufficient and used
  only for comparison/teaching purposes).
- FR-2.2: Forge supports bind-mounting host directories into the container.
- FR-2.3: Forge unmounts and cleans up all mounts created for a container
  when that container terminates, leaving no orphaned mounts on the host.
- FR-2.4: Forge manages a per-container root filesystem directory on disk
  under a predictable, documented path.

### 8.3 Stage 3 — Resource Limits
- FR-3.1: Forge creates a cgroup v2 leaf per container.
- FR-3.2: Forge supports memory limits (`memory.max`).
- FR-3.3: Forge supports CPU limits (`cpu.max`, weight-based or quota-based).
- FR-3.4: Forge supports process count limits (`pids.max`).
- FR-3.5: Forge removes the cgroup when the container exits.

### 8.4 Stage 4 — Networking
- FR-4.1: Each container is given its own network namespace
  (`CLONE_NEWNET`).
- FR-4.2: Forge creates a veth pair per container, with one end in the
  container's netns and the other attached to a Forge-managed bridge on the
  host.
- FR-4.3: Forge assigns each container an IP address from a configurable
  private subnet.
- FR-4.4: Forge configures NAT (via nftables or iptables) so containers can
  reach the internet through the host.
- FR-4.5: Forge tears down veth pairs and IP allocations when a container
  exits.

### 8.5 Stage 5 — Images
- FR-5.1: Forge pulls OCI images from a registry (e.g. Docker Hub / any
  OCI Distribution Spec–compliant registry) given a reference like
  `alpine:3.20`.
- FR-5.2: Forge verifies pulled content against manifest digests.
- FR-5.3: Forge unpacks image layers into a per-container rootfs using
  overlay-style layering (implemented via OverlayFS or explicit layer
  extraction — decision recorded as an ADR).
- FR-5.4: Forge caches downloaded layers by digest to avoid redundant pulls.
- FR-5.5: `forge run <image> <cmd> [args...]` runs a command inside a
  container created from a named image, replacing the Stage 1–4 requirement
  of a pre-existing local rootfs path.

### 8.6 Stage 6 — Runtime
- FR-6.1: `forge ps` lists running (and optionally stopped) containers with
  ID, image, command, status, and creation time.
- FR-6.2: `forge exec <container-id> <cmd> [args...]` runs an additional
  process inside an already-running container's namespaces.
- FR-6.3: `forge stop <container-id>` gracefully terminates a running
  container (SIGTERM, then SIGKILL after a timeout).
- FR-6.4: `forge logs <container-id>` streams stdout/stderr captured from the
  container process.
- FR-6.5: Forge persists container state (metadata) across runtime restarts,
  under a documented state directory.
- FR-6.6: Forge provides a `forge rm` for cleanup of stopped containers, and
  full cleanup on `forge stop --rm` or equivalent.

---

## 9. Non-Functional Requirements

- **NFR-1 Correctness over completeness.** A smaller, correct feature set is
  preferred over a larger, partially-correct one.
- **NFR-2 Readability.** Code must be understandable by a mid-level Go
  engineer without prior container-runtime experience. Prefer explicit code
  over clever abstractions.
- **NFR-3 Testability.** All packages must be unit-testable without root
  privileges where possible; integration tests that require root/Linux
  kernel features must be clearly separated (build tags) and documented.
- **NFR-4 Minimal dependencies.** Standard library first. Third-party
  dependencies require an ADR justifying their inclusion (see SSOT §10).
- **NFR-5 Idempotent cleanup.** Any resource Forge creates (namespace,
  cgroup, mount, veth, IP allocation) must be reliably removable, including
  after a crash (best-effort reconciliation on startup).
- **NFR-6 Platform constraint.** Forge targets Linux kernel 5.10+ (cgroups v2
  unified hierarchy required), x86_64 and arm64.
- **NFR-7 Observability.** All state-changing operations must be logged at
  an appropriate level (see SSOT §7) with enough context to debug failures
  without attaching a debugger.
- **NFR-8 Fail-safe defaults.** Operations that partially fail must not leave
  the host in a worse state than before the operation began (e.g., partial
  namespace setup must roll back).

---

## 10. Success Criteria

Forge Stage N is considered successful when:

1. All functional requirements for that stage are implemented and covered by
   automated tests (unit + integration where applicable).
2. A contributor can, from a clean Linux VM, clone the repo, run
   `make test`, and get a green build.
3. A contributor can run the stage's example commands from the README and
   observe the documented behavior.
4. No manual cleanup is required after running the test suite or example
   commands — all kernel resources are released.
5. The stage's SSOT sections (architecture, invariants) are updated to
   reflect what was actually built.

The project as a whole is successful when all six stages are complete and a
user can run `forge run alpine:3.20 /bin/sh`, land in an isolated, resource
limited, networked shell, and exit cleanly with no host-side residue.

---

## 11. Risks

| Risk | Impact | Mitigation |
|---|---|---|
| Namespace/cgroup APIs require root or specific capabilities, complicating CI | Tests fail or are skipped in constrained environments | Use privileged Linux CI runners or containers-in-containers; gate integration tests behind build tags and document required capabilities |
| Kernel version differences (cgroups v1 vs v2, syscall availability) | Inconsistent behavior across dev environments | Pin a minimum kernel version (5.10+), detect and fail fast with a clear error if the unified cgroup hierarchy isn't available |
| Networking stage complexity (veth, bridge, NAT) is a common source of bugs | Stage 4 slips schedule, flaky tests | Build a minimal integration test harness using network namespaces without requiring physical network access; document manual verification steps |
| OCI registry interactions (auth, manifest lists, multi-arch) add scope creep | Stage 5 balloons beyond "educational" intent | Explicitly scope to single-arch, anonymous-pull-compatible registries (e.g. public Docker Hub images) for v1; document multi-arch/auth as future work |
| Contributors add dependencies or abstractions that erode readability | Architectural drift, harder onboarding | SSOT ADR process required for new dependencies or structural changes; PR review checklist enforces this |
| Security corners cut for simplicity are mistaken for production-readiness | Misuse in non-educational contexts | README and PRD explicitly and repeatedly state Forge is not production-hardened |

---

## 12. Milestones

| Milestone | Description | Exit Criteria |
|---|---|---|
| M0 | Project scaffolding | Repo structure, CI, Makefile, empty CLI skeleton in place |
| M1 | Stage 1 complete | Process isolation working, tested, documented |
| M2 | Stage 2 complete | Filesystem isolation working, tested, documented |
| M3 | Stage 3 complete | cgroups v2 resource limits working, tested, documented |
| M4 | Stage 4 complete | Networking (netns, veth, bridge, NAT) working, tested, documented |
| M5 | Stage 5 complete | OCI image pull/unpack working, tested, documented |
| M6 | Stage 6 complete | Full CLI (ps/exec/stop/logs) working; Forge is a usable runtime |

---

## 13. Stage Breakdown

Each stage is developed as an independently mergeable increment. A stage is
not considered "done" until:

- Its functional requirements (§8) are implemented.
- Unit and integration tests pass.
- SSOT architecture/invariants sections are updated.
- README roadmap/example commands are updated.
- A stage-specific ADR exists for any non-obvious design decision made
  during implementation.

| Stage | Name | Primary Kernel Mechanisms | Primary New Packages |
|---|---|---|---|
| 1 | Process Isolation | `clone(2)`, PID/UTS/mount namespaces | `internal/namespace`, `internal/process` |
| 2 | Filesystem Isolation | `pivot_root(2)`, bind mounts | `internal/rootfs`, `internal/mount` |
| 3 | Resource Limits | cgroups v2 | `internal/cgroup` |
| 4 | Networking | `CLONE_NEWNET`, veth, bridge, nftables/iptables | `internal/network` |
| 5 | Images | OCI Image/Distribution specs | `internal/image`, `internal/registry` |
| 6 | Runtime | process supervision, IPC, state store | `internal/runtime`, `internal/state`, `cmd/forge` (expanded) |

See SSOT §2–3 for exact package responsibilities.

---

## 14. Future Enhancements

Explicitly out of scope for v1 but recorded as plausible future work:

- Rootless container support (user namespace remapping).
- seccomp-bpf syscall filtering.
- Checkpoint/restore (CRIU integration).
- A minimal Dockerfile-compatible image builder.
- OCI Runtime Spec compliance (so Forge could theoretically be driven by
  containerd as a shim) — tracked as a stretch goal, not a requirement.
- Multi-host networking (overlay networks).
- A TUI or web dashboard for container inspection.
- Windows/WSL2 development support.

These are not scheduled and should not influence Stage 1–6 architecture
except insofar as the architecture should not actively preclude them.
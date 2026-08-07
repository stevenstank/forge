# Forge — Single Source of Truth (SSOT)

**Status:** Living document. Update in the same PR as any architectural change.
**Purpose:** This is the authoritative engineering reference for Forge. If
code and this document disagree, that is a bug — in the code or in the
document — and must be resolved before merge.

---

## 1. Repository Structure

```
forge/
├── cmd/
│   └── forge/                # CLI entrypoint (main package)
│       └── main.go
├── internal/
│   ├── process/               # Stage 1: process creation & lifecycle
│   ├── namespace/              # Stage 1: namespace setup (PID, UTS, mount, net)
│   ├── rootfs/                 # Stage 2: root filesystem management
│   ├── mount/                  # Stage 2: mount/unmount, pivot_root
│   ├── cgroup/                  # Stage 3: cgroups v2 management
│   ├── network/                 # Stage 4: netns, veth, bridge, NAT
│   ├── image/                   # Stage 5: OCI image handling, layer unpack
│   ├── registry/                # Stage 5: registry client (pull, auth, manifest)
│   ├── runtime/                  # Stage 6: container orchestration/supervision
│   ├── state/                    # Stage 6: on-disk state store
│   ├── cli/                       # CLI command implementations (thin layer over runtime)
│   └── logging/                    # Structured logging helpers
├── pkg/
│   └── oci/                         # Public, reusable OCI spec types (if extracted)
├── test/
│   ├── integration/                  # Root/privileged integration tests (build-tagged)
│   └── testutil/                      # Shared test helpers, fixtures
├── docs/
│   ├── adr/                            # Architecture Decision Records
│   └── stages/                          # Per-stage design notes
├── scripts/                              # Dev/CI helper scripts
├── Makefile
├── go.mod
├── go.sum
├── PRD.md
├── SSOT.md
└── README.md
```

**Rule:** `internal/` packages are never imported outside the module. `pkg/`
is reserved for types genuinely useful to external consumers (e.g. OCI spec
structs) and must remain dependency-light. Until there is a concrete external
consumer, prefer `internal/`.

---

## 2. Package Layout & Responsibilities

| Package | Responsibility | Must NOT do |
|---|---|---|
| `internal/process` | Fork/exec the container's init process via `clone(2)`; manage its lifecycle (start, wait, signal, exit code) | Know about images, networking, or cgroups directly — receives fully-prepared config |
| `internal/namespace` | Compute and apply `CloneFlags` for PID/UTS/mount/net namespaces; in-child namespace setup (`sethostname`, marking the mount tree `MS_REC\|MS_PRIVATE`); namespace-entry helpers (`setns`) for `forge exec` | Perform filesystem or network *configuration* — mounting filesystems, writing files, assigning addresses. Setting a namespace's mount *propagation* is namespace creation, not filesystem configuration: without it `CLONE_NEWNS` does not isolate (ADR-0008) |
| `internal/rootfs` | Prepare a container's root filesystem directory; validate/own the rootfs path | Unpack OCI layers, or know what an image is. It owns the *directory*; `internal/image` owns the *contents*, and `internal/runtime` sequences the two (ADR-0020) |
| `internal/mount` | `pivot_root`, bind mounts, mount table cleanup | Decide *what* to mount — receives a mount plan from the caller |
| `internal/cgroup` | Create/configure/destroy cgroup v2 leaves; write resource limits | Interpret CLI flags — receives a typed `ResourceLimits` struct |
| `internal/network` | netns creation, veth pair creation, bridge attachment, IP allocation, NAT rules | Own container lifecycle — only owns networking resources for a given container ID |
| `internal/image` | Everything between an image reference and a populated directory: reference parsing, the OCI Distribution Spec client (anonymous auth, manifest fetch, blob download), digest verification, the content-addressed blob cache, layer unpacking (ADR-0020) | Know what a container is — it is handed a destination directory and writes into it. Decide *which* image, *which* platform, or *where* the rootfs goes: those are `internal/runtime`'s |
| `internal/runtime` | Orchestrate the above packages to implement container lifecycle (create, start, stop, exec, remove); the "conductor". Also owns container ID generation (§8) and `Init`, the container-side entry point Forge re-executes itself as (ADR-0008). First appears in **Stage 1** — orchestration has to live somewhere from the first stage, and §13.2 says it lives here (ADR-0007) — and grows through Stage 6 | Contain namespace/cgroup/mount syscall logic itself — always delegates |
| `internal/state` | Persist and query container metadata (ID, PID, status, image, timestamps) on disk | Perform any kernel resource management |
| `internal/cli` | Parse CLI args/flags, call into `internal/runtime`, format output | Contain business logic — CLI packages are intentionally "dumb" |
| `internal/logging` | Structured logger construction, log level configuration | N/A (leaf package, no internal dependencies) |

**Dependency rule (strict, see §9):** `internal/runtime` may depend on every
other `internal/*` package. No other package may depend on `internal/runtime`.
Leaf packages (`process`, `namespace`, `rootfs`, `mount`, `cgroup`, `network`,
`image`, `state`, `logging`) must not depend on each other. There are no
exceptions: the two this document previously allowed, `rootfs` → `image` and
`image` → `registry`, were both retired by ADR-0020 when Stage 5 was
implemented.

---

## 3. Architecture Overview

Forge follows a **layered, orchestrator-delegates-to-primitives** architecture:

```
                     ┌─────────────┐
                     │  cmd/forge  │   (main, wiring only)
                     └──────┬──────┘
                            │
                     ┌──────▼──────┐
                     │ internal/cli│   (flag parsing, output formatting)
                     └──────┬──────┘
                            │
                     ┌──────▼───────┐
                     │internal/     │   (orchestration / lifecycle)
                     │runtime       │
                     └──┬───┬───┬───┘
           ┌────────────┘   │   └─────────────┐
           │                │                  │
    ┌──────▼─────┐   ┌──────▼──────┐   ┌───────▼───────┐   ┌──────▼──────┐
    │ namespace/  │   │  cgroup/    │   │   network/    │   │   image/    │
    │ process/    │   │             │   │               │   │             │
    │ rootfs/     │   │             │   │               │   │             │
    │ mount/      │   │             │   │               │   │             │
    └─────────────┘   └─────────────┘   └───────────────┘   └─────────────┘
```

Every primitive is a leaf, including `image`: it hangs off `runtime` beside the
others rather than under `rootfs`, because `rootfs` owns a container's directory
and `image` owns the bytes written into one (ADR-0020).

Each primitive package wraps a specific kernel/OS mechanism and exposes a
narrow, typed Go API. `internal/runtime` composes these into the container
lifecycle. `internal/state` is a side-channel used by `runtime` to persist
and recover state; it does not sit "in" the request path of container
creation.

### 3.1 Design Principles

1. **One package, one kernel mechanism.** Each Stage 1–4 package corresponds
   to a distinct Linux mechanism. This keeps the mapping from "concept" to
   "code" obvious for learners.
2. **Orchestration is centralized.** All cross-package sequencing lives in
   `internal/runtime`. Primitive packages never call each other directly —
   without exception, since ADR-0020.
3. **Config in, resource out.** Every primitive package takes a fully-formed
   configuration struct and returns a handle/resource (or error). No package
   reads global state, environment variables, or CLI flags directly except
   `internal/cli`.
4. **Explicit over implicit.** No reflection-based config, no hidden magic
   defaults. If a default exists, it is a named constant with a comment
   explaining why.
5. **Fail loudly, clean up quietly.** Errors propagate up immediately
   (no swallowed errors). Cleanup (namespace teardown, cgroup removal, mount
   unmounting) is handled via `defer` and idempotent `Cleanup()` methods, and
   is best-effort even when the primary operation failed.

---

## 4. Coding Conventions

- **Go version:** 1.24+. Use generics only where they measurably reduce
  duplication (e.g., generic set/slice helpers); do not use generics for
  their own sake.
- **Formatting:** `gofmt`/`goimports` enforced via CI; no manual formatting
  debates.
- **Linting:** `golangci-lint` with a checked-in config (`.golangci.yml`).
  CI fails on lint errors.
- **Error wrapping:** always wrap with context using `fmt.Errorf("...: %w",
  err)`. Never discard an error silently; use `_ = x` only for genuinely
  ignorable cleanup errors, and log them.
- **Interfaces:** defined at the point of use (consumer-side), not
  preemptively in the producer package. Do not create an interface until at
  least two concrete implementations or a clear test-seam need exists.
- **Constructors:** exported types requiring setup use `New<Type>(...)
  (*Type, error)` constructors; avoid exported structs with required-but-
  zero-value-unsafe fields.
- **Context:** `context.Context` is the first parameter of any function that
  performs I/O, blocks, or could be long-running (mount operations, registry
  calls, process wait). Not required for pure computation.
- **No global mutable state.** No package-level `var` holding mutable
  runtime state. Dependency inject everything (loggers, config, clients).
- **Comments:** exported identifiers require doc comments. Comments explain
  *why*, not *what*, where the "what" isn't already obvious from the code.

---

## 5. Error Handling Strategy

- Forge defines a small set of **sentinel errors** in each primitive package
  for conditions callers may need to branch on (e.g.,
  `namespace.ErrUnsupportedKernel`, `cgroup.ErrControllerUnavailable`).
- All other errors are wrapped, not sentinel — callers should not need to
  string-match error messages.
- `internal/runtime` is responsible for **rollback on partial failure**: if
  container creation fails after namespaces are set up but before cgroups
  are applied, runtime must tear down the namespaces before returning the
  error to the caller.
- Errors that occur during best-effort cleanup are logged at `WARN` and do
  not mask the original error returned to the caller (the original error is
  always the one surfaced; cleanup errors are logged as auxiliary context).
- CLI-layer error output is human-readable; internal layers never format
  errors for terminal display (no ANSI codes, no "Error: " prefixes below
  the CLI layer).

---

## 6. Logging Strategy

- Structured logging via the standard library `log/slog` (Go 1.21+),
  configured once in `internal/logging` and injected into every package that
  needs it.
- Log levels:
  - `DEBUG`: syscall-level detail (exact clone flags, mount options, cgroup
    file writes). Off by default.
  - `INFO`: lifecycle events (container created, started, stopped, removed).
  - `WARN`: recoverable issues (best-effort cleanup failure, retryable
    registry error).
  - `ERROR`: operation failed and is being returned to the caller.
- Every log line for a container operation includes a `container_id` field
  for correlation.
- No `fmt.Println`/`fmt.Printf` for diagnostic output anywhere outside
  `internal/cli` (which owns user-facing stdout formatting).

---

## 7. Testing Strategy

- **Unit tests** (`_test.go`, no build tag): must run without root and
  without special kernel features. Achieved by testing pure logic (flag
  computation, config validation, state serialization) and by mocking
  syscall-boundary interfaces where needed.
- **Integration tests** (`test/integration`, build tag `//go:build
  integration`): require root and a Linux kernel with namespaces/cgroups v2
  enabled. Run in CI via a privileged container/VM runner. Each test cleans
  up all kernel resources it creates, even on failure (`t.Cleanup`).
- **Table-driven tests** are the default style for anything with multiple
  input/output cases.
- **TDD is expected**, not optional: a stage's tests are written alongside
  (ideally before) the implementation. PRs that add functional behavior
  without corresponding tests are not merged.
- **Coverage** is tracked but not gated on an arbitrary percentage; gated
  instead on "every exported function has at least one test exercising its
  primary success path and its primary failure path."
- **No sleeps in tests.** Use explicit synchronization (channels, polling
  with timeout) rather than `time.Sleep` to wait for process/state changes.

---

## 8. Naming Conventions

- Packages: short, lowercase, no underscores (`cgroup`, not `c_group` or
  `cGroup`).
- Files: `snake_case.go`; test files `snake_case_test.go`.
- Container IDs: 12-character lowercase hex (like Docker's short ID), encoding
  48 bits read from a CSPRNG — documented in `internal/runtime` (ADR-0005).
- CLI commands: verbs (`run`, `stop`, `exec`, `ps`, `logs`, `rm`), matching
  Docker's vocabulary intentionally, since it is the vocabulary users already
  know — this is the one place Forge deliberately mirrors Docker, for
  usability, not implementation.
- Exported types describing configuration end in `Config` (e.g.,
  `namespace.Config`); exported types describing runtime handles end in
  nothing special but are documented as handles (e.g., `process.Process`).

---

## 9. CLI Conventions

- Root command: `forge`.
- Global flags: `--log-level`, `--state-dir` (default
  `/var/lib/forge`), `--root` (rootfs storage root, default
  `/var/lib/forge/containers`), `--image-root` (blob cache root, default
  `/var/lib/forge/images`, arrives with Stage 5's runtime integration). `--root`
  and `--image-root` are independent so they can sit on different filesystems.
- Subcommands (final set, delivered incrementally by stage):
  - `forge run [flags] <image|rootfs-path> [cmd] [args...]`
    - **Until Stage 5**, the root filesystem is named by a `-rootfs` *flag*
      rather than positionally, and the positional argument is the command:
      `forge run -rootfs <dir> <cmd> [args...]`. Omitting `-rootfs` runs the
      command against the host's filesystem, which is Stage 1's behaviour and
      remains valid. The positional `<image>` form arrives with images in
      Stage 5, when there is a reference to resolve.
    - Stage 2 flags: `-rootfs`, `-mount src:dst[:opts]` (repeatable),
      `-read-only`, `-workdir`.
  - `forge ps [-a]`
  - `forge exec <container-id> <cmd> [args...]`
  - `forge stop [-t timeout] <container-id>`
  - `forge logs [-f] <container-id>`
  - `forge rm <container-id>`
- Internal commands, prefixed `__`, are hidden from help and are not part of
  the user-facing surface. The only one is `__init`, which Forge re-executes
  itself as to set up a container from inside its namespaces (ADR-0008).
- All commands that mutate state print the affected container ID to stdout
  on success (scriptable, Docker-like convention) and write human-readable
  detail to stderr.
  - **Exception — `forge run` in attached mode** (ADR-0009): stdout belongs to
    the container, so Forge writes nothing to it. The container ID is reported
    on stderr via the `container_id` log field. When Stage 6 adds `-d`, the
    detached path follows the general rule.
- Non-zero exit codes are used consistently: `1` for user error (bad flags,
  unknown container), `2` for internal/unexpected error.
  - **Exception — `forge run`** (ADR-0009): a container that runs is reported
    by propagating its own exit status, as FR-1.4 requires and as Docker does;
    a container killed by a signal reports `128+signal`; a container that could
    not be started at all reports `127`. Codes `1` and `2` keep their meaning
    for failures originating in Forge rather than the container.

---

## 10. Dependency Rules

- **Standard library first**, always. A new third-party dependency requires
  an ADR (§12) answering: what does the stdlib not provide, why is this
  library the right choice, what is the maintenance/security posture of the
  library.
- Pre-approved dependency categories (still require an ADR to actually add,
  but are expected over the project's life):
  - A CLI framework (e.g. `spf13/cobra`) — optional; `flag`/stdlib may
    suffice and is preferred if sufficient.
  - An OCI types library (e.g. `opencontainers/image-spec` for struct
    definitions) — acceptable since re-implementing the spec types verbatim
    adds no educational value.
  - A cgroups helper library is explicitly **discouraged** — cgroups v2 is a
    filesystem interface; Forge should read/write it directly to preserve
    the educational value, wrapped in `internal/cgroup`.
- **No dependency on Docker, containerd, runc, or their client libraries.**
  Forge must not shell out to or link against existing container runtimes.
  This is a hard invariant (§13), not a preference.
- Dependencies are vendored or pinned via `go.sum`; no floating versions.

---

## 11. Data Flow

### 11.1 `forge run` (Stage 6, full pipeline)

```
CLI parses args
   → runtime.CreateContainer(ctx, Spec)
       → image.ParseReference(ref)     [if image ref, not local path]
       → image.Client.Resolve(ctx, ref, platform)   → Manifest (digests verified)
       → image.Pull(ctx, client, cache, repo, manifest)  → blobs cached by digest
       → rootfs.Store.Prepare(containerID)          → <root>/<id>/rootfs
       → image.BuildRootfs(ctx, cache, layers, dir) → layers applied in order
       → state.Create(containerID, metadata)   [status: created]
       → namespace.Prepare(Config{PID,UTS,Mount,Net})
       → cgroup.Create(containerID, ResourceLimits)
       → network.Setup(containerID)     [veth, bridge attach, IP assign]
       → process.Start(namespace handle, rootfs, cmd)
           → child: mount.PivotRoot(rootfs)
           → child: exec(cmd)
       → state.Update(containerID, status=running, pid=...)
   → CLI prints containerID
```

### 11.2 `forge stop`

```
CLI parses args
   → runtime.StopContainer(ctx, id, timeout)
       → state.Get(id) → pid
       → process.Signal(pid, SIGTERM)
       → wait up to timeout
       → if still running: process.Signal(pid, SIGKILL)
       → network.Teardown(id)
       → cgroup.Destroy(id)
       → mount.Cleanup(id)
       → state.Update(id, status=stopped, exitCode=...)
```

### 11.3 Failure/rollback path

Any failure during `CreateContainer` triggers reverse-order cleanup of every
resource successfully created up to that point, then returns the original
error. This is implemented via a `cleanupStack` of `func() error` closures
accumulated as each step succeeds, invoked in `defer` on early return.

---

## 12. Runtime Lifecycle

Container states (persisted in `internal/state`):

```
created → running → stopped → removed
              │
              └──▶ exited (process exited on its own; distinct from `stopped`
                            which implies an explicit forge stop)
```

- `created`: namespaces/cgroup/network/rootfs prepared, process not yet
  exec'd.
- `running`: init process has been exec'd and is alive.
- `exited`: init process terminated on its own (any exit code).
- `stopped`: init process was terminated via `forge stop`.
- `removed`: all resources released, state entry marked for GC (or deleted,
  per `forge rm`).

State transitions are the only place `internal/state` is written by
`internal/runtime`; no other package touches container state directly.

---

## 13. Engineering Invariants

These must never be violated by any future change without an explicit ADR
overriding them:

1. **No shelling out to `docker`, `runc`, `containerd`, or `crun`.** Forge
   implements primitives itself via direct syscalls (`golang.org/x/sys/unix`
   or raw syscalls) — this is the entire point of the project.
2. **`internal/runtime` is the only orchestrator.** Primitive packages never
   call each other. No exceptions (ADR-0020).
3. **Every kernel resource Forge creates has a corresponding, idempotent
   teardown path**, exercised in tests.
4. **No global mutable state, no package-level singletons holding runtime
   data.**
5. **A stage's code must not require a later stage's code to function.**
   Stage 1 must run without cgroups, networking, or image support existing.
6. **CLI packages contain no business logic.** All logic lives below the CLI
   layer and is independently testable without invoking the CLI.
7. **No silent error suppression.** Every error is either returned, wrapped,
   or logged — never discarded without at least a `WARN` log entry.
8. **Root-requiring code paths are isolated and clearly marked** (build tags
   or explicit capability checks) so unit tests never accidentally require
   root.

---

## 14. Intentionally Out of Scope

(Mirrors PRD §5, restated here for engineering context)

- Rootless containers, user namespace UID/GID remapping.
- seccomp/AppArmor/SELinux integration.
- Multi-host/overlay networking.
- Image building (Dockerfile-equivalent).
- Plugin architecture.
- Windows/macOS native support.
- OCI Runtime Spec compliance as a runnable `runc`-compatible binary (may be
  revisited, see PRD §14, but is not assumed by current architecture).

Do not add scaffolding, config options, or abstraction layers in anticipation
of these — they are non-goals, not "phase 2." If they are pursued later, they
get their own ADR and PRD amendment first.

---

## 15. Architecture Decision Records (ADR)

ADRs live in `docs/adr/NNNN-title.md` and follow this template:

```
# NNNN. Title

Date: YYYY-MM-DD
Status: Proposed | Accepted | Superseded by NNNN

## Context
What problem are we solving? What constraints apply?

## Decision
What did we decide?

## Consequences
What becomes easier or harder as a result?
```

### Recorded ADRs (to be created as the project proceeds)

| ID | Title | Status |
|---|---|---|
| 0001 | Use `pivot_root` instead of `chroot` for rootfs isolation | Accepted |
| 0002 | Use direct cgroups v2 filesystem writes instead of a cgroups library | Proposed |
| 0003 | Layer assembly strategy: OverlayFS vs. explicit layer extraction | Accepted |
| 0004 | CLI flag library choice (stdlib `flag` vs. `cobra`) | Accepted |
| 0005 | Container ID generation scheme | Accepted |
| 0006 | State store format (JSON files vs. embedded DB) | Proposed |
| 0007 | Introduce `internal/runtime` in Stage 1 | Accepted |
| 0008 | Child-side namespace setup via re-executing `/proc/self/exe` | Accepted |
| 0009 | `forge run` output streams and exit status | Accepted |
| 0010 | Per-container rootfs layout, and how Stage 2 populates it | Accepted |
| 0011 | The mount plan is built by `runtime` and executed by `mount` | Accepted |
| 0012 | All container mounts are made child-side, before `pivot_root` | Accepted |
| 0013 | Depend on `golang.org/x/sys/unix` for syscalls | Accepted |
| 0020 | One `internal/image` package, and no primitive-to-primitive edges | Accepted |
| 0021 | The layer cache is a content-addressed blob store | Accepted |
| 0022 | `forge run` positional grammar and command resolution from an image | Proposed |

Each must be filled in and marked `Accepted` before the corresponding stage's
implementation is merged.

IDs 0014–0019 are unallocated. Stages 3 and 4 were merged without recording
ADRs, and their decisions are documented in `docs/stages/` only; the numbers are
left free so those records can be written retroactively without renumbering.
0022 stays `Proposed` until `forge run <image>` is wired through
`internal/runtime` and `internal/cli`, which Stage 5's package work deliberately
leaves for a following change.

---

## 16. Change Control

Any change to package boundaries (§2), architecture (§3), or invariants
(§13) requires:

1. A new or amended ADR.
2. An update to this document in the same PR.
3. Review from at least one other maintainer familiar with the affected
   stage.

This document is the tiebreaker in any design disagreement. If the code and
this document diverge, treat it as a bug and fix one or the other before
merging further work.
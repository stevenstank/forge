# Forge — Single Source of Truth (SSOT)

**Status:** Living document, current as of the end of Stage 6. Update in the
same PR as any architectural change.
**Purpose:** This is the authoritative engineering reference for Forge. If
code and this document disagree, that is a bug — in the code or in the
document — and must be resolved before merge.

> Stages 1–6 are complete. This document has been reconciled against the
> delivered code: §1, §9, §11 and §12 describe what exists rather than what was
> planned, and §15 records where the ADR register still owes the codebase a
> written decision.

---

## 1. Repository Structure

This is the structure as built, at the end of Stage 6.

```
forge/
├── cmd/
│   └── forge/                # CLI entrypoint (main package)
│       └── main.go
├── internal/
│   ├── process/              # Stage 1: process creation & lifecycle
│   ├── namespace/            # Stage 1: namespace creation (clone flags) and entry (setns)
│   ├── rootfs/               # Stage 2: per-container root filesystem directories
│   ├── mount/                # Stage 2: mount/unmount, pivot_root, mount cleanup
│   ├── cgroup/               # Stage 3: cgroups v2 management
│   ├── network/              # Stage 4: netns, veth, bridge, NAT, IP leases
│   ├── image/                # Stage 5: registry client, blob cache, layer unpack
│   ├── runtime/              # Stages 1-6: container orchestration/supervision
│   ├── state/                # Stage 6: on-disk container records
│   ├── logs/                 # Stage 6: captured container stdout/stderr
│   ├── cli/                  # CLI command implementations (thin layer over runtime)
│   └── logging/              # Structured logging helpers
├── test/
│   └── integration/          # Root/privileged integration tests (build tag `integration`)
│       └── testutil/         # Shared test helpers, fixtures
├── docs/
│   ├── adr/                  # Architecture Decision Records
│   ├── stages/               # Per-stage design notes
│   ├── PRD.md
│   └── SSOT.md
├── Makefile
├── go.mod
├── go.sum
└── README.md
```

**Rule:** `internal/` packages are never imported outside the module. There is
no `pkg/`: no type in Forge has an external consumer, and the rule was always
"prefer `internal/` until one exists". One never did.

Two directories this document previously anticipated do not exist and are not
missing: `pkg/oci/` (above) and `scripts/` — the Makefile carries everything a
helper script would have. `internal/registry` was folded into `internal/image`
by ADR-0020; `internal/logs` was not anticipated at all and arrived with
Stage 6's `forge logs`.

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
| `internal/state` | Persist and query container metadata (ID, PID, status, image, timestamps) on disk, crash-safely: atomic replace plus `flock` per record | Perform any kernel resource management |
| `internal/logs` | Append and read a container's captured stdout/stderr as a stream of timestamped, stream-tagged JSON entries | Decide what is worth logging, or know why a container produced output |
| `internal/cli` | Parse CLI args/flags, call into `internal/runtime`, format output | Contain business logic — CLI packages are intentionally "dumb" |
| `internal/logging` | Structured logger construction, log level configuration | N/A (leaf package, no internal dependencies) |

**Dependency rule (strict, see §9):** `internal/runtime` may depend on every
other `internal/*` package. No other package may depend on `internal/runtime`.
Leaf packages (`process`, `namespace`, `rootfs`, `mount`, `cgroup`, `network`,
`image`, `state`, `logs`, `logging`) must not depend on each other. There are
no exceptions: the two this document previously allowed, `rootfs` → `image` and
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
  `/var/lib/forge/images`). `--root` and `--image-root` are independent so they
  can sit on different filesystems.

### 9.1 On-disk layout

What each flag actually moves, and who owns each tree:

| Path | Owner | Contents |
|---|---|---|
| `<state-dir>/state/containers/<id>/metadata.json` | `internal/state` | the container record, with `.lock` beside it |
| `<state-dir>/logs/<id>` | `internal/logs` | captured output, one JSON entry per line |
| `<root>/<id>/rootfs` | `internal/rootfs` | the container's root filesystem |
| `<image-root>/blobs/…` | `internal/image` | the content-addressed layer cache |
| `/var/lib/forge/network/leases/<ip>` | `internal/network` | one file per claimed address, holding the owning container ID and PID |
| `/sys/fs/cgroup/forge/<id>/` | `internal/cgroup` | the container's cgroup v2 leaf |

Two of these are not repointed by any global flag, and both are deliberate
rather than overlooked: the cgroup path follows the host's unified hierarchy
(`cgroup.DefaultRoot` + `cgroup.ParentName`), and the network lease directory
takes `network.DefaultStateDir` because `internal/cli` does not populate
`runtime.Config.Network`. A test that needs its own pool sets that field
directly. If `--state-dir` is ever expected to move the leases too, that is a
change to `newRunner`, and it should be made deliberately: leases outliving a
redirected state directory is the failure it would fix.
- Subcommands (the final set, all delivered):
  - `forge run [flags] <image> [cmd] [args...]`
    - Three grammars, disambiguated by `splitImageAndCommand` without a
      lookahead flag: `-rootfs <dir>` given means the positionals are the
      command; a first positional beginning `/`, `./` or `../` is a command
      run against the host's filesystem (Stage 1, still valid); anything else
      in first position is an image reference and the rest is the command.
      The namespaces cannot overlap — a bare command must be an absolute path,
      and a registry reference can never begin with `/`.
    - Stage 2 flags: `-rootfs`, `-mount src:dst[:ro,nosuid,nodev,noexec]`
      (repeatable), `-read-only`, `-workdir`; `-hostname` from Stage 1.
    - Stage 3 flags: `-memory`, `-cpus`, `-cpu-weight`, `-pids`.
    - Stage 4 flags: `-network bridge|none|host`, `-mtu`.
    - Stage 6 flags: `-keep`.
  - `forge ps [-a] [-q]`
  - `forge exec [-workdir dir] [-env NAME=VALUE] <container-id> <cmd> [args...]`
  - `forge stop [-t seconds] [-rm] <container-id>`
  - `forge logs [-f] [-n count] [-t] <container-id>`
  - `forge rm [-f] <container-id>`

- **There is no `-d`.** `forge run` is attached and blocks until the container
  exits, propagating its status (ADR-0009); a container's lifetime is its
  `forge run`. `-keep` is the flag that retains the record and root filesystem
  afterwards, which is what gives `forge ps -a` and `forge rm` something to act
  on. Detaching is future work and would need its own ADR: it changes who
  supervises a container, which is the load-bearing assumption behind `stop`
  waiting on either the process or the record (§11.2).
- Internal commands, prefixed `__`, are hidden from help and are not part of
  the user-facing surface. The only one is `__init`, which Forge re-executes
  itself as to set up a container from inside its namespaces (ADR-0008).
- All commands that mutate state print the affected container ID to stdout
  on success (scriptable, Docker-like convention) and write human-readable
  detail to stderr.
  - **Exception — `forge run`** (ADR-0009): stdout belongs to the container, so
    Forge writes nothing to it. The container ID is reported on stderr via the
    `container_id` log field. Since there is no detached mode, this exception
    covers every `forge run`.
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

### 11.1 `forge run` (full pipeline)

The order is load-bearing and is explained where it is implemented
(`runtime.Runner.Run`). The short version: everything that can fail cheaply
happens before anything is created, and the record is written at the last
moment before the first container-specific resource exists, because the record
is how a `forge rm` running tomorrow finds the rest.

```
CLI parses args → runtime.Spec
   → runtime.Runner.Run(ctx, spec)
       → image.ParseReference(ref)                    [if the first positional is an image]
       → image.Client.Resolve(ctx, ref, HostPlatform) → Manifest (digests verified)
       → image.Pull / Cache                           → blobs cached and verified by digest
       ── nothing host-visible created yet; a mistyped reference costs nothing ──
       → state.Store.Save(metadata)                   [status: creating]   ← cleanup stack entry 1
       → logs.Store.Open(id)                          → captured stdout/stderr
       → rootfs.Store.Prepare(id)                     → <root>/<id>/rootfs
       → image.BuildRootfs(ctx, cache, layers, dir)   → layers applied in order
       → runtime builds a mount.Plan                  (ADR-0011: runtime decides, mount executes)
       → cgroup.Manager.Create(id, limits)            → leaf, limits written before the container exists
       → network.Manager.Allocate(id)                 → IP lease claimed before the container exists
       → process.New/Start with namespace.Config.CloneFlags()
           → the child is `forge __init` (ADR-0008), re-executed into the new namespaces,
             blocking on a read of the payload pipe before it does anything else
           ── the handshake window: the PID exists, the container has run no instruction ──
           → parent: record status=created and the PID
           → parent: cgroup.Manager.Add(id, pid)      every limit in force before the payload
           → parent: network.Manager.Attach(net, pid) a netns can only be named by a PID in it
           → parent: write the payload; the child unblocks
           → child: namespace.Apply, mount.Apply, mount.PivotRoot,
                    network.Configure, execve(cmd)
       → state.Store.Update(id, status=running)
       → wait; state.Store.Update(id, status=exited, exitCode=…)
   → cleanup stack unwinds in reverse (§11.3)
```

The handshake is the whole reason a container is never briefly unconstrained: a
cgroup can only be joined by writing a PID to `cgroup.procs`, and a netns can
only be named by the PID of a process already inside it, so neither can be done
before `clone(2)` returns. The child blocking on the pipe is what makes "after
clone" and "before the container runs" the same moment.

### 11.2 `forge stop`

Forge has no daemon, so the process running `stop` is almost never the
container's parent — and only a parent can reap a child or read its exit
status. `stop` therefore signals and then watches for *either* the process
disappearing or the record going terminal, and never invents an exit code it
could not have observed.

```
CLI parses args
   → runtime.Runner.Stop(ctx, id, StopOptions{Timeout, Remove})
       → state.Store.Load(id) → pid            [terminal already? release resources and return]
       → mark status=stopping                  so `forge ps` and the supervisor both see it
       → process.Signal(SIGTERM)
       → await: process gone OR record terminal, up to Timeout
       → if still there: process.Signal(SIGKILL), await again over KillGrace
       → network.Manager.Destroy(id)           idempotent; the supervisor is racing to do the same
       → cgroup.Manager.Destroy(id)            idempotent, same reason
       → finalise the record                   only if nobody else did; no exit code if nobody saw one
       → if Remove: Runner.Remove(id), tolerating an already-deleted record
```

The container's filesystem, its log and its record deliberately survive a
`stop`. That is what makes `forge ps -a` able to describe a container that has
finished, and it is `forge rm`'s job to release them.

### 11.2.1 `forge exec`

The one operation that acts on a container from outside it, and the one with a
failure mode that damages Forge rather than the container:

```
   → runtime.Runner.Exec(ctx, ExecSpec)
       → state.Store.Load(id), refuse anything not running
       → process.Open(pid) → pidfd, held for the whole setup
              so "the namespaces of PID n" cannot come to mean a recycled process
       → namespace.Open(pid, EntryOrder...)   all descriptors, before any is joined
       → open the container's cgroup directory (clone3 places the child at birth)
       → on a thread that is locked, never unlocked, and not the initial thread:
              namespace.Enter(handles)   unshare(CLONE_FS), then setns in EntryOrder
              resolve the command on the container's PATH
              fork
       → wait on an ordinary thread; the joined thread dies with its goroutine
```

`setns(2)` moves the calling *thread*. The initial thread is excluded
explicitly because the Go runtime parks it rather than terminating it, and
`/proc/self` reports its namespaces — joining there would move the whole Forge
process into the container, permanently and visibly.

### 11.3 Failure/rollback path

Any failure during `CreateContainer` triggers reverse-order cleanup of every
resource successfully created up to that point, then returns the original
error. This is implemented via a `cleanupStack` of `func() error` closures
accumulated as each step succeeds, invoked in `defer` on early return.

---

## 12. Runtime Lifecycle

Container states (persisted in `internal/state`, and enforced by
`Status.CanTransitionTo`):

```
creating ─▶ created ─▶ running ─▶ stopping ─┬─▶ stopped ─▶ removing ─▶ (record deleted)
    │           │          │                │
    │           └──────────┴────────────────┴─▶ exited ──▶ removing ─▶ (record deleted)
    └─▶ exited                                             (a container that never started)
```

- `creating`: the record exists and resources are being acquired. A crash here
  leaves a record that says so.
- `created`: every resource is prepared and the init process exists, but is
  still blocked on the payload — the handshake window of §11.1.
- `running`: the payload was written and the container's own binary is alive.
- `stopping`: a stop was requested and the grace period is running. Written
  *before* the signal, so a concurrent `forge ps` says "stopping" rather than
  "running", and so the supervisor records this as a stop rather than as an
  exit of the container's own accord.
- `exited`: the init process terminated on its own, at any exit code.
- `stopped`: the init process was terminated by `forge stop`.
- `removing`: the retained resources are being released. This is what makes an
  interrupted `forge rm` finishable rather than ambiguous: a later `rm` picks
  it up instead of finding a record that claims to be a healthy stopped
  container while half its resources are gone.

`exited` and `stopped` are the two terminal states; `removing` is not terminal
in that sense — the container is finished, but the record is not yet free to be
deleted. There is no persisted `removed` state, because a removed container has
no record to hold one.

State transitions are the only place `internal/state` is written by
`internal/runtime`; no other package touches container state directly.

### 12.1 Who writes a record, and when two processes race

Forge has no daemon, so more than one process routinely holds an opinion about
one container at the same moment, and every write has to be correct under that:

- The supervising `forge run` owns the container and is the only process that
  can reap it and read its exit status.
- A container started **without** `-keep` has its record deleted by that same
  supervisor as it unwinds — and the unwind is triggered by the very exit a
  concurrent `forge stop --rm` is waiting for. Both are therefore removing the
  same record at the same moment. `stop --rm` treats an already-deleted record
  as success; `forge rm` of a container that never existed remains an error,
  because there the missing record is the answer to the user's question rather
  than the outcome they asked for.
- Every read-modify-write of a record is serialised by an `flock` on the
  container's directory, released by the kernel when the holder's descriptor
  closes — including when the holder was killed or the host lost power.
- Resource teardown is idempotent everywhere (§13.3), because two processes
  releasing the same veth or cgroup at the same time is the ordinary case here,
  not a race to be prevented.

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
9. **The Forge process never permanently leaves the host's namespaces.**
   `setns(2)` moves the calling thread, so every join happens on a thread that
   is locked to one goroutine, never unlocked, and destroyed when that
   goroutine returns — and never on the process's initial thread, which the Go
   runtime parks instead of terminating and whose namespaces `/proc/self`
   reports. Only the `exec`'d child joins a container; the long-lived runtime
   does not. A regression here is silent, intermittent, and corrupts every
   later operation in the same process, which is why it is an invariant rather
   than a code comment (see `internal/runtime/exec.go`).

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

### Recorded ADRs

`File` says whether `docs/adr/NNNN-*.md` exists. A decision that is cited by the
code but has no file is a documentation debt, not an undecided question — the
decision was made and implemented; only the record is missing.

| ID | Title | Status | File |
|---|---|---|---|
| 0001 | Use `pivot_root` instead of `chroot` for rootfs isolation | Accepted | ✅ |
| 0002 | Use direct cgroups v2 filesystem writes instead of a cgroups library | Accepted, implemented | ❌ |
| 0003 | Layer assembly strategy: OverlayFS vs. explicit layer extraction | Accepted | ✅ |
| 0004 | CLI flag library choice (stdlib `flag` vs. `cobra`) | Accepted | ✅ |
| 0005 | Container ID generation scheme | Accepted | ✅ |
| 0006 | State store format (JSON files vs. embedded DB) | Accepted, implemented | ❌ |
| 0007 | Introduce `internal/runtime` in Stage 1 | Accepted | ✅ |
| 0008 | Child-side namespace setup via re-executing `/proc/self/exe` | Accepted | ✅ |
| 0009 | `forge run` output streams and exit status | Accepted | ✅ |
| 0010 | Per-container rootfs layout, and how Stage 2 populates it | Accepted | ✅ |
| 0011 | The mount plan is built by `runtime` and executed by `mount` | Accepted | ✅ |
| 0012 | All container mounts are made child-side, before `pivot_root` | Accepted | ✅ |
| 0013 | Depend on `golang.org/x/sys/unix` for syscalls | Accepted | ✅ |
| 0014 | The cgroup attach happens in the handshake window, between `Start` and `writePayload` | Accepted, implemented | ❌ |
| 0015 | Degradation policy when the cgroup v2 unified hierarchy is unavailable | Accepted, implemented | ❌ |
| 0016 | Netlink is spoken directly, with no netlink library | Accepted, implemented | ❌ |
| 0017 | NAT via nf_tables over `NETLINK_NETFILTER`, not `iptables` | Accepted, implemented | ❌ |
| 0018 | The container configures its own interface from plain data across the re-exec boundary | Accepted, implemented | ❌ |
| 0019 | Interface names and IP leases are derived from the container ID | Accepted, implemented | ❌ |
| 0020 | One `internal/image` package, and no primitive-to-primitive edges | Accepted | ✅ |
| 0021 | The layer cache is a content-addressed blob store | Accepted | ✅ |
| 0022 | `forge run` positional grammar and command resolution from an image | Accepted, implemented | ❌ |

**Outstanding documentation debt.** Nine decisions above have no ADR file.
0002 and 0006 were marked `Proposed` and then simply implemented. 0014–0019
were described here as "unallocated" while Stage 4 went ahead and cited
0016–0019 from `internal/network` and `internal/runtime`, so the numbers are
now spoken for by the code whether or not the records exist. 0022 was to stay
`Proposed` until `forge run <image>` was wired through `internal/runtime` and
`internal/cli`; that shipped in Stage 5, and the status was never updated.

**One number is claimed twice.** `docs/stages/stage-3.md` uses 0015 for the
cgroup-v2 degradation policy, and `docs/stages/stage-6.md` says `forge exec`'s
thread discipline "needs ADR-0015". Both cannot have it. Whoever writes these
records should give the exec decision the next free number — it is the more
consequential of the two, and it is the one with rejected alternatives worth
recording: a cgo prelude, and a persistent in-container agent.

Writing those nine is the largest known gap between this document and the
codebase. It changes no behaviour, and `docs/stages/` covers most of the
reasoning in narrative form in the meantime. `docs/stages/stage-4.md` is also
absent, which is why Stage 4's decisions are the least well recorded of the
six.

For any *future* change, the original rule stands: an ADR must be filled in and
marked `Accepted` before the corresponding implementation is merged.

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
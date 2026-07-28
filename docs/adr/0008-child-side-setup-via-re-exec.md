# 0008. Child-side namespace setup via re-executing `/proc/self/exe`

Date: 2026-07-27
Status: Accepted

## Context

`clone(2)` places a child in fresh namespaces, but two Stage 1 requirements
cannot be met by clone flags alone:

- **FR-1.2** wants the container to have its own hostname. `CLONE_NEWUTS` gives
  it a private copy of the host's, so a distinct name requires calling
  `sethostname(2)` *inside* the new UTS namespace.
- **FR-1.3** wants mounts made inside the container not to propagate to the
  host. `CLONE_NEWNS` copies the parent's mount table *including each mount's
  propagation type*. On any systemd host `/` is `MS_SHARED`, so without further
  action a mount made in the container still propagates out. Preventing that
  requires `mount(none, /, MS_REC|MS_PRIVATE)` inside the new mount namespace.

Both are operations that must run after `clone(2)` and before the container's
binary is `execve`'d. Go cannot run arbitrary code in that window: between fork
and exec a Go child may only call async-signal-safe functions, and the runtime's
threads do not exist in the child. `os/exec` exposes only a fixed set of
`SysProcAttr` knobs, and no hostname knob among them.

## Decision

Forge re-executes *itself* as the container's init process:

```
forge run
  └─ clone(CLONE_NEWPID|CLONE_NEWUTS|CLONE_NEWNS)
       └─ /proc/self/exe __init          ← a fresh Go program, full runtime
            ├─ namespace.Apply(config)   ← sethostname, make mount tree private
            └─ execve(user binary)       ← becomes PID 1 of the namespace
```

The child is an ordinary Go program started by `execve`, so it may do anything;
the fork/exec restriction does not apply to it.

Configuration crosses the boundary as JSON on an `os.Pipe` inherited as file
descriptor 3, written by the parent *after* the child is started so a payload
larger than the pipe buffer cannot deadlock. A pipe is used rather than an
environment variable because the environment is size-limited and visible in
`/proc/<pid>/environ`, and because the container's own environment must be
exactly what the caller specified. The descriptor is closed before `execve`, so
Forge's plumbing is not inherited by the container.

`__init` is registered as a hidden CLI command. The double underscore marks it
internal and keeps it clear of the user-facing verbs in SSOT §9, which is
amended to record its existence.

A container that fails to start exits **127**, the shell's "command not found"
convention, which Docker also uses. This distinguishes "Forge could not start
the container" from "the container ran and exited", which would otherwise be
indistinguishable from an ordinary non-zero status.

### The re-exec contract, and its guard

`Run` re-executes *the currently running binary*. Any program that calls it must
therefore route `__init` to `runtime.Init`, which `runtime.IsInitCommand`
reports. A program that does not would re-enter its own `main`, start another
container, and repeat — a fork bomb. This is not hypothetical: it happened while
developing the Stage 1 integration tests, whose binary is itself re-executed.

`Run` therefore sets a marker variable in the init process's environment and
refuses to start a container when it finds that marker already set, turning an
unbounded fork bomb into an immediate, named error (`ErrNestedInit`). The marker
is never passed to the container, because `execve` replaces the environment with
the Spec's.

### Where the propagation change lives

`makeMountTreePrivate` lives in `internal/namespace`, not `internal/mount`.
`internal/mount` belongs to Stage 2, and Stage 1 must not depend on a later
stage's code (SSOT §13.5). The placement is also defensible on its own terms:
SSOT §2 says `namespace` must not perform filesystem *configuration*, and this
mounts nothing and changes no file — it sets a propagation property, without
which `CLONE_NEWNS` does not actually isolate anything. It is part of creating
the namespace correctly. SSOT §2 is amended to say so.

## Consequences

Easier:

- FR-1.2 and FR-1.3 are genuinely satisfied rather than nominally. In
  particular, the mount test fails if the `MS_PRIVATE` remount is removed on a
  host where `/` is shared.
- The architecture matches the pipeline SSOT §11.1 already sketches, which shows
  `child: mount.PivotRoot(rootfs)` and `child: exec(cmd)` as distinct child-side
  steps. Stage 2 adds `pivot_root` to `Init` with no structural change.
- The mechanism is explicit in Forge's own source, which is the educational
  point: a reader sees `sethostname` and the `MS_PRIVATE` remount rather than
  a library call.

Harder:

- Forge must be able to find and re-execute itself. `os.Executable` resolves
  `/proc/self/exe`, which continues to work if the binary is moved or deleted
  while running, but a container cannot be started by a program that cannot
  execute itself.
- Every binary embedding `internal/runtime` carries the dispatch obligation
  described above. It is documented on `IsInitCommand` and enforced by the
  guard, but it is real coupling and it will surprise someone.
- Startup costs one extra `execve` and a small JSON round trip per container.
  Irrelevant at Forge's scale, and it buys the ability to run arbitrary Go in
  the container's namespaces, which every later stage needs.

## Alternatives considered

**`SysProcAttr.Unshareflags: CLONE_NEWNS`.** Go's `syscall` package performs a
`MS_REC|MS_PRIVATE` remount of `/` itself when this is set, which would satisfy
FR-1.3 with no re-exec. Rejected on two grounds: it does nothing for FR-1.2, so
a re-exec would still be needed for the hostname; and hiding the mechanism
inside a stdlib flag defeats the project's purpose, which is to make the
mechanism visible (PRD §2).

**A cgo constructor running before the Go runtime starts.** This is what runc
does, for reasons Forge does not share — runc needs `CLONE_NEWUSER` and `setns`
handling that must precede Go's thread creation. Forge's child-side work is
ordinary post-`execve` code. Rejected: it would introduce cgo and a large amount
of subtlety for no benefit at this scope.

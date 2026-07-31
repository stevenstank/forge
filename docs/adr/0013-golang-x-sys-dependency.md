# 0013. Depend on `golang.org/x/sys/unix` for syscalls

Date: 2026-07-31
Status: Accepted

## Context

Stage 1 called the kernel through the standard library's `syscall` package.
Stage 2 needs `pivot_root(2)`, `mount(2)`, `umount2(2)`, `chdir(2)` and
`rmdir(2)`, all of which `syscall` also provides on Linux, so the dependency is
not forced by a missing call.

Two things push the other way:

- **`syscall` is frozen.** Its own documentation directs new code to
  `golang.org/x/sys`. It receives no new constants and no new wrappers, so the
  first Stage 3–5 call it lacks would force the move anyway, mid-stage, with
  Stage 2 already written against the other package.
- **SSOT §13.1 already names it.** The invariant reads "Forge implements
  primitives itself via direct syscalls (`golang.org/x/sys/unix` or raw
  syscalls)". The dependency is anticipated by the architecture; this ADR is
  the §10 record it still requires.

SSOT §10 requires a new third-party dependency to answer three questions:

*What does the stdlib not provide?* Today, nothing Stage 2 needs. What it does
not provide is a maintained surface: `syscall` will not grow the constants and
wrappers later stages want, and mixing the two packages across stages would be
worse than choosing one now.

*Why is this library the right choice?* It is the Go project's own package,
maintained by the same team, with no transitive dependencies. There is no
alternative that is not either frozen (`syscall`) or a third-party wrapper of
the kind SSOT §10 discourages for cgroups.

*What is its maintenance and security posture?* Maintained in the Go project's
repository, released regularly, covered by the Go security process, and used by
essentially every Go program that touches Linux internals — including the
runtimes Forge is modelled on.

## Decision

Add `golang.org/x/sys v0.38.0` and use `unix` for syscalls in new code.

Version 0.38.0 rather than the current 0.43.0: 0.43.0 declares `go 1.25.0`,
which would raise Forge's own Go directive above the 1.24 that SSOT §4 states.
0.38.0 declares `go 1.24.0` and carries every call Stage 2 uses. The pin is
recorded in `go.sum`, per SSOT §10's prohibition on floating versions.

This is Forge's **only** third-party dependency, and the bar for a second is
unchanged.

Stage 1's `internal/namespace` and `internal/process` keep using `syscall`. They
work, they are not the subject of this stage, and rewriting working code to
change which package a constant comes from is churn. New code uses `unix`;
Stage 1's code migrates if and when it is touched for another reason.

## Consequences

Easier:

- Later stages have the constants they need — cgroup and network work reaches
  well past what `syscall` exposes.
- `unix.PivotRoot` exists as a named wrapper, so `internal/mount` reads as the
  syscall sequence it is rather than as `Syscall(SYS_PIVOT_ROOT, ...)` with
  hand-rolled string marshalling. That legibility is the project's point
  (PRD §2).

Harder:

- Forge is no longer dependency-free, which was a nice property to be able to
  state plainly. `go.mod` now has one entry, and the discipline that keeps it
  at one is this ADR process.
- Two packages now supply syscall constants, and Stage 1's code uses the other
  one. A reader meeting `syscall.MS_PRIVATE` in `internal/namespace` and
  `unix.MS_BIND` in `internal/mount` may reasonably wonder why. They are the
  same numbers; the split is historical and is recorded here so it is not
  mistaken for a distinction.

## Alternatives considered

**Stay on `syscall`.** It would have worked for all of Stage 2 and kept the
dependency count at zero. Rejected because the migration is inevitable, and
doing it at the start of the first stage that needs real syscall breadth is
cheaper than doing it midway through a later one.

**Raw `syscall.Syscall` with `BytePtrFromString`.** SSOT §13.1 permits it, and
it needs no dependency at all. Rejected: it puts pointer arithmetic and manual
NUL-termination in front of the reader for every call, which obscures exactly
the mechanism Forge exists to show.

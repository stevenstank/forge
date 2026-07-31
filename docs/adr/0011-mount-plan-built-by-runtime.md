# 0011. The mount plan is built by `runtime` and executed by `mount`

Date: 2026-07-31
Status: Accepted

## Context

SSOT §2 gives `internal/mount` a responsibility and a prohibition in the same
row: it performs `pivot_root`, bind mounts and mount cleanup, and it must not
"decide *what* to mount — receives a mount plan from the caller".

Stage 2 makes that line consequential, because a container does not mount only
what the user asked for. It also needs `/proc` — without which Stage 1's PID
namespace remains invisible from inside — plus a `/dev` that contains
`/dev/null`, a `/dev/pts`, a `/dev/shm`, and a `/sys`. That set is a series of
judgements: which filesystems, with which options, and how much of the host to
expose.

Somebody has to make those judgements, and the obvious place to put them —
next to the code that performs the mounts — is the place SSOT §2 forbids.

## Decision

The default mount set lives in `internal/runtime`, in `defaultMounts`.
`internal/mount` never constructs a `Mount` of its own.

The split is drawn at policy versus mechanism:

| Question | Answer lives in |
|---|---|
| Should every container get a `/proc`? | `runtime` |
| Should `/sys` be read-only? | `runtime` |
| Are the caller's mounts applied before or after the defaults? | `runtime` |
| What flags does `ro` mean? | `mount` |
| Does a bind mount need a second `MS_REMOUNT` call? | `mount` |
| In what order must nested mounts be made? | `mount` |
| Where does `/data` resolve to, given a rootfs with symlinks? | `mount` |

`runtime.mountPlan` composes a `mount.Plan` from the defaults and the caller's
`--mount` flags and validates it. `mount.Apply` performs it without judgement.

A caller mount whose destination collides with a default is **refused**, not
resolved by ordering. A user who bind-mounts over `/proc` has more likely made
a mistake than a decision, and Stage 2 is not the place to guess which.

## Consequences

Easier:

- `defaultMounts` is a single function a reader can point at to answer "what
  does a Forge container actually get?", with a comment on each entry saying
  why it is there. `TestDefaultMountSet` pins it, so changing what every
  container gets is a deliberate, reviewable diff.
- `internal/mount` stays a description of three syscalls, testable almost
  entirely without root, with no opinions to disagree with.
- Stage 4 changes `/sys` from read-only to writable by editing one line of
  policy, in the package whose job policy is, without touching mount code.

Harder:

- The two halves of "what a container mounts" are in different packages, so
  following a mount from flag to syscall means reading both. The plan is a
  plain struct that appears verbatim in the debug log, which is what makes that
  traversal tractable.
- `runtime` grows a table of filesystem-specific data strings (`mode=755`,
  `newinstance,ptmxmode=0666`) that look like they belong next to the mount
  code. They are policy — they are what Forge chooses to give a container — and
  the discomfort is the boundary doing its job.

## Alternatives considered

**Put `defaultMounts` in `internal/mount`.** Reads better locally: everything
about mounting in one package. Rejected: it violates SSOT §2 as written, and
the invariant is load-bearing rather than decorative — it is what keeps the
primitive packages free of container policy so a reader can learn `mount(2)`
from `internal/mount` without also learning Forge's opinions.

**A third package, `internal/mountpolicy`.** Honours the boundary without
growing `runtime`. Rejected: a package for one function, and it would need its
own row in SSOT §2 for no gain. Composing primitives *is* `runtime`'s job
(ADR-0007).

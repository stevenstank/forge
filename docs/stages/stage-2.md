# Stage 2 — Filesystem Isolation (design)

**Status:** Implemented
**Requirements:** PRD §8.2 (FR-2.1 … FR-2.4)
**New packages:** `internal/mount`, `internal/rootfs`
**ADRs:** 0001, 0010, 0011, 0012, 0013

> This document was written as the stage's design and kept as its record. §7a
> lists what changed between the design and the implementation. The one
> ordering question it did not anticipate — whether bind mounts are made before
> or after `pivot_root` — is settled in ADR-0012: **before**, because a bind
> mount's source is a host path and the pivot detaches the host.

---

## 0. What Stage 2 adds

`forge run --rootfs <dir> /bin/sh` runs a command whose `/` is a root filesystem
Forge prepared for that container, not the host's. Host directories can be
bind-mounted in. When the container exits, no mount and no directory it created
survives on the host.

Stage 1's container shares the host filesystem; only its process tree, hostname
and mount table are its own. Stage 2 keeps all of that and replaces the shared
filesystem view.

Explicitly still out of scope: images (Stage 5 supplies the rootfs content),
layering/copy-on-write (ADR-0003, Stage 5), cgroups (Stage 3), network (Stage 4),
and any persistence of the mount set across a Forge restart (Stage 6).

Because there is no image yet, the rootfs *source* is a host directory the user
supplies — e.g. an unpacked `alpine-minirootfs` tarball. Stage 5 replaces the
source with unpacked layers and drops the `--rootfs` flag in favour of an image
reference; nothing else in this design changes.

---

## 1. Runtime flow changes

### 1.1 Where the work happens

Every mount Stage 2 makes happens **inside the container's mount namespace**, in
the re-exec'd init process, after `namespace.Apply` has marked the tree
`MS_REC|MS_PRIVATE` and before `execve`. The parent's only filesystem work is
creating and later removing directories (ADR-0012).

This is the decision the rest of the stage hangs off:

- FR-2.3 ("no orphaned mounts on the host") becomes structurally true rather
  than a cleanup routine that must be correct. The kernel destroys a mount
  namespace when its last member exits, unmounting everything in it.
- A partially-built mount stack needs no unwinding: if any child-side step
  fails, the init process exits 127 and the namespace — with whatever mounts it
  had — disappears (PRD NFR-8).
- The host mount table's entry count is invariant across a run, which is exactly
  what Stage 1's `TestNoHostResidueAfterRun` already asserts and what Stage 2
  extends.

`mount.Cleanup` still exists, because SSOT §11.2 names it and SSOT §13.3 demands
an idempotent teardown path per resource — but it is a *reconciliation* path for
crash residue, not the normal one. See §1.4.

### 1.2 Parent side — `runtime.Run`

Additions in **bold**:

```
runtime.Run
  ├── spec.Validate()                      + rootfs / mount validation
  ├── NewID()
  ├── **rootfs.Store.Prepare(id)**         → /var/lib/forge/containers/<id>/rootfs
  │     └── push cleanup: Store.Remove(id)
  ├── **plan := mountPlan(spec, dir)**     what to mount (runtime decides, SSOT §2)
  ├── namespace.Config{PID,UTS,Mount,Hostname}
  ├── os.Pipe()
  ├── process.New/Start                    clone(2)
  │     └── child: __init (see §1.3)
  ├── write payload, close pipe
  ├── process.Wait
  └── **deferred cleanup stack unwinds**   → Store.Remove(id)
```

Two structural changes to `runtime`:

1. **A cleanup stack.** Stage 1 had one resource (the process) and unwound it
   inline. Stage 2 has two (the per-container directory, the process), so
   SSOT §11.3's `cleanupStack` of `func() error` closures, unwound in reverse
   order on any early return, is introduced now rather than improvised again in
   Stage 3. Cleanup failures are logged at `WARN` and never mask the original
   error (SSOT §5).
2. **`Runner` gains configuration.** `NewRunner(logger)` becomes
   `NewRunner(logger, Config{Root: ...})` so the rootfs storage root
   (`--root`, default `/var/lib/forge/containers`, already parsed by
   `internal/cli` and currently unused) reaches the runtime. This is an internal
   API break; the only callers are `internal/cli` and the integration tests.

### 1.3 Child side — `runtime.Init`

```
Init
  ├── readInitPayload()                    payload now carries Rootfs + Mounts
  ├── namespace.Apply(cfg)                 MS_REC|MS_PRIVATE, sethostname   [Stage 1]
  ├── **mount.Apply(plan)**
  │     ├── bind rootfs → rootfs (MS_BIND|MS_REC)   make it a mount point
  │     ├── mkdir <rootfs>/.forge-oldroot            put_old, before any ro remount
  │     ├── each planned mount, shallowest first:
  │     │     ├── resolve + containment-check destination
  │     │     ├── create destination (dir, or file for a file bind)
  │     │     ├── mount(source, dest, type, flags, data)
  │     │     └── if ro/nosuid/…: remount MS_REMOUNT|MS_BIND to apply them
  │     └── if spec.ReadonlyRootfs: remount rootfs MS_REMOUNT|MS_BIND|MS_RDONLY
  ├── **mount.PivotRoot(rootfs, ".forge-oldroot")**
  │     ├── chdir(rootfs)
  │     ├── pivot_root(rootfs, rootfs/.forge-oldroot)
  │     ├── chdir("/")
  │     ├── umount2("/.forge-oldroot", MNT_DETACH)
  │     └── rmdir("/.forge-oldroot")        best-effort; WARN on a ro rootfs
  ├── **chdir(payload.WorkingDir)**         default "/"
  └── execve(command)                                                     [Stage 1]
```

Ordering constraints, all load-bearing:

- `MS_PRIVATE` first. Without it every mount below propagates to the host and
  FR-2.3 fails on any systemd host.
- The self-bind of the rootfs comes before everything else: `pivot_root(2)`
  requires `new_root` to be a mount point and to differ from the current root's
  mount. A plain directory is neither.
- `put_old` is created *before* the read-only remount, because you cannot mkdir
  on a read-only filesystem.
- All mounts happen before `pivot_root`, addressed via the host-visible
  `<rootfs>/...` prefix. Mounting after the pivot would work too but splits the
  path logic in two.
- Nested destinations are mounted shallowest-first (`/var` before `/var/log`),
  otherwise the outer mount hides the inner one.

### 1.4 Cleanup

| Resource | Normal teardown | Crash teardown |
|---|---|---|
| Mounts inside the container | Kernel, when the mount namespace dies | Kernel, same — the namespace cannot outlive its processes |
| `<root>/<id>/` directory tree | `rootfs.Store.Remove(id)` in the parent's deferred cleanup | Left behind if `forge` is `SIGKILL`ed. Reconciled by `mount.Cleanup` + `Store.Remove` on the next run touching that ID, and fully by Stage 6's startup reconciliation, which needs the state store |
| Host-side mounts under `<root>/<id>` | None are created | `mount.Cleanup(dir)` unmounts everything under `dir`, deepest-first, `MNT_DETACH`, idempotent. Present so no future stage that *does* mount host-side has to invent it, and so §13.3 is satisfiable by test |

`Remove` refuses to delete anything that still has a mount under it, so a bug
elsewhere can never turn `os.RemoveAll` into a recursive delete through a bind
mount into the host. That check runs first, unconditionally.

### 1.5 CLI

```
forge run [flags] <path> [args...]
  --rootfs <dir>          host directory to use as the container's root filesystem
  --mount <src>:<dst>[:ro,nosuid,nodev,noexec]   bind mount, repeatable
  --read-only             mount the container's root filesystem read-only
  --workdir <path>        working directory inside the container (default "/")
  --hostname <name>       [Stage 1]
```

`--rootfs` is optional. Omitted, `forge run` behaves exactly as it does in
Stage 1 — host filesystem, no pivot — so Stage 1's documented behaviour and its
integration tests survive unchanged, and the difference between the two modes is
one flag a reader can toggle. `--mount`, `--read-only` and `--workdir` without
`--rootfs` are a usage error: there is no container filesystem to mount into.

`--mount` parsing (splitting on `:`, validating options) lives in
`internal/mount` as a pure function, not in `internal/cli`, per SSOT §13.6; the
CLI only collects the strings.

`Spec.Validate`'s "not a path" message changes: the path is now resolved inside
the container's rootfs, so the hint becomes "give a path inside the container,
such as /bin/sh".

---

## 2. New packages and responsibilities

### `internal/mount` — the kernel mechanism

Owns `mount(2)`, `umount2(2)` and `pivot_root(2)`, and nothing else. It is told
*what* to mount and never decides (SSOT §2). It has no dependency on any other
`internal` package.

Split, mirroring `internal/namespace`'s parent/child split:

- **Pure, unit-testable without root:** `Options` → kernel flag computation,
  mount-spec parsing, plan validation, destination containment checking,
  depth-ordering of a plan.
- **Privileged, child-side:** `Apply`, `PivotRoot`.
- **Privileged, parent-side, reconciliation only:** `Cleanup`, `IsMountPoint`.

### `internal/rootfs` — the on-disk layout

Owns the per-container directory tree (FR-2.4): where it lives, creating it,
validating a source tree, refusing to remove one that is still mounted. It is
the package that will grow the `→ internal/image` edge in Stage 5 (the only
inter-primitive dependency SSOT §2 permits). In Stage 2 it has no dependencies.

It does **not** mount anything — it produces paths; `runtime` turns those into a
`mount.Plan`.

### `internal/runtime` — the conductor

Gains: the mount plan (the "what"), including Forge's default mount set; the
cleanup stack; the two new payload fields. Still no syscalls of its own beyond
`execve` (ADR-0007).

The default mount set lives here, not in `internal/mount`, precisely because it
is a policy decision about what a container should have:

| Destination | Type | Options | Why |
|---|---|---|---|
| `/proc` | `proc` | `nosuid,nodev,noexec` | Resolves Stage 1's headline limitation: `ps` inside the container currently lists host processes. A fresh procfs instance is bound to the PID namespace, so this is what makes `CLONE_NEWPID` observable |
| `/dev` | `tmpfs` | `nosuid,mode=755,size=65536k` | Practically every binary opens `/dev/null`; the source rootfs's `/dev` is usually empty |
| `/dev/null`, `zero`, `full`, `random`, `urandom`, `tty` | bind | `nosuid,noexec` | Bind-mounted from the host's `/dev`. Bind rather than `mknod(2)` because it needs no `CAP_MKNOD` reasoning and behaves identically |
| `/dev/pts` | `devpts` | `nosuid,noexec,newinstance,ptmxmode=0666,mode=0620` | A private pty instance; required for anything interactive |
| `/dev/shm` | `tmpfs` | `nosuid,nodev,mode=1777` | POSIX shared memory; glibc expects it |
| `/sys` | `sysfs` | `ro,nosuid,nodev,noexec` | Read-only until Stage 4 gives the container a network namespace of its own — a writable `/sys` in the host's netns is a host-configuration hole |

User `--mount`s are applied after the defaults, so an explicit mount can shadow
one. Duplicate destinations are rejected rather than silently ordered.

### Dependency graph after Stage 2

```
runtime ──┬──▶ namespace
          ├──▶ process
          ├──▶ rootfs
          └──▶ mount
```

Unchanged from SSOT §3, no new edges between leaves. Invariant §13.2 holds.

---

## 3. Public APIs

Signatures only — no bodies, no implementation.

### `internal/mount`

```go
// Type is the filesystem type passed to mount(2). Empty for a bind mount.
type Type string

const (
    TypeBind   Type = ""        // MS_BIND; source is a path
    TypeProc   Type = "proc"
    TypeSysfs  Type = "sysfs"
    TypeTmpfs  Type = "tmpfs"
    TypeDevpts Type = "devpts"
)

// Option is a per-mount modifier that maps to one MS_* flag.
type Option string

const (
    OptionReadOnly  Option = "ro"
    OptionNoSuid    Option = "nosuid"
    OptionNoDev     Option = "nodev"
    OptionNoExec    Option = "noexec"
    OptionRecursive Option = "rec"    // MS_REC, bind mounts only
)

// Mount is one entry in a Plan.
type Mount struct {
    Source      string   // host path for a bind; ignored for tmpfs/proc/sysfs
    Destination string   // absolute path *inside* the container
    Type        Type
    Options     []Option
    Data        string   // filesystem-specific data, e.g. "mode=755"
}

func (m Mount) Validate() error          // pure
func (m Mount) Flags() uintptr           // pure: Options → MS_* bitmask
func (m Mount) NeedsRemount() bool       // pure: bind + flags that a first mount(2) ignores

// Plan is the complete, ordered set of mounts for one container.
type Plan struct {
    Source       string    // host tree bind-mounted onto Root
    Root         string    // host path to the directory that becomes "/"
    ReadonlyRoot bool
    Mounts       []Mount
}

func (p Plan) Validate() error           // pure: containment, duplicates, absolute paths
func (p Plan) Ordered() []Mount          // pure: shallowest destination first

// ParseMountSpec parses a --mount value: "src:dst" or "src:dst:ro,nodev".
func ParseMountSpec(spec string) (Mount, error)   // pure

// Apply performs every mount in the plan, inside the caller's mount namespace.
// It must be called from the container's init process.
func Apply(plan Plan) error

// PivotRoot makes newRoot the caller's "/" and detaches the old root.
// It must be called from the container's init process, after Apply.
func PivotRoot(newRoot string) error

// Cleanup unmounts everything mounted at or under dir, deepest first.
// Idempotent: a dir with no mounts under it is not an error.
func Cleanup(ctx context.Context, dir string) error

// IsMountPoint reports whether path is a mount point in the caller's namespace.
func IsMountPoint(path string) (bool, error)
```

Sentinel errors (SSOT §5): `ErrNotADirectory`, `ErrEscapesRoot`,
`ErrDuplicateDestination`, `ErrInvalidOption`, `ErrInvalidMountSpec`,
`ErrRootNotMountPoint`, `ErrPermission`.

### `internal/rootfs`

```go
// Store owns the on-disk directory holding every container's root filesystem.
type Store struct{ /* root string; logger */ }

func NewStore(root string, logger *slog.Logger) (*Store, error)

// Dir is one container's prepared directory tree.
type Dir struct {
    ID     string
    Base   string  // <root>/<id>
    Rootfs string  // <root>/<id>/rootfs — becomes the container's "/"
}

// Prepare creates <root>/<id>/rootfs and returns its paths.
func (s *Store) Prepare(id string) (Dir, error)

// Remove deletes a container's directory tree. Idempotent. It refuses to
// proceed while any mount exists under the tree.
func (s *Store) Remove(ctx context.Context, id string) error

// Lookup returns the paths for an existing container without creating them.
func (s *Store) Lookup(id string) (Dir, error)

// ValidateSource reports whether path can serve as a container root filesystem:
// absolute, existing, a directory, not "/", symlinks resolved.
func ValidateSource(path string) (string, error)   // pure but stats the filesystem
```

Sentinel errors: `ErrSourceNotFound`, `ErrSourceNotADirectory`,
`ErrSourceIsHostRoot`, `ErrNotPrepared`, `ErrStillMounted`.

### `internal/runtime`

```go
type Config struct {
    Root string   // rootfs storage root; --root
}

func NewRunner(logger *slog.Logger, cfg Config) (*Runner, error)   // was NewRunner(logger)

type Spec struct {
    Command  []string      // [Stage 1]
    Env      []string      // [Stage 1]
    Hostname string        // [Stage 1]
    Stdin    io.Reader     // [Stage 1]
    Stdout   io.Writer     // [Stage 1]
    Stderr   io.Writer     // [Stage 1]

    Rootfs       string          // NEW: host dir to use as "/"; empty = Stage 1 behaviour
    Mounts       []mount.Mount   // NEW: user bind mounts
    ReadonlyRoot bool            // NEW
    WorkingDir   string          // NEW: default "/"
}
```

`Runner.Run`, `Init`, `IsInitCommand`, `InitCommandName`, `InitExitCode`,
`NewID` and every `process`/`namespace` API are unchanged.

---

## 4. Data structures

### 4.1 On-disk layout (FR-2.4)

```
/var/lib/forge/containers/          ← --root, 0700
└── <container-id>/                 ← 0700
    └── rootfs/                     ← 0755, becomes the container's "/"
        └── .forge-oldroot/         ← created child-side, put_old for pivot_root
```

`<root>/<id>/` rather than `<root>/<id>` directly-as-rootfs, because Stage 3
puts a cgroup path and Stage 6 puts state and log files as siblings of `rootfs/`.
The extra level costs nothing now and avoids a migration later.

`.forge-oldroot` is dotted and Forge-prefixed to minimise collision with a real
entry in a user-supplied source tree; a collision is detected and reported
rather than silently reused.

### 4.2 Init payload (the re-exec boundary, ADR-0008)

```go
type initPayload struct {
    Namespace  namespace.Config `json:"namespace"`   // [Stage 1]
    Command    []string         `json:"command"`     // [Stage 1]
    Env        []string         `json:"env"`         // [Stage 1]
    Mount      *mount.Plan      `json:"mount,omitempty"`      // NEW; nil = no pivot
    WorkingDir string           `json:"working_dir,omitempty"` // NEW
}
```

A nil `Mount` is what preserves Stage 1 behaviour when `--rootfs` is absent, and
it is what makes the payload backward-compatible in the only sense that matters
here: an old init reading a new payload is impossible, since parent and child are
the same binary.

`mount.Plan` must be JSON-serialisable, which is why `Option` is a string type
and `Flags()` is derived rather than stored — a raw `uintptr` bitmask on the wire
would be unreadable in a `--log-level debug` dump and unverifiable in a test.

### 4.3 Cleanup stack (SSOT §11.3)

```go
type cleanupStack struct{ /* fns []func() error; logger */ }

func (c *cleanupStack) push(name string, fn func() error)
func (c *cleanupStack) unwind()   // reverse order, WARN on failure, never returns an error
```

---

## 5. Syscalls required

All via `syscall`/`golang.org/x/sys/unix`; no shelling out (SSOT §13.1).

| Syscall | Where | Purpose |
|---|---|---|
| `mount(2)` `MS_BIND` | child | Bind the rootfs onto itself so `pivot_root` has a mount point; every `--mount`; the `/dev/*` device nodes |
| `mount(2)` `MS_BIND\|MS_REC` | child | Recursive binds, so a source with submounts arrives complete |
| `mount(2)` `MS_REMOUNT\|MS_BIND` | child | Apply `ro`/`nosuid`/`nodev`/`noexec`. A first `MS_BIND` call **silently ignores** these; the second call is not optional |
| `mount(2)` `proc`/`sysfs`/`tmpfs`/`devpts` | child | The default mount set |
| `mount(2)` `MS_REC\|MS_PRIVATE` | child | Already Stage 1, in `namespace.Apply`; a prerequisite for all of the above |
| `pivot_root(2)` | child | FR-2.1. No Go stdlib wrapper — `unix.PivotRoot`, or `syscall.Syscall(SYS_PIVOT_ROOT, …)` |
| `umount2(2)` `MNT_DETACH` | child | Detach the old root after the pivot; also `mount.Cleanup` |
| `chdir(2)` | child | Before `pivot_root` (to `new_root`), after it (to `/`), and to `WorkingDir` |
| `mkdir(2)` / `MkdirAll` | both | Destination directories, `put_old`, the per-container tree |
| `openat(2)` + `O_CREAT` | child | Destination *files* for file binds (`/dev/null` style targets) |
| `rmdir(2)` / `unlinkat(2)` | both | `put_old`; `Store.Remove` |
| `lstat(2)` / `fstatat(2)` | both | Source validation, `IsMountPoint`, symlink detection |
| `readlinkat(2)` | parent | Resolving a symlinked `--rootfs` before it becomes a mount source |
| `execve(2)` | child | Unchanged (Stage 1) |

`pivot_root(2)`'s preconditions, each of which the design satisfies deliberately:
`new_root` and `put_old` must be directories; `new_root` must be a mount point;
`put_old` must be at or under `new_root`; neither may be on the same mount as the
current root; and the current root must not have shared propagation.

**Why not `chroot(2)` (FR-2.1, ADR-0001):** `chroot` changes only the calling
process's root *directory* — the old root remains mounted and reachable. A
process with `CAP_SYS_CHROOT` escapes with the classic `mkdir tmp; chroot tmp;
chdir("../../..")` in a dozen lines, and its mount table still lists every host
mount. `pivot_root` moves the *mount* and lets the old root be unmounted, so
there is nothing left to walk back to. `chroot` stays in the docs as the
comparison FR-2.1 asks for, not in the code.

---

## 6. Edge cases

Grouped by what goes wrong. Each is a test in §7 unless marked *(documented
limitation)*.

**Source rootfs**
1. `--rootfs` does not exist, is a file, or is a symlink → resolve, then reject with a named error.
2. `--rootfs /` → rejected outright: pivoting onto the host root would be a very confusing no-op-shaped disaster.
3. `--rootfs` is on a filesystem mounted `noexec`/`nosuid` → the container's binary silently fails to exec. Detected and reported as an init error, not a bare `EACCES`.
4. Source tree lacks `/proc`, `/dev`, `/sys` mount points → created by Forge before mounting (a minimal rootfs frequently ships without them).
5. Source tree already contains `.forge-oldroot` → rejected; do not reuse a directory whose contents are unknown.
6. Source contains device nodes or setuid binaries → mounted as-is. *(documented limitation: no user-namespace remapping, PRD §5 — a setuid binary in the rootfs is host-root inside the container.)*
7. Two concurrent `forge run`s share one `--rootfs` source → both bind it, both write to it, no isolation between them. *(documented limitation: resolved by Stage 5's layering, ADR-0003. `--read-only` is the mitigation available now.)*

**Bind mounts**
8. Destination escapes the rootfs — `../`, an absolute symlink inside the source tree, or a symlink planted mid-path — → containment is checked after resolving every component *against the rootfs*, not against the host's `/`. This is the single most security-relevant check in the stage and is tested adversarially.
9. Destination's parent does not exist → created (`MkdirAll`) with 0755.
10. Destination exists as a file and the source is a directory, or vice versa → rejected with a message naming both.
11. Source of a bind does not exist → rejected in the *parent*, so the error names the user's flag rather than surfacing as an opaque init failure.
12. Nested destinations (`/var` and `/var/log`) → depth-ordered so the outer mount cannot hide the inner one.
13. Duplicate destinations, including a user mount colliding with a default → rejected, not last-wins.
14. `ro` on a recursive bind applies only to the top mount on kernels below 5.12 (`mount_setattr(2)` `AT_RECURSIVE` is 5.12+, and Forge targets 5.10+ per NFR-6). *(documented limitation, with the kernel version named.)*
15. Bind source is itself a mount point with `MS_SHARED` propagation → the `MS_REC|MS_PRIVATE` in `namespace.Apply` covers it; the failure mode if that step were ever removed is a container mount landing on the host, so the existing Stage 1 test guards it.

**pivot_root**
16. Rootfs is not a mount point → the self-bind is what prevents this; if the bind is skipped, `pivot_root` fails `EINVAL`, which is translated into a message that explains the requirement.
17. `put_old` cannot be created because the rootfs was already remounted read-only → ordering forbids it: `put_old` is created before the `ro` remount. Its `rmdir` afterwards is best-effort and logs `WARN` on a read-only root, leaving one empty directory rather than failing a healthy container.
18. Old root still visible after the pivot → `umount2(MNT_DETACH)` runs unconditionally; a test asserts the container's own `/proc/self/mountinfo` contains no host paths.
19. `MNT_DETACH` is lazy: the unmount completes when the last reference drops. Since the only referencing process is about to `execve` with `put_old` no longer its cwd, there is no reference. The `chdir("/")` between pivot and umount is what guarantees that.

**Lifecycle and cleanup**
20. Container exits normally → mounts die with the namespace; parent removes the directory tree.
21. Container is killed by a cancelled context (Stage 1's path) → identical, because the mounts were never the parent's.
22. `forge` itself is `SIGKILL`ed → the container survives (existing Stage 1 limitation) and `<root>/<id>/` is left behind. Reconciled by `mount.Cleanup` + `Store.Remove` when Stage 6 adds the state store; until then a stray empty directory. *(documented limitation, carried forward from Stage 1.)*
23. `Store.Remove` while a mount still exists under the tree → refused, `ErrStillMounted`. This is the guard that keeps `os.RemoveAll` from ever descending through a bind mount into the host filesystem. Non-negotiable, checked first.
24. Init fails after some mounts succeeded → no unwinding; the namespace's destruction is the unwind (PRD NFR-8).
25. `--root` itself is on a shared mount, or is a symlink → resolved and its propagation made irrelevant by the child-side-only rule.
26. Container ID collision with an existing directory → `Prepare` fails rather than adopting an unknown tree; a 48-bit ID makes this effectively impossible but "effectively" is not a guarantee (ADR-0005).

**Interaction with Stage 1**
27. `--mount`, `--read-only` or `--workdir` without `--rootfs` → usage error.
28. No `--rootfs` at all → Stage 1 behaviour exactly, no pivot, `Mount` payload nil. Stage 1's integration tests must still pass unmodified; that is the regression signal.
29. The command path is now resolved inside the new root, so `forge run --rootfs /tmp/alpine /bin/sh` needs `/bin/sh` to exist *in the rootfs*. The "command not found" error must say which root it searched, or this becomes the stage's most common confused bug report.
30. `WorkingDir` does not exist inside the container → init fails with 127 naming the directory, rather than silently landing in `/`.

---

## 7. Test plan

Following SSOT §7: unit tests run without root, integration tests are build-tagged
`integration`, skip with a message when not root, and clean up on failure via
`t.Cleanup`.

### 7.1 Unit — `internal/mount` (no root, table-driven)

- `TestMountFlags` — every `Option` → expected `MS_*` bit; combinations; unknown option rejected.
- `TestNeedsRemount` — bind + `ro`/`nosuid`/`nodev`/`noexec` requires a second call; tmpfs + `ro` does not.
- `TestParseMountSpec` — `src:dst`, `src:dst:ro,nodev`, missing colon, empty field, relative path, unknown option, `:`-containing paths.
- `TestPlanValidate` — absolute destinations required; duplicate destinations rejected; empty root rejected; bind with empty source rejected.
- `TestPlanOrdered` — `/var/log` after `/var`; stable for siblings; independent of input order.
- `TestDestinationContainment` — **the adversarial table**: `../etc`, `/a/../../etc`, a symlinked component pointing to `/etc`, an absolute symlink, a symlink to `..` several levels deep, a destination equal to the root. Every case must be rejected. Built on a `t.TempDir()` tree, no privileges needed.
- `TestValidateRejectsHostRoot` — `/` as a mount root.

### 7.2 Unit — `internal/rootfs` (no root)

- `TestStoreLayout` — `Prepare`/`Lookup` produce `<root>/<id>/rootfs`; permissions 0700/0755.
- `TestPrepareIsNotIdempotentByAccident` — a second `Prepare` for the same ID fails rather than adopting the tree.
- `TestRemoveIsIdempotent` — removing an absent ID is nil.
- `TestRemoveRefusesWhileMounted` — with `IsMountPoint` faked at the seam; asserts `ErrStillMounted`.
- `TestValidateSource` — missing, file, symlink-to-dir (accepted, resolved), symlink-to-file (rejected), `/` (rejected), relative (rejected).

### 7.3 Unit — `internal/runtime` (no root)

- `TestSpecValidateStage2` — `--mount`/`--read-only`/`--workdir` without a rootfs; bad mount specs; empty command unchanged.
- `TestDefaultMountSet` — the exact set and its options, so a change to what every container gets is a deliberate diff.
- `TestMountPlanIncludesUserMountsAfterDefaults` — ordering and shadowing rules.
- `TestInitPayloadRoundTrip` — a `Plan` survives JSON; a nil `Mount` decodes as "no pivot".
- `TestCleanupStackUnwindsInReverse` — with a failing closure in the middle; asserts every closure ran and the failure was logged, not returned.

### 7.4 Unit — `internal/cli` (no root)

- `TestRunFlagsStage2` — repeated `--mount`, `--read-only`, `--workdir`, `--rootfs`; usage errors exit 1 and print usage.

### 7.5 Integration — `test/integration/stage2_test.go` (root)

Same harness shape as Stage 1: the test binary re-executes itself as the
container, dispatching `__init` in `TestMain`. A helper builds a throwaway rootfs
in `t.TempDir()` containing the statically-linked test binary at a known path,
so the tests depend on no host image.

**FR-2.1 — pivot_root**
- `TestContainerRootIsTheRootfs` — a file written into the source tree is visible at `/` inside; a marker file on the host `/` is not.
- `TestHostRootIsUnreachable` — the container cannot reach the host tree by any path, including `../` walking from `/` and from a nested cwd.
- `TestOldRootIsDetached` — the container's `/proc/self/mountinfo` contains no host mount paths and no `.forge-oldroot`.
- `TestProcIsTheContainersOwn` — `/proc` inside lists exactly the container's process tree, closing Stage 1's documented limitation. Directly asserts the FR-1.1 payoff.
- `TestPivotRootFailsWithoutAMountPoint` — a negative test that removes the self-bind step and asserts the translated `EINVAL` message.

**FR-2.2 — bind mounts**
- `TestBindMountIsVisibleInContainer` — host file readable at the destination.
- `TestReadOnlyBindMountRejectsWrites` — write fails `EROFS`; and the same source is still writable on the host, proving the `ro` came from the remount and not from permissions.
- `TestNestedBindMountsAreOrdered` — `/var` and `/var/log` both readable.
- `TestBindMountEscapeIsRejected` — the §6.8 adversarial cases end-to-end, asserting the container never sees `/etc/shadow`.
- `TestReadOnlyRootfs` — `--read-only` makes `/` unwritable while a writable bind mount inside it still works.

**FR-2.3 — cleanup**
- `TestNoHostMountResidueAfterRun` — host `/proc/self/mountinfo` is byte-identical before and after a run with a full mount set. The strongest single assertion in the stage.
- `TestMountsDieWithTheNamespace` — the container mounts an extra tmpfs itself and is killed with `SIGKILL`; the host mount table is unchanged.
- `TestContainerDirectoryIsRemoved` — `<root>/<id>` does not exist after `Run` returns, on both the success and the failure path.
- `TestCleanupIsIdempotent` — `mount.Cleanup` twice, and against a directory with no mounts.
- `TestCleanupUnmountsNestedMountsDeepestFirst` — build a host-side nested stack by hand, then reconcile it, exercising the crash path that has no natural trigger.
- `TestRemoveRefusesToDeleteThroughAMount` — a bind of a host directory under `<root>/<id>`, then `Remove`; asserts the host directory's contents survive. This test exists because the failure it guards against is unrecoverable.

**FR-2.4 — per-container rootfs**
- `TestRootfsPathIsPerContainer` — two concurrent containers get distinct trees and cannot see each other's.
- `TestRootfsHonoursRootFlag` — `--root` in a temp dir is where the tree appears.

**Regression**
- The entire Stage 1 suite must pass unmodified. `TestStage1BehaviourWithoutRootfs` asserts explicitly that omitting `--rootfs` still yields the host filesystem view.

### 7.6 Definition of done (PRD §10)

`make test` green without root; `sudo -E make test-integration` green; no manual
cleanup needed afterwards (`mountinfo` and `--root` both clean); README roadmap,
example commands and project structure updated; SSOT §2 and §11 updated in the
same PR; ADRs below written and `Accepted`.

---

## 7a. Refinements the tests forced

Writing the tests before the implementation surfaced three gaps in §3, recorded
here and folded into the API above.

1. **`Plan.Source`.** §1.3 described the first step as binding the rootfs onto
   itself to make it a mount point. That is Stage 5's shape, where layers are
   unpacked into the per-container directory. In Stage 2 the content lives in
   the user's `--rootfs` tree, so the first mount is `Source → Root`, which
   makes `Root` a mount point *and* populates it in one operation. When Stage 5
   arrives, `Source == Root` and the same code performs a self-bind. Without the
   field there was nowhere to say which host tree the container's root comes
   from.

2. **`NewRunner` returns an error.** It constructs the `rootfs.Store`, which
   validates and creates the storage root — the first thing that can fail.

3. **`cli.parseRunSpec(args) (runtime.Spec, error)`.** `execRun` parsed flags
   and ran the container in one function, so flag handling could only be tested
   by starting a container, which needs root. Splitting parsing out keeps SSOT
   §13.6's promise that the CLI layer is testable without invoking anything
   below it. `execRun` becomes: parse, attach streams, run, map status to an
   exit code.

## 8. ADRs to write

| ID | Title | Note |
|---|---|---|
| 0001 | Use `pivot_root` instead of `chroot` for rootfs isolation | Already listed as *Proposed* in SSOT §15. Fill in and accept. Records the escape argument in §5 and keeps `chroot` as teaching comparison per FR-2.1 |
| 0010 | Per-container rootfs layout and how Stage 2 populates it | `<root>/<id>/rootfs`; bind-mounting a user-supplied source tree rather than copying it; the shared-writes limitation and its Stage 5 resolution |
| 0011 | The mount plan is built by `runtime`, executed by `mount` | Why the default mount set is policy (`runtime`) and not mechanism (`mount`), per SSOT §2's "must NOT decide *what* to mount" |
| 0012 | All container mounts are made child-side, inside the mount namespace | The FR-2.3 argument in §1.1: cleanup by namespace destruction rather than by a cleanup routine that must be correct. Also records why `mount.Cleanup` still exists |

An ADR is also owed if the `pivot_root(".", ".")` form is chosen over an explicit
`put_old` directory — the alternative is recorded in 0001 either way. This design
recommends the explicit form: it costs one directory and one edge case (§6.17),
and it is legible to a reader meeting `pivot_root` for the first time, which is
the project's stated purpose (PRD §2).

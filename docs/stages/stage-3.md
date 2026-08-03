# Stage 3 — Resource Limits (design)

**Status:** Designed, not implemented
**Requirements:** PRD §8.3 (FR-3.1 … FR-3.5)
**New packages:** `internal/cgroup`
**ADRs to write:** 0002 (direct cgroupfs writes), 0014 (attach point), 0015 (degrade when v2 absent)

---

## 0. What Stage 3 adds

```bash
sudo forge run -rootfs /srv/alpine -memory 128m -cpus 1.5 -pids 64 /bin/sh
```

The container's process tree lives in a cgroup v2 leaf of its own, with
`memory.max`, `cpu.max`, `cpu.weight` and `pids.max` written before the
container's binary is ever `execve`d. When the container exits, the leaf is
removed.

Stages 1 and 2 are untouched: namespaces still come from `clone(2)`, the rootfs
is still built child-side. Stage 3 adds exactly one thing to the parent's
sequence — a directory under `/sys/fs/cgroup` and a PID written into it.

Out of scope: `io.max` (no block-device policy until images land), `memory.high`
/ `memory.low` (FR-3.2 names `memory.max` only), swap accounting, cgroup
namespaces (`CLONE_NEWCGROUP` — the container can still *see* the host
hierarchy under `/sys`, which is read-only anyway), live limit updates
(`forge update`, Stage 6), and any cross-restart bookkeeping of leaves (Stage 6
state store).

---

## 1. Package layout

```
internal/cgroup/
├── cgroup.go     # package doc, Limits, quantity types, sentinel errors, Files()
├── parse.go      # ParseBytes / ParseCPUs / ParseWeight / ParsePIDs — pure value parsers
├── manager.go    # Manager, New, Create — hierarchy discovery, controller delegation
├── apply.go      # Cgroup.Apply, Cgroup.Add — the filesystem writes
└── cleanup.go    # Cgroup.Destroy, Manager.Destroy — kill-then-rmdir, idempotent
```

This mirrors `internal/mount`: pure computation (`cgroup.go`, `parse.go`) is
separated from the calls that touch the kernel (`apply.go`, `cleanup.go`), so
the majority of the package is testable without root — and, as §7 explains, so
is most of the rest.

Files touched elsewhere:

| File | Change |
|---|---|
| `internal/runtime/runtime.go` | `Runner.cgroups`, `Spec.Limits`, `prepareCgroup`, attach step |
| `internal/runtime/limits.go` (new) | policy: defaults and the `Spec` → `cgroup.Limits` mapping |
| `internal/cli/run.go` | `-memory`, `-cpus`, `-cpu-weight`, `-pids` flags; new sentinels in `isUserError` |
| `test/integration/stage3_test.go` (new) | privileged enforcement tests |

**No third-party dependency.** `golang.org/x/sys/unix` is not even needed here —
cgroup v2 is a filesystem interface, so this is `os.WriteFile` and `os.Mkdir`
(SSOT §10, ADR-0002).

### Boundary: who parses what

The constraint is that `internal/cgroup` must not parse CLI flags, and SSOT §2
says it "receives a typed `ResourceLimits` struct". It does:

- `internal/cli` owns flag *names*, registration, usage text, and the
  `flag.FlagSet`. It never computes a limit.
- `internal/cgroup` owns *value* parsing — `"128m" → Bytes(134217728)`,
  `"1.5" → CPUQuota{150ms, 100ms}` — as pure functions with no knowledge of
  where the string came from. This is exactly the `mount.ParseMountSpec`
  precedent from Stage 2, and it keeps the unit-conversion rules next to the
  types that define them rather than in a CLI package that is supposed to hold
  no logic (SSOT §13.6).
- `internal/runtime` owns *policy*: what a missing limit means, and what the
  defaults are.

### Dependency graph after Stage 3

```
cli → runtime → { namespace, process, rootfs, mount, cgroup }
```

`cgroup` imports only the standard library. It is a leaf: it does not know what
a container is, only what a leaf directory and a `Limits` value are (SSOT §13.2).

---

## 2. Exported types

```go
// Controller is a cgroup v2 controller name, as it appears in
// cgroup.controllers and cgroup.subtree_control.
type Controller string

const (
    ControllerCPU    Controller = "cpu"
    ControllerMemory Controller = "memory"
    ControllerPIDs   Controller = "pids"
)

// Bytes is a memory quantity. String renders the decimal form the kernel
// expects in memory.max.
type Bytes int64

// Weight is a cpu.weight value: relative CPU share, 1..10000, default 100.
type Weight uint64

// CPUQuota is a cpu.max setting: Quota of CPU time per Period.
// A zero Quota means "max" — unlimited — which is the kernel's own default.
type CPUQuota struct {
    Quota  time.Duration
    Period time.Duration // zero means DefaultCPUPeriod (100ms)
}

// Limits is the complete set of constraints for one container. A nil field
// means "not set by the caller": Forge writes nothing and the kernel's
// inherited value stands. This is why every field is a pointer — 0 is a
// meaningful value for memory.max and pids.max, and "unset" must not collide
// with it.
type Limits struct {
    MemoryMax *Bytes    // FR-3.2 → memory.max
    CPU       *CPUQuota // FR-3.3 → cpu.max
    CPUWeight *Weight   // FR-3.3 → cpu.weight
    PIDsMax   *int64    // FR-3.4 → pids.max
}

// File is one controller file and the exact bytes to write to it. Limits.Files
// produces these; Cgroup.Apply performs them. Splitting the two is what makes
// the whole limit-rendering layer testable as pure data.
type File struct {
    Name  string // e.g. "memory.max", relative to the leaf
    Value string // e.g. "134217728"
}

// Config configures a Manager. Zero values take the defaults, so
// cgroup.New(logger, cgroup.Config{}) is the production configuration.
type Config struct {
    Root   string // unified hierarchy mount point; default "/sys/fs/cgroup"
    Parent string // Forge's own subtree under Root; default "forge"
}

// Manager owns Forge's parent cgroup and creates per-container leaves under it.
type Manager struct { /* root, parent, logger, enabled controllers */ }

// Cgroup is one container's leaf (FR-3.1).
type Cgroup struct { /* id, path, logger */ }
```

Constants: `DefaultRoot`, `DefaultParent`, `DefaultCPUPeriod`, `MinWeight`,
`MaxWeight`, `DefaultWeight`.

Sentinel errors (SSOT §5 — callers branch on these; `ErrControllerUnavailable`
is named in SSOT §5 by this exact spelling):

```go
var (
    ErrUnifiedHierarchyNotMounted = errors.New("cgroup v2 unified hierarchy is not mounted at the expected path")
    ErrControllerUnavailable      = errors.New("required cgroup controller is not available")
    ErrInvalidLimit               = errors.New("invalid resource limit")
    ErrPermission                 = errors.New("managing cgroups requires root (or CAP_SYS_ADMIN)")
    ErrNotEmpty                   = errors.New("cgroup still has member processes")
)
```

---

## 3. Interfaces

**The package defines none, deliberately.** Forge's existing primitives
(`process`, `mount`, `rootfs`) are concrete types, and an interface here would
buy nothing: the seam that makes `cgroup` testable is `Config.Root`, not a
mock. Point it at a `t.TempDir()` containing a synthetic `cgroup.controllers`
file and every code path except kernel *enforcement* runs unprivileged, against
real `os` calls, asserting real file contents. A mock would test less and cost
an indirection.

The one interface Stage 3 may introduce is unexported and lives in
`internal/runtime`, only if the runtime's own unit tests need to run without a
cgroup filesystem at all:

```go
// runtime/limits.go — satisfied by *cgroup.Manager.
type cgroupManager interface {
    Create(id string, limits cgroup.Limits) (*cgroup.Cgroup, error)
    Destroy(ctx context.Context, id string) error
}
```

Declared in the consumer, per Go convention and SSOT §4. If the temp-dir seam
proves sufficient (it should), this is dropped.

---

## 4. Function signatures

### `internal/cgroup` — pure

```go
func ParseBytes(s string) (Bytes, error)      // "128m", "1G", "134217728", "max"
func ParseCPUs(s string) (CPUQuota, error)    // "1.5" → 150000/100000
func ParseWeight(s string) (Weight, error)    // "1".."10000"
func ParsePIDs(s string) (int64, error)       // "64", "max"

func (b Bytes) String() string                // "134217728" | "max"
func (q CPUQuota) String() string             // "150000 100000" | "max 100000"
func (w Weight) String() string

func (q CPUQuota) Validate() error
func (l Limits) Validate() error              // ErrInvalidLimit, one message per bad field
func (l Limits) IsZero() bool                 // no field set → nothing to write
func (l Limits) Controllers() []Controller    // exactly what must be delegated
func (l Limits) Files() []File                // ordered, deterministic; the heart of §7.1
```

### `internal/cgroup` — kernel-touching

```go
// Available reports whether root is a cgroup v2 unified hierarchy Forge can
// use, without creating anything. Returns ErrUnifiedHierarchyNotMounted.
func Available(root string) error

// New validates the hierarchy, creates <Root>/<Parent> if absent, and enables
// in its cgroup.subtree_control every controller Forge can offer. Returns
// ErrControllerUnavailable naming the missing controller, or ErrPermission.
func New(logger *slog.Logger, cfg Config) (*Manager, error)

func (m *Manager) Root() string
func (m *Manager) Path() string                       // <Root>/<Parent>
func (m *Manager) Has(c Controller) bool

// Create makes the leaf <Root>/<Parent>/<id> (FR-3.1) and writes limits into
// it before any process is a member.
func (m *Manager) Create(id string, limits Limits) (*Cgroup, error)

// Destroy removes a leaf by ID with no live handle — the crash-recovery and
// (Stage 6) `forge rm` path named in SSOT §11.2. Idempotent.
func (m *Manager) Destroy(ctx context.Context, id string) error

func (c *Cgroup) ID() string
func (c *Cgroup) Path() string
func (c *Cgroup) Apply(limits Limits) error            // idempotent, re-writable
func (c *Cgroup) Add(pid int) error                    // write pid to cgroup.procs
func (c *Cgroup) Destroy(ctx context.Context) error    // FR-3.5
```

### `internal/runtime`

```go
// Spec gains one field, not four: the CLI has already produced a typed value.
type Spec struct {
    // ...
    Limits cgroup.Limits
}

func (s Spec) validateLimits() error          // called from Spec.Validate

// prepareCgroup creates the container's leaf and registers its removal on the
// cleanup stack. Mirrors prepareFilesystem exactly.
func (r *Runner) prepareCgroup(ctx context.Context, log *slog.Logger, id string,
    spec Spec, cleanup *cleanupStack) (*cgroup.Cgroup, error)
```

`Runner` gains a `cgroups *cgroup.Manager` field; `NewRunner` constructs it
alongside the `rootfs.Store`. `Runner.start` gains a `cg *cgroup.Cgroup`
parameter — see §5.

### `internal/cli`

```go
type runFlags struct {
    // ...
    memory    string   // -memory 128m
    cpus      string   // -cpus 1.5
    cpuWeight string   // -cpu-weight 512
    pids      string   // -pids 64
}

func parseLimits(local *runFlags) (cgroup.Limits, error)  // calls cgroup.Parse*
```

Strings are captured and parsed in `parseRunSpec`, not via `flag.Value`, so an
invalid value is reported by `parseRunSpec` — the function Stage 2 already made
the single unit-testable seam for argument handling.

---

## 5. Runtime integration flow

```
runtime.Run
  ├─ spec.Validate()                       ← limits rejected before anything is created
  ├─ NewID()
  ├─ cleanup := newCleanupStack(log)
  ├─ prepareFilesystem(...)                ← Stage 2, unchanged
  │     └─ cleanup.push("removing the container root filesystem", …)
  ├─ prepareCgroup(...)                    ← NEW
  │     ├─ cgroups.Create(id, spec.Limits) ← mkdir leaf + write memory.max / cpu.max /
  │     │                                     cpu.weight / pids.max  (FR-3.1..3.4)
  │     └─ cleanup.push("removing the container cgroup", cg.Destroy)   (FR-3.5)
  ├─ start(...)
  │     ├─ os.Pipe()                       ← the init payload pipe
  │     ├─ process.New/Start               ← clone(2); child blocks reading the pipe
  │     ├─ cg.Add(p.PID())                 ← NEW: the container joins its cgroup
  │     ├─ writePayload(...)               ← child unblocks, mounts, pivots, execve
  │     └─ p.Wait(ctx)
  └─ defer cleanup.unwind()                ← cgroup destroyed, then rootfs removed
```

### Why the attach happens between `Start` and `writePayload`

This is the stage's one non-obvious decision (ADR-0014). A cgroup can only be
joined by writing a PID to `cgroup.procs`, which requires the process to exist —
so there is an inherent window between `clone(2)` and membership, and in a naive
implementation the container could fork or allocate inside it.

Stage 1's re-exec handshake closes that window for free. The child is
`forge-init`, Forge's own code, and its first action is a blocking read on the
payload pipe (ADR-0008). It cannot mount, `pivot_root`, `execve`, or fork until
the parent writes. Attaching *before* the write therefore guarantees every limit
is in force before a single instruction of the user's binary runs, using
machinery that already exists.

The alternative — `SysProcAttr.CgroupFD` / `CLONE_INTO_CGROUP`, which makes the
child a member atomically at `clone(2)` — is strictly cleaner but forces Go onto
`clone3(2)`, which older seccomp profiles (notably Docker's pre-20.10 default)
reject with `EPERM`, and would put a cgroup file descriptor into
`process.Config`, which SSOT §2 says must know nothing about cgroups. Recorded
as the rejected option in ADR-0014; it is a one-field change if Forge ever drops
the compatibility concern.

Because the child inherits its parent's cgroup at `clone(2)`, and Forge's own
process sits outside `<Root>/<Parent>`, the container is briefly a member of
forge's cgroup. That is why the leaf is created — with its limits already
written — before `process.Start`, not after: the migration is into an
already-constrained group.

### When cgroups are unavailable

`NewRunner` calls `cgroup.New`, which fails on a cgroup v1/hybrid host. Failing
`forge run` outright there would regress Stages 1 and 2 on machines where they
work today. The rule (ADR-0015):

| `spec.Limits.IsZero()` | v2 available | Behaviour |
|---|---|---|
| yes | yes | leaf created, no limit files written (FR-3.1 holds; accounting is free) |
| yes | no | `WARN` once, container runs unconstrained |
| no | yes | leaf created and limits applied |
| no | no | hard error: `ErrUnifiedHierarchyNotMounted`, nothing is started |

A limit the user asked for is never silently dropped (SSOT §13.7). A limit they
did not ask for never blocks a run.

`isUserError` in `internal/cli` gains `cgroup.ErrInvalidLimit` (exit 1) but not
`ErrUnifiedHierarchyNotMounted` or `ErrControllerUnavailable`, which are
environment failures (exit 2).

---

## 6. Cleanup strategy (FR-3.5)

Unlike mounts, cgroups get no help from the kernel: a mount namespace dies with
its last process, but an empty cgroup directory persists until something calls
`rmdir(2)`. FR-3.5 is therefore real work, and `Cgroup.Destroy` is the only
place it happens.

```
Destroy(ctx):
  ctx = context.WithoutCancel(ctx)   // a cancelled run must still release
  1. rmdir(path)                     // the common case: everything already exited
     ├─ nil    → done
     ├─ ENOENT → done                // idempotent (SSOT §13.3)
     └─ EBUSY  → step 2
  2. read cgroup.procs
     ├─ empty → the kernel has not yet finished reaping; retry rmdir
     └─ non-empty → step 3
  3. evict survivors:
     ├─ write "1" to cgroup.kill     // kernel ≥ 5.14: atomic, kills descendants
     └─ fallback: SIGKILL each PID in cgroup.procs   // kernel 5.10 … 5.13
  4. poll cgroup.procs until empty — bounded attempts, exponential backoff,
     honouring ctx — then rmdir.
  5. still EBUSY → return ErrNotEmpty wrapping the survivor PIDs.
```

Registration and ordering:

- Pushed onto `cleanupStack` immediately after `Manager.Create` succeeds, so
  every failure path from that point on — payload encode, `process.Start`,
  handshake failure — removes the leaf (SSOT §11.3).
- The stack unwinds in reverse, so the cgroup goes before the rootfs directory.
  That is the right order: killing stragglers first means nothing is holding the
  container's directory open when `rootfs.Store.Remove` runs.
- `unwind` swallows errors into `WARN` logs, so a leaf Forge cannot remove is
  loud but never masks the container's own failure (SSOT §5).

Survivors are expected to be rare but are not a pathological case: a container
that double-forks a daemon leaves it running after PID 1 exits only if it escaped
the PID namespace — which it cannot — so in practice step 3 fires when the kernel
is mid-reap. Step 3 exists so that "removes the cgroup when the container exits"
is true unconditionally rather than usually.

`<Root>/<Parent>` — `/sys/fs/cgroup/forge` — is created on demand and **never
removed**. Two concurrent `forge run`s would race to `rmdir` a directory the
other is about to create in, and an empty cgroup directory costs nothing.
Documented in ADR-0015 and in the README's residue example.

Orphans from a `SIGKILL`ed forge are not reconciled in Stage 3; the leaf is
visible under `/sys/fs/cgroup/forge/<id>` and `Manager.Destroy(ctx, id)` exists
to remove it. A sweep at startup belongs with the Stage 6 state store, which is
what will know which IDs are live.

---

## 7. Testing strategy

TDD, per SSOT §7: the table-driven unit tests in §7.1 are written before the
package compiles.

### 7.1 Unit — `internal/cgroup`, pure (no root)

Table-driven, and the largest test file in the stage:

- `ParseBytes`: `"128m"`, `"1G"`, `"1024"`, `"max"`, `""`, `"-1"`, `"12x"`,
  `"1.5g"`, overflow. Asserts the exact `Bytes` value — a units bug here is a
  silently wrong limit, the worst failure mode in the stage.
- `ParseCPUs`: `"1"` → `100000 100000`, `"0.5"` → `50000 100000`, `"2.5"`,
  `"0"`, `"-1"`, `"abc"`; sub-microsecond quotas rejected.
- `ParseWeight` / `ParsePIDs`: bounds, `"max"`, non-numeric.
- `Limits.Files()`: every combination of set/unset fields → exact
  `[]File{{"memory.max","134217728"}, {"cpu.max","150000 100000"}, …}`, and
  `nil` for the zero `Limits`. Deterministic ordering asserted.
- `Limits.Validate()` / `Controllers()` / `IsZero()`.

### 7.2 Unit — `internal/cgroup`, filesystem (no root)

The `Config.Root` seam. Each test builds a synthetic hierarchy in `t.TempDir()`:
a `cgroup.controllers` file listing `cpu memory pids`, an empty
`cgroup.subtree_control`. Then:

- `New` creates `<root>/forge` and writes `+cpu +memory +pids` to
  `cgroup.subtree_control`.
- `New` on a root with no `cgroup.controllers` → `ErrUnifiedHierarchyNotMounted`.
- `New` on a root whose `cgroup.controllers` lacks `memory` →
  `ErrControllerUnavailable`, error text naming `memory`.
- `Create` makes the leaf and leaves exactly the expected file contents on disk;
  read them back and compare bytes.
- `Create` with zero `Limits` writes no limit files at all.
- `Apply` is idempotent: applying twice yields the same contents.
- `Add` appends the PID to `cgroup.procs`.
- `Destroy` removes the leaf; `Destroy` on an already-removed leaf returns nil;
  `Destroy` after `Create` twice is safe.

This covers the primary success *and* failure path of every exported function
(SSOT §7 coverage gate) without root, which is the point of the seam.

The gap it cannot cover, and which §7.4 exists for: the kernel enforcing
anything, `subtree_control` rejecting a controller, `cgroup.procs` migration
semantics, and `EBUSY` on `rmdir`.

### 7.3 Unit — `internal/runtime` and `internal/cli` (no root)

- `Spec.Validate` with invalid limits → `ErrInvalidLimit`, and with valid ones →
  nil; existing table extended.
- `parseRunSpec`: `-memory 128m -cpus 1.5 -pids 64` → the expected
  `cgroup.Limits`; each flag rejected individually with a bad value; no flags →
  `Limits.IsZero()`.
- `isUserError(cgroup.ErrInvalidLimit)` is true; `isUserError` of the
  environment sentinels is false.
- `forge run -h` usage snapshot includes the four new flags.

### 7.4 Integration — `test/integration/stage3_test.go` (root, `//go:build integration`)

Using the existing harness, each test `t.Cleanup`s its container:

- **FR-3.1** the leaf exists at `/sys/fs/cgroup/forge/<id>` while the container
  runs, and the container's PID appears in its `cgroup.procs`.
- **FR-3.2** `-memory 32m` running an allocator: process dies, exit status 137,
  and the leaf's `memory.events` shows `oom_kill 1`. Read before teardown.
- **FR-3.3 quota** `-cpus 0.5` running a fixed-work busy loop: `cpu.stat`'s
  `throttled_usecs` is non-zero and `nr_throttled > 0`. Asserting a *ratio*
  rather than wall-clock time keeps this deterministic on a loaded CI box, and
  there is no `time.Sleep` anywhere (SSOT §7).
- **FR-3.3 weight** `-cpu-weight 512` → `cpu.weight` reads `512`.
- **FR-3.4** `-pids 16` running a fork loop: forks fail with `EAGAIN`, and
  `pids.events` shows `max` incremented.
- **FR-3.5** after `forge run` returns, `/sys/fs/cgroup/forge/<id>` does not
  exist; extends Stage 1's `TestNoHostResidueAfterRun` to count leaves under
  `/sys/fs/cgroup/forge` before and after (invariant: unchanged).
- **Cleanup on failure**: a run that fails after cgroup creation (an unreadable
  `-rootfs`) still leaves no leaf behind — the `cleanupStack` registration.
- **Destroy with a survivor**: create a leaf directly via the package, put a
  sleeping process in it, `Destroy`, assert the process is killed and the
  directory gone — the only test of step 3 of §6.
- **Skip, don't fail**, when `/sys/fs/cgroup/cgroup.controllers` is absent, with
  a message naming the requirement, so the suite stays usable on a v1 host.

### 7.5 Definition of done (PRD §10)

Unit and integration tests pass; SSOT §2/§11/§15 updated with the cgroup rows;
README roadmap flips Stage 3 to complete and gains a Stage 3 example block;
ADRs 0002, 0014 and 0015 written.

---

## 9. Post-review hardening

Three changes made after the Stage 3 security and correctness review. None of
them alters the architecture or an exported signature.

### 9.1 `memory.max` never travels alone (swap policy)

`memory.max` caps a cgroup's **resident** memory. cgroup v2 accounts swap
separately in `memory.swap.max`, which defaults to unlimited, so a container
held at 32MB of RAM could page out indefinitely: never OOM-killed, just slower,
while the host thrashes. A limit that can be escaped by being slow is not a
limit, and the giveaway was in the integration test — it had to write
`memory.swap.max=0` by hand before the OOM assertion became reliable.

`Limits.Files` now emits a swap limit with every memory limit:

| `MemoryMax` | `memory.max` | `memory.swap.max` |
|---|---|---|
| unset (nil) | not written | not written |
| finite, e.g. 128MB | `134217728` | `0` |
| explicitly unlimited | `max` | `max` |

**The trade-offs this makes:**

- *Swap is not independently configurable.* There is no `-memory-swap` flag and
  no `SwapMax` field. Docker has one; Forge does not, because two knobs whose
  interaction is famously confusing is a poor trade for an educational runtime.
  Swap policy is derived from memory policy, always.
- *A finite limit means "RAM or death".* A workload that would have survived by
  swapping now gets OOM-killed. That is the intended reading of `--memory 32m`,
  it is what the exit code will say, and it is far easier to diagnose than a
  container that mysteriously crawls. It is stricter than Docker's default,
  which allows swap up to twice the memory limit.
- *Explicit `max` is left alone.* Asking for no memory limit must not be
  silently converted into a swap restriction nobody requested, so the swap file
  mirrors the memory file rather than being forced to `0`.
- *Absence is tolerated.* A kernel built without swap accounting has no
  `memory.swap.max` at all. The file is marked `Optional`, so ENOENT is skipped
  and the memory limit still applies. This is the one case where the limit
  remains soft, and nothing in userspace can fix it: such a kernel offers no
  per-cgroup swap interface to anyone. It is not silent suppression — the
  condition is precisely "the interface does not exist" — but it is a real
  residual gap and is called out here rather than buried.

### 9.2 `removeResidualFiles` deleted, not guarded

Teardown used to unlink the regular files in a leaf before retrying `rmdir`.
On real cgroupfs that was inert — the kernel owns the interface files, refuses
to unlink them, and ignores them when deciding whether a cgroup is empty — and
it existed only so that a temp-directory test could destroy a leaf. That put a
general-purpose file remover, pointed at a caller-supplied path, on a
production teardown path.

Both offered options were considered:

1. **Remove it entirely.** The deletion code ceases to exist, so it cannot be
   misused, misconfigured, or pointed anywhere. Cost: temp-directory tests can
   no longer destroy a leaf that holds files.
2. **Guard it with a `statfs` cgroup2 magic check.** Superficially the safer
   option, but it is strictly worse on inspection: `statfs` on a temp directory
   reports tmpfs, so the guard disables the unlink in exactly the tests it was
   written for, while on real cgroupfs the unlink was already a no-op. The
   result is dead code plus a syscall plus a new dependency, with the same test
   churn as option 1.

**Option 1 was taken.** The tests adapted instead: `Destroy` is now exercised
against leaves that hold no regular files, and the real behaviour — the kernel
removing a fully-populated cgroup in one `rmdir` — is asserted against the
kernel in `test/integration/stage3_test.go`, which destroys leaves holding
every limit Forge writes.

### 9.3 kernfs writes are `O_WRONLY`

`os.WriteFile` opens `O_WRONLY|O_CREATE|O_TRUNC`. Neither extra flag belongs on
a cgroup interface file:

- `O_CREATE` asks the kernel to create a file it creates itself and refuses to
  duplicate. Worse, on any path that is *not* cgroupfs it **succeeds**, writing
  a limit into a regular file nothing will ever enforce.
- `O_TRUNC` is meaningless to kernfs, whose file contents are generated rather
  than stored, and some files reject the truncate outright.

`writeControlFile` now opens `O_WRONLY`, performs a single `write(2)`, and
checks the error from `Close` as well — some kernfs files report a rejected
value at close rather than at write.

This restores a signal that `O_CREATE` was masking: a missing interface file now
means what the kernel means by it — **the controller was never delegated to this
cgroup** — and maps onto `ErrControllerUnavailable`.

It also changed the shape of the unit tests. A leaf's interface files are
created by the kernel when the leaf is made, and a temp directory does not do
that, so `Manager.Create` can no longer write limits into a fake hierarchy. The
tests were split to match the seam rather than fight it:

| Layer | Where | Covers |
|---|---|---|
| Rendering | `cgroup_test.go` | `Limits.Files` → exact bytes, swap policy included |
| Writing | `apply_internal_test.go` | `applyLimits` against a seeded leaf; optional-file skip; undelegated controller; that a write never creates a file |
| Delegation | `manager_internal_test.go` | `prepareParent` against a seeded hierarchy |
| Exported surface | `manager_test.go` | `Create`/`Add`/`Destroy`, IDs, error paths |
| Enforcement | `test/integration/stage3_test.go` | the real kernel |

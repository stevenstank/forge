# Stage 6 — Runtime (design)

**Status:** Design. No code written.
**Requirements:** PRD §8.6 (FR-6.1 … FR-6.6)
**New packages:** `internal/state`, `internal/logs`
**Grown packages:** `internal/runtime`, `internal/process`, `internal/namespace`,
`internal/cli`
**ADRs required:** 0006 (state store format — currently *Proposed*, this design
accepts it), 0014, 0015, 0016, 0017 (numbers free per SSOT §15)

Stage 6 is the stage where a container stops being an invocation and becomes an
object. Everything before it fits in one process: `forge run` creates the
container, holds the whole cleanup stack in its own memory as closures, and
unwinds it on return. `ps`, `exec`, `stop`, `logs` and `rm` are all statements
about a container that some *other* process created, and that is the whole of
what makes this stage different.

---

## 0. What Stage 6 adds

```bash
sudo forge run -d alpine:3.20 /bin/sh -c 'while true; do date; sleep 1; done'
7f3c9a1b2d04
sudo forge ps
CONTAINER ID   IMAGE          COMMAND                 STATUS         CREATED
7f3c9a1b2d04   alpine:3.20    /bin/sh -c while tru…   running        12 seconds ago
sudo forge logs -f 7f3c9a1b2d04
sudo forge exec 7f3c9a1b2d04 /bin/ls /etc
sudo forge stop -t 5 7f3c9a1b2d04
sudo forge rm 7f3c9a1b2d04
```

Stages 1–5 are untouched at the mechanism level. Namespaces still come from
`clone(2)`, the cgroup is still written before the container joins it, the veth
pair is still made in the handshake window, the mounts and `pivot_root` still
happen child-side, layers are still unpacked by the parent before the clone.
Stage 6 adds three things around that unchanged core:

1. a **record on disk** that outlives the process that created the container,
2. a **supervisor** — a role, not always a separate process — that owns the
   container's resources for the whole of its life, and
3. **five commands** that operate on a container through the record rather than
   through a handle they hold.

**Out of scope**, recorded as decisions rather than omissions:

- Exec sessions are not persisted. A record describes a container, not the
  processes attached to it.
- No reattach: `forge attach` does not exist, and `forge logs` is the only way
  to see a detached container's output. Stdin for a detached container is
  `/dev/null`, permanently.
- No restart policies, no `pause`/`unpause`, no `events`, no `stats`, no
  `forge inspect` beyond what `ps` prints.
- No daemon. There is no long-running `forged`; every command reconciles what
  it finds and exits. Supervisors are per-container and own nothing global.
- Log rotation is fixed policy, not configurable.
- No log driver abstraction. One format, one place.

---

## 1. The shape of the problem

Three kernel facts drive nearly every decision below. They are not Forge's
choices; they are the constraints the design has to be shaped around.

**Only the parent can reap.** `wait(2)` works on children. A container's exit
code can only be collected by the process that called `clone(2)`. If
`forge run -d` returned to the shell, whatever process holds that relationship
must survive, or the exit code is lost and the container becomes a zombie
reparented to host PID 1. This forces a supervisor process and forbids any
design where the CLI hands the container off and exits.

**Only the record survives everything.** A supervisor can be `SIGKILL`ed. The
host can lose power. Any resource whose only trace is a live process's memory
leaks the moment that process dies. So the in-memory `cleanupStack` — which
Stages 1–5 rely on completely — needs a durable twin.

**Signals to PID 1 of a namespace are not ordinary signals.** The kernel
discards a signal sent from an ancestor namespace to a namespace's init unless
that process installed a handler for it. `SIGKILL` and `SIGSTOP` are
special-cased and always delivered. `/bin/sh` as PID 1 installs no `SIGTERM`
handler, so `forge stop` on a shell will *always* run its grace period out and
then `SIGKILL`. This is not a bug to fix; it is a behaviour to document and
design the timeout around.

A fourth fact shapes `exec` specifically and gets its own section (§7):
`setns(2)` refuses `CLONE_NEWNS` from a multithreaded caller, and the Go runtime
is multithreaded before `main` runs.

---

## 2. Package layout

### 2.1 Package diagram

```
                                cmd/forge
                                    │
                                    ▼
                             internal/cli
              run ─ ps ─ exec ─ stop ─ logs ─ rm ─ __init ─ __supervise ─ __exec
                                    │
                                    ▼
       ┌──────────────────────  internal/runtime  ──────────────────────┐
       │            the only orchestrator (SSOT §13.2)                  │
       │                                                               │
       │  runtime.go   Spec, Runner, Run          (Stage 1–5, extended) │
       │  supervise.go the supervisor role, log tee, exit recording     │  NEW
       │  exec.go      exec orchestration + the __exec helper           │  NEW
       │  stop.go      signal, grace, terminal wait                     │  NEW
       │  list.go      ps rows, translation state.Record → Container    │  NEW
       │  remove.go    rm, the retained frame                           │  NEW
       │  recover.go   reconciliation, liveness predicates              │  NEW
       │  record.go    Spec ⇄ state.Record translation                  │  NEW
       └───┬─────┬─────┬─────┬─────┬─────┬─────┬─────┬──────────────────┘
           │     │     │     │     │     │     │     │
           ▼     ▼     ▼     ▼     ▼     ▼     ▼     ▼
       process namespace rootfs mount cgroup network image  state   logs
          │        │       │      │      │      │      │      │       │
          └────────┴───────┴──────┴──────┴──────┴──────┴──────┴───────┘
                    no edges between any two of them (SSOT §13.2)

                              internal/logging
                       (leaf, imported by cli and runtime)
```

Nine primitives after Stage 6, up from seven. The rule that made ADR-0020 worth
writing is unchanged and gains no exception: `state` does not import `logs`,
`logs` does not import `state`, and neither imports `process`, `cgroup` or
`network` — even though the record describes cgroups and networks and the
supervisor writes logs about processes.

### 2.2 What each new package owns

| Package | Owns | Must NOT do |
|---|---|---|
| `internal/state` | The per-container record: schema, atomic replacement, per-container locking, listing, deletion. One tree on disk, exclusively. | Touch any kernel resource. Judge whether a PID is alive. Import any type from another primitive. |
| `internal/logs` | The container's captured output: the on-disk format, the writer, rotation, and a reader that can tail and follow. One tree on disk, exclusively. | Know what a container is beyond an ID-shaped directory name. Decide when following should stop. |

Two boundaries here are easy to get wrong and are worth stating as rules.

**`state` stores its own types, never other packages' types.** A `Record` that
embedded `cgroup.Limits` or `network.Mode` would create exactly the
primitive-to-primitive edge SSOT §13.2 forbids, and it would couple an on-disk
format that must stay readable across versions to Go structs that are free to
change. The record's fields are strings, ints, and times. `internal/runtime`
translates in both directions, in `record.go`, and that translation is the only
place the mapping exists.

**`state` never asks whether a process is alive.** SSOT §2 says `state`
performs no kernel resource management, and `kill(pid, 0)` is over the line by
intent even if not by cost. Instead `state` takes liveness as a parameter, the
way `network`'s IPAM already does:

```
network:  func (m *Manager) reclaimStale(alive func(lease) bool) (int, error)
state:    the runtime passes an Alive predicate into reconciliation
```

The predicate lives in `internal/runtime/recover.go`, is built from
`internal/process`, and is a function rather than an interface because there is
one implementation in production and no polymorphism to model. It also makes
the whole of crash recovery unit-testable without creating a single process.

### 2.3 Growth in existing packages

| Package | Addition | Why it belongs there |
|---|---|---|
| `internal/process` | `Handle` — a handle on a process Forge did *not* start: signal it, ask whether it is alive, wait for it to disappear. Plus `Identity` (PID + start time) and `Config.Setsid`, `Config.CgroupFD`. | Everything here is process knowledge. `stop` needs to signal a PID from a different process than the one that forked it; nothing else in the tree may hold `kill(2)`. |
| `internal/namespace` | `Enter` and namespace-path helpers for `setns(2)`. | SSOT §2 already assigns "namespace-entry helpers (`setns`) for `forge exec`" to this package. Stage 6 is when that line stops being aspirational. |
| `internal/cli` | Five user commands, two hidden ones, `-d` on `run`. | Thin, per SSOT §13.6. |
| `internal/runtime` | Seven new files, listed in §2.1. | It is the only orchestrator. |

---

## 3. State directory layout

```
/var/lib/forge/                       ← --state-dir (SSOT §9)
├── containers/                       ← --root, internal/rootfs (Stage 2)
│   └── <id>/
│       └── rootfs/
├── images/                           ← --image-root, internal/image (Stage 5)
│   ├── blobs/sha256/<hex>
│   └── staging/
├── net/
│   └── leases/<ip>                   ← internal/network (Stage 4)
├── state/                            ← internal/state (Stage 6)      NEW
│   └── <id>/
│       ├── container.json            0600  the record
│       ├── .container.json.<rand>    0600  transient, renamed over the above
│       └── lock                      0600  flock target, never read
└── logs/                             ← internal/logs (Stage 6)       NEW
    └── <id>/
        ├── container.log             0600  current
        └── container.log.1           0600  previous, after one rotation
```

Directories are `0700`, files `0600`. The record contains the container's
resolved environment, which routinely holds credentials passed by the user;
`ps` output does not print it, and the file mode is the only thing standing
between it and every user on the host.

**Why `state/` and `logs/` are separate trees rather than files inside
`containers/<id>/`.** Putting them there is tempting — one directory per
container, one thing to delete — and it is wrong twice over. First, it gives
one directory three owners: `rootfs.Store.Remove` would be deleting `state`'s
and `logs`' files, and the single-ownership rule that ADR-0020 was written to
protect would be broken by the layout instead of by an import. Second, and
decisively: **the record must outlive every resource it describes.** It is the
list of things to clean up. A record stored inside the thing it is responsible
for removing is destroyed by the first step of its own cleanup, and a crash
mid-removal leaves an orphan directory nobody can attribute.

Three trees, three owners, and the removal order falls out of it: rootfs first,
logs next, record last.

**`--state-dir` becomes real.** SSOT §9 has always documented it; until now
only `internal/network` used it. Stage 6 makes it the parent of `state/` and
`logs/` as well. Two Forges pointed at different state directories on one host
share nothing except the kernel — and the kernel resources they create are
namespaced by container ID, so distinct IDs keep them apart. This is stated as
a property, not a supported configuration; §10.4 covers what happens when it is
abused.

---

## 4. Metadata schema

ADR-0006 asks: JSON files or an embedded database? This design accepts **one
JSON file per container, replaced atomically**, and the reasoning is the same
as ADR-0021's for blobs and `ipam.go`'s for leases: the failure mode of a
database is a corrupt database, and the failure mode of a directory of small
files is one unreadable file. An educational runtime should be debuggable with
`cat`, and a state store that needs a migration tool to inspect has taken a
dependency the project's whole point argues against. There is no query pattern
here more complex than "list them all".

```jsonc
{
  "schema": 1,

  "id": "7f3c9a1b2d04",
  "status": "running",
  "reason": "",                       // set only when status was inferred

  "created_at":  "2026-08-07T18:22:01.114Z",
  "started_at":  "2026-08-07T18:22:01.702Z",
  "finished_at": null,

  "host": {
    "boot_id": "6f1a…",               // /proc/sys/kernel/random/boot_id
    "supervisor": { "pid": 41118, "start_ticks": 9948213 },
    "init":       { "pid": 41120, "start_ticks": 9948260 }
  },

  "spec": {
    "image":     "alpine:3.20",
    "digest":    "sha256:beef…",      // what was actually run
    "command":   ["/bin/sh", "-c", "while true; do date; sleep 1; done"],
    "env":       ["PATH=/usr/sbin:/usr/bin:/sbin:/bin"],
    "workdir":   "/",
    "hostname":  "7f3c9a1b2d04",
    "detached":  true,
    "remove_on_exit": false
  },

  "resources": [                      // acquisition order — see §10
    { "kind": "logs",    "ref": "/var/lib/forge/logs/7f3c9a1b2d04" },
    { "kind": "rootfs",  "ref": "/var/lib/forge/containers/7f3c9a1b2d04" },
    { "kind": "cgroup",  "ref": "forge/7f3c9a1b2d04" },
    { "kind": "network", "ref": "bridge:10.88.0.3" }
  ],

  "exit": null                        // { "code": 137, "signal": 9 } when known
}
```

Notes on the fields that are not obvious:

- **`schema`** is checked before anything else is interpreted. A record whose
  schema is greater than this build understands is reported as
  `state.ErrSchema` and is never rewritten. A newer Forge's record must not be
  silently downgraded by an older one, and guessing is worse than refusing.
- **`boot_id`** is what makes PIDs meaningful. After a reboot every PID in
  every record is a lie, and a record from a previous boot is unconditionally
  dead regardless of what `/proc` says about its PID today.
- **`start_ticks`** is field 22 of `/proc/<pid>/stat`, the process's start time
  in clock ticks since boot. A PID alone is not an identity: PIDs are reused,
  and a reconciler that trusts a bare PID will one day decide an unrelated
  process is a container and signal it. `(pid, start_ticks, boot_id)` is an
  identity. `internal/network`'s leases check only the PID today; that is a
  known weakness Stage 6 does not inherit, and it is worth a follow-up.
- **`exit`** is `null` for a container whose exit status was never observed —
  a supervisor that was killed alongside its container. `status` is then
  `exited` with `reason` set. A null exit code is honest; a fabricated `255`
  is not.
- **`resources`** is the durable cleanup stack. §10 is about it entirely.
- **`digest`** records what ran, not what was asked for. `alpine:3.20` moves;
  the record must still say which bytes were on disk.

### 4.1 Writing a record

Every write is: create `.container.json.<rand>` in the same directory, write,
`fsync` the file, `rename` over `container.json`, `fsync` the directory.
`rename(2)` within a directory is atomic, so a reader either sees the whole old
record or the whole new one — never a torn one, and never a zero-length file.
The directory `fsync` is what makes the rename survive power loss rather than
merely a crash; without it the file can be durable while the name pointing at it
is not.

Readers take no lock. Writers take an exclusive `flock` on `<id>/lock` for the
whole read-modify-write, which is what makes `Update` safe when `forge stop` and
the supervisor both want the record at the same moment. `flock` is advisory,
process-scoped, released by the kernel on exit — including a killed process —
so a crash cannot leave a container's record permanently locked. That last
property is why `flock` and not a lock file whose existence means "locked".

---

## 5. Container lifecycle states

SSOT §12 already fixes the state set. This design keeps it and adds exactly one
state, which requires an SSOT amendment under §16:

```
created → running → stopped   → removed
              │   ↘ exited    ↗
              └──── stopping ─┘
```

| Status | Meaning | Resources held |
|---|---|---|
| `creating` | The record exists; resources are being acquired. Never observed by a healthy container for more than the length of a pull. | Some prefix of the resource list |
| `created` | Everything is prepared, the init process is cloned and blocked on the payload pipe. Nothing of the user's has run. | All |
| `running` | The payload was written; the container's own binary is executing. | All |
| `stopping` | A stop was requested; the grace period is running. | All |
| `exited` | The init process terminated on its own. | Retained only (§10) |
| `stopped` | The init process was terminated by `forge stop`. | Retained only |
| `removing` | `forge rm` is unwinding the retained frame. | Some suffix of the retained frame |
| *(absent)* | Removed. The record is gone. | None |

Two of these need defending.

**`creating` and `removing` are not decoration; they are the crash markers.**
A record that exists only from `created` onwards cannot describe the window in
which resources are being acquired, which is precisely the window most likely to
be interrupted. Likewise, a crash halfway through `rm` with no `removing`
marker leaves a record that claims to be `exited` while half its resources are
already gone — and the next reconciler would have no way to know that finishing
the job is safe. Both states exist so that a reconciler's decision is a lookup
rather than a guess.

**`removed` is the absence of a record, not a value of `status`.** SSOT §12
offers "state entry marked for GC (or deleted, per `forge rm`)"; this design
takes deletion. A tombstone would need its own garbage collector, and the
question it answers — "did this container once exist?" — is not one Forge is
asked. `removing` is the durable part; `removed` is what you observe.

### 5.1 State-machine diagram

Edges are labelled with **who** performs the transition. That column matters
more than the transition itself: it is the whole of the concurrency design.

```
                 ┌──────────────┐
   run/launcher  │              │
   ────────────▶ │   creating   │
                 │              │
                 └──┬────────┬──┘
       supervisor:  │        │  supervisor: any failure while acquiring
       all acquired │        │  reconciler: supervisor died here
                    ▼        ▼
                 ┌──────────────┐        ┌──────────────────────────┐
                 │   created    │        │  unwind acquired prefix  │
                 └──────┬───────┘        │  in reverse, then ───────┼──▶ exited
      supervisor:       │                └──────────────────────────┘
      payload written   │
                        ▼
                 ┌──────────────┐
        ┌────────│   running    │────────┐
        │        └──────┬───────┘        │
        │               │                │
        │ stop:         │ supervisor:    │ reconciler: supervisor and init
        │ SIGTERM sent  │ init exited    │ both gone, or boot_id stale
        ▼               │                ▼
 ┌──────────────┐       │         ┌──────────────┐
 │   stopping   │       │         │    exited    │  exit = null
 └──────┬───────┘       │         │ reason set   │
        │               │         └──────────────┘
        │ supervisor: init reaped after the signal
        ▼               ▼
 ┌──────────────┐  ┌──────────────┐
 │   stopped    │  │    exited    │   exit = { code, signal }
 └──────┬───────┘  └──────┬───────┘
        │                 │
        └────────┬────────┘
                 │  rm  (or run -rm, or stop -rm)
                 ▼
          ┌──────────────┐
          │   removing   │
          └──────┬───────┘
                 │  rm / reconciler: finish the retained frame
                 ▼
             (no record)
```

Transitions not on this diagram are refused by `Status.CanTransitionTo`, which
is pure and lives in `internal/state` — a state machine is exactly the kind of
logic that should be testable with no filesystem at all. Notably: nothing moves
*out* of a terminal state except into `removing`, so a late-arriving supervisor
write cannot resurrect a container someone already reconciled.

**`stopping` is an SSOT §12 amendment** and needs an ADR (0014). The argument
for it: without a durable "a stop is in flight" marker, a second `forge stop`
arriving during the first one's grace period has no way to tell a container that
is ignoring `SIGTERM` from one that was never signalled, and `ps` cannot tell a
user why a container has been sitting there for eight seconds. Both are
answerable with one field.

---

## 6. Lifecycle: who runs, and when

### 6.1 The supervisor is a role

The single most important structural decision in this stage: **attached and
detached runs execute the same code.** The supervisor is a role, and in an
attached run the `forge run` process itself plays it.

```
attached:   forge run  ─────────────────────────────▶ supervisor role
                                                          │
detached:   forge run -d ──▶ __supervise (re-exec) ──▶ supervisor role
                (launcher)      setsid, stdio → /dev/null
```

This is worth insisting on because the alternative — a detached path that
prepares resources differently from the attached one — doubles the number of
failure windows in §11 and guarantees that the less-used path is the buggier
one. `ps`, `exec`, `stop` and `logs` cannot tell the difference between an
attached and a detached container, and that is a property of the design rather
than an effort.

### 6.2 Lifecycle diagram — `forge run -d`

```
 launcher (forge run -d)                supervisor (__supervise)          container
 ───────────────────────                ──────────────────────────        ─────────
  1 Spec.Validate()          pure, no I/O — a bad flag costs nothing
  2 NewID()
  3 state.Create(creating)   ─── the first thing that touches the host ───
  4 process.New(self, __supervise, Setsid, stdio → /dev/null,
                 ExtraFiles: [specPipe, readyPipe])
  5 Start()  ────────────────▶  6 read Spec from specPipe (ADR-0008 again)
  6 write Spec ──────────────▶
                                 7 resolve + pull image      ── steps 1–4 of
                                                                Stage 5, unchanged
                                 8 record "logs"    → acquire ─┐
                                 9 record "rootfs"  → acquire  │ each intent is
                                10 record "cgroup"  → acquire  │ written BEFORE
                                11 record "network" → acquire ─┘ the resource
                                                                exists (§10.2)
                                12 clone(2) ──────────────────────▶ __init blocks
                                13 state: created, init pid+ticks     on payload
                                14 cgroup.Add(pid) ────────────────▶ (joins cgroup)
                                15 network attach ─────────────────▶ (gets veth)
                                16 write payload ─────────────────▶ configures,
                                                                    mounts, pivots,
                                                                    execve
                                17 state: running
 18 ◀── readyPipe: {"ok"} ─────  18 write readiness
 19 print ID, exit 0
                                20 tee stdout/stderr → logs.Writer
                                21 process.Wait()
                                                          ◀─────── container exits
                                22 state: stopping? → exited/stopped, exit code
                                23 unwind runtime frame: network → cgroup
                                24 state: terminal, finished_at
                                25 if remove_on_exit: the retained frame too
                                26 exit
```

Steps 1–2 create nothing, as in every stage before this one. Step 3 is the
first host mutation, and it is deliberately the record: from that instant
forward, everything Forge does to this host is attributable to an ID that
something on disk knows about.

Step 18 is the launcher's contract. `forge run -d` does not return until the
container is running or has definitively failed, so `image not found`, `no such
file`, and `permission denied` are reported to the user's terminal with a
non-zero exit, exactly as an attached run reports them. A launcher that
returned immediately would have to report those failures through the log file,
which is a much worse place for the commonest errors in the stage.

If the supervisor fails at any point in 7–17, it unwinds what it acquired,
writes a terminal record (or removes it — §10.5), and reports the error on
`readyPipe`. The launcher prints it and exits 1 or 2 per SSOT §9. Nothing is
left behind, and the launcher never owns a resource it did not create.

### 6.3 Attached run

Identical, minus steps 4–6 and 18–19, and with `Stdout`/`Stderr` teed to both
the log file and the caller's terminal rather than to the log file alone.
ADR-0009's rule is preserved: in attached mode stdout belongs to the container
and the ID is reported on stderr via the `container_id` log field.

---

## 7. Exec protocol

`forge exec` is the hardest thing in this stage, and the difficulty is entirely
about mount namespaces.

### 7.1 The constraint

`setns(2)`: *"A process may not be reassociated with a new mount namespace if it
is multithreaded."* The Go runtime starts threads before `main` runs, and
`runtime.LockOSThread` does not help — it pins a goroutine to a thread, it does
not un-multithread the process. There is no point in a Go program's life at
which `setns(CLONE_NEWNS)` succeeds. This is why runc carries a C prelude
(`nsexec`) that runs before the Go runtime initialises.

Forge will not take a cgo dependency for this: SSOT §10 is stdlib-first, and a C
constructor in an educational runtime buys correctness in one command at the
cost of making the whole build harder to read and cross-compile.

`CLONE_NEWPID`, `CLONE_NEWUTS` and `CLONE_NEWNET` have no such restriction; they
are per-task and work fine from a locked OS thread.

### 7.2 The decision

**Join the PID, UTS and network namespaces with `setns`; obtain the container's
filesystem view through `/proc/<init-pid>/root` instead of joining its mount
namespace.** `/proc/<pid>/root` is a magic symlink the kernel resolves in the
target's mount namespace, so it exposes the container's root *including every
mount the container made* — its `/proc`, its `/sys`, its binds. Chrooting a
child to it gives that child the same filesystem the container sees.

What this is not: it is not membership. The exec'd process's own mounts would
land in the host's mount namespace, and mounts the container makes *after* the
exec'd process starts are visible to it (they are the same mount tree seen
through a different root), which is arguably more correct than a snapshot but is
not what `setns` would give. `mount` inside a `forge exec` session is therefore
refused territory — documented, not prevented.

This needs ADR-0015, and the ADR should record the rejected alternatives: a cgo
prelude (rejected: §10), a persistent in-container agent that forks on request
(rejected: it makes `forge-init` PID 1 forever instead of `execve`ing the user's
binary, which changes signal delivery and exit-status propagation for every
stage before this one), and simply not shipping `exec` (rejected: FR-6.2).

### 7.3 The protocol

```
 forge exec (CLI)                     __exec helper                 the container
 ────────────────                     ─────────────                 ─────────────
  1 state.Get(id) → status, init pid+ticks, env, workdir
  2 refuse unless status == running
  3 process.Handle.Open(pid, identity)     ← proves the PID is still that init
  4 open /proc/<pid>/ns/{pid,uts,net}      ← held open; the fds pin the
                                             namespaces even if init exits
  5 open cgroup dir fd (forge/<id>)
  6 process.New(self, __exec,
        ExtraFiles: [payloadPipe, nsFDs…, cgroupFD],
        Stdin/out/err: the caller's)
  7 Start() ─────────────────────────▶  8 read payload (ADR-0008, third use)
  7 write payload ───────────────────▶
                                        9 runtime.LockOSThread()
                                       10 setns(uts), setns(net), setns(pid)
                                       11 process.New(cmd,
                                              Chroot: /proc/<pid>/root,
                                              CgroupFD: <container cgroup>,
                                              Dir: workdir)
                                       12 Start() from the locked goroutine ──▶ born
                                                                                in the
                                                                                container
                                       13 Wait() ◀───────────────────────────── exits
 14 ◀── exit status ──────────────────  14 exit with the same status
 15 exit with the same status
```

Five details carry the weight:

- **Step 3 before step 4.** Opening the namespace fds without first checking
  `(pid, start_ticks)` against the record is how you `setns` into some unrelated
  process's namespaces after a PID reuse. The identity check is the entire
  defence, and it must precede the `open`.
- **`setns(CLONE_NEWPID)` affects children, not the caller.** The helper stays
  in the host's PID namespace forever; the *forked* command is what lands in the
  container's. This is why there must be a fork at step 12 rather than a plain
  `execve` — and it means the helper is well placed to wait for and report the
  exit status, which is what the user wants anyway.
- **Step 12 must run on the locked goroutine.** The forked child inherits the
  namespaces of the *thread* that forked it. A `cmd.Start()` from another
  goroutine lands the command in the host's namespaces, silently and
  intermittently. This is the single most dangerous line in the stage and
  belongs in a comment saying so.
- **`Chroot` is set on the child, not applied to the helper.** `chroot(2)` acts
  on the `fs_struct`, which every thread of a Go process shares; chrooting the
  helper would blind it to `/proc` and to the state directory. `SysProcAttr.Chroot`
  performs it between fork and exec, in the child only, and stdlib already
  supports it.
- **`CgroupFD` (`clone3` + `CLONE_INTO_CGROUP`, kernel 5.7+, and Forge targets
  5.10+) puts the command in the container's cgroup at birth.** The alternative
  — adding the *helper* to the cgroup and letting the fork inherit — is simpler
  and needs no new field, but it charges a Go runtime's RSS against the
  container's `memory.max`, where it can trigger an OOM kill that destroys the
  container the user was only trying to look at. Writing the child's PID to
  `cgroup.procs` after `Start` is the third option and is a race: the command is
  already running unbounded. `CLONE_INTO_CGROUP` is the only choice with no
  window.

Exec sessions are not recorded. If the helper is killed, the command it started
is reparented to host PID 1 and continues inside the container until the
container dies; `forge stop` kills it along with everything else, because
killing a PID namespace's init makes the kernel kill every member.

---

## 8. Stop protocol

```
forge stop [-t seconds] [-rm] <id>
```

```
 1  state.Get(id)
 2  if terminal      → success, no-op                    ← idempotent (FR-6.6)
 3  if creating      → refuse: ErrNotRunning, "still starting"
 4  Handle.Open(init pid, identity)
       └─ identity mismatch or boot_id stale → the container is gone:
          reconcile (§11) and return success
 5  state.Update: running → stopping, deadline recorded
 6  Handle.Signal(SIGTERM)
 7  wait for the record to become terminal, up to -t (default 10s)
 8  on timeout: Handle.Signal(SIGKILL)
 9  wait for the record to become terminal, up to a fixed 5s grace
10  if it never does and the supervisor is gone → reconcile: unwind from the
    record's resource list, write stopped with exit = null
11  if -rm: proceed to §9.4
```

**Why the wait is on the record and not on the process.** `forge stop` is not
the container's parent and cannot `wait(2)` it. It could watch the PID
disappear — and it does, via `pidfd_open(2)`, which is exact and immune to PID
reuse in a way `kill(pid, 0)` polling is not — but the PID disappearing tells it
only *that* the container died, never *how*. The exit code exists in exactly one
process, the supervisor, and reaches the user through exactly one place, the
record. So the pidfd is the fast signal that something changed and the record is
the authority on what.

**The `SIGTERM` reality.** As §1 sets out, a container's PID 1 does not receive
`SIGTERM` from outside its namespace unless it installed a handler. For
`/bin/sh`, `sleep`, and most single-binary images, `forge stop` will burn the
full grace period and then `SIGKILL`. The right response is not to shorten the
default but to say so: `forge stop` reports `stopped (killed after 10s)` on
stderr when the grace period expires, so the user learns the mechanism from the
tool rather than from a mailing list. Applications that want a graceful stop
must handle `SIGTERM`, and that is the contract everywhere, not a Forge quirk.

**Killing init kills the namespace.** When a PID namespace's init dies, the
kernel sends `SIGKILL` to every remaining process in that namespace. One signal
tears down the whole container, including any `forge exec` sessions. Nothing
walks a process list.

---

## 9. Log persistence

### 9.1 Format

One JSON object per line, one line per write the container performed:

```json
{"t":"2026-08-07T18:22:03.114233Z","s":"stdout","m":"Wed Aug  7 18:22:03 UTC 2026\n"}
{"t":"2026-08-07T18:22:03.115901Z","s":"stderr","m":"sh: nope: not found\n"}
```

Rejected: two raw files, `stdout` and `stderr`. Raw files are simpler to write
and simpler to `cat`, and they cannot represent the one thing a user actually
needs — the true interleaving of the two streams. They also make `-t`
(timestamps), `--since` and `--tail` unimplementable without re-reading the
whole file and guessing. JSON Lines costs one `encoding/json` call per write and
answers all of it. ADR-0016.

Framing is per `read(2)`, not per line: what the writer receives from the pipe is
whatever the kernel handed it, and a container that writes half a line and pauses
should not have its output withheld. `forge logs` concatenates `m` verbatim, so
the user sees exactly the byte stream the container produced; the framing is
only visible when `-t` is asked for.

### 9.2 Rotation

Fixed policy: `container.log` is capped at **10 MiB**; on crossing it, it is
renamed to `container.log.1` (replacing any previous `.1`) and a new file is
started. Maximum on-disk cost per container is therefore ~20 MiB, bounded by
construction rather than by hope. A reader that wants the whole log reads `.1`
then the current file; a rotation during a follow is detected by the reader's
`(dev, ino)` changing and is handled by reopening.

Unbounded logs are a resource leak, and a leak that fills the root filesystem
takes the host down with it. Making the cap configurable is deferred, not
forgotten: one number that nobody can get wrong beats a flag that is wrong on
one machine.

### 9.3 Following

`forge logs -f` polls for growth on a short interval rather than using
`inotify`. `golang.org/x/sys/unix` is already a dependency and offers
`InotifyInit1`, so this is a genuine choice: polling a single file descriptor at
100 ms costs nothing measurable, handles rotation without a second watch, and
avoids the queue-overflow handling that makes inotify code long. If following
ever needs to be exact rather than prompt, inotify is a drop-in change behind
the same reader.

**When following stops** is a boundary question. `internal/logs` cannot answer
it — that would mean reading container state, which would make `logs` import
`state`. So the reader takes a `Done <-chan struct{}` from the caller, and
`internal/runtime` closes it when the record goes terminal *and* the reader has
reached EOF. Both conditions are required: a terminal record with unread bytes
still has output the user asked for.

### 9.4 Who writes

The supervisor, and only the supervisor. It owns the read ends of the
container's stdout and stderr pipes for the container's whole life, and it is the
only process that ever opens the log file for writing — so there is no
concurrent-append problem to solve and no locking in `internal/logs` at all.
`forge exec` output goes to the caller's terminal and is never logged, matching
Docker and matching the record's scope: a record describes a container.

A failure to write a log line does **not** kill the container. It is logged at
`WARN` (SSOT §13.7) and the write is dropped. A full disk should degrade
observability, not terminate workloads.

---

## 10. Cleanup semantics

### 10.1 The stack splits into two frames

Stages 1–5 acquire and release everything within one function call. Stage 6
cannot: `forge ps -a` and `forge logs` on a stopped container are the whole
point of FR-6.1 and FR-6.4, and both require that something survives the
container's death.

So the stack splits by *lifetime*, not by kind:

| Frame | Contents | Released by | Why |
|---|---|---|---|
| **Runtime frame** | the process, the veth + lease + NAT, the cgroup | the supervisor, as the container exits | These are live kernel resources. A dead container holding an IP address and a cgroup is a leak with a name. |
| **Retained frame** | the rootfs directory, the log files, the record | `forge rm` | These are the corpse: what `ps -a` lists, what `logs` reads, what a post-mortem needs. |

And here is the property that matters: **splitting the stack does not break
reverse order.** Acquisition order is

```
  record → logs → rootfs → cgroup → network → process
     1       2       3        4        5         6
```

The runtime frame is 6, 5, 4 — the *last* acquired — and it unwinds first. The
retained frame is 3, 2, 1 — the *first* acquired — and it unwinds last. Global
release order is 6→5→4→3→2→1, exactly reverse acquisition, with an arbitrary
amount of wall-clock time in the middle. The invariant SSOT §11.3 states is
preserved literally, not merely in spirit, and the ordering rationale is
unchanged from Stage 5's: the network goes first because the container may still
hold an interface plugged into the bridge, the cgroup next because a cgroup with
a process in it cannot be removed, the filesystem after those because it is what
a running container has open, and the record last because it is the list of what
to remove.

### 10.2 The durable stack is written *ahead* of acquisition

The in-memory rule is "register the cleanup the moment the resource exists, and
never later." The on-disk rule inverts it: **write the intent before acquiring
the resource.**

The asymmetry is forced by where a crash can land. In memory, registration and
acquisition are adjacent instructions and a crash between them is a crash of the
whole process, which loses the stack anyway. On disk they are separated by an
`fsync`, and the two orderings fail differently:

- Record *after* acquiring: a crash in between leaves a resource nothing knows
  about. Unrecoverable — that is a leak with no name to clean up under.
- Record *before* acquiring: a crash in between leaves a record of a resource
  that does not exist. Recoverable and free, because **every `Destroy` is
  idempotent** (SSOT §13.3) and releasing something that was never created is
  precisely the no-op that contract promises.

Over-recording costs a no-op. Under-recording costs a leak. The invariant that
already exists is what makes the safe direction free, which is a pleasing
argument for having insisted on it since Stage 1.

The cost is one `fsync` per resource — four or five per container start, on the
slow path of an operation that already pulls an image over a network. That is a
price worth naming and paying.

### 10.3 Unwinding a record

Reconciliation walks `resources` in reverse and dispatches on `kind`:

| `kind` | Released by | Idempotent because |
|---|---|---|
| `network` | `network.Manager.Destroy(id)` | Stage 4's teardown already tolerates a missing veth and a missing lease |
| `cgroup` | `cgroup.Manager.Destroy(id)` | Stage 3's `destroy` already tolerates a missing directory |
| `rootfs` | `mount.Cleanup` then `rootfs.Store.Remove(id)` | Stage 2's remove already tolerates a missing tree |
| `logs` | `logs.Store.Remove(id)` | `os.RemoveAll` semantics |

The dispatch table lives in `internal/runtime` — it is precisely the
cross-package sequencing knowledge SSOT §13.2 says belongs there, and it is the
reason `state` can store a `kind` string without knowing what any of them mean.
An unknown `kind` (a record written by a newer Forge) is logged at `WARN` and
skipped rather than treated as an error: the rest of the unwind is still worth
doing.

### 10.4 Removal

`forge rm <id>`:

```
1  state.Get(id)
2  if not terminal:
      without -f → ErrRunning, exit 1
      with    -f → run §8 first, then continue
3  state.Update → removing
4  unwind the retained frame in reverse: rootfs → logs
5  state.Delete(id)          ← the record, last
```

`state.Delete` removing the record last is what makes step 4 restartable: a
crash anywhere in 3–4 leaves a `removing` record, and the next command to
reconcile finishes it. Deleting the record first would strand the rootfs
forever.

### 10.5 What an attached run leaves behind

Stages 1–5 remove everything on exit, and PRD §10.4 ("no manual cleanup is
required after running the test suite") is written against that behaviour.
Docker's default is the opposite: containers persist until `docker rm`.

The decision is to keep both, split by mode:

- **Attached `forge run` removes on exit by default** (`-rm=false` to keep it).
  Its output already went to the user's terminal, so the log file has no second
  reader, and existing behaviour, existing integration tests, and PRD §10.4 are
  all preserved unchanged.
- **Detached `forge run -d` retains until `forge rm`** (`-rm` to auto-remove).
  Its output exists *only* in the log file, and a container whose record
  vanished the moment it exited would make `forge logs` useless for exactly the
  containers it was written for.

Asymmetric defaults are usually a smell. This one is defensible in a sentence —
*retain output that has nowhere else to go* — and the alternative is either
breaking Stage 1–5 behaviour or shipping a `logs` command that cannot see the
containers most likely to need it. ADR-0017.

---

## 11. Failure analysis

Every window in which a Stage 6 operation can be interrupted, what detects it,
and what is guaranteed afterwards. "Reconcile" refers to §12.

| # | Failure | Detected by | Response | Guarantee |
|---|---|---|---|---|
| 1 | `state.Create` fails (disk full, permissions) | Launcher | Return the error; nothing was created | Host bit-for-bit unchanged |
| 2 | Supervisor fails to start after the record is written | Launcher: `Start` returns | Delete the record; report | No orphan record |
| 3 | Launcher is killed after spawning the supervisor | Nobody — and nothing is wrong | Supervisor is `setsid`, survives, keeps running; the ID was already printed or is discoverable via `ps` | Container is not orphaned; the record names it |
| 4 | Image pull fails | Supervisor | Only `logs` and the record exist; unwind both; report on `readyPipe` | Nothing on disk but the failure message |
| 5 | Unpack fails halfway (full disk, bad layer) | Supervisor | `rootfs` intent was written first (§10.2); unwind removes the partial tree | No partial rootfs |
| 6 | `cgroup.Create` fails | Supervisor | Unwind rootfs → logs → record | Stage 3 behaviour, now durable |
| 7 | `network.Allocate` fails (pool exhausted) | Supervisor | Unwind cgroup → rootfs → logs → record | No lease held |
| 8 | `clone(2)` fails (EPERM) | Supervisor | Full unwind; `namespace.ErrPermission` reaches the user's terminal via `readyPipe` | Unchanged from Stage 1 |
| 9 | `cgroup.Add` or veth attach fails after clone | Supervisor | `abandon`: SIGKILL + reap the blocked init, then full unwind | No orphan process — the child is still blocked on the payload pipe and has run no user code |
| 10 | Supervisor is `SIGKILL`ed while the container runs | Any later command: record says `running`, supervisor identity is dead, init identity is alive | Container is `orphaned`; it keeps running but its exit code can never be collected. `ps` shows `running (orphaned)`. `stop` works (SIGKILL), and records `exit: null` | Never a false exit code |
| 11 | Supervisor and container both killed (e.g. OOM killer takes the cgroup) | Reconcile: both identities dead | Unwind the runtime frame from the record; `exited`, `exit: null`, `reason` set | No leaked veth, lease, or cgroup |
| 12 | Host reboots with containers running | Reconcile: `boot_id` mismatch | All records from a prior boot are terminal by definition; PIDs are never consulted. Unwind the runtime frame (cgroups and veths are already gone; the calls are no-ops) and mark `exited` | Reboot cannot resurrect a PID |
| 13 | PID reuse: the recorded init PID is now an unrelated process | `start_ticks` mismatch in `Handle.Open` | Treated as dead; never signalled | Forge never signals a process it does not own |
| 14 | `forge stop` while the container is exiting on its own | `state.Update` refuses `exited → stopping` | Stop returns success, reporting the actual terminal state | No spurious error, no double unwind |
| 15 | Two `forge stop`s concurrently | `flock` on the record | Second sees `stopping`, joins the wait rather than re-signalling | One grace period, not two |
| 16 | `forge rm` concurrent with `forge stop` | `flock`; `rm` requires terminal | `rm` fails with `ErrRunning` unless `-f` | Resources are never released underneath a live container |
| 17 | Crash during `rm` | Reconcile: `removing` record | Finish the retained frame, delete the record | Restartable, no orphan tree |
| 18 | `exec` into a container that exits mid-setup | `Handle.Open` identity check, and the held `/proc/<pid>/ns/*` fds | The fds pin the namespaces; the command either starts in a doomed namespace and is killed with it, or fails cleanly. Never lands in the host's namespaces | The dangerous outcome is impossible |
| 19 | `exec` helper is killed | Its child is reparented to host PID 1 | The command keeps running inside the container; `stop` reaps it via the PID-namespace kill | No host-side residue |
| 20 | Log write fails (disk full) | Supervisor | `WARN`, drop the line, container keeps running | Observability degrades; workloads do not |
| 21 | Record is corrupt or unparseable | Any reader | `ps` lists it as `unknown` and does not hide it; `rm` can still delete it; nothing else touches it | One bad file does not break the store |
| 22 | Record has a newer `schema` | `state.Get` | `ErrSchema`; refuse to read, refuse to write, list as `unknown` | An old Forge never corrupts a new Forge's state |
| 23 | Two Forges, one `--state-dir`, same container ID | ID collision (2⁻⁴⁸, ADR-0005) | `state.Create` uses `O_EXCL` and fails | Two containers can never share a record |
| 24 | Two Forges, different `--state-dir`, same host | Not detected | They share the bridge and the subnet but not the IPAM leases, so addresses can be double-allocated | Stated as unsupported; a single state directory per host is the contract |

The pattern across rows 4–8 is the one Stage 5 established and Stage 6 keeps:
the operations that create nothing happen first, and every subsequent failure
unwinds through a stack that already knows about everything created so far —
except that the stack is now on disk, so "unwinds" can mean "a different process
unwinds, minutes later."

---

## 12. Crash recovery

There is no daemon, so there is no startup to hook. Instead, **every command
that reads state reconciles first**, over the records it is about to look at:
`ps` reconciles all, `stop`/`exec`/`logs`/`rm` reconcile one, `run` reconciles
all before allocating an address (so a crashed container's lease is back in the
pool before the new container asks for one).

For each record, reconciliation asks three questions in order:

```
1  Is boot_id the current boot?
      no  → the container is dead, and every PID in the record is meaningless.
            Unwind the runtime frame; status = exited, exit = null,
            reason = "host rebooted".

2  Is the supervisor alive?  (pid + start_ticks)
      yes → it owns the container. Do nothing at all. The supervisor is the
            only writer for a live container, and a reconciler that "helped"
            here would be racing it.
      no  → question 3.

3  Is the init process alive?  (pid + start_ticks)
      yes → orphaned. Leave the resources alone — they are in use — and mark
            the record so ps can say so. Only stop may act on it.
      no  → dead. Unwind the runtime frame in reverse from the resource list;
            status = exited (or stopped if it was stopping), exit = null,
            reason = "supervisor lost".
```

Plus two record-shape cases: `creating` with a dead supervisor is unwound
completely, record and all — nothing was ever running, so there is nothing to
show a user; and `removing` is finished, per §10.4.

Three properties are what make this safe to run from five different commands at
once:

- **Idempotent.** Every step is a `Destroy` that tolerates absence (§10.3), so
  running reconciliation twice on the same record is running it once.
- **Serialised per container.** The `flock` in `state.Update` means two
  reconcilers on one record take turns, and the second finds a terminal record
  and does nothing.
- **Best-effort.** Reconciliation failures are logged at `WARN` and never fail
  the command that triggered them. `forge logs` must work on a container whose
  cgroup happens to be un-removable. NFR-5 asks for exactly this.

Reconciliation is also where the design is most testable without root: the
liveness predicate is a parameter (§2.2), so the entire decision table above is
exercised by a table test with a fake `Alive` function, a temp directory, and no
processes at all.

---

## 13. API proposal

Declarations only, with the properties that constrain them. No bodies, no
implementation.

### 13.1 `internal/state`

```go
// Pure: schema, states, validation. No I/O.
type Status string

const (
    StatusCreating Status = "creating"
    StatusCreated  Status = "created"
    StatusRunning  Status = "running"
    StatusStopping Status = "stopping"
    StatusExited   Status = "exited"
    StatusStopped  Status = "stopped"
    StatusRemoving Status = "removing"
)

func (s Status) Terminal() bool
func (s Status) Valid() bool
func (s Status) CanTransitionTo(next Status) error   // the §5.1 machine, pure

type Kind string   // "logs" | "rootfs" | "cgroup" | "network"

type Resource struct {
    Kind Kind   `json:"kind"`
    Ref  string `json:"ref"`
}

type Identity struct {                       // (pid, start time) — see §4
    PID        int    `json:"pid"`
    StartTicks uint64 `json:"start_ticks"`
}

type Exit struct {
    Code   int `json:"code"`
    Signal int `json:"signal"`
}

type Record struct {
    Schema     int        `json:"schema"`
    ID         string     `json:"id"`
    Status     Status     `json:"status"`
    Reason     string     `json:"reason,omitempty"`
    CreatedAt  time.Time  `json:"created_at"`
    StartedAt  *time.Time `json:"started_at,omitempty"`
    FinishedAt *time.Time `json:"finished_at,omitempty"`
    BootID     string     `json:"boot_id"`
    Supervisor Identity   `json:"supervisor"`
    Init       Identity   `json:"init"`
    Spec       Spec       `json:"spec"`       // state's own Spec: strings and ints
    Resources  []Resource `json:"resources"`  // acquisition order
    Exit       *Exit      `json:"exit,omitempty"`
}

func (r Record) Validate() error   // pure

// Store. New performs no I/O (SSOT §13, and the Stage 5 precedent in
// image.NewCache): it validates that dir is absolute and returns.
type Store struct{ /* … */ }

func New(dir string) (*Store, error)

func (s *Store) Create(rec Record) error                        // O_EXCL
func (s *Store) Get(id string) (Record, error)
func (s *Store) List() ([]Record, []error)                      // partial results + per-record errors
func (s *Store) Update(id string, fn func(*Record) error) error // flock, RMW, atomic replace
func (s *Store) Delete(id string) error                         // idempotent
func (s *Store) Dir(id string) (string, error)

var (
    ErrNotFound   = errors.New("no such container")
    ErrExists     = errors.New("container already exists")
    ErrInvalidID  = errors.New("invalid container id")
    ErrSchema     = errors.New("unsupported state schema")
    ErrTransition = errors.New("invalid state transition")
)
```

`List` returning `([]Record, []error)` rather than `([]Record, error)` is
deliberate: one corrupt record must not hide the other nine from `forge ps`
(failure 21). The caller prints what it got and warns about what it did not.

`Update` taking a mutator rather than a whole `Record` is what makes the lock
correct by construction — there is no way to express "read, mutate, write" that
skips the lock, and no way for a caller to write back a record it read a minute
ago.

### 13.2 `internal/logs`

```go
type Stream string

const (
    Stdout Stream = "stdout"
    Stderr Stream = "stderr"
)

type Entry struct {
    Time    time.Time `json:"t"`
    Stream  Stream    `json:"s"`
    Message []byte    `json:"m"`
}

type Store struct{ /* … */ }

func New(dir string) (*Store, error)   // no I/O

// Writing: one writer per container, held by the supervisor for its lifetime.
func (s *Store) Open(id string) (*Writer, error)   // creates the directory lazily

type Writer struct{ /* … */ }

func (w *Writer) Stream(st Stream) io.Writer   // an adapter to hand to process.Config
func (w *Writer) Close() error

// Reading.
type ReadOptions struct {
    Tail   int                // last N entries; 0 means all
    Since  time.Time          // zero means from the beginning
    Follow bool
    Done   <-chan struct{}    // closed by the caller to end a follow (§9.3)
}

func (s *Store) Read(id string, opts ReadOptions) (*Reader, error)

type Reader struct{ /* … */ }

func (r *Reader) Next(ctx context.Context) (Entry, error)   // io.EOF when done
func (r *Reader) Close() error

func (s *Store) Remove(id string) error   // idempotent

const (
    MaxLogBytes  = 10 << 20
    RotatedFiles = 1
)
```

`Writer.Stream` returning an `io.Writer` is what lets the supervisor hand
`logs` straight to `process.Config.Stdout` for a detached container, or to an
`io.MultiWriter` alongside the terminal for an attached one, with no branch in
`logs` at all.

### 13.3 `internal/process` — additions

```go
// Identity distinguishes a PID from the process that currently holds it.
type Identity struct {
    PID        int
    StartTicks uint64   // /proc/<pid>/stat field 22
}

func Identify(pid int) (Identity, error)

// Handle is a handle on a process Forge did not start, and therefore cannot
// reap. It can be signalled and watched, never waited on for a status.
type Handle struct{ /* … */ }

func Open(id Identity) (*Handle, error)   // verifies the identity, opens a pidfd

func (h *Handle) Signal(sig syscall.Signal) error
func (h *Handle) Alive() bool
func (h *Handle) WaitExit(ctx context.Context) error   // pidfd poll; no status
func (h *Handle) Close() error

// Config additions.
type Config struct {
    // … Stage 1 fields unchanged …

    // Setsid detaches the child into a new session, so it survives the death
    // of the process that started it and of the terminal it was started from.
    Setsid bool

    // CgroupFD is a directory the kernel places the child in at birth, via
    // clone3(CLONE_INTO_CGROUP). Nil starts the child in the caller's.
    // This package does not know what the directory means.
    CgroupFD *os.File

    // Chroot is applied in the child, between fork and exec.
    Chroot string
}
```

`Handle` deliberately cannot return an exit status. Encoding "only the parent
can reap" in the type is worth more than a convenience method that would have to
lie.

### 13.4 `internal/namespace` — additions

```go
// Paths returns the /proc/<pid>/ns/* paths for the namespaces c selects.
func Paths(pid int, c Config) []string   // pure

// Enter associates the calling thread with the namespaces behind fds.
//
// The caller must have locked its OS thread, and must fork from that same
// thread: the namespaces of a forked child come from the thread that forked
// it. Mount namespaces are not supported — setns(2) refuses CLONE_NEWNS from a
// multithreaded caller, and the Go runtime is always multithreaded (§7.1).
func Enter(fds []*os.File) error

var ErrMountNamespaceEntry = errors.New("cannot enter a mount namespace from a multithreaded process")
```

### 13.5 `internal/runtime` — additions

```go
// Spec additions.
type Spec struct {
    // … Stage 1–5 fields unchanged …
    Detach       bool   // run in the background under a supervisor
    RemoveOnExit bool   // defaults per §10.5: true attached, false detached
}

// Container is the runtime's view of a container: what ps prints. It is
// translated from state.Record so that internal/cli never sees the on-disk
// schema (SSOT §13.6).
type Container struct {
    ID        string
    Image     string
    Command   []string
    Status    string
    Orphaned  bool
    CreatedAt time.Time
    ExitCode  *int
    IP        string
}

func (r *Runner) List(ctx context.Context, all bool) ([]Container, []error)
func (r *Runner) Stop(ctx context.Context, id string, opts StopOptions) error
func (r *Runner) Exec(ctx context.Context, spec ExecSpec) (process.Status, error)
func (r *Runner) Logs(ctx context.Context, id string, opts LogOptions, w io.Writer) error
func (r *Runner) Remove(ctx context.Context, id string, opts RemoveOptions) error
func (r *Runner) Reconcile(ctx context.Context) []error

type StopOptions struct {
    Timeout time.Duration   // zero means DefaultStopTimeout
    Remove  bool
}

type RemoveOptions struct{ Force bool }

type LogOptions struct {
    Follow     bool
    Tail       int
    Since      time.Time
    Timestamps bool
}

type ExecSpec struct {
    ID         string
    Command    []string
    Env        []string   // appended to the container's recorded environment
    WorkingDir string     // empty means the container's
    Stdin      io.Reader
    Stdout     io.Writer
    Stderr     io.Writer
}

// Hidden entry points, dispatched from cmd/forge exactly as Init already is.
const (
    InitCommandName      = "__init"        // Stage 1, unchanged
    SuperviseCommandName = "__supervise"
    ExecCommandName      = "__exec"
)

func Supervise(ctx context.Context) error   // reads its Spec from fd 3
func ExecHelper(ctx context.Context) error  // reads its payload from fd 3

var (
    ErrNotFound   = errors.New("no such container")
    ErrNotRunning = errors.New("container is not running")
    ErrRunning    = errors.New("container is running")
    ErrOrphaned   = errors.New("container has no supervisor")
)

const (
    DefaultStopTimeout = 10 * time.Second
    KillGrace          = 5 * time.Second
)
```

### 13.6 `internal/cli`

Five user commands and two hidden ones. Per SSOT §13.6 they parse, call one
runtime method, and format:

```
forge run [-d] [-rm] …                → Runner.Run
forge ps [-a] [-q]                    → Runner.List
forge exec <id> <cmd> [args…]         → Runner.Exec
forge stop [-t seconds] [-rm] <id>    → Runner.Stop
forge logs [-f] [-n N] [-t] <id>      → Runner.Logs
forge rm [-f] <id>…                   → Runner.Remove
forge __supervise                     → runtime.Supervise    (hidden)
forge __exec                          → runtime.ExecHelper   (hidden)
```

Exit codes follow SSOT §9: `1` user error (unknown container, container
running), `2` internal. `forge exec` follows ADR-0009's `run` exception and
propagates the exec'd command's own status, because the alternative is a
command whose exit code cannot be scripted against. ID prefix matching (`forge
stop 7f3c`) resolves in `internal/cli` against `Runner.List`, and an ambiguous
prefix is a user error.

---

## 14. ADRs and SSOT amendments

| ADR | Title | Status after this design |
|---|---|---|
| 0006 | State store format (JSON files vs. embedded DB) | **Accepted** — one JSON file per container, atomically replaced (§4) |
| 0014 | The `stopping` state, and who may write a transition | Proposed (§5) |
| 0015 | `forge exec` enters PID/UTS/net namespaces and chroots to `/proc/<pid>/root` | Proposed (§7) |
| 0016 | Container logs are JSON Lines with per-write framing | Proposed (§9) |
| 0017 | What `stop` releases and what `rm` releases | Proposed (§10) |

SSOT changes required in the same PR (§16):

- **§2** — add `internal/logs`; extend `internal/state`'s row with "never
  performs liveness checks; takes them as a parameter"; extend
  `internal/process` with foreign-process handles.
- **§9** — `run -d`, `run -rm`, `stop -rm`, `rm -f`, `logs -n/-t`, `ps -q`.
- **§11.2** — replace the `forge stop` sketch with §8 of this document. The
  sketch has stop performing the teardown; in this design the supervisor does,
  and stop waits for the record. The difference is not cosmetic: only the
  parent can reap.
- **§12** — add `creating`, `stopping`, `removing`; state that `removed` is the
  absence of a record.
- **§13** — no invariant changes. All six hold as written, which is the main
  claim this design makes.

---

## 15. Testing strategy

The design's testability is a property of two decisions: liveness is a
parameter, and the state machine is pure.

**Unit, no root:** the §5.1 transition table; record round-trips including a
newer `schema`, a truncated file, and a corrupt one; atomic replacement under a
concurrent reader; `flock` under two writers; the whole of §12's decision table
against a fake `Alive`; JSONL framing, rotation, tail, and a follow that ends
when `Done` closes; the `rm` restart from a `removing` record.

**Integration, root, `//go:build integration`:** `run -d` → `ps` → `logs -f` →
`exec` → `stop` → `rm`; `SIGKILL` the supervisor and assert the next command
reconciles and leaks nothing (`ip link`, the lease directory, and the cgroup
tree all checked); `exec` into a container that is killed mid-exec; `stop` on a
container that ignores `SIGTERM`, asserting the grace period and the reported
reason; a simulated stale `boot_id`; and PRD §10.4's standing requirement that
the suite leaves no residue.

**Definition of done** is PRD §10: FR-6.1 through FR-6.6 implemented and
covered, `make test` green from a clean VM, README examples reproducible, no
manual cleanup, SSOT updated.

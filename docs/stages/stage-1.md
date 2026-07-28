# Stage 1 — Process Isolation

**Status:** Complete
**Requirements:** PRD §8.1 (FR-1.1 … FR-1.5)
**New packages:** `internal/namespace`, `internal/process`, `internal/runtime`
**ADRs:** 0005, 0007, 0008, 0009

---

## What Stage 1 does

`forge run <path> [args...]` executes a binary on the host filesystem as PID 1
of a new process tree, with its own hostname and its own mount table, and
reports how it exited.

There is no image, no root filesystem switch, no resource limit and no network
of its own — those are Stages 2 through 5. The container shares the host's
filesystem and network; only its process tree, hostname and mount table are
isolated.

## How it fits together

```
cmd/forge  ──▶ internal/cli ──▶ internal/runtime ──┬──▶ internal/namespace
 (wiring)      (flags, exit          (conductor)   └──▶ internal/process
                 codes)
```

Starting a container:

```
runtime.Run
  ├── NewID()                        12 hex chars (ADR-0005)
  ├── namespace.Config{PID,UTS,Mount, Hostname: id}
  ├── os.Pipe()                      the init payload channel
  ├── process.New/Start              clone(2) with the computed flags
  │     └── child: /proc/self/exe __init          (ADR-0008)
  │           ├── decode payload from fd 3
  │           ├── namespace.Apply
  │           │     ├── mount(none, /, MS_REC|MS_PRIVATE)   → FR-1.3
  │           │     └── sethostname(hostname)               → FR-1.2
  │           └── execve(user binary)                       → FR-1.1
  ├── write payload, close pipe
  └── process.Wait                   → FR-1.4, FR-1.5
```

## Why the child-side step exists

Clone flags alone satisfy none of FR-1.2 and only half of FR-1.3.
`CLONE_NEWUTS` copies the host's hostname rather than assigning a new one, and
`CLONE_NEWNS` copies the parent's mount table *including its propagation type* —
so on a host where `/` is `MS_SHARED`, which is every systemd host, a mount made
inside the container still propagates out.

Both fixes must run after `clone(2)` and before `execve`, a window in which Go
cannot run arbitrary code. Forge therefore re-executes itself as the container's
init. See ADR-0008, including the fork-bomb guard that re-exec makes necessary.

## Requirement traceability

| Requirement | Implementation | Verified by |
|---|---|---|
| FR-1.1 — new process tree via `CLONE_NEWPID` | `namespace.Config.CloneFlags`, `process.Config.CloneFlags` | `TestContainerInitIsPIDOne`, `TestContainerHasItsOwnPIDNamespace`, `TestConfigCloneFlags` |
| FR-1.2 — isolated hostname via `CLONE_NEWUTS` | `namespace.Apply` → `sethostname(2)` | `TestHostnameIsIsolated`, `TestContainerHasItsOwnUTSNamespace`, `TestDefaultHostnameIsTheContainerID` |
| FR-1.3 — isolated mount table via `CLONE_NEWNS`, no propagation | `namespace.makeMountTreePrivate` → `mount(none, /, MS_REC\|MS_PRIVATE)` | `TestMountsDoNotPropagateToHost`, `TestContainerHasItsOwnMountNamespace` |
| FR-1.4 — lifecycle tracking and exit code | `process.State` (created/running/exited), `process.Status` | `TestLifecycleStates`, `TestExitCodeIsReported` (unit + integration), `TestSignalledProcessReportsSignal` |
| FR-1.5 — cleanup on exit | `process.Wait` reaps; `context.AfterFunc` kills on cancellation; namespaces released by the kernel when the last member exits | `TestCancellingTheContextKillsTheContainer`, `TestNoHostResidueAfterRun`, `TestWaitKillsOnContextCancellation` |

## Known limitations

These are consequences of Stage 1's scope, not defects. Each is resolved by a
later stage or is explicitly out of scope per PRD §5.

- **`/proc` inside the container is the host's.** A new PID namespace does not
  remount `/proc`, so `ps` inside a container still lists host processes even
  though `getpid()` correctly returns 1. Remounting `/proc` requires a root
  filesystem to mount it into — Stage 2.
- **The container shares the host's filesystem and network.** Stages 2 and 4.
- **The container's init is not a real init.** If the container's binary spawns
  children that outlive their parent, nothing reaps them. Docker has the same
  behaviour without `--init`; process supervision is Stage 6.
- **If `forge` is killed with `SIGKILL`, the container survives.** Forge kills
  the container when its context is cancelled, which covers `SIGINT` and
  `SIGTERM`, but nothing runs on `SIGKILL`. Reconciliation on startup needs the
  state store from Stage 6. `Pdeathsig` was considered and rejected: it fires on
  *thread* death, and Go retires OS threads, so it can kill a healthy container
  (golang/go#27505).
- **No user namespace, so `forge` needs real root.** Rootless containers are a
  non-goal for Stages 1–6 (PRD §5).

## Running the tests

```bash
make test                 # unit tests, no root
sudo -E make test-integration   # privileged, exercises the real kernel
```

The integration tests skip with a message rather than failing when run without
root. The integration test binary re-executes itself as a container, so it
dispatches `__init` in its `TestMain` exactly as `cmd/forge` does — see
ADR-0008's note on the re-exec contract.

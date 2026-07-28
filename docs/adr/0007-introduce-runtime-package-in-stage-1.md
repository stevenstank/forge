# 0007. Introduce `internal/runtime` in Stage 1

Date: 2026-07-27
Status: Accepted

## Context

SSOT §2 attributes `internal/runtime` to Stage 6, alongside `internal/state`
and the expanded CLI. PRD §13 lists it the same way.

Stage 1 nevertheless requires orchestration. Satisfying FR-1.1 through FR-1.5
means sequencing two packages: `internal/namespace` computes the clone flags and
applies the in-child setup, and `internal/process` starts and supervises the
resulting process. Something has to hold that sequence.

Every other candidate is forbidden by an invariant:

- Putting it in `internal/cli` violates SSOT §13.6, which says CLI packages
  contain no business logic and that all logic must be independently testable
  without invoking the CLI.
- Having `internal/process` import `internal/namespace` violates SSOT §13.2,
  which says primitive packages never call each other except the documented
  `rootfs → image` edge.
- Leaving the sequence in `cmd/forge` violates SSOT §3, which restricts `main`
  to wiring.

SSOT §13.2 also states positively that `internal/runtime` *is* the only
orchestrator. So the stage table says the orchestrator does not exist yet, while
the invariants say all orchestration must live in it. Stage 1 cannot satisfy
both.

## Decision

Introduce `internal/runtime` in Stage 1, containing only what Stage 1 needs: a
`Spec`, a `Runner` that composes `namespace` and `process`, container ID
generation, and the container-side `Init` entry point.

The invariant (§13) outranks the stage attribution (§2). Invariants are
described as rules that "must never be violated ... without an explicit ADR",
whereas the stage table is a plan for when packages first appear.

SSOT §2 is amended in the same change to record `internal/runtime` as
first appearing in Stage 1 and growing through Stage 6.

The package stays within Stage 1's scope. It does not reference `state`,
`cgroup`, `network`, `image`, or `registry`, and adds no configuration fields,
interfaces, or extension points in anticipation of them, per SSOT §14. Stage 3
will add cgroup creation to `Run`; Stage 6 will add the state-store calls and
the create/start/stop/exec split that SSOT §11.1 sketches.

## Consequences

Easier:

- Stage 1 satisfies every invariant in SSOT §13 rather than trading one off
  against another.
- The architecture a reader meets in Stage 1 is the architecture in SSOT §3 —
  the CLI is thin, the primitives are leaves, and one package conducts. Later
  stages extend that shape instead of reorganising it.
- `Runner.Run` is directly testable without the CLI, which is what makes the
  Stage 1 integration tests possible.

Harder:

- `internal/runtime` will be edited in most subsequent stages, so it is the
  file most likely to see merge conflicts between concurrent stage branches.
- There is a standing risk of it becoming a grab bag. The mitigation is SSOT
  §2's prohibition: it must delegate rather than contain syscall logic. Stage 1
  holds to that — `runtime` contains no syscall other than the `SIGKILL` it
  sends through `process`, and the `execve` in `Init`, which is the act of
  becoming the container rather than a primitive worth wrapping.

## Alternatives considered

**Defer orchestration to a Stage 1-only helper, then delete it.** Rejected: it
is the same code under a name that advertises it as temporary, and it would
have to be moved wholesale in Stage 3 anyway.

**Let `cmd/forge` orchestrate until Stage 6.** Rejected: it puts the container
lifecycle somewhere no test can reach without executing a binary, which
conflicts with SSOT §7's requirement that logic be unit-testable.

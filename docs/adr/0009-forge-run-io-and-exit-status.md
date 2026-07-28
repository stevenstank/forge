# 0009. `forge run` output streams and exit status

Date: 2026-07-27
Status: Accepted

## Context

SSOT §9 states two rules that Stage 1's `forge run` cannot both follow:

> All commands that mutate state print the affected container ID to stdout on
> success (scriptable, Docker-like convention) and write human-readable detail
> to stderr.

> Non-zero exit codes are used consistently: `1` for user error (bad flags,
> unknown container), `2` for internal/unexpected error.

Stage 1's `forge run` is attached: it has no `-d` flag, because detaching
requires the state store that arrives in Stage 6. An attached run gives the
container Forge's own stdout.

That breaks the first rule. Printing the container ID to stdout would interleave
Forge's output with the container's, so `forge run /bin/echo hi` would emit

```
a1b2c3d4e5f6
hi
```

corrupting both the scriptable output the rule is meant to protect and the
container's own stream.

It also collides with the second rule. FR-1.4 requires Forge to report the
container's exit code, but §9 assigns 1 and 2 fixed meanings, and a container is
free to exit with either.

Docker resolves both the same way, and Docker's vocabulary is what SSOT §8 says
Forge deliberately mirrors: `docker run` prints the ID to stdout only when
detached, and propagates the container's exit status when attached.

## Decision

For `forge run` in attached mode:

1. **stdout belongs to the container.** Forge writes nothing to it. The
   container ID is reported on stderr, through the structured logger, as the
   `container_id` field that SSOT §6 already requires on every container log
   line.
2. **The container's exit status is Forge's exit status.** A container that
   exits 42 makes `forge run` exit 42. This is carried by `cli.ExitError`, which
   overrides the usage/internal classification.
3. **A container that could not be started exits 127**, per ADR-0008, keeping
   "failed to start" distinguishable from "ran and exited non-zero".
4. Errors originating in Forge rather than the container keep the §9 codes: 1
   for user error, 2 for internal failure.

SSOT §9 is amended in the same change to record this exception.

The ambiguity that remains — a container exiting 1 or 2 looks like a Forge
error — is inherent to propagating child status and is the same tradeoff Docker
makes. It is resolved by the stderr stream, which says which occurred.

When Stage 6 adds `-d`, the detached path takes the §9 rule unchanged: it prints
the container ID to stdout and exits 0, because in that mode stdout is not the
container's.

## Consequences

Easier:

- `forge run /bin/echo hi | cat` produces exactly `hi`, so `forge run` composes
  in a pipeline like any other command.
- `if forge run /bin/false; then` behaves the way a shell author expects.
- Users transferring intuition from Docker are not surprised.

Harder:

- `forge run` is the one mutating command whose stdout does not carry the
  container ID, so scripts wanting the ID must read stderr or wait for Stage 6's
  `-d`. This is why the deviation is recorded rather than left implicit.
- Exit codes 1 and 2 are ambiguous between Forge and the container, as above.
- `cli.ExitError` exists solely to carry an exit code. It is a small type, but
  it is a second error-classification mechanism alongside `ErrUsage`, and future
  commands must be clear about which they mean.

# 0004. CLI flag library choice (stdlib `flag` vs. `cobra`)

Date: 2026-07-27
Status: Accepted

## Context

Forge needs a command-line interface. SSOT §9 fixes the final command surface:
a root command `forge`, three global flags (`--log-level`, `--state-dir`,
`--root`), and six flat subcommands (`run`, `ps`, `exec`, `stop`, `logs`, `rm`)
with a handful of local flags between them (`-a`, `-t`, `-f`).

SSOT §10 lists a CLI framework such as `spf13/cobra` as a pre-approved
*category*, while stating that `flag`/stdlib "may suffice and is preferred if
sufficient". Any actual dependency requires this ADR to answer: what does the
standard library not provide, why is this library the right choice, and what is
its maintenance and security posture.

The decision is needed now because the M0 milestone delivers the CLI skeleton,
and SSOT §15 requires the corresponding ADR to be `Accepted` before that
implementation merges.

## Decision

Use the standard library's `flag` package. Do not add `spf13/cobra` or any
other CLI framework.

Subcommand dispatch is implemented in `internal/cli` as a `Command` slice —
name, summary, and an `Exec` function — with the root-level flags parsed by a
single `flag.FlagSet` and each subcommand owning its own `FlagSet` for its
local flags. Help text is generated from the command registry plus
`FlagSet.PrintDefaults`, so adding a verb in a later stage does not require
touching the help output.

Forge deliberately does not reproduce the parts of cobra it does not need:
there is no command aliasing, no shell-completion generation, no config-file
binding, no persistent pre-run hooks, and no nested command tree.

The one behavioural difference this accepts is flag syntax: `flag` treats
`-root` and `--root` as equivalent and does not support clustered short flags
(`-af`). SSOT §9 writes global flags in the `--flag` form, which `flag`
accepts, so the documented interface is unaffected.

## Consequences

Easier:

- Forge keeps a zero-dependency `go.mod` through M0, which is the strongest
  possible expression of the "standard library first" principle (SSOT §10,
  PRD NFR-4).
- No supply-chain surface for the CLI layer: nothing to audit, pin, or patch.
  A CLI framework is a large transitive dependency tree for a program whose
  entire argument surface fits on one screen.
- The dispatch mechanism is ~40 lines of readable Go that a learner can follow
  end to end, which matches the project's educational purpose (PRD §2). A
  reader does not have to learn cobra's lifecycle to understand how `forge run`
  reaches `internal/runtime`.
- `internal/cli` stays trivially unit-testable: `Run` takes its arguments and
  its output writers as parameters and returns an exit code, so the whole CLI
  is exercised in-process without spawning a subprocess.

Harder:

- Per-command flag parsing, `--` handling, and usage strings are written by
  hand for each verb rather than declared. With six flat commands this is a
  small, bounded cost, but it is a real one and it recurs once per stage.
- No shell completions. This is accepted: Forge is a teaching artifact, and
  completions are not a stated goal.
- If Forge later grows nested subcommands (`forge network create`, say), this
  decision should be revisited with a superseding ADR rather than by growing
  the hand-rolled dispatcher into a framework.

Revisit if the command surface departs from SSOT §9 — in particular if nested
subcommands or shell completions become requirements.

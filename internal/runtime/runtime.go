// Package runtime orchestrates Forge's primitive packages into a container
// lifecycle.
//
// Per SSOT §13.2 it is the only orchestrator: the primitive packages never call
// one another, and every cross-package sequencing decision lives here. Stage 1
// composes exactly two primitives, internal/namespace and internal/process.
//
// # How a container starts
//
// Namespaces are created by clone(2), but two of Stage 1's requirements can
// only be satisfied by code running *inside* the new namespaces: setting the
// container's hostname (FR-1.2) and detaching its mount tree from the host's
// (FR-1.3). Forge therefore starts itself rather than the container's binary:
//
//	forge run          →  clone(CLONE_NEWPID|NEWUTS|NEWNS)
//	                        →  /proc/self/exe __init      (this package, Init)
//	                             →  namespace.Apply
//	                             →  execve(user binary)
//
// The configuration crosses that boundary as JSON on an inherited pipe. See
// ADR-0008.
package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
	"syscall"

	"github.com/stevenstank/forge/internal/namespace"
	"github.com/stevenstank/forge/internal/process"
)

// InitCommandName is the hidden subcommand Forge re-executes itself as, to run
// Init inside the container's new namespaces. The double underscore marks it as
// internal and keeps it clear of the user-facing verbs in SSOT §9.
const InitCommandName = "__init"

// initArgv0 is the argv[0] the init process runs under. It exists only to make
// the process legible in host tooling such as ps.
const initArgv0 = "forge-init"

// Spec describes a container to run. It is the caller's complete statement of
// intent; this package reads no flags, environment, or global state.
type Spec struct {
	// Command is the argument vector to execute inside the container.
	// Command[0] is the path to the binary.
	Command []string

	// Env is the container's complete environment. Nil means an empty
	// environment: nothing is inherited from the host implicitly.
	Env []string

	// Hostname is the container's hostname. Empty means Forge uses the
	// container ID, which is what Docker does.
	Hostname string

	// Stdin, Stdout and Stderr are wired to the container process.
	Stdin  io.Reader
	Stdout io.Writer
	Stderr io.Writer
}

// Validate reports whether the spec can be run. It is pure and performs no
// syscalls, so bad input is rejected before anything is forked.
func (s Spec) Validate() error {
	if len(s.Command) == 0 {
		return ErrNoCommand
	}
	if s.Command[0] == "" {
		return ErrNoCommand
	}
	// Stage 1 runs a binary from the host filesystem and has no image to
	// supply a PATH, so a bare name would be resolved against the *host's*
	// PATH — a surprise worth refusing outright. Path resolution arrives with
	// images in Stage 5.
	if !strings.Contains(s.Command[0], "/") {
		return fmt.Errorf("%w: %q is not a path; forge does not search PATH, give a path such as /bin/%s",
			ErrNotAPath, s.Command[0], s.Command[0])
	}
	return nil
}

// Sentinel errors callers may branch on.
var (
	// ErrNoCommand reports a Spec with nothing to execute.
	ErrNoCommand = errors.New("a command is required")

	// ErrNotAPath reports a command that is a bare name rather than a path.
	ErrNotAPath = errors.New("command must be a path")
)

// Runner runs containers. Construct it with NewRunner.
type Runner struct {
	logger *slog.Logger
}

// NewRunner returns a Runner that logs through logger. The logger is injected
// rather than global so every container operation can be correlated by the
// container_id attribute this package attaches (SSOT §6).
func NewRunner(logger *slog.Logger) *Runner {
	return &Runner{logger: logger}
}

// Run creates a container from spec, runs it to completion, and reports how it
// exited.
//
// A container that exits non-zero is not an error: its status is returned with
// a nil error, and only a failure of Forge itself produces an error. Run blocks
// until the container terminates. If ctx is cancelled the container is killed
// rather than abandoned.
func (r *Runner) Run(ctx context.Context, spec Spec) (process.Status, error) {
	if err := spec.Validate(); err != nil {
		return process.Status{}, err
	}

	// Fail fast rather than fork-bombing if the calling binary re-executed
	// itself without routing InitCommandName to Init (PRD NFR-8).
	if os.Getenv(envInitGuard) != "" {
		return process.Status{}, ErrNestedInit
	}

	id, err := NewID()
	if err != nil {
		return process.Status{}, err
	}
	log := r.logger.With("container_id", id)

	nsCfg := namespace.Config{
		PID:      true,
		UTS:      true,
		Mount:    true,
		Hostname: spec.Hostname,
	}
	if nsCfg.Hostname == "" {
		nsCfg.Hostname = id
	}
	// Validate in the parent so a bad hostname is a clear error here rather
	// than an opaque failure inside the container's init.
	if err := nsCfg.Validate(); err != nil {
		return process.Status{}, fmt.Errorf("container %s: %w", id, err)
	}

	self, err := os.Executable()
	if err != nil {
		return process.Status{}, fmt.Errorf("locating the forge binary to re-execute: %w", err)
	}

	payload, err := json.Marshal(initPayload{
		Namespace: nsCfg,
		Command:   spec.Command,
		Env:       spec.Env,
	})
	if err != nil {
		return process.Status{}, fmt.Errorf("encoding init payload: %w", err)
	}

	log.Debug("creating container",
		"command", spec.Command,
		"hostname", nsCfg.Hostname,
		"clone_flags", fmt.Sprintf("%#x", nsCfg.CloneFlags()),
	)

	status, err := r.start(ctx, log, self, payload, nsCfg, spec)
	if err != nil {
		return process.Status{}, fmt.Errorf("container %s: %w", id, err)
	}

	log.Info("container exited", "exit_code", status.Code, "status", status.String())

	return status, nil
}

// start performs the re-exec handshake and supervises the container. It is
// separated from Run so the resource-ordering — pipe, process, payload, wait —
// reads top to bottom.
func (r *Runner) start(
	ctx context.Context,
	log *slog.Logger,
	self string,
	payload []byte,
	nsCfg namespace.Config,
	spec Spec,
) (process.Status, error) {
	payloadReader, payloadWriter, err := os.Pipe()
	if err != nil {
		return process.Status{}, fmt.Errorf("creating init payload pipe: %w", err)
	}

	p, err := process.New(process.Config{
		Path: self,
		Args: []string{initArgv0, InitCommandName},
		// The init process is Forge's own code and needs no environment beyond
		// the guard that makes a missing init dispatch fail loudly. The
		// container's own environment is applied by execve inside Init.
		Env:        []string{envInitGuard + "=1"},
		CloneFlags: nsCfg.CloneFlags(),
		Stdin:      spec.Stdin,
		Stdout:     spec.Stdout,
		Stderr:     spec.Stderr,
		ExtraFiles: []*os.File{payloadReader},
	})
	if err != nil {
		closeFile(log, payloadReader, "init payload reader")
		closeFile(log, payloadWriter, "init payload writer")
		return process.Status{}, err
	}

	if err := p.Start(ctx); err != nil {
		closeFile(log, payloadReader, "init payload reader")
		closeFile(log, payloadWriter, "init payload writer")
		return process.Status{}, translateCloneError(err)
	}

	// The child holds its own descriptor now. Closing ours is what lets the
	// child's read reach EOF.
	closeFile(log, payloadReader, "init payload reader")

	log.Info("container started", "pid", p.PID(), "state", p.State().String())

	if err := writePayload(payloadWriter, payload); err != nil {
		// The container is already running; reap it rather than leaking it
		// (PRD NFR-8).
		if killErr := p.Signal(syscall.SIGKILL); killErr != nil {
			log.Warn("killing container after a failed handshake", "error", killErr)
		}
		if _, waitErr := p.Wait(ctx); waitErr != nil {
			log.Warn("reaping container after a failed handshake", "error", waitErr)
		}
		return process.Status{}, err
	}

	status, err := p.Wait(ctx)
	if err != nil {
		return status, err
	}

	return status, nil
}

// writePayload hands the init configuration to the child and closes the pipe,
// which is what tells the child it has the whole payload.
//
// The write happens after the child has been started, so the child is already
// draining the pipe; a payload larger than the pipe buffer cannot deadlock.
func writePayload(w *os.File, payload []byte) error {
	_, writeErr := w.Write(payload)
	closeErr := w.Close()

	if writeErr != nil {
		return fmt.Errorf("sending init payload: %w", writeErr)
	}
	if closeErr != nil {
		return fmt.Errorf("closing init payload pipe: %w", closeErr)
	}
	return nil
}

// closeFile closes f, logging rather than returning a failure: these closes
// happen on cleanup paths where the original error must not be masked
// (SSOT §5). Never silently discarded (SSOT §13.7).
func closeFile(log *slog.Logger, f *os.File, what string) {
	if err := f.Close(); err != nil {
		log.Warn("closing "+what, "error", err)
	}
}

// translateCloneError turns the kernel's EPERM into an actionable message.
// Creating namespaces is the first privileged thing Forge does, so this is
// where an unprivileged user finds out.
func translateCloneError(err error) error {
	if errors.Is(err, syscall.EPERM) {
		return fmt.Errorf("%w: %w", namespace.ErrPermission, err)
	}
	return err
}

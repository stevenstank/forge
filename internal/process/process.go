// Package process creates and supervises a container's init process.
//
// It is deliberately ignorant of what a container is: it takes a fully-prepared
// Config — including the clone(2) flags someone else computed — starts the
// process, and tracks it through its lifecycle. Per SSOT §2 it knows nothing
// about namespaces, images, cgroups, or networking.
package process

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sync"
	"syscall"
)

// exitCodeSignalBase is added to a signal number to form an exit code for a
// process killed by a signal, following the convention shells and Docker use:
// SIGKILL (9) reports as 137.
const exitCodeSignalBase = 128

// Sentinel errors callers may branch on.
var (
	// ErrNotStarted reports an operation that requires a running process,
	// attempted before Start.
	ErrNotStarted = errors.New("process has not been started")

	// ErrAlreadyStarted reports a second call to Start.
	ErrAlreadyStarted = errors.New("process has already been started")

	// ErrNoPath reports a Config with no binary to execute.
	ErrNoPath = errors.New("process path is required")

	// ErrNoArgs reports a Config with an empty argument vector. Args[0] is
	// argv[0] and is mandatory.
	ErrNoArgs = errors.New("process args must contain at least argv[0]")
)

// State is a point in the process lifecycle, mirroring the container states in
// SSOT §12 that Stage 1 can observe.
type State int

// The states a Process moves through. Transitions are strictly forward:
// Created → Running → Exited.
const (
	// StateCreated means the process has been configured but not yet forked.
	StateCreated State = iota
	// StateRunning means the process has been forked and has not been reaped.
	StateRunning
	// StateExited means the process terminated and has been reaped.
	StateExited
)

// String implements fmt.Stringer.
func (s State) String() string {
	switch s {
	case StateCreated:
		return "created"
	case StateRunning:
		return "running"
	case StateExited:
		return "exited"
	default:
		return fmt.Sprintf("unknown(%d)", int(s))
	}
}

// Status is how a process terminated.
type Status struct {
	// Code is the exit status. For a process killed by a signal this is
	// 128+signal, matching shell convention, so callers always have a single
	// number to propagate.
	Code int

	// Signal is the signal that killed the process, or 0 if it exited
	// normally.
	Signal syscall.Signal
}

// Signaled reports whether the process was killed by a signal rather than
// exiting of its own accord.
func (s Status) Signaled() bool { return s.Signal != 0 }

// String implements fmt.Stringer.
func (s Status) String() string {
	if s.Signaled() {
		return fmt.Sprintf("%s (exit %d)", s.Signal, s.Code)
	}
	return fmt.Sprintf("exit %d", s.Code)
}

// Config fully describes a process to start. Every field is supplied by the
// caller; this package reads no environment variables and no global state.
type Config struct {
	// Path is the binary to execute. It is used verbatim — no PATH lookup is
	// performed, because a container's PATH is not the host's.
	Path string

	// Args is the complete argument vector, including argv[0].
	Args []string

	// Env is the complete environment. A nil Env yields an empty environment
	// rather than inheriting the host's, so nothing leaks in implicitly.
	Env []string

	// Dir is the working directory. Empty means the caller's.
	Dir string

	// CloneFlags are passed to clone(2). Zero starts an ordinary child.
	CloneFlags uintptr

	// Stdin, Stdout and Stderr are wired to the process. A nil Stdin gives
	// the process an immediate EOF; nil writers discard output.
	Stdin  io.Reader
	Stdout io.Writer
	Stderr io.Writer

	// ExtraFiles are inherited by the child starting at file descriptor 3, in
	// order.
	ExtraFiles []*os.File

	// CgroupFD is a directory the kernel places the child in as it creates
	// it, via clone3(2)'s CLONE_INTO_CGROUP. Nil starts the child wherever
	// the caller is.
	//
	// This package is not told what the directory means, only that the child
	// is to be born inside it. What it buys over writing the child's PID
	// somewhere after the fact is that there is no "after the fact": a
	// process placed into a cgroup once it is already running has, however
	// briefly, run outside it.
	CgroupFD *os.File
}

// Validate reports whether the configuration can be started. It is pure and
// performs no syscalls.
func (c Config) Validate() error {
	if c.Path == "" {
		return ErrNoPath
	}
	if len(c.Args) == 0 {
		return ErrNoArgs
	}
	return nil
}

// Process is a handle on a container's init process.
//
// It is safe for concurrent use: callers routinely signal a process from one
// goroutine while another waits on it.
type Process struct {
	mu     sync.Mutex
	cmd    *exec.Cmd
	state  State
	status Status
}

// New validates cfg and returns a Process in StateCreated. It does not fork;
// call Start for that.
func New(cfg Config) (*Process, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}

	// Build exec.Cmd directly rather than via exec.Command, which would
	// perform a PATH lookup this package explicitly does not want.
	cmd := &exec.Cmd{
		Path:       cfg.Path,
		Args:       cfg.Args,
		Env:        cfg.Env,
		Dir:        cfg.Dir,
		Stdin:      cfg.Stdin,
		Stdout:     cfg.Stdout,
		Stderr:     cfg.Stderr,
		ExtraFiles: cfg.ExtraFiles,
		SysProcAttr: &syscall.SysProcAttr{
			Cloneflags: cfg.CloneFlags,
		},
	}
	if cfg.CgroupFD != nil {
		cmd.SysProcAttr.UseCgroupFD = true
		cmd.SysProcAttr.CgroupFD = int(cfg.CgroupFD.Fd())
	}
	if cmd.Env == nil {
		cmd.Env = []string{}
	}

	return &Process{cmd: cmd, state: StateCreated}, nil
}

// Start forks and execs the process, moving it to StateRunning.
func (p *Process) Start(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	p.mu.Lock()
	defer p.mu.Unlock()

	if p.state != StateCreated {
		return ErrAlreadyStarted
	}
	if err := p.cmd.Start(); err != nil {
		return fmt.Errorf("starting %s: %w", p.cmd.Path, err)
	}
	p.state = StateRunning

	return nil
}

// Wait blocks until the process terminates, reaps it, and returns how it
// exited. A non-zero exit is reported through Status, not as an error; only a
// failure to wait is an error.
//
// If ctx is cancelled the process is killed rather than abandoned, so a
// cancelled run cannot leave a container behind (PRD NFR-5). Because the
// process is PID 1 of its namespace, killing it makes the kernel terminate
// every process in that namespace too.
//
// Wait may be called concurrently with Signal, and may be called again after
// it has returned (subsequent calls repeat the recorded Status). It must not
// be called concurrently with itself: a process can only be reaped once.
func (p *Process) Wait(ctx context.Context) (Status, error) {
	p.mu.Lock()
	state, cmd := p.state, p.cmd
	p.mu.Unlock()

	switch state {
	case StateCreated:
		return Status{}, ErrNotStarted
	case StateExited:
		p.mu.Lock()
		defer p.mu.Unlock()
		return p.status, nil
	}

	// A failure to kill is recorded rather than discarded: on its own it is not
	// actionable, but if the process then also fails to exit it is the
	// explanation, and SSOT §13.7 forbids dropping it silently. The guard is
	// separate from p.mu because the callback takes p.mu via Signal.
	var (
		killMu  sync.Mutex
		killErr error
	)
	stopKiller := context.AfterFunc(ctx, func() {
		if err := p.Signal(syscall.SIGKILL); err != nil {
			killMu.Lock()
			killErr = err
			killMu.Unlock()
		}
	})
	defer stopKiller()

	waitErr := cmd.Wait()

	killMu.Lock()
	kill := killErr
	killMu.Unlock()

	p.mu.Lock()
	defer p.mu.Unlock()

	p.state = StateExited
	p.status = statusFrom(cmd.ProcessState)

	// An ExitError only restates the status we already extracted, so it is not
	// an error from this method's perspective. Anything else is.
	var exitErr *exec.ExitError
	if waitErr != nil && !errors.As(waitErr, &exitErr) {
		err := fmt.Errorf("waiting for pid %d: %w", cmd.Process.Pid, waitErr)
		if kill != nil {
			err = errors.Join(err, fmt.Errorf("killing pid %d after cancellation: %w", cmd.Process.Pid, kill))
		}
		return p.status, err
	}

	return p.status, nil
}

// statusFrom converts the kernel's wait status into Forge's representation.
func statusFrom(ps *os.ProcessState) Status {
	if ps == nil {
		return Status{Code: -1}
	}

	waitStatus, ok := ps.Sys().(syscall.WaitStatus)
	if ok && waitStatus.Signaled() {
		sig := waitStatus.Signal()
		return Status{Code: exitCodeSignalBase + int(sig), Signal: sig}
	}

	return Status{Code: ps.ExitCode()}
}

// Signal sends sig to the process. It is a no-op returning ErrNotStarted before
// Start, and nil after the process has been reaped, so callers can signal
// unconditionally during cleanup without racing.
func (p *Process) Signal(sig syscall.Signal) error {
	p.mu.Lock()
	defer p.mu.Unlock()

	switch p.state {
	case StateCreated:
		return ErrNotStarted
	case StateExited:
		return nil
	}

	if err := p.cmd.Process.Signal(sig); err != nil {
		// The process may have exited between our state check and here; that
		// is a benign race, not a failure to signal.
		if errors.Is(err, os.ErrProcessDone) {
			return nil
		}
		return fmt.Errorf("signalling pid %d with %s: %w", p.cmd.Process.Pid, sig, err)
	}

	return nil
}

// PID returns the process ID as seen from the host, or 0 before Start.
//
// This is the host's view. Inside its own PID namespace the process is PID 1.
func (p *Process) PID() int {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.state == StateCreated || p.cmd.Process == nil {
		return 0
	}
	return p.cmd.Process.Pid
}

// State returns the current lifecycle state.
func (p *Process) State() State {
	p.mu.Lock()
	defer p.mu.Unlock()

	return p.state
}

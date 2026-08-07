package process

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"
	"syscall"

	"golang.org/x/sys/unix"
)

// A handle on a process Forge did not start.
//
// Every stage before Stage 6 only ever talks to processes it forked, and a
// *Process is enough for those: it wraps the child, so it can wait for it and
// collect its status. `forge stop` is the first thing in Forge that has to act
// on a process belonging to a different invocation entirely — a container
// started by a `forge run` running in another terminal — and none of that is
// available. It has a PID out of a file and nothing else.
//
// That difference is why this is a separate type rather than a constructor for
// Process, and why it deliberately cannot report an exit status: only a parent
// can reap, so a handle that offered a Wait returning a status would have to
// invent one. What it can do is signal, and say whether the process is still
// there.

// Sentinel errors for handles.
var (
	// ErrNoProcess reports a PID with no live process behind it. It is
	// returned by Open, and is the ordinary answer for a container that has
	// already exited rather than a failure.
	ErrNoProcess = errors.New("no such process")

	// ErrHandleClosed reports use of a handle after Close.
	ErrHandleClosed = errors.New("process handle is closed")

	// ErrInvalidPID reports a PID that cannot name a process.
	ErrInvalidPID = errors.New("invalid pid")
)

// procStat is the part of /proc/<pid>/stat this package reads.
type procStat struct {
	// state is the single-character run state: R, S, D, Z, T and so on.
	state byte

	// startTicks is the process's start time in clock ticks since boot. It is
	// what distinguishes a PID from the process currently holding it.
	startTicks uint64
}

// Handle is a handle on a running process that this process did not start.
//
// It is safe for concurrent use.
type Handle struct {
	mu     sync.Mutex
	pid    int
	start  uint64
	fd     int
	closed bool
}

// Open returns a handle on the process with the given PID.
//
// It returns ErrNoProcess if there is no live process there — including a
// zombie, which has terminated even though its entry lingers until its parent
// reaps it.
//
// The handle it returns is bound to the process that held the PID at this
// moment, not to the number. That distinction is the whole point of the type.
// PIDs are recycled, and a container's recorded PID may by now belong to
// something else entirely; a signal aimed at a container that lands on an
// unrelated process — sent, as Forge is, by root — is the worst thing this
// package could do. Open takes a pidfd (pidfd_open(2), Linux 5.3, well inside
// the 5.10 floor NFR-6 sets), and every later signal goes through that
// descriptor rather than the number. The kernel will not let a pidfd follow a
// recycled PID onto a new process, so the race cannot be lost after Open even
// in principle.
//
// What Open cannot rule out is a recycle that happened *before* it: if the
// container died and its PID was reused before Forge looked, the handle is a
// legitimate handle on the wrong process. Closing that gap needs the process's
// start time persisted alongside its PID, which the metadata schema does not
// yet carry. The start time is read and kept here so that Alive at least
// notices when the PID underneath a long-lived handle changes hands.
func Open(pid int) (*Handle, error) {
	if pid <= 0 {
		return nil, fmt.Errorf("%w: %d", ErrInvalidPID, pid)
	}

	stat, err := readProcStat(pid)
	if err != nil {
		return nil, err
	}
	if stat.state == 'Z' {
		// A zombie has already terminated; only its entry survives. Reporting
		// it as live would have stop wait out a grace period for a process
		// that ended before it was asked to.
		return nil, fmt.Errorf("%w: %d has exited and is awaiting reaping", ErrNoProcess, pid)
	}

	fd, err := unix.PidfdOpen(pid, 0)
	if err != nil {
		if errors.Is(err, unix.ESRCH) {
			return nil, fmt.Errorf("%w: %d", ErrNoProcess, pid)
		}
		return nil, fmt.Errorf("opening a pidfd for %d: %w", pid, err)
	}

	return &Handle{pid: pid, start: stat.startTicks, fd: fd}, nil
}

// PID returns the process ID the handle refers to.
func (h *Handle) PID() int {
	h.mu.Lock()
	defer h.mu.Unlock()

	return h.pid
}

// Alive reports whether the process is still running.
//
// A process that has exited but not yet been reaped is *not* alive: it will
// never run another instruction, and a caller waiting for it to die is already
// done waiting. Its parent may take any amount of time to collect it, and for
// a container whose parent has itself died that is until host init gets to it.
//
// A PID that has been recycled onto a different process is likewise not alive,
// because the process this handle was opened on is gone. That is what the
// start time recorded by Open is for.
func (h *Handle) Alive() bool {
	h.mu.Lock()
	pid, start, closed := h.pid, h.start, h.closed
	h.mu.Unlock()

	if closed {
		return false
	}

	stat, err := readProcStat(pid)
	if err != nil {
		return false
	}

	return stat.state != 'Z' && stat.startTicks == start
}

// Signal sends sig to the process.
//
// A process that has already exited is not an error: cleanup paths signal
// unconditionally, and "it was already gone" is the outcome they wanted. The
// signal is delivered through the pidfd, so it cannot be redirected onto a
// process that has since taken the PID.
//
// Note what this means for a container's init, which is PID 1 of its own PID
// namespace: the kernel discards a signal sent from an ancestor namespace
// unless that process installed a handler for it. SIGTERM to a shell that has
// none is delivered by this function and then dropped by the kernel, which is
// why a caller has to be prepared to follow it with SIGKILL. SIGKILL and
// SIGSTOP are special-cased and always take effect.
func (h *Handle) Signal(sig syscall.Signal) error {
	h.mu.Lock()
	defer h.mu.Unlock()

	if h.closed {
		return ErrHandleClosed
	}

	err := unix.PidfdSendSignal(h.fd, sig, nil, 0)
	switch {
	case err == nil:
		return nil
	case errors.Is(err, unix.ESRCH):
		// The process exited between the caller's check and here. That is the
		// benign race Process.Signal treats the same way.
		return nil
	default:
		return fmt.Errorf("signalling pid %d with %s: %w", h.pid, sig, err)
	}
}

// Close releases the handle. It is idempotent, so a caller can defer it and
// still close explicitly on an error path.
func (h *Handle) Close() error {
	h.mu.Lock()
	defer h.mu.Unlock()

	if h.closed {
		return nil
	}
	h.closed = true

	if err := unix.Close(h.fd); err != nil {
		return fmt.Errorf("closing the pidfd for %d: %w", h.pid, err)
	}

	return nil
}

// statStartTimeField is the 1-based field number of a process's start time in
// /proc/<pid>/stat, per proc(5).
const statStartTimeField = 22

// statFieldsBeforeComm is how many fields precede the state character: the PID
// and the comm, both of which are consumed by the split below.
const statFieldsBeforeComm = 2

// readProcStat reads the run state and start time of a process.
//
// The parsing is not as simple as splitting on spaces, and the reason is the
// second field: comm is the executable's name, in parentheses, and it may
// itself contain spaces and parentheses — a binary called "my program (beta)"
// is unusual but perfectly legal, and it shifts every field after it. The
// kernel's own guarantee is that the *last* ')' in the line ends comm, so that
// is where parsing starts.
func readProcStat(pid int) (procStat, error) {
	path := fmt.Sprintf("/proc/%d/stat", pid)

	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) || errors.Is(err, unix.ESRCH) {
			return procStat{}, fmt.Errorf("%w: %d", ErrNoProcess, pid)
		}
		return procStat{}, fmt.Errorf("reading %s: %w", path, err)
	}

	return parseProcStat(string(data), pid)
}

// parseProcStat extracts what this package needs from one /proc/<pid>/stat
// line. It is separate from the read so it can be tested against the shapes
// that make the format awkward, without needing a process that has them.
func parseProcStat(line string, pid int) (procStat, error) {
	end := strings.LastIndexByte(line, ')')
	if end < 0 || end+2 > len(line) {
		return procStat{}, fmt.Errorf("parsing /proc/%d/stat: no comm field in %q", pid, line)
	}

	fields := strings.Fields(line[end+1:])
	// The state is the first field after comm; the start time is field 22
	// overall, so it is that far along minus the two fields already consumed.
	const startIndex = statStartTimeField - statFieldsBeforeComm - 1
	if len(fields) <= startIndex {
		return procStat{}, fmt.Errorf("parsing /proc/%d/stat: %d fields after comm, want more than %d",
			pid, len(fields), startIndex)
	}

	state := fields[0]
	if len(state) != 1 {
		return procStat{}, fmt.Errorf("parsing /proc/%d/stat: state %q is not a single character", pid, state)
	}

	start, err := strconv.ParseUint(fields[startIndex], 10, 64)
	if err != nil {
		return procStat{}, fmt.Errorf("parsing /proc/%d/stat: start time %q: %w", pid, fields[startIndex], err)
	}

	return procStat{state: state[0], startTicks: start}, nil
}

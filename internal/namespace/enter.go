package namespace

import (
	"errors"
	"fmt"
	"os"
	"strconv"

	"golang.org/x/sys/unix"
)

// Namespace entry: joining namespaces that already exist, which is what
// `forge exec` needs and what SSOT §2 assigns to this package.
//
// Everything else here creates namespaces, through clone(2) flags computed
// from a Config. Entering is the opposite operation and has almost nothing in
// common with it: the namespaces are named by a process that is already in
// them, they are joined one at a time by setns(2), and the effect is on the
// calling *thread* rather than on a child.
//
// # Why this is harder in Go than it looks
//
// setns(2) with CLONE_NEWNS is refused unless the calling task has a private
// fs_struct — the kernel's mntns_install() ends with "if (fs->users != 1)
// return -EINVAL", because joining a mount namespace replaces the caller's
// root and working directory, and doing that to a structure shared with other
// threads would move them too.
//
// Every thread the Go runtime creates is cloned with CLONE_FS, so they all
// share one fs_struct and fs->users is never 1. A Go program is multithreaded
// before main runs — the runtime starts sysmon during startup — so there is no
// moment at which a plain setns(CLONE_NEWNS) succeeds. This is why runc
// carries a C prelude that runs before its Go runtime initialises, and it is
// the usual reason a runtime written in Go is said to be unable to implement
// exec.
//
// It can. unshare(CLONE_FS) gives the *calling thread* a private copy of the
// fs_struct, with a users count of exactly 1, which is precisely what
// mntns_install asks for:
//
//	runtime.LockOSThread()      pin this goroutine to one thread
//	unix.Unshare(CLONE_FS)      that thread stops sharing root and cwd
//	setns(mntFD, CLONE_NEWNS)   now permitted
//	fork                        the child inherits the thread's namespaces
//
// The thread is not usable for anything else afterwards: its root, its working
// directory and its namespaces are the container's. That is what the
// LockOSThread is really for — not the syscall, but the guarantee that no
// other goroutine is ever scheduled onto it. A goroutine that exits while
// still locked has its thread terminated by the runtime rather than returned
// to the pool, so the altered thread dies with the operation that needed it
// and nothing leaks back into the process.

// Kind is a type of namespace, named as the kernel names it under
// /proc/<pid>/ns.
type Kind string

// The namespaces Forge can enter.
const (
	// KindIPC is the System V IPC and POSIX message queue namespace.
	KindIPC Kind = "ipc"

	// KindUTS is the hostname and domain name namespace.
	KindUTS Kind = "uts"

	// KindNetwork is the network namespace: interfaces, routes, firewall
	// rules and sockets.
	KindNetwork Kind = "net"

	// KindPID is the process ID namespace.
	KindPID Kind = "pid"

	// KindMount is the mount namespace.
	KindMount Kind = "mnt"
)

// EntryOrder is the order namespaces are joined in, and it is an order rather
// than a set because two of the five interact with the joining itself.
//
// The mount namespace is last because joining it replaces the thread's root
// and working directory: from that point every relative or absolute path
// resolves inside the container, so anything still needing a host path must
// have happened already. Nothing here does — the descriptors are all opened
// before any of them is joined — but the ordering makes that a property of the
// code rather than of the reader's memory.
//
// The PID namespace is next to last because it is the odd one out: setns with
// CLONE_NEWPID does not move the caller at all. It sets the namespace that the
// caller's *children* will be created in, so the effect only appears at the
// fork after it.
var EntryOrder = []Kind{KindIPC, KindUTS, KindNetwork, KindPID, KindMount}

// Sentinel errors callers may branch on.
var (
	// ErrNoSuchNamespace reports a namespace that does not exist for the
	// given process, which for a container almost always means the container
	// has exited.
	ErrNoSuchNamespace = errors.New("namespace not found")

	// ErrNotSingleThreaded reports the EINVAL the kernel returns when a
	// mount namespace is joined without a private fs_struct. It should be
	// unreachable — Enter unshares first — and exists so that if it ever is
	// reached, the message says what is actually wrong.
	ErrNotSingleThreaded = errors.New("joining a mount namespace requires an unshared fs_struct")
)

// flag returns the setns(2) nstype for a kind.
//
// Passing the type rather than 0 makes the kernel check that the descriptor is
// the namespace it was asked for. Handing setns the wrong file would otherwise
// silently join something else, and "silently joined the wrong namespace" is
// not a failure anybody would enjoy debugging.
func (k Kind) flag() int {
	switch k {
	case KindIPC:
		return unix.CLONE_NEWIPC
	case KindUTS:
		return unix.CLONE_NEWUTS
	case KindNetwork:
		return unix.CLONE_NEWNET
	case KindPID:
		return unix.CLONE_NEWPID
	case KindMount:
		return unix.CLONE_NEWNS
	default:
		return 0
	}
}

// Valid reports whether k is a namespace this package knows.
func (k Kind) Valid() bool { return k.flag() != 0 }

// Path returns the /proc entry naming one of a process's namespaces. It is
// pure and touches nothing.
func (k Kind) Path(pid int) string {
	return "/proc/" + strconv.Itoa(pid) + "/ns/" + string(k)
}

// Handle is an open reference to one namespace.
//
// It is a descriptor rather than a path, and holding it matters: an open
// handle keeps the namespace alive even after the last process in it exits, so
// a container that dies between being looked up and being joined produces a
// clean failure rather than a join into whatever has since reused the
// identifier.
type Handle struct {
	// Kind is which namespace this is.
	Kind Kind

	file *os.File
}

// Close releases the handle. It is idempotent.
func (h *Handle) Close() error {
	if h.file == nil {
		return nil
	}
	err := h.file.Close()
	h.file = nil
	if err != nil && !errors.Is(err, os.ErrClosed) {
		return fmt.Errorf("closing the %s namespace handle: %w", h.Kind, err)
	}
	return nil
}

// Open returns handles on the given namespaces of a process.
//
// All of them are opened before any is joined, which is what lets the mount
// namespace be joined at all: after that join the paths these come from
// resolve inside the container, where /proc may be the container's own or may
// not be mounted.
//
// A failure part-way closes what it already opened, so a caller that gets an
// error has nothing to release.
func Open(pid int, kinds ...Kind) ([]Handle, error) {
	if pid <= 0 {
		return nil, fmt.Errorf("invalid pid %d", pid)
	}

	handles := make([]Handle, 0, len(kinds))
	for _, kind := range kinds {
		if !kind.Valid() {
			return nil, errors.Join(fmt.Errorf("unknown namespace %q", string(kind)), CloseAll(handles))
		}

		path := kind.Path(pid)
		f, err := os.Open(path)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				err = fmt.Errorf("%w: %s", ErrNoSuchNamespace, path)
			} else {
				err = fmt.Errorf("opening %s: %w", path, err)
			}
			return nil, errors.Join(err, CloseAll(handles))
		}

		handles = append(handles, Handle{Kind: kind, file: f})
	}

	return handles, nil
}

// CloseAll releases every handle, collecting rather than short-circuiting on
// failure so that one bad descriptor cannot strand the others.
func CloseAll(handles []Handle) error {
	var errs []error
	for i := range handles {
		if err := handles[i].Close(); err != nil {
			errs = append(errs, err)
		}
	}

	return errors.Join(errs...)
}

// Enter associates the calling thread with the given namespaces.
//
// The caller must have locked its OS thread and must not unlock it: the thread
// is left in the container's namespaces, with the container's root and working
// directory, and returning it to the Go runtime's pool would hand a container's
// view of the world to whatever goroutine ran there next. Letting the locked
// goroutine exit is the correct disposal — the runtime terminates a thread that
// was never unlocked.
//
// It is also the caller's job to fork from that same thread. A child inherits
// the namespaces of the thread that created it, so a process started from any
// other goroutine lands on the host, silently and intermittently. That is the
// most dangerous mistake available in this area, and it is why Enter does not
// start the process itself: the fork belongs to internal/process, and the two
// have to happen on one thread that the caller owns.
//
// Handles are joined in EntryOrder rather than in the order given, so a caller
// cannot get the ordering wrong by listing them differently.
func Enter(handles []Handle) error {
	byKind := make(map[Kind]*Handle, len(handles))
	for i := range handles {
		byKind[handles[i].Kind] = &handles[i]
	}

	// Before anything else, and only for this thread: a private fs_struct.
	// Without it the mount namespace below is refused with EINVAL, for the
	// reason set out at the top of this file. It is unconditional rather than
	// done only when a mount namespace is present, because it is harmless —
	// the thread is being dedicated to this operation either way — and a
	// conditional would be one more thing to get wrong.
	if err := unix.Unshare(unix.CLONE_FS); err != nil {
		return fmt.Errorf("unsharing the thread's filesystem context: %w", err)
	}

	for _, kind := range EntryOrder {
		h, ok := byKind[kind]
		if !ok {
			continue
		}

		if err := unix.Setns(int(h.file.Fd()), kind.flag()); err != nil {
			if errors.Is(err, unix.EINVAL) && kind == KindMount {
				return fmt.Errorf("%w: joining the %s namespace: %w", ErrNotSingleThreaded, kind, err)
			}
			return fmt.Errorf("joining the %s namespace: %w", kind, translatePermission(err))
		}
	}

	return nil
}

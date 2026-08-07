package state

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
)

// writeAtomic replaces path with data, or leaves it exactly as it was.
//
// The sequence is the one every durable-write does, and each step earns its
// place:
//
//	1  write a temporary file in the same directory   a rename is only atomic
//	                                                  within a filesystem, and
//	                                                  the same directory is the
//	                                                  only way to be sure
//	2  fsync the file                                 the bytes are on the disk,
//	                                                  not just in the page cache
//	3  rename it over the target                      atomic: a reader sees the
//	                                                  whole old file or the
//	                                                  whole new one, never a
//	                                                  mixture and never a
//	                                                  zero-length file
//	4  fsync the directory                            the *name* is on the disk
//	                                                  too — without this the
//	                                                  file survives a power cut
//	                                                  but the name pointing at
//	                                                  it may not
//
// Step 4 is the one that is usually skipped and is the difference between
// surviving a crash and surviving a power loss. Forge is a container runtime;
// a host that lost power with containers running is exactly the case the
// record exists to clean up after.
//
// A failure anywhere removes the temporary file, so an interrupted write
// leaves no residue and no partial record. If that removal also fails, its
// error is joined to the one being returned rather than dropped: the original
// failure stays the one a caller matches on, and the leftover file is still
// reported (SSOT §5, §13.7).
func writeAtomic(path string, data []byte) (err error) {
	dir := filepath.Dir(path)

	tmp, err := os.CreateTemp(dir, tempPrefix+"*")
	if err != nil {
		return fmt.Errorf("creating a temporary file in %q: %w", dir, err)
	}
	tmpName := tmp.Name()

	// Every early return from here removes the temporary file. The named
	// return is what lets one deferred cleanup serve all of them without each
	// error path having to remember.
	defer func() {
		if err == nil {
			return
		}
		if rmErr := os.Remove(tmpName); rmErr != nil && !errors.Is(rmErr, os.ErrNotExist) {
			err = errors.Join(err, fmt.Errorf("removing the temporary file %q: %w", tmpName, rmErr))
		}
	}()

	if err := writeAndSync(tmp, data); err != nil {
		return fmt.Errorf("writing %q: %w", tmpName, err)
	}

	// os.CreateTemp is subject to the process umask, and these permissions are
	// a decision rather than a default: a record holds the container's command
	// line and is not other users' to read.
	if err := os.Chmod(tmpName, filePerm); err != nil {
		return fmt.Errorf("setting permissions on %q: %w", tmpName, err)
	}

	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("replacing %q: %w", path, err)
	}

	if err := syncDir(dir); err != nil {
		// The rename has already happened, so the record is live and correct;
		// what is uncertain is only whether it would survive losing power in
		// the next moment. That is worth reporting and is not worth undoing.
		return fmt.Errorf("flushing the directory %q: %w", dir, err)
	}

	return nil
}

// writeAndSync writes data to f, flushes it to the disk, and closes it.
//
// Close is deferred through the named return rather than called at the end,
// because a write that succeeded into a buffer and then failed to close has
// not been written, and returning nil there is how data is lost.
func writeAndSync(f *os.File, data []byte) (err error) {
	defer func() {
		if closeErr := f.Close(); closeErr != nil && err == nil {
			err = fmt.Errorf("closing: %w", closeErr)
		}
	}()

	if _, err := f.Write(data); err != nil {
		return err
	}

	return f.Sync()
}

// syncDir flushes a directory's own entries, which is what makes a rename
// durable rather than merely visible.
// The named return is load-bearing: without it the deferred close would
// assign to a local and the error would vanish, which SSOT §13.7 forbids and
// which is easy to write by accident.
func syncDir(dir string) (err error) {
	f, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer func() {
		if closeErr := f.Close(); closeErr != nil && err == nil {
			err = closeErr
		}
	}()

	return f.Sync()
}

// dirLock is an exclusive hold on one container's record, taken for the whole
// of a read-modify-write.
type dirLock struct {
	f *os.File
}

// lockDir takes the exclusive lock on a container's directory.
//
// flock rather than a lock file whose existence means "locked", because the
// kernel releases an flock when the holder's descriptor closes — including
// when the holder was killed, or panicked, or the host cut power. A lock whose
// release depends on the holder running its own cleanup is a lock that a crash
// leaves held forever, and the container it protects unreadable until somebody
// deletes a file by hand.
//
// The lock is advisory and scoped to the open file description, not the
// process, so two goroutines in one process contend with each other exactly as
// two processes do. That is what makes it the right primitive here: `forge
// stop` and a supervisor are different processes, while a test's goroutines
// are not, and both have to be serialised by the same code.
//
// It blocks. A caller waiting here is waiting for another writer's fsync,
// which is bounded by the disk; a caller waiting forever means a live process
// is holding a record it never finished with, which is a bug to find rather
// than a timeout to paper over.
func lockDir(dir string) (*dirLock, error) {
	path := filepath.Join(dir, lockFileName)

	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, filePerm)
	if err != nil {
		return nil, fmt.Errorf("opening the lock file %q: %w", path, err)
	}

	// flock is interruptible, and the Go runtime interrupts threads routinely
	// to preempt goroutines. EINTR here means "try again", not "failed".
	for {
		err := unix.Flock(int(f.Fd()), unix.LOCK_EX)
		if err == nil {
			return &dirLock{f: f}, nil
		}
		if errors.Is(err, unix.EINTR) {
			continue
		}

		return nil, errors.Join(
			fmt.Errorf("locking %q: %w", path, err),
			f.Close(),
		)
	}
}

// unlock releases the lock. Closing the descriptor is what releases it; the
// explicit LOCK_UN is left to the kernel.
func (l *dirLock) unlock() error {
	if err := l.f.Close(); err != nil {
		return fmt.Errorf("releasing the lock %q: %w", l.f.Name(), err)
	}
	return nil
}

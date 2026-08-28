package cgroup

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

// Destroy's retry budget. A cgroup can refuse to be removed for a few
// milliseconds after its last process exits, because the kernel has not
// finished reaping it yet; these bound the wait at roughly 90ms, which is far
// longer than that takes and still short enough that a container teardown
// never appears to hang.
const (
	destroyAttempts    = 10
	destroyBackoff     = 10 * time.Millisecond
	evictAfterAttempts = 1
)

// destroy removes the cgroup at dir (FR-3.5).
//
// Unlike a mount namespace, a cgroup gets no help from the kernel: it outlives
// every process that was in it and persists until something calls rmdir(2). So
// this is real work, and it is the only place in the package that does it.
//
// It is idempotent (SSOT §13.3): a directory that is already gone is success,
// which is what lets internal/runtime register Destroy on its cleanup stack
// unconditionally and call it again after a Create that failed part-way.
func destroy(dir string) error {
	var lastErr error

	for attempt := range destroyAttempts {
		err := os.Remove(dir)
		if err == nil || errors.Is(err, os.ErrNotExist) {
			// ENOENT is success. Nothing to remove is the state Destroy
			// exists to reach.
			return nil
		}
		lastErr = err

		// rmdir(2) refuses a cgroup that still has child cgroups or member
		// processes. Deal with the children first: a container cannot create
		// them itself, but a crashed run reconciled later might find some.
		//
		// The interface files inside are not touched. The kernel owns them,
		// ignores them when deciding whether a cgroup is empty, and removes
		// them with the directory — so there is nothing here to unlink, and
		// code that tried would be a general-purpose file remover pointed at a
		// caller-supplied path.
		if err := removeChildCgroups(dir); err != nil {
			return err
		}

		// Only then start killing. The common case — the container's last
		// process has exited and the kernel is mid-reap — resolves on the
		// retry alone, and evicting before giving that a chance would mean
		// signalling processes that are already dead.
		if attempt >= evictAfterAttempts {
			if err := evict(dir); err != nil {
				return err
			}
			time.Sleep(destroyBackoff)
		}
	}

	if pids, err := readProcs(dir); err == nil && len(pids) > 0 {
		return fmt.Errorf("%w: %s still holds %v", ErrNotEmpty, dir, pids)
	}

	return fmt.Errorf("removing cgroup %s: %w", dir, translate(lastErr))
}

// removeChildCgroups removes every subdirectory of dir, depth first. A cgroup
// cannot be removed while it has children.
func removeChildCgroups(dir string) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("listing cgroup %s: %w", dir, translate(err))
	}

	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		if err := destroy(filepath.Join(dir, entry.Name())); err != nil {
			return err
		}
	}

	return nil
}

// evict kills every process still in the cgroup at dir.
//
// cgroup.kill is the right tool: writing "1" kills the whole subtree at once,
// atomically, with no window for a survivor to fork. It arrived in Linux 5.14,
// so on Forge's minimum kernel (5.10, PRD NFR-6) the fallback is to signal each
// member PID directly — racier, but the cgroup is already being torn down and
// anything forked in that window is a member too and dies on the next pass.
func evict(dir string) error {
	pids, err := readProcs(dir)
	if err != nil {
		return err
	}
	if len(pids) == 0 {
		return nil
	}

	if err := writeControlFile(dir, fileKill, "1"); err == nil {
		return nil
	}

	for _, pid := range pids {
		// Never signal PID 1 or below: 0 means "every process in my process
		// group", which on this code path would include forge itself.
		if pid <= 1 {
			continue
		}
		process, err := os.FindProcess(pid)
		if err != nil {
			continue
		}
		// A process that exited between the read and here is the outcome this
		// wanted, so "already finished" is not a failure. Anything else — a
		// permission error, most plausibly — means the cgroup cannot be
		// emptied, and saying so beats retrying until the budget runs out and
		// reporting the survivors without a reason.
		if err := process.Signal(os.Kill); err != nil &&
			!errors.Is(err, os.ErrProcessDone) && !errors.Is(err, syscall.ESRCH) {
			return fmt.Errorf("killing process %d in cgroup %s: %w", pid, dir, translate(err))
		}
	}

	return nil
}

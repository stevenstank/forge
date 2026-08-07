package image

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Cleanup and rollback (NFR-5, PRD §10.4).
//
// Nothing in this package registers itself on internal/runtime's cleanup stack,
// and that is the design rather than an omission. Everything durable it creates
// is either content-addressed and shared — a blob, which no single run may
// delete — or written inside a destination directory the caller already owns
// and already cleans up.
//
// What is left is rollback scoped to a single function, and there are exactly
// three pieces of it:
//
//   - A staging file, released by the deferred Discard in fetchInto.
//   - A partially built root filesystem, emptied by BuildRootfs's own stack.
//   - A blob that failed verification at use, quarantined by Remove.
//
// All three are idempotent, and all three tolerate the resource being already
// gone, because that is the state they exist to reach.

// cleanupStack releases what a function created, in reverse order.
//
// It is the same model internal/runtime uses (SSOT §11.3), reproduced here
// rather than shared because a leaf package must not import the orchestrator.
// Two properties matter:
//
//   - It always runs everything. A cleanup that fails must not prevent the ones
//     registered before it, or a failed operation leaks precisely the resources
//     the unwind existed to release.
//   - It never returns an error. Failures are logged at WARN and the original
//     error — the reason the stack is unwinding at all — is the one the caller
//     sees (SSOT §5). Logging rather than discarding satisfies §13.7.
type cleanupStack struct {
	logger *slog.Logger

	mu        sync.Mutex
	steps     []cleanupStep
	done      bool
	cancelled bool
}

// cleanupStep is one registered cleanup, with a name for the log line.
type cleanupStep struct {
	name string
	fn   func() error
}

// newCleanupStack returns an empty stack that logs failures through logger.
func newCleanupStack(logger *slog.Logger) *cleanupStack {
	if logger == nil {
		logger = slog.Default()
	}
	return &cleanupStack{logger: logger}
}

// push registers a cleanup. name describes what is being released, in the
// present participle, so the log line reads as the action that failed.
func (c *cleanupStack) push(name string, fn func() error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.steps = append(c.steps, cleanupStep{name: name, fn: fn})
}

// cancel disarms the stack, for the path where everything succeeded and there
// is nothing to roll back.
func (c *cleanupStack) cancel() {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.cancelled = true
}

// unwind runs every registered cleanup in reverse order.
//
// It is idempotent, so a function can defer it unconditionally and still call
// it explicitly on an error path without releasing anything twice.
func (c *cleanupStack) unwind() {
	c.mu.Lock()
	if c.done || c.cancelled {
		c.done = true
		c.mu.Unlock()
		return
	}
	c.done = true
	steps := c.steps
	c.steps = nil
	c.mu.Unlock()

	for i := len(steps) - 1; i >= 0; i-- {
		if err := steps[i].fn(); err != nil {
			c.logger.Warn("cleanup failed", "step", steps[i].name, "error", err)
		}
	}
}

// Discard closes and removes a staging file (NFR-5).
//
// It is idempotent by construction rather than by flag: both steps treat
// "already done" as success, so the deferred call in fetchInto is correct on
// every path into it — a download that failed, one that was interrupted, one
// that committed, a Commit that failed part-way through publishing, and a
// Discard that already ran.
//
// It deliberately does not short-circuit on the Staging having been committed.
// Commit closes the file and marks the Staging spent before link(2) has
// succeeded, so a publication that fails after that point — a full disk, an
// unwritable blobs/ directory — would otherwise leave this function with
// nothing to do and the .part file behind until PruneStaging collected it a day
// later. Removing the staging *name* can never affect a blob that was
// published: the blob is a separate hard link to the same inode.
//
// This is the whole of an interrupted download's rollback. There is nothing
// else to undo, because a staging file is never under blobs/, is never opened
// by anything but its own download, and has a random name — so it can neither
// be mistaken for a blob nor collide with another run's.
func (s *Staging) Discard() error {
	s.closed = true

	name := s.file.Name()

	var errs []error
	if err := s.file.Close(); err != nil && !errors.Is(err, os.ErrClosed) {
		errs = append(errs, fmt.Errorf("closing %q: %w", name, err))
	}
	if err := os.Remove(name); err != nil && !errors.Is(err, os.ErrNotExist) {
		errs = append(errs, fmt.Errorf("removing %q: %w", name, err))
	}

	if len(errs) > 0 {
		return fmt.Errorf("discarding a staging file: %w", errors.Join(errs...))
	}

	return nil
}

// Remove deletes a blob from the cache (NFR-5).
//
// It is idempotent: removing a blob that is not there is success, so quarantine
// paths can call it unconditionally.
//
// It exists for exactly one caller — UnpackLayer, when a cached blob does not
// hash to its own name. That narrowness is deliberate. The cache is shared with
// every concurrent and future run, so "clean up after yourself" is the wrong
// instinct here: a run must never delete a blob it has not just proved is
// corrupt. Content addressing is what makes the one legitimate deletion safe.
// Bytes that do not hash to their name are definitively wrong, not merely
// suspect, and the next run will download them again.
func (c *Cache) Remove(digest string) error {
	path, err := c.Path(digest)
	if err != nil {
		return err
	}

	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("removing blob %s: %w", digest, err)
	}

	c.logger.Debug("removed a blob from the cache", "digest", digest)

	return nil
}

// PruneStaging removes abandoned downloads older than olderThan (NFR-5).
//
// A forge that was SIGKILLed mid-download leaves a staging file no deferred
// Discard will ever run. Nothing reads it and nothing can mistake it for a
// blob, so it is waste rather than corruption — but waste that accumulates, so
// Pull collects it once per pull.
//
// The age bound is the entire safety argument. Staging files are not owned by
// any process in a way this can observe, so a bound shorter than the longest
// plausible download would delete a concurrent run's live one. It is idempotent
// and tolerant: a file that disappears between the listing and the removal was
// another run finishing normally, which is success.
func (c *Cache) PruneStaging(ctx context.Context, olderThan time.Duration) error {
	dir := filepath.Join(c.root, stagingDirName)

	entries, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			// Nothing has ever been downloaded into this cache.
			return nil
		}
		return fmt.Errorf("reading %q: %w", dir, err)
	}

	cutoff := time.Now().Add(-olderThan)

	var errs []error
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return err
		}
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), strings.TrimPrefix(stagingPattern, "*")) {
			continue
		}

		info, err := entry.Info()
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			errs = append(errs, fmt.Errorf("inspecting %q: %w", entry.Name(), err))
			continue
		}
		if info.ModTime().After(cutoff) {
			continue
		}

		path := filepath.Join(dir, entry.Name())
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			errs = append(errs, fmt.Errorf("removing %q: %w", path, err))
			continue
		}

		c.logger.Debug("removed an abandoned download", "path", path, "age", time.Since(info.ModTime()))
	}

	if len(errs) > 0 {
		return fmt.Errorf("pruning abandoned downloads in %q: %w", dir, errors.Join(errs...))
	}

	return nil
}

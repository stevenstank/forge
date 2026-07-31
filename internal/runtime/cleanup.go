package runtime

import (
	"log/slog"
	"sync"
)

// cleanupStack releases the host resources a run created, in reverse order.
//
// It implements SSOT §11.3. Two properties matter and are tested:
//
//   - It always runs everything. A cleanup that fails must not prevent the
//     ones registered before it, or a failed run leaks precisely the resources
//     the unwind existed to release.
//   - It never returns an error. Cleanup failures are logged at WARN and the
//     original error — the reason the run is unwinding at all — is the one the
//     caller sees (SSOT §5). Logging rather than discarding satisfies §13.7.
type cleanupStack struct {
	logger *slog.Logger

	mu   sync.Mutex
	fns  []cleanupStep
	done bool
}

// cleanupStep is one registered cleanup, with a name for the log line.
type cleanupStep struct {
	name string
	fn   func() error
}

// newCleanupStack returns an empty stack that logs failures through logger.
func newCleanupStack(logger *slog.Logger) *cleanupStack {
	return &cleanupStack{logger: logger}
}

// push registers a cleanup. name describes what is being released, in the
// present participle, so the log line reads as the action that failed.
func (c *cleanupStack) push(name string, fn func() error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.fns = append(c.fns, cleanupStep{name: name, fn: fn})
}

// unwind runs every registered cleanup in reverse order.
//
// It is idempotent, so a run can defer it unconditionally and still call it
// explicitly on an error path without releasing anything twice.
func (c *cleanupStack) unwind() {
	c.mu.Lock()
	if c.done {
		c.mu.Unlock()
		return
	}
	c.done = true
	steps := c.fns
	c.fns = nil
	c.mu.Unlock()

	for i := len(steps) - 1; i >= 0; i-- {
		if err := steps[i].fn(); err != nil {
			c.logger.Warn("cleanup failed", "step", steps[i].name, "error", err)
		}
	}
}

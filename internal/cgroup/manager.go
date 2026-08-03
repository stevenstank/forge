package cgroup

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// Manager creates and removes container cgroups under a single cgroup v2
// hierarchy.
//
// It holds no state beyond the hierarchy's root: every operation is addressed
// by container ID and re-derives its path, so a Manager can destroy a leaf it
// did not create — which is what a crash-recovery sweep and, from Stage 6,
// `forge rm` need (SSOT §11.2, §13.4).
type Manager struct {
	root string
}

// New returns a Manager for the cgroup v2 hierarchy mounted at root. An empty
// root means DefaultRoot.
//
// It performs no I/O and cannot fail. Whether root really is a unified
// hierarchy, and whether the controllers a container needs are available there,
// depends on what is being asked for, so it is decided by Create — which can
// report it — rather than here.
func New(root string) *Manager {
	if root == "" {
		root = DefaultRoot
	}
	return &Manager{root: filepath.Clean(root)}
}

// Root returns the hierarchy root the Manager was built with.
func (m *Manager) Root() string { return m.root }

// Parent returns Forge's own cgroup, the parent of every container leaf.
//
// It is created on demand and never removed: two concurrent runs would race to
// remove a directory the other is about to create in, and an empty cgroup
// directory costs nothing. Forge's own process is never a member of it, so it
// stays free of the "no internal processes" rule and controllers can be
// delegated to it at any time.
func (m *Manager) Parent() string { return filepath.Join(m.root, ParentName) }

// Path returns the cgroup path for a container.
func (m *Manager) Path(containerID string) (string, error) {
	if err := validateContainerID(containerID); err != nil {
		return "", err
	}
	return filepath.Join(m.Parent(), containerID), nil
}

// Create makes the container's cgroup leaf and writes its limits (FR-3.1 …
// FR-3.4).
//
// The limits are written before any process is a member, so they are in force
// from the moment the container joins rather than some time after it starts.
//
// A container with no limits still gets a leaf: FR-3.1 is unconditional, and an
// unlimited cgroup still accounts for what the container uses, which is what
// `forge stats` will read in a later stage.
//
// Create performs no rollback of its own. If it fails after the leaf exists it
// says so and leaves it; unwinding is the orchestrator's job and Destroy is
// idempotent, so internal/runtime's cleanup stack removes it either way
// (SSOT §11.3).
func (m *Manager) Create(containerID string, limits Limits) error {
	path, err := m.Path(containerID)
	if err != nil {
		return err
	}
	if err := limits.Validate(); err != nil {
		return fmt.Errorf("cgroup %s: %w", containerID, err)
	}

	required := limits.Controllers()
	if err := m.prepareParent(required); err != nil {
		return fmt.Errorf("cgroup %s: %w", containerID, err)
	}

	// Mkdir, not MkdirAll: the parent is created deliberately just above, and
	// an already-existing leaf means two containers were given the same ID —
	// a bug worth reporting rather than papering over by reusing the cgroup.
	if err := os.Mkdir(path, 0o755); err != nil {
		if errors.Is(err, os.ErrExist) {
			return fmt.Errorf("%w: %s", ErrExists, path)
		}
		return fmt.Errorf("creating cgroup %s: %w", path, translate(err))
	}

	if err := applyLimits(path, limits); err != nil {
		return fmt.Errorf("cgroup %s: %w", containerID, err)
	}

	return nil
}

// Add makes the process a member of the container's cgroup, and with it every
// process it goes on to fork.
//
// It is separate from Create because the two happen either side of clone(2):
// the cgroup must exist and be configured before the container starts, and the
// PID only exists afterwards. internal/runtime closes that window by attaching
// the container's init while it is still blocked on its startup handshake, so
// nothing of the container's own has run yet.
func (m *Manager) Add(containerID string, pid int) error {
	path, err := m.Path(containerID)
	if err != nil {
		return err
	}
	if pid <= 0 {
		return fmt.Errorf("%w: pid %d is not a process", ErrInvalidLimit, pid)
	}

	if _, err := os.Stat(path); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("%w: %s", ErrNotFound, path)
		}
		return fmt.Errorf("cgroup %s: %w", containerID, translate(err))
	}

	if err := addProc(path, pid); err != nil {
		return fmt.Errorf("adding pid %d to cgroup %s: %w", pid, containerID, err)
	}

	return nil
}

// Destroy removes the container's cgroup (FR-3.5).
//
// It is idempotent: a container that never had a cgroup, or whose cgroup was
// already removed, is not an error. Any process still inside is killed first —
// a cgroup cannot be removed while it has members, and leaving the leaf behind
// because something outlived the container would leak precisely the resource
// this releases.
func (m *Manager) Destroy(containerID string) error {
	path, err := m.Path(containerID)
	if err != nil {
		return err
	}

	if err := destroy(path); err != nil {
		return fmt.Errorf("destroying cgroup %s: %w", containerID, err)
	}

	return nil
}

// prepareParent makes sure <root>/forge exists and delegates the controllers a
// container is about to need.
//
// Controllers are enabled in two places, because cgroup v2 delegation is one
// level deep: writing "+memory" to a cgroup's cgroup.subtree_control makes
// memory available to its *children*. The root delegates to <root>/forge, and
// <root>/forge delegates to the leaves.
//
// Only the controllers this container asks for are enabled. A host that is not
// offering, say, the cpu controller then stays perfectly usable for containers
// that only limit memory.
func (m *Manager) prepareParent(required []Controller) error {
	available, err := readControllers(m.root)
	if err != nil {
		return err
	}
	for _, controller := range required {
		if !available[controller] {
			return fmt.Errorf("%w: %q is not available at %s", ErrControllerUnavailable, controller, m.root)
		}
	}

	if err := enableControllers(m.root, required); err != nil {
		return err
	}

	parent := m.Parent()
	if err := os.Mkdir(parent, 0o755); err != nil && !errors.Is(err, os.ErrExist) {
		return fmt.Errorf("creating %s: %w", parent, translate(err))
	}

	return enableControllers(parent, required)
}

package cgroup

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// The cgroup v2 interface files this package reads or writes. Naming them once
// keeps the string literals out of the logic below.
const (
	fileControllers    = "cgroup.controllers"
	fileSubtreeControl = "cgroup.subtree_control"
	fileProcs          = "cgroup.procs"
	fileKill           = "cgroup.kill"
)

// applyLimits writes every limit into the leaf at dir.
//
// The writes are independent: each controller file is its own transaction as
// far as the kernel is concerned, so a failure part-way leaves earlier limits
// in force. That is deliberate and safe — the caller (internal/runtime) treats
// a failed Create as a failed container and destroys the leaf, and a
// half-limited cgroup that is about to be removed constrains nothing.
func applyLimits(dir string, limits Limits) error {
	for _, file := range limits.Files() {
		err := writeControlFile(dir, file.Name, file.Value)
		if err == nil {
			continue
		}

		// An optional file that is not there is a kernel that does not
		// implement it — memory.swap.max on a build without swap accounting.
		// Nothing can be done about it from here and nothing is being
		// silently weakened: a kernel with no swap accounting has no
		// per-cgroup swap interface for anyone to set.
		if file.Optional && errors.Is(err, os.ErrNotExist) {
			continue
		}

		return err
	}

	return nil
}

// addProc makes the process a member of the cgroup at dir by writing its PID
// to cgroup.procs.
//
// Writing a PID moves the whole thread group, and every process it forks from
// then on is a member too. It does not move processes it has *already* forked,
// which is why internal/runtime attaches the container's init before the init
// is allowed to proceed past its handshake.
func addProc(dir string, pid int) error {
	return writeControlFile(dir, fileProcs, strconv.Itoa(pid))
}

// readProcs returns the PIDs currently in the cgroup at dir.
//
// A missing file means the cgroup is gone, which is not an error to a caller
// that is trying to empty it.
func readProcs(dir string) ([]int, error) {
	content, err := os.ReadFile(filepath.Join(dir, fileProcs))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("reading %s: %w", filepath.Join(dir, fileProcs), translate(err))
	}

	var pids []int
	for _, line := range strings.Fields(string(content)) {
		pid, err := strconv.Atoi(line)
		if err != nil {
			// The kernel does not write anything else here; if it ever does,
			// skipping it is better than failing a cleanup over it.
			continue
		}
		pids = append(pids, pid)
	}

	return pids, nil
}

// readControllers returns the controllers the cgroup at dir can delegate to its
// children, from cgroup.controllers.
//
// The absence of this file is how a non-v2 hierarchy is detected: cgroup v1 has
// no such interface file, and neither does an ordinary directory.
func readControllers(dir string) (map[Controller]bool, error) {
	content, err := os.ReadFile(filepath.Join(dir, fileControllers))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("%w: %s has no %s",
				ErrUnifiedHierarchyNotMounted, dir, fileControllers)
		}
		return nil, fmt.Errorf("reading %s: %w", filepath.Join(dir, fileControllers), translate(err))
	}

	available := make(map[Controller]bool)
	for _, name := range strings.Fields(string(content)) {
		available[Controller(name)] = true
	}

	return available, nil
}

// enableControllers delegates controllers to the children of the cgroup at dir
// by writing "+name" entries to cgroup.subtree_control.
//
// It writes only what is missing, and nothing at all when everything is already
// enabled. That matters on a delegated hierarchy — a Forge running inside a
// container — where the file may not be writable even though the controllers
// the caller needs are already on.
func enableControllers(dir string, controllers []Controller) error {
	if len(controllers) == 0 {
		return nil
	}

	enabled, err := readSubtreeControl(dir)
	if err != nil {
		return err
	}

	var missing []string
	for _, controller := range controllers {
		if !enabled[controller] {
			missing = append(missing, "+"+string(controller))
		}
	}
	if len(missing) == 0 {
		return nil
	}

	if err := writeControlFile(dir, fileSubtreeControl, strings.Join(missing, " ")); err != nil {
		return fmt.Errorf("delegating %s to the children of %s: %w",
			strings.Join(missing, " "), dir, err)
	}

	return nil
}

// readSubtreeControl returns the controllers already delegated by the cgroup at
// dir. A missing file means nothing is delegated yet.
func readSubtreeControl(dir string) (map[Controller]bool, error) {
	content, err := os.ReadFile(filepath.Join(dir, fileSubtreeControl))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return map[Controller]bool{}, nil
		}
		return nil, fmt.Errorf("reading %s: %w", filepath.Join(dir, fileSubtreeControl), translate(err))
	}

	enabled := make(map[Controller]bool)
	for _, name := range strings.Fields(string(content)) {
		enabled[Controller(name)] = true
	}

	return enabled, nil
}

// writeControlFile writes value to a cgroup interface file.
//
// The open flags are deliberate and are the whole reason this is not
// os.WriteFile. That opens O_WRONLY|O_CREATE|O_TRUNC, and neither extra flag
// belongs on kernfs:
//
//   - O_CREATE would ask the kernel to create an interface file. cgroupfs
//     creates them itself when the cgroup is made and refuses to create any
//     more, so at best the flag is ignored and at worst the refusal is
//     reported as the wrong error. Worse, on any path that is *not* cgroupfs
//     it succeeds — writing a limit into a regular file that nothing will ever
//     enforce. Without it, a missing file is reported as what it means: the
//     controller was never delegated to this cgroup.
//   - O_TRUNC is meaningless to a kernfs file, whose contents are generated
//     rather than stored, and some of them reject the truncate outright.
//
// Every cgroup write is a single write(2) of the complete value with no
// trailing newline: the kernel parses one buffer as one command and rejects a
// partial one, so buffered I/O must not be used here either.
func writeControlFile(dir, name, value string) error {
	path := filepath.Join(dir, name)

	f, err := os.OpenFile(path, os.O_WRONLY, 0)
	if err != nil {
		return fmt.Errorf("opening %s to write %q: %w", path, value, translate(err))
	}

	_, writeErr := f.Write([]byte(value))
	closeErr := f.Close()

	if writeErr != nil {
		return fmt.Errorf("writing %q to %s: %w", value, path, translate(writeErr))
	}
	// Some kernfs files report a rejected value at close rather than at write,
	// so this is a real error path, not a formality (SSOT §13.7).
	if closeErr != nil {
		return fmt.Errorf("closing %s after writing %q: %w", path, value, translate(closeErr))
	}

	return nil
}

// translate maps the errors the kernel returns through the filesystem onto this
// package's sentinels, so callers get a usable message rather than "permission
// denied" with no clue which capability is missing.
func translate(err error) error {
	switch {
	case errors.Is(err, os.ErrPermission):
		return fmt.Errorf("%w: %w", ErrPermission, err)
	case errors.Is(err, os.ErrNotExist):
		// Inside a leaf, a missing controller file means the controller was
		// never delegated by the parent's cgroup.subtree_control.
		return fmt.Errorf("%w: %w", ErrControllerUnavailable, err)
	default:
		return err
	}
}

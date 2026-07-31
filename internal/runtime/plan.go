package runtime

import (
	"fmt"

	"github.com/stevenstank/forge/internal/mount"
	"github.com/stevenstank/forge/internal/rootfs"
)

// The default mount set every container with a root filesystem gets.
//
// Deciding *what* a container mounts is policy, and SSOT §2 puts policy here:
// internal/mount is told what to do and never chooses. Each entry below earns
// its place; nothing is here "because Docker does it".
func defaultMounts() []mount.Mount {
	mounts := []mount.Mount{
		{
			// The one that closes Stage 1's headline limitation. A procfs
			// instance is bound to the PID namespace it is mounted in, so this
			// is what finally makes CLONE_NEWPID observable from inside: `ps`
			// in a container stops listing the host's processes.
			Destination: "/proc",
			Type:        mount.TypeProc,
			Options:     []mount.Option{mount.OptionNoSuid, mount.OptionNoDev, mount.OptionNoExec},
		},
		{
			// A minimal root filesystem ships with an empty /dev, and
			// practically every binary opens /dev/null.
			Destination: "/dev",
			Type:        mount.TypeTmpfs,
			Options:     []mount.Option{mount.OptionNoSuid},
			Data:        "mode=755,size=65536k",
		},
		{
			// A private pty instance, required by anything interactive.
			Destination: "/dev/pts",
			Type:        mount.TypeDevpts,
			Options:     []mount.Option{mount.OptionNoSuid, mount.OptionNoExec},
			Data:        "newinstance,ptmxmode=0666,mode=0620",
		},
		{
			// POSIX shared memory, which glibc expects to exist.
			Destination: "/dev/shm",
			Type:        mount.TypeTmpfs,
			Options:     []mount.Option{mount.OptionNoSuid, mount.OptionNoDev},
			Data:        "mode=1777,size=65536k",
		},
		{
			// Read-only until Stage 4 gives the container a network namespace
			// of its own. A writable /sys in the host's network namespace is a
			// hole in the *host's* configuration, not the container's.
			Destination: "/sys",
			Type:        mount.TypeSysfs,
			Options: []mount.Option{
				mount.OptionReadOnly, mount.OptionNoSuid, mount.OptionNoDev, mount.OptionNoExec,
			},
		},
	}

	return append(mounts, defaultDeviceNodes()...)
}

// defaultDeviceNodes returns the device files every container needs.
//
// They are bind mounts of the host's nodes rather than mknod(2) calls: the
// result is identical, and it keeps device-number arithmetic out of Forge for
// no loss of clarity. They mount after /dev, which the ordering in
// mount.Plan.Ordered guarantees — a tmpfs mounted over /dev afterwards would
// hide all of them.
func defaultDeviceNodes() []mount.Mount {
	names := []string{"null", "zero", "full", "random", "urandom", "tty"}

	nodes := make([]mount.Mount, 0, len(names))
	for _, name := range names {
		path := "/dev/" + name
		nodes = append(nodes, mount.Mount{
			Source:      path,
			Destination: path,
			Type:        mount.TypeBind,
			Options:     []mount.Option{mount.OptionNoSuid, mount.OptionNoExec},
		})
	}
	return nodes
}

// mountPlan builds the complete filesystem plan for a container: what its root
// is, where that root's contents come from, and what is mounted inside it.
//
// The caller's mounts come after the defaults, so they read as additions to a
// known base. A collision with a default is refused rather than resolved by
// order — a caller who bind-mounts over /proc has more likely made a mistake
// than a decision, and Stage 2 is not the place to guess which.
func mountPlan(spec Spec, dir rootfs.Dir) (mount.Plan, error) {
	defaults := defaultMounts()

	mounts := make([]mount.Mount, 0, len(defaults)+len(spec.Mounts))
	mounts = append(mounts, defaults...)
	mounts = append(mounts, spec.Mounts...)

	plan := mount.Plan{
		// The container's "/" is the directory Forge owns, not the caller's
		// tree. FR-2.4 requires a per-container root filesystem, and Stage 5
		// replaces the source bind with unpacked image layers without moving
		// the root.
		Source:       spec.Rootfs,
		Root:         dir.Rootfs,
		ReadonlyRoot: spec.ReadonlyRoot,
		Mounts:       mounts,
	}

	if err := plan.Validate(); err != nil {
		return mount.Plan{}, fmt.Errorf("building the mount plan for container %s: %w", dir.ID, err)
	}

	return plan, nil
}

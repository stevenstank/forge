package mount

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
)

// oldRootDirName is where pivot_root(2) parks the old root before it is
// detached. It is dotted and Forge-prefixed to minimise the chance of colliding
// with a real entry in a root filesystem Forge did not build; a collision is
// reported rather than silently reused.
const oldRootDirName = ".forge-oldroot"

// Directory and file permissions for the mount points Apply creates. They are
// mount points, so nothing is ever read from or written to them directly —
// whatever is mounted over them supplies its own permissions.
const (
	mountPointDirPerm  = 0o755
	mountPointFilePerm = 0o644
	oldRootPerm        = 0o700
)

// Apply performs every mount in the plan.
//
// It must be called from the container's init process, after clone(2) has
// placed it in a new mount namespace and that namespace's mount tree has been
// made private. Called anywhere else it would mount onto the host.
//
// Apply does not undo its work on failure, and deliberately so: everything it
// mounts lives in the container's mount namespace, which the kernel destroys —
// along with every mount in it — as soon as the init process exits. A failed
// Apply is followed by exactly that (PRD NFR-8).
func Apply(plan Plan) error {
	if err := plan.Validate(); err != nil {
		return err
	}

	// The container's root must be a mount point before pivot_root(2) will
	// accept it, and this is also what puts the source tree's contents there.
	if err := mountRoot(plan); err != nil {
		return err
	}

	// Created before anything is remounted read-only: on a read-only
	// filesystem there is no creating it later, and pivot_root needs it.
	if err := createOldRootDir(plan.Root); err != nil {
		return err
	}

	for _, m := range plan.Ordered() {
		if err := applyMount(plan.Root, m); err != nil {
			return err
		}
	}

	if plan.ReadonlyRoot {
		if err := remountReadOnly(plan.Root); err != nil {
			return err
		}
	}

	return nil
}

// mountRoot binds the source tree onto the container's root directory.
//
// MS_REC carries the source's own submounts along, so a source tree that spans
// filesystems arrives complete rather than showing empty directories where its
// submounts were.
func mountRoot(plan Plan) error {
	if err := os.MkdirAll(plan.Root, mountPointDirPerm); err != nil {
		return fmt.Errorf("creating the container root %q: %w", plan.Root, err)
	}
	if err := unix.Mount(plan.Source, plan.Root, "", unix.MS_BIND|unix.MS_REC, ""); err != nil {
		return fmt.Errorf("binding %q onto the container root %q: %w",
			plan.Source, plan.Root, translatePermission(err))
	}
	return nil
}

// createOldRootDir creates the directory pivot_root(2) moves the old root to.
func createOldRootDir(root string) error {
	putOld := filepath.Join(root, oldRootDirName)

	if err := os.Mkdir(putOld, oldRootPerm); err != nil {
		if os.IsExist(err) {
			return fmt.Errorf("%w: the root filesystem already contains %q, which forge needs for pivot_root",
				ErrInvalidMountSpec, oldRootDirName)
		}
		return fmt.Errorf("creating %q: %w", putOld, err)
	}
	return nil
}

// applyMount performs a single mount inside the container's root.
func applyMount(root string, m Mount) error {
	destination, err := resolveDestination(root, m.Destination)
	if err != nil {
		return err
	}

	if err := createMountPoint(m, destination); err != nil {
		return err
	}

	if err := unix.Mount(mountSource(m), destination, string(m.Type), m.Flags(), m.Data); err != nil {
		return fmt.Errorf("mounting %s at %q: %w", describe(m), m.Destination, translatePermission(err))
	}

	if !m.NeedsRemount() {
		return nil
	}

	// The first call created the mount but ignored MS_RDONLY and friends,
	// because a bind mount inherits those from the mount it was bound from.
	// This second call is what actually applies them. MS_REC is dropped: it
	// belongs to the bind, not to the remount.
	flags := m.Flags()&^unix.MS_REC | unix.MS_REMOUNT
	if err := unix.Mount("", destination, "", flags, ""); err != nil {
		return fmt.Errorf("applying options %v to %q: %w", m.Options, m.Destination, translatePermission(err))
	}

	return nil
}

// remountReadOnly makes the container's root filesystem read-only. Like any
// bind mount, it needs a second call to take the flag.
func remountReadOnly(root string) error {
	flags := uintptr(unix.MS_BIND | unix.MS_REMOUNT | unix.MS_RDONLY)
	if err := unix.Mount("", root, "", flags, ""); err != nil {
		return fmt.Errorf("remounting the container root %q read-only: %w", root, translatePermission(err))
	}
	return nil
}

// createMountPoint creates whatever the mount needs to land on: a directory for
// most mounts, a file for a bind mount whose source is a file or device node —
// /dev/null being the case every container meets.
func createMountPoint(m Mount, destination string) error {
	wantDir := true
	if m.Type == TypeBind {
		info, err := os.Stat(m.Source)
		if err != nil {
			return fmt.Errorf("inspecting mount source %q: %w", m.Source, err)
		}
		wantDir = info.IsDir()
	}

	existing, err := os.Stat(destination)
	switch {
	case err == nil:
		if existing.IsDir() != wantDir {
			return fmt.Errorf("%w: %q is %s but the source %q is %s",
				ErrNotADirectory, m.Destination, kindOf(existing.IsDir()), m.Source, kindOf(wantDir))
		}
		return nil
	case !os.IsNotExist(err):
		return fmt.Errorf("inspecting mount destination %q: %w", m.Destination, err)
	}

	if wantDir {
		if err := os.MkdirAll(destination, mountPointDirPerm); err != nil {
			return fmt.Errorf("creating mount point %q: %w", m.Destination, err)
		}
		return nil
	}

	if err := os.MkdirAll(filepath.Dir(destination), mountPointDirPerm); err != nil {
		return fmt.Errorf("creating the parent of mount point %q: %w", m.Destination, err)
	}
	file, err := os.OpenFile(destination, os.O_CREATE|os.O_WRONLY, mountPointFilePerm)
	if err != nil {
		return fmt.Errorf("creating mount point %q: %w", m.Destination, err)
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("closing mount point %q: %w", m.Destination, err)
	}

	return nil
}

// mountSource is what mount(2) is given as its source. A bind mount names a
// path; a filesystem with no backing device conventionally names its own type,
// which is what shows up in /proc/self/mountinfo.
func mountSource(m Mount) string {
	if m.Source != "" {
		return m.Source
	}
	if m.Type != TypeBind {
		return string(m.Type)
	}
	return "none"
}

// describe renders a mount for an error message.
func describe(m Mount) string {
	if m.Type == TypeBind {
		return fmt.Sprintf("bind of %q", m.Source)
	}
	return fmt.Sprintf("%s filesystem", m.Type)
}

func kindOf(isDir bool) string {
	if isDir {
		return "a directory"
	}
	return "a file"
}

// PivotRoot makes newRoot the calling process's root filesystem and detaches
// the old one.
//
// It must be called from the container's init process, after Apply. On success
// the process's "/" is newRoot, its working directory is "/", and no mount
// belonging to the old root remains reachable — which is the whole reason
// FR-2.1 demands pivot_root rather than chroot. chroot changes only the calling
// process's root *directory*: the old root stays mounted, still listed in the
// process's own mount table, and reachable by a process with CAP_SYS_CHROOT
// through the classic "mkdir tmp; chroot tmp; chdir(../../..)" walk. After
// pivot_root there is nothing left to walk back to.
func PivotRoot(newRoot string) error {
	newRoot = filepath.Clean(newRoot)

	// The kernel refuses a new root that is not a mount point, with a bare
	// EINVAL that explains nothing. Say what it means instead.
	mounted, err := IsMountPoint(newRoot)
	if err != nil {
		return err
	}
	if !mounted {
		return fmt.Errorf("%w: %q is a plain directory; bind it onto itself first", ErrRootNotMountPoint, newRoot)
	}

	putOld := filepath.Join(newRoot, oldRootDirName)
	if err := os.Mkdir(putOld, oldRootPerm); err != nil && !os.IsExist(err) {
		return fmt.Errorf("creating %q for pivot_root: %w", putOld, err)
	}

	// Entering the new root first means the process holds no reference to the
	// old one when it is detached below.
	if err := unix.Chdir(newRoot); err != nil {
		return fmt.Errorf("entering the new root %q: %w", newRoot, err)
	}

	if err := unix.PivotRoot(newRoot, putOld); err != nil {
		return fmt.Errorf("pivot_root to %q: %w", newRoot, translatePermission(err))
	}

	// "/" now means the new root, and the old one hangs off it.
	if err := unix.Chdir("/"); err != nil {
		return fmt.Errorf("entering the pivoted root: %w", err)
	}

	oldRoot := string(filepath.Separator) + oldRootDirName
	if err := unix.Unmount(oldRoot, unix.MNT_DETACH); err != nil {
		return fmt.Errorf("detaching the old root at %q: %w", oldRoot, translatePermission(err))
	}

	// With the old root gone, the empty directory it hung from is litter in
	// the container's "/". A read-only root filesystem is the one case where
	// it cannot be removed, and there it stays: an empty directory is not
	// worth failing a healthy container over.
	if err := unix.Rmdir(oldRoot); err != nil && !errors.Is(err, unix.EROFS) {
		return fmt.Errorf("removing %q after the pivot: %w", oldRoot, err)
	}

	return nil
}

//go:build integration

package integration

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/stevenstank/forge/internal/logging"
	"github.com/stevenstank/forge/internal/mount"
	"github.com/stevenstank/forge/internal/network"
	"github.com/stevenstank/forge/internal/process"
	"github.com/stevenstank/forge/internal/rootfs"
	"github.com/stevenstank/forge/internal/runtime"
)

// These tests exercise Stage 2 against the real kernel: pivot_root, bind
// mounts, and the promise that a finished container leaves no mount behind.
//
// Every container here runs this test binary from *inside* its own root
// filesystem, which is what makes the assertions meaningful: if the pivot did
// not happen, the binary at /bin/helper would not exist.

// helperBinary is where the sandbox installs the test binary inside the
// container's root filesystem.
const helperBinary = "/bin/helper"

// rootfsMarker is a file that exists only in the container's root filesystem,
// so reading it proves which tree the container is rooted at.
const (
	rootfsMarkerPath    = "/etc/forge-rootfs-marker"
	rootfsMarkerContent = "this file lives in the container rootfs"
)

// Replies from the stat-path helper.
const (
	replyExists  = "exists"
	replyMissing = "missing"
)

// stage2Helper runs the container-side half of a Stage 2 test.
func stage2Helper(mode string) (int, bool) {
	switch mode {
	case "stat-path":
		if _, err := os.Lstat(os.Getenv(helperPathEnv)); err != nil {
			fmt.Println(replyMissing)
			return 0, true
		}
		fmt.Println(replyExists)
		return 0, true

	case "read-file":
		data, err := os.ReadFile(os.Getenv(helperPathEnv))
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1, true
		}
		fmt.Print(string(data))
		return 0, true

	case "write-file":
		err := os.WriteFile(os.Getenv(helperPathEnv), []byte(os.Getenv(helperDataEnv)), 0o600)
		if err != nil {
			// The error text is the assertion: a read-only mount must fail
			// with EROFS, not with a permission error that could come from
			// anywhere.
			fmt.Fprintln(os.Stderr, err)
			return 1, true
		}
		return 0, true

	case "list-dir":
		entries, err := os.ReadDir(os.Getenv(helperPathEnv))
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1, true
		}
		names := make([]string, 0, len(entries))
		for _, e := range entries {
			names = append(names, e.Name())
		}
		slices.Sort(names)
		fmt.Println(strings.Join(names, " "))
		return 0, true

	case "print-cwd":
		cwd, err := os.Getwd()
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1, true
		}
		fmt.Println(cwd)
		return 0, true

	case "print-mountinfo":
		data, err := os.ReadFile("/proc/self/mountinfo")
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1, true
		}
		fmt.Print(string(data))
		return 0, true

	case "count-processes":
		entries, err := os.ReadDir("/proc")
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1, true
		}
		count := 0
		for _, e := range entries {
			if _, err := strconv.Atoi(e.Name()); err == nil {
				count++
			}
		}
		fmt.Println(count)
		return 0, true

	case "mount-tmpfs-and-wait":
		target := os.Getenv(helperPathEnv)
		if err := syscall.Mount("tmpfs", target, "tmpfs", 0, ""); err != nil {
			fmt.Fprintln(os.Stderr, "mount:", err)
			return 1, true
		}
		fmt.Println(readyMarker)
		select {}

	case "pivot-plain-directory":
		// pivot_root(2) refuses a new root that is not a mount point. This
		// helper runs in a Stage 1 container, so it already has a private
		// mount namespace and cannot disturb the host either way.
		dir, err := os.MkdirTemp("", "forge-not-a-mount")
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1, true
		}
		if err := mount.PivotRoot(dir); err != nil {
			fmt.Println(err)
			return 0, true
		}
		fmt.Fprintln(os.Stderr, "PivotRoot succeeded on a plain directory")
		return 1, true

	default:
		return 0, false
	}
}

// --- the sandbox ----------------------------------------------------------

// sandbox is a container's source root filesystem plus the storage root Forge
// prepares containers under. It is built per test so nothing is shared.
type sandbox struct {
	// source is the host directory handed to forge as --rootfs.
	source string
	// root is forge's per-container storage root (--root).
	root string
	// libraries are the read-only binds a dynamically linked test binary needs
	// in order to run inside the rootfs. They double as live coverage of
	// FR-2.2 in every test that uses the sandbox.
	libraries []mount.Mount
}

func newSandbox(t *testing.T) *sandbox {
	t.Helper()

	source := t.TempDir()
	for _, dir := range []string{"bin", "dev", "etc", "proc", "sys", "tmp", "var/log", "data"} {
		if err := os.MkdirAll(filepath.Join(source, dir), 0o755); err != nil {
			t.Fatalf("building the source rootfs: %v", err)
		}
	}

	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable() = %v", err)
	}
	installBinary(t, exe, filepath.Join(source, "bin", "helper"))

	marker := filepath.Join(source, strings.TrimPrefix(rootfsMarkerPath, "/"))
	if err := os.WriteFile(marker, []byte(rootfsMarkerContent), 0o644); err != nil {
		t.Fatalf("writing the rootfs marker: %v", err)
	}

	return &sandbox{
		source:    source,
		root:      filepath.Join(t.TempDir(), "containers"),
		libraries: libraryMounts(t, source),
	}
}

// spec returns a Spec that runs the test binary in the given helper mode,
// inside the sandbox's root filesystem.
func (s *sandbox) spec(mode string, env ...string) runtime.Spec {
	return runtime.Spec{
		Command: []string{helperBinary},
		Rootfs:  s.source,
		Mounts:  slices.Clone(s.libraries),
		Env:     append([]string{helperEnv + "=" + mode}, env...),
		// Host networking, for the reason given on helperSpec: this is a
		// Stage 2 test and asserts nothing about Stage 4.
		Network: network.ModeHost,
	}
}

// run runs a spec in this sandbox's storage root.
func (s *sandbox) run(t *testing.T, spec runtime.Spec) result {
	t.Helper()

	return runContainerIn(t.Context(), t, s.root, spec)
}

// containerDirs returns the per-container directories currently under the
// sandbox's storage root, which is how the cleanup tests see what was left.
func (s *sandbox) containerDirs(t *testing.T) []string {
	t.Helper()

	entries, err := os.ReadDir(s.root)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		t.Fatalf("reading the storage root: %v", err)
	}

	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return names
}

// installBinary places the test binary inside the source rootfs. A hard link
// avoids copying tens of megabytes per test; a copy is the fallback when the
// two paths are on different filesystems.
func installBinary(t *testing.T, from, to string) {
	t.Helper()

	if err := os.Link(from, to); err == nil {
		return
	}

	src, err := os.Open(from)
	if err != nil {
		t.Fatalf("opening the test binary: %v", err)
	}
	defer src.Close()

	dst, err := os.OpenFile(to, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o755)
	if err != nil {
		t.Fatalf("creating %s: %v", to, err)
	}
	if _, err := io.Copy(dst, src); err != nil {
		dst.Close()
		t.Fatalf("copying the test binary: %v", err)
	}
	if err := dst.Close(); err != nil {
		t.Fatalf("closing %s: %v", to, err)
	}
}

// libraryMounts returns read-only binds of the host's shared-library
// directories. A `go test` binary is usually dynamically linked, so without
// them the container's exec fails with "no such file or directory" — the
// dynamic loader, not the binary, is what is missing.
func libraryMounts(t *testing.T, source string) []mount.Mount {
	t.Helper()

	var mounts []mount.Mount
	for _, dir := range []string{"/lib", "/lib64", "/usr/lib", "/usr/lib64"} {
		resolved, err := filepath.EvalSymlinks(dir)
		if err != nil {
			continue // not present on this host
		}
		info, err := os.Stat(resolved)
		if err != nil || !info.IsDir() {
			continue
		}
		if err := os.MkdirAll(filepath.Join(source, strings.TrimPrefix(dir, "/")), 0o755); err != nil {
			t.Fatalf("creating a library mount point: %v", err)
		}
		mounts = append(mounts, mount.Mount{
			Source:      resolved,
			Destination: dir,
			Type:        mount.TypeBind,
			Options:     []mount.Option{mount.OptionReadOnly, mount.OptionRecursive},
		})
	}
	return mounts
}

// hostMountPoints returns the host's mount points, sorted, for before/after
// comparison. The set is compared rather than the raw file because mount IDs
// change whenever anything on the host mounts or unmounts.
func hostMountPoints(t *testing.T) []string {
	t.Helper()

	data, err := os.ReadFile("/proc/self/mountinfo")
	if err != nil {
		t.Fatalf("reading host mountinfo: %v", err)
	}
	return mountPointsFrom(t, string(data))
}

// mountPointsFrom extracts the mount-point field of each mountinfo line.
func mountPointsFrom(t *testing.T, mountinfo string) []string {
	t.Helper()

	var points []string
	for line := range strings.Lines(strings.TrimSpace(mountinfo)) {
		fields := strings.Fields(line)
		if len(fields) < 5 {
			continue
		}
		points = append(points, fields[4])
	}
	slices.Sort(points)
	return points
}

// --- FR-2.1: pivot_root ---------------------------------------------------

// TestPivotRootMakesTheRootfsTheContainerRoot is the headline Stage 2
// behaviour: "/" inside the container is the prepared root filesystem.
func TestPivotRootMakesTheRootfsTheContainerRoot(t *testing.T) {
	requireRoot(t)

	box := newSandbox(t)

	got := box.run(t, box.spec("read-file", helperPathEnv+"="+rootfsMarkerPath))

	if got.status.Code != 0 {
		t.Fatalf("exit code = %d, want 0 (stderr: %s)", got.status.Code, got.stderr)
	}
	if got.stdout != rootfsMarkerContent {
		t.Errorf("read %q from %s, want %q", got.stdout, rootfsMarkerPath, rootfsMarkerContent)
	}
}

// TestContainerRootListsOnlyTheRootfs checks the other direction: "/" contains
// what the source tree contains, and nothing of the host's.
func TestContainerRootListsOnlyTheRootfs(t *testing.T) {
	requireRoot(t)

	box := newSandbox(t)

	got := box.run(t, box.spec("list-dir", helperPathEnv+"=/"))

	if got.status.Code != 0 {
		t.Fatalf("exit code = %d, want 0 (stderr: %s)", got.status.Code, got.stderr)
	}

	entries := strings.Fields(got.stdout)
	for _, want := range []string{"bin", "dev", "etc", "proc", "sys", "tmp"} {
		if !slices.Contains(entries, want) {
			t.Errorf("container / = %v, want it to contain %q", entries, want)
		}
	}

	// The put_old directory is an implementation detail of the pivot and must
	// not survive into the container's view of its own root.
	for _, unwanted := range []string{".forge-oldroot", "root", "boot", "home"} {
		if slices.Contains(entries, unwanted) {
			t.Errorf("container / = %v, want it not to contain %q", entries, unwanted)
		}
	}
}

// TestOldRootIsDetached covers the reason FR-2.1 demands pivot_root rather than
// chroot: after the pivot, the host's filesystem must not be reachable through
// any mount the container can see.
func TestOldRootIsDetached(t *testing.T) {
	requireRoot(t)

	box := newSandbox(t)

	got := box.run(t, box.spec("print-mountinfo"))

	if got.status.Code != 0 {
		t.Fatalf("exit code = %d, want 0 (stderr: %s)", got.status.Code, got.stderr)
	}

	points := mountPointsFrom(t, got.stdout)
	for _, point := range points {
		if strings.Contains(point, ".forge-oldroot") {
			t.Errorf("container mount table still lists the old root at %q", point)
		}
	}
	if slices.Contains(points, box.source) || slices.Contains(points, box.root) {
		t.Errorf("container mount table exposes a host path: %v", points)
	}

	// A container mounts its own root plus the default set and the library
	// binds. The host's full table is very much larger; if the container's
	// table were the host's, this would be obvious.
	if hostCount := len(hostMountPoints(t)); len(points) >= hostCount {
		t.Errorf("container sees %d mounts, host sees %d; the mount table was not replaced", len(points), hostCount)
	}
}

// TestProcIsTheContainersOwn closes the limitation Stage 1 documented: with a
// root filesystem to mount it into, /proc finally reflects the container's PID
// namespace, so `ps` inside a container stops listing host processes.
func TestProcIsTheContainersOwn(t *testing.T) {
	requireRoot(t)

	box := newSandbox(t)

	got := box.run(t, box.spec("count-processes"))

	if got.status.Code != 0 {
		t.Fatalf("exit code = %d, want 0 (stderr: %s)", got.status.Code, got.stderr)
	}

	count, err := strconv.Atoi(got.stdout)
	if err != nil {
		t.Fatalf("process count = %q, want a number (stderr: %s)", got.stdout, got.stderr)
	}
	// The container runs exactly one process: itself, as PID 1.
	if count != 1 {
		t.Errorf("/proc lists %d processes inside the container, want 1", count)
	}
}

// TestPivotRootRequiresAMountPoint pins the kernel precondition that the
// self-bind of the rootfs exists to satisfy. It runs in a Stage 1 container, so
// it has a private mount namespace and cannot affect the host.
func TestPivotRootRequiresAMountPoint(t *testing.T) {
	requireRoot(t)

	got := runContainer(t.Context(), t, helperSpec(t, "pivot-plain-directory"))

	if got.status.Code != 0 {
		t.Fatalf("exit code = %d, want 0 (stderr: %s)", got.status.Code, got.stderr)
	}
	if !strings.Contains(got.stdout, "mount point") {
		t.Errorf("PivotRoot error = %q, want it to explain that the new root must be a mount point", got.stdout)
	}
}

// TestWorkingDirDefaultsToTheContainerRoot documents where a container starts.
func TestWorkingDirDefaultsToTheContainerRoot(t *testing.T) {
	requireRoot(t)

	box := newSandbox(t)

	got := box.run(t, box.spec("print-cwd"))

	if got.stdout != "/" {
		t.Errorf("cwd = %q, want %q", got.stdout, "/")
	}
}

func TestWorkingDirIsHonoured(t *testing.T) {
	requireRoot(t)

	box := newSandbox(t)
	spec := box.spec("print-cwd")
	spec.WorkingDir = "/data"

	got := box.run(t, spec)

	if got.stdout != "/data" {
		t.Errorf("cwd = %q, want %q (stderr: %s)", got.stdout, "/data", got.stderr)
	}
}

// TestMissingWorkingDirFailsToStart keeps a typo'd -workdir from silently
// landing the container in "/".
func TestMissingWorkingDirFailsToStart(t *testing.T) {
	requireRoot(t)

	box := newSandbox(t)
	spec := box.spec("print-cwd")
	spec.WorkingDir = "/nonexistent"

	got := box.run(t, spec)

	if got.status.Code != runtime.InitExitCode {
		t.Errorf("exit code = %d, want %d for a container that could not start", got.status.Code, runtime.InitExitCode)
	}
	if !strings.Contains(got.stderr, "/nonexistent") {
		t.Errorf("stderr = %q, want it to name the missing working directory", got.stderr)
	}
}

// --- host files are hidden ------------------------------------------------

// TestHostFilesAreHidden is the containment assertion: a file that exists on
// the host, at a path the container could name, must not be readable.
func TestHostFilesAreHidden(t *testing.T) {
	requireRoot(t)

	box := newSandbox(t)

	hostFile := filepath.Join(t.TempDir(), "host-only-marker")
	if err := os.WriteFile(hostFile, []byte("host"), 0o644); err != nil {
		t.Fatalf("writing the host marker: %v", err)
	}

	got := box.run(t, box.spec("stat-path", helperPathEnv+"="+hostFile))

	if got.status.Code != 0 {
		t.Fatalf("exit code = %d, want 0 (stderr: %s)", got.status.Code, got.stderr)
	}
	if got.stdout != replyMissing {
		t.Errorf("the container can see the host file %s; the pivot did not isolate the filesystem", hostFile)
	}
}

// TestHostRootCannotBeReachedByClimbingOut is the chroot-escape shape from
// ADR-0001, run against Forge: after pivot_root there is no "up" to walk to,
// because ".." at "/" resolves to "/" and the old root is unmounted.
func TestHostRootCannotBeReachedByClimbingOut(t *testing.T) {
	requireRoot(t)

	box := newSandbox(t)

	hostFile := filepath.Join(t.TempDir(), "host-only-marker")
	if err := os.WriteFile(hostFile, []byte("host"), 0o644); err != nil {
		t.Fatalf("writing the host marker: %v", err)
	}

	for _, climb := range []string{"/..", "/../..", "/../../../../../.."} {
		t.Run(climb, func(t *testing.T) {
			path := filepath.Join(climb, hostFile)

			got := box.run(t, box.spec("stat-path", helperPathEnv+"="+path))

			if got.status.Code != 0 {
				t.Fatalf("exit code = %d, want 0 (stderr: %s)", got.status.Code, got.stderr)
			}
			if got.stdout != replyMissing {
				t.Errorf("the container reached %s by climbing out of its root", hostFile)
			}
		})
	}
}

// TestHostBinariesAreHidden checks a path the container is overwhelmingly
// likely to try: the host's /bin, which the source rootfs does not populate.
func TestHostBinariesAreHidden(t *testing.T) {
	requireRoot(t)

	box := newSandbox(t)

	got := box.run(t, box.spec("list-dir", helperPathEnv+"=/bin"))

	if got.status.Code != 0 {
		t.Fatalf("exit code = %d, want 0 (stderr: %s)", got.status.Code, got.stderr)
	}
	if entries := strings.Fields(got.stdout); len(entries) != 1 || entries[0] != "helper" {
		t.Errorf("container /bin = %v, want only the helper this test installed", entries)
	}
}

// --- FR-2.2: bind mounts --------------------------------------------------

// TestBindMountIsVisibleInContainer is the direct test of FR-2.2.
func TestBindMountIsVisibleInContainer(t *testing.T) {
	requireRoot(t)

	box := newSandbox(t)

	hostDir := t.TempDir()
	const content = "bound from the host"
	if err := os.WriteFile(filepath.Join(hostDir, "file"), []byte(content), 0o644); err != nil {
		t.Fatalf("writing into the host directory: %v", err)
	}

	spec := box.spec("read-file", helperPathEnv+"=/data/file")
	spec.Mounts = append(spec.Mounts, mount.Mount{
		Source: hostDir, Destination: "/data", Type: mount.TypeBind,
	})

	got := box.run(t, spec)

	if got.status.Code != 0 {
		t.Fatalf("exit code = %d, want 0 (stderr: %s)", got.status.Code, got.stderr)
	}
	if got.stdout != content {
		t.Errorf("read %q from the bind mount, want %q", got.stdout, content)
	}
}

// TestBindMountIsWritable confirms a plain bind is read-write, and that the
// write really lands in the host directory rather than in a copy.
func TestBindMountIsWritable(t *testing.T) {
	requireRoot(t)

	box := newSandbox(t)

	hostDir := t.TempDir()
	const content = "written from inside the container"

	spec := box.spec("write-file",
		helperPathEnv+"=/data/written",
		helperDataEnv+"="+content,
	)
	spec.Mounts = append(spec.Mounts, mount.Mount{
		Source: hostDir, Destination: "/data", Type: mount.TypeBind,
	})

	got := box.run(t, spec)

	if got.status.Code != 0 {
		t.Fatalf("exit code = %d, want 0 (stderr: %s)", got.status.Code, got.stderr)
	}

	written, err := os.ReadFile(filepath.Join(hostDir, "written"))
	if err != nil {
		t.Fatalf("reading what the container wrote: %v", err)
	}
	if string(written) != content {
		t.Errorf("host file = %q, want %q", written, content)
	}
}

// TestBindMountDestinationIsCreated covers a destination that does not exist in
// the source rootfs, which is the common case for an arbitrary --mount.
func TestBindMountDestinationIsCreated(t *testing.T) {
	requireRoot(t)

	box := newSandbox(t)

	hostDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(hostDir, "file"), []byte("x"), 0o644); err != nil {
		t.Fatalf("writing into the host directory: %v", err)
	}

	spec := box.spec("stat-path", helperPathEnv+"=/srv/nested/deep/file")
	spec.Mounts = append(spec.Mounts, mount.Mount{
		Source: hostDir, Destination: "/srv/nested/deep", Type: mount.TypeBind,
	})

	got := box.run(t, spec)

	if got.status.Code != 0 {
		t.Fatalf("exit code = %d, want 0 (stderr: %s)", got.status.Code, got.stderr)
	}
	if got.stdout != replyExists {
		t.Errorf("the bind mount at a nonexistent destination is not visible (stderr: %s)", got.stderr)
	}
}

// TestNestedBindMountsAreBothVisible covers the ordering rule: mounting /var
// after /var/log would hide the inner mount.
func TestNestedBindMountsAreBothVisible(t *testing.T) {
	requireRoot(t)

	box := newSandbox(t)

	outer, inner := t.TempDir(), t.TempDir()
	if err := os.MkdirAll(filepath.Join(outer, "log"), 0o755); err != nil {
		t.Fatalf("preparing the outer directory: %v", err)
	}
	if err := os.WriteFile(filepath.Join(inner, "inner-file"), []byte("inner"), 0o644); err != nil {
		t.Fatalf("writing into the inner directory: %v", err)
	}

	spec := box.spec("read-file", helperPathEnv+"=/var/log/inner-file")
	// Deliberately given innermost-first: the plan must reorder them.
	spec.Mounts = append(spec.Mounts,
		mount.Mount{Source: inner, Destination: "/var/log", Type: mount.TypeBind},
		mount.Mount{Source: outer, Destination: "/var", Type: mount.TypeBind},
	)

	got := box.run(t, spec)

	if got.status.Code != 0 {
		t.Fatalf("exit code = %d, want 0 (stderr: %s)", got.status.Code, got.stderr)
	}
	if got.stdout != "inner" {
		t.Errorf("read %q, want %q; the outer mount hid the inner one", got.stdout, "inner")
	}
}

// TestBindMountedFileIsVisible covers binding a single file rather than a
// directory, which needs a file destination rather than a directory one.
func TestBindMountedFileIsVisible(t *testing.T) {
	requireRoot(t)

	box := newSandbox(t)

	hostFile := filepath.Join(t.TempDir(), "resolv.conf")
	const content = "nameserver 127.0.0.53"
	if err := os.WriteFile(hostFile, []byte(content), 0o644); err != nil {
		t.Fatalf("writing the host file: %v", err)
	}

	spec := box.spec("read-file", helperPathEnv+"=/etc/resolv.conf")
	spec.Mounts = append(spec.Mounts, mount.Mount{
		Source: hostFile, Destination: "/etc/resolv.conf", Type: mount.TypeBind,
	})

	got := box.run(t, spec)

	if got.status.Code != 0 {
		t.Fatalf("exit code = %d, want 0 (stderr: %s)", got.status.Code, got.stderr)
	}
	if got.stdout != content {
		t.Errorf("read %q, want %q", got.stdout, content)
	}
}

// --- read-only mounts -----------------------------------------------------

// TestReadOnlyBindMountRejectsWrites is the assertion that a `:ro` suffix means
// something. It fails if the implementation performs only the first mount(2)
// call, because a bind mount silently ignores MS_RDONLY until the remount.
func TestReadOnlyBindMountRejectsWrites(t *testing.T) {
	requireRoot(t)

	box := newSandbox(t)

	hostDir := t.TempDir()

	spec := box.spec("write-file",
		helperPathEnv+"=/data/written",
		helperDataEnv+"=should not reach the host",
	)
	spec.Mounts = append(spec.Mounts, mount.Mount{
		Source: hostDir, Destination: "/data", Type: mount.TypeBind,
		Options: []mount.Option{mount.OptionReadOnly},
	})

	got := box.run(t, spec)

	if got.status.Code == 0 {
		t.Fatalf("the container wrote to a read-only bind mount (exit 0)")
	}
	if !strings.Contains(got.stderr, syscall.EROFS.Error()) {
		t.Errorf("stderr = %q, want it to report %q", got.stderr, syscall.EROFS)
	}

	entries, err := os.ReadDir(hostDir)
	if err != nil {
		t.Fatalf("reading the host directory: %v", err)
	}
	if len(entries) != 0 {
		t.Errorf("host directory contains %d entries, want none: the write reached the host", len(entries))
	}

	// The source is still writable from the host, which proves the refusal
	// came from the mount options rather than from file permissions.
	if err := os.WriteFile(filepath.Join(hostDir, "from-host"), []byte("x"), 0o644); err != nil {
		t.Errorf("the host cannot write to the source directory either: %v", err)
	}
}

// TestReadOnlyBindMountIsStillReadable keeps the read-only mount useful.
func TestReadOnlyBindMountIsStillReadable(t *testing.T) {
	requireRoot(t)

	box := newSandbox(t)

	hostDir := t.TempDir()
	const content = "read me"
	if err := os.WriteFile(filepath.Join(hostDir, "file"), []byte(content), 0o644); err != nil {
		t.Fatalf("writing into the host directory: %v", err)
	}

	spec := box.spec("read-file", helperPathEnv+"=/data/file")
	spec.Mounts = append(spec.Mounts, mount.Mount{
		Source: hostDir, Destination: "/data", Type: mount.TypeBind,
		Options: []mount.Option{mount.OptionReadOnly},
	})

	got := box.run(t, spec)

	if got.stdout != content {
		t.Errorf("read %q from a read-only bind, want %q (stderr: %s)", got.stdout, content, got.stderr)
	}
}

// TestReadOnlyRootfsRejectsWrites covers --read-only.
func TestReadOnlyRootfsRejectsWrites(t *testing.T) {
	requireRoot(t)

	box := newSandbox(t)

	spec := box.spec("write-file",
		helperPathEnv+"=/written-into-the-root",
		helperDataEnv+"=x",
	)
	spec.ReadonlyRoot = true

	got := box.run(t, spec)

	if got.status.Code == 0 {
		t.Fatal("the container wrote to a read-only root filesystem (exit 0)")
	}
	if !strings.Contains(got.stderr, syscall.EROFS.Error()) {
		t.Errorf("stderr = %q, want it to report %q", got.stderr, syscall.EROFS)
	}
	if _, err := os.Stat(filepath.Join(box.source, "written-into-the-root")); err == nil {
		t.Error("the write reached the source tree")
	}
}

// TestWritableBindMountInsideAReadOnlyRootfs is the combination a real
// workload uses: an immutable root with one writable directory.
func TestWritableBindMountInsideAReadOnlyRootfs(t *testing.T) {
	requireRoot(t)

	box := newSandbox(t)

	hostDir := t.TempDir()
	const content = "scratch"

	spec := box.spec("write-file",
		helperPathEnv+"=/data/written",
		helperDataEnv+"="+content,
	)
	spec.ReadonlyRoot = true
	spec.Mounts = append(spec.Mounts, mount.Mount{
		Source: hostDir, Destination: "/data", Type: mount.TypeBind,
	})

	got := box.run(t, spec)

	if got.status.Code != 0 {
		t.Fatalf("exit code = %d, want 0 (stderr: %s)", got.status.Code, got.stderr)
	}
	written, err := os.ReadFile(filepath.Join(hostDir, "written"))
	if err != nil {
		t.Fatalf("reading what the container wrote: %v", err)
	}
	if string(written) != content {
		t.Errorf("host file = %q, want %q", written, content)
	}
}

// --- FR-2.3: cleanup ------------------------------------------------------

// TestNoHostMountResidueAfterRun is the strongest single assertion in Stage 2:
// running a container with a full mount set leaves the host's mount table
// exactly as it was.
func TestNoHostMountResidueAfterRun(t *testing.T) {
	requireRoot(t)

	box := newSandbox(t)

	hostDir := t.TempDir()
	before := hostMountPoints(t)

	spec := box.spec("read-file", helperPathEnv+"="+rootfsMarkerPath)
	spec.Mounts = append(spec.Mounts, mount.Mount{
		Source: hostDir, Destination: "/data", Type: mount.TypeBind,
		Options: []mount.Option{mount.OptionReadOnly},
	})

	if got := box.run(t, spec); got.status.Code != 0 {
		t.Fatalf("exit code = %d, want 0 (stderr: %s)", got.status.Code, got.stderr)
	}

	after := hostMountPoints(t)
	if strings.Join(before, "\n") != strings.Join(after, "\n") {
		t.Errorf("host mount table changed across the run:\nbefore:\n%s\nafter:\n%s",
			strings.Join(before, "\n"), strings.Join(after, "\n"))
	}
	for _, point := range after {
		if strings.HasPrefix(point, box.root) {
			t.Errorf("host mount table still contains %q under forge's storage root", point)
		}
	}
}

// TestContainerDirectoryIsRemovedAfterRun covers the FR-2.4 half of cleanup:
// the per-container directory does not outlive the container.
func TestContainerDirectoryIsRemovedAfterRun(t *testing.T) {
	requireRoot(t)

	box := newSandbox(t)

	if got := box.run(t, box.spec("read-file", helperPathEnv+"="+rootfsMarkerPath)); got.status.Code != 0 {
		t.Fatalf("exit code = %d, want 0 (stderr: %s)", got.status.Code, got.stderr)
	}

	if dirs := box.containerDirs(t); len(dirs) != 0 {
		t.Errorf("storage root contains %v after the run, want it empty", dirs)
	}
}

// TestContainerDirectoryIsRemovedWhenTheContainerFails covers the same promise
// on the failure path, which is where cleanup is usually forgotten.
func TestContainerDirectoryIsRemovedWhenTheContainerFails(t *testing.T) {
	requireRoot(t)

	box := newSandbox(t)

	spec := box.spec("read-file", helperPathEnv+"="+rootfsMarkerPath)
	spec.Command = []string{"/bin/nonexistent-binary"}

	got := box.run(t, spec)
	if got.status.Code != runtime.InitExitCode {
		t.Fatalf("exit code = %d, want %d", got.status.Code, runtime.InitExitCode)
	}
	if dirs := box.containerDirs(t); len(dirs) != 0 {
		t.Errorf("storage root contains %v after a failed run, want it empty", dirs)
	}
}

// TestMountsDieWithTheNamespace is the structural claim behind Stage 2's
// cleanup design: mounts made inside the container's mount namespace are the
// kernel's to release, so even a SIGKILLed container leaves nothing behind.
func TestMountsDieWithTheNamespace(t *testing.T) {
	requireRoot(t)

	box := newSandbox(t)
	before := hostMountPoints(t)

	stdoutReader, stdoutWriter := io.Pipe()
	defer stdoutReader.Close()

	spec := box.spec("mount-tmpfs-and-wait", helperPathEnv+"=/tmp")
	spec.Stdout = stdoutWriter

	var logs bytes.Buffer

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	runner, err := runtime.NewRunner(logging.New(&logs, slog.LevelDebug), runtime.Config{Root: box.root})
	if err != nil {
		t.Fatalf("NewRunner() = %v", err)
	}

	type outcome struct {
		status process.Status
		err    error
	}
	done := make(chan outcome, 1)

	go func() {
		status, err := runner.Run(ctx, spec)
		_ = stdoutWriter.Close()
		done <- outcome{status, err}
	}()

	ready := make(chan struct{})
	go func() {
		line, err := bufio.NewReader(stdoutReader).ReadString('\n')
		if err == nil && strings.TrimSpace(line) == readyMarker {
			close(ready)
		}
	}()

	select {
	case <-ready:
	case <-time.After(30 * time.Second):
		t.Fatal("container never signalled readiness")
	}

	cancel()

	select {
	case got := <-done:
		if got.err != nil {
			t.Fatalf("Run() = %v (log: %s)", got.err, logs.String())
		}
		if got.status.Signal != syscall.SIGKILL {
			t.Errorf("Signal = %v, want SIGKILL after cancellation", got.status.Signal)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("Run did not return after its context was cancelled")
	}

	if after := hostMountPoints(t); strings.Join(before, "\n") != strings.Join(after, "\n") {
		t.Errorf("host mount table changed after a killed container:\nbefore:\n%s\nafter:\n%s",
			strings.Join(before, "\n"), strings.Join(after, "\n"))
	}
	if dirs := box.containerDirs(t); len(dirs) != 0 {
		t.Errorf("storage root contains %v after a killed container, want it empty", dirs)
	}
}

// TestCleanupUnmountsEverythingUnderADirectory exercises the reconciliation
// path directly. Nothing in the normal flow creates host-side mounts, so this
// builds the residue a crashed Forge would leave and asks Cleanup to remove it.
func TestCleanupUnmountsEverythingUnderADirectory(t *testing.T) {
	requireRoot(t)

	base := t.TempDir()
	source := t.TempDir()

	// A nested stack, so Cleanup has to unmount deepest-first.
	nested := []string{"rootfs", "rootfs/data", "rootfs/data/inner"}
	for _, rel := range nested {
		target := filepath.Join(base, rel)
		if err := os.MkdirAll(target, 0o755); err != nil {
			t.Fatalf("creating %s: %v", target, err)
		}
		if err := syscall.Mount(source, target, "", syscall.MS_BIND, ""); err != nil {
			t.Fatalf("binding %s: %v", target, err)
		}
	}
	// Belt and braces: if the test fails before Cleanup runs, do not leave the
	// host with mounts (SSOT §7).
	t.Cleanup(func() {
		for i := len(nested) - 1; i >= 0; i-- {
			_ = syscall.Unmount(filepath.Join(base, nested[i]), syscall.MNT_DETACH)
		}
	})

	if err := mount.Cleanup(t.Context(), base); err != nil {
		t.Fatalf("Cleanup(%q) = %v", base, err)
	}

	for _, point := range hostMountPoints(t) {
		if strings.HasPrefix(point, base) {
			t.Errorf("%q is still mounted after Cleanup", point)
		}
	}
}

// TestCleanupIsIdempotent covers SSOT §13.3: cleanup runs on paths where it may
// already have run, and on directories that were never mounted at all.
func TestCleanupIsIdempotent(t *testing.T) {
	requireRoot(t)

	base := t.TempDir()
	target := filepath.Join(base, "rootfs")
	if err := os.MkdirAll(target, 0o755); err != nil {
		t.Fatalf("creating %s: %v", target, err)
	}
	if err := syscall.Mount(t.TempDir(), target, "", syscall.MS_BIND, ""); err != nil {
		t.Fatalf("binding %s: %v", target, err)
	}
	t.Cleanup(func() { _ = syscall.Unmount(target, syscall.MNT_DETACH) })

	for i := range 3 {
		if err := mount.Cleanup(t.Context(), base); err != nil {
			t.Fatalf("Cleanup() call %d = %v, want nil", i+1, err)
		}
	}

	if err := mount.Cleanup(t.Context(), filepath.Join(t.TempDir(), "never-existed")); err != nil {
		t.Errorf("Cleanup(nonexistent) = %v, want nil", err)
	}
}

// TestStoreRemoveRefusesToDeleteThroughAMount is the test that exists because
// the failure it guards against is unrecoverable: os.RemoveAll walking through
// a live bind mount deletes the host's files, not the container's.
func TestStoreRemoveRefusesToDeleteThroughAMount(t *testing.T) {
	requireRoot(t)

	box := newSandbox(t)

	store, err := rootfs.NewStore(box.root, logging.New(io.Discard, slog.LevelError))
	if err != nil {
		t.Fatalf("NewStore() = %v", err)
	}

	dir, err := store.Prepare("a1b2c3d4e5f6")
	if err != nil {
		t.Fatalf("Prepare() = %v", err)
	}

	// A host directory with a precious file in it, bind-mounted under the
	// container's tree the way a crashed run might have left it.
	hostDir := t.TempDir()
	precious := filepath.Join(hostDir, "precious")
	if err := os.WriteFile(precious, []byte("host data"), 0o644); err != nil {
		t.Fatalf("writing the host file: %v", err)
	}

	target := filepath.Join(dir.Rootfs, "data")
	if err := os.MkdirAll(target, 0o755); err != nil {
		t.Fatalf("creating %s: %v", target, err)
	}
	if err := syscall.Mount(hostDir, target, "", syscall.MS_BIND, ""); err != nil {
		t.Fatalf("binding %s: %v", target, err)
	}
	mounted := true
	t.Cleanup(func() {
		if mounted {
			_ = syscall.Unmount(target, syscall.MNT_DETACH)
		}
	})

	if err := store.Remove(t.Context(), dir.ID); !errors.Is(err, rootfs.ErrStillMounted) {
		t.Fatalf("Remove() = %v, want %v", err, rootfs.ErrStillMounted)
	}
	if _, err := os.Stat(precious); err != nil {
		t.Fatalf("the host file was deleted through the bind mount: %v", err)
	}

	// Once the mount is gone, removal proceeds — and still leaves the host
	// directory alone.
	if err := mount.Cleanup(t.Context(), dir.Base); err != nil {
		t.Fatalf("Cleanup() = %v", err)
	}
	mounted = false

	if err := store.Remove(t.Context(), dir.ID); err != nil {
		t.Fatalf("Remove() after Cleanup = %v", err)
	}
	if _, err := os.Stat(precious); err != nil {
		t.Errorf("the host file was deleted after the mount was released: %v", err)
	}
	if _, err := os.Stat(dir.Base); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("stat %s = %v, want the container tree to be gone", dir.Base, err)
	}
}

// --- FR-2.4: per-container root filesystems -------------------------------

// TestContainersDoNotShareARootfsDirectory covers FR-2.4: each container gets
// its own directory, named by its own ID.
func TestContainersDoNotShareARootfsDirectory(t *testing.T) {
	requireRoot(t)

	box := newSandbox(t)

	// Two runs in the same storage root; each must clean up after itself, so
	// the observable evidence is that both succeed and nothing is left.
	for range 2 {
		got := box.run(t, box.spec("read-file", helperPathEnv+"="+rootfsMarkerPath))
		if got.status.Code != 0 {
			t.Fatalf("exit code = %d, want 0 (stderr: %s)", got.status.Code, got.stderr)
		}
	}
	if dirs := box.containerDirs(t); len(dirs) != 0 {
		t.Errorf("storage root contains %v, want it empty", dirs)
	}
}

// TestRootfsDirectoryLivesUnderTheConfiguredRoot pins the layout the design
// documents, observed from outside: while a container runs, its rootfs is at
// <root>/<id>/rootfs. The container itself reports the ID through its hostname,
// which Forge defaults to the container ID.
func TestRootfsDirectoryLivesUnderTheConfiguredRoot(t *testing.T) {
	requireRoot(t)

	box := newSandbox(t)

	got := box.run(t, box.spec("stat-path", helperPathEnv+"=/proc/self/root"))
	if got.status.Code != 0 {
		t.Fatalf("exit code = %d, want 0 (stderr: %s)", got.status.Code, got.stderr)
	}

	// The storage root itself is created and kept; only per-container trees
	// come and go.
	info, err := os.Stat(box.root)
	if err != nil {
		t.Fatalf("stat %s: %v", box.root, err)
	}
	if !info.IsDir() {
		t.Errorf("%s is not a directory", box.root)
	}
}

// --- Stage 1 regression ---------------------------------------------------

// TestStage1BehaviourWithoutARootfs is the regression signal for the whole
// stage: with no rootfs requested, a container still runs against the host's
// filesystem exactly as it did in Stage 1.
func TestStage1BehaviourWithoutARootfs(t *testing.T) {
	requireRoot(t)

	hostFile := filepath.Join(t.TempDir(), "host-marker")
	if err := os.WriteFile(hostFile, []byte("host"), 0o644); err != nil {
		t.Fatalf("writing the host marker: %v", err)
	}

	got := runContainer(t.Context(), t, helperSpec(t, "stat-path", helperPathEnv+"="+hostFile))

	if got.status.Code != 0 {
		t.Fatalf("exit code = %d, want 0 (stderr: %s)", got.status.Code, got.stderr)
	}
	if got.stdout != replyExists {
		t.Errorf("a container without a rootfs cannot see %s; stage 1 behaviour regressed", hostFile)
	}
}

// TestNoRootfsCreatesNoContainerDirectory confirms Stage 1 containers do not
// pay for Stage 2: with nothing to prepare, nothing is created on disk.
func TestNoRootfsCreatesNoContainerDirectory(t *testing.T) {
	requireRoot(t)

	root := filepath.Join(t.TempDir(), "containers")

	got := runContainerIn(t.Context(), t, root, helperSpec(t, "print-cwd"))
	if got.status.Code != 0 {
		t.Fatalf("exit code = %d, want 0 (stderr: %s)", got.status.Code, got.stderr)
	}

	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatalf("reading the storage root: %v", err)
	}
	if len(entries) != 0 {
		t.Errorf("storage root contains %d entries, want none", len(entries))
	}
}

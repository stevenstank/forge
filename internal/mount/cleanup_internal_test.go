package mount

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"golang.org/x/sys/unix"
)

// The reconciliation half of Stage 2: reading the kernel's mount table and
// unmounting what Forge left in it.
//
// Making a mount needs CAP_SYS_ADMIN and belongs to the privileged suite.
// Reading /proc/self/mountinfo does not, and neither does asking Cleanup to
// unmount a directory that has nothing under it — which is the idempotence
// SSOT §13.3 requires of it and the case every cleanup stack relies on. Both
// are exercised here against the real kernel rather than a fixture; only the
// escape decoding, which the kernel applies to paths a test cannot create
// without root, is driven from crafted input.

// TestUnescapeMountPoint covers the octal escaping the kernel applies to mount
// points containing characters that would otherwise break mountinfo's
// whitespace-separated fields.
func TestUnescapeMountPoint(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		field string
		want  string
	}{
		{name: "an ordinary path is returned untouched", field: "/var/lib/forge", want: "/var/lib/forge"},
		{name: "an empty field", field: "", want: ""},
		{name: "a space", field: `/mnt/my\040disk`, want: "/mnt/my disk"},
		{name: "a tab", field: `/mnt/a\011b`, want: "/mnt/a\tb"},
		{name: "a newline", field: `/mnt/a\012b`, want: "/mnt/a\nb"},
		{name: "a literal backslash", field: `/mnt/a\134b`, want: `/mnt/a\b`},
		{name: "several escapes", field: `/mnt/a\040b\040c`, want: "/mnt/a b c"},
		{name: "an escape at the end", field: `/mnt/x\040`, want: "/mnt/x "},
		// A backslash the kernel did not write is left alone rather than
		// guessed at: mangling it would rename a directory that exists.
		{name: "a truncated escape", field: `/mnt/x\04`, want: `/mnt/x\04`},
		{name: "a trailing backslash", field: `/mnt/x\`, want: `/mnt/x\`},
		{name: "a non-octal escape", field: `/mnt/x\09z`, want: `/mnt/x\09z`},
		{name: "a value above a byte", field: `/mnt/x\777z`, want: `/mnt/x\777z`},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			if got := unescapeMountPoint(tc.field); got != tc.want {
				t.Errorf("unescapeMountPoint(%q) = %q, want %q", tc.field, got, tc.want)
			}
		})
	}
}

// TestMountPointsReadsTheRealTable checks the parser against the kernel's own
// output. Every Linux process has at least "/" mounted, and the paths are
// absolute and free of the escapes the decoder has just removed.
func TestMountPointsReadsTheRealTable(t *testing.T) {
	t.Parallel()

	points, err := mountPoints()
	if err != nil {
		t.Fatalf("mountPoints() = %v", err)
	}
	if len(points) == 0 {
		t.Fatal("mountPoints() found nothing; every process has at least /")
	}
	if !slices.Contains(points, "/") {
		t.Errorf("mountPoints() does not include /: %v", points)
	}
	for _, point := range points {
		if !filepath.IsAbs(point) {
			t.Errorf("mount point %q is not absolute", point)
		}
		if strings.Contains(point, `\0`) {
			t.Errorf("mount point %q still carries an octal escape", point)
		}
	}
}

// TestIsMountPoint asks the question Stage 2 asks before pivot_root: is this
// directory a mount point in my namespace?
func TestIsMountPoint(t *testing.T) {
	t.Parallel()

	t.Run("the root is one", func(t *testing.T) {
		t.Parallel()

		is, err := IsMountPoint("/")
		if err != nil {
			t.Fatalf("IsMountPoint(/) = %v", err)
		}
		if !is {
			t.Error("IsMountPoint(/) = false; / is always a mount point")
		}
	})

	t.Run("a fresh temporary directory is not", func(t *testing.T) {
		t.Parallel()

		dir := t.TempDir()

		is, err := IsMountPoint(dir)
		if err != nil {
			t.Fatalf("IsMountPoint(%s) = %v", dir, err)
		}
		if is {
			t.Errorf("IsMountPoint(%s) = true; nothing is mounted there", dir)
		}
	})

	t.Run("a path that does not exist is not", func(t *testing.T) {
		t.Parallel()

		is, err := IsMountPoint(filepath.Join(t.TempDir(), "absent"))
		if err != nil {
			t.Fatalf("IsMountPoint() = %v", err)
		}
		if is {
			t.Error("IsMountPoint() = true for a path that does not exist")
		}
	})

	t.Run("a trailing slash does not change the answer", func(t *testing.T) {
		t.Parallel()

		// The answer must not depend on how the caller spelled the path: the
		// table holds cleaned paths, so IsMountPoint has to clean its argument
		// too or "/proc/" would never be recognised.
		clean, err := IsMountPoint("/proc")
		if err != nil {
			t.Fatalf("IsMountPoint(/proc) = %v", err)
		}
		spelled, err := IsMountPoint("/proc/")
		if err != nil {
			t.Fatalf("IsMountPoint(/proc/) = %v", err)
		}
		if clean != spelled {
			t.Errorf("IsMountPoint(/proc) = %t but IsMountPoint(/proc/) = %t", clean, spelled)
		}
	})
}

// TestMountPointsUnder checks the prefix matching that decides what Cleanup
// will try to unmount. Getting it wrong in the other direction is the
// dangerous case: "/var/lib/forge" must not match "/var/lib/forge-other".
func TestMountPointsUnder(t *testing.T) {
	t.Parallel()

	t.Run("a directory with nothing mounted under it", func(t *testing.T) {
		t.Parallel()

		points, err := mountPointsUnder(t.TempDir())
		if err != nil {
			t.Fatalf("mountPointsUnder() = %v", err)
		}
		if len(points) != 0 {
			t.Errorf("mountPointsUnder() = %v, want none", points)
		}
	})

	t.Run("the root matches everything", func(t *testing.T) {
		t.Parallel()

		all, err := mountPoints()
		if err != nil {
			t.Fatalf("mountPoints() = %v", err)
		}
		under, err := mountPointsUnder("/")
		if err != nil {
			t.Fatalf("mountPointsUnder(/) = %v", err)
		}
		if len(under) != len(all) {
			t.Errorf("mountPointsUnder(/) found %d of %d mount points", len(under), len(all))
		}
	})

	t.Run("a sibling with a shared prefix is not under it", func(t *testing.T) {
		t.Parallel()

		// /proc is a mount point on every Linux host; /pro is not a directory
		// at all, and must not collect /proc by string prefix alone.
		under, err := mountPointsUnder("/pro")
		if err != nil {
			t.Fatalf("mountPointsUnder(/pro) = %v", err)
		}
		if slices.Contains(under, "/proc") {
			t.Errorf("mountPointsUnder(/pro) = %v, which swept in /proc", under)
		}
	})

	t.Run("a mount point is under itself", func(t *testing.T) {
		t.Parallel()

		under, err := mountPointsUnder("/proc")
		if err != nil {
			t.Fatalf("mountPointsUnder(/proc) = %v", err)
		}
		if !slices.Contains(under, "/proc") {
			t.Errorf("mountPointsUnder(/proc) = %v, want it to include /proc itself", under)
		}
	})
}

// TestCleanupOnADirectoryWithNoMounts is the idempotence SSOT §13.3 requires:
// every cleanup stack calls this unconditionally, including on the paths where
// nothing was ever mounted.
func TestCleanupOnADirectoryWithNoMounts(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()

	for i := range 3 {
		if err := Cleanup(t.Context(), dir); err != nil {
			t.Fatalf("Cleanup() on pass %d = %v, want nil", i, err)
		}
	}

	// The directory itself is not touched: Cleanup unmounts, it does not
	// delete, and rootfs.Store.Remove depends on that division.
	if _, err := os.Stat(dir); err != nil {
		t.Errorf("Cleanup removed the directory: %v", err)
	}
}

// TestCleanupOnAMissingDirectory checks the other half of idempotence: a
// container directory that has already been deleted.
func TestCleanupOnAMissingDirectory(t *testing.T) {
	t.Parallel()

	if err := Cleanup(t.Context(), filepath.Join(t.TempDir(), "gone")); err != nil {
		t.Errorf("Cleanup() on a missing directory = %v, want nil", err)
	}
}

// TestCleanupHonoursCancellation checks that a cancelled context stops the
// pass loop rather than being ignored, which is what lets a `forge rm` be
// interrupted.
func TestCleanupHonoursCancellation(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	err := Cleanup(ctx, t.TempDir())
	if !errors.Is(err, context.Canceled) {
		t.Errorf("Cleanup() with a cancelled context = %v, want context.Canceled", err)
	}
}

// TestTranslatePermission checks the sentinel swap that turns a bare EPERM
// from a syscall the user never named into an actionable message.
func TestTranslatePermission(t *testing.T) {
	t.Parallel()

	if got := translatePermission(unix.EPERM); !errors.Is(got, ErrPermission) {
		t.Errorf("translatePermission(EPERM) = %v, want ErrPermission", got)
	}
	if got := translatePermission(fmtWrapped(unix.EPERM)); !errors.Is(got, ErrPermission) {
		t.Errorf("translatePermission(wrapped EPERM) = %v, want ErrPermission", got)
	}

	// Everything else is passed through unchanged, so the caller still sees
	// which syscall failed and why.
	other := unix.ENOENT
	if got := translatePermission(other); !errors.Is(got, unix.ENOENT) {
		t.Errorf("translatePermission(ENOENT) = %v, want it left alone", got)
	}
	if got := translatePermission(nil); got != nil {
		t.Errorf("translatePermission(nil) = %v, want nil", got)
	}
}

// fmtWrapped returns err inside another error, as a syscall failure reaches
// translatePermission from the middle of a call chain.
func fmtWrapped(err error) error {
	return &wrappedError{err: err}
}

type wrappedError struct{ err error }

func (e *wrappedError) Error() string { return "mounting: " + e.err.Error() }
func (e *wrappedError) Unwrap() error { return e.err }

// TestCheckNoEscape covers the cheap containment check by itself. The
// symlink-following half is resolveDestination's, and is tested in
// containment_internal_test.go.
func TestCheckNoEscape(t *testing.T) {
	t.Parallel()

	tests := []struct {
		path string
		want bool
	}{
		{path: "/etc/hosts", want: true},
		{path: "/", want: true},
		{path: "", want: true},
		{path: "/a/b/../c", want: true},
		{path: "/a/./b", want: true},
		{path: "/a/b/../../c", want: true},
		{path: "/..", want: false},
		{path: "/a/../..", want: false},
		{path: "/a/b/../../../c", want: false},
		{path: "../etc", want: false},
	}

	for _, tc := range tests {
		t.Run(tc.path, func(t *testing.T) {
			t.Parallel()

			err := checkNoEscape(tc.path)
			if tc.want && err != nil {
				t.Errorf("checkNoEscape(%q) = %v, want nil", tc.path, err)
			}
			if !tc.want && !errors.Is(err, ErrEscapesRoot) {
				t.Errorf("checkNoEscape(%q) = %v, want ErrEscapesRoot", tc.path, err)
			}
		})
	}
}

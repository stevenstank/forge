package rootfs

import (
	"io"
	"log/slog"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/stevenstank/forge/internal/logging"
)

// The mount-table guard behind Store.Remove.
//
// Store.Remove refuses to delete a tree with anything mounted under it, and the
// tests in store_internal_test.go drive that refusal through the injected seam.
// What is left is the production implementation of the seam itself: the parser
// that reads the kernel's table, and the escape decoding it applies. Creating a
// mount to read back needs CAP_SYS_ADMIN and belongs to the privileged suite;
// reading the table this process already has does not.

// TestMountsUnderReadsTheRealTable checks the parser against the kernel's own
// mountinfo rather than a fixture.
func TestMountsUnderReadsTheRealTable(t *testing.T) {
	t.Parallel()

	t.Run("the root collects every mount", func(t *testing.T) {
		t.Parallel()

		points, err := mountsUnder("/")
		if err != nil {
			t.Fatalf("mountsUnder(/) = %v", err)
		}
		if len(points) == 0 {
			t.Fatal("mountsUnder(/) found nothing; every process has at least /")
		}
		if !slices.Contains(points, "/") {
			t.Errorf("mountsUnder(/) = %v, want it to include / itself", points)
		}
		for _, point := range points {
			if !filepath.IsAbs(point) {
				t.Errorf("mount point %q is not absolute", point)
			}
		}
	})

	t.Run("a fresh directory has nothing under it", func(t *testing.T) {
		t.Parallel()

		points, err := mountsUnder(t.TempDir())
		if err != nil {
			t.Fatalf("mountsUnder() = %v", err)
		}
		if len(points) != 0 {
			t.Errorf("mountsUnder() = %v, want none", points)
		}
	})

	t.Run("a mount point is under itself", func(t *testing.T) {
		t.Parallel()

		points, err := mountsUnder("/proc")
		if err != nil {
			t.Fatalf("mountsUnder(/proc) = %v", err)
		}
		if !slices.Contains(points, "/proc") {
			t.Errorf("mountsUnder(/proc) = %v, want it to include /proc", points)
		}
	})

	t.Run("a sibling sharing a prefix is not under it", func(t *testing.T) {
		t.Parallel()

		// This is the direction that matters. A prefix match without the
		// separator would let a container directory named "forge" claim the
		// mounts of a sibling named "forge-other", and Remove would then refuse
		// to delete a tree that is perfectly safe — or, in the mirror case,
		// believe a mounted tree was clear.
		points, err := mountsUnder("/pro")
		if err != nil {
			t.Fatalf("mountsUnder(/pro) = %v", err)
		}
		if slices.Contains(points, "/proc") {
			t.Errorf("mountsUnder(/pro) = %v, which swept in /proc", points)
		}
	})
}

// TestUnescapeMountPoint covers the octal escaping the kernel writes into
// mountinfo for characters that would otherwise break its field separation.
//
// A container directory the user named with a space in it is the realistic
// case: without this, its mount would be read as a different path and the
// guard that stops Remove deleting through a live bind mount would miss it.
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
		{name: "a truncated escape is left alone", field: `/mnt/x\04`, want: `/mnt/x\04`},
		{name: "a trailing backslash is left alone", field: `/mnt/x\`, want: `/mnt/x\`},
		{name: "a non-octal escape is left alone", field: `/mnt/x\09z`, want: `/mnt/x\09z`},
		{name: "a value that does not fit a byte", field: `/mnt/x\777z`, want: `/mnt/x\777z`},
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

// TestStoreRootReportsWhereItWrites pins the accessor `forge rm` and the
// runtime use to name a container's directory in an error.
func TestStoreRootReportsWhereItWrites(t *testing.T) {
	t.Parallel()

	root := filepath.Join(t.TempDir(), "containers")

	store, err := NewStore(root, logging.New(io.Discard, slog.LevelError))
	if err != nil {
		t.Fatalf("NewStore() = %v", err)
	}
	if got := store.Root(); got != root {
		t.Errorf("Root() = %q, want %q", got, root)
	}

	// A path with a trailing separator names the same directory, and the store
	// must report it the way every other path in Forge is spelled.
	store, err = NewStore(root+string(filepath.Separator), logging.New(io.Discard, slog.LevelError))
	if err != nil {
		t.Fatalf("NewStore() = %v", err)
	}
	if got := store.Root(); strings.HasSuffix(got, string(filepath.Separator)) {
		t.Errorf("Root() = %q, want it cleaned", got)
	}
}

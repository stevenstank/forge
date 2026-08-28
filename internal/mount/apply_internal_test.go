package mount

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The parts of Apply that are not mount(2).
//
// mount(2), umount2(2) and pivot_root(2) all need CAP_SYS_ADMIN, so Apply and
// PivotRoot themselves belong to the privileged suite. What runs before each
// syscall does not: deciding what to create for a mount to land on, deciding
// what to pass as its source, and describing it in an error are all ordinary
// filesystem work and pure functions, and each of them can be wrong in a way
// that produces a confusing failure much later.

// TestCreateMountPointMakesADirectory covers the ordinary case: a container
// path that does not exist yet, for a mount that needs a directory.
func TestCreateMountPointMakesADirectory(t *testing.T) {
	t.Parallel()

	dest := filepath.Join(t.TempDir(), "nested", "proc")

	if err := createMountPoint(Mount{Type: TypeProc, Destination: "/proc"}, dest); err != nil {
		t.Fatalf("createMountPoint() = %v", err)
	}

	info, err := os.Stat(dest)
	if err != nil {
		t.Fatalf("stat %s = %v", dest, err)
	}
	if !info.IsDir() {
		t.Errorf("%s is not a directory", dest)
	}
}

// TestCreateMountPointMakesAFileForAFileSource is the case every container
// meets: a bind of /dev/null needs a file to land on, not a directory.
func TestCreateMountPointMakesAFileForAFileSource(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	source := filepath.Join(root, "source-file")
	if err := os.WriteFile(source, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	dest := filepath.Join(root, "container", "dev", "null")

	m := Mount{Type: TypeBind, Source: source, Destination: "/dev/null"}
	if err := createMountPoint(m, dest); err != nil {
		t.Fatalf("createMountPoint() = %v", err)
	}

	info, err := os.Stat(dest)
	if err != nil {
		t.Fatalf("stat %s = %v", dest, err)
	}
	if info.IsDir() {
		t.Errorf("%s is a directory; a file source needs a file to bind onto", dest)
	}
}

// TestCreateMountPointIsIdempotent covers a destination that already exists and
// is of the right kind, which is every default mount on a second run.
func TestCreateMountPointIsIdempotent(t *testing.T) {
	t.Parallel()

	dest := filepath.Join(t.TempDir(), "proc")
	m := Mount{Type: TypeProc, Destination: "/proc"}

	for i := range 2 {
		if err := createMountPoint(m, dest); err != nil {
			t.Fatalf("createMountPoint() on pass %d = %v", i, err)
		}
	}
}

// TestCreateMountPointRefusesAKindMismatch is the failure worth naming: a
// container image with a directory where a file must be bound produces an
// EINVAL from mount(2) that says nothing. This says which path and which way
// round.
func TestCreateMountPointRefusesAKindMismatch(t *testing.T) {
	t.Parallel()

	root := t.TempDir()

	fileSource := filepath.Join(root, "file-source")
	if err := os.WriteFile(fileSource, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	dirSource := filepath.Join(root, "dir-source")
	if err := os.Mkdir(dirSource, 0o755); err != nil {
		t.Fatal(err)
	}

	existingDir := filepath.Join(root, "dest-dir")
	if err := os.Mkdir(existingDir, 0o755); err != nil {
		t.Fatal(err)
	}
	existingFile := filepath.Join(root, "dest-file")
	if err := os.WriteFile(existingFile, nil, 0o600); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name string
		m    Mount
		dest string
	}{
		{
			name: "a file source onto an existing directory",
			m:    Mount{Type: TypeBind, Source: fileSource, Destination: "/dev/null"},
			dest: existingDir,
		},
		{
			name: "a directory source onto an existing file",
			m:    Mount{Type: TypeBind, Source: dirSource, Destination: "/data"},
			dest: existingFile,
		},
		{
			name: "a filesystem mount onto an existing file",
			m:    Mount{Type: TypeProc, Destination: "/proc"},
			dest: existingFile,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			err := createMountPoint(tc.m, tc.dest)
			if !errors.Is(err, ErrNotADirectory) {
				t.Fatalf("createMountPoint() = %v, want ErrNotADirectory", err)
			}
			if !strings.Contains(err.Error(), tc.m.Destination) {
				t.Errorf("createMountPoint() = %q, want it to name the container path", err)
			}
		})
	}
}

// TestCreateMountPointReportsAMissingBindSource checks that the source is
// inspected before anything is created, so a typo in -mount does not leave an
// empty directory behind in the container.
func TestCreateMountPointReportsAMissingBindSource(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	missing := filepath.Join(root, "no-such-source")
	dest := filepath.Join(root, "dest")

	m := Mount{Type: TypeBind, Source: missing, Destination: "/data"}

	err := createMountPoint(m, dest)
	if err == nil {
		t.Fatal("createMountPoint() = nil for a source that does not exist")
	}
	if !strings.Contains(err.Error(), missing) {
		t.Errorf("createMountPoint() = %q, want it to name the source", err)
	}
	if _, statErr := os.Stat(dest); !errors.Is(statErr, os.ErrNotExist) {
		t.Errorf("a failed createMountPoint left %s behind", dest)
	}
}

// TestMountSource pins what mount(2) is handed, which is what shows up in
// /proc/self/mountinfo and so in every diagnosis of a container's filesystem.
func TestMountSource(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		m    Mount
		want string
	}{
		{name: "a bind names its path", m: Mount{Type: TypeBind, Source: "/srv/data"}, want: "/srv/data"},
		{name: "a bind with no source", m: Mount{Type: TypeBind}, want: "none"},
		{name: "proc names itself", m: Mount{Type: TypeProc}, want: string(TypeProc)},
		{name: "tmpfs names itself", m: Mount{Type: TypeTmpfs}, want: string(TypeTmpfs)},
		{name: "an explicit source wins", m: Mount{Type: TypeTmpfs, Source: "swap"}, want: "swap"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			if got := mountSource(tc.m); got != tc.want {
				t.Errorf("mountSource(%+v) = %q, want %q", tc.m, got, tc.want)
			}
		})
	}
}

// TestDescribe covers the phrasing of the error a failed mount produces.
func TestDescribe(t *testing.T) {
	t.Parallel()

	if got, want := describe(Mount{Type: TypeBind, Source: "/srv/data"}), `bind of "/srv/data"`; got != want {
		t.Errorf("describe() = %q, want %q", got, want)
	}
	if got, want := describe(Mount{Type: TypeProc}), "proc filesystem"; got != want {
		t.Errorf("describe() = %q, want %q", got, want)
	}
}

// TestKindOf covers the words the mismatch error is built from.
func TestKindOf(t *testing.T) {
	t.Parallel()

	if got := kindOf(true); got != "a directory" {
		t.Errorf("kindOf(true) = %q", got)
	}
	if got := kindOf(false); got != "a file" {
		t.Errorf("kindOf(false) = %q", got)
	}
}

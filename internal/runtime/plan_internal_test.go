package runtime

import (
	"errors"
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stevenstank/forge/internal/logging"
	"github.com/stevenstank/forge/internal/mount"
	"github.com/stevenstank/forge/internal/rootfs"
)

// Deciding *what* a container mounts is policy, and SSOT §2 puts policy here:
// internal/mount is told what to do and never chooses. These tests pin the
// choices, so a change to what every container gets is a deliberate diff rather
// than a side effect.

func testDir(t *testing.T) rootfs.Dir {
	t.Helper()

	base := filepath.Join(t.TempDir(), "a1b2c3d4e5f6")
	return rootfs.Dir{ID: "a1b2c3d4e5f6", Base: base, Rootfs: filepath.Join(base, "rootfs")}
}

// TestDefaultMountSet pins the set every container gets and why each member is
// in it. /proc is the load-bearing one: without it, `ps` inside a container
// still lists host processes, which is the headline limitation Stage 1
// documented and Stage 2 exists to close.
func TestDefaultMountSet(t *testing.T) {
	t.Parallel()

	got := defaultMounts()

	byDestination := make(map[string]mount.Mount, len(got))
	for _, m := range got {
		if _, dup := byDestination[m.Destination]; dup {
			t.Errorf("default mount set contains %q twice", m.Destination)
		}
		byDestination[m.Destination] = m
	}

	tests := []struct {
		destination string
		wantType    mount.Type
		wantOptions []mount.Option
	}{
		{
			destination: "/proc",
			wantType:    mount.TypeProc,
			wantOptions: []mount.Option{mount.OptionNoSuid, mount.OptionNoDev, mount.OptionNoExec},
		},
		{
			destination: "/dev",
			wantType:    mount.TypeTmpfs,
			wantOptions: []mount.Option{mount.OptionNoSuid},
		},
		{
			destination: "/dev/pts",
			wantType:    mount.TypeDevpts,
			wantOptions: []mount.Option{mount.OptionNoSuid, mount.OptionNoExec},
		},
		{
			destination: "/dev/shm",
			wantType:    mount.TypeTmpfs,
			wantOptions: []mount.Option{mount.OptionNoSuid, mount.OptionNoDev},
		},
		{
			// Read-only until Stage 4 gives the container a network namespace
			// of its own: a writable /sys in the host's netns is a hole in the
			// host's configuration, not the container's.
			destination: "/sys",
			wantType:    mount.TypeSysfs,
			wantOptions: []mount.Option{mount.OptionReadOnly, mount.OptionNoSuid, mount.OptionNoDev, mount.OptionNoExec},
		},
	}

	for _, tt := range tests {
		t.Run(tt.destination, func(t *testing.T) {
			t.Parallel()

			m, ok := byDestination[tt.destination]
			if !ok {
				t.Fatalf("default mount set has no %q", tt.destination)
			}
			if m.Type != tt.wantType {
				t.Errorf("%s type = %q, want %q", tt.destination, m.Type, tt.wantType)
			}
			for _, want := range tt.wantOptions {
				if !hasOption(m.Options, want) {
					t.Errorf("%s options = %v, want them to include %q", tt.destination, m.Options, want)
				}
			}
			if err := m.Validate(); err != nil {
				t.Errorf("%s is not a valid mount: %v", tt.destination, err)
			}
		})
	}
}

// TestDefaultDeviceNodes covers the mounts that make a container usable at all:
// practically every binary opens /dev/null, and a minimal rootfs ships with an
// empty /dev. They are binds from the host rather than mknod(2) calls, which
// keeps the code free of device-number arithmetic.
func TestDefaultDeviceNodes(t *testing.T) {
	t.Parallel()

	byDestination := make(map[string]mount.Mount)
	for _, m := range defaultMounts() {
		byDestination[m.Destination] = m
	}

	for _, name := range []string{"null", "zero", "full", "random", "urandom", "tty"} {
		destination := "/dev/" + name

		m, ok := byDestination[destination]
		if !ok {
			t.Errorf("default mount set has no %s", destination)
			continue
		}
		if m.Type != mount.TypeBind {
			t.Errorf("%s type = %q, want a bind", destination, m.Type)
		}
		if m.Source != destination {
			t.Errorf("%s source = %q, want the host's %q", destination, m.Source, destination)
		}
	}
}

// TestDefaultMountsAreOrderedSafely guards the nesting rule: /dev must be
// mounted before anything under it, or the tmpfs hides the device nodes.
func TestDefaultMountsAreOrderedSafely(t *testing.T) {
	t.Parallel()

	plan := mount.Plan{Source: "/srv/alpine", Root: "/var/lib/forge/containers/abc/rootfs", Mounts: defaultMounts()}

	seen := make(map[string]int)
	for i, m := range plan.Ordered() {
		seen[m.Destination] = i
	}

	dev, ok := seen["/dev"]
	if !ok {
		t.Fatalf("ordered defaults = %v, want them to include /dev", seen)
	}

	for _, nested := range []string{"/dev/pts", "/dev/shm", "/dev/null"} {
		position, ok := seen[nested]
		if !ok {
			t.Errorf("ordered defaults do not include %s", nested)
			continue
		}
		if position < dev {
			t.Errorf("%s is mounted before /dev, so the /dev tmpfs would hide it", nested)
		}
	}
}

func TestMountPlan(t *testing.T) {
	t.Parallel()

	dir := testDir(t)
	source := t.TempDir()

	spec := Spec{
		Command: []string{"/bin/sh"},
		Rootfs:  source,
		Mounts: []mount.Mount{
			{Source: "/host/data", Destination: "/data", Type: mount.TypeBind, Options: []mount.Option{mount.OptionReadOnly}},
		},
	}

	plan, err := mountPlan(spec, dir)
	if err != nil {
		t.Fatalf("mountPlan() = %v", err)
	}

	// The container's "/" is the directory Forge owns, not the user's tree:
	// FR-2.4 requires a per-container rootfs directory, and Stage 5 replaces
	// the source bind with unpacked image layers without moving the root.
	if plan.Root != dir.Rootfs {
		t.Errorf("plan.Root = %q, want %q", plan.Root, dir.Rootfs)
	}
	if plan.Source != source {
		t.Errorf("plan.Source = %q, want %q", plan.Source, source)
	}
	if plan.ReadonlyRoot {
		t.Error("plan.ReadonlyRoot = true, want false when the spec did not ask for it")
	}
	if err := plan.Validate(); err != nil {
		t.Errorf("mountPlan() produced an invalid plan: %v", err)
	}
}

// TestMountPlanPutsUserMountsAfterTheDefaults fixes the shadowing rule: an
// explicit --mount wins over a default with the same destination, and the user
// never has to reason about an ordering Forge chose for its own reasons.
func TestMountPlanPutsUserMountsAfterTheDefaults(t *testing.T) {
	t.Parallel()

	userMount := mount.Mount{Source: "/host/data", Destination: "/data", Type: mount.TypeBind}

	plan, err := mountPlan(Spec{Command: []string{"/bin/sh"}, Rootfs: t.TempDir(), Mounts: []mount.Mount{userMount}}, testDir(t))
	if err != nil {
		t.Fatalf("mountPlan() = %v", err)
	}

	if len(plan.Mounts) != len(defaultMounts())+1 {
		t.Fatalf("plan has %d mounts, want the %d defaults plus one", len(plan.Mounts), len(defaultMounts()))
	}
	if last := plan.Mounts[len(plan.Mounts)-1]; last.Destination != userMount.Destination {
		t.Errorf("last mount is %q, want the user's %q", last.Destination, userMount.Destination)
	}
}

// TestMountPlanRejectsAUserMountCollidingWithADefault refuses a silent
// last-wins: a user who bind-mounts over /proc is far more likely to have made
// a mistake than to have meant it, and Stage 2 is not the place to guess.
func TestMountPlanRejectsAUserMountCollidingWithADefault(t *testing.T) {
	t.Parallel()

	spec := Spec{
		Command: []string{"/bin/sh"},
		Rootfs:  t.TempDir(),
		Mounts:  []mount.Mount{{Source: "/host/proc", Destination: "/proc", Type: mount.TypeBind}},
	}

	_, err := mountPlan(spec, testDir(t))
	if !errors.Is(err, mount.ErrDuplicateDestination) {
		t.Fatalf("mountPlan() = %v, want %v", err, mount.ErrDuplicateDestination)
	}
}

func TestMountPlanCarriesReadonlyRoot(t *testing.T) {
	t.Parallel()

	plan, err := mountPlan(Spec{Command: []string{"/bin/sh"}, Rootfs: t.TempDir(), ReadonlyRoot: true}, testDir(t))
	if err != nil {
		t.Fatalf("mountPlan() = %v", err)
	}
	if !plan.ReadonlyRoot {
		t.Error("plan.ReadonlyRoot = false, want true")
	}
}

// --- cleanup stack --------------------------------------------------------

// The cleanup stack implements SSOT §11.3: every resource created during a run
// is unwound in reverse order if a later step fails. Its contract is that it
// always runs everything — a cleanup that fails must not prevent the cleanups
// registered before it, or a failed run leaks the very resources it was meant
// to release.

func TestCleanupStackUnwindsInReverseOrder(t *testing.T) {
	t.Parallel()

	var order []string
	stack := newCleanupStack(logging.New(io.Discard, slog.LevelError))

	for _, name := range []string{"first", "second", "third"} {
		stack.push(name, func() error {
			order = append(order, name)
			return nil
		})
	}

	stack.unwind()

	if got := strings.Join(order, ","); got != "third,second,first" {
		t.Errorf("cleanup order = %q, want %q", got, "third,second,first")
	}
}

func TestCleanupStackContinuesAfterAFailure(t *testing.T) {
	t.Parallel()

	var ran []string
	stack := newCleanupStack(logging.New(io.Discard, slog.LevelError))

	stack.push("first", func() error { ran = append(ran, "first"); return nil })
	stack.push("second", func() error { ran = append(ran, "second"); return errors.New("boom") })
	stack.push("third", func() error { ran = append(ran, "third"); return nil })

	stack.unwind()

	if got := strings.Join(ran, ","); got != "third,second,first" {
		t.Errorf("ran = %q, want every cleanup to run despite the failure", got)
	}
}

// TestCleanupStackLogsFailures covers SSOT §13.7: unwind returns nothing, so a
// silently dropped error would be invisible. It must reach the log.
func TestCleanupStackLogsFailures(t *testing.T) {
	t.Parallel()

	var logs strings.Builder
	stack := newCleanupStack(logging.New(&logs, slog.LevelWarn))

	stack.push("removing the container directory", func() error { return errors.New("device or resource busy") })
	stack.unwind()

	got := logs.String()
	for _, want := range []string{"removing the container directory", "device or resource busy"} {
		if !strings.Contains(got, want) {
			t.Errorf("log %q does not mention %q", got, want)
		}
	}
}

// TestCleanupStackIsIdempotent lets a run defer unwind unconditionally and also
// call it explicitly on an error path without cleaning up twice.
func TestCleanupStackIsIdempotent(t *testing.T) {
	t.Parallel()

	calls := 0
	stack := newCleanupStack(logging.New(io.Discard, slog.LevelError))
	stack.push("once", func() error { calls++; return nil })

	stack.unwind()
	stack.unwind()

	if calls != 1 {
		t.Errorf("cleanup ran %d times, want 1", calls)
	}
}

// TestCleanupStackOnAnEmptyStackIsANoOp covers the path where a run fails
// before creating anything.
func TestCleanupStackOnAnEmptyStackIsANoOp(t *testing.T) {
	t.Parallel()

	newCleanupStack(logging.New(io.Discard, slog.LevelError)).unwind()
}

func hasOption(options []mount.Option, want mount.Option) bool {
	for _, o := range options {
		if o == want {
			return true
		}
	}
	return false
}

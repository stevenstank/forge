package runtime_test

import (
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stevenstank/forge/internal/logging"
	"github.com/stevenstank/forge/internal/mount"
	"github.com/stevenstank/forge/internal/rootfs"
	"github.com/stevenstank/forge/internal/runtime"
)

// newRunner builds a Runner whose rootfs store lives in a temporary directory,
// so a test can never write to the real /var/lib/forge.
func newRunner(t *testing.T) *runtime.Runner {
	t.Helper()

	runner, err := runtime.NewRunner(
		logging.New(io.Discard, slog.LevelError),
		runtime.Config{Root: filepath.Join(t.TempDir(), "containers")},
	)
	if err != nil {
		t.Fatalf("NewRunner() = %v", err)
	}
	return runner
}

// TestNewRunnerRejectsARelativeRoot keeps a misconfigured --root from resolving
// against whatever directory forge happened to be started in.
func TestNewRunnerRejectsARelativeRoot(t *testing.T) {
	t.Parallel()

	_, err := runtime.NewRunner(logging.New(io.Discard, slog.LevelError), runtime.Config{Root: "containers"})
	if err == nil {
		t.Fatal("NewRunner(relative root) = nil error, want a failure")
	}
}

func TestSpecValidate(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		spec    runtime.Spec
		wantErr error
	}{
		{
			name: "absolute path is valid",
			spec: runtime.Spec{Command: []string{"/bin/echo", "hello"}},
		},
		{
			name: "relative path is valid",
			spec: runtime.Spec{Command: []string{"./forge-test"}},
		},
		{
			name:    "no command is rejected",
			spec:    runtime.Spec{},
			wantErr: runtime.ErrNoCommand,
		},
		{
			name:    "empty command is rejected",
			spec:    runtime.Spec{Command: []string{}},
			wantErr: runtime.ErrNoCommand,
		},
		{
			name:    "empty binary is rejected",
			spec:    runtime.Spec{Command: []string{""}},
			wantErr: runtime.ErrNoCommand,
		},
		{
			name:    "bare name is rejected because forge does not search PATH",
			spec:    runtime.Spec{Command: []string{"echo"}},
			wantErr: runtime.ErrNotAPath,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			err := tt.spec.Validate()
			if tt.wantErr == nil {
				if err != nil {
					t.Fatalf("Validate() = %v, want nil", err)
				}
				return
			}
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("Validate() = %v, want %v", err, tt.wantErr)
			}
		})
	}
}

// TestValidateSuggestsAPath keeps the PATH refusal actionable.
func TestValidateSuggestsAPath(t *testing.T) {
	t.Parallel()

	err := runtime.Spec{Command: []string{"echo"}}.Validate()
	if err == nil {
		t.Fatal("Validate() = nil, want an error")
	}
	if got := err.Error(); !strings.Contains(got, "/bin/echo") {
		t.Errorf("error %q does not suggest a concrete path", got)
	}
}

func TestIsInitCommand(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		args []string
		want bool
	}{
		{name: "init command", args: []string{"forge-init", runtime.InitCommandName}, want: true},
		{name: "init command with trailing args", args: []string{"forge", runtime.InitCommandName, "x"}, want: true},
		{name: "run command", args: []string{"forge", "run", "/bin/echo"}, want: false},
		{name: "no arguments", args: []string{"forge"}, want: false},
		{name: "empty vector", args: nil, want: false},
		{name: "init name in the wrong position", args: []string{runtime.InitCommandName}, want: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if got := runtime.IsInitCommand(tt.args); got != tt.want {
				t.Errorf("IsInitCommand(%q) = %v, want %v", tt.args, got, tt.want)
			}
		})
	}
}

// TestRunRefusesNestedInit covers the fork-bomb guard: a binary that
// re-executes itself without dispatching the init command must fail
// immediately rather than starting containers forever (PRD NFR-8).
//
// It cannot run in parallel because it manipulates the process environment.
func TestRunRefusesNestedInit(t *testing.T) {
	t.Setenv("FORGE_CONTAINER_INIT", "1")

	runner := newRunner(t)

	_, err := runner.Run(t.Context(), runtime.Spec{Command: []string{"/bin/echo", "hi"}})
	if !errors.Is(err, runtime.ErrNestedInit) {
		t.Fatalf("Run() = %v, want %v", err, runtime.ErrNestedInit)
	}
}

// TestRunRejectsInvalidSpecBeforeForking confirms validation happens in the
// parent, so bad input never reaches the kernel. This is why it needs no root.
func TestRunRejectsInvalidSpecBeforeForking(t *testing.T) {
	t.Parallel()

	runner := newRunner(t)

	if _, err := runner.Run(t.Context(), runtime.Spec{}); !errors.Is(err, runtime.ErrNoCommand) {
		t.Errorf("Run() = %v, want %v", err, runtime.ErrNoCommand)
	}
	if _, err := runner.Run(t.Context(), runtime.Spec{Command: []string{"echo"}}); !errors.Is(err, runtime.ErrNotAPath) {
		t.Errorf("Run() = %v, want %v", err, runtime.ErrNotAPath)
	}
}

// --- Stage 2 --------------------------------------------------------------

// TestSpecValidateStage2 covers the filesystem half of the spec. Everything
// here is pure: a bad --mount must be rejected in the parent, before anything
// is forked and long before any mount(2) reaches the kernel.
func TestSpecValidateStage2(t *testing.T) {
	t.Parallel()

	const rootfsPath = "/srv/images/alpine"
	bind := func(src, dst string, opts ...mount.Option) mount.Mount {
		return mount.Mount{Source: src, Destination: dst, Type: mount.TypeBind, Options: opts}
	}

	tests := []struct {
		name    string
		spec    runtime.Spec
		wantErr error
	}{
		{
			name: "a rootfs alone is valid",
			spec: runtime.Spec{Command: []string{"/bin/sh"}, Rootfs: rootfsPath},
		},
		{
			name: "rootfs with mounts is valid",
			spec: runtime.Spec{
				Command: []string{"/bin/sh"},
				Rootfs:  rootfsPath,
				Mounts:  []mount.Mount{bind("/host/data", "/data", mount.OptionReadOnly)},
			},
		},
		{
			name: "rootfs with a working directory is valid",
			spec: runtime.Spec{Command: []string{"/bin/sh"}, Rootfs: rootfsPath, WorkingDir: "/data"},
		},
		{
			name: "no rootfs is valid: that is stage 1 behaviour",
			spec: runtime.Spec{Command: []string{"/bin/echo", "hi"}},
		},
		{
			name:    "a relative rootfs is rejected",
			spec:    runtime.Spec{Command: []string{"/bin/sh"}, Rootfs: "images/alpine"},
			wantErr: runtime.ErrRootfsNotAbsolute,
		},
		{
			name: "mounts without a rootfs are rejected: there is nothing to mount into",
			spec: runtime.Spec{
				Command: []string{"/bin/echo", "hi"},
				Mounts:  []mount.Mount{bind("/host/data", "/data")},
			},
			wantErr: runtime.ErrMountWithoutRootfs,
		},
		{
			name:    "a read-only root without a rootfs is rejected",
			spec:    runtime.Spec{Command: []string{"/bin/echo", "hi"}, ReadonlyRoot: true},
			wantErr: runtime.ErrMountWithoutRootfs,
		},
		{
			name:    "a working directory without a rootfs is rejected",
			spec:    runtime.Spec{Command: []string{"/bin/echo", "hi"}, WorkingDir: "/data"},
			wantErr: runtime.ErrMountWithoutRootfs,
		},
		{
			name: "an invalid mount is rejected",
			spec: runtime.Spec{
				Command: []string{"/bin/sh"},
				Rootfs:  rootfsPath,
				Mounts:  []mount.Mount{bind("relative/source", "/data")},
			},
			wantErr: mount.ErrInvalidMountSpec,
		},
		{
			name: "a mount destination escaping the container root is rejected",
			spec: runtime.Spec{
				Command: []string{"/bin/sh"},
				Rootfs:  rootfsPath,
				Mounts:  []mount.Mount{bind("/host/data", "/../../etc")},
			},
			wantErr: mount.ErrEscapesRoot,
		},
		{
			name: "duplicate mount destinations are rejected",
			spec: runtime.Spec{
				Command: []string{"/bin/sh"},
				Rootfs:  rootfsPath,
				Mounts:  []mount.Mount{bind("/host/one", "/data"), bind("/host/two", "/data")},
			},
			wantErr: mount.ErrDuplicateDestination,
		},
		{
			name:    "a relative working directory is rejected",
			spec:    runtime.Spec{Command: []string{"/bin/sh"}, Rootfs: rootfsPath, WorkingDir: "data"},
			wantErr: runtime.ErrWorkingDirNotAbsolute,
		},
		{
			name:    "the command is still validated",
			spec:    runtime.Spec{Command: []string{"sh"}, Rootfs: rootfsPath},
			wantErr: runtime.ErrNotAPath,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			err := tt.spec.Validate()
			if tt.wantErr == nil {
				if err != nil {
					t.Fatalf("Validate() = %v, want nil", err)
				}
				return
			}
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("Validate() = %v, want %v", err, tt.wantErr)
			}
		})
	}
}

// TestValidateSuggestsAPathInsideTheContainer keeps the stage's most likely
// confusion — "/bin/sh exists on my host, why is it not found?" — answerable
// from the error text.
func TestValidateSuggestsAPathInsideTheContainer(t *testing.T) {
	t.Parallel()

	err := runtime.Spec{Command: []string{"sh"}, Rootfs: "/srv/images/alpine"}.Validate()
	if err == nil {
		t.Fatal("Validate() = nil, want an error")
	}
	if got := err.Error(); !strings.Contains(got, "container") {
		t.Errorf("error %q does not say the path is resolved inside the container", got)
	}
}

// TestRunRejectsAMissingRootfsBeforeForking confirms the source tree is checked
// in the parent, so a typo'd --rootfs is reported by forge rather than showing
// up as an opaque exit 127 from a container that never started.
func TestRunRejectsAMissingRootfsBeforeForking(t *testing.T) {
	t.Parallel()

	runner := newRunner(t)

	spec := runtime.Spec{Command: []string{"/bin/sh"}, Rootfs: filepath.Join(t.TempDir(), "nonexistent")}

	_, err := runner.Run(t.Context(), spec)
	if !errors.Is(err, rootfs.ErrSourceNotFound) {
		t.Fatalf("Run() = %v, want %v", err, rootfs.ErrSourceNotFound)
	}
}

// TestRunLeavesNoContainerDirectoryWhenItFailsEarly covers the rollback promise
// in PRD NFR-8: a run that fails after the rootfs directory was created must
// not leave that directory behind.
func TestRunLeavesNoContainerDirectoryWhenItFailsEarly(t *testing.T) {
	t.Parallel()

	root := filepath.Join(t.TempDir(), "containers")
	runner, err := runtime.NewRunner(logging.New(io.Discard, slog.LevelError), runtime.Config{Root: root})
	if err != nil {
		t.Fatalf("NewRunner() = %v", err)
	}

	// A source tree that exists, so preparation gets far enough to create the
	// container directory, with a command that cannot possibly be validated.
	spec := runtime.Spec{Command: []string{"sh"}, Rootfs: t.TempDir()}
	if _, err := runner.Run(t.Context(), spec); err == nil {
		t.Fatal("Run() = nil, want a failure")
	}

	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatalf("reading the store root: %v", err)
	}
	if len(entries) != 0 {
		names := make([]string, 0, len(entries))
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Errorf("store root contains %v after a failed run, want it empty", names)
	}
}

// Init itself is exercised end to end by the Stage 1 integration tests; its
// wire format is covered by the decodeInitPayload tests in init_internal_test.go.
// Calling Init here would read whatever the test harness left on descriptor 3.

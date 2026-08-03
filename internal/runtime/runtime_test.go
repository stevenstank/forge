package runtime_test

import (
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stevenstank/forge/internal/cgroup"
	"github.com/stevenstank/forge/internal/logging"
	"github.com/stevenstank/forge/internal/mount"
	"github.com/stevenstank/forge/internal/rootfs"
	"github.com/stevenstank/forge/internal/runtime"
)

// newRunner builds a Runner whose rootfs store and cgroup hierarchy both live
// in temporary directories, so a test can never write to the real
// /var/lib/forge or the host's /sys/fs/cgroup.
func newRunner(t *testing.T) *runtime.Runner {
	t.Helper()

	runner, err := runtime.NewRunner(
		logging.New(io.Discard, slog.LevelError),
		runtime.Config{
			Root:       filepath.Join(t.TempDir(), "containers"),
			CgroupRoot: fakeCgroupRoot(t),
		},
	)
	if err != nil {
		t.Fatalf("NewRunner() = %v", err)
	}
	return runner
}

// fakeCgroupRoot builds a directory that looks enough like the root of a cgroup
// v2 unified hierarchy for internal/cgroup to work in.
//
// cgroup v2 is a filesystem interface driven entirely through os and filepath,
// so a temp directory exercises the real create/attach/destroy code against
// real files without root. What it cannot do is enforce a limit — that is what
// the Stage 3 integration tests are for.
func fakeCgroupRoot(t *testing.T) string {
	t.Helper()

	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "cgroup.controllers"), []byte("cpu io memory pids"), 0o644); err != nil {
		t.Fatalf("writing cgroup.controllers: %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, "cgroup.subtree_control"), nil, 0o644); err != nil {
		t.Fatalf("writing cgroup.subtree_control: %v", err)
	}

	return root
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

// --- Stage 3 --------------------------------------------------------------

// The Stage 3 tests assert what the parent does around the container: that a
// bad limit is refused before anything is forked, that a leaf is created for
// every container, and that no leaf survives a run however that run ends.
//
// None of them needs root, and none of them asserts enforcement — whether the
// kernel actually caps memory is a property of the kernel, not of this package,
// and belongs to test/integration.

// runToCompletion runs a spec and returns the error, if any. Without root the
// clone(2) is refused and Run fails; as root the container really starts. Both
// outcomes are useful to the tests below, which assert what is true either way:
// what the cleanup left behind.
func runToCompletion(t *testing.T, runner *runtime.Runner, spec runtime.Spec) error {
	t.Helper()

	_, err := runner.Run(t.Context(), spec)
	return err
}

// forgeCgroupEntries lists what is left under Forge's own cgroup.
func forgeCgroupEntries(t *testing.T, cgroupRoot string) []string {
	t.Helper()

	entries, err := os.ReadDir(filepath.Join(cgroupRoot, "forge"))
	if err != nil {
		t.Fatalf("reading forge's cgroup: %v", err)
	}

	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() {
			names = append(names, entry.Name())
		}
	}

	return names
}

func TestSpecValidateStage3(t *testing.T) {
	t.Parallel()

	memory := func(b cgroup.Bytes) *cgroup.Bytes { return &b }
	weight := func(w cgroup.Weight) *cgroup.Weight { return &w }
	procs := func(p int64) *int64 { return &p }

	tests := []struct {
		name    string
		spec    runtime.Spec
		wantErr error
	}{
		{
			name: "no limits is valid: that is stage 1 and 2 behaviour",
			spec: runtime.Spec{Command: []string{"/bin/echo", "hi"}},
		},
		{
			name: "every limit at once is valid",
			spec: runtime.Spec{
				Command: []string{"/bin/sh"},
				Limits: cgroup.Limits{
					MemoryMax: memory(128 << 20),
					CPU:       &cgroup.CPUQuota{Quota: 150_000},
					CPUWeight: weight(512),
					PIDsMax:   procs(64),
				},
			},
		},
		{
			name: "limits need no rootfs: a stage 1 container can be limited too",
			spec: runtime.Spec{
				Command: []string{"/bin/echo", "hi"},
				Limits:  cgroup.Limits{MemoryMax: memory(64 << 20)},
			},
		},
		{
			name: "a memory limit of zero is rejected",
			spec: runtime.Spec{
				Command: []string{"/bin/sh"},
				Limits:  cgroup.Limits{MemoryMax: memory(0)},
			},
			wantErr: cgroup.ErrInvalidLimit,
		},
		{
			name: "a cpu quota of zero is rejected",
			spec: runtime.Spec{
				Command: []string{"/bin/sh"},
				Limits:  cgroup.Limits{CPU: &cgroup.CPUQuota{Quota: 0}},
			},
			wantErr: cgroup.ErrInvalidLimit,
		},
		{
			name: "a cpu weight outside the kernel's range is rejected",
			spec: runtime.Spec{
				Command: []string{"/bin/sh"},
				Limits:  cgroup.Limits{CPUWeight: weight(cgroup.MaxWeight + 1)},
			},
			wantErr: cgroup.ErrInvalidLimit,
		},
		{
			name: "a pids limit of zero is rejected",
			spec: runtime.Spec{
				Command: []string{"/bin/sh"},
				Limits:  cgroup.Limits{PIDsMax: procs(0)},
			},
			wantErr: cgroup.ErrInvalidLimit,
		},
		{
			name: "the command is still validated",
			spec: runtime.Spec{
				Command: []string{"sh"},
				Limits:  cgroup.Limits{MemoryMax: memory(64 << 20)},
			},
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

// TestRunRejectsInvalidLimitsBeforeForking keeps a limit the kernel would
// refuse from becoming a container that fails half-way through starting.
func TestRunRejectsInvalidLimitsBeforeForking(t *testing.T) {
	t.Parallel()

	cgroupRoot := fakeCgroupRoot(t)
	runner, err := runtime.NewRunner(
		logging.New(io.Discard, slog.LevelError),
		runtime.Config{Root: filepath.Join(t.TempDir(), "containers"), CgroupRoot: cgroupRoot},
	)
	if err != nil {
		t.Fatalf("NewRunner() = %v", err)
	}

	zero := cgroup.Bytes(0)
	spec := runtime.Spec{Command: []string{"/bin/true"}, Limits: cgroup.Limits{MemoryMax: &zero}}

	if err := runToCompletion(t, runner, spec); !errors.Is(err, cgroup.ErrInvalidLimit) {
		t.Fatalf("Run() = %v, want %v", err, cgroup.ErrInvalidLimit)
	}

	// Refused before anything was created: not even Forge's own cgroup exists.
	if _, err := os.Stat(filepath.Join(cgroupRoot, "forge")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("a rejected spec created %s", filepath.Join(cgroupRoot, "forge"))
	}
}

// TestRunCreatesAndDestroysTheContainerCgroup covers FR-3.1 and FR-3.5 from the
// orchestrator's side: a leaf is created for the container, and nothing
// survives the run.
//
// It asserts the same postcondition whether the run succeeded or the clone was
// refused for want of privileges, which is what makes it meaningful in both a
// developer's shell and a privileged CI runner.
func TestRunCreatesAndDestroysTheContainerCgroup(t *testing.T) {
	t.Parallel()

	cgroupRoot := fakeCgroupRoot(t)
	runner, err := runtime.NewRunner(
		logging.New(io.Discard, slog.LevelError),
		runtime.Config{Root: filepath.Join(t.TempDir(), "containers"), CgroupRoot: cgroupRoot},
	)
	if err != nil {
		t.Fatalf("NewRunner() = %v", err)
	}

	// Limit-free, so this exercises the success path: a fake hierarchy has none
	// of the interface files the kernel creates with a leaf, so a container
	// with limits could only fail here. What Forge writes into a real leaf is
	// asserted by the Stage 3 integration tests.
	spec := runtime.Spec{Command: []string{"/bin/true"}}

	runErr := runToCompletion(t, runner, spec)

	// Forge's own cgroup is created on demand and deliberately never removed:
	// concurrent runs would race to remove a directory the other is about to
	// create in. Its existence is also the evidence that the container's leaf
	// was created at all.
	if _, err := os.Stat(filepath.Join(cgroupRoot, "forge")); err != nil {
		t.Fatalf("expected forge's cgroup to exist after a run (run error: %v): %v", runErr, err)
	}

	if leaves := forgeCgroupEntries(t, cgroupRoot); len(leaves) != 0 {
		t.Errorf("cgroups %v survived the run (run error: %v), want none", leaves, runErr)
	}
}

// TestRunFailsWhenLimitsCannotBeApplied is the rule that a limit the caller
// asked for is never silently dropped: a container that was meant to be capped
// must not start uncapped.
func TestRunFailsWhenLimitsCannotBeApplied(t *testing.T) {
	t.Parallel()

	// A directory with no cgroup.controllers is what a cgroup v1 or hybrid
	// host looks like from here.
	runner, err := runtime.NewRunner(
		logging.New(io.Discard, slog.LevelError),
		runtime.Config{Root: filepath.Join(t.TempDir(), "containers"), CgroupRoot: t.TempDir()},
	)
	if err != nil {
		t.Fatalf("NewRunner() = %v", err)
	}

	memory := cgroup.Bytes(64 << 20)
	spec := runtime.Spec{Command: []string{"/bin/true"}, Limits: cgroup.Limits{MemoryMax: &memory}}

	if err := runToCompletion(t, runner, spec); !errors.Is(err, cgroup.ErrUnifiedHierarchyNotMounted) {
		t.Fatalf("Run() = %v, want %v", err, cgroup.ErrUnifiedHierarchyNotMounted)
	}
}

// TestRunWithoutLimitsToleratesAMissingHierarchy is the other half of that
// rule. A container that asked for no limits loses only accounting, and
// refusing to start it would regress Stages 1 and 2 on every cgroup v1 host.
func TestRunWithoutLimitsToleratesAMissingHierarchy(t *testing.T) {
	t.Parallel()

	var logs strings.Builder
	runner, err := runtime.NewRunner(
		logging.New(&logs, slog.LevelWarn),
		runtime.Config{Root: filepath.Join(t.TempDir(), "containers"), CgroupRoot: t.TempDir()},
	)
	if err != nil {
		t.Fatalf("NewRunner() = %v", err)
	}

	// The run itself may still fail for want of privileges; what matters is
	// that it was not the cgroup that stopped it.
	err = runToCompletion(t, runner, runtime.Spec{Command: []string{"/bin/true"}})
	if errors.Is(err, cgroup.ErrUnifiedHierarchyNotMounted) {
		t.Fatalf("Run() = %v, want the missing hierarchy to be tolerated", err)
	}

	// Tolerated, but never silent (SSOT §13.7).
	if !strings.Contains(logs.String(), "cgroup") {
		t.Errorf("log %q does not warn that the container runs without a cgroup", logs.String())
	}
}

// Init itself is exercised end to end by the Stage 1 integration tests; its
// wire format is covered by the decodeInitPayload tests in init_internal_test.go.
// Calling Init here would read whatever the test harness left on descriptor 3.

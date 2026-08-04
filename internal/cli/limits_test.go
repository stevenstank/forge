package cli

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/stevenstank/forge/internal/cgroup"
	"github.com/stevenstank/forge/internal/rootfs"
	"github.com/stevenstank/forge/internal/runtime"
)

// Stage 3's CLI surface: the four resource-limit flags. What this package is
// responsible for is turning them into a cgroup.Limits and rejecting what it
// cannot, so that is what these tests assert — none of them start a container,
// and so none of them need root (SSOT §13.6).

// TestParseRunSpecLimits pins the mapping from flags to Limits, including the
// unit arithmetic each flag inherits from internal/cgroup's parsers.
func TestParseRunSpecLimits(t *testing.T) {
	t.Parallel()

	spec, err := parseRunSpec([]string{
		"-memory", "128m",
		"-cpus", "1.5",
		"-cpu-weight", "512",
		"-pids", "64",
		"/bin/sh",
	})
	if err != nil {
		t.Fatalf("parseRunSpec() = %v", err)
	}

	limits := spec.Limits
	if limits.IsZero() {
		t.Fatal("Limits.IsZero() = true, want the four flags to have been recorded")
	}

	if limits.MemoryMax == nil {
		t.Fatal("MemoryMax = nil, want 128m")
	}
	if want := cgroup.Bytes(128 << 20); *limits.MemoryMax != want {
		t.Errorf("MemoryMax = %d, want %d", *limits.MemoryMax, want)
	}

	if limits.CPU == nil {
		t.Fatal("CPU = nil, want 1.5 cores")
	}
	// 1.5 cores over the kernel's default 100ms period.
	if want := (cgroup.CPUQuota{Quota: 150_000, Period: cgroup.DefaultCPUPeriod}); *limits.CPU != want {
		t.Errorf("CPU = %+v, want %+v", *limits.CPU, want)
	}

	if limits.CPUWeight == nil {
		t.Fatal("CPUWeight = nil, want 512")
	}
	if *limits.CPUWeight != cgroup.Weight(512) {
		t.Errorf("CPUWeight = %d, want 512", *limits.CPUWeight)
	}

	if limits.PIDsMax == nil {
		t.Fatal("PIDsMax = nil, want 64")
	}
	if *limits.PIDsMax != 64 {
		t.Errorf("PIDsMax = %d, want 64", *limits.PIDsMax)
	}
}

// TestParseRunSpecWithoutLimitFlags is the flip side of the pointer fields: a
// caller who asks for nothing must leave every field nil, because that is what
// stops internal/cgroup writing a controller file at all. A default filled in
// here would be the CLI making a policy decision that is not its to make.
func TestParseRunSpecWithoutLimitFlags(t *testing.T) {
	t.Parallel()

	spec, err := parseRunSpec([]string{"/bin/echo", "hello"})
	if err != nil {
		t.Fatalf("parseRunSpec() = %v", err)
	}

	if !spec.Limits.IsZero() {
		t.Errorf("Limits = %+v, want the zero value", spec.Limits)
	}
	if got := spec.Limits.Controllers(); len(got) != 0 {
		t.Errorf("Controllers() = %v, want none delegated", got)
	}
	if got := spec.Limits.Files(); len(got) != 0 {
		t.Errorf("Files() = %v, want nothing written", got)
	}
}

// TestParseRunSpecUnlimitedLimits covers "max": asking for no limit explicitly
// is not the same as saying nothing. The field is set, so the controller is
// delegated and the file is written — which is what keeps -memory max from
// being quietly turned into a swap restriction.
func TestParseRunSpecUnlimitedLimits(t *testing.T) {
	t.Parallel()

	spec, err := parseRunSpec([]string{"-memory", "max", "-cpus", "max", "-pids", "max", "/bin/sh"})
	if err != nil {
		t.Fatalf("parseRunSpec() = %v", err)
	}

	if spec.Limits.MemoryMax == nil || *spec.Limits.MemoryMax >= 0 {
		t.Errorf("MemoryMax = %v, want an explicitly unlimited value", spec.Limits.MemoryMax)
	}
	if spec.Limits.CPU == nil || spec.Limits.CPU.Quota >= 0 {
		t.Errorf("CPU = %v, want an explicitly unlimited quota", spec.Limits.CPU)
	}
	if spec.Limits.PIDsMax == nil || *spec.Limits.PIDsMax >= 0 {
		t.Errorf("PIDsMax = %v, want an explicitly unlimited value", spec.Limits.PIDsMax)
	}
}

// TestParseRunSpecLimitsAreIndependent confirms each flag stands alone: one
// limit set leaves the other three untouched, so a container capped on memory
// is not silently given a CPU or pids limit as well.
func TestParseRunSpecLimitsAreIndependent(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		args []string
		want func(cgroup.Limits) bool
	}{
		{
			name: "memory only",
			args: []string{"-memory", "64m"},
			want: func(l cgroup.Limits) bool { return l.MemoryMax != nil },
		},
		{
			name: "cpus only",
			args: []string{"-cpus", "0.5"},
			want: func(l cgroup.Limits) bool { return l.CPU != nil },
		},
		{
			name: "cpu-weight only",
			args: []string{"-cpu-weight", "200"},
			want: func(l cgroup.Limits) bool { return l.CPUWeight != nil },
		},
		{
			name: "pids only",
			args: []string{"-pids", "32"},
			want: func(l cgroup.Limits) bool { return l.PIDsMax != nil },
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			spec, err := parseRunSpec(append(tt.args, "/bin/sh"))
			if err != nil {
				t.Fatalf("parseRunSpec() = %v", err)
			}
			if !tt.want(spec.Limits) {
				t.Errorf("Limits = %+v, want the flag under test to be set", spec.Limits)
			}

			// Exactly one controller file group: nothing else was inferred.
			if got := len(spec.Limits.Controllers()); got != 1 {
				t.Errorf("Controllers() = %v, want exactly one", spec.Limits.Controllers())
			}
		})
	}
}

// TestParseRunSpecRejectsBadLimits covers the values a caller can get wrong.
// Every one is refused by parseRunSpec, before a cgroup exists to write to, and
// the message names the flag that carried the value.
func TestParseRunSpecRejectsBadLimits(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		args     []string
		wantFlag string
		wantText string
	}{
		{
			name:     "memory is not a quantity",
			args:     []string{"-memory", "lots"},
			wantFlag: "-memory",
			wantText: "128m",
		},
		{
			name:     "fractional memory is ambiguous",
			args:     []string{"-memory", "1.5g"},
			wantFlag: "-memory",
			wantText: "1.5g",
		},
		{
			name:     "negative memory is not how you say unlimited",
			args:     []string{"-memory", "-1"},
			wantFlag: "-memory",
			wantText: "max",
		},
		{
			name:     "a memory limit of zero leaves nothing to run in",
			args:     []string{"-memory", "0"},
			wantFlag: "",
			wantText: "no memory to run in",
		},
		{
			name:     "cpus is not a count",
			args:     []string{"-cpus", "half"},
			wantFlag: "-cpus",
			wantText: "0.5",
		},
		{
			name:     "a cpu quota of zero starves the container",
			args:     []string{"-cpus", "0"},
			wantFlag: "-cpus",
			wantText: "no CPU time",
		},
		{
			name:     "cpu weight below the kernel's range",
			args:     []string{"-cpu-weight", "0"},
			wantFlag: "-cpu-weight",
			wantText: "outside the kernel's range",
		},
		{
			name:     "cpu weight above the kernel's range",
			args:     []string{"-cpu-weight", "10001"},
			wantFlag: "-cpu-weight",
			wantText: "outside the kernel's range",
		},
		{
			name:     "cpu weight is not a number",
			args:     []string{"-cpu-weight", "heavy"},
			wantFlag: "-cpu-weight",
			wantText: "heavy",
		},
		{
			name:     "pids is not a count",
			args:     []string{"-pids", "many"},
			wantFlag: "-pids",
			wantText: "many",
		},
		{
			name:     "a pids limit of zero leaves no process to run",
			args:     []string{"-pids", "0"},
			wantFlag: "-pids",
			wantText: "no process to run",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			_, err := parseRunSpec(append(tt.args, "/bin/sh"))
			if err == nil {
				t.Fatal("parseRunSpec() = nil, want the limit to be rejected")
			}

			// Whatever the wording, it must classify as a bad limit so the
			// caller sees exit 1 rather than an internal error.
			if !errors.Is(err, cgroup.ErrInvalidLimit) {
				t.Errorf("error %v does not wrap cgroup.ErrInvalidLimit", err)
			}
			if !strings.Contains(err.Error(), tt.wantText) {
				t.Errorf("error = %q, want it to contain %q", err, tt.wantText)
			}
			if tt.wantFlag != "" && !strings.Contains(err.Error(), tt.wantFlag) {
				t.Errorf("error = %q, want it to name %q", err, tt.wantFlag)
			}
		})
	}
}

// TestRunCommandLimitErrorsExitUsage carries the check through the dispatcher:
// a bad limit is the caller's mistake, so it must exit 1 with a message on
// stderr and nothing on stdout.
func TestRunCommandLimitErrorsExitUsage(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		args       []string
		wantStderr string
	}{
		{
			name:       "unparseable memory",
			args:       []string{"run", "-memory", "lots", "/bin/sh"},
			wantStderr: "-memory",
		},
		{
			name:       "unparseable cpus",
			args:       []string{"run", "-cpus", "half", "/bin/sh"},
			wantStderr: "-cpus",
		},
		{
			name:       "cpu weight out of range",
			args:       []string{"run", "-cpu-weight", "99999", "/bin/sh"},
			wantStderr: "-cpu-weight",
		},
		{
			name:       "pids of zero",
			args:       []string{"run", "-pids", "0", "/bin/sh"},
			wantStderr: "-pids",
		},
		{
			name:       "a limit flag with no value",
			args:       []string{"run", "-memory"},
			wantStderr: "memory",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			a, stdout, stderr := newTestApp(commands()...)

			if got := a.run(t.Context(), tt.args); got != ExitUsage {
				t.Errorf("exit code = %d, want %d (stderr: %q)", got, ExitUsage, stderr)
			}
			if !strings.Contains(stderr.String(), tt.wantStderr) {
				t.Errorf("stderr = %q, want it to contain %q", stderr, tt.wantStderr)
			}
			if stdout.Len() != 0 {
				t.Errorf("stdout = %q, want it empty", stdout)
			}
		})
	}
}

// TestIsUserErrorClassifiesLimits pins the Stage 3 addition to isUserError. A
// limit the kernel would refuse is a bad argument however late it surfaces; a
// host without cgroup v2 is not, and must not send the user back to their
// command line.
func TestIsUserErrorClassifiesLimits(t *testing.T) {
	t.Parallel()

	userErrors := []error{
		cgroup.ErrInvalidLimit,
		// Wrapped as the runtime would deliver it, since that is how it
		// actually arrives at execRun.
		fmt.Errorf("creating the cgroup for container abc123: %w", cgroup.ErrInvalidLimit),
		runtime.ErrNoCommand,
		rootfs.ErrSourceNotFound,
	}
	for _, err := range userErrors {
		if !isUserError(err) {
			t.Errorf("isUserError(%v) = false, want true", err)
		}
	}

	forgeErrors := []error{
		cgroup.ErrUnifiedHierarchyNotMounted,
		cgroup.ErrControllerUnavailable,
		cgroup.ErrPermission,
		cgroup.ErrExists,
		cgroup.ErrNotEmpty,
		errors.New("something unexpected"),
	}
	for _, err := range forgeErrors {
		if isUserError(err) {
			t.Errorf("isUserError(%v) = true, want false", err)
		}
	}
}

// TestRunHelpListsLimitFlags keeps the four flags discoverable: `forge run -h`
// is the only place a user finds out they exist.
func TestRunHelpListsLimitFlags(t *testing.T) {
	t.Parallel()

	a, _, stderr := newTestApp(commands()...)

	if got := a.run(t.Context(), []string{"run", "-h"}); got != ExitOK {
		t.Errorf("exit code = %d, want %d", got, ExitOK)
	}

	got := stderr.String()
	for _, want := range []string{"-memory", "-cpus", "-cpu-weight", "-pids", "max"} {
		if !strings.Contains(got, want) {
			t.Errorf("run help does not mention %q:\n%s", want, got)
		}
	}
}

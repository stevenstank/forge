package cli

import (
	"strings"
	"testing"

	"github.com/stevenstank/forge/internal/network"
	"github.com/stevenstank/forge/internal/runtime"
)

// `forge run` needs root to actually start a container, so these tests cover
// the layer this package is responsible for: turning arguments into a Spec and
// a Status into an exit code (SSOT §13.6). The container behaviour itself is
// covered by test/integration.

func TestRunCommandArgumentErrors(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		args       []string
		wantStderr string
	}{
		{
			name:       "no command to run",
			args:       []string{"run"},
			wantStderr: "run requires a command to execute",
		},
		{
			name:       "unknown flag",
			args:       []string{"run", "-nonesuch", "/bin/echo"},
			wantStderr: "nonesuch",
		},
		{
			name:       "bare name is not a path",
			args:       []string{"run", "echo"},
			wantStderr: "does not search PATH",
		},
		{
			name:       "hostname flag requires a value",
			args:       []string{"run", "-hostname"},
			wantStderr: "hostname",
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
			// Diagnostics belong on stderr; stdout is the container's.
			if stdout.Len() != 0 {
				t.Errorf("stdout = %q, want it empty", stdout)
			}
		})
	}
}

// TestRunCommandRejectsInvalidHostnameBeforeForking confirms a bad hostname is
// caught in the parent, which is why this test needs no root.
func TestRunCommandRejectsInvalidHostnameBeforeForking(t *testing.T) {
	t.Parallel()

	a, _, stderr := newTestApp(commands()...)

	code := a.run(t.Context(), []string{"run", "-hostname", "bad hostname", "/bin/echo", "hi"})
	if code == ExitOK {
		t.Fatalf("exit code = %d, want a failure", code)
	}
	if !strings.Contains(stderr.String(), "hostname") {
		t.Errorf("stderr = %q, want it to mention the hostname", stderr)
	}
}

func TestRunCommandHelp(t *testing.T) {
	t.Parallel()

	a, _, stderr := newTestApp(commands()...)

	if got := a.run(t.Context(), []string{"run", "-h"}); got != ExitOK {
		t.Errorf("exit code = %d, want %d", got, ExitOK)
	}

	got := stderr.String()
	for _, want := range []string{
		"forge run", "<path>", "-hostname", "does not search PATH",
		"-rootfs", "-mount", "-read-only", "-workdir",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("run help does not mention %q:\n%s", want, got)
		}
	}
}

// --- Stage 2 --------------------------------------------------------------

// TestRunCommandStage2ArgumentErrors covers the flags Stage 2 adds. Everything
// here is rejected before a container is started, which is why it needs no root.
func TestRunCommandStage2ArgumentErrors(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		args       []string
		wantStderr string
	}{
		{
			name:       "mount without a rootfs",
			args:       []string{"run", "-mount", "/host/data:/data", "/bin/echo", "hi"},
			wantStderr: "rootfs",
		},
		{
			name:       "read-only without a rootfs",
			args:       []string{"run", "-read-only", "/bin/echo", "hi"},
			wantStderr: "rootfs",
		},
		{
			name:       "workdir without a rootfs",
			args:       []string{"run", "-workdir", "/data", "/bin/echo", "hi"},
			wantStderr: "rootfs",
		},
		{
			name:       "malformed mount spec",
			args:       []string{"run", "-rootfs", "/srv/alpine", "-mount", "/host/data", "/bin/sh"},
			wantStderr: "/host/data",
		},
		{
			name:       "unknown mount option",
			args:       []string{"run", "-rootfs", "/srv/alpine", "-mount", "/host/data:/data:rw", "/bin/sh"},
			wantStderr: "rw",
		},
		{
			name:       "relative rootfs",
			args:       []string{"run", "-rootfs", "images/alpine", "/bin/sh"},
			wantStderr: "absolute",
		},
		{
			name:       "relative workdir",
			args:       []string{"run", "-rootfs", "/srv/alpine", "-workdir", "data", "/bin/sh"},
			wantStderr: "absolute",
		},
		{
			name:       "mount destination escaping the container root",
			args:       []string{"run", "-rootfs", "/srv/alpine", "-mount", "/host/data:/../etc", "/bin/sh"},
			wantStderr: "/../etc",
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

// TestParseRunMountFlags pins that -mount is repeatable and that the parsed
// order is the order the user gave, which is what decides shadowing.
func TestParseRunMountFlags(t *testing.T) {
	t.Parallel()

	spec, err := parseRunSpec([]string{
		"-rootfs", "/srv/alpine",
		"-mount", "/host/one:/one",
		"-mount", "/host/two:/two:ro",
		"-read-only",
		"-workdir", "/one",
		"/bin/sh", "-c", "true",
	})
	if err != nil {
		t.Fatalf("parseRunSpec() = %v", err)
	}

	if spec.Rootfs != "/srv/alpine" {
		t.Errorf("Rootfs = %q, want %q", spec.Rootfs, "/srv/alpine")
	}
	if !spec.ReadonlyRoot {
		t.Error("ReadonlyRoot = false, want true")
	}
	if spec.WorkingDir != "/one" {
		t.Errorf("WorkingDir = %q, want %q", spec.WorkingDir, "/one")
	}
	if len(spec.Mounts) != 2 {
		t.Fatalf("got %d mounts, want 2", len(spec.Mounts))
	}
	if spec.Mounts[0].Destination != "/one" || spec.Mounts[1].Destination != "/two" {
		t.Errorf("mount order = %q, %q; want /one then /two",
			spec.Mounts[0].Destination, spec.Mounts[1].Destination)
	}
	if !spec.Mounts[1].NeedsRemount() {
		t.Error("the second mount is not read-only; the :ro suffix was dropped")
	}

	// The command is everything after the flags, unchanged.
	if strings.Join(spec.Command, " ") != "/bin/sh -c true" {
		t.Errorf("Command = %q, want %q", spec.Command, "/bin/sh -c true")
	}
}

// TestParseRunSpecWithoutStage2Flags confirms the Stage 1 invocation still
// produces a Stage 1 spec: no rootfs, no mounts, nothing to pivot into.
func TestParseRunSpecWithoutStage2Flags(t *testing.T) {
	t.Parallel()

	spec, err := parseRunSpec([]string{"/bin/echo", "hello"})
	if err != nil {
		t.Fatalf("parseRunSpec() = %v", err)
	}

	if spec.Rootfs != "" {
		t.Errorf("Rootfs = %q, want empty", spec.Rootfs)
	}
	if len(spec.Mounts) != 0 {
		t.Errorf("Mounts = %v, want none", spec.Mounts)
	}
	if spec.ReadonlyRoot {
		t.Error("ReadonlyRoot = true, want false")
	}
	if spec.WorkingDir != "" {
		t.Errorf("WorkingDir = %q, want empty", spec.WorkingDir)
	}
}

// TestDefaultContainerEnvIsMinimal pins the Stage 1 promise that a container's
// environment is explicit rather than inherited from the host.
func TestDefaultContainerEnvIsMinimal(t *testing.T) {
	t.Parallel()

	got := defaultContainerEnv()

	if len(got) != 1 {
		t.Fatalf("defaultContainerEnv() = %q, want exactly one entry", got)
	}
	if !strings.HasPrefix(got[0], "PATH=") {
		t.Errorf("defaultContainerEnv() = %q, want a PATH entry", got)
	}
}

// TestInitCommandIsWiredToTheRuntime pins the contract ADR-0008 depends on: the
// name Forge re-executes itself with must be the one the runtime looks for, and
// it must stay hidden from users.
//
// The command is not executed here — doing so would read descriptor 3, whose
// contents in a test binary are undefined. Its behaviour is covered end to end
// by test/integration.
func TestInitCommandIsWiredToTheRuntime(t *testing.T) {
	t.Parallel()

	cmd := newInitCommand()

	if cmd.Name != runtime.InitCommandName {
		t.Errorf("init command name = %q, want %q", cmd.Name, runtime.InitCommandName)
	}
	if !cmd.Hidden {
		t.Error("init command is visible; it must not appear in help output")
	}
	if cmd.Exec == nil {
		t.Error("init command has no implementation")
	}
	if !runtime.IsInitCommand([]string{"forge", cmd.Name}) {
		t.Errorf("runtime does not recognise %q as its init command", cmd.Name)
	}
}

// --- Stage 4 --------------------------------------------------------------

// TestParseRunSpecNetworkFlags covers the only thing this package contributes
// to Stage 4: carrying the flags through untranslated. An unset -network stays
// empty rather than being resolved here, because what a container gets when
// nobody asks is the runtime's decision (SSOT §2).
func TestParseRunSpecNetworkFlags(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		args    []string
		want    network.Mode
		wantMTU int
	}{
		{
			name: "unset stays empty for the runtime to resolve",
			args: []string{"/bin/echo", "hi"},
			want: "",
		},
		{
			name: "an explicit mode is passed through",
			args: []string{"-network", "none", "/bin/sh"},
			want: network.ModeNone,
		},
		{
			name:    "an mtu is passed through",
			args:    []string{"-mtu", "1400", "/bin/sh"},
			want:    "",
			wantMTU: 1400,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			spec, err := parseRunSpec(tt.args)
			if err != nil {
				t.Fatalf("parseRunSpec() = %v", err)
			}
			if spec.Network != tt.want {
				t.Errorf("Network = %q, want %q", spec.Network, tt.want)
			}
			if spec.NetworkMTU != tt.wantMTU {
				t.Errorf("NetworkMTU = %d, want %d", spec.NetworkMTU, tt.wantMTU)
			}
		})
	}
}

// TestRunCommandStage4ArgumentErrors confirms a bad network flag is rejected
// before anything is started, and reported as the usage error it is.
func TestRunCommandStage4ArgumentErrors(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		args       []string
		wantStderr string
	}{
		{
			name:       "a mode forge does not implement",
			args:       []string{"run", "-network", "macvlan", "/bin/sh"},
			wantStderr: "macvlan",
		},
		{
			name:       "an mtu with no interface to put it on",
			args:       []string{"run", "-network", "host", "-mtu", "1400", "/bin/sh"},
			wantStderr: "-mtu needs an interface",
		},
		{
			name:       "an mtu the kernel would refuse",
			args:       []string{"run", "-mtu", "70000", "/bin/sh"},
			wantStderr: "invalid MTU",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			a, _, stderr := newTestApp(commands()...)

			if got := a.run(t.Context(), tt.args); got != ExitUsage {
				t.Errorf("exit code = %d, want %d (stderr: %q)", got, ExitUsage, stderr)
			}
			if !strings.Contains(stderr.String(), tt.wantStderr) {
				t.Errorf("stderr = %q, want it to contain %q", stderr, tt.wantStderr)
			}
		})
	}
}

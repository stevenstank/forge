//go:build integration

package integration

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"testing"

	"github.com/stevenstank/forge/internal/logging"
	"github.com/stevenstank/forge/internal/process"
	"github.com/stevenstank/forge/internal/runtime"
)

// The integration tests run containers against the real kernel. The container's
// binary is always this test binary re-executed in a helper mode, which keeps
// the tests independent of whatever binaries the host happens to have.
//
// Helper modes are selected by an environment variable and dispatched per stage
// from TestMain, so each stage's helpers live beside the tests that use them.

const (
	helperEnv     = "FORGE_INTEGRATION_HELPER"
	helperDirEnv  = "FORGE_INTEGRATION_DIR"
	helperCodeEnv = "FORGE_INTEGRATION_EXIT_CODE"
	helperPathEnv = "FORGE_INTEGRATION_PATH"
	helperDataEnv = "FORGE_INTEGRATION_DATA"
)

// readyMarker is what a long-running helper prints once it is up, so tests can
// synchronise on it instead of sleeping (SSOT §7).
const readyMarker = "ready"

// unknownHelperExitCode reports a mode no stage claimed, which is a bug in the
// test rather than a container failure.
const unknownHelperExitCode = 252

func TestMain(m *testing.M) {
	// This binary plays the role cmd/forge plays in production: Forge starts a
	// container by re-executing the *current* executable as its container init,
	// so the test binary must route that command to runtime.Init exactly as the
	// real CLI does. Without this it would re-enter the test suite inside every
	// container it starts. See ADR-0008.
	if runtime.IsInitCommand(os.Args) {
		if err := runtime.Init(); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(runtime.InitExitCode)
		}
		os.Exit(runtime.InitExitCode)
	}

	mode := os.Getenv(helperEnv)
	if mode == "" {
		os.Exit(m.Run())
	}

	for _, dispatch := range []func(string) (int, bool){stage1Helper, stage2Helper} {
		if code, handled := dispatch(mode); handled {
			os.Exit(code)
		}
	}

	fmt.Fprintf(os.Stderr, "unknown helper mode %q\n", mode)
	os.Exit(unknownHelperExitCode)
}

// requireRoot skips a test that cannot run without CAP_SYS_ADMIN.
func requireRoot(t *testing.T) {
	t.Helper()

	if os.Geteuid() != 0 {
		t.Skip("integration tests need root: run `sudo -E make test-integration`")
	}
}

// result is everything a test wants to know about a finished container.
type result struct {
	status process.Status
	stdout string
	stderr string
}

// runContainer runs spec to completion and returns its output. A container that
// fails to start is a test failure; a container that exits non-zero is not.
func runContainer(ctx context.Context, t *testing.T, spec runtime.Spec) result {
	t.Helper()

	return runContainerIn(ctx, t, t.TempDir(), spec)
}

// runContainerIn is runContainer with an explicit rootfs storage root, for
// tests that inspect what Forge left there.
func runContainerIn(ctx context.Context, t *testing.T, root string, spec runtime.Spec) result {
	t.Helper()

	var stdout, stderr, logs bytes.Buffer
	spec.Stdout = &stdout
	spec.Stderr = &stderr

	runner, err := runtime.NewRunner(logging.New(&logs, slog.LevelDebug), runtime.Config{Root: root})
	if err != nil {
		t.Fatalf("NewRunner() = %v", err)
	}

	status, err := runner.Run(ctx, spec)
	t.Logf("forge log:\n%s", logs.String())
	if err != nil {
		t.Fatalf("Run() = %v\ncontainer stderr: %s", err, stderr.String())
	}

	return result{
		status: status,
		stdout: strings.TrimSpace(stdout.String()),
		stderr: strings.TrimSpace(stderr.String()),
	}
}

// helperSpec returns a Spec that runs this test binary in the given mode,
// directly from the host filesystem. Stage 2's rootfs-based equivalent is
// rootfsSpec.
func helperSpec(t *testing.T, mode string, env ...string) runtime.Spec {
	t.Helper()

	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable() = %v", err)
	}

	return runtime.Spec{
		Command: []string{exe},
		Env:     append([]string{helperEnv + "=" + mode}, env...),
	}
}

// hostNamespace returns the inode identity of one of the test process's
// namespaces, for comparison against a container's.
func hostNamespace(t *testing.T, kind string) string {
	t.Helper()

	link, err := os.Readlink("/proc/self/ns/" + kind)
	if err != nil {
		t.Fatalf("reading host %s namespace: %v", kind, err)
	}
	return link
}

// hostMountCount returns the number of entries in the host's mount table.
func hostMountCount(t *testing.T) int {
	t.Helper()

	data, err := os.ReadFile("/proc/self/mountinfo")
	if err != nil {
		t.Fatalf("reading host mountinfo: %v", err)
	}
	return len(strings.Split(strings.TrimSpace(string(data)), "\n"))
}

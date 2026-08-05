package runtime

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/stevenstank/forge/internal/network"
)

// prepareNetwork holds Stage 4's orchestration decisions, and like Stage 3's
// they are invisible from outside the package: which modes reach
// internal/network at all, and whether the teardown was registered the moment
// anything was created. These tests reach in and check.
//
// Everything here runs unprivileged. The paths that need CAP_NET_ADMIN — the
// bridge, the veth pair, the move into the namespace — belong to the privileged
// integration suite; what is asserted here is the sequencing around them.

// newNetworkTestRunner builds a Runner whose lease directory is a temp
// directory, so nothing touches Forge's real network state.
func newNetworkTestRunner(t *testing.T) *Runner {
	t.Helper()

	runner, err := NewRunner(discardLogger(), Config{
		Root:       filepath.Join(t.TempDir(), "containers"),
		CgroupRoot: t.TempDir(),
		Network:    network.Config{StateDir: t.TempDir()},
	})
	if err != nil {
		t.Fatalf("NewRunner() = %v", err)
	}

	return runner
}

// TestSpecNetworkModeDefault pins the decision that a container which asks for
// nothing is networked, which is what makes FR-4.1 the default rather than an
// opt-in.
func TestSpecNetworkModeDefault(t *testing.T) {
	t.Parallel()

	if got := (Spec{}).NetworkMode(); got != network.ModeBridge {
		t.Errorf("NetworkMode() = %q, want %q", got, network.ModeBridge)
	}
	if got := (Spec{Network: network.ModeHost}).NetworkMode(); got != network.ModeHost {
		t.Errorf("NetworkMode() = %q, want the mode the caller asked for", got)
	}
}

// TestPrepareNetworkCreatesNothingWithoutAVeth covers both modes that hold no
// host resources. Neither may reach internal/network, and neither may register
// a cleanup: there is nothing to release, and a Destroy for a container that
// never allocated would be a lie on the stack.
func TestPrepareNetworkCreatesNothingWithoutAVeth(t *testing.T) {
	t.Parallel()

	for _, mode := range []network.Mode{network.ModeHost, network.ModeNone} {
		t.Run(string(mode), func(t *testing.T) {
			t.Parallel()

			runner := newNetworkTestRunner(t)
			cleanup := newCleanupStack(discardLogger())

			spec := Spec{Command: []string{"/bin/true"}, Network: mode}

			got, err := runner.prepareNetwork(discardLogger(), "abc123", spec, cleanup)
			if err != nil {
				t.Fatalf("prepareNetwork() = %v", err)
			}
			if got.mode != mode {
				t.Errorf("mode = %q, want %q", got.mode, mode)
			}
			if got.alloc != nil {
				t.Errorf("alloc = %+v, want nothing allocated for %q", got.alloc, mode)
			}
			if got.iface() != nil {
				t.Errorf("iface() = %+v, want nothing for the container to configure", got.iface())
			}
			if len(cleanup.fns) != 0 {
				t.Errorf("cleanup stack has %d steps, want none: nothing was created", len(cleanup.fns))
			}
		})
	}
}

// TestAttachNetworkIsANoOpWithoutAnAllocation is the other half of the same
// decision: a container with no interface must not be handed one, and must not
// fail for the lack of a bridge it never asked for.
func TestAttachNetworkIsANoOpWithoutAnAllocation(t *testing.T) {
	t.Parallel()

	runner := newNetworkTestRunner(t)

	// A PID that is deliberately not a running process: reaching the kernel at
	// all would be the bug this test exists to catch.
	if err := runner.attachNetwork(discardLogger(), containerNetwork{mode: network.ModeHost}, -1); err != nil {
		t.Errorf("attachNetwork() = %v, want nothing done for a container with no interface", err)
	}
}

// TestNetworkTeardownUnwindsBeforeTheCgroup pins the ordering the whole stage
// depends on: the network is registered last, so it is released first, before
// the cgroup and long before the rootfs (SSOT §11.3).
//
// It uses the stack directly rather than prepareNetwork because reaching the
// registration needs a bridge, and the property under test is the order, not
// what created it.
func TestNetworkTeardownUnwindsBeforeTheCgroup(t *testing.T) {
	t.Parallel()

	var released []string
	cleanup := newCleanupStack(discardLogger())

	record := func(what string) func() error {
		return func() error {
			released = append(released, what)
			return nil
		}
	}

	// The registration order of Run: filesystem, cgroup, network.
	cleanup.push("removing the container root filesystem", record("rootfs"))
	cleanup.push("removing the container cgroup", record("cgroup"))
	cleanup.push("releasing the container network", record("network"))

	cleanup.unwind()

	want := []string{"network", "cgroup", "rootfs"}
	if strings.Join(released, ",") != strings.Join(want, ",") {
		t.Errorf("released %v, want %v", released, want)
	}
}

// TestPrepareNetworkWithoutAVethNeverReachesIPAM proves the short-circuit is a
// real one rather than an allocation that happens to be discarded. An ID that
// IPAM would refuse outright passes through untouched, because nothing looks at
// it: no lease directory is read, no address is claimed, no name is derived.
func TestPrepareNetworkWithoutAVethNeverReachesIPAM(t *testing.T) {
	t.Parallel()

	runner := newNetworkTestRunner(t)
	cleanup := newCleanupStack(discardLogger())

	spec := Spec{Command: []string{"/bin/true"}, Network: network.ModeNone}

	if _, err := runner.prepareNetwork(discardLogger(), "../escape", spec, cleanup); err != nil {
		t.Fatalf("prepareNetwork() = %v, want none mode to ignore the id entirely", err)
	}
	if len(cleanup.fns) != 0 {
		t.Errorf("cleanup stack has %d steps, want none", len(cleanup.fns))
	}
}

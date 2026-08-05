package network

import (
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/stevenstank/forge/internal/logging"
)

func testManager(t *testing.T, subnet string) *Manager {
	t.Helper()

	m, err := New(logging.New(io.Discard, slog.LevelError), Config{
		Subnet:   subnet,
		StateDir: t.TempDir(),
	})
	if err != nil {
		t.Fatalf("New() = %v", err)
	}

	return m
}

// writeLease plants a lease directly, which is how the reclaimer is tested
// without having to create interfaces or wait for processes to die.
func writeLease(t *testing.T, m *Manager, ip string, l lease) {
	t.Helper()

	if err := os.MkdirAll(m.leaseDir(), 0o755); err != nil {
		t.Fatalf("creating the lease directory: %v", err)
	}
	if err := os.WriteFile(m.leasePath(ip), []byte(l.encode()), 0o644); err != nil {
		t.Fatalf("writing the lease %s: %v", ip, err)
	}
}

func TestLeaseRoundTrip(t *testing.T) {
	t.Parallel()

	original := lease{containerID: "abc123def456", pid: 4242}

	got := decodeLease(original.encode())
	if got != original {
		t.Errorf("decodeLease(encode()) = %+v, want %+v", got, original)
	}
}

// A lease file written by an older Forge has no PID line. It must still parse,
// and must fall back to being judged on its interface alone rather than being
// treated as corrupt.
func TestDecodeLeaseWithoutAPID(t *testing.T) {
	t.Parallel()

	got := decodeLease("abc123def456\n")

	if got.containerID != "abc123def456" {
		t.Errorf("containerID = %q, want %q", got.containerID, "abc123def456")
	}
	if got.pid != 0 {
		t.Errorf("pid = %d, want 0", got.pid)
	}
	if pidAlive(got.pid) {
		t.Error("pid 0 reported as alive; it must never be")
	}
}

func TestPidAlive(t *testing.T) {
	t.Parallel()

	if !pidAlive(os.Getpid()) {
		t.Error("this test process reported as not alive")
	}
	for _, pid := range []int{0, -1} {
		if pidAlive(pid) {
			t.Errorf("pidAlive(%d) = true, want false", pid)
		}
	}
}

// The reclaimer is the pool's only defence against a Forge that was SIGKILLed,
// and its only real hazard is being too eager. These two tests pin both sides.
func TestReclaimStaleTakesBackDeadLeases(t *testing.T) {
	t.Parallel()

	m := testManager(t, "10.99.0.0/16")
	writeLease(t, m, "10.99.0.2", lease{containerID: "deadaaaaaaaa", pid: 999999})
	writeLease(t, m, "10.99.0.3", lease{containerID: "deadbbbbbbbb", pid: 999998})

	reclaimed, err := m.reclaimStale(func(lease) bool { return false })
	if err != nil {
		t.Fatalf("reclaimStale() = %v", err)
	}
	if reclaimed != 2 {
		t.Errorf("reclaimed %d leases, want 2", reclaimed)
	}

	taken, err := m.takenAddresses()
	if err != nil {
		t.Fatalf("takenAddresses() = %v", err)
	}
	if len(taken) != 0 {
		t.Errorf("%d leases survived the reclaim, want 0", len(taken))
	}
}

func TestReclaimStaleLeavesLiveLeasesAlone(t *testing.T) {
	t.Parallel()

	m := testManager(t, "10.99.0.0/16")
	writeLease(t, m, "10.99.0.2", lease{containerID: "liveaaaaaaaa", pid: os.Getpid()})

	reclaimed, err := m.reclaimStale(leaseAlive)
	if err != nil {
		t.Fatalf("reclaimStale() = %v", err)
	}
	if reclaimed != 0 {
		t.Errorf("reclaimed %d leases, want 0: the holder is still running", reclaimed)
	}
}

// This is the regression that matters most. A container is allocated an address
// before its veth exists, so for a moment it is a lease with no interface. A
// reclaimer that judged on the interface alone would hand that address to
// somebody else and produce two containers sharing one address.
func TestReclaimStaleDoesNotStealFromAContainerThatIsStillStarting(t *testing.T) {
	t.Parallel()

	m := testManager(t, "10.99.0.0/16")

	// Exactly the state between Allocate and Attach: a lease held by this live
	// process, and no interface anywhere.
	if _, err := m.Allocate("starting0000"); err != nil {
		t.Fatalf("Allocate() = %v", err)
	}
	if linkExists("fhstarting0000") {
		t.Skip("an interface named fhstarting0000 exists on this host; the premise does not hold")
	}

	reclaimed, err := m.reclaimStale(leaseAlive)
	if err != nil {
		t.Fatalf("reclaimStale() = %v", err)
	}
	if reclaimed != 0 {
		t.Fatalf("reclaimed %d leases, want 0: the container is mid-start, not dead", reclaimed)
	}

	if _, found, err := m.Lookup("starting0000"); err != nil || !found {
		t.Errorf("the starting container lost its lease: (found %v, err %v)", found, err)
	}
}

// A pool that is genuinely full of dead leases must recover rather than stay
// exhausted forever, or one SIGKILLed Forge poisons the host.
func TestClaimRecoversFromAPoolFullOfDeadLeases(t *testing.T) {
	t.Parallel()

	m := testManager(t, "10.0.0.0/30")
	writeLease(t, m, "10.0.0.2", lease{containerID: "deadaaaaaaaa", pid: 999999})

	ip, err := m.claim("newcontainer")
	if err != nil {
		t.Fatalf("claim() = %v", err)
	}
	if ip != "10.0.0.2" {
		t.Errorf("claim() = %q, want the reclaimed %q", ip, "10.0.0.2")
	}

	held, err := m.readLease("10.0.0.2")
	if err != nil {
		t.Fatalf("readLease() = %v", err)
	}
	if held.containerID != "newcontainer" {
		t.Errorf("lease holder = %q, want %q", held.containerID, "newcontainer")
	}
}

func TestTakenAddressesOnAMissingDirectory(t *testing.T) {
	t.Parallel()

	m := testManager(t, "10.99.0.0/16")

	// Nothing has been allocated, so the directory does not exist yet. That is
	// an empty pool, not an error.
	taken, err := m.takenAddresses()
	if err != nil {
		t.Fatalf("takenAddresses() = %v", err)
	}
	if len(taken) != 0 {
		t.Errorf("got %d taken addresses, want 0", len(taken))
	}
}

// Allocate must never write outside its own state directory, whatever it is
// handed. The ID is checked before any path is built from it.
func TestAllocateContainsItsPaths(t *testing.T) {
	t.Parallel()

	m := testManager(t, "10.99.0.0/16")
	outside := filepath.Join(m.stateDir, "..", "escaped")

	if _, err := m.Allocate("../escaped"); err == nil {
		t.Fatal("Allocate() with a traversing id succeeded")
	}
	if _, err := os.Stat(outside); !os.IsNotExist(err) {
		t.Errorf("something was created at %s", outside)
	}
}

func TestSubnetAddressAccessor(t *testing.T) {
	t.Parallel()

	parsed, err := parseCIDR("10.99.0.7/16")
	if err != nil {
		t.Fatalf("parseCIDR() = %v", err)
	}

	if got := parsed.address(); got != "10.99.0.7" {
		t.Errorf("address() = %q, want %q", got, "10.99.0.7")
	}
	if got := parsed.PrefixLen(); got != 16 {
		t.Errorf("PrefixLen() = %d, want 16", got)
	}
}

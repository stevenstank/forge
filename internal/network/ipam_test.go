package network_test

import (
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"testing"

	"github.com/stevenstank/forge/internal/logging"
	"github.com/stevenstank/forge/internal/network"
)

func discardLogger() *slog.Logger { return logging.New(io.Discard, slog.LevelError) }

// newManager returns a Manager whose leases live in a temporary directory, so
// the whole of IPAM runs unprivileged against real files.
func newManager(t *testing.T, subnet string) *network.Manager {
	t.Helper()

	m, err := network.New(discardLogger(), network.Config{
		Subnet:   subnet,
		StateDir: t.TempDir(),
	})
	if err != nil {
		t.Fatalf("New() = %v", err)
	}

	return m
}

func TestParseSubnet(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		cidr        string
		wantGateway string
		wantSize    int
		wantErr     bool
	}{
		{name: "a /16", cidr: "10.99.0.0/16", wantGateway: "10.99.0.1", wantSize: 65533},
		{name: "a /24", cidr: "192.168.5.0/24", wantGateway: "192.168.5.1", wantSize: 253},
		{name: "a /30", cidr: "10.0.0.0/30", wantGateway: "10.0.0.1", wantSize: 1},
		{name: "surrounding space is trimmed", cidr: "  10.99.0.0/16  ", wantGateway: "10.99.0.1", wantSize: 65533},
		{name: "not a CIDR", cidr: "10.99.0.0", wantErr: true},
		{name: "empty", cidr: "", wantErr: true},
		{name: "nonsense", cidr: "banana", wantErr: true},
		{name: "IPv6 is not supported", cidr: "fd00::/64", wantErr: true},
		{name: "a /31 leaves no room", cidr: "10.0.0.0/31", wantErr: true},
		{name: "a /32 leaves no room", cidr: "10.0.0.0/32", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			subnet, err := network.ParseSubnet(tt.cidr)
			if tt.wantErr {
				if !errors.Is(err, network.ErrInvalidSubnet) {
					t.Fatalf("ParseSubnet(%q) = %v, want %v", tt.cidr, err, network.ErrInvalidSubnet)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseSubnet(%q) = %v", tt.cidr, err)
			}

			if got := subnet.Gateway(); got != tt.wantGateway {
				t.Errorf("Gateway() = %q, want %q", got, tt.wantGateway)
			}
			if got := subnet.Size(); got != tt.wantSize {
				t.Errorf("Size() = %d, want %d", got, tt.wantSize)
			}
		})
	}
}

func TestSubnetGatewayCIDR(t *testing.T) {
	t.Parallel()

	subnet, err := network.ParseSubnet("10.99.0.0/16")
	if err != nil {
		t.Fatalf("ParseSubnet() = %v", err)
	}

	if got := subnet.GatewayCIDR(); got != "10.99.0.1/16" {
		t.Errorf("GatewayCIDR() = %q, want %q", got, "10.99.0.1/16")
	}
	if got := subnet.String(); got != "10.99.0.0/16" {
		t.Errorf("String() = %q, want %q", got, "10.99.0.0/16")
	}
}

func TestSubnetContains(t *testing.T) {
	t.Parallel()

	subnet, err := network.ParseSubnet("10.99.0.0/16")
	if err != nil {
		t.Fatalf("ParseSubnet() = %v", err)
	}

	tests := map[string]bool{
		"10.99.0.1":   true,
		"10.99.255.4": true,
		"10.98.0.1":   false,
		"10.100.0.1":  false,
		"192.168.1.1": false,
		"not an ip":   false,
		"":            false,
	}

	for ip, want := range tests {
		if got := subnet.Contains(ip); got != want {
			t.Errorf("Contains(%q) = %v, want %v", ip, got, want)
		}
	}
}

// Hosts must skip the network address, the gateway, and the broadcast address.
// Handing any of the three to a container produces a network that looks
// configured and does not work.
func TestSubnetHostsSkipsReservedAddresses(t *testing.T) {
	t.Parallel()

	subnet, err := network.ParseSubnet("10.0.0.0/29")
	if err != nil {
		t.Fatalf("ParseSubnet() = %v", err)
	}

	got := slices.Collect(subnet.Hosts())
	want := []string{"10.0.0.2", "10.0.0.3", "10.0.0.4", "10.0.0.5", "10.0.0.6"}

	if !slices.Equal(got, want) {
		t.Errorf("Hosts() = %v, want %v", got, want)
	}
	if len(got) != subnet.Size() {
		t.Errorf("Hosts() yielded %d addresses but Size() says %d", len(got), subnet.Size())
	}
}

func TestSubnetHostsStopsEarlyWhenAsked(t *testing.T) {
	t.Parallel()

	subnet, err := network.ParseSubnet("10.99.0.0/16")
	if err != nil {
		t.Fatalf("ParseSubnet() = %v", err)
	}

	// A /16 has 65533 hosts; the iterator must honour an early break rather
	// than walking all of them.
	seen := 0
	for range subnet.Hosts() {
		seen++
		if seen == 3 {
			break
		}
	}

	if seen != 3 {
		t.Errorf("saw %d addresses, want 3", seen)
	}
}

// New performs no I/O. It is called once per Runner, before anything is known
// about the host, so it must not touch the filesystem or the kernel.
func TestNewPerformsNoIO(t *testing.T) {
	t.Parallel()

	dir := filepath.Join(t.TempDir(), "does-not-exist")

	m, err := network.New(discardLogger(), network.Config{StateDir: dir})
	if err != nil {
		t.Fatalf("New() = %v", err)
	}
	if _, err := os.Stat(dir); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("New() created %s; it must perform no I/O", dir)
	}
	if got := m.StateDir(); got != dir {
		t.Errorf("StateDir() = %q, want %q", got, dir)
	}
}

func TestNewDefaults(t *testing.T) {
	t.Parallel()

	m, err := network.New(discardLogger(), network.Config{})
	if err != nil {
		t.Fatalf("New() = %v", err)
	}

	if got := m.Bridge(); got != network.DefaultBridge {
		t.Errorf("Bridge() = %q, want %q", got, network.DefaultBridge)
	}
	if got := m.StateDir(); got != network.DefaultStateDir {
		t.Errorf("StateDir() = %q, want %q", got, network.DefaultStateDir)
	}
	if got := m.Subnet().String(); got != network.DefaultSubnet {
		t.Errorf("Subnet() = %q, want %q", got, network.DefaultSubnet)
	}
}

func TestNewRejectsBadConfig(t *testing.T) {
	t.Parallel()

	if _, err := network.New(discardLogger(), network.Config{Subnet: "nonsense"}); !errors.Is(err, network.ErrInvalidSubnet) {
		t.Errorf("New() with a bad subnet = %v, want %v", err, network.ErrInvalidSubnet)
	}
	if _, err := network.New(discardLogger(), network.Config{Bridge: "this-name-is-far-too-long"}); !errors.Is(err, network.ErrInvalidInterface) {
		t.Errorf("New() with an over-long bridge name = %v, want %v", err, network.ErrInvalidInterface)
	}
}

func TestAllocateProducesACompletePlan(t *testing.T) {
	t.Parallel()

	m := newManager(t, "10.99.0.0/16")

	n, err := m.Allocate("abc123def456")
	if err != nil {
		t.Fatalf("Allocate() = %v", err)
	}

	if n.ContainerID != "abc123def456" {
		t.Errorf("ContainerID = %q, want %q", n.ContainerID, "abc123def456")
	}
	if n.HostVeth != "fhabc123def456" || n.PeerVeth != "fcabc123def456" {
		t.Errorf("veth names = %q/%q, want fh…/fc…", n.HostVeth, n.PeerVeth)
	}
	// The first container gets the first address after the gateway.
	if n.Interface.Address != "10.99.0.2/16" {
		t.Errorf("Address = %q, want %q", n.Interface.Address, "10.99.0.2/16")
	}
	if n.Interface.Gateway != "10.99.0.1" {
		t.Errorf("Gateway = %q, want %q", n.Interface.Gateway, "10.99.0.1")
	}
	if n.Interface.Source != n.PeerVeth {
		t.Errorf("Source = %q, want the peer end %q", n.Interface.Source, n.PeerVeth)
	}
	if n.Interface.Name != network.ContainerIfaceName {
		t.Errorf("Name = %q, want %q", n.Interface.Name, network.ContainerIfaceName)
	}
	if err := n.Interface.Validate(); err != nil {
		t.Errorf("the plan Allocate produced does not validate: %v", err)
	}
}

func TestAllocateGivesEachContainerADifferentAddress(t *testing.T) {
	t.Parallel()

	m := newManager(t, "10.99.0.0/16")

	seen := map[string]string{}
	for i := range 20 {
		id := fmt.Sprintf("container%04d", i)
		n, err := m.Allocate(id)
		if err != nil {
			t.Fatalf("Allocate(%q) = %v", id, err)
		}
		if previous, taken := seen[n.Interface.Address]; taken {
			t.Fatalf("%s and %s were both given %s", previous, id, n.Interface.Address)
		}
		seen[n.Interface.Address] = id
	}
}

func TestAllocateRefusesASecondAddressForOneContainer(t *testing.T) {
	t.Parallel()

	m := newManager(t, "10.99.0.0/16")

	if _, err := m.Allocate("abc123def456"); err != nil {
		t.Fatalf("Allocate() = %v", err)
	}
	if _, err := m.Allocate("abc123def456"); !errors.Is(err, network.ErrAlreadyAllocated) {
		t.Errorf("second Allocate() = %v, want %v", err, network.ErrAlreadyAllocated)
	}
}

func TestAllocateRejectsBadContainerIDs(t *testing.T) {
	t.Parallel()

	m := newManager(t, "10.99.0.0/16")

	for _, id := range []string{"", "..", "a/b", `a\b`, "a\x00b"} {
		if _, err := m.Allocate(id); !errors.Is(err, network.ErrInvalidContainerID) {
			t.Errorf("Allocate(%q) = %v, want %v", id, err, network.ErrInvalidContainerID)
		}
	}
}

func TestAllocateReportsAnExhaustedPool(t *testing.T) {
	t.Parallel()

	// A /30 has exactly one allocatable address.
	m := newManager(t, "10.0.0.0/30")

	if _, err := m.Allocate("first0000000"); err != nil {
		t.Fatalf("first Allocate() = %v", err)
	}
	if _, err := m.Allocate("second000000"); !errors.Is(err, network.ErrPoolExhausted) {
		t.Errorf("second Allocate() = %v, want %v", err, network.ErrPoolExhausted)
	}
}

func TestLookupFindsAndMissesCorrectly(t *testing.T) {
	t.Parallel()

	m := newManager(t, "10.99.0.0/16")

	if _, found, err := m.Lookup("abc123def456"); err != nil || found {
		t.Errorf("Lookup() before allocation = (found %v, err %v), want (false, nil)", found, err)
	}

	n, err := m.Allocate("abc123def456")
	if err != nil {
		t.Fatalf("Allocate() = %v", err)
	}

	ip, found, err := m.Lookup("abc123def456")
	if err != nil {
		t.Fatalf("Lookup() = %v", err)
	}
	if !found {
		t.Fatal("Lookup() did not find an address that was just allocated")
	}
	if want := "10.99.0.2"; ip != want {
		t.Errorf("Lookup() = %q, want %q", ip, want)
	}
	if !m.Subnet().Contains(ip) {
		t.Errorf("Lookup() returned %q, which is outside %s", ip, m.Subnet())
	}
	_ = n
}

// Destroy is registered on the orchestrator's cleanup stack the moment Allocate
// succeeds, so it has to be correct however many times it runs and whatever
// state the container reached.
func TestDestroyIsIdempotent(t *testing.T) {
	t.Parallel()

	m := newManager(t, "10.99.0.0/16")

	if _, err := m.Allocate("abc123def456"); err != nil {
		t.Fatalf("Allocate() = %v", err)
	}

	for attempt := range 3 {
		if err := m.Destroy("abc123def456"); err != nil {
			t.Fatalf("Destroy() attempt %d = %v", attempt+1, err)
		}
		if _, found, err := m.Lookup("abc123def456"); err != nil || found {
			t.Fatalf("after Destroy() attempt %d: (found %v, err %v), want (false, nil)", attempt+1, found, err)
		}
	}
}

func TestDestroyOnAContainerThatNeverAllocated(t *testing.T) {
	t.Parallel()

	m := newManager(t, "10.99.0.0/16")

	if err := m.Destroy("never000000"); err != nil {
		t.Errorf("Destroy() on an unknown container = %v, want nil", err)
	}
}

func TestDestroyRejectsBadContainerIDs(t *testing.T) {
	t.Parallel()

	m := newManager(t, "10.99.0.0/16")

	// A traversing ID must be refused rather than turned into a path that
	// unlinks something outside the lease directory.
	if err := m.Destroy("../../etc/passwd"); !errors.Is(err, network.ErrInvalidContainerID) {
		t.Errorf("Destroy() with a traversing id = %v, want %v", err, network.ErrInvalidContainerID)
	}
}

// The address returned by Destroy must go back into the pool, or a long-running
// host leaks its subnet one container at a time.
func TestDestroyReturnsTheAddressToThePool(t *testing.T) {
	t.Parallel()

	m := newManager(t, "10.0.0.0/30")

	first, err := m.Allocate("first0000000")
	if err != nil {
		t.Fatalf("Allocate() = %v", err)
	}
	if err := m.Destroy("first0000000"); err != nil {
		t.Fatalf("Destroy() = %v", err)
	}

	second, err := m.Allocate("second000000")
	if err != nil {
		t.Fatalf("Allocate() after Destroy() = %v", err)
	}
	if second.Interface.Address != first.Interface.Address {
		t.Errorf("reallocated %q, want the released %q", second.Interface.Address, first.Interface.Address)
	}
}

// Two forge processes starting at the same instant must never be handed the
// same address. O_EXCL is what guarantees it, and this is the test that would
// notice if that were ever softened to a check-then-create.
func TestConcurrentAllocationsNeverCollide(t *testing.T) {
	t.Parallel()

	m := newManager(t, "10.99.0.0/16")

	const containers = 50

	var (
		wg        sync.WaitGroup
		mu        sync.Mutex
		addresses = make(map[string]string, containers)
		failures  []error
	)

	for i := range containers {
		wg.Add(1)
		go func() {
			defer wg.Done()

			id := fmt.Sprintf("parallel%04d", i)
			n, err := m.Allocate(id)

			mu.Lock()
			defer mu.Unlock()

			if err != nil {
				failures = append(failures, fmt.Errorf("%s: %w", id, err))
				return
			}
			if previous, taken := addresses[n.Interface.Address]; taken {
				failures = append(failures, fmt.Errorf("%s and %s both got %s", previous, id, n.Interface.Address))
				return
			}
			addresses[n.Interface.Address] = id
		}()
	}
	wg.Wait()

	for _, err := range failures {
		t.Error(err)
	}
	if len(addresses) != containers {
		t.Errorf("allocated %d distinct addresses, want %d", len(addresses), containers)
	}
}

func TestConcurrentDestroyIsSafe(t *testing.T) {
	t.Parallel()

	m := newManager(t, "10.99.0.0/16")

	if _, err := m.Allocate("abc123def456"); err != nil {
		t.Fatalf("Allocate() = %v", err)
	}

	var wg sync.WaitGroup
	errs := make([]error, 8)
	for i := range errs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs[i] = m.Destroy("abc123def456")
		}()
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Errorf("concurrent Destroy() %d = %v", i, err)
		}
	}
}

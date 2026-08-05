//go:build integration

package integration

import (
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stevenstank/forge/internal/logging"
	"github.com/stevenstank/forge/internal/network"
	"github.com/stevenstank/forge/internal/runtime"
	"github.com/stevenstank/forge/test/integration/testutil"
)

// Stage 4 is the stage where the assertions are least able to lie. A namespace
// either is the host's or it is not; an interface either is enslaved to the
// bridge or it is not; a packet either comes back or it does not. So these tests
// run against the host's real networking — the real forge0, the real
// 10.99.0.0/16, the real lease directory — exactly as `forge run` does, for the
// same reason the Stage 3 tests read the real /sys/fs/cgroup.
//
// # What the tests do not do
//
// They never remove the bridge. Forge deliberately leaves it (ADR-0019) because
// two concurrent runs would race to delete something the other is about to
// attach to, and a test suite that removed it would be that second run. An idle
// bridge is a few hundred bytes of kernel memory; a flaky suite is worse. Every
// *per-container* resource — the veth pair, the lease — is asserted to be gone,
// which is what FR-4.5 actually promises.
//
// # Parallelism
//
// Containers share the bridge and the address pool, which is what production
// does, so most of these run in parallel. The two that read conntrack do not:
// Ensure applies Forge's NAT rules as a flush-and-refill transaction, and a
// test asserting on a live translation should not have another test rewriting
// the rule set underneath it. Go runs the serial tests to completion before the
// parallel ones resume, so leaving them serial is enough to separate them.

// The helper modes Stage 4 adds. Each is this test binary re-executed inside the
// container; see harness_test.go.
const (
	stage4ModeReport = "stage4-report"
	stage4ModeDial   = "stage4-dial"
)

// The keys the report helper prints, one "key=value" per line. They are named
// constants because both halves of every test — the container writing them and
// the test reading them — have to agree.
const (
	reportID     = "id"
	reportNetns  = "netns"
	reportAddr   = "addr"
	reportGw     = "gw"
	reportMTU    = "mtu"
	reportIfaces = "ifaces"
	reportDial   = "dial"
	reportLocal  = "local"
)

// stage4Helper runs the container side of the Stage 4 tests.
func stage4Helper(mode string) (int, bool) {
	switch mode {
	case stage4ModeReport:
		printNetworkReport()
		announceReady()
		waitForEOF()
		return 0, true

	case stage4ModeDial:
		// The connection is held open rather than closed, so the test can read
		// the kernel's conntrack table while the flow is still established.
		conn, err := net.DialTimeout("tcp", os.Getenv(helperDataEnv), 10*time.Second)
		if err != nil {
			printKV(reportDial, "failed: "+err.Error())
			announceReady()
			waitForEOF()
			return 0, true
		}
		defer conn.Close()

		printKV(reportDial, "ok")
		printKV(reportLocal, conn.LocalAddr().String())
		printNetworkReport()
		announceReady()
		waitForEOF()
		return 0, true

	default:
		return 0, false
	}
}

// printNetworkReport describes the container's own network namespace, from
// inside it.
//
// Everything here is read through the kernel rather than assumed: the namespace
// identity from /proc/self/ns/net, the addresses from the interface list, the
// gateway from the routing table. A test that compared Forge's intentions
// against Forge's own record of them would prove nothing.
func printNetworkReport() {
	name, err := os.Hostname()
	if err != nil {
		fmt.Fprintln(os.Stderr, "helper: hostname:", err)
	}
	// Forge defaults a container's hostname to its ID, which is how the
	// container tells the test which container it is — and therefore which veth
	// pair and which lease belong to it.
	printKV(reportID, name)

	link, err := os.Readlink("/proc/self/ns/net")
	if err != nil {
		fmt.Fprintln(os.Stderr, "helper: reading the network namespace:", err)
	}
	printKV(reportNetns, link)

	ifaces, err := net.Interfaces()
	if err != nil {
		fmt.Fprintln(os.Stderr, "helper: listing interfaces:", err)
		return
	}

	names := make([]string, 0, len(ifaces))
	for _, iface := range ifaces {
		names = append(names, iface.Name)

		if iface.Name != network.ContainerIfaceName {
			continue
		}
		printKV(reportMTU, strconv.Itoa(iface.MTU))

		addrs, err := iface.Addrs()
		if err != nil {
			fmt.Fprintln(os.Stderr, "helper: reading addresses:", err)
			continue
		}
		for _, addr := range addrs {
			if ip, ok := addr.(*net.IPNet); ok && ip.IP.To4() != nil {
				printKV(reportAddr, ip.String())
			}
		}
	}
	printKV(reportIfaces, strings.Join(names, ","))

	printKV(reportGw, defaultGateway())
}

// defaultGateway returns the container's default route, read from the routing
// table of its own network namespace.
//
// /proc/net is a symlink to /proc/self/net, which the kernel resolves per
// network namespace, so this reports the container's routes even though the
// container never mounted a procfs of its own.
func defaultGateway() string {
	content, err := os.ReadFile("/proc/net/route")
	if err != nil {
		return ""
	}

	for _, line := range strings.Split(string(content), "\n")[1:] {
		fields := strings.Fields(line)
		if len(fields) < 3 || fields[1] != "00000000" {
			continue
		}

		// The gateway is a hex-encoded 32-bit address in the host's byte
		// order, which on every machine Forge runs on is little-endian.
		raw, err := hex.DecodeString(fields[2])
		if err != nil || len(raw) != 4 {
			continue
		}
		gw := make(net.IP, 4)
		binary.BigEndian.PutUint32(gw, binary.LittleEndian.Uint32(raw))

		return gw.String()
	}

	return ""
}

// printKV writes one line of the report.
func printKV(key, value string) { fmt.Printf("%s=%s\n", key, value) }

// --- the host side --------------------------------------------------------

// forgeNetworkState mirrors internal/network's own defaults. They are repeated
// rather than imported for the same reason testutil repeats the cgroup paths: a
// failure should mean "the kernel does not have what we expected", not "the
// package agrees with itself".
const (
	forgeBridge   = "forge0"
	forgeSubnet   = "10.99.0.0/16"
	forgeGateway  = "10.99.0.1"
	forgeLeaseDir = "/var/lib/forge/network/leases"
)

// internetTargetEnv overrides where the reachability tests dial. It is a
// literal address rather than a name: a container has no resolver configured,
// and a test that depended on DNS would be testing something else.
const (
	internetTargetEnv     = "FORGE_TEST_INTERNET"
	defaultInternetTarget = "1.1.1.1:53"
)

// requireForgeNetworking skips a test the host cannot support, and returns a
// Manager configured exactly as `forge run` configures one.
//
// The bridge is prepared here rather than left to the first container so that a
// host without nf_tables, or without CAP_NET_ADMIN, is reported as a skip with a
// reason instead of as a failure in whichever test happened to run first.
func requireForgeNetworking(t *testing.T) *network.Manager {
	t.Helper()

	requireRoot(t)

	manager, err := network.New(logging.New(io.Discard, slog.LevelError), network.Config{})
	if err != nil {
		t.Fatalf("network.New() = %v", err)
	}
	if err := manager.Ensure(); err != nil {
		t.Skipf("this host cannot provide forge networking: %v", err)
	}

	return manager
}

// report is the key/value description a container printed about itself.
type report map[string]string

// value returns a reported field, failing the test when the container never
// printed it — an absent field means the container could not see something it
// was supposed to have.
func (r report) value(t *testing.T, key string) string {
	t.Helper()

	value, ok := r[key]
	if !ok || value == "" {
		t.Fatalf("the container reported no %q; it said:\n%s", key, r)
	}

	return value
}

// String renders the whole report, for a failure message.
func (r report) String() string {
	keys := make([]string, 0, len(r))
	for key := range r {
		keys = append(keys, key)
	}
	slices.Sort(keys)

	var b strings.Builder
	for _, key := range keys {
		fmt.Fprintf(&b, "  %s=%s\n", key, r[key])
	}

	return b.String()
}

// parseReport reads the key/value lines a helper printed.
func parseReport(output string) report {
	parsed := report{}
	for _, line := range strings.Split(output, "\n") {
		key, value, found := strings.Cut(strings.TrimSpace(line), "=")
		if !found || key == readyMarker {
			continue
		}
		parsed[key] = value
	}

	return parsed
}

// stage4Spec returns a Spec for a bridged container running the test binary in
// the given helper mode.
func stage4Spec(t *testing.T, mode string, env ...string) runtime.Spec {
	t.Helper()

	spec := helperSpec(t, mode, env...)
	spec.Network = network.ModeBridge

	return spec
}

// startNetworked starts a container, waits for it to describe itself, and
// returns both it and what it said.
//
// The teardown of the container's networking is registered the moment the test
// learns which container it is, and it runs even when the test fails or panics
// half-way through. Forge releases these itself on every ordinary path; this is
// what keeps the promise when the test is the thing that crashed (PRD NFR-5).
func startNetworked(t *testing.T, manager *network.Manager, spec runtime.Spec) (*testutil.Live, report) {
	t.Helper()

	live := testutil.StartLive(t.Context(), t, spec)
	live.WaitForOutput(t, readyMarker, testutil.DefaultTimeout)

	got := parseReport(live.Stdout.String())
	id := got.value(t, reportID)

	t.Cleanup(func() {
		// Idempotent by contract (SSOT §13.3), so this costs nothing when the
		// container exited cleanly and released its own network.
		if err := manager.Destroy(id); err != nil {
			t.Errorf("releasing the network of container %s: %v", id, err)
		}
	})

	return live, got
}

// vethNames returns the two interface names Forge derives for a container.
// They are recomputed here from the ID rather than taken from Forge, so the
// test is asserting against the kernel and not against Forge's own bookkeeping.
func vethNames(t *testing.T, id string) (host, peer string) {
	t.Helper()

	host, peer, err := network.VethNames(id)
	if err != nil {
		t.Fatalf("VethNames(%q) = %v", id, err)
	}

	return host, peer
}

// linkPresent reports whether an interface exists in the caller's namespace.
func linkPresent(name string) bool {
	_, err := net.InterfaceByName(name)
	return err == nil
}

// linkMTU returns the MTU the kernel reports for an interface in the caller's
// namespace.
func linkMTU(t *testing.T, name string) int {
	t.Helper()

	iface, err := net.InterfaceByName(name)
	if err != nil {
		t.Fatalf("looking up %s: %v", name, err)
	}

	return iface.MTU
}

// bridgePorts returns the interfaces currently enslaved to Forge's bridge, as
// the kernel reports them under sysfs.
func bridgePorts(t *testing.T) []string {
	t.Helper()

	entries, err := os.ReadDir(filepath.Join("/sys/class/net", forgeBridge, "brif"))
	if err != nil {
		t.Fatalf("reading the ports of %s: %v", forgeBridge, err)
	}

	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		names = append(names, entry.Name())
	}

	return names
}

// leaseHolding returns the address a container currently holds, if any.
//
// Every assertion about leases goes through this rather than through os.Stat on
// a path under forgeLeaseDir, and the distinction is not pedantry: an address is
// a *pooled* resource. The moment one container releases 10.99.0.6, a container
// in a test running in parallel claims it — tryClaim walks the pool in ascending
// order, so the address just freed is very often the next one handed out. A test
// that asked "does this file exist?" would then be answered about somebody
// else's lease. Asking "does this container hold anything?" is a question only
// this test can be the subject of.
func leaseHolding(t *testing.T, manager *network.Manager, id string) (string, bool) {
	t.Helper()

	address, held, err := manager.Lookup(id)
	if err != nil {
		t.Fatalf("Lookup(%s) = %v", id, err)
	}

	return address, held
}

// addressOf strips the prefix length from a reported CIDR.
func addressOf(t *testing.T, cidr string) string {
	t.Helper()

	ip, _, err := net.ParseCIDR(cidr)
	if err != nil {
		t.Fatalf("the container reported the address %q, which does not parse: %v", cidr, err)
	}

	return ip.String()
}

// pollUntilGone waits for something the kernel releases asynchronously to
// actually be gone. Nothing here sleeps for a fixed duration and then assumes
// (SSOT §7).
func pollUntilGone(t *testing.T, what string, present func() bool) {
	t.Helper()

	testutil.PollUntil(t, what+" to be released", testutil.DefaultTimeout, func() bool {
		return !present()
	})
}

// --- FR-4.1: the network namespace ----------------------------------------

// TestContainerGetsItsOwnNetworkNamespace is the direct test of FR-4.1, in all
// three directions that matter: a bridged container is not in the host's
// namespace, two containers are not in each other's, and a container that asked
// for host networking is in the host's — which is what keeps every Stage 1 to 3
// example working unchanged.
func TestContainerGetsItsOwnNetworkNamespace(t *testing.T) {
	manager := requireForgeNetworking(t)
	t.Parallel()

	host := hostNamespace(t, "net")

	_, first := startNetworked(t, manager, stage4Spec(t, stage4ModeReport))
	_, second := startNetworked(t, manager, stage4Spec(t, stage4ModeReport))

	firstNS := first.value(t, reportNetns)
	secondNS := second.value(t, reportNetns)

	if firstNS == host {
		t.Errorf("container network namespace %q is the host's; want a distinct namespace", firstNS)
	}
	if !strings.HasPrefix(firstNS, "net:[") {
		t.Errorf("container network namespace = %q, want a net:[inode] identity", firstNS)
	}
	if firstNS == secondNS {
		t.Errorf("two containers share the network namespace %q; want one each", firstNS)
	}

	// The escape hatch, checked in the same test so the two cannot drift apart:
	// -network host must put the container in the host's namespace and create
	// nothing at all.
	hostMode := testutil.StartLive(t.Context(), t, helperSpec(t, stage4ModeReport))
	hostMode.WaitForOutput(t, readyMarker, testutil.DefaultTimeout)

	shared := parseReport(hostMode.Stdout.String())
	if got := shared.value(t, reportNetns); got != host {
		t.Errorf("a host-networked container is in namespace %q, want the host's %q", got, host)
	}

	// Nothing was created for it either: no veth pair is named after it, so
	// there is nothing for FR-4.5 to release later.
	hostVeth, _ := vethNames(t, shared.value(t, reportID))
	if linkPresent(hostVeth) {
		t.Errorf("forge created %s for a host-networked container; it must create nothing", hostVeth)
	}
}

// --- FR-4.3: addressing ---------------------------------------------------

// TestEachContainerGetsAUniqueAddress covers FR-4.3. Three containers run at
// once, because the property is about what the pool does under contention: an
// address handed out twice is the failure this exists to catch, and it can only
// happen while the first holder still has its lease.
func TestEachContainerGetsAUniqueAddress(t *testing.T) {
	manager := requireForgeNetworking(t)
	t.Parallel()

	_, subnet, err := net.ParseCIDR(forgeSubnet)
	if err != nil {
		t.Fatalf("parsing %s: %v", forgeSubnet, err)
	}

	const containers = 3
	seen := make(map[string]string, containers)

	for range containers {
		_, got := startNetworked(t, manager, stage4Spec(t, stage4ModeReport))

		id := got.value(t, reportID)
		address := addressOf(t, got.value(t, reportAddr))

		if !subnet.Contains(net.ParseIP(address)) {
			t.Errorf("container %s has %s, which is outside %s", id, address, forgeSubnet)
		}
		if address == forgeGateway {
			t.Errorf("container %s was given the gateway address %s", id, address)
		}
		if other, taken := seen[address]; taken {
			t.Errorf("containers %s and %s were both given %s", other, id, address)
		}
		seen[address] = id

		// The address the kernel gave the interface must be the one Forge
		// leased for it, or the lease is not tracking anything real.
		if leased, held := leaseHolding(t, manager, id); !held || leased != address {
			t.Errorf("container %s has %s on its interface but leases %q (held=%v)", id, address, leased, held)
		}

		// FR-4.3 is an address *and* a route: an address with no default route
		// is a container that can talk to its own subnet and nowhere else.
		if gw := got.value(t, reportGw); gw != forgeGateway {
			t.Errorf("container %s has default gateway %q, want %q", id, gw, forgeGateway)
		}
	}
}

// --- FR-4.2: the veth pair and the bridge ---------------------------------

// TestHostVethIsEnslavedToTheBridge covers the host half of FR-4.2. The peer
// half is asserted by its absence: an end that is still on the host is an end
// that was never moved into the container.
func TestHostVethIsEnslavedToTheBridge(t *testing.T) {
	manager := requireForgeNetworking(t)
	t.Parallel()

	_, got := startNetworked(t, manager, stage4Spec(t, stage4ModeReport))

	id := got.value(t, reportID)
	host, peer := vethNames(t, id)

	if !linkPresent(host) {
		t.Fatalf("the host end %s of container %s does not exist", host, id)
	}
	if linkPresent(peer) {
		t.Errorf("the peer %s is still on the host; it should have been moved into the container", peer)
	}

	// The bridge's own view, from sysfs rather than from Forge.
	if ports := bridgePorts(t); !slices.Contains(ports, host) {
		t.Errorf("%s is not a port of %s (ports: %v)", host, forgeBridge, ports)
	}

	// And the container's view: it has the peer, renamed to eth0, and loopback.
	ifaces := strings.Split(got.value(t, reportIfaces), ",")
	if !slices.Contains(ifaces, network.ContainerIfaceName) {
		t.Errorf("the container has interfaces %v, want one called %s", ifaces, network.ContainerIfaceName)
	}
	if slices.Contains(ifaces, peer) {
		t.Errorf("the container's interface is still called %s, want it renamed to %s",
			peer, network.ContainerIfaceName)
	}
}

// TestMTUIsAppliedToBothEndsOfTheVethPair covers the half of FR-4.3 that only
// shows up under load.
//
// A veth is two interfaces, each with its own MTU, and the container can only
// configure the one it holds. If the host end keeps the kernel's default while
// the peer is lowered, the pair is asymmetric: veth_xmit drops a frame bigger
// than the receiving end's MTU, and because the sending end still believes it
// can carry 1500 bytes, nothing upstream is ever told to send less. The result
// is a container where a ping succeeds and a download stalls — which is why
// this asserts on both ends rather than on the one the container can see.
func TestMTUIsAppliedToBothEndsOfTheVethPair(t *testing.T) {
	manager := requireForgeNetworking(t)
	t.Parallel()

	// Below the 1500 every veth starts at, so a passing assertion cannot come
	// from the default having been left in place.
	const mtu = 1400

	spec := stage4Spec(t, stage4ModeReport)
	spec.NetworkMTU = mtu

	_, got := startNetworked(t, manager, spec)

	id := got.value(t, reportID)
	host, _ := vethNames(t, id)

	// The container's end, as the container itself sees it.
	if reported := got.value(t, reportMTU); reported != strconv.Itoa(mtu) {
		t.Errorf("the container's %s has MTU %s, want %d", network.ContainerIfaceName, reported, mtu)
	}

	// The host's end, which nothing inside the container can reach and which
	// therefore nothing inside the container can prove.
	if got := linkMTU(t, host); got != mtu {
		t.Errorf("the host end %s has MTU %d, want %d: an asymmetric veth pair silently drops "+
			"every frame larger than the smaller end", host, got, mtu)
	}
}

// --- FR-4.4: connectivity -------------------------------------------------

// TestContainerReachesTheGateway is the first end-to-end proof that the cable
// is plugged in at both ends: a real TCP connection from inside the container's
// namespace to a listener on the bridge, carried by the veth pair.
//
// It also pins the other half of the masquerade rule. Traffic to the gateway
// leaves through the bridge, which the rule excludes by oifname, so the server
// must see the container's own address. If that ever became the host's, every
// container on the bridge would look identical to every other.
func TestContainerReachesTheGateway(t *testing.T) {
	manager := requireForgeNetworking(t)
	t.Parallel()

	listener, err := net.Listen("tcp", net.JoinHostPort(forgeGateway, "0"))
	if err != nil {
		t.Fatalf("listening on the bridge gateway %s: %v", forgeGateway, err)
	}
	defer listener.Close()

	peers := make(chan string, 1)
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		defer conn.Close()

		host, _, _ := net.SplitHostPort(conn.RemoteAddr().String())
		peers <- host
	}()

	_, got := startNetworked(t, manager,
		stage4Spec(t, stage4ModeDial, helperDataEnv+"="+listener.Addr().String()))

	if dial := got.value(t, reportDial); dial != "ok" {
		t.Fatalf("the container could not reach the gateway at %s: %s", listener.Addr(), dial)
	}

	address := addressOf(t, got.value(t, reportAddr))

	select {
	case peer := <-peers:
		if peer != address {
			t.Errorf("the listener saw a connection from %s, want the container's own address %s "+
				"(traffic across the bridge must not be masqueraded)", peer, address)
		}
	case <-time.After(testutil.DefaultTimeout):
		t.Fatal("the container reported a successful connection the listener never accepted")
	}
}

// TestContainerReachesTheInternet covers FR-4.4 the way a user experiences it.
//
// It is skipped rather than failed when the host itself cannot reach the
// target: a machine with no outbound connectivity says nothing about Forge. A
// host that *can* reach it while its containers cannot is the real failure, and
// on a machine also running Docker the usual cause is not Forge at all — see
// the note in internal/network/nat.go about the FORWARD policy.
func TestContainerReachesTheInternet(t *testing.T) {
	manager := requireForgeNetworking(t)
	target := requireInternet(t)

	_, got := startNetworked(t, manager, stage4Spec(t, stage4ModeDial, helperDataEnv+"="+target))

	if dial := got.value(t, reportDial); dial != "ok" {
		t.Fatalf("the container could not reach %s: %s\n"+
			"the host can, so the container's route out is broken. On a host also running Docker, "+
			"check whether the kernel's FORWARD chain has a DROP policy: forge's rule lives in its "+
			"own table and cannot override a drop in another.", target, dial)
	}
}

// TestNATRewritesTheContainerSource covers FR-4.4 at the level the requirement
// is actually about: the packet leaving the host no longer carries the
// container's private address.
//
// Reachability alone nearly proves this — nothing would route a reply back to
// 10.99.0.0/16 — but "nearly" is not an assertion, so this reads the kernel's
// own record of the translation. conntrack stores a flow as two tuples, the
// original and the reply; the reply's destination is the address the peer will
// answer, which is the masqueraded one.
//
// Deliberately not parallel: Ensure applies Forge's NAT rules as a
// flush-and-refill transaction, and another test doing that mid-flow is a race
// this test would report as a bug.
func TestNATRewritesTheContainerSource(t *testing.T) {
	manager := requireForgeNetworking(t)
	target := requireInternet(t)

	const conntrackPath = "/proc/net/nf_conntrack"
	if _, err := os.Stat(conntrackPath); err != nil {
		t.Skipf("%s is unavailable, so the translation cannot be observed: %v", conntrackPath, err)
	}

	targetIP, _, err := net.SplitHostPort(target)
	if err != nil {
		t.Fatalf("splitting %q: %v", target, err)
	}

	// The helper holds the connection open, so the flow is still in the table
	// when this reads it.
	_, got := startNetworked(t, manager, stage4Spec(t, stage4ModeDial, helperDataEnv+"="+target))

	if dial := got.value(t, reportDial); dial != "ok" {
		t.Fatalf("the container could not reach %s: %s", target, dial)
	}

	address := addressOf(t, got.value(t, reportAddr))

	var flow string
	testutil.PollUntil(t, "the container's flow to appear in conntrack", testutil.DefaultTimeout, func() bool {
		flow = findConntrackFlow(conntrackPath, address, targetIP)
		return flow != ""
	})

	// "src=<container> dst=<target> ... src=<target> dst=<translated>": the
	// second dst is what the peer will reply to, and it is the host's address
	// if the masquerade happened.
	translated := lastField(flow, "dst=")
	if translated == "" {
		t.Fatalf("could not read the reply tuple from the conntrack entry:\n%s", flow)
	}
	if translated == address {
		t.Errorf("conntrack shows the flow still sourced from the container's private address %s; "+
			"it was not masqueraded:\n%s", address, flow)
	}
	if net.ParseIP(translated) == nil {
		t.Errorf("conntrack reports the translated address as %q, which does not parse:\n%s", translated, flow)
	}
}

// requireInternet returns the address the reachability tests dial, skipping
// when the host cannot reach it itself.
func requireInternet(t *testing.T) string {
	t.Helper()

	target := os.Getenv(internetTargetEnv)
	if target == "" {
		target = defaultInternetTarget
	}

	conn, err := net.DialTimeout("tcp", target, 5*time.Second)
	if err != nil {
		t.Skipf("this host cannot reach %s itself, so a container failing to is not forge's doing: %v"+
			" (set %s to a reachable host:port to run this test)", target, err, internetTargetEnv)
	}
	if err := conn.Close(); err != nil {
		t.Logf("closing the reachability probe: %v", err)
	}

	return target
}

// findConntrackFlow returns the conntrack line describing a flow from source to
// destination, or "" if the kernel has no such flow.
func findConntrackFlow(path, source, destination string) string {
	content, err := os.ReadFile(path)
	if err != nil {
		return ""
	}

	for _, line := range strings.Split(string(content), "\n") {
		if strings.Contains(line, "src="+source+" ") && strings.Contains(line, "dst="+destination+" ") {
			return line
		}
	}

	return ""
}

// lastField returns the value of the final occurrence of a "key=value" field in
// a conntrack line. A flow has two tuples and each names both ends, so the last
// dst= is the reply's destination: the address the translation produced.
func lastField(line, key string) string {
	fields := strings.Fields(line)
	value := ""
	for _, field := range fields {
		if after, found := strings.CutPrefix(field, key); found {
			value = after
		}
	}

	return value
}

// --- FR-4.5: teardown -----------------------------------------------------

// TestNetworkingIsReleasedWhenTheContainerExits covers FR-4.5 end to end. Both
// halves are asserted in one test because they are one promise: an address
// returned to the pool while its interface is still plugged into the bridge
// would be handed to a container that could not use it.
func TestNetworkingIsReleasedWhenTheContainerExits(t *testing.T) {
	manager := requireForgeNetworking(t)
	t.Parallel()

	live, got := startNetworked(t, manager, stage4Spec(t, stage4ModeReport))

	id := got.value(t, reportID)
	address := addressOf(t, got.value(t, reportAddr))
	host, peer := vethNames(t, id)

	// Everything is there while the container is.
	if !linkPresent(host) {
		t.Fatalf("the host end %s does not exist while container %s is running", host, id)
	}
	if leased, held := leaseHolding(t, manager, id); !held || leased != address {
		t.Fatalf("container %s is running on %s but leases %q (held=%v)", id, address, leased, held)
	}

	live.CloseStdin(t)
	if status := live.Wait(t, testutil.DefaultTimeout); status.Code != 0 {
		t.Fatalf("container exited %v, want 0", status)
	}

	// FR-4.5, the interface half. Deleting the host end deletes both, and the
	// kernel usually gets there first when the namespace dies — either way,
	// neither end may survive.
	pollUntilGone(t, "the host veth "+host, func() bool { return linkPresent(host) })
	if linkPresent(peer) {
		t.Errorf("the peer %s outlived container %s", peer, id)
	}
	if ports := bridgePorts(t); slices.Contains(ports, host) {
		t.Errorf("%s is still a port of %s after container %s exited (ports: %v)",
			host, forgeBridge, id, ports)
	}

	// FR-4.5, the address half. The lease is the only record that an address is
	// spoken for, so a lease that outlives its container leaks it permanently.
	//
	// The question is whether *this container* still holds an address, not
	// whether a file exists at its old one: by now another container may quite
	// correctly have been given 10.99.0.x, which is the pool working rather
	// than a leak.
	pollUntilGone(t, "the lease held by container "+id, func() bool {
		_, held := leaseHolding(t, manager, id)
		return held
	})

	// And the pool must genuinely have it back, not merely have forgotten it.
	reused, err := manager.Allocate("reuse" + id[:7])
	if err != nil {
		t.Fatalf("allocating after the container exited: %v", err)
	}
	t.Cleanup(func() {
		if err := manager.Destroy(reused.ContainerID); err != nil {
			t.Errorf("releasing the probe allocation: %v", err)
		}
	})
}

// TestDestroyReleasesAContainerItDidNotCreate covers the property the whole
// cleanup design rests on (SSOT §11.2, §13.4): a Manager holds no per-container
// state, so it can tear down an allocation it did not make. That is what lets
// the runtime's cleanup stack call Destroy for a container in any state, and
// what `forge rm` will need in Stage 6.
//
// It destroys a container's networking while the container is still running,
// which is the harshest version of the case: nothing has released anything, so
// Destroy is doing all of the work rather than confirming the kernel already
// did it.
func TestDestroyReleasesAContainerItDidNotCreate(t *testing.T) {
	manager := requireForgeNetworking(t)
	t.Parallel()

	live, got := startNetworked(t, manager, stage4Spec(t, stage4ModeReport))

	id := got.value(t, reportID)
	address := addressOf(t, got.value(t, reportAddr))
	host, _ := vethNames(t, id)

	// Held before, so the assertion after Destroy is about something that was
	// actually there to release.
	if leased, held := leaseHolding(t, manager, id); !held || leased != address {
		t.Fatalf("container %s is running on %s but leases %q (held=%v)", id, address, leased, held)
	}

	// A second Manager, constructed from configuration alone and with no
	// knowledge of this container beyond its ID.
	other, err := network.New(logging.New(io.Discard, slog.LevelError), network.Config{})
	if err != nil {
		t.Fatalf("network.New() = %v", err)
	}

	if err := other.Destroy(id); err != nil {
		t.Fatalf("Destroy(%s) = %v", id, err)
	}

	if linkPresent(host) {
		t.Errorf("the host end %s survived Destroy", host)
	}
	if ports := bridgePorts(t); slices.Contains(ports, host) {
		t.Errorf("%s is still a port of %s after Destroy (ports: %v)", host, forgeBridge, ports)
	}
	if leased, held := leaseHolding(t, other, id); held {
		t.Errorf("container %s still holds %s after Destroy", id, leased)
	}

	// Idempotence is what makes Destroy safe to put on a cleanup stack: this is
	// the second call, against a container that now has nothing left.
	if err := other.Destroy(id); err != nil {
		t.Errorf("a second Destroy(%s) = %v, want nil: it must be idempotent", id, err)
	}

	// The container itself is unharmed — it has simply lost its network — and
	// must still exit cleanly, so a failed teardown cannot strand a process.
	live.CloseStdin(t)
	if status := live.Wait(t, testutil.DefaultTimeout); status.Code != 0 {
		t.Errorf("container exited %v after its network was destroyed, want 0", status)
	}
}

// TestALeaseHeldByALiveProcessIsNotReclaimed guards the window that makes
// crash recovery safe rather than dangerous.
//
// A lease is checkable by something other than the process that wrote it — the
// veth name is derived from the container ID, so the kernel can be asked
// whether the holder still exists (ADR-0019). But between Allocate and Attach a
// container legitimately has a lease and no interface, and a reclaimer that
// looked only at interfaces would hand that address to somebody else. The
// claiming PID is what closes the window, and this is the test that it does.
//
// The reclamation itself only runs when the pool is exhausted, which a /16
// cannot be made to be in a test; internal/network covers that path against a
// deliberately tiny subnet.
func TestALeaseHeldByALiveProcessIsNotReclaimed(t *testing.T) {
	manager := requireForgeNetworking(t)
	t.Parallel()

	// A lease for a container that never existed and never will: no veth, and
	// no live process holding it.
	const ghost = "dead0forge01"

	allocated, err := manager.Allocate(ghost)
	if err != nil {
		t.Fatalf("Allocate(%s) = %v", ghost, err)
	}
	t.Cleanup(func() {
		if err := manager.Destroy(ghost); err != nil {
			t.Errorf("releasing the ghost lease: %v", err)
		}
	})

	address := addressOf(t, allocated.Interface.Address)

	// The lease names this process, which is alive, so it must *not* be
	// reclaimed: a container part-way through starting has no veth yet, and
	// treating that as dead would hand its address to somebody else.
	leased, held := leaseHolding(t, manager, ghost)
	if !held || leased != address {
		t.Errorf("Lookup(%s) = %q, %v; want %q while the claiming process is alive",
			ghost, leased, held, address)
	}
}

// TestNoLeaseIsOrphaned asserts the pool's invariant, which holds at every
// moment rather than only at the end of the suite: a lease is backed either by
// an interface that exists or by a process that is still alive to finish
// creating one. Anything else is an address nothing can ever hand back.
//
// This is deliberately not a "run me last" sweep. Go gives no ordering between
// a serial test and the parallel ones that resume after it, so a final-sweep
// test would be checking a state the rest of the suite had not reached yet.
// Phrasing it as an invariant makes it correct whenever it happens to run.
func TestNoLeaseIsOrphaned(t *testing.T) {
	requireForgeNetworking(t)
	t.Parallel()

	entries, err := os.ReadDir(forgeLeaseDir)
	if err != nil {
		if os.IsNotExist(err) {
			return
		}
		t.Fatalf("reading %s: %v", forgeLeaseDir, err)
	}

	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}

		content, err := os.ReadFile(filepath.Join(forgeLeaseDir, entry.Name()))
		if err != nil {
			continue
		}

		// A lease is "<container id>\n<pid>\n". One naming a container whose
		// veth is gone and whose claiming process is dead is residue.
		lines := strings.SplitN(strings.TrimSpace(string(content)), "\n", 2)
		id := lines[0]
		if id == "" {
			continue
		}

		host, _, err := network.VethNames(id)
		if err != nil || linkPresent(host) {
			continue
		}
		if len(lines) > 1 {
			if pid, err := strconv.Atoi(strings.TrimSpace(lines[1])); err == nil && processAlive(pid) {
				continue
			}
		}

		t.Errorf("%s is a stale lease: container %s has no interface and no live claimant",
			filepath.Join(forgeLeaseDir, entry.Name()), id)
	}
}

// processAlive reports whether a PID names a running process.
func processAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	_, err := os.Stat(filepath.Join("/proc", strconv.Itoa(pid)))

	return err == nil
}

package network

import (
	"errors"
	"io"
	"log/slog"
	"net"
	"os"
	"strings"
	"testing"

	"golang.org/x/sys/unix"

	"github.com/stevenstank/forge/internal/logging"
)

// The netlink transport, against the real kernel.
//
// Opening a netlink socket, asking it about an interface, and being refused a
// change to one are all unprivileged operations, so the encode/send/read/decode
// path can be exercised for real here rather than against a fixture. What needs
// root is the far side of it — a request the kernel actually carries out — and
// that belongs to the privileged suite. The tests that would create something
// therefore run only as an unprivileged user, where the kernel's refusal is the
// observable result and is itself worth pinning: it is how `forge run` without
// sudo reports what is wrong.

// testConn opens a route netlink socket, or skips the test if the environment
// has none.
func testConn(t *testing.T) *nlConn {
	t.Helper()

	conn, err := dialNetlink(unix.NETLINK_ROUTE)
	if err != nil {
		t.Skipf("no NETLINK_ROUTE socket available here: %v", err)
	}
	t.Cleanup(func() {
		if err := conn.close(); err != nil {
			t.Errorf("close() = %v", err)
		}
	})

	return conn
}

// requireUnprivileged skips a test that would perform a real kernel change if
// it were run with the privileges to succeed.
func requireUnprivileged(t *testing.T) {
	t.Helper()

	if os.Geteuid() == 0 {
		t.Skip("running as root: this case asserts the kernel's refusal, and as root it would not refuse")
	}
}

// TestDialNetlinkOpensAndClosesASocket covers the lifecycle of the descriptor
// every operation in this package borrows.
func TestDialNetlinkOpensAndClosesASocket(t *testing.T) {
	t.Parallel()

	conn, err := dialNetlink(unix.NETLINK_ROUTE)
	if err != nil {
		t.Skipf("no NETLINK_ROUTE socket available here: %v", err)
	}
	if conn.fd < 0 {
		t.Errorf("fd = %d, want a valid descriptor", conn.fd)
	}
	if conn.seq != 0 {
		t.Errorf("seq = %d, want a fresh connection to start at 0", conn.seq)
	}
	if err := conn.close(); err != nil {
		t.Errorf("close() = %v", err)
	}
}

// TestExecuteAdvancesTheSequenceNumber pins the property readAck depends on:
// every request on a connection carries a sequence number of its own, so a
// reply can be matched to the request that caused it.
func TestExecuteAdvancesTheSequenceNumber(t *testing.T) {
	t.Parallel()

	conn := testConn(t)

	// The request is expected to fail without privileges; what is asserted is
	// that the counter moved regardless, because a failed request has still
	// consumed its sequence number as far as the kernel is concerned.
	before := conn.seq
	_ = conn.execute(unix.RTM_NEWLINK, unix.NLM_F_CREATE|unix.NLM_F_EXCL,
		concat(ifInfoMsg(0, 0, 0), nlAttrString(unix.IFLA_IFNAME, "forge-seq-test")))

	if conn.seq != before+1 {
		t.Errorf("seq = %d after one request, want %d", conn.seq, before+1)
	}
}

// TestLinkIndexFindsLoopback checks the lookup every operation starts from
// against an interface that exists in every network namespace.
func TestLinkIndexFindsLoopback(t *testing.T) {
	t.Parallel()

	index, err := linkIndex(loopbackName)
	if err != nil {
		t.Fatalf("linkIndex(%q) = %v", loopbackName, err)
	}
	if index <= 0 {
		t.Errorf("linkIndex(%q) = %d, want a positive index", loopbackName, index)
	}

	iface, err := net.InterfaceByName(loopbackName)
	if err != nil {
		t.Fatal(err)
	}
	if int(index) != iface.Index {
		t.Errorf("linkIndex(%q) = %d, want %d", loopbackName, index, iface.Index)
	}
}

// TestLinkIndexReportsAMissingInterface covers the sentinel Configure relies on
// to say "the container's interface is not in its namespace" rather than
// leaking a netlink error.
func TestLinkIndexReportsAMissingInterface(t *testing.T) {
	t.Parallel()

	const absent = "forge-no-such-if"

	_, err := linkIndex(absent)
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("linkIndex(%q) = %v, want ErrNotFound", absent, err)
	}
	if !strings.Contains(err.Error(), absent) {
		t.Errorf("linkIndex(%q) = %q, want it to name the interface", absent, err.Error())
	}
}

// TestLinkExists is the liveness check the lease reclaimer uses, and it must
// never report true for an interface that is gone — an address would be handed
// to a second container while the first still held it.
func TestLinkExists(t *testing.T) {
	t.Parallel()

	if !linkExists(loopbackName) {
		t.Errorf("linkExists(%q) = false, want true", loopbackName)
	}
	if linkExists("forge-no-such-if") {
		t.Error("linkExists() = true for an interface that does not exist")
	}
	if linkExists("") {
		t.Error("linkExists(\"\") = true, want false")
	}
}

// TestDeleteVethOnAMissingInterfaceIsNotAnError is the idempotence teardown
// depends on: when the container's netns is destroyed the kernel takes the veth
// with it, so the common case for Destroy is that there is nothing left to
// delete.
func TestDeleteVethOnAMissingInterfaceIsNotAnError(t *testing.T) {
	t.Parallel()

	conn := testConn(t)

	if err := deleteVeth(conn, "forge-no-such-if"); err != nil {
		t.Errorf("deleteVeth() on a missing interface = %v, want nil", err)
	}
}

// TestKernelRefusalsBecomeErrPermission drives a real request through encode,
// send, readAck and the errno translation, and checks that what comes back is
// the sentinel a user can act on rather than a bare "operation not permitted".
func TestKernelRefusalsBecomeErrPermission(t *testing.T) {
	requireUnprivileged(t)
	t.Parallel()

	conn := testConn(t)

	tests := []struct {
		name string
		call func() error
	}{
		{
			name: "creating a veth pair",
			call: func() error { return createVethPair(conn, "forge-t-a", "forge-t-b") },
		},
		{
			name: "creating a bridge",
			call: func() error {
				return conn.execute(unix.RTM_NEWLINK, unix.NLM_F_CREATE|unix.NLM_F_EXCL,
					bridgeCreateMessage("forge-test-br"))
			},
		},
		{
			name: "bringing loopback up",
			call: func() error {
				index, err := linkIndex(loopbackName)
				if err != nil {
					return err
				}
				return setLinkUp(conn, index)
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.call()
			if err == nil {
				t.Fatalf("%s succeeded without privileges", tc.name)
			}
			if !errors.Is(err, ErrPermission) {
				t.Errorf("%s = %v, want ErrPermission", tc.name, err)
			}
		})
	}
}

// TestEnsureIsRefusedWithoutPrivileges checks the same translation one level
// up, where the message a user sees is produced.
func TestEnsureIsRefusedWithoutPrivileges(t *testing.T) {
	requireUnprivileged(t)
	t.Parallel()

	m, err := New(logging.New(io.Discard, slog.LevelError), Config{
		Bridge:   "forge-test-br0",
		Subnet:   "10.211.0.0/24",
		StateDir: t.TempDir(),
	})
	if err != nil {
		t.Fatalf("New() = %v", err)
	}

	err = m.Ensure()
	if err == nil {
		t.Fatal("Ensure() = nil without privileges")
	}
	if !errors.Is(err, ErrBridgeUnavailable) && !errors.Is(err, ErrPermission) {
		t.Errorf("Ensure() = %v, want ErrBridgeUnavailable or ErrPermission", err)
	}
}

// TestTranslate covers the errno mapping by itself, including the wrapped forms
// it meets in production.
func TestTranslate(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		err  error
		want bool
	}{
		{name: "EPERM", err: unix.EPERM, want: true},
		{name: "EACCES", err: unix.EACCES, want: true},
		{name: "os.ErrPermission", err: os.ErrPermission, want: true},
		{name: "a wrapped permission error", err: &os.PathError{Op: "open", Path: "/x", Err: unix.EACCES}, want: true},
		{name: "EEXIST", err: unix.EEXIST, want: false},
		{name: "something else", err: errors.New("nope"), want: false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got := translate(tc.err)
			if errors.Is(got, ErrPermission) != tc.want {
				t.Errorf("translate(%v) = %v, want ErrPermission == %t", tc.err, got, tc.want)
			}
			// The cause survives either way, so the message still says which
			// operation the kernel refused.
			if !errors.Is(got, tc.err) {
				t.Errorf("translate(%v) dropped its cause", tc.err)
			}
		})
	}

	if got := translate(nil); got != nil {
		t.Errorf("translate(nil) = %v, want nil", got)
	}
}

// TestLinkOperationsAreRefusedWithoutPrivileges walks every mutating operation
// this package builds a message for.
//
// Each one is encoded, sent to the kernel and answered, so the whole transport
// is exercised for real; what the kernel answers without CAP_NET_ADMIN is a
// refusal, and Forge must surface it as the sentinel rather than as a raw
// errno. As root these would carry out the change — renaming loopback, moving
// it into another namespace — so they run unprivileged only, and the successful
// forms belong to the privileged suite.
func TestLinkOperationsAreRefusedWithoutPrivileges(t *testing.T) {
	requireUnprivileged(t)
	t.Parallel()

	index, err := linkIndex(loopbackName)
	if err != nil {
		t.Fatalf("linkIndex(%q) = %v", loopbackName, err)
	}

	tests := []struct {
		name string
		call func(*nlConn) error
	}{
		{name: "setLinkMTU", call: func(c *nlConn) error { return setLinkMTU(c, index, 1400) }},
		{name: "setLinkMaster", call: func(c *nlConn) error { return setLinkMaster(c, index, index) }},
		{name: "renameLink", call: func(c *nlConn) error { return renameLink(c, index, "forge-renamed") }},
		{name: "deleteLink", call: func(c *nlConn) error { return deleteLink(c, index) }},
		{name: "addAddress", call: func(c *nlConn) error { return addAddress(c, index, "10.211.0.2/24") }},
		{name: "addDefaultRoute", call: func(c *nlConn) error { return addDefaultRoute(c, index, "10.211.0.1") }},
		{name: "moveLinkToNetns", call: func(c *nlConn) error { return moveLinkToNetns(c, index, os.Getpid()) }},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			conn := testConn(t)

			err := tc.call(conn)
			if err == nil {
				t.Fatalf("%s succeeded without privileges", tc.name)
			}
			if !errors.Is(err, ErrPermission) {
				t.Errorf("%s = %v, want ErrPermission", tc.name, err)
			}
		})
	}
}

// TestAddAddressRejectsAMalformedAddress checks the encoding refuses input the
// kernel would have to guess at, before a socket is opened.
func TestAddAddressRejectsAMalformedAddress(t *testing.T) {
	t.Parallel()

	conn := testConn(t)

	for _, addr := range []string{"", "10.211.0.2", "not-an-address/24", "10.211.0.2/33", "::1/64"} {
		if err := addAddress(conn, 1, addr); err == nil {
			t.Errorf("addAddress(%q) = nil, want a refusal", addr)
		}
	}
}

// TestAddDefaultRouteRejectsAMalformedGateway is the same guard for the route.
func TestAddDefaultRouteRejectsAMalformedGateway(t *testing.T) {
	t.Parallel()

	conn := testConn(t)

	for _, gw := range []string{"", "not-an-address", "10.211.0.1/24", "::1"} {
		if err := addDefaultRoute(conn, 1, gw); err == nil {
			t.Errorf("addDefaultRoute(%q) = nil, want a refusal", gw)
		}
	}
}

// TestConfigureValidatesBeforeOpeningASocket covers the container-side entry
// point's refusals, which happen before anything is asked of the kernel.
func TestConfigureValidatesBeforeOpeningASocket(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		iface Interface
	}{
		{name: "empty", iface: Interface{}},
		{name: "no source", iface: Interface{Name: "eth0", Address: "10.211.0.2/24"}},
		{name: "no address", iface: Interface{Source: "veth0", Name: "eth0"}},
		{name: "a malformed address", iface: Interface{Source: "veth0", Name: "eth0", Address: "10.211.0.2"}},
		{name: "a malformed gateway", iface: Interface{
			Source: "veth0", Name: "eth0", Address: "10.211.0.2/24", Gateway: "nope",
		}},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			if err := Configure(tc.iface); !errors.Is(err, ErrInvalidInterface) {
				t.Errorf("Configure(%+v) = %v, want ErrInvalidInterface", tc.iface, err)
			}
		})
	}
}

// TestConfigureIsRefusedWithoutPrivileges checks that the container-side setup
// reports the missing capability rather than a bare errno from a syscall the
// user never named. Loopback comes first in Configure's order, so that is where
// an unprivileged run stops.
func TestConfigureIsRefusedWithoutPrivileges(t *testing.T) {
	requireUnprivileged(t)
	t.Parallel()

	t.Run("Configure", func(t *testing.T) {
		t.Parallel()

		iface := Interface{
			Source:  "forge-absent0",
			Name:    ContainerIfaceName,
			Address: "10.211.0.2/24",
			Gateway: "10.211.0.1",
		}
		if err := Configure(iface); !errors.Is(err, ErrPermission) {
			t.Errorf("Configure() = %v, want ErrPermission", err)
		}
	})

	t.Run("ConfigureLoopback", func(t *testing.T) {
		t.Parallel()

		if err := ConfigureLoopback(); !errors.Is(err, ErrPermission) {
			t.Errorf("ConfigureLoopback() = %v, want ErrPermission", err)
		}
	})
}

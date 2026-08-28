package network

import (
	"errors"
	"fmt"
	"strings"

	"golang.org/x/sys/unix"
)

// The veth pair (FR-4.2).
//
// A veth is a cable: two interfaces, whatever enters one leaves the other. One
// end stays on the host and is plugged into Forge's bridge; the other is moved
// into the container's network namespace and becomes its eth0. Deleting either
// end deletes both, which is what makes teardown a single operation.

// Interface naming.
const (
	// ifNameMaxLen is IFNAMSIZ-1 from linux/if.h: the kernel's buffer is 16
	// bytes and one of them is the NUL terminator. A longer name is rejected
	// with EINVAL, so it is checked here where the error can say why.
	ifNameMaxLen = 15

	// hostVethPrefix and peerVethPrefix are prepended to the container ID.
	// Container IDs are 12 hex characters (SSOT §8), so both names come to 14
	// characters and fit with a byte to spare.
	hostVethPrefix = "fh"
	peerVethPrefix = "fc"

	// ContainerIfaceName is what the container's interface is called once it
	// has been renamed inside the container's namespace. Every image and every
	// piece of software inside a container expects eth0.
	ContainerIfaceName = "eth0"
)

// vethInfoPeer is VETH_INFO_PEER from linux/veth.h. It is spelled out rather
// than taken from x/sys/unix because the constant's presence there has varied
// between releases, and a build failure on someone else's toolchain is a poor
// trade for saving one line.
const vethInfoPeer uint16 = 1

// VethNames derives the two interface names for a container.
//
// They are derived rather than random on purpose. A name that can be recomputed
// from the container ID is a name that something other than the process which
// created it can check, which is what lets a stale IP lease be reclaimed by
// asking the kernel whether the interface is still there (ADR-0019).
func VethNames(containerID string) (host, peer string, err error) {
	if err := validateContainerID(containerID); err != nil {
		return "", "", err
	}

	host = hostVethPrefix + containerID
	peer = peerVethPrefix + containerID

	if err := validateIfaceName(host); err != nil {
		return "", "", fmt.Errorf("%w: container id %q is too long for an interface name: %w",
			ErrInvalidContainerID, containerID, err)
	}
	if err := validateIfaceName(peer); err != nil {
		return "", "", fmt.Errorf("%w: container id %q is too long for an interface name: %w",
			ErrInvalidContainerID, containerID, err)
	}

	return host, peer, nil
}

// validateIfaceName reports whether a name is one the kernel will accept.
//
// The rules are the kernel's own (dev_valid_name): non-empty, shorter than
// IFNAMSIZ, not "." or "..", and free of "/" and whitespace.
func validateIfaceName(name string) error {
	switch {
	case name == "":
		return fmt.Errorf("%w: empty interface name", ErrInvalidInterface)
	case len(name) > ifNameMaxLen:
		return fmt.Errorf("%w: %q is %d characters; the kernel allows %d",
			ErrInvalidInterface, name, len(name), ifNameMaxLen)
	case name == "." || name == "..":
		return fmt.Errorf("%w: %q is not a usable interface name", ErrInvalidInterface, name)
	case strings.ContainsAny(name, "/ \t\n"):
		return fmt.Errorf("%w: %q contains a separator or whitespace", ErrInvalidInterface, name)
	case strings.ContainsRune(name, 0):
		return fmt.Errorf("%w: interface name contains a NUL byte", ErrInvalidInterface)
	}

	return nil
}

// vethCreateMessage builds the RTM_NEWLINK body that creates a veth pair.
//
// It is separated from the send so the byte layout can be asserted in a unit
// test. This is the most intricate message Forge builds — three levels of
// nesting, and the peer's own ifinfomsg buried inside the innermost — and it is
// exactly the kind of thing that is easy to get subtly wrong and impossible to
// debug from EINVAL alone.
//
//	ifinfomsg
//	IFLA_IFNAME              = host
//	IFLA_LINKINFO
//	  IFLA_INFO_KIND         = "veth"
//	  IFLA_INFO_DATA
//	    VETH_INFO_PEER
//	      ifinfomsg           (zeroed; the peer inherits the defaults)
//	      IFLA_IFNAME        = peer
func vethCreateMessage(host, peer string) []byte {
	peerBlock := nlNestedPlain(vethInfoPeer,
		ifInfoMsg(0, 0, 0),
		nlAttrString(unix.IFLA_IFNAME, peer),
	)

	return concat(
		ifInfoMsg(0, 0, 0),
		nlAttrString(unix.IFLA_IFNAME, host),
		nlNestedPlain(unix.IFLA_LINKINFO,
			nlAttrString(unix.IFLA_INFO_KIND, "veth"),
			nlNestedPlain(unix.IFLA_INFO_DATA, peerBlock),
		),
	)
}

// createVethPair creates both ends of the cable in the caller's namespace.
//
// Both ends start here: a veth pair cannot be created with one end already in
// another namespace, so the peer is created on the host and moved afterwards.
// NLM_F_EXCL makes a name collision an error rather than a silent adoption of
// somebody else's interface.
func createVethPair(c *nlConn, host, peer string) error {
	if err := validateIfaceName(host); err != nil {
		return err
	}
	if err := validateIfaceName(peer); err != nil {
		return err
	}

	body := vethCreateMessage(host, peer)
	if err := c.execute(unix.RTM_NEWLINK, unix.NLM_F_CREATE|unix.NLM_F_EXCL, body); err != nil {
		return fmt.Errorf("creating the veth pair %s/%s: %w", host, peer, err)
	}

	return nil
}

// deleteVeth removes a veth pair by naming its host end.
//
// It is idempotent: an interface that is not there is the state this is trying
// to reach, not a failure. That matters more here than anywhere else in the
// package, because the common case is that the kernel got there first — when
// the container's netns is destroyed the peer goes with it, and a veth whose
// peer is gone is gone too.
func deleteVeth(c *nlConn, name string) error {
	index, err := linkIndex(name)
	if errors.Is(err, ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}

	if err := deleteLink(c, index); err != nil {
		// The race is real and benign: the netns can be torn down between the
		// lookup above and the delete below.
		if !linkExists(name) {
			return nil
		}
		return err
	}

	return nil
}

// Attach creates the container's veth pair, moves the peer into the container's
// network namespace, and plugs the host end into the bridge (FR-4.2).
//
// It is separate from Allocate because the two happen either side of clone(2):
// an address can be reserved before the container exists, but a namespace
// cannot be joined until there is a namespace to join. internal/runtime calls
// this while the container's init is still blocked on its startup handshake, so
// the interface is in place before any of the container's own code runs.
//
// Attach rolls nothing back. A failure part-way leaves what it made and says
// so; Destroy is idempotent and the orchestrator's cleanup stack calls it
// either way (SSOT §11.3).
func (m *Manager) Attach(n *Network, pid int) error {
	if n == nil {
		return fmt.Errorf("%w: no network to attach", ErrNotFound)
	}
	if pid <= 0 {
		return fmt.Errorf("%w: pid %d is not a process", ErrInvalidInterface, pid)
	}

	conn, err := dialNetlink(unix.NETLINK_ROUTE)
	if err != nil {
		return err
	}
	defer func() {
		if err := conn.close(); err != nil && m.logger != nil {
			m.logger.Warn("closing the netlink socket after attaching a container", "error", err)
		}
	}()

	if err := createVethPair(conn, n.HostVeth, n.PeerVeth); err != nil {
		return err
	}

	// The peer moves first. Until it is in the container's namespace it is
	// visible on the host, and an interface briefly carrying a container's
	// name on the host is the sort of thing that confuses other tooling.
	peerIndex, err := linkIndex(n.PeerVeth)
	if err != nil {
		return err
	}
	if err := moveLinkToNetns(conn, peerIndex, pid); err != nil {
		return err
	}

	hostIndex, err := linkIndex(n.HostVeth)
	if err != nil {
		return err
	}

	// Both ends of a veth carry their own MTU, and the container only ever
	// configures its own. Leaving the host end at the kernel's default while
	// the peer is lowered is not a cosmetic mismatch: veth_xmit drops a frame
	// larger than the *receiving* end's MTU, and the sending end's own MTU is
	// what decides whether the kernel fragments or reports "fragmentation
	// needed" first. A host end at 1500 feeding a peer at 1400 therefore
	// forwards oversized frames into a silent drop, with nothing upstream told
	// to send anything smaller — small packets work, large ones vanish.
	//
	// It is set here rather than left to Configure because Configure runs
	// inside the container's namespace, where the host end is unreachable.
	if n.Interface.MTU > 0 {
		if err := setLinkMTU(conn, hostIndex, n.Interface.MTU); err != nil {
			return err
		}
	}

	bridgeIndex, err := linkIndex(m.bridge)
	if err != nil {
		return fmt.Errorf("%w: %s is not present; call Ensure first: %w", ErrBridgeUnavailable, m.bridge, err)
	}
	if err := setLinkMaster(conn, hostIndex, bridgeIndex); err != nil {
		return err
	}
	if err := setLinkUp(conn, hostIndex); err != nil {
		return err
	}

	if m.logger != nil {
		m.logger.Debug("attached the container to the bridge",
			"container_id", n.ContainerID,
			"host_veth", n.HostVeth,
			"peer_veth", n.PeerVeth,
			"bridge", m.bridge,
			"address", n.Interface.Address,
			"pid", pid,
		)
	}

	return nil
}

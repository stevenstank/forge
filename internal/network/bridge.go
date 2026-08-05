package network

import (
	"errors"
	"fmt"

	"golang.org/x/sys/unix"
)

// Forge's bridge.
//
// One bridge per host, shared by every container, holding the gateway address
// that every container's default route points at. Containers on it can reach
// each other and, once nat.go has done its work, the outside world.
//
// # Why the bridge is never removed
//
// Ensure creates it on demand; nothing deletes it. Two concurrent `forge run`s
// would otherwise race to remove a bridge the other is about to attach a
// container to, and the loser would fail for reasons that have nothing to do
// with what it was asked to do. An idle bridge costs a few hundred bytes of
// kernel memory and is visible in `ip link`, which is a better trade than a
// race. This is the same decision Stage 3 made for /sys/fs/cgroup/forge, and
// it is recorded in ADR-0019 alongside it.

// bridgeCreateMessage builds the RTM_NEWLINK body that creates a bridge.
//
// Like the veth message it is separated from the send so its bytes can be
// asserted without a kernel. It is the simpler of the two: a bridge has no
// peer, so there is one level of nesting rather than three.
//
//	ifinfomsg
//	IFLA_IFNAME       = name
//	IFLA_LINKINFO
//	  IFLA_INFO_KIND  = "bridge"
func bridgeCreateMessage(name string) []byte {
	return concat(
		ifInfoMsg(unix.AF_UNSPEC, 0, 0, 0),
		nlAttrString(unix.IFLA_IFNAME, name),
		nlNestedPlain(unix.IFLA_LINKINFO,
			nlAttrString(unix.IFLA_INFO_KIND, "bridge"),
		),
	)
}

// Ensure makes the host side ready for containers (FR-4.2, FR-4.4).
//
// It creates the bridge if it is missing, gives it the subnet's gateway
// address, brings it up, turns on IPv4 forwarding, and installs the masquerade
// rule. Every step is idempotent, so calling it before each container — which
// is what internal/runtime does — costs one netlink round trip on the second
// and subsequent runs rather than being an error.
//
// It is deliberately not called from New: New performs no I/O, and whether the
// host has a usable bridge is a question about the machine rather than about
// the configuration, so it is answered where it can be reported per container.
func (m *Manager) Ensure() error {
	conn, err := dialNetlink(unix.NETLINK_ROUTE)
	if err != nil {
		return err
	}
	defer func() {
		if err := conn.close(); err != nil && m.logger != nil {
			m.logger.Warn("closing the netlink socket after preparing the bridge", "error", err)
		}
	}()

	created, err := m.ensureBridgeLink(conn)
	if err != nil {
		return err
	}

	index, err := linkIndex(m.bridge)
	if err != nil {
		return fmt.Errorf("%w: %s vanished after being created: %w", ErrBridgeUnavailable, m.bridge, err)
	}

	if err := m.ensureGatewayAddress(conn, index); err != nil {
		return err
	}
	if err := setLinkUp(conn, index); err != nil {
		return fmt.Errorf("%w: %w", ErrBridgeUnavailable, err)
	}

	if err := m.ensureNAT(); err != nil {
		return err
	}

	if created && m.logger != nil {
		m.logger.Info("created the forge bridge",
			"bridge", m.bridge,
			"gateway", m.subnet.GatewayCIDR(),
			"subnet", m.subnet.String(),
		)
	}

	return nil
}

// ensureBridgeLink creates the bridge if it is not already there, and reports
// whether it had to.
//
// A bridge that already exists is accepted as-is rather than recreated. On a
// host where a previous run left one behind that is the whole point; on a host
// where something else owns an interface of that name, adopting it is still
// better than deleting it, and the address check below will fail loudly if it
// is not the bridge Forge expects.
func (m *Manager) ensureBridgeLink(c *nlConn) (bool, error) {
	if linkExists(m.bridge) {
		return false, nil
	}

	body := bridgeCreateMessage(m.bridge)
	if err := c.execute(unix.RTM_NEWLINK, unix.NLM_F_CREATE|unix.NLM_F_EXCL, body); err != nil {
		// Another forge run may have created it between the check and here,
		// which is a success for both of them.
		if errors.Is(err, unix.EEXIST) || linkExists(m.bridge) {
			return false, nil
		}
		return false, fmt.Errorf("%w: creating %s: %w", ErrBridgeUnavailable, m.bridge, err)
	}

	return true, nil
}

// ensureGatewayAddress puts the subnet's gateway on the bridge.
//
// EEXIST is success: the address is already there, which is the state this is
// trying to reach. Anything else is not, because a bridge without the gateway
// address is a bridge every container's default route points into a void.
func (m *Manager) ensureGatewayAddress(c *nlConn, index int32) error {
	err := addAddress(c, index, m.subnet.GatewayCIDR())
	if err == nil {
		return nil
	}
	if errors.Is(err, unix.EEXIST) {
		return nil
	}

	return fmt.Errorf("%w: assigning the gateway %s to %s: %w",
		ErrBridgeUnavailable, m.subnet.GatewayCIDR(), m.bridge, err)
}

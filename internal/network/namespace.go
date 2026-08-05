package network

import (
	"fmt"

	"golang.org/x/sys/unix"
)

// The network namespace half of Stage 4 (FR-4.1).
//
// Forge does not create network namespaces here: they come from CLONE_NEWNET at
// clone(2), which internal/namespace owns because that is where clone flags are
// computed (SSOT §2). What this file does is the two things that happen either
// side of that namespace's boundary — the parent pushing an interface across
// it, and the container configuring what it finds on the other side.
//
// # Why the container configures itself
//
// The parent could enter the container's namespace with setns(2) and do the
// work there, and that is what most runtimes do. It is rejected here (ADR-0018)
// because setns in Go means locking an OS thread for the duration and hoping
// nothing in the runtime migrates: get it wrong and Forge itself is left
// sitting in a container's network namespace, which is a failure mode with no
// good recovery.
//
// Forge already has a mechanism for work that must happen inside the new
// namespaces — the re-exec handshake of ADR-0008, which Stage 2 already uses
// for mounts (ADR-0012). The parent does the one thing only it can do, which is
// to move an interface into a namespace it can name by PID; the container does
// the rest to itself, from a plain description that arrived over a pipe.

// moveLinkToNetns moves an interface into the network namespace of a process.
//
// The namespace is named by PID rather than by an open file descriptor. Both
// work — IFLA_NET_NS_FD takes a descriptor on /proc/<pid>/ns/net — but the PID
// form needs no descriptor to open, pass, and close correctly on every error
// path, and the process is one Forge started and is still waiting on, so it
// cannot have been recycled underneath us.
func moveLinkToNetns(c *nlConn, index int32, pid int) error {
	body := concat(
		ifInfoMsg(unix.AF_UNSPEC, index, 0, 0),
		nlAttrU32(unix.IFLA_NET_NS_PID, uint32(pid)),
	)

	if err := c.execute(unix.RTM_NEWLINK, 0, body); err != nil {
		return fmt.Errorf("moving interface %d into the network namespace of pid %d: %w", index, pid, err)
	}

	return nil
}

// Configure applies an interface description to the calling process's own
// network namespace (FR-4.3).
//
// This is the container-side half of the package and the only function in it
// that a container calls on itself. It takes plain data rather than a Manager
// because the container has none: no bridge, no pool, no state directory, just
// a struct that came over a pipe.
//
// The order is forced by the kernel and each step earns its place:
//
//  1. loopback up. A fresh namespace has lo, but down. Anything binding
//     127.0.0.1 fails until this happens, and the resulting errors look like
//     DNS problems rather than like a missing interface.
//  2. rename to eth0. The kernel refuses to rename an interface that is up, so
//     this precedes everything else. The interface arrives under its peer name
//     because both ends of a veth are briefly created on the host, where eth0
//     very probably already exists.
//  3. MTU, if one was asked for. Cheap while the link is still down.
//  4. address, then up, then the default route. The route needs the interface
//     up and addressed, or the kernel has no path to attach it to.
func Configure(iface Interface) error {
	if err := iface.Validate(); err != nil {
		return err
	}

	conn, err := dialNetlink(unix.NETLINK_ROUTE)
	if err != nil {
		return err
	}
	defer func() { _ = conn.close() }()

	if err := configureLoopback(conn); err != nil {
		return err
	}

	index, err := linkIndex(iface.Source)
	if err != nil {
		return fmt.Errorf("%w: the container's interface %q is not in its namespace: %w",
			ErrNotFound, iface.Source, err)
	}

	if iface.Source != iface.Name {
		if err := renameLink(conn, index, iface.Name); err != nil {
			return err
		}
	}

	if iface.MTU > 0 {
		if err := setLinkMTU(conn, index, iface.MTU); err != nil {
			return err
		}
	}

	if err := addAddress(conn, index, iface.Address); err != nil {
		return err
	}
	if err := setLinkUp(conn, index); err != nil {
		return err
	}

	// A container with no gateway is a container on an isolated segment, which
	// is a legitimate thing to ask for and not an error.
	if iface.Gateway != "" {
		if err := addDefaultRoute(conn, index, iface.Gateway); err != nil {
			return err
		}
	}

	return nil
}

// ConfigureLoopback brings up loopback and nothing else.
//
// It is what a container in ModeNone gets: its own network namespace, isolated
// from every other, but still able to talk to itself. Without it such a
// container cannot even connect to 127.0.0.1, which surprises everyone once.
func ConfigureLoopback() error {
	conn, err := dialNetlink(unix.NETLINK_ROUTE)
	if err != nil {
		return err
	}
	defer func() { _ = conn.close() }()

	return configureLoopback(conn)
}

// loopbackName is what the kernel calls loopback in every namespace.
const loopbackName = "lo"

// configureLoopback brings lo up on an existing connection.
func configureLoopback(c *nlConn) error {
	index, err := linkIndex(loopbackName)
	if err != nil {
		return fmt.Errorf("%w: no loopback interface in this namespace: %w", ErrNotFound, err)
	}

	if err := setLinkUp(c, index); err != nil {
		return fmt.Errorf("bringing loopback up: %w", err)
	}

	return nil
}

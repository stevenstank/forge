package network

import (
	"errors"
	"fmt"

	"golang.org/x/sys/unix"
)

// Teardown (FR-4.5).
//
// Unlike a network namespace, which the kernel destroys with its last process,
// the two things Forge holds for a container outlive it: a veth pair persists
// until something deletes it, and an IP lease is a file that persists until
// something unlinks it. So this is real work, and Destroy is the only place it
// happens.
//
// # Reverse order
//
// Acquisition is address, then interface: Allocate claims a lease before the
// container exists and Attach creates the veth once it does. Release is
// therefore interface, then address. The order is not cosmetic — the lease is
// what proves an address is spoken for, so releasing it first would open a
// window in which another container could be handed an address while the
// previous holder's interface was still plugged into the bridge carrying it.
//
// # Idempotence
//
// Every step treats "already gone" as success (SSOT §13.3). That is what lets
// internal/runtime push Destroy onto its cleanup stack the moment Allocate
// succeeds and have it be correct for every later failure — a veth that was
// never created, a container that exited normally and took its interface with
// it, or a Destroy that already ran.

// Destroy releases everything the Manager holds for a container (FR-4.5).
//
// It is idempotent, and it is deliberately tolerant of partial state: a
// container that got an address but never an interface, or one whose interface
// the kernel has already reclaimed, are both ordinary cases rather than errors.
//
// Failures in the two steps are collected rather than short-circuited. A veth
// that cannot be deleted must not prevent the address being returned to the
// pool, or one stuck interface would leak an address permanently.
func (m *Manager) Destroy(containerID string) error {
	if err := validateContainerID(containerID); err != nil {
		return err
	}

	var errs []error

	// 1. The interface, which is what carries traffic for the address below.
	if err := m.destroyVeth(containerID); err != nil {
		errs = append(errs, err)
	}

	// 2. The address, which is only safe to hand out once nothing is using it.
	if err := m.release(containerID); err != nil {
		errs = append(errs, err)
	}

	if len(errs) > 0 {
		return fmt.Errorf("releasing the network of container %s: %w", containerID, errors.Join(errs...))
	}

	if m.logger != nil {
		m.logger.Debug("released the container network", "container_id", containerID)
	}

	return nil
}

// destroyVeth removes the container's veth pair.
//
// Deleting the host end deletes both, so the peer inside the container's
// namespace needs no separate handling — and in the common case there is
// nothing to do at all: when the container's last process exits, the kernel
// destroys its network namespace, which destroys the peer, which takes the host
// end with it. This exists for the paths where that has not happened: a run
// that failed between Attach and the container starting, or a container still
// running when the orchestrator unwinds.
func (m *Manager) destroyVeth(containerID string) error {
	host, _, err := VethNames(containerID)
	if err != nil {
		return err
	}

	// Nothing to delete is the state Destroy exists to reach, and checking
	// first avoids opening a netlink socket in the overwhelmingly common case
	// where the kernel has already done the work.
	if !linkExists(host) {
		return nil
	}

	conn, err := dialNetlink(unix.NETLINK_ROUTE)
	if err != nil {
		return err
	}
	defer func() {
		if err := conn.close(); err != nil && m.logger != nil {
			m.logger.Warn("closing the netlink socket after removing a veth", "error", err)
		}
	}()

	if err := deleteVeth(conn, host); err != nil {
		return fmt.Errorf("deleting %s: %w", host, err)
	}

	return nil
}

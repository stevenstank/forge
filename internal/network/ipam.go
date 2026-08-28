package network

import (
	"encoding/binary"
	"errors"
	"fmt"
	"iter"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"
)

// Address management (FR-4.3).
//
// Two halves, as everywhere else in this package: Subnet is pure arithmetic on
// a CIDR, and the lease functions below are the filesystem half that decides
// which address a container actually gets.
//
// # Why leases are files
//
// Stage 4 has no state store — that arrives in Stage 6 — but two `forge run`s
// started at the same instant must not be handed the same address. The claim is
// therefore made with O_CREAT|O_EXCL on a file named after the address: the
// kernel arbitrates, there is no lock to leak, and no daemon has to be running.
// The file's contents are the container ID, which is what makes a lease
// independently verifiable later (ADR-0019).

// leaseDirName is the directory under StateDir that holds one file per claimed
// address.
const leaseDirName = "leases"

// Subnet is an IPv4 CIDR that containers are addressed from.
//
// Addresses are held as uint32 rather than net.IP because every operation here
// is arithmetic — "the next host", "is this inside" — and 4-byte slices with
// their v4-in-v6 ambiguity make that harder to get right, not easier.
type Subnet struct {
	base   uint32
	prefix int
}

// ParseSubnet parses an IPv4 CIDR such as "10.99.0.0/16".
//
// The subnet must be large enough for a gateway and at least one container, so
// a /31 or /32 is refused: they would parse cleanly and then produce a pool
// with nothing in it, which is a worse failure than being told now.
func ParseSubnet(cidr string) (Subnet, error) {
	ip, ipNet, err := net.ParseCIDR(strings.TrimSpace(cidr))
	if err != nil {
		return Subnet{}, fmt.Errorf("%w: %q is not a CIDR such as 10.99.0.0/16", ErrInvalidSubnet, cidr)
	}
	if ip.To4() == nil {
		return Subnet{}, fmt.Errorf("%w: %q is not IPv4; Forge does not do IPv6 yet", ErrInvalidSubnet, cidr)
	}

	prefix, bits := ipNet.Mask.Size()
	if bits != 32 {
		return Subnet{}, fmt.Errorf("%w: %q has a non-IPv4 mask", ErrInvalidSubnet, cidr)
	}
	// /31 leaves two addresses and /32 leaves one; both are consumed by the
	// network address and the gateway, so no container could ever be placed.
	if prefix > 30 {
		return Subnet{}, fmt.Errorf("%w: /%d leaves no room for a gateway and a container; use /30 or larger",
			ErrInvalidSubnet, prefix)
	}

	return Subnet{base: ipToU32(ipNet.IP), prefix: prefix}, nil
}

// String renders the subnet in CIDR form.
func (s Subnet) String() string {
	return fmt.Sprintf("%s/%d", u32ToIP(s.base), s.prefix)
}

// PrefixLen returns the subnet's prefix length.
func (s Subnet) PrefixLen() int { return s.prefix }

// Gateway returns the address Forge gives the bridge: the first usable host in
// the subnet, which is what every container's default route points at.
func (s Subnet) Gateway() string { return u32ToIP(s.base + 1).String() }

// GatewayCIDR returns the gateway with the subnet's prefix, which is the form
// the kernel wants when the address is assigned to the bridge.
func (s Subnet) GatewayCIDR() string { return fmt.Sprintf("%s/%d", s.Gateway(), s.prefix) }

// Size returns how many addresses the subnet can hand to containers: every
// address except the network address, the gateway, and the broadcast address.
func (s Subnet) Size() int {
	total := uint64(1) << uint(32-s.prefix)
	// Network address, gateway, broadcast.
	if total < 3 {
		return 0
	}
	return int(total - 3)
}

// Contains reports whether ip falls inside the subnet.
func (s Subnet) Contains(ip string) bool {
	parsed, err := parseIPv4(ip)
	if err != nil {
		return false
	}
	return parsed&s.mask() == s.base
}

// Hosts iterates the addresses a container may be given, in ascending order.
//
// The network address, the gateway, and the broadcast address are skipped, so
// what this yields is exactly the allocatable pool and callers need no further
// arithmetic of their own.
func (s Subnet) Hosts() iter.Seq[string] {
	return func(yield func(string) bool) {
		// The first host is base+1 and is the gateway, so allocation starts one
		// past it; the last is the broadcast address and is excluded.
		first := s.base + 2
		last := s.base | ^s.mask()

		for addr := first; addr < last; addr++ {
			if !yield(u32ToIP(addr).String()) {
				return
			}
		}
	}
}

// mask returns the subnet mask as a uint32.
func (s Subnet) mask() uint32 { return ^uint32(0) << uint(32-s.prefix) }

// ipToU32 converts a 4-byte IPv4 address to a uint32 in host terms.
func ipToU32(ip net.IP) uint32 {
	v4 := ip.To4()
	if v4 == nil {
		return 0
	}
	return binary.BigEndian.Uint32(v4)
}

// u32ToIP converts back.
func u32ToIP(v uint32) net.IP {
	ip := make(net.IP, net.IPv4len)
	binary.BigEndian.PutUint32(ip, v)
	return ip
}

// parseIPv4 parses a bare IPv4 address into its numeric form.
func parseIPv4(s string) (uint32, error) {
	ip := net.ParseIP(strings.TrimSpace(s))
	if ip == nil || ip.To4() == nil {
		return 0, fmt.Errorf("%q is not an IPv4 address", s)
	}
	return ipToU32(ip), nil
}

// parseCIDR parses an address-with-prefix such as "10.99.0.7/16" and reports
// the address and prefix length separately.
func parseCIDR(s string) (Subnet, error) {
	ip, ipNet, err := net.ParseCIDR(strings.TrimSpace(s))
	if err != nil {
		return Subnet{}, fmt.Errorf("%q is not an address with a prefix", s)
	}
	if ip.To4() == nil {
		return Subnet{}, fmt.Errorf("%q is not IPv4", s)
	}

	prefix, _ := ipNet.Mask.Size()

	return Subnet{base: ipToU32(ip), prefix: prefix}, nil
}

// address returns the address part of a parsed CIDR.
func (s Subnet) address() string { return u32ToIP(s.base).String() }

// lease is what a lease file records: who holds the address, and which process
// claimed it.
//
// The PID is not bookkeeping. An address is claimed before the container
// exists, so between Allocate and Attach there is a window in which the holder
// has no interface to point at — and a reclaimer that only asked "does the veth
// exist?" would decide a container that is halfway through starting is dead and
// hand its address to somebody else. The claiming process is alive for the
// whole of that window, so it is what closes it.
type lease struct {
	containerID string
	pid         int
}

// encode renders a lease for the file. Two lines rather than JSON: this is read
// by the same package that writes it, and a format a human can read with cat is
// worth more here than a marshaller.
func (l lease) encode() string {
	return fmt.Sprintf("%s\n%d\n", l.containerID, l.pid)
}

// decodeLease parses a lease file. A file with no PID line is from an older
// Forge and is treated as PID 0, which is never alive — such a lease is then
// judged solely on whether its interface still exists, which is the behaviour
// it was written under.
func decodeLease(content string) lease {
	lines := strings.SplitN(strings.TrimSpace(content), "\n", 2)

	l := lease{containerID: strings.TrimSpace(lines[0])}
	if len(lines) > 1 {
		if pid, err := strconv.Atoi(strings.TrimSpace(lines[1])); err == nil {
			l.pid = pid
		}
	}

	return l
}

// leaseAlive reports whether a lease still has an owner.
//
// Either signal is enough. The interface exists once the container is attached
// and running; the process exists from the moment the address is claimed until
// Forge exits. Together they cover the whole life of a container, including the
// gap between the two that the interface check alone would miss.
func leaseAlive(l lease) bool {
	host, _, err := VethNames(l.containerID)
	if err == nil && linkExists(host) {
		return true
	}

	return pidAlive(l.pid)
}

// pidAlive reports whether a process exists.
//
// Signal 0 performs the permission and existence checks without delivering
// anything. EPERM means the process is there and owned by somebody else, which
// is still very much alive.
func pidAlive(pid int) bool {
	if pid <= 0 {
		return false
	}

	err := unix.Kill(pid, 0)

	return err == nil || errors.Is(err, unix.EPERM)
}

// leaseDir is where this Manager records claimed addresses.
func (m *Manager) leaseDir() string { return filepath.Join(m.stateDir, leaseDirName) }

// leasePath is the file that claims one address.
func (m *Manager) leasePath(ip string) string { return filepath.Join(m.leaseDir(), ip) }

// Allocate reserves an address for a container and returns its complete
// networking plan (FR-4.3).
//
// It creates nothing in the kernel. The veth pair cannot exist until the
// container does, so Allocate is the part that can run before clone(2) and
// Attach is the part that cannot.
//
// A container that already holds an address is an error rather than a silent
// re-use: two allocations for one ID means the caller lost track of a
// container, and handing out a second address would leak the first.
func (m *Manager) Allocate(containerID string) (*Network, error) {
	host, peer, err := VethNames(containerID)
	if err != nil {
		return nil, err
	}

	if err := os.MkdirAll(m.leaseDir(), 0o755); err != nil {
		return nil, fmt.Errorf("creating the lease directory %s: %w", m.leaseDir(), translate(err))
	}

	if existing, ok, err := m.Lookup(containerID); err != nil {
		return nil, err
	} else if ok {
		return nil, fmt.Errorf("%w: %s already holds %s", ErrAlreadyAllocated, containerID, existing)
	}

	ip, err := m.claim(containerID)
	if err != nil {
		return nil, err
	}

	return &Network{
		ContainerID: containerID,
		HostVeth:    host,
		PeerVeth:    peer,
		Interface: Interface{
			Source:  peer,
			Name:    ContainerIfaceName,
			Address: fmt.Sprintf("%s/%d", ip, m.subnet.PrefixLen()),
			Gateway: m.subnet.Gateway(),
		},
	}, nil
}

// claim takes the first free address in the subnet.
//
// The directory is read once to find what is already taken, and O_EXCL still
// guards each attempt: the listing is a shortcut past addresses that are
// obviously in use, not the thing that makes the claim safe. A concurrent run
// that takes the same address first is simply lost to EEXIST and the loop moves
// on.
func (m *Manager) claim(containerID string) (string, error) {
	taken, err := m.takenAddresses()
	if err != nil {
		return "", err
	}

	if ip, ok := m.tryClaim(taken, containerID); ok {
		return ip, nil
	}

	// Nothing free on the first pass. Before giving up, reclaim the leases of
	// containers that are demonstrably gone — a Forge killed with SIGKILL
	// leaves its lease behind, and the pool must not leak because of it.
	reclaimed, err := m.reclaimStale(leaseAlive)
	if err != nil {
		return "", err
	}
	if reclaimed == 0 {
		return "", fmt.Errorf("%w: %s has %d addresses and all are in use",
			ErrPoolExhausted, m.subnet, m.subnet.Size())
	}

	taken, err = m.takenAddresses()
	if err != nil {
		return "", err
	}
	if ip, ok := m.tryClaim(taken, containerID); ok {
		return ip, nil
	}

	return "", fmt.Errorf("%w: %s has %d addresses and all are in use",
		ErrPoolExhausted, m.subnet, m.subnet.Size())
}

// tryClaim walks the pool once, attempting to create a lease for the first
// address not already known to be taken.
func (m *Manager) tryClaim(taken map[string]bool, containerID string) (string, bool) {
	for ip := range m.subnet.Hosts() {
		if taken[ip] {
			continue
		}

		f, err := os.OpenFile(m.leasePath(ip), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
		if err != nil {
			// EEXIST means another run won the race for this address. Anything
			// else — a permission problem, a missing directory — will recur for
			// every address, so there is no point walking the rest of the pool.
			if errors.Is(err, os.ErrExist) {
				continue
			}
			return "", false
		}

		_, writeErr := f.WriteString(lease{containerID: containerID, pid: os.Getpid()}.encode())
		closeErr := f.Close()
		if writeErr != nil || closeErr != nil {
			// A lease that does not name its owner cannot be verified or
			// reclaimed later, so it is worse than no lease at all. If it
			// cannot be removed either, the address is lost to the pool until
			// something cleans the directory up, which is worth a word.
			if rmErr := os.Remove(m.leasePath(ip)); rmErr != nil &&
				!errors.Is(rmErr, os.ErrNotExist) && m.logger != nil {
				m.logger.Warn("could not remove an unwritable lease file",
					"address", ip, "error", rmErr)
			}
			return "", false
		}

		return ip, true
	}

	return "", false
}

// takenAddresses returns the addresses that currently have a lease.
func (m *Manager) takenAddresses() (map[string]bool, error) {
	entries, err := os.ReadDir(m.leaseDir())
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return map[string]bool{}, nil
		}
		return nil, fmt.Errorf("reading the lease directory %s: %w", m.leaseDir(), translate(err))
	}

	taken := make(map[string]bool, len(entries))
	for _, entry := range entries {
		if !entry.IsDir() {
			taken[entry.Name()] = true
		}
	}

	return taken, nil
}

// Lookup returns the address leased to a container, if it has one.
func (m *Manager) Lookup(containerID string) (string, bool, error) {
	if err := validateContainerID(containerID); err != nil {
		return "", false, err
	}

	entries, err := os.ReadDir(m.leaseDir())
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", false, nil
		}
		return "", false, fmt.Errorf("reading the lease directory %s: %w", m.leaseDir(), translate(err))
	}

	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		held, err := m.readLease(entry.Name())
		if err != nil {
			return "", false, err
		}
		if held.containerID == containerID {
			return entry.Name(), true, nil
		}
	}

	return "", false, nil
}

// readLease reads the lease on an address. A lease that vanished between a
// listing and the read is not an error: it was released by whoever owned it,
// and an empty container ID is how that is reported.
func (m *Manager) readLease(ip string) (lease, error) {
	content, err := os.ReadFile(m.leasePath(ip))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return lease{}, nil
		}
		return lease{}, fmt.Errorf("reading the lease %s: %w", m.leasePath(ip), translate(err))
	}

	return decodeLease(string(content)), nil
}

// release drops the address a container holds (FR-4.5).
//
// It is idempotent: a container that never had an address, or whose lease has
// already gone, is success. That is what lets Destroy be registered on the
// cleanup stack unconditionally.
func (m *Manager) release(containerID string) error {
	ip, ok, err := m.Lookup(containerID)
	if err != nil {
		return err
	}
	if !ok {
		return nil
	}

	if err := os.Remove(m.leasePath(ip)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("releasing the lease %s: %w", m.leasePath(ip), translate(err))
	}

	return nil
}

// reclaimStale removes the leases of containers that are demonstrably gone, and
// reports how many it took back.
//
// "Demonstrably gone" means the host end of the container's veth pair no longer
// exists. The kernel is the source of truth and the lease file is a hint, which
// is exactly why the interface name is derived from the container ID rather
// than being random: it makes a lease checkable by something other than the
// process that wrote it (ADR-0019).
//
// alive is a parameter rather than a fixed call so the pool's behaviour can be
// tested without creating interfaces or waiting for processes to die. It is a
// function, not an interface: there is one implementation in production and no
// polymorphism to model.
func (m *Manager) reclaimStale(alive func(lease) bool) (int, error) {
	entries, err := os.ReadDir(m.leaseDir())
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return 0, nil
		}
		return 0, fmt.Errorf("reading the lease directory %s: %w", m.leaseDir(), translate(err))
	}

	reclaimed := 0
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}

		held, err := m.readLease(entry.Name())
		if err != nil {
			return reclaimed, err
		}
		// A lease with no owner recorded cannot be verified, so it is left
		// alone rather than guessed at.
		if held.containerID == "" {
			continue
		}
		if alive(held) {
			continue
		}

		if err := os.Remove(m.leasePath(entry.Name())); err != nil && !errors.Is(err, os.ErrNotExist) {
			return reclaimed, fmt.Errorf("reclaiming the lease %s: %w", m.leasePath(entry.Name()), translate(err))
		}
		reclaimed++
	}

	return reclaimed, nil
}

// validateContainerID reports whether id can be used as a single path element
// and as the stem of an interface name.
//
// This is a containment check, not a style check: an ID containing a separator
// or ".." would place a lease outside Forge's state directory, and release
// would then remove a file that is not Forge's.
func validateContainerID(id string) error {
	switch {
	case id == "":
		return fmt.Errorf("%w: empty", ErrInvalidContainerID)
	case id == "." || id == "..":
		return fmt.Errorf("%w: %q is a path element with special meaning", ErrInvalidContainerID, id)
	case strings.ContainsAny(id, `/\`):
		return fmt.Errorf("%w: %q contains a path separator", ErrInvalidContainerID, id)
	case strings.ContainsRune(id, 0):
		return fmt.Errorf("%w: contains a NUL byte", ErrInvalidContainerID)
	}

	return nil
}

// translate maps the errors the kernel returns onto this package's sentinels,
// so a caller gets a usable message rather than "permission denied" with no
// clue which capability is missing.
func translate(err error) error {
	if errors.Is(err, os.ErrPermission) {
		return fmt.Errorf("%w: %w", ErrPermission, err)
	}
	return err
}

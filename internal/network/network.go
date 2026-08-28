// Package network gives a container its own network namespace, a veth pair, an
// address from a private subnet, and a route to the internet.
//
// One veth pair per container (FR-4.2), one address per container (FR-4.3),
// with the host end attached to a Forge-managed bridge and a masquerade rule so
// the container can reach the outside world (FR-4.4). Everything the kernel is
// told goes over a netlink socket — no netlink library, no shelling out to `ip`
// or `iptables` (ADR-0016, ADR-0017).
//
// The package is deliberately ignorant of what a container is. It is handed a
// container ID and, per SSOT §2, owns only the networking resources for that
// ID: it never decides whether a container should be networked, never reads a
// flag, and never touches another primitive package.
//
// # The two halves
//
// Like internal/mount and internal/cgroup, computation is separated from kernel
// contact:
//
//   - The message builders below, ipam.go's subnet arithmetic, and veth.go's
//     name derivation are pure. A netlink message is a byte layout, so it is
//     built by a function that returns bytes and asserted as data.
//   - The rest opens a socket and sends them. Point Config.StateDir at a
//     t.TempDir() and the whole of IPAM runs unprivileged; the link operations
//     need CAP_NET_ADMIN and are covered by the privileged suite.
//
// # Rollback
//
// There is none inside a single operation. A Attach that fails part-way leaves
// what it made and says so; internal/runtime owns the cleanup stack and calls
// Destroy, which is idempotent and releases every resource this package can
// hold for a container, in reverse order of acquisition (SSOT §11.3, §13.3).
package network

import (
	"encoding/binary"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"path/filepath"
	"time"

	"golang.org/x/sys/unix"
)

// Defaults for Config. Every one of them is overridable, because a host may
// already be using the name or the range.
const (
	// DefaultBridge is the Forge-managed bridge every container attaches to.
	DefaultBridge = "forge0"

	// DefaultSubnet is the private range containers are addressed from. It is
	// deliberately not 172.17.0.0/16 (Docker) or 10.88.0.0/16 (Podman), so a
	// machine running one of those keeps working.
	DefaultSubnet = "10.99.0.0/16"

	// DefaultStateDir is where IP leases live. It sits under Forge's state
	// directory rather than the container root, because a lease outlives the
	// container directory during teardown.
	DefaultStateDir = "/var/lib/forge/network"
)

// Sentinel errors callers may branch on (SSOT §5).
var (
	// ErrInvalidSubnet reports a subnet that cannot be parsed, or that is too
	// small to hold a gateway and at least one container.
	ErrInvalidSubnet = errors.New("invalid subnet")

	// ErrInvalidContainerID reports an ID that cannot be turned into a legal
	// interface name, or that would escape the lease directory.
	ErrInvalidContainerID = errors.New("invalid container id")

	// ErrPoolExhausted reports a subnet with no free addresses left.
	ErrPoolExhausted = errors.New("no free addresses in the subnet")

	// ErrAlreadyAllocated reports a second allocation for one container ID.
	ErrAlreadyAllocated = errors.New("container already has an address")

	// ErrNotFound reports an operation on a container that has no allocation.
	ErrNotFound = errors.New("no network allocation for container")

	// ErrBridgeUnavailable reports a bridge that cannot be created or used.
	ErrBridgeUnavailable = errors.New("forge bridge unavailable")

	// ErrNATUnavailable reports a kernel that will not accept Forge's NAT
	// configuration — typically one built without nf_tables.
	ErrNATUnavailable = errors.New("NAT configuration unavailable")

	// ErrPermission reports that the caller cannot manage networking.
	ErrPermission = errors.New("managing networking requires root (or CAP_NET_ADMIN)")

	// ErrInvalidInterface reports a container-side interface description that
	// cannot be applied.
	ErrInvalidInterface = errors.New("invalid interface configuration")
)

// Mode is how a container is attached to the network.
type Mode string

// The network modes Stage 4 offers.
const (
	// ModeBridge is the default: a private netns, a veth pair, an address, and
	// NAT to the outside world (FR-4.1 … FR-4.4).
	ModeBridge Mode = "bridge"

	// ModeNone gives a private netns with nothing in it but loopback. The
	// container is isolated rather than connected.
	ModeNone Mode = "none"

	// ModeHost leaves the container in the host's network namespace, which is
	// exactly what Stages 1 to 3 did. It is the escape hatch that keeps every
	// earlier example working unchanged.
	ModeHost Mode = "host"
)

// Validate reports whether the mode is one Forge implements.
func (m Mode) Validate() error {
	switch m {
	case ModeBridge, ModeNone, ModeHost:
		return nil
	default:
		return fmt.Errorf("%w: %q is not one of %q, %q or %q",
			ErrInvalidInterface, string(m), ModeBridge, ModeNone, ModeHost)
	}
}

// NeedsNetns reports whether the mode requires CLONE_NEWNET. internal/namespace
// owns the flag itself; this only states the requirement.
func (m Mode) NeedsNetns() bool { return m != ModeHost }

// NeedsVeth reports whether the mode requires a veth pair and an address.
func (m Mode) NeedsVeth() bool { return m == ModeBridge }

// Config configures a Manager. Zero values take the defaults, so
// network.New(logger, network.Config{}) is the production configuration.
type Config struct {
	// Bridge is the name of the Forge-managed bridge. Empty means
	// DefaultBridge.
	Bridge string

	// Subnet is the CIDR containers are addressed from. Empty means
	// DefaultSubnet.
	Subnet string

	// StateDir is where IP leases are recorded. Empty means DefaultStateDir.
	// Tests point this at a temporary directory.
	StateDir string
}

// Interface is the container-side configuration the container applies to
// itself. It is the only part of this package that crosses the re-exec
// boundary, so it is plain data: strings the child can act on without needing
// a Manager, a bridge, or a pool (ADR-0018).
type Interface struct {
	// Source is the name the interface arrives under, before it is renamed.
	// It is the peer half of the veth pair.
	Source string `json:"source"`

	// Name is what the interface is called once configured, conventionally
	// "eth0".
	Name string `json:"name"`

	// Address is the container's address in CIDR form, e.g. "10.99.0.7/16".
	Address string `json:"address"`

	// Gateway is the bridge's address, which the default route points at.
	Gateway string `json:"gateway"`

	// MTU is the interface MTU. Zero leaves the kernel's default.
	MTU int `json:"mtu,omitempty"`
}

// Validate reports whether the interface can be applied. It is pure, so a bad
// description is refused before the container is asked to act on it.
func (i Interface) Validate() error {
	switch {
	case i.Source == "":
		return fmt.Errorf("%w: no source interface name", ErrInvalidInterface)
	case i.Name == "":
		return fmt.Errorf("%w: no target interface name", ErrInvalidInterface)
	case i.Address == "":
		return fmt.Errorf("%w: no address", ErrInvalidInterface)
	}

	if err := validateIfaceName(i.Source); err != nil {
		return err
	}
	if err := validateIfaceName(i.Name); err != nil {
		return err
	}
	if _, err := parseCIDR(i.Address); err != nil {
		return fmt.Errorf("%w: address %q: %w", ErrInvalidInterface, i.Address, err)
	}
	if i.Gateway != "" {
		if _, err := parseIPv4(i.Gateway); err != nil {
			return fmt.Errorf("%w: gateway %q: %w", ErrInvalidInterface, i.Gateway, err)
		}
	}
	if i.MTU < 0 {
		return fmt.Errorf("%w: mtu %d is negative", ErrInvalidInterface, i.MTU)
	}

	return nil
}

// Network is one container's allocated networking, as returned by Allocate.
//
// It is a plan rather than a state handle: every field is derived from the
// container ID and the subnet, so a Manager can reconstruct it and tear it
// down without having been the one to create it (SSOT §13.4).
type Network struct {
	// ContainerID is the container this belongs to.
	ContainerID string

	// HostVeth is the host end, which is enslaved to the bridge.
	HostVeth string

	// PeerVeth is the end that is moved into the container's netns.
	PeerVeth string

	// Interface is what the container configures on itself.
	Interface Interface
}

// Manager owns Forge's bridge, its NAT rules, and the address pool.
//
// It holds no per-container state: every operation is addressed by container
// ID and re-derives what it needs, so a Manager can destroy an allocation it
// did not create — which is what crash recovery and, from Stage 6, `forge rm`
// need (SSOT §11.2, §13.4).
type Manager struct {
	logger   *slog.Logger
	bridge   string
	subnet   Subnet
	stateDir string
}

// New returns a Manager for cfg.
//
// It performs no I/O and touches neither the kernel nor the filesystem: the
// only thing that can fail here is the configuration itself, which is pure to
// check. Whether the host actually has a usable bridge, or the privileges to
// make one, depends on what is being asked for and is reported by Ensure.
func New(logger *slog.Logger, cfg Config) (*Manager, error) {
	if cfg.Bridge == "" {
		cfg.Bridge = DefaultBridge
	}
	if cfg.Subnet == "" {
		cfg.Subnet = DefaultSubnet
	}
	if cfg.StateDir == "" {
		cfg.StateDir = DefaultStateDir
	}

	if err := validateIfaceName(cfg.Bridge); err != nil {
		return nil, fmt.Errorf("bridge name %q: %w", cfg.Bridge, err)
	}

	subnet, err := ParseSubnet(cfg.Subnet)
	if err != nil {
		return nil, err
	}

	return &Manager{
		logger:   logger,
		bridge:   cfg.Bridge,
		subnet:   subnet,
		stateDir: filepath.Clean(cfg.StateDir),
	}, nil
}

// Bridge returns the name of the bridge the Manager attaches containers to.
func (m *Manager) Bridge() string { return m.bridge }

// Subnet returns the range containers are addressed from.
func (m *Manager) Subnet() Subnet { return m.subnet }

// StateDir returns the directory IP leases are recorded in.
func (m *Manager) StateDir() string { return m.stateDir }

// --- netlink message construction -------------------------------------------
//
// Everything below is pure. A netlink message is a byte layout, so building one
// is a function from values to bytes, and the tests assert the bytes. That is
// the same seam cgroup.Limits.Files provides: what the kernel is told is
// checkable without a kernel.

// Netlink header and attribute sizes, from linux/netlink.h.
const (
	nlMsgHdrLen  = 16 // struct nlmsghdr
	nlAttrHdrLen = 4  // struct nlattr

	// ifInfoMsgLen is sizeof(struct ifinfomsg) from linux/rtnetlink.h.
	ifInfoMsgLen = 16
	// ifAddrMsgLen is sizeof(struct ifaddrmsg).
	ifAddrMsgLen = 8
	// rtMsgLen is sizeof(struct rtmsg).
	rtMsgLen = 12
)

// nlaFNested is NLA_F_NESTED. Modern kernels want it on nested attributes, and
// nf_tables rejects them without it.
const nlaFNested uint16 = 0x8000

// order is the byte order netlink uses: the host's own, not network order.
// Getting this wrong produces messages the kernel rejects with EINVAL and no
// further explanation, so it is named rather than assumed at each call site.
var order = binary.NativeEndian

// nlAlign rounds a length up to netlink's 4-byte alignment (NLMSG_ALIGN).
func nlAlign(n int) int { return (n + 3) &^ 3 }

// nlAttr encodes one netlink attribute: a length, a type, the payload, and
// enough padding to align the next attribute.
//
// The encoded length covers the header and the payload but *not* the padding,
// which is why this cannot be a simple append of a header to a body.
func nlAttr(typ uint16, payload []byte) []byte {
	length := nlAttrHdrLen + len(payload)
	buf := make([]byte, nlAlign(length))

	order.PutUint16(buf[0:2], uint16(length))
	order.PutUint16(buf[2:4], typ)
	copy(buf[nlAttrHdrLen:], payload)

	return buf
}

// nlAttrString encodes a NUL-terminated string attribute, which is what the
// kernel expects for names such as IFLA_IFNAME.
func nlAttrString(typ uint16, s string) []byte {
	return nlAttr(typ, append([]byte(s), 0))
}

// nlAttrU32 encodes a 32-bit attribute in host byte order.
func nlAttrU32(typ uint16, v uint32) []byte {
	payload := make([]byte, 4)
	order.PutUint32(payload, v)
	return nlAttr(typ, payload)
}

// nlAttrU32BE encodes a 32-bit attribute in big-endian order. nf_tables stores
// several of its own fields network-order regardless of the host.
func nlAttrU32BE(typ uint16, v uint32) []byte {
	payload := make([]byte, 4)
	binary.BigEndian.PutUint32(payload, v)
	return nlAttr(typ, payload)
}

// nlNested encodes an attribute whose payload is itself a sequence of
// attributes, with NLA_F_NESTED set.
func nlNested(typ uint16, children ...[]byte) []byte {
	return nlAttr(typ|nlaFNested, concat(children...))
}

// nlNestedPlain is nlNested without NLA_F_NESTED, for the rtnetlink attributes
// that predate the flag and are matched by exact type — IFLA_LINKINFO and the
// veth peer block among them.
func nlNestedPlain(typ uint16, children ...[]byte) []byte {
	return nlAttr(typ, concat(children...))
}

// concat joins byte slices. It exists so the builders below read as a list of
// attributes rather than as a sequence of appends.
func concat(parts ...[]byte) []byte {
	total := 0
	for _, p := range parts {
		total += len(p)
	}

	out := make([]byte, 0, total)
	for _, p := range parts {
		out = append(out, p...)
	}

	return out
}

// ifInfoMsg encodes struct ifinfomsg, the header every RTM_*LINK message
// carries.
//
// ifi_family is always AF_UNSPEC: it is vestigial for link messages, which the
// kernel dispatches on the message type rather than on the address family.
func ifInfoMsg(index int32, flags, change uint32) []byte {
	buf := make([]byte, ifInfoMsgLen)

	buf[0] = unix.AF_UNSPEC
	// buf[1] is __ifi_pad, and buf[2:4] is ifi_type, both left zero.
	order.PutUint32(buf[4:8], uint32(index))
	order.PutUint32(buf[8:12], flags)
	order.PutUint32(buf[12:16], change)

	return buf
}

// ifAddrMsg encodes struct ifaddrmsg, the header of an RTM_*ADDR message.
func ifAddrMsg(family, prefixLen, flags, scope uint8, index int32) []byte {
	buf := make([]byte, ifAddrMsgLen)

	buf[0] = family
	buf[1] = prefixLen
	buf[2] = flags
	buf[3] = scope
	order.PutUint32(buf[4:8], uint32(index))

	return buf
}

// rtMsg encodes struct rtmsg, the header of an RTM_*ROUTE message.
func rtMsg(family, dstLen, table, protocol, scope, rtType uint8, flags uint32) []byte {
	buf := make([]byte, rtMsgLen)

	buf[0] = family
	buf[1] = dstLen
	// buf[2] is rtm_src_len and buf[3] is rtm_tos, both zero for what Forge does.
	buf[4] = table
	buf[5] = protocol
	buf[6] = scope
	buf[7] = rtType
	order.PutUint32(buf[8:12], flags)

	return buf
}

// nlMessage encodes a complete netlink message: the header, then the
// family-specific body.
//
// The length in the header covers the whole message including padding, which is
// the one place netlink counts padding and the attribute encoding does not.
func nlMessage(msgType, flags uint16, seq uint32, body []byte) []byte {
	length := nlMsgHdrLen + len(body)
	buf := make([]byte, nlAlign(length))

	order.PutUint32(buf[0:4], uint32(length))
	order.PutUint16(buf[4:6], msgType)
	order.PutUint16(buf[6:8], flags)
	order.PutUint32(buf[8:12], seq)
	// buf[12:16] is nlmsg_pid, left zero so the kernel fills in the port ID.
	copy(buf[nlMsgHdrLen:], body)

	return buf
}

// --- netlink transport ------------------------------------------------------

// nlConn is a netlink socket.
//
// It is not safe for concurrent use and is not meant to be: every operation
// here opens one, performs a handful of request/ACK exchanges, and closes it.
// A long-lived shared socket would need sequence-number demultiplexing to no
// benefit, since none of this is on a hot path.
type nlConn struct {
	fd  int
	seq uint32
}

// dialNetlink opens a netlink socket for one of the kernel's netlink families —
// NETLINK_ROUTE for links, addresses and routes, NETLINK_NETFILTER for NAT.
//
// The socket belongs to the network namespace of the calling thread at the
// moment it is opened, which is what makes the same code work unchanged for the
// host side and for the container configuring itself.
func dialNetlink(protocol int) (*nlConn, error) {
	fd, err := unix.Socket(unix.AF_NETLINK, unix.SOCK_RAW|unix.SOCK_CLOEXEC, protocol)
	if err != nil {
		return nil, fmt.Errorf("opening a netlink socket: %w", translate(err))
	}

	if err := unix.Bind(fd, &unix.SockaddrNetlink{Family: unix.AF_NETLINK}); err != nil {
		return nil, errors.Join(
			fmt.Errorf("binding the netlink socket: %w", translate(err)),
			closeSocket(fd),
		)
	}

	// Every request Forge sends carries NLM_F_ACK, so the kernel answers
	// immediately or not at all; there is no long-running operation here to
	// wait out. Without a deadline, a reply that never comes — a protocol
	// mistake on Forge's side, since the kernel does not simply forget — is an
	// unkillable `forge run` blocked in recvmsg with no diagnostic. With one it
	// is an error naming the operation that hung.
	timeout := unix.NsecToTimeval(int64(netlinkReplyTimeout))
	if err := unix.SetsockoptTimeval(fd, unix.SOL_SOCKET, unix.SO_RCVTIMEO, &timeout); err != nil {
		return nil, errors.Join(
			fmt.Errorf("setting the netlink receive timeout: %w", translate(err)),
			closeSocket(fd),
		)
	}

	return &nlConn{fd: fd}, nil
}

// netlinkReplyTimeout bounds the wait for a single netlink reply. It is
// generous by two orders of magnitude — an acknowledgement is prepared as the
// request is processed — because its job is to turn a hang into an error, not
// to police latency.
const netlinkReplyTimeout = 10 * time.Second

// close releases the socket.
func (c *nlConn) close() error { return closeSocket(c.fd) }

// closeSocket releases a netlink descriptor, named separately so the paths in
// dialNetlink that abandon a half-configured socket report a failure to let go
// of it the same way every other caller does.
func closeSocket(fd int) error {
	if err := unix.Close(fd); err != nil {
		return fmt.Errorf("closing the netlink socket: %w", err)
	}
	return nil
}

// execute sends one request and waits for the kernel to acknowledge it.
//
// NLM_F_ACK is always set: without it a successful request is silent, and
// "silent" and "the kernel has not got to it yet" are indistinguishable — which
// is precisely the ambiguity that makes networking code flaky.
func (c *nlConn) execute(msgType, flags uint16, body []byte) error {
	c.seq++
	seq := c.seq

	msg := nlMessage(msgType, flags|unix.NLM_F_REQUEST|unix.NLM_F_ACK, seq, body)

	return c.send(msg, seq, seq)
}

// sendRaw sends an already-encoded sequence of messages — the form nf_tables
// needs, where a transaction is several messages written at once and the
// acknowledgements cover a range of sequence numbers rather than one.
func (c *nlConn) sendRaw(msg []byte, first, last uint32) error {
	return c.send(msg, first, last)
}

// send writes a message and consumes replies until the whole sequence range has
// been accounted for.
func (c *nlConn) send(msg []byte, first, last uint32) error {
	addr := &unix.SockaddrNetlink{Family: unix.AF_NETLINK}
	for {
		if err := unix.Sendto(c.fd, msg, 0, addr); err != nil {
			if errors.Is(err, unix.EINTR) {
				continue
			}
			return fmt.Errorf("sending a netlink message: %w", translate(err))
		}
		break
	}

	return c.readAck(first, last)
}

// readAck consumes netlink replies until the acknowledgement for last arrives.
//
// The range matters for nf_tables, where one write carries a whole transaction.
// A failure is reported against the message that caused it, which may be any of
// them — so an error anywhere in [first, last] fails the call, and only seeing
// a clean acknowledgement for last counts as success. Waiting for last alone
// would silently discard the error that explains why the batch did nothing.
//
// A netlink read can return several messages in one datagram, and replies for
// other sequences may be interleaved, so this walks every message rather than
// assuming the first one is the answer.
func (c *nlConn) readAck(first, last uint32) error {
	buf := make([]byte, 16384)

	for {
		n, _, err := unix.Recvfrom(c.fd, buf, 0)
		if err != nil {
			if errors.Is(err, unix.EINTR) {
				continue
			}
			// The deadline set in dialNetlink. Reaching it means Forge asked
			// for an acknowledgement the kernel was never going to send, which
			// is a bug in what Forge sent rather than a slow machine.
			if errors.Is(err, unix.EAGAIN) || errors.Is(err, unix.EWOULDBLOCK) {
				return fmt.Errorf("timed out after %s waiting for the netlink acknowledgement of message %d",
					netlinkReplyTimeout, last)
			}
			return fmt.Errorf("reading the netlink reply: %w", translate(err))
		}

		data := buf[:n]
		for len(data) >= nlMsgHdrLen {
			msgLen := int(order.Uint32(data[0:4]))
			msgType := order.Uint16(data[4:6])
			msgSeq := order.Uint32(data[8:12])

			if msgLen < nlMsgHdrLen || msgLen > len(data) {
				return errors.New("netlink returned a truncated message")
			}
			payload := data[nlMsgHdrLen:msgLen]

			if msgSeq >= first && msgSeq <= last {
				switch msgType {
				case unix.NLMSG_ERROR:
					if len(payload) < 4 {
						return errors.New("netlink returned a short error message")
					}
					// The kernel reports a negative errno, and zero is the
					// acknowledgement of success rather than an error.
					code := int32(order.Uint32(payload[0:4]))
					if code != 0 {
						return translate(unix.Errno(-code))
					}
					if msgSeq == last {
						return nil
					}
				case unix.NLMSG_DONE:
					if msgSeq == last {
						return nil
					}
				}
			}

			advance := nlAlign(msgLen)
			if advance > len(data) {
				break
			}
			data = data[advance:]
		}
	}
}

// --- generic link operations ------------------------------------------------
//
// These are shared by veth.go, bridge.go and namespace.go. They are here rather
// than in one of those because they belong to none of them: setting a link up
// is the same operation whether the link is a bridge, a veth, or loopback.

// linkIndex returns the kernel's index for a named interface.
//
// net.InterfaceByName is the standard library asking the same netlink socket
// the same question, so there is nothing to be gained by hand-rolling a
// RTM_GETLINK here.
func linkIndex(name string) (int32, error) {
	iface, err := net.InterfaceByName(name)
	if err != nil {
		return 0, fmt.Errorf("%w: no interface named %q: %w", ErrNotFound, name, err)
	}
	return int32(iface.Index), nil
}

// linkExists reports whether an interface is present. It is the liveness check
// the lease reclaimer uses, so it must never report true for an interface that
// is gone.
func linkExists(name string) bool {
	_, err := net.InterfaceByName(name)
	return err == nil
}

// setLinkUp brings an interface up.
func setLinkUp(c *nlConn, index int32) error {
	body := ifInfoMsg(index, unix.IFF_UP, unix.IFF_UP)
	if err := c.execute(unix.RTM_NEWLINK, 0, body); err != nil {
		return fmt.Errorf("bringing interface %d up: %w", index, err)
	}
	return nil
}

// setLinkMTU sets an interface's MTU.
func setLinkMTU(c *nlConn, index int32, mtu int) error {
	body := concat(
		ifInfoMsg(index, 0, 0),
		nlAttrU32(unix.IFLA_MTU, uint32(mtu)),
	)
	if err := c.execute(unix.RTM_NEWLINK, 0, body); err != nil {
		return fmt.Errorf("setting the MTU of interface %d to %d: %w", index, mtu, err)
	}
	return nil
}

// setLinkMaster enslaves an interface to a bridge.
func setLinkMaster(c *nlConn, index, master int32) error {
	body := concat(
		ifInfoMsg(index, 0, 0),
		nlAttrU32(unix.IFLA_MASTER, uint32(master)),
	)
	if err := c.execute(unix.RTM_NEWLINK, 0, body); err != nil {
		return fmt.Errorf("attaching interface %d to bridge %d: %w", index, master, err)
	}
	return nil
}

// renameLink changes an interface's name. The kernel requires the interface to
// be down, which is why the container renames its interface before configuring
// it rather than after.
func renameLink(c *nlConn, index int32, name string) error {
	body := concat(
		ifInfoMsg(index, 0, 0),
		nlAttrString(unix.IFLA_IFNAME, name),
	)
	if err := c.execute(unix.RTM_NEWLINK, 0, body); err != nil {
		return fmt.Errorf("renaming interface %d to %q: %w", index, name, err)
	}
	return nil
}

// deleteLink removes an interface. Deleting either end of a veth pair removes
// both, which is why teardown only ever names the host end.
func deleteLink(c *nlConn, index int32) error {
	body := ifInfoMsg(index, 0, 0)
	if err := c.execute(unix.RTM_DELLINK, 0, body); err != nil {
		return fmt.Errorf("deleting interface %d: %w", index, err)
	}
	return nil
}

// addAddress assigns an IPv4 address to an interface.
//
// Both IFA_LOCAL and IFA_ADDRESS are sent. On a point-to-point link they differ,
// and on a broadcast link the kernel wants them equal — sending only one is the
// classic way to end up with an address the routing table does not believe in.
func addAddress(c *nlConn, index int32, cidr string) error {
	addr, err := parseCIDR(cidr)
	if err != nil {
		return fmt.Errorf("%w: %w", ErrInvalidInterface, err)
	}

	raw := u32ToIP(addr.base).To4()
	body := concat(
		ifAddrMsg(unix.AF_INET, uint8(addr.prefix), 0, unix.RT_SCOPE_UNIVERSE, index),
		nlAttr(unix.IFA_LOCAL, raw),
		nlAttr(unix.IFA_ADDRESS, raw),
	)

	if err := c.execute(unix.RTM_NEWADDR, unix.NLM_F_CREATE|unix.NLM_F_EXCL, body); err != nil {
		return fmt.Errorf("assigning %s to interface %d: %w", cidr, index, err)
	}

	return nil
}

// addDefaultRoute installs a default route through gateway, out of index.
//
// A destination prefix length of zero with no RTA_DST is how netlink spells
// "default"; RTA_OIF pins it to the container's interface so the route cannot
// be satisfied by anything else that happens to be present.
func addDefaultRoute(c *nlConn, index int32, gateway string) error {
	gw, err := parseIPv4(gateway)
	if err != nil {
		return fmt.Errorf("%w: %w", ErrInvalidInterface, err)
	}

	body := concat(
		rtMsg(unix.AF_INET, 0, unix.RT_TABLE_MAIN, unix.RTPROT_BOOT, unix.RT_SCOPE_UNIVERSE, unix.RTN_UNICAST, 0),
		nlAttr(unix.RTA_GATEWAY, u32ToIP(gw).To4()),
		nlAttrU32(unix.RTA_OIF, uint32(index)),
	)

	if err := c.execute(unix.RTM_NEWROUTE, unix.NLM_F_CREATE|unix.NLM_F_EXCL, body); err != nil {
		return fmt.Errorf("adding a default route via %s: %w", gateway, err)
	}

	return nil
}

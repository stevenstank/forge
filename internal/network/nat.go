package network

import (
	"errors"
	"fmt"
	"os"

	"golang.org/x/sys/unix"
)

// NAT (FR-4.4).
//
// Containers hold addresses from a private range that nothing outside the host
// knows how to route back to, so their traffic has to leave wearing the host's
// address. That is a masquerade rule, and Forge installs it over netlink into
// nf_tables rather than by shelling out to iptables (ADR-0017): nf_tables is a
// netlink protocol, so this reuses the socket machinery in network.go, and
// Forge does not acquire a runtime dependency on a binary whose legacy/nft
// split it would then have to reason about.
//
// The rule Forge installs, in nft's own syntax:
//
//	table ip forge {
//	    chain postrouting {
//	        type nat hook postrouting priority srcnat;
//	        ip saddr 10.99.0.0/16 oifname != "forge0" masquerade
//	    }
//	}
//
// The oifname exclusion is what stops container-to-container traffic across the
// bridge being masqueraded: those packets never leave the host, and rewriting
// their source would make containers unable to tell each other apart.
//
// # A host-level interaction worth knowing about
//
// On a machine already running Docker, the kernel's FORWARD chain is left with
// a DROP policy. Forge's rule lives in Forge's own table and cannot undo that:
// netfilter drops a packet if *any* chain drops it, so an accept in one table
// does not override a drop in another. Containers will masquerade correctly and
// still not route. That is a property of the host's firewall rather than
// something this package can fix, and it is called out here rather than
// discovered later.

// nftables protocol constants, from linux/netfilter/nf_tables.h and
// linux/netfilter/nfnetlink.h.
//
// They are spelled out rather than taken from x/sys/unix because that package's
// nf_tables coverage is partial and has changed between releases; a fixed local
// definition is one line each and cannot break someone else's build.
const (
	nfnlSubsysNFTables = 10

	nfnlMsgBatchBegin = 16
	nfnlMsgBatchEnd   = 17

	nftMsgNewTable = 0
	nftMsgNewChain = 3
	nftMsgNewRule  = 6
	nftMsgDelRule  = 8

	// nfProtoIPv4 is NFPROTO_IPV4. Masquerading is an IPv4 concept here;
	// Forge does not do IPv6 yet.
	nfProtoIPv4 = 2

	// nfnetlinkV0 is the only nfnetlink version there has ever been.
	nfnetlinkV0 = 0

	// nfInetPostRouting is NF_INET_POST_ROUTING: the last hook before a packet
	// leaves, and the only place source NAT can be done.
	nfInetPostRouting = 4

	// nfIPPriNATSrc is NF_IP_PRI_NAT_SRC, the conventional priority for a
	// source-NAT chain.
	nfIPPriNATSrc = 100
)

// nftables attribute types.
const (
	nftaTableName = 1

	nftaChainTable = 1
	nftaChainName  = 3
	nftaChainHook  = 4
	nftaChainType  = 7

	nftaHookHooknum  = 1
	nftaHookPriority = 2

	nftaRuleTable       = 1
	nftaRuleChain       = 2
	nftaRuleExpressions = 4

	nftaListElem = 1

	nftaExprName = 1
	nftaExprData = 2

	nftaDataValue = 1

	nftaPayloadDReg   = 1
	nftaPayloadBase   = 2
	nftaPayloadOffset = 3
	nftaPayloadLen    = 4

	nftaBitwiseSReg = 1
	nftaBitwiseDReg = 2
	nftaBitwiseLen  = 3
	nftaBitwiseMask = 4
	nftaBitwiseXor  = 5

	nftaCmpSReg = 1
	nftaCmpOp   = 2
	nftaCmpData = 3

	nftaMetaDReg = 2
	nftaMetaKey  = 1
)

// nftables expression values.
const (
	// nftRegister1 is NFT_REG_1, the first of the general-purpose registers an
	// expression loads into and the next one compares.
	nftRegister1 = 1

	// nftPayloadNetworkHeader selects the IP header as the base for a payload
	// load, so an offset of 12 is the source address.
	nftPayloadNetworkHeader = 1

	// ipv4SrcOffset and ipv4AddrLen locate the source address in an IPv4
	// header: 12 bytes in, 4 bytes long.
	ipv4SrcOffset = 12
	ipv4AddrLen   = 4

	nftCmpEq  = 0
	nftCmpNeq = 1

	// nftMetaOIFName is NFT_META_OIFNAME, the outbound interface's name.
	nftMetaOIFName = 12

	// ifNameSize is IFNAMSIZ. A meta oifname comparison is against a fixed-size
	// NUL-padded buffer, not against a trimmed string.
	ifNameSize = 16
)

// Forge's own table and chain. They are named rather than derived so that an
// operator can find them: `nft list table ip forge`.
const (
	natTableName = "forge"
	natChainName = "postrouting"
	natChainType = "nat"
)

// ipForwardPath is the sysctl that lets the kernel route between interfaces.
// Without it, masquerading is irrelevant: packets from the container are never
// forwarded far enough to be rewritten.
const ipForwardPath = "/proc/sys/net/ipv4/ip_forward"

// ensureNAT turns on forwarding and installs Forge's masquerade rule.
//
// Both halves are idempotent. Forwarding is only written when it is off, so a
// host that has it on for its own reasons is not written to at all; the rule
// set is applied as a single nf_tables transaction that flushes Forge's chain
// before refilling it, so running it a thousand times leaves exactly one rule.
func (m *Manager) ensureNAT() error {
	if err := enableIPForward(); err != nil {
		return err
	}

	if err := m.applyNATRules(); err != nil {
		return err
	}

	return nil
}

// enableIPForward turns on IPv4 forwarding if it is not already on.
//
// It reads before writing so that Forge does not take ownership of a global
// setting it did not change. Nothing turns it back off: it is host-wide, other
// things may now depend on it, and a container exiting is not a reason to break
// them. That is the same reasoning that leaves the bridge in place.
func enableIPForward() error {
	current, err := os.ReadFile(ipForwardPath)
	if err != nil {
		return fmt.Errorf("%w: reading %s: %w", ErrNATUnavailable, ipForwardPath, translate(err))
	}
	if len(current) > 0 && current[0] == '1' {
		return nil
	}

	if err := os.WriteFile(ipForwardPath, []byte("1"), 0o644); err != nil {
		return fmt.Errorf("%w: enabling IPv4 forwarding via %s: %w",
			ErrNATUnavailable, ipForwardPath, translate(err))
	}

	return nil
}

// nfgenMsg encodes struct nfgenmsg, the header every nf_tables message carries.
//
// resID is big-endian even on a little-endian host: nfnetlink fixes the byte
// order of this field regardless of the machine, which is exactly the sort of
// detail that produces an EINVAL with no explanation.
func nfgenMsg(family, version uint8, resID uint16) []byte {
	buf := make([]byte, 4)

	buf[0] = family
	buf[1] = version
	buf[2] = byte(resID >> 8)
	buf[3] = byte(resID)

	return buf
}

// nftMsgType folds an nf_tables message number into the netlink message type,
// which is the subsystem in the high byte and the message in the low one.
func nftMsgType(msg uint16) uint16 { return nfnlSubsysNFTables<<8 | msg }

// natTableMessage builds the body of an NFT_MSG_NEWTABLE.
func natTableMessage() []byte {
	return concat(
		nfgenMsg(nfProtoIPv4, nfnetlinkV0, 0),
		nlAttrString(nftaTableName, natTableName),
	)
}

// natChainMessage builds the body of an NFT_MSG_NEWCHAIN for a base chain
// hooked into postrouting as a NAT chain.
//
// A chain with a hook and a type is a "base chain": one the kernel calls
// directly for every packet reaching that hook, as opposed to one that only
// runs when jumped to.
func natChainMessage() []byte {
	return concat(
		nfgenMsg(nfProtoIPv4, nfnetlinkV0, 0),
		nlAttrString(nftaChainTable, natTableName),
		nlAttrString(nftaChainName, natChainName),
		nlNested(nftaChainHook,
			nlAttrU32BE(nftaHookHooknum, nfInetPostRouting),
			nlAttrU32BE(nftaHookPriority, nfIPPriNATSrc),
		),
		nlAttrString(nftaChainType, natChainType),
	)
}

// natFlushMessage builds the body of an NFT_MSG_DELRULE naming a chain but no
// rule, which the kernel reads as "delete every rule in this chain".
//
// This is what makes the transaction idempotent. Rules, unlike tables and
// chains, are appended rather than reconciled, so without a flush every call to
// Ensure would add another copy of the same masquerade rule.
func natFlushMessage() []byte {
	return concat(
		nfgenMsg(nfProtoIPv4, nfnetlinkV0, 0),
		nlAttrString(nftaRuleTable, natTableName),
		nlAttrString(nftaRuleChain, natChainName),
	)
}

// nftExpr wraps one expression as a list element: its name, then its data.
func nftExpr(name string, data ...[]byte) []byte {
	return nlNested(nftaListElem,
		nlAttrString(nftaExprName, name),
		nlNested(nftaExprData, data...),
	)
}

// natRuleMessage builds the body of an NFT_MSG_NEWRULE for
//
//	ip saddr <subnet> oifname != <bridge> masquerade
//
// The six expressions are a small program the kernel runs per packet:
//
//  1. payload  — load 4 bytes at offset 12 of the IP header (the source
//     address) into register 1.
//  2. bitwise  — mask register 1 with the subnet's netmask, in place.
//  3. cmp      — is it equal to the subnet's network address? If not, stop.
//  4. meta     — load the outbound interface's name into register 1.
//  5. cmp      — is it *not* the bridge? If it is the bridge, stop: that is
//     container-to-container traffic and must not be rewritten.
//  6. masq     — rewrite the source to whatever the outbound interface has.
func (m *Manager) natRuleMessage() []byte {
	mask := make([]byte, ipv4AddrLen)
	putBE32(mask, m.subnet.mask())

	network := make([]byte, ipv4AddrLen)
	putBE32(network, m.subnet.base)

	zero := make([]byte, ipv4AddrLen)

	oifname := make([]byte, ifNameSize)
	copy(oifname, m.bridge)

	expressions := concat(
		nftExpr("payload",
			nlAttrU32BE(nftaPayloadDReg, nftRegister1),
			nlAttrU32BE(nftaPayloadBase, nftPayloadNetworkHeader),
			nlAttrU32BE(nftaPayloadOffset, ipv4SrcOffset),
			nlAttrU32BE(nftaPayloadLen, ipv4AddrLen),
		),
		nftExpr("bitwise",
			nlAttrU32BE(nftaBitwiseSReg, nftRegister1),
			nlAttrU32BE(nftaBitwiseDReg, nftRegister1),
			nlAttrU32BE(nftaBitwiseLen, ipv4AddrLen),
			nlNested(nftaBitwiseMask, nlAttr(nftaDataValue, mask)),
			nlNested(nftaBitwiseXor, nlAttr(nftaDataValue, zero)),
		),
		nftExpr("cmp",
			nlAttrU32BE(nftaCmpSReg, nftRegister1),
			nlAttrU32BE(nftaCmpOp, nftCmpEq),
			nlNested(nftaCmpData, nlAttr(nftaDataValue, network)),
		),
		nftExpr("meta",
			nlAttrU32BE(nftaMetaKey, nftMetaOIFName),
			nlAttrU32BE(nftaMetaDReg, nftRegister1),
		),
		nftExpr("cmp",
			nlAttrU32BE(nftaCmpSReg, nftRegister1),
			nlAttrU32BE(nftaCmpOp, nftCmpNeq),
			nlNested(nftaCmpData, nlAttr(nftaDataValue, oifname)),
		),
		nftExpr("masq"),
	)

	return concat(
		nfgenMsg(nfProtoIPv4, nfnetlinkV0, 0),
		nlAttrString(nftaRuleTable, natTableName),
		nlAttrString(nftaRuleChain, natChainName),
		nlNested(nftaRuleExpressions, expressions),
	)
}

// putBE32 writes a uint32 big-endian, which is how addresses and masks travel
// inside nf_tables data attributes regardless of the host's byte order.
func putBE32(b []byte, v uint32) {
	b[0] = byte(v >> 24)
	b[1] = byte(v >> 16)
	b[2] = byte(v >> 8)
	b[3] = byte(v)
}

// natBatch is Forge's nf_tables transaction, encoded and ready to send.
//
// The three sequence numbers are separate because the kernel does not
// acknowledge every message in a batch, and getting that wrong does not produce
// a wrong rule — it produces a wait that never ends.
type natBatch struct {
	// wire is every message, concatenated.
	wire []byte

	// first is the sequence number of the batch begin.
	first uint32

	// acked is the sequence number of the last message the kernel will
	// acknowledge, which is what a caller must wait for.
	acked uint32

	// last is the sequence number of the batch end, which is *not*
	// acknowledged. It exists only so the connection can resume numbering
	// after the batch.
	last uint32
}

// buildNATBatch encodes the whole rule set as one nf_tables transaction,
// numbering the messages from startSeq+1.
//
// It is separated from the send for the reason every other builder in this
// package is: a netlink message is a byte layout, so it can be asserted as
// data. Here that seam earns its place twice over, because the sequence
// arithmetic below is not checkable any other way short of a kernel.
//
// # Which message the kernel answers
//
// A batch is bracketed by NFNL_MSG_BATCH_BEGIN and NFNL_MSG_BATCH_END, and
// neither bracket is acknowledged. nfnetlink_rcv_batch consumes the begin to
// enter batch mode, and on reaching the end it jumps straight to the commit,
// skipping the path that would queue an acknowledgement for it. What comes back
// on success is one ACK per *inner* message that asked for one — here the
// table, the chain, the flush and the rule.
//
// So the last thing the kernel will ever say about this batch is the ACK for
// the rule, not for the batch end. A caller that waited for the end's sequence
// number would sit in recvmsg forever on every batch that succeeded, while a
// batch that failed returned promptly with its error: the exact inversion that
// makes such a bug survive every negative test.
func (m *Manager) buildNATBatch(startSeq uint32) natBatch {
	messages := []struct {
		typ  uint16
		body []byte
	}{
		{nfnlMsgBatchBegin, nfgenMsg(unix.AF_UNSPEC, nfnetlinkV0, nfnlSubsysNFTables)},
		{nftMsgType(nftMsgNewTable), natTableMessage()},
		{nftMsgType(nftMsgNewChain), natChainMessage()},
		{nftMsgType(nftMsgDelRule), natFlushMessage()},
		{nftMsgType(nftMsgNewRule), m.natRuleMessage()},
		{nfnlMsgBatchEnd, nfgenMsg(unix.AF_UNSPEC, nfnetlinkV0, nfnlSubsysNFTables)},
	}

	batch := natBatch{first: startSeq + 1}

	seq := startSeq
	for _, msg := range messages {
		seq++

		flags := uint16(unix.NLM_F_REQUEST | unix.NLM_F_ACK)
		// Creating a table or a chain that is already there must be a success,
		// so these carry NLM_F_CREATE without NLM_F_EXCL.
		if msg.typ == nftMsgType(nftMsgNewTable) || msg.typ == nftMsgType(nftMsgNewChain) {
			flags |= unix.NLM_F_CREATE
		}
		if msg.typ == nftMsgType(nftMsgNewRule) {
			flags |= unix.NLM_F_CREATE | unix.NLM_F_APPEND
		}

		// The brackets are the two the kernel will not answer.
		if msg.typ != nfnlMsgBatchBegin && msg.typ != nfnlMsgBatchEnd {
			batch.acked = seq
		}

		batch.wire = append(batch.wire, nlMessage(msg.typ, flags, seq, msg.body)...)
	}
	batch.last = seq

	return batch
}

// applyNATRules sends the whole rule set as one nf_tables transaction.
//
// nf_tables is transactional: everything between a batch begin and a batch end
// is applied atomically or not at all. That matters here because the flush and
// the rule that replaces it are in the same batch — there is no instant at
// which Forge's chain exists with no masquerade rule in it, so a container
// starting while another is starting never sees a half-built rule set.
func (m *Manager) applyNATRules() error {
	conn, err := dialNetlink(unix.NETLINK_NETFILTER)
	if err != nil {
		return fmt.Errorf("%w: %w", ErrNATUnavailable, err)
	}
	defer func() {
		if err := conn.close(); err != nil && m.logger != nil {
			m.logger.Warn("closing the netfilter socket after installing NAT rules", "error", err)
		}
	}()

	batch := m.buildNATBatch(conn.seq)
	conn.seq = batch.last

	// A failure is reported against whichever message caused it, so the whole
	// range is watched; success is the acknowledgement of the last message that
	// gets one, which is not the batch end. See buildNATBatch.
	if err := conn.sendRaw(batch.wire, batch.first, batch.acked); err != nil {
		// A kernel with no nf_tables support rejects the batch outright rather
		// than failing one message, and saying so is far more useful than
		// reporting a protocol error nobody can act on.
		if errors.Is(err, unix.EOPNOTSUPP) || errors.Is(err, unix.EPROTONOSUPPORT) {
			return fmt.Errorf("%w: this kernel does not support nf_tables: %w", ErrNATUnavailable, err)
		}
		return fmt.Errorf("%w: installing the masquerade rule for %s: %w",
			ErrNATUnavailable, m.subnet, err)
	}

	return nil
}

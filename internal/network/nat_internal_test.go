package network

import (
	"bytes"
	"io"
	"log/slog"
	"testing"

	"github.com/stevenstank/forge/internal/logging"
)

// nf_tables is the part of Stage 4 with the least margin for error: a rule that
// encodes wrongly is rejected with EINVAL and no indication of which of its
// twenty-odd attributes was at fault. These tests walk the encoded structure so
// that a mistake shows up as a named assertion rather than as a container with
// no internet.

func natManager(t *testing.T, bridge, subnet string) *Manager {
	t.Helper()

	m, err := New(logging.New(io.Discard, slog.LevelError), Config{
		Bridge:   bridge,
		Subnet:   subnet,
		StateDir: t.TempDir(),
	})
	if err != nil {
		t.Fatalf("New() = %v", err)
	}

	return m
}

// nfgenmsg's res_id is big-endian regardless of the host, which is the sort of
// detail that silently works on one machine and fails on another.
func TestNfgenMsgLayout(t *testing.T) {
	t.Parallel()

	body := nfgenMsg(nfProtoIPv4, nfnetlinkV0, nfnlSubsysNFTables)

	if len(body) != 4 {
		t.Fatalf("length = %d, want 4", len(body))
	}
	if body[0] != nfProtoIPv4 {
		t.Errorf("family = %d, want NFPROTO_IPV4 (%d)", body[0], nfProtoIPv4)
	}
	if body[1] != nfnetlinkV0 {
		t.Errorf("version = %d, want %d", body[1], nfnetlinkV0)
	}
	if got := uint16(body[2])<<8 | uint16(body[3]); got != nfnlSubsysNFTables {
		t.Errorf("res_id decoded big-endian = %d, want %d", got, nfnlSubsysNFTables)
	}
}

func TestNftMsgType(t *testing.T) {
	t.Parallel()

	// The subsystem lives in the high byte and the message in the low one.
	if got := nftMsgType(nftMsgNewRule); got != nfnlSubsysNFTables<<8|nftMsgNewRule {
		t.Errorf("nftMsgType(NEWRULE) = %#x, want %#x", got, nfnlSubsysNFTables<<8|nftMsgNewRule)
	}
	if got := nftMsgType(nftMsgNewTable) >> 8; got != nfnlSubsysNFTables {
		t.Errorf("subsystem = %d, want %d", got, nfnlSubsysNFTables)
	}
}

func TestNatTableMessage(t *testing.T) {
	t.Parallel()

	attrs := parseAttrs(t, natTableMessage()[4:])

	if got := str(find(t, attrs, nftaTableName)); got != natTableName {
		t.Errorf("table name = %q, want %q", got, natTableName)
	}
}

// A chain with a hook and a type is a base chain — one the kernel runs for every
// packet at that hook. Without either attribute it is a chain nothing ever
// jumps to, and the rule inside it never executes.
func TestNatChainMessageIsABaseChain(t *testing.T) {
	t.Parallel()

	attrs := parseAttrs(t, natChainMessage()[4:])

	if got := str(find(t, attrs, nftaChainTable)); got != natTableName {
		t.Errorf("chain table = %q, want %q", got, natTableName)
	}
	if got := str(find(t, attrs, nftaChainName)); got != natChainName {
		t.Errorf("chain name = %q, want %q", got, natChainName)
	}
	if got := str(find(t, attrs, nftaChainType)); got != natChainType {
		t.Errorf("chain type = %q, want %q", got, natChainType)
	}

	hook := parseAttrs(t, find(t, attrs, nftaChainHook))
	if got := beUint32(find(t, hook, nftaHookHooknum)); got != nfInetPostRouting {
		t.Errorf("hook = %d, want NF_INET_POST_ROUTING (%d)", got, nfInetPostRouting)
	}
	if got := beUint32(find(t, hook, nftaHookPriority)); got != nfIPPriNATSrc {
		t.Errorf("priority = %d, want NF_IP_PRI_NAT_SRC (%d)", got, nfIPPriNATSrc)
	}
}

// The flush message names a table and a chain but no rule, which is what the
// kernel reads as "every rule in this chain". Adding a rule handle here would
// turn an idempotent apply into one that deletes the wrong thing.
func TestNatFlushMessageNamesNoRule(t *testing.T) {
	t.Parallel()

	attrs := parseAttrs(t, natFlushMessage()[4:])

	if got := str(find(t, attrs, nftaRuleTable)); got != natTableName {
		t.Errorf("table = %q, want %q", got, natTableName)
	}
	if got := str(find(t, attrs, nftaRuleChain)); got != natChainName {
		t.Errorf("chain = %q, want %q", got, natChainName)
	}
	for _, a := range attrs {
		if a.typ == nftaRuleExpressions {
			t.Error("the flush message carries expressions; it must name no rule")
		}
	}
}

// The rule is a six-instruction program. This walks all six in order, because
// the ordering is semantic: the register loaded by one expression is the
// register compared by the next.
func TestNatRuleMessageExpressions(t *testing.T) {
	t.Parallel()

	m := natManager(t, "forge0", "10.99.0.0/16")
	attrs := parseAttrs(t, m.natRuleMessage()[4:])

	if got := str(find(t, attrs, nftaRuleTable)); got != natTableName {
		t.Errorf("table = %q, want %q", got, natTableName)
	}

	elems := parseAttrs(t, find(t, attrs, nftaRuleExpressions))
	if len(elems) != 6 {
		t.Fatalf("got %d expressions, want 6", len(elems))
	}

	names := make([]string, 0, len(elems))
	datas := make([][]byte, 0, len(elems))
	for _, elem := range elems {
		if elem.typ != nftaListElem {
			t.Fatalf("expression is attribute type %d, want NFTA_LIST_ELEM", elem.typ)
		}
		inner := parseAttrs(t, elem.data)
		names = append(names, str(find(t, inner, nftaExprName)))
		datas = append(datas, find(t, inner, nftaExprData))
	}

	want := []string{"payload", "bitwise", "cmp", "meta", "cmp", "masq"}
	for i, name := range want {
		if names[i] != name {
			t.Fatalf("expression %d is %q, want %q (order is semantic)", i, names[i], name)
		}
	}

	// 1. payload: load the IPv4 source address into register 1.
	payload := parseAttrs(t, datas[0])
	if got := beUint32(find(t, payload, nftaPayloadBase)); got != nftPayloadNetworkHeader {
		t.Errorf("payload base = %d, want the network header (%d)", got, nftPayloadNetworkHeader)
	}
	if got := beUint32(find(t, payload, nftaPayloadOffset)); got != ipv4SrcOffset {
		t.Errorf("payload offset = %d, want %d (the IPv4 source address)", got, ipv4SrcOffset)
	}
	if got := beUint32(find(t, payload, nftaPayloadLen)); got != ipv4AddrLen {
		t.Errorf("payload length = %d, want %d", got, ipv4AddrLen)
	}
	if got := beUint32(find(t, payload, nftaPayloadDReg)); got != nftRegister1 {
		t.Errorf("payload dreg = %d, want register 1", got)
	}

	// 2. bitwise: mask it with the subnet's netmask, in place.
	bitwise := parseAttrs(t, datas[1])
	if got := beUint32(find(t, bitwise, nftaBitwiseSReg)); got != nftRegister1 {
		t.Errorf("bitwise sreg = %d, want the register payload loaded", got)
	}
	mask := parseAttrs(t, find(t, bitwise, nftaBitwiseMask))
	if got := find(t, mask, nftaDataValue); !bytes.Equal(got, []byte{0xff, 0xff, 0x00, 0x00}) {
		t.Errorf("mask = %v, want the /16 netmask 255.255.0.0", got)
	}

	// 3. cmp: equal to the subnet's network address.
	cmpSubnet := parseAttrs(t, datas[2])
	if got := beUint32(find(t, cmpSubnet, nftaCmpOp)); got != nftCmpEq {
		t.Errorf("subnet comparison op = %d, want NFT_CMP_EQ", got)
	}
	subnetData := parseAttrs(t, find(t, cmpSubnet, nftaCmpData))
	if got := find(t, subnetData, nftaDataValue); !bytes.Equal(got, []byte{10, 99, 0, 0}) {
		t.Errorf("compared against %v, want 10.99.0.0", got)
	}

	// 4. meta: load the outbound interface name.
	meta := parseAttrs(t, datas[3])
	if got := beUint32(find(t, meta, nftaMetaKey)); got != nftMetaOIFName {
		t.Errorf("meta key = %d, want NFT_META_OIFNAME (%d)", got, nftMetaOIFName)
	}

	// 5. cmp: not equal to the bridge. This is what stops container-to-container
	//    traffic across the bridge being masqueraded.
	cmpBridge := parseAttrs(t, datas[4])
	if got := beUint32(find(t, cmpBridge, nftaCmpOp)); got != nftCmpNeq {
		t.Errorf("bridge comparison op = %d, want NFT_CMP_NEQ; equality would masquerade exactly the traffic that must not be", got)
	}
	bridgeData := parseAttrs(t, find(t, cmpBridge, nftaCmpData))
	name := find(t, bridgeData, nftaDataValue)
	if len(name) != ifNameSize {
		t.Errorf("oifname comparison is %d bytes, want a %d-byte padded buffer", len(name), ifNameSize)
	}
	if got := str(name); got != "forge0" {
		t.Errorf("oifname compared against %q, want %q", got, "forge0")
	}

	// 6. masq takes no attributes.
	if len(datas[5]) != 0 {
		t.Errorf("masq carries %d bytes of data, want none", len(datas[5]))
	}
}

// The rule must follow the configured subnet and bridge, not the defaults, or a
// custom -subnet would masquerade nothing.
func TestNatRuleMessageFollowsConfig(t *testing.T) {
	t.Parallel()

	m := natManager(t, "br-test", "192.168.50.0/24")
	attrs := parseAttrs(t, m.natRuleMessage()[4:])
	elems := parseAttrs(t, find(t, attrs, nftaRuleExpressions))

	bitwise := parseAttrs(t, parseAttrs(t, elems[1].data)[1].data)
	mask := parseAttrs(t, find(t, bitwise, nftaBitwiseMask))
	if got := find(t, mask, nftaDataValue); !bytes.Equal(got, []byte{0xff, 0xff, 0xff, 0x00}) {
		t.Errorf("mask = %v, want the /24 netmask", got)
	}

	cmpSubnet := parseAttrs(t, parseAttrs(t, elems[2].data)[1].data)
	subnetData := parseAttrs(t, find(t, cmpSubnet, nftaCmpData))
	if got := find(t, subnetData, nftaDataValue); !bytes.Equal(got, []byte{192, 168, 50, 0}) {
		t.Errorf("compared against %v, want 192.168.50.0", got)
	}

	cmpBridge := parseAttrs(t, parseAttrs(t, elems[4].data)[1].data)
	bridgeData := parseAttrs(t, find(t, cmpBridge, nftaCmpData))
	if got := str(find(t, bridgeData, nftaDataValue)); got != "br-test" {
		t.Errorf("oifname compared against %q, want %q", got, "br-test")
	}
}

func TestPutBE32(t *testing.T) {
	t.Parallel()

	buf := make([]byte, 4)
	putBE32(buf, 0x0A630000) // 10.99.0.0

	if !bytes.Equal(buf, []byte{0x0A, 0x63, 0x00, 0x00}) {
		t.Errorf("putBE32() = %v, want big-endian 10.99.0.0", buf)
	}
}

// The batch must be a transaction: begin, the changes, end. A rule applied
// outside one is not atomic, and the flush that precedes it would then be
// briefly visible as a chain with no masquerade rule in it.
func TestNATBatchIsWellFormed(t *testing.T) {
	t.Parallel()

	m := natManager(t, "forge0", "10.99.0.0/16")

	want := []uint16{
		nfnlMsgBatchBegin,
		nftMsgType(nftMsgNewTable),
		nftMsgType(nftMsgNewChain),
		nftMsgType(nftMsgDelRule),
		nftMsgType(nftMsgNewRule),
		nfnlMsgBatchEnd,
	}

	// The production builder, not a copy of it: a batch this test assembled
	// itself could not tell us anything about the one Forge sends.
	batch := m.buildNATBatch(0)

	// Walk it back out the way the kernel would.
	type decoded struct {
		typ uint16
		seq uint32
	}
	var seen []decoded
	rest := batch.wire
	for len(rest) >= nlMsgHdrLen {
		length := int(order.Uint32(rest[0:4]))
		if length < nlMsgHdrLen || nlAlign(length) > len(rest) {
			t.Fatalf("message claims length %d in a %d-byte remainder", length, len(rest))
		}
		seen = append(seen, decoded{typ: order.Uint16(rest[4:6]), seq: order.Uint32(rest[8:12])})
		rest = rest[nlAlign(length):]
	}

	if len(seen) != len(want) {
		t.Fatalf("decoded %d messages, want %d", len(seen), len(want))
	}
	for i, msg := range seen {
		if msg.typ != want[i] {
			t.Errorf("message %d is type %d, want %d", i, msg.typ, want[i])
		}
		if msg.seq != uint32(i+1) {
			t.Errorf("message %d has sequence %d, want %d", i, msg.seq, i+1)
		}
	}
}

// TestNATBatchWaitsForTheLastAcknowledgedMessage pins the sequence number a
// caller blocks on, which is the one thing about this batch that no amount of
// inspecting the bytes would reveal.
//
// The kernel acknowledges the messages *inside* a batch and neither bracket:
// NFNL_MSG_BATCH_END is consumed by the commit path, which never queues an
// acknowledgement for it. Waiting for the end's sequence number therefore hangs
// forever on every batch that succeeds — while a batch that fails still returns
// its error promptly, so no failure test would ever catch it.
func TestNATBatchWaitsForTheLastAcknowledgedMessage(t *testing.T) {
	t.Parallel()

	m := natManager(t, "forge0", "10.99.0.0/16")

	const start = 7
	batch := m.buildNATBatch(start)

	if batch.first != start+1 {
		t.Errorf("first = %d, want %d: numbering continues from the connection", batch.first, start+1)
	}
	if batch.last != start+6 {
		t.Errorf("last = %d, want %d: the batch is six messages", batch.last, start+6)
	}

	// The rule is the fifth message and the last one the kernel answers.
	wantAcked := start + 5
	if batch.acked != uint32(wantAcked) {
		t.Errorf("acked = %d, want %d (the rule, the last message the kernel acknowledges)",
			batch.acked, wantAcked)
	}
	if batch.acked == batch.last {
		t.Errorf("acked = %d is the batch end; waiting for it would block forever on success", batch.acked)
	}

	// And the message at that sequence number really is the rule, so the
	// arithmetic cannot drift if the batch gains a message.
	if typ := messageTypeAt(t, batch.wire, batch.acked-batch.first); typ != nftMsgType(nftMsgNewRule) {
		t.Errorf("the last acknowledged message is type %d, want NFT_MSG_NEWRULE (%d)",
			typ, nftMsgType(nftMsgNewRule))
	}
}

// messageTypeAt returns the type of the nth message in an encoded sequence.
func messageTypeAt(t *testing.T, wire []byte, n uint32) uint16 {
	t.Helper()

	rest := wire
	for i := uint32(0); len(rest) >= nlMsgHdrLen; i++ {
		length := int(order.Uint32(rest[0:4]))
		if length < nlMsgHdrLen || nlAlign(length) > len(rest) {
			t.Fatalf("message %d claims length %d in a %d-byte remainder", i, length, len(rest))
		}
		if i == n {
			return order.Uint16(rest[4:6])
		}
		rest = rest[nlAlign(length):]
	}

	t.Fatalf("no message at index %d", n)
	return 0
}

// beUint32 decodes a big-endian attribute payload.
func beUint32(b []byte) uint32 {
	if len(b) < 4 {
		return 0
	}
	return uint32(b[0])<<24 | uint32(b[1])<<16 | uint32(b[2])<<8 | uint32(b[3])
}

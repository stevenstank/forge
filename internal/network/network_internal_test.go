package network

import (
	"bytes"
	"testing"

	"golang.org/x/sys/unix"
)

// The netlink encoders are the half of this package a unit test can reach: a
// message is a byte layout, so what the kernel will be told is checkable
// without a kernel. These tests decode what the builders produce and assert the
// structure, rather than hard-coding a byte string — the layout is the contract,
// and the byte order it is written in is the host's.

// parsedAttr is one decoded netlink attribute.
type parsedAttr struct {
	typ  uint16
	data []byte
}

// parseAttrs walks a sequence of netlink attributes. It is the inverse of
// nlAttr, and having it lets every builder be tested by round trip.
func parseAttrs(t *testing.T, b []byte) []parsedAttr {
	t.Helper()

	var out []parsedAttr
	for len(b) >= nlAttrHdrLen {
		length := int(order.Uint16(b[0:2]))
		typ := order.Uint16(b[2:4]) &^ nlaFNested

		if length < nlAttrHdrLen || length > len(b) {
			t.Fatalf("attribute claims length %d in a %d-byte buffer", length, len(b))
		}
		out = append(out, parsedAttr{typ: typ, data: b[nlAttrHdrLen:length]})

		advance := nlAlign(length)
		if advance > len(b) {
			break
		}
		b = b[advance:]
	}

	return out
}

// find returns the first attribute of a type, failing the test if it is absent.
func find(t *testing.T, attrs []parsedAttr, typ uint16) []byte {
	t.Helper()

	for _, a := range attrs {
		if a.typ == typ {
			return a.data
		}
	}
	t.Fatalf("no attribute of type %d in %d attributes", typ, len(attrs))

	return nil
}

// str decodes a NUL-terminated attribute payload.
func str(b []byte) string { return string(bytes.TrimRight(b, "\x00")) }

func TestNLAlign(t *testing.T) {
	t.Parallel()

	tests := []struct {
		in, want int
	}{
		{0, 0}, {1, 4}, {2, 4}, {3, 4}, {4, 4},
		{5, 8}, {7, 8}, {8, 8}, {9, 12}, {16, 16},
	}

	for _, tt := range tests {
		if got := nlAlign(tt.in); got != tt.want {
			t.Errorf("nlAlign(%d) = %d, want %d", tt.in, got, tt.want)
		}
	}
}

// The length field counts the header and the payload but not the padding, which
// is the single easiest thing to get wrong in netlink and produces an EINVAL
// with no explanation when it is.
func TestNLAttrLengthExcludesPadding(t *testing.T) {
	t.Parallel()

	// A 5-byte payload: header (4) + payload (5) = 9, padded to 12.
	attr := nlAttr(7, []byte{1, 2, 3, 4, 5})

	if len(attr) != 12 {
		t.Errorf("encoded length = %d, want 12 (padded)", len(attr))
	}
	if got := order.Uint16(attr[0:2]); got != 9 {
		t.Errorf("length field = %d, want 9 (header + payload, no padding)", got)
	}
	if got := order.Uint16(attr[2:4]); got != 7 {
		t.Errorf("type field = %d, want 7", got)
	}
	if !bytes.Equal(attr[4:9], []byte{1, 2, 3, 4, 5}) {
		t.Errorf("payload = %v, want 1..5", attr[4:9])
	}
	if !bytes.Equal(attr[9:12], []byte{0, 0, 0}) {
		t.Errorf("padding = %v, want zeroes", attr[9:12])
	}
}

func TestNLAttrString(t *testing.T) {
	t.Parallel()

	attr := nlAttrString(unix.IFLA_IFNAME, "forge0")

	// "forge0" is 6 bytes plus a NUL, so the payload is 7 and the length is 11.
	if got := order.Uint16(attr[0:2]); got != 11 {
		t.Errorf("length = %d, want 11 (4 + 6 + NUL)", got)
	}
	if attr[10] != 0 {
		t.Errorf("string attribute is not NUL-terminated: %v", attr)
	}
	if got := str(attr[4:11]); got != "forge0" {
		t.Errorf("payload = %q, want %q", got, "forge0")
	}
}

func TestNLAttrU32(t *testing.T) {
	t.Parallel()

	attr := nlAttrU32(unix.IFLA_MTU, 1500)
	if len(attr) != 8 {
		t.Fatalf("encoded length = %d, want 8", len(attr))
	}
	if got := order.Uint32(attr[4:8]); got != 1500 {
		t.Errorf("value = %d, want 1500", got)
	}

	// nf_tables wants several of its fields big-endian regardless of the host,
	// so the two encoders must genuinely differ.
	be := nlAttrU32BE(unix.IFLA_MTU, 1500)
	if got := uint32(be[4])<<24 | uint32(be[5])<<16 | uint32(be[6])<<8 | uint32(be[7]); got != 1500 {
		t.Errorf("big-endian value decoded as %d, want 1500", got)
	}
}

// NLA_F_NESTED must be set on nested attributes, because nf_tables rejects them
// without it — and must not be set on the rtnetlink attributes that predate the
// flag and are matched by exact type.
func TestNestedFlagIsSetOnlyWhereItBelongs(t *testing.T) {
	t.Parallel()

	nested := nlNested(4, nlAttrU32(1, 9))
	if got := order.Uint16(nested[2:4]); got&nlaFNested == 0 {
		t.Errorf("nlNested type = %#x, want NLA_F_NESTED set", got)
	}

	plain := nlNestedPlain(unix.IFLA_LINKINFO, nlAttrString(unix.IFLA_INFO_KIND, "veth"))
	if got := order.Uint16(plain[2:4]); got&nlaFNested != 0 {
		t.Errorf("nlNestedPlain type = %#x, want NLA_F_NESTED clear", got)
	}
	if got := order.Uint16(plain[2:4]); got != unix.IFLA_LINKINFO {
		t.Errorf("nlNestedPlain type = %d, want IFLA_LINKINFO (%d)", got, unix.IFLA_LINKINFO)
	}
}

func TestNestedRoundTrip(t *testing.T) {
	t.Parallel()

	encoded := nlNested(10,
		nlAttrString(1, "kind"),
		nlAttrU32(2, 42),
	)

	outer := parseAttrs(t, encoded)
	if len(outer) != 1 {
		t.Fatalf("got %d outer attributes, want 1", len(outer))
	}

	inner := parseAttrs(t, outer[0].data)
	if len(inner) != 2 {
		t.Fatalf("got %d inner attributes, want 2", len(inner))
	}
	if got := str(inner[0].data); got != "kind" {
		t.Errorf("first inner = %q, want %q", got, "kind")
	}
	if got := order.Uint32(inner[1].data); got != 42 {
		t.Errorf("second inner = %d, want 42", got)
	}
}

func TestIfInfoMsgLayout(t *testing.T) {
	t.Parallel()

	body := ifInfoMsg(unix.AF_UNSPEC, 7, unix.IFF_UP, unix.IFF_UP)

	if len(body) != ifInfoMsgLen {
		t.Fatalf("length = %d, want %d", len(body), ifInfoMsgLen)
	}
	if body[0] != unix.AF_UNSPEC {
		t.Errorf("family = %d, want AF_UNSPEC", body[0])
	}
	if got := int32(order.Uint32(body[4:8])); got != 7 {
		t.Errorf("index = %d, want 7", got)
	}
	if got := order.Uint32(body[8:12]); got != unix.IFF_UP {
		t.Errorf("flags = %#x, want IFF_UP", got)
	}
	if got := order.Uint32(body[12:16]); got != unix.IFF_UP {
		t.Errorf("change = %#x, want IFF_UP; a flag set without a change mask is ignored", got)
	}
}

func TestIfAddrMsgLayout(t *testing.T) {
	t.Parallel()

	body := ifAddrMsg(unix.AF_INET, 16, 0, unix.RT_SCOPE_UNIVERSE, 3)

	if len(body) != ifAddrMsgLen {
		t.Fatalf("length = %d, want %d", len(body), ifAddrMsgLen)
	}
	if body[0] != unix.AF_INET {
		t.Errorf("family = %d, want AF_INET", body[0])
	}
	if body[1] != 16 {
		t.Errorf("prefix length = %d, want 16", body[1])
	}
	if got := order.Uint32(body[4:8]); got != 3 {
		t.Errorf("index = %d, want 3", got)
	}
}

func TestRtMsgLayout(t *testing.T) {
	t.Parallel()

	body := rtMsg(unix.AF_INET, 0, unix.RT_TABLE_MAIN, unix.RTPROT_BOOT,
		unix.RT_SCOPE_UNIVERSE, unix.RTN_UNICAST, 0)

	if len(body) != rtMsgLen {
		t.Fatalf("length = %d, want %d", len(body), rtMsgLen)
	}
	if body[0] != unix.AF_INET {
		t.Errorf("family = %d, want AF_INET", body[0])
	}
	// A destination length of zero with no RTA_DST is how netlink spells
	// "default route"; anything else here would install a host route.
	if body[1] != 0 {
		t.Errorf("dst_len = %d, want 0 for a default route", body[1])
	}
	if body[4] != unix.RT_TABLE_MAIN {
		t.Errorf("table = %d, want RT_TABLE_MAIN", body[4])
	}
	if body[7] != unix.RTN_UNICAST {
		t.Errorf("type = %d, want RTN_UNICAST", body[7])
	}
}

// Unlike an attribute, a netlink message header's length *does* include
// padding, and mixing the two conventions up is a silent corruption.
func TestNLMessageLengthIncludesBody(t *testing.T) {
	t.Parallel()

	body := ifInfoMsg(unix.AF_UNSPEC, 1, 0, 0)
	msg := nlMessage(unix.RTM_NEWLINK, unix.NLM_F_REQUEST, 99, body)

	wantLen := nlMsgHdrLen + len(body)
	if got := int(order.Uint32(msg[0:4])); got != wantLen {
		t.Errorf("length = %d, want %d", got, wantLen)
	}
	if got := order.Uint16(msg[4:6]); got != unix.RTM_NEWLINK {
		t.Errorf("type = %d, want RTM_NEWLINK", got)
	}
	if got := order.Uint16(msg[6:8]); got != unix.NLM_F_REQUEST {
		t.Errorf("flags = %#x, want NLM_F_REQUEST", got)
	}
	if got := order.Uint32(msg[8:12]); got != 99 {
		t.Errorf("sequence = %d, want 99", got)
	}
	if got := order.Uint32(msg[12:16]); got != 0 {
		t.Errorf("port id = %d, want 0 so the kernel fills it in", got)
	}
}

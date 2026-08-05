package network

import (
	"errors"
	"strings"
	"testing"

	"golang.org/x/sys/unix"
)

func TestVethNames(t *testing.T) {
	t.Parallel()

	host, peer, err := VethNames("abc123def456")
	if err != nil {
		t.Fatalf("VethNames() = %v", err)
	}
	if host != "fhabc123def456" {
		t.Errorf("host = %q, want %q", host, "fhabc123def456")
	}
	if peer != "fcabc123def456" {
		t.Errorf("peer = %q, want %q", peer, "fcabc123def456")
	}
	if host == peer {
		t.Error("both ends of the veth pair got the same name")
	}
}

// A 12-character container ID (SSOT §8) must leave the derived names inside
// IFNAMSIZ. This is the assertion that stops a future ID-length change from
// producing an EINVAL nobody can explain.
func TestVethNamesFitTheKernelsLimit(t *testing.T) {
	t.Parallel()

	const idLen = 12
	host, peer, err := VethNames(strings.Repeat("a", idLen))
	if err != nil {
		t.Fatalf("VethNames() = %v", err)
	}

	for name, got := range map[string]string{"host": host, "peer": peer} {
		if len(got) > ifNameMaxLen {
			t.Errorf("%s name %q is %d characters; the kernel allows %d",
				name, got, len(got), ifNameMaxLen)
		}
	}
}

func TestVethNamesRejectsBadIDs(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		id      string
		wantErr error
	}{
		{name: "empty", id: "", wantErr: ErrInvalidContainerID},
		{name: "path separator", id: "../escape", wantErr: ErrInvalidContainerID},
		{name: "backslash", id: `a\b`, wantErr: ErrInvalidContainerID},
		{name: "dot dot", id: "..", wantErr: ErrInvalidContainerID},
		{name: "NUL byte", id: "a\x00b", wantErr: ErrInvalidContainerID},
		{name: "too long for an interface name", id: strings.Repeat("a", 32), wantErr: ErrInvalidContainerID},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if _, _, err := VethNames(tt.id); !errors.Is(err, tt.wantErr) {
				t.Errorf("VethNames(%q) = %v, want %v", tt.id, err, tt.wantErr)
			}
		})
	}
}

func TestValidateIfaceName(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		in   string
		ok   bool
	}{
		{name: "ordinary", in: "forge0", ok: true},
		{name: "eth0", in: "eth0", ok: true},
		{name: "exactly at the limit", in: strings.Repeat("a", ifNameMaxLen), ok: true},
		{name: "one past the limit", in: strings.Repeat("a", ifNameMaxLen+1)},
		{name: "empty"},
		{name: "slash", in: "a/b"},
		{name: "space", in: "a b"},
		{name: "tab", in: "a\tb"},
		{name: "dot", in: "."},
		{name: "dot dot", in: ".."},
		{name: "NUL", in: "a\x00"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			err := validateIfaceName(tt.in)
			if tt.ok && err != nil {
				t.Errorf("validateIfaceName(%q) = %v, want nil", tt.in, err)
			}
			if !tt.ok && !errors.Is(err, ErrInvalidInterface) {
				t.Errorf("validateIfaceName(%q) = %v, want %v", tt.in, err, ErrInvalidInterface)
			}
		})
	}
}

// The veth creation message is the most intricate thing Forge encodes: three
// levels of nesting with the peer's own ifinfomsg buried in the innermost one.
// Getting it wrong yields EINVAL and nothing else, so the structure is walked
// here rather than trusted.
func TestVethCreateMessageStructure(t *testing.T) {
	t.Parallel()

	body := vethCreateMessage("fhabc", "fcabc")

	if len(body) < ifInfoMsgLen {
		t.Fatalf("message is %d bytes, shorter than an ifinfomsg", len(body))
	}
	attrs := parseAttrs(t, body[ifInfoMsgLen:])

	if got := str(find(t, attrs, unix.IFLA_IFNAME)); got != "fhabc" {
		t.Errorf("IFLA_IFNAME = %q, want the host end %q", got, "fhabc")
	}

	linkInfo := parseAttrs(t, find(t, attrs, unix.IFLA_LINKINFO))
	if got := str(find(t, linkInfo, unix.IFLA_INFO_KIND)); got != "veth" {
		t.Errorf("IFLA_INFO_KIND = %q, want %q", got, "veth")
	}

	infoData := parseAttrs(t, find(t, linkInfo, unix.IFLA_INFO_DATA))
	peerBlock := find(t, infoData, vethInfoPeer)

	// The peer block opens with its own ifinfomsg, and the peer's name follows
	// it. Skipping that header is the step most easily forgotten.
	if len(peerBlock) < ifInfoMsgLen {
		t.Fatalf("peer block is %d bytes, too short for an ifinfomsg", len(peerBlock))
	}
	peerAttrs := parseAttrs(t, peerBlock[ifInfoMsgLen:])
	if got := str(find(t, peerAttrs, unix.IFLA_IFNAME)); got != "fcabc" {
		t.Errorf("peer IFLA_IFNAME = %q, want %q", got, "fcabc")
	}
}

func TestBridgeCreateMessageStructure(t *testing.T) {
	t.Parallel()

	body := bridgeCreateMessage("forge0")
	attrs := parseAttrs(t, body[ifInfoMsgLen:])

	if got := str(find(t, attrs, unix.IFLA_IFNAME)); got != "forge0" {
		t.Errorf("IFLA_IFNAME = %q, want %q", got, "forge0")
	}

	linkInfo := parseAttrs(t, find(t, attrs, unix.IFLA_LINKINFO))
	if got := str(find(t, linkInfo, unix.IFLA_INFO_KIND)); got != "bridge" {
		t.Errorf("IFLA_INFO_KIND = %q, want %q", got, "bridge")
	}
}

func TestCreateVethPairRejectsBadNames(t *testing.T) {
	t.Parallel()

	// No socket is opened: the names are checked before anything is dialled,
	// so this runs unprivileged and proves the check comes first.
	if err := createVethPair(nil, strings.Repeat("a", 40), "peer"); !errors.Is(err, ErrInvalidInterface) {
		t.Errorf("createVethPair() with an over-long host name = %v, want %v", err, ErrInvalidInterface)
	}
	if err := createVethPair(nil, "host", ""); !errors.Is(err, ErrInvalidInterface) {
		t.Errorf("createVethPair() with an empty peer name = %v, want %v", err, ErrInvalidInterface)
	}
}

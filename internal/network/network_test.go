package network_test

import (
	"errors"
	"testing"

	"github.com/stevenstank/forge/internal/network"
)

func TestModeValidate(t *testing.T) {
	t.Parallel()

	for _, mode := range []network.Mode{network.ModeBridge, network.ModeNone, network.ModeHost} {
		if err := mode.Validate(); err != nil {
			t.Errorf("Mode(%q).Validate() = %v, want nil", mode, err)
		}
	}

	for _, mode := range []network.Mode{"", "brige", "container", "HOST"} {
		if err := network.Mode(mode).Validate(); err == nil {
			t.Errorf("Mode(%q).Validate() = nil, want an error", mode)
		}
	}
}

// The two questions the orchestrator asks a mode. Getting either wrong changes
// what clone(2) is asked for, so they are pinned rather than left implicit.
func TestModeCapabilities(t *testing.T) {
	t.Parallel()

	tests := []struct {
		mode       network.Mode
		wantNetns  bool
		wantVeth   bool
		descriptio string
	}{
		{network.ModeBridge, true, true, "a full network"},
		{network.ModeNone, true, false, "isolation without connectivity"},
		{network.ModeHost, false, false, "Stage 1-3 behaviour"},
	}

	for _, tt := range tests {
		t.Run(string(tt.mode), func(t *testing.T) {
			t.Parallel()

			if got := tt.mode.NeedsNetns(); got != tt.wantNetns {
				t.Errorf("NeedsNetns() = %v, want %v (%s)", got, tt.wantNetns, tt.descriptio)
			}
			if got := tt.mode.NeedsVeth(); got != tt.wantVeth {
				t.Errorf("NeedsVeth() = %v, want %v (%s)", got, tt.wantVeth, tt.descriptio)
			}
		})
	}
}

func TestInterfaceValidate(t *testing.T) {
	t.Parallel()

	valid := network.Interface{
		Source:  "fcabc123",
		Name:    "eth0",
		Address: "10.99.0.2/16",
		Gateway: "10.99.0.1",
		MTU:     1500,
	}

	if err := valid.Validate(); err != nil {
		t.Fatalf("a valid interface did not validate: %v", err)
	}

	tests := []struct {
		name   string
		mutate func(network.Interface) network.Interface
	}{
		{"no source", func(i network.Interface) network.Interface { i.Source = ""; return i }},
		{"no name", func(i network.Interface) network.Interface { i.Name = ""; return i }},
		{"no address", func(i network.Interface) network.Interface { i.Address = ""; return i }},
		{"address without a prefix", func(i network.Interface) network.Interface { i.Address = "10.99.0.2"; return i }},
		{"address that is not an address", func(i network.Interface) network.Interface { i.Address = "banana/16"; return i }},
		{"gateway that is not an address", func(i network.Interface) network.Interface { i.Gateway = "banana"; return i }},
		{"negative mtu", func(i network.Interface) network.Interface { i.MTU = -1; return i }},
		{"source name too long", func(i network.Interface) network.Interface {
			i.Source = "aaaaaaaaaaaaaaaaaaaa"
			return i
		}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if err := tt.mutate(valid).Validate(); !errors.Is(err, network.ErrInvalidInterface) {
				t.Errorf("Validate() = %v, want %v", err, network.ErrInvalidInterface)
			}
		})
	}
}

// A container on an isolated segment is a legitimate thing to ask for, so an
// interface with no gateway must validate.
func TestInterfaceWithoutAGatewayIsValid(t *testing.T) {
	t.Parallel()

	iface := network.Interface{
		Source:  "fcabc123",
		Name:    "eth0",
		Address: "10.99.0.2/16",
	}

	if err := iface.Validate(); err != nil {
		t.Errorf("Validate() = %v, want nil", err)
	}
}

func TestVethNamesAreDistinctAndDerived(t *testing.T) {
	t.Parallel()

	// Derivation is what lets a lease be verified by something other than the
	// process that wrote it, so the same ID must always give the same names.
	firstHost, firstPeer, err := network.VethNames("abc123def456")
	if err != nil {
		t.Fatalf("VethNames() = %v", err)
	}
	secondHost, secondPeer, err := network.VethNames("abc123def456")
	if err != nil {
		t.Fatalf("VethNames() = %v", err)
	}

	if firstHost != secondHost || firstPeer != secondPeer {
		t.Error("VethNames() is not deterministic")
	}

	otherHost, _, err := network.VethNames("999999999999")
	if err != nil {
		t.Fatalf("VethNames() = %v", err)
	}
	if otherHost == firstHost {
		t.Error("two container IDs produced the same host interface name")
	}
}

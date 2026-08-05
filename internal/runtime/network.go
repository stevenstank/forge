package runtime

import (
	"fmt"
	"log/slog"

	"github.com/stevenstank/forge/internal/network"
)

// The Stage 4 half of the orchestration: deciding whether a container gets a
// network, and making sure the one it gets is released.
//
// internal/network performs networking operations and decides nothing; the
// policy below — which mode implies which resources, when the bridge is
// prepared, where the teardown is registered, and what crosses the re-exec
// boundary — is cross-package sequencing and so lives here (SSOT §2, §13.2).
// This mirrors limits.go, which does the same job for cgroups.
//
// # The two halves, and why they are apart
//
// A container's networking is acquired in two steps on either side of clone(2):
//
//	prepareNetwork   before   the bridge, and an address reserved for the
//	                          container out of the pool. Neither needs the
//	                          container to exist.
//	attachNetwork    after    the veth pair, its peer pushed into the
//	                          container's namespace, its host end plugged into
//	                          the bridge. All three need a namespace, and a
//	                          namespace needs a process.
//
// The cleanup for *both* is registered by the first, the moment the address is
// claimed. That is what makes a failure anywhere in the second — a veth that
// could not be created, a namespace the peer could not be moved into, a bridge
// that vanished between Ensure and Attach — leak nothing: Destroy is idempotent
// and releases whatever of the pair actually exists (SSOT §11.3, §13.3).

// containerNetwork is what prepareNetwork hands the rest of a run.
//
// It carries the mode as well as the allocation because "no allocation" has two
// meanings that must not be confused: a container in host mode has no namespace
// of its own and must be left entirely alone, while a container in none mode
// has one and still needs its loopback brought up.
type containerNetwork struct {
	mode  network.Mode
	alloc *network.Network
}

// iface returns the description the container configures on itself, or nil for
// a container with no interface to configure. It is what crosses the re-exec
// boundary in the init payload (ADR-0018).
func (c containerNetwork) iface() *network.Interface {
	if c.alloc == nil {
		return nil
	}
	return &c.alloc.Interface
}

// prepareNetwork readies the host side of the container's networking and
// registers its teardown (FR-4.2, FR-4.3, FR-4.5).
//
// It returns before the container exists, so it does everything that does not
// need one: the bridge, the NAT rule, and the address. The veth pair is
// attachNetwork's job.
//
// Unlike prepareCgroup there is no degradation rule here. A container that
// cannot be given the network it asked for must not start: silently falling
// back to the host's namespace would hand a container the host's addresses and
// its loopback, which is the one failure mode a caller can neither see nor
// recover from.
func (r *Runner) prepareNetwork(
	log *slog.Logger,
	id string,
	spec Spec,
	cleanup *cleanupStack,
) (containerNetwork, error) {
	mode := spec.NetworkMode()

	// Host mode creates nothing and so has nothing to release; none mode gets
	// its namespace from a clone flag, which the kernel reclaims with the
	// container's last process. Neither reaches internal/network on the host
	// side at all.
	if !mode.NeedsVeth() {
		log.Debug("container network prepared", "mode", string(mode))
		return containerNetwork{mode: mode}, nil
	}

	// Ensure before Allocate: an address out of a subnet whose gateway is not
	// on a bridge is an address that routes nowhere. It is idempotent and
	// removes nothing, so there is no cleanup to register for it — the bridge
	// is shared by every container and outlives them all (ADR-0019).
	if err := r.networks.Ensure(); err != nil {
		return containerNetwork{}, err
	}

	alloc, err := r.networks.Allocate(id)
	if err != nil {
		return containerNetwork{}, err
	}

	// Registered immediately, before anything else can fail — including the
	// MTU line below. From here every path out of Run unwinds through this,
	// and Destroy releases both the lease taken above and the veth pair
	// attachNetwork has not created yet (SSOT §11.3).
	cleanup.push("releasing the container network", func() error {
		return r.networks.Destroy(id)
	})

	// The MTU is the caller's, so it is applied to the plan here rather than
	// inside internal/network, which is not told what a Spec is.
	alloc.Interface.MTU = spec.NetworkMTU

	log.Debug("container network prepared",
		"mode", string(mode),
		"bridge", r.networks.Bridge(),
		"subnet", r.networks.Subnet().String(),
		"address", alloc.Interface.Address,
		"gateway", alloc.Interface.Gateway,
		"host_veth", alloc.HostVeth,
		"peer_veth", alloc.PeerVeth,
	)

	return containerNetwork{mode: mode, alloc: alloc}, nil
}

// attachNetwork gives the started container its interface (FR-4.2).
//
// It runs in the handshake window, after the container has been cloned and
// before it has been told anything, so the interface is in its namespace before
// its first instruction. A container with no allocation — host or none mode —
// has nothing to attach.
func (r *Runner) attachNetwork(log *slog.Logger, cnet containerNetwork, pid int) error {
	if cnet.alloc == nil {
		return nil
	}

	if err := r.networks.Attach(cnet.alloc, pid); err != nil {
		return fmt.Errorf("attaching the container to %s: %w", r.networks.Bridge(), err)
	}

	log.Debug("container attached to the network",
		"pid", pid,
		"address", cnet.alloc.Interface.Address,
		"bridge", r.networks.Bridge(),
	)

	return nil
}

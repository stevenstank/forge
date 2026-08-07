package runtime

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/stevenstank/forge/internal/image"
)

// The Stage 5 half of the orchestration: turning an image reference into a
// populated root filesystem, and deciding what the image contributes to the
// container.
//
// internal/image speaks the Distribution Spec and writes tar members to disk,
// and decides nothing. The policy below — which platform is asked for, what an
// image config means for a Spec, and where in the run the pull happens — is
// cross-package sequencing and so lives here (SSOT §2, §13.2). This mirrors
// limits.go and network.go, which do the same job for cgroups and networking.
//
// # Why the pull happens first
//
// Steps 1 to 4 of the lifecycle touch the network and the shared blob cache and
// nothing else. No container ID has been used, no directory made, no address
// leased, and the cleanup stack is empty. A reference that does not parse, a
// registry that is down, a platform that is not published, a layer that does not
// verify — every one of them fails with the host bit-for-bit unchanged apart
// from a possibly larger cache, which is shared and which the next run will
// reuse.
//
// The alternative — prepare the container and pull into it — would mean the
// commonest failure in the stage, a typo in an image name, left a container
// directory and an IP lease behind to be unwound.

// DefaultImageRoot is where downloaded layers are cached (SSOT §9).
//
// It is a sibling of the container root rather than a child, so the two can be
// pointed at different filesystems: a machine with a small root partition wants
// the blob cache somewhere else, and images are far larger than the directories
// they are unpacked into.
const DefaultImageRoot = "/var/lib/forge/images"

// DefaultEnv is the environment a container gets when nothing else supplies
// one.
//
// It is deliberately minimal and explicit: nothing is inherited from the host.
// PATH is included because almost every program expects one to exist, and
// because from Stage 5 on it is the PATH a bare command name is resolved
// against inside the container.
//
// It is exported because it is the fallback the CLI applies to a container
// that has no image, and having two definitions of "the default environment"
// is exactly the kind of divergence SSOT §13.6 exists to prevent.
func DefaultEnv() []string {
	return []string{
		"PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin",
	}
}

// containerImage is what resolveImage hands the rest of a run: an image that
// has been resolved to a digest, verified, and cached in full.
type containerImage struct {
	ref      image.Reference
	manifest image.Manifest
	config   image.Config
}

// resolveImage performs steps 1 to 4 of the lifecycle: parse the reference,
// resolve it to a manifest, download whatever the cache is missing, and verify
// everything (FR-5.1, FR-5.2, FR-5.4).
//
// A spec with no Image returns nil, which is what keeps every Stage 1 to 4
// container working unchanged: nothing is parsed, no socket is opened, and the
// image cache directory is never even created.
//
// It creates nothing on the host that belongs to this container, so it
// registers nothing on the cleanup stack. Blobs are shared and content
// addressed; a run must never delete one it did not just prove corrupt
// (ADR-0021).
func (r *Runner) resolveImage(ctx context.Context, log *slog.Logger, spec Spec) (*containerImage, error) {
	if spec.Image == "" {
		return nil, nil
	}

	// 1. Parse the reference. Pure, and already done once by Validate — this is
	//    where the parsed form is kept rather than thrown away.
	ref, err := image.ParseReference(spec.Image)
	if err != nil {
		return nil, err
	}

	// 2. Resolve the tag to an immutable manifest, verifying it (FR-5.2).
	//
	//    Which platform is asked for is the orchestrator's decision, not
	//    internal/image's, which is why HostPlatform is a parameter there and a
	//    call here. From this point the pull is expressed entirely in digests,
	//    so a tag that moves mid-pull cannot produce a rootfs assembled from two
	//    different images.
	platform := image.HostPlatform()

	manifest, err := r.registry.Resolve(ctx, ref, platform)
	if err != nil {
		return nil, err
	}

	log.Debug("resolved image",
		"reference", ref.String(), "manifest", manifest.Digest,
		"platform", platform.String(), "layers", len(manifest.Layers))

	// 3 and 4. Download what is missing and verify every byte of it. Anything
	//          already cached is not transferred at all (FR-5.4).
	stats, err := image.Pull(ctx, r.registry, r.images, ref, manifest)
	if err != nil {
		return nil, err
	}

	log.Info("pulled image",
		"reference", ref.String(), "manifest", manifest.Digest,
		"fetched", stats.Fetched, "cached", stats.Cached, "bytes", stats.Bytes)

	// ReadAll verifies the config blob against its digest a second time, on the
	// read rather than the write. It is small enough that the check is free.
	raw, err := r.images.ReadAll(manifest.Config.Digest)
	if err != nil {
		return nil, err
	}

	config, err := image.ParseConfig(raw)
	if err != nil {
		return nil, err
	}

	if err := checkPlatform(platform, config.Platform, ref); err != nil {
		return nil, err
	}

	return &containerImage{ref: ref, manifest: manifest, config: config}, nil
}

// checkPlatform refuses an image that cannot run on this machine.
//
// The check has to happen here because it cannot happen in Resolve. A
// descriptor's platform field exists only on the entries of an index, so a tag
// that points straight at a single-platform manifest gives Resolve nothing to
// match against — and Docker Hub serves plenty of those. The image's own config
// is the only remaining statement of what it was built for, and Forge already
// downloads and parses it, so the check costs one comparison and no I/O.
//
// Without it the failure surfaces as ENOEXEC from execve inside a container
// that has already been created, networked and given a filesystem: the least
// debuggable place it could possibly appear, and after every resource the run
// will have to unwind has been acquired.
//
// A config that declares no platform at all is accepted. That is unusual rather
// than wrong, and refusing it would invent a requirement the image spec does
// not state.
func checkPlatform(host, declared image.Platform, ref image.Reference) error {
	if declared.IsZero() || host.Matches(declared) {
		return nil
	}

	return fmt.Errorf("%w: %s is built for %s and this host is %s",
		image.ErrNoMatchingPlatform, ref, declared, host)
}

// apply merges the image's configuration into the spec.
//
// One rule, and it is the one users expect from Docker: **the image supplies the
// default, the caller overrides it.** Nothing the caller asked for is ever
// discarded, and nothing the image declared is ignored unless the caller said
// otherwise.
//
//	Command     Entrypoint + Cmd; positional arguments replace Cmd and keep
//	            Entrypoint
//	Env         the image's, merged per variable with the caller's, caller
//	            winning; DefaultEnv only if neither supplies anything
//	WorkingDir  the image's, unless -workdir was given
//
// Hostname, limits and networking take nothing from the image: no field in the
// OCI config feeds them, and inventing a mapping would be Forge deciding
// something the spec does not say.
func (i *containerImage) apply(spec Spec) (Spec, error) {
	// The caller's positional arguments are an override of Cmd, not a
	// replacement for the whole command, which is why this cannot be a simple
	// "if empty, use the image's".
	command := i.config.Command(spec.Command)
	if len(command) == 0 {
		return Spec{}, fmt.Errorf("%w: %s declares neither an entrypoint nor a command, so one must be given: forge run %s <cmd>",
			ErrNoCommand, i.ref, i.ref.Original)
	}
	spec.Command = command

	if env := i.config.Environ(spec.Env); len(env) > 0 {
		spec.Env = env
	} else if len(spec.Env) == 0 {
		spec.Env = DefaultEnv()
	}

	if spec.WorkingDir == "" {
		spec.WorkingDir = i.config.WorkingDir
	}

	return spec, nil
}

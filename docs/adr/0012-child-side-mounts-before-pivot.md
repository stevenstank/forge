# 0012. All container mounts are made child-side, before `pivot_root`

Date: 2026-07-31
Status: Accepted

## Context

Two orderings had to be settled for Stage 2, and they interact.

**Where do mounts happen — in `forge`, or in the container's init?** The parent
could mount into `<root>/<id>/rootfs` before starting the container, since that
directory is a host path. Or the container's init could do it after `clone(2)`
has placed it in a new mount namespace.

**When does `pivot_root` happen — before or after the bind mounts?** Mounting
after the pivot is appealing: destinations are then plain container paths, with
no `<root>/<id>/rootfs` prefix to reason about.

FR-2.3 hangs on the first question: Forge must unmount and clean up every mount
it creates, "leaving no orphaned mounts on the host".

## Decision

**Every mount is made in the container's init, inside its mount namespace.**
The parent creates directories and nothing else.

**Every mount is made before `pivot_root`.** `mount.Apply` runs first,
addressing destinations through the host-visible `<root>/<id>/rootfs` prefix;
`mount.PivotRoot` runs second.

The init sequence is therefore:

```
namespace.Apply     mount tree made private   (FR-1.3, ADR-0008)
mount.Apply         the container's mounts
mount.PivotRoot     "/" becomes the container's root
chdir(workdir)
execve
```

### Why child-side

Because it makes FR-2.3 structurally true rather than a cleanup routine that
has to be correct. The kernel destroys a mount namespace, and every mount in
it, when the namespace's last process exits. Forge does not need to unmount
anything on the ordinary path, or on the failure path, or when the container is
`SIGKILL`ed — there is nothing on the host to unmount.

It also removes rollback from `mount.Apply` entirely. A plan that fails half
way leaves a half-built mount stack in a namespace that is about to cease to
exist, so `Apply` returns the error and the init exits 127 (PRD NFR-8).

`mount.Cleanup` still exists, for the one case the kernel cannot cover: a
`forge` process killed with `SIGKILL` leaves the container running and its
directory behind. It is a reconciliation path, and it is also what
`rootfs.Store.Remove`'s refusal to delete through a live mount points an
operator at.

### Why before the pivot

Because bind mount **sources are host paths**, and after `pivot_root` detaches
the old root there is no host filesystem left to bind from. `--mount
/srv/data:/data` cannot be performed after the pivot: `/srv/data` does not
exist any more. The same applies to the device nodes bound from the host's
`/dev`.

Mounting first also means the read-only remount of the container's root
(`--read-only`) can be the last step, after everything has been mounted into a
still-writable tree, and that `.forge-oldroot` can be created before that
remount rather than failing against a read-only filesystem.

The cost is that `mount.Apply` resolves every destination against
`<root>/<id>/rootfs` rather than `/`. That resolution is `resolveDestination`,
which Stage 2 needs regardless — a rootfs Forge did not build may contain
symlinks, and following them the way the *host* would is precisely how a bind
mount ends up writing to the host's `/etc`.

## Consequences

Easier:

- `TestNoHostMountResidueAfterRun` compares the host's mount table before and
  after a container with a full mount set and a bind, and expects it byte-for-
  byte identical. That test passes by construction rather than by diligence.
- `TestMountsDieWithTheNamespace` kills a container that mounted a tmpfs of its
  own and shows the host table unchanged.
- The parent's cleanup has exactly one job — remove a directory — so its
  failure modes are a directory's failure modes.

Harder:

- Mount errors happen inside the container, where the only channels back are
  the exit code (127) and stderr. A misconfigured mount reports less context
  than a parent-side failure would. Mitigated by validating the whole plan in
  the parent first, where a bad destination is refused with a clear message
  before anything is forked.
- Every destination is resolved twice conceptually — once as
  `<root>/<id>/rootfs/data` for the mount, once as `/data` in messages — so
  error text has to be careful to name the container path the user typed rather
  than the host path Forge computed.

## Alternatives considered

**Mount in the parent, before starting the container.** Errors surface in
`forge` with full context, and no re-exec is involved. Rejected on FR-2.3: it
puts every container's mounts in the host's mount namespace, so a crashed forge
leaves them all behind, and correctness then depends on a cleanup path running
exactly right on every failure — including the ones that kill the process.

**`pivot_root` first, then mount.** Destinations become plain container paths
and `resolveDestination` gets simpler. Rejected because it cannot satisfy
FR-2.2: the host paths a bind mount needs are gone once the old root is
detached. Keeping the old root mounted to reach them would defeat ADR-0001.

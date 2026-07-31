# 0010. Per-container rootfs layout, and how Stage 2 populates it

Date: 2026-07-31
Status: Accepted

## Context

FR-2.4 requires Forge to manage a per-container root filesystem directory under
a predictable, documented path. SSOT §9 already names the path's root: `--root`,
defaulting to `/var/lib/forge/containers`.

Two questions remain. What is the layout under that root, and — since Stage 2
has no images — where does the *content* of a container's root filesystem come
from?

Stage 5 answers the second question permanently by unpacking OCI layers. Until
then a container needs a filesystem tree from somewhere, and the only source
available is a directory the user already has: an unpacked `alpine-minirootfs`,
a debootstrap tree, or similar.

## Decision

**Layout.**

```
<root>/                     the storage root, from --root, 0700
└── <container-id>/         one container's tree, 0700
    └── rootfs/             becomes the container's "/", 0755
        └── .forge-oldroot/ created by the container's init, for pivot_root
```

The intermediate `<container-id>/` level costs nothing now and avoids a
migration later: Stage 3 needs somewhere to record a cgroup path and Stage 6
needs somewhere for state and logs, and both are siblings of `rootfs/` rather
than things that belong inside the container's filesystem.

`internal/rootfs` owns this layout and nothing else. It creates the tree,
validates a source directory, and removes the tree. It performs no mount
operation (SSOT §2).

**Population.** `forge run --rootfs <dir>` bind-mounts `<dir>` onto
`<root>/<id>/rootfs`, inside the container's mount namespace. One mount does
two jobs: it puts the tree's contents where the container's root must be, and
it makes that directory a mount point, which `pivot_root(2)` requires
(ADR-0001).

This is carried in `mount.Plan.Source`. When Stage 5 unpacks layers into
`<root>/<id>/rootfs` directly, `Source` becomes equal to `Root` and the same
code performs a self-bind that satisfies the `pivot_root` precondition and
nothing else. No structural change is needed to get there.

**Deletion is guarded.** `rootfs.Store.Remove` ends in `os.RemoveAll`. If a
bind mount into the host were still live under the tree, that walk would delete
the host's files. `Remove` therefore reads the mount table first and refuses on
`ErrStillMounted` — and refuses equally when it cannot read the table at all.
Not knowing whether something is mounted is exactly the situation in which
deleting would be unrecoverable.

## Consequences

Easier:

- FR-2.4 is satisfied with a layout that later stages extend rather than
  replace.
- Stage 2 is usable today with any unpacked rootfs tarball, so the stage can be
  demonstrated and tested without waiting for Stage 5.
- Populating by bind mount costs nothing per container. Copying a 300 MB tree
  for every `forge run` would have made the stage unpleasant to use and its
  tests slow.

Harder:

- **Containers share the source tree, and writes go back to it.** Two
  containers started from one `--rootfs` see each other's changes, and both see
  changes made on the host. This is the price of not copying, and it is the
  limitation Stage 5's layering (ADR-0003) removes. `--read-only` is the
  mitigation available now, and the README says so plainly.
- The container's root filesystem is empty on the host while nothing is
  running, which reads oddly: `<root>/<id>/rootfs` is a mount point, and its
  contents exist only inside the container's mount namespace. Correct, and
  surprising the first time.
- `Remove` can refuse. A caller that ignores the error leaks a directory; the
  runtime's cleanup logs it at WARN and carries on, which is the documented
  behaviour (SSOT §5) but does mean a crashed forge can leave an empty tree
  behind until Stage 6 adds startup reconciliation.

## Alternatives considered

**Copy the source tree per container.** Gives each container a private
filesystem immediately, with no shared-write surprise. Rejected: it makes every
`forge run` pay a full tree copy, it duplicates gigabytes across containers,
and it is throwaway work — Stage 5 replaces it with layering regardless.

**Use the source directory as the container root directly.** Simplest possible
thing: pivot straight into `--rootfs`. Rejected: it fails FR-2.4, which asks for
a *per-container* directory Forge manages, and it leaves nowhere for Stage 3's
and Stage 6's per-container state to live.

**OverlayFS now, with the source as a lower layer.** Would give copy-on-write
immediately and remove the shared-write limitation. Rejected as Stage 5's
decision to make (ADR-0003 is explicitly about layer assembly strategy);
choosing it here would pre-empt that ADR from the wrong stage.

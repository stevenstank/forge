# 0001. Use `pivot_root` instead of `chroot` for rootfs isolation

Date: 2026-07-31
Status: Accepted

## Context

FR-2.1 requires each container to have its own root filesystem, established
with `pivot_root(2)` rather than `chroot(2)`, and asks that `chroot` be
documented as insufficient. This ADR is that documentation.

`chroot(2)` changes one thing: the calling process's root *directory*. The
mount that the old root lives on is untouched — still mounted, still listed in
the process's own `/proc/self/mountinfo`, still reachable by any file
descriptor that was open across the call.

That has two consequences Forge cannot accept.

**It is escapable.** A process with `CAP_SYS_ADMIN` — which every Forge
container has, since Stage 1–6 do not implement user namespaces (PRD §5) —
walks out in a few lines:

```c
mkdir("escape", 0755);
chroot("escape");        /* root is now .../escape, cwd is still outside it */
for (i = 0; i < 1000; i++) chdir("..");   /* climbs past the new root */
chroot(".");             /* root is now the host's / */
```

The climb works because `chroot` does not move the process's working
directory, and `..` at a root the kernel does not consider *the* root keeps
resolving upward. This is not an obscure bug; it is documented behaviour, and
it is why no container runtime uses `chroot` alone.

**It leaks the host's mount table.** Even without escaping, the container's
view of what is mounted is the host's view. Every host filesystem, every other
container's rootfs, and every path on the machine is enumerable from inside.

`pivot_root(2)` instead moves the *mount*: the new root becomes the root of the
process's mount namespace, and the old root is relocated to a directory
underneath it, from where it can be unmounted. After
`umount2(put_old, MNT_DETACH)` there is no mount left that refers to the host's
tree, so there is nothing to walk back to and nothing to enumerate. `..` at `/`
resolves to `/`, as it does for any process at a real root.

`pivot_root` has preconditions `chroot` does not, and Stage 2's implementation
exists largely to satisfy them: the new root must be a mount point (hence the
bind of the source tree onto the container's root directory), `put_old` must be
under the new root (hence `.forge-oldroot`), and the current root must not have
shared propagation (hence the `MS_REC|MS_PRIVATE` remount that ADR-0008 already
put in `internal/namespace`).

## Decision

`internal/mount.PivotRoot` uses `pivot_root(2)`, then detaches and removes the
old root. Forge never calls `chroot(2)`.

The sequence, in the container's init:

```
chdir(new_root)                    hold no reference to the old root
pivot_root(new_root, put_old)      the mounts swap
chdir("/")                         now the new root
umount2("/.forge-oldroot", MNT_DETACH)
rmdir("/.forge-oldroot")           tolerating EROFS on a read-only root
```

`chroot` stays in the project as the comparison FR-2.1 asks for — in this ADR
and in the README — and not in the code.

Two integration tests hold the line: `TestHostRootCannotBeReachedByClimbingOut`
runs the escape shape above against a real container, and `TestOldRootIsDetached`
asserts the container's own mount table lists no host path.

## Consequences

Easier:

- The isolation is real rather than nominal, and the tests demonstrate it
  rather than asserting it.
- A reader meets the preconditions as code — a self-bind, a `put_old`
  directory, a propagation change — each with a comment saying which
  precondition it satisfies. That is the mechanism made visible, which is the
  project's stated purpose (PRD §2).

Harder:

- Four syscalls and a directory where `chroot` would have been one call. The
  Stage 2 implementation is meaningfully larger for it.
- `pivot_root` fails with a bare `EINVAL` for several distinct mistakes, most
  often "the new root is not a mount point". `PivotRoot` checks that case
  itself and returns `ErrRootNotMountPoint` with an explanation, because the
  kernel's error tells the reader nothing.

## Alternatives considered

**`chroot` plus dropping `CAP_SYS_CHROOT`.** Closes the specific escape above
by preventing the second `chroot`. Rejected: it leaves the mount-table leak
untouched, it depends on a capability drop Forge does not otherwise implement,
and it teaches a workaround rather than the mechanism.

**`pivot_root(".", ".")`.** The form runc uses: with the new root as the
working directory, both arguments can be `.`, and the old root is then
unmounted from `.` with `MNT_DETACH` — no `put_old` directory is needed at all,
which sidesteps the read-only-rootfs case where `rmdir` cannot clean up.
Rejected for Stage 2 on legibility grounds: it relies on the reader knowing
that `put_old` may equal `new_root` and that the old root remains the
`.`-resolved mount until detached. The explicit form says what it does. The
cost is one empty directory left behind in a read-only root, recorded as a
known limitation.

# Stage 5 — Images (design)

**Status:** Implemented — `internal/image`, its integration into
`internal/runtime` and `internal/cli`, and the integration suite.
**Requirements:** PRD §8.5 (FR-5.1 … FR-5.5)
**New packages:** `internal/image`
**ADRs:** 0003 (layer assembly strategy), 0020 (one image package), 0021
(content-addressed blob store), 0022 (positional grammar and command
resolution) — all Accepted.

`forge run alpine:3.20` works end to end. Stages 1–4 are untouched at the
mechanism level, and `-rootfs` and the bare-path form behave exactly as before;
§7.4 states the compatibility guarantees as assertions rather than claims.

`test/integration/stage5_test.go` runs against real registries: every manifest
and every layer byte in it comes from Docker Hub over the real Distribution
protocol.

---

## 0. What Stage 5 adds

```bash
sudo forge run alpine:3.20 /bin/sh
sudo forge run -memory 128m alpine:3.20 ls /etc
sudo forge run docker.io/library/busybox@sha256:<hex> /bin/sh
```

Forge resolves the reference against an OCI Distribution registry, verifies
every byte it downloads against the digest that names it, caches those bytes on
disk, unpacks the image's layers into the container's own `rootfs` directory,
and runs the container exactly as Stages 1–4 already do.

Stages 1–4 are untouched at the mechanism level. Namespaces still come from
`clone(2)`, the cgroup is still written before the container joins it, the veth
pair is still made in the handshake window, the mounts and `pivot_root` still
happen child-side. Stage 5 adds one thing to the parent's sequence: before the
container's `rootfs` directory is handed to the mount plan, its contents are
written there from an image instead of being bind-mounted from a directory the
user already had.

`-rootfs` does not go away. It is how Stages 2–4 are still exercised, how the
integration suite runs without a registry, and how a developer runs against a
tree they built by hand.

**Out of scope**, recorded so it is a decision rather than an omission:

- Authenticated pulls. Anonymous Bearer-token flow only, which is what public
  Docker Hub, GHCR, and Quay serve (PRD §11 scopes Stage 5 this way).
- Pushing, image building, `docker save`/`load` tarballs.
- zstd-compressed layers (no stdlib decompressor; refused by media type with a
  clear error), foreign/URL layers, encrypted layers.
- Signature or provenance verification (cosign, Notary). Digest verification is
  integrity, not authenticity.
- `forge images` / `forge pull` / `forge rmi` verbs and cache garbage
  collection. The cache grows; §10 says why that is acceptable for now.
- Image config fields beyond `Env`, `Cmd`, `Entrypoint`, `WorkingDir` —
  `User`, `Volumes`, `ExposedPorts`, `StopSignal` and healthchecks are Stage 6
  concerns or non-goals.
- Offline operation. A tag is resolved over the network on every run; §9 covers
  what a cached blob does and does not save.

---

## 1. Package layout

```
internal/image/
├── image.go       # package doc, sentinel errors, digest parsing and formatting
├── reference.go   # Reference, ParseReference — pure name normalization
├── registry.go    # Client, New, ClientConfig, anonymous Bearer auth, retries
├── manifest.go    # descriptors, manifests, indexes, platform choice, Resolve;
│                  #   image config parsing and command/env resolution (pure)
├── blob.go        # FetchBlob — streams one blob out, verifying as it goes; Pull
├── cache.go       # Cache, NewCache, blob paths, Has/Open/ReadAll/Verify, Staging
├── unpack.go      # UnpackLayer — tar+gzip → a directory, containment-checked
├── rootfs.go      # BuildRootfs — apply an ordered layer list to one directory
└── cleanup.go     # cleanupStack, Discard, Remove, PruneStaging — idempotent
```

The file split carries the separation a package split would have. The network
half is `registry.go`, `manifest.go` and `blob.go`; the disk half is `cache.go`,
`unpack.go`, `rootfs.go` and `cleanup.go`. They share exactly one type,
`Descriptor`, and one concept, the digest.

Within that, the same pure/impure split every Stage 1–4 package uses:
`reference.go`, the type layer and config half of `manifest.go`, the digest
arithmetic in `image.go` and the path arithmetic in `cache.go` touch nothing.
Everything else is testable against `httptest.Server` and `t.TempDir()`, which
is why §11's unit suite needs no root and no network.

**One package, not two.** SSOT §1 and §2 originally named `internal/image` and
`internal/registry` separately, with two exceptions to the leaf-package rule:
`rootfs → image` and `image → registry`. ADR-0020 retires both.

- `image → registry` disappears by construction — there is no second package.
  The alternative would have preserved an exception to SSOT §13.2 in order to
  separate two halves that share one string type, and would have forced either a
  duplicated `Descriptor` or a conversion at the seam.
- `rootfs → image` is declined. `internal/rootfs` owns the *directory*
  (`<root>/<id>/rootfs`), unchanged since Stage 2; `internal/image` owns the
  *contents*. `image` is handed a destination path and writes into it; it never
  learns what a container is, and `rootfs` never learns what a layer is.

The result is that "primitive packages never call each other" is now true
without a footnote, and `internal/runtime` is the only package that will import
`internal/image`. ADR-0020 amends SSOT §2, §3, §11.1 and §13.2 accordingly
(SSOT §16).

### Files touched elsewhere

| File | Change | Status |
|---|---|---|
| `internal/runtime/runtime.go` | `Runner.images`, `Runner.registry`, `Spec.Image`, `Config.ImageRoot`, `Config.Registry`; `Validate` gains `validateImage` and `validateCommand`; `prepareFilesystem` takes the resolved image | done |
| `internal/runtime/image.go` (new) | policy: platform choice, the pull sequence, the image config → `Spec` merge, `DefaultImageRoot`, `DefaultEnv` | done |
| `internal/runtime/init.go` | child-side `PATH` resolution for a bare command name (§7.3), and the payload guard that refuses one without a filesystem | done |
| `internal/cli/run.go` | positional grammar (`<image> [cmd]`), image sentinels in `isUserError`, usage text | done |
| `internal/cli/cli.go` | `-image-root` global flag, `Options.ImageRoot`, `DefaultImageRoot` | done |
| `test/integration/stage5_test.go` (new) | 17 tests against the real Docker Hub | done |
| `test/integration/harness_test.go` | `TestMain` releases the run-wide blob cache | done |
| `Makefile` | `-timeout 30m`, since the suite now does real network I/O | done |

**No third-party dependency.** `net/http`, `encoding/json`, `archive/tar`,
`compress/gzip`, `crypto/sha256` and `golang.org/x/sys/unix` cover all of it.
SSOT §10 pre-approves an OCI types library; Stage 5 does not need one, because
the subset of the spec Forge reads is about forty struct fields and writing them
out is worth more to a reader than an import (ADR would be required to add it;
none is required to not).

---

## 2. Public APIs

Everything below is implemented. No interfaces are defined anywhere in the
package: there is one client and one cache, and the two seams tests actually
need — an HTTP transport and a directory — are already injectable as an
`*http.Client` and a path. That is the same reasoning behind
`network.Config.StateDir` and `rootfs.Store.mountsUnder`.

### Names — pure, no network, no disk

```go
type Reference struct {
    Registry   string // "docker.io", "ghcr.io", "localhost:5000"
    Repository string // "library/alpine"
    Tag        string // "3.20", empty when Digest is set
    Digest     string // "sha256:…", empty when Tag is set
    Original   string // exactly what the user typed, for error messages
}
func ParseReference(s string) (Reference, error)
func (r Reference) Host() string     // registry-1.docker.io for docker.io
func (r Reference) IsDigest() bool
func (r Reference) Target() string   // the digest if there is one, else the tag
func (r Reference) String() string   // the fully-qualified form
```

`Registry` holds the canonical name and `Host()` resolves it to the host serving
`/v2/`. The two differ only for Docker Hub, and keeping them apart is what lets
`String()` round-trip.

### The registry client

```go
type ClientConfig struct {
    HTTPClient       *http.Client  // nil means a client with a 10-minute timeout
    UserAgent        string        // empty means "forge/<version>"
    MaxManifestBytes int64         // zero means 4 MiB
    MaxAttempts      int           // zero means 3; one disables retrying
    Backoff          time.Duration // zero means 200ms, doubling
    PlainHTTP        bool          // http:// — for a loopback or local registry
}

// New performs no I/O: no connection, no DNS lookup, no token.
func New(logger *slog.Logger, cfg ClientConfig) (*Client, error)

type Descriptor struct {
    MediaType string
    Digest    string
    Size      int64
    Platform  *Platform // set only on the entries of an index
}
type Platform struct{ OS, Architecture, Variant string }
func HostPlatform() Platform
func (p Platform) String() string

type Manifest struct {
    Digest string       // the manifest's own digest, verified on arrival
    Config Descriptor   // the image config blob
    Layers []Descriptor // in application order, base first
}

// Resolve turns a reference into a single-platform manifest (FR-5.1, FR-5.2).
func (c *Client) Resolve(ctx context.Context, ref Reference, p Platform) (Manifest, error)

// FetchBlob streams one blob into w, verifying digest and length as the bytes
// pass (FR-5.2). A mismatch is an error and w has already been written to, so
// callers write to something they are prepared to discard.
func (c *Client) FetchBlob(ctx context.Context, ref Reference, d Descriptor, w io.Writer) error
```

`Client` holds an `*http.Client`, a logger, the limits above, and a per-scope
anonymous token cache. Nothing else. It never learns a path on disk, so it
cannot create — and therefore cannot leak — a file.

### The cache

```go
// NewCache performs no I/O, not even creating the root. See §4.
func NewCache(root string, logger *slog.Logger) (*Cache, error)
func (c *Cache) Root() string

func (c *Cache) Path(digest string) (string, error)   // pure: blobs/sha256/<hex>
func (c *Cache) Has(digest string) (bool, error)      // FR-5.4's whole question
func (c *Cache) Open(digest string) (io.ReadCloser, error)
func (c *Cache) ReadAll(digest string) ([]byte, error) // verifies; small blobs only
func (c *Cache) Verify(ctx context.Context, digest string) error

// Writing. A blob becomes visible under blobs/ only complete and verified.
func (c *Cache) Stage() (*Staging, error)
func (s *Staging) Write(p []byte) (int, error)  // io.Writer, for FetchBlob
func (s *Staging) Commit(digest string) error   // verifies, then link(2)s into place
func (s *Staging) Discard() error               // idempotent
func (s *Staging) Name() string
func (s *Staging) Written() int64               // stays valid after Commit

// Hygiene, both idempotent.
func (c *Cache) Remove(digest string) error
func (c *Cache) PruneStaging(ctx context.Context, olderThan time.Duration) error
```

### Pull, unpack, build

```go
type PullStats struct{ Fetched, Cached int; Bytes int64 }

// Pull downloads every blob of a manifest the cache does not already have
// (FR-5.1, FR-5.2, FR-5.4).
func Pull(ctx context.Context, client *Client, cache *Cache, ref Reference, m Manifest) (PullStats, error)

type Stats struct {
    Files, Dirs, Symlinks, Hardlinks, Devices, Whiteouts int
    Bytes          int64
    UnownedEntries int // entries whose uid/gid could not be applied without root
}

// UnpackLayer applies one cached layer to dest, verifying the blob as it
// decompresses (FR-5.3).
func (c *Cache) UnpackLayer(ctx context.Context, digest, dest string) (Stats, error)

// BuildRootfs applies an image's layers to dest, base layer first (FR-5.3).
func BuildRootfs(ctx context.Context, cache *Cache, layers []Descriptor, dest string) (Stats, error)
```

### Image config — pure

```go
type Config struct {
    Env        []string
    Cmd        []string
    Entrypoint []string
    WorkingDir string
}
func ParseConfig(b []byte) (Config, error)
func (c Config) Command(override []string) []string // entrypoint + cmd resolution
func (c Config) Environ(override []string) []string // image env, caller wins per key
```

`User`, `Volumes`, `ExposedPorts` and `StopSignal` are not read, and the package
doc says so, because silently ignoring a field the user set is worse than not
claiming to support it.

### Sentinel errors

`ErrInvalidReference`, `ErrInvalidDigest`, `ErrDigestMismatch`, `ErrNotFound`,
`ErrUnauthorized`, `ErrRegistryUnavailable`, `ErrUnsupportedMediaType`,
`ErrNoMatchingPlatform`, `ErrManifestTooLarge`, `ErrBlobNotFound`,
`ErrCorruptLayer`, `ErrEscapesRoot`, `ErrStagingCommitted`.

## 3. Ownership boundaries

| Concern | Owner | Why there, and nowhere else |
|---|---|---|
| **Image reference parsing** | `internal/image` (`reference.go`) | A reference is an address in a registry's namespace. Every rule in it — `alpine` → `docker.io/library/alpine`, the implicit `:latest`, `docker.io` → `registry-1.docker.io`, "a host is a host because it has a dot or a colon or is `localhost`" — is a Distribution/Docker naming convention, and the client is the only thing that needs the parts. `internal/image` never sees a tag; it sees digests. |
| **Registry client** | `internal/image` (`registry.go`, `blob.go`) | The only package that opens a socket. Owns the `/v2/` API surface, the token dance, redirects to blob storage, retries, and media-type negotiation via `Accept`. Knows no path on disk except the ones it is handed as an `io.Writer`. |
| **Manifest fetching and platform choice** | `internal/image` (`manifest.go`) | Manifests and indexes are protocol documents: their media types, their `Accept` headers, and the platform fields in an index are all registry vocabulary. `Resolve` collapses "index → manifest" so callers get one flat answer. *Which* platform is asked for is the caller's, so `Platform` is a parameter, not a lookup. |
| **Digest verification** | Every boundary the bytes cross (§9) | `FetchBlob` verifies in flight, because that is the only moment the bytes exist as a stream and the only place a truncated response can be told from a complete one. `Commit` verifies again on the write, and `UnpackLayer` a third time at use, because the disk between those moments is not trusted. Neither delegates it: a verification you did not do yourself is a verification you are asserting on someone else's word. |
| **Layer cache** | `internal/image` (`cache.go`) | SSOT §2 gives local filesystem layout to `image` explicitly. The `Cache` owns the whole of `<image-root>/`: what a blob's path is, how a blob becomes visible, and what a half-written one looks like. The client writes to an `io.Writer` and never learns where it went. |
| **Rootfs construction** | Split, and the split is the point | `internal/rootfs` owns the *directory* (`<root>/<id>/rootfs`), as it has since Stage 2 — unchanged by this stage. `internal/image` owns the *contents*: it applies one layer to one directory. `internal/runtime` owns the *sequence*: which layers, in what order, into which container's directory, and what happens when it fails. No new edge between primitives. |
| **Config → container** | `internal/runtime` (`image.go`, the integration change) | "An unset `-workdir` falls back to the image's `WorkingDir`", "the user's `-env` overrides the image's", "no command given means `Entrypoint`+`Cmd`" are policy. `image.Config` provides the pure merge functions; `runtime` decides that they are what a container gets. Same shape as `limits.go` and `network.go`. |

### Dependency graph after Stage 5

```
                     cmd/forge
                         │
                    internal/cli
                         │
                  internal/runtime
        ┌────────┬───────┼────────┬─────────┬─────────┐
    namespace  process  rootfs  mount  cgroup  network  image
```

`image` is a leaf like every other primitive, with no edges to or from any of
them. The only package that will import it is `runtime`.

---

## 4. Storage layout on disk

Forge's state root is `/var/lib/forge` (SSOT §9). Stage 5 adds one subtree
beside the two that already exist:

```
/var/lib/forge/
├── containers/                       --root: per-container trees (Stage 2)
│   └── <container-id>/
│       └── rootfs/                   ← image layers are unpacked here
├── network/                          Stage 4 IP leases
│   └── leases/
└── images/                           --image-root, new in Stage 5, 0700
    ├── blobs/
    │   └── sha256/
    │       └── <64 hex chars>        every blob: manifests, configs, layers
    └── staging/
        └── <random>.part             downloads in progress
```

Notes on each decision:

- **One flat, content-addressed blob directory.** A manifest, a config and a
  layer are all "bytes named by their digest", and giving them separate
  directories would mean three code paths to verify one invariant. The
  `sha256/` level exists because the digest's algorithm is part of its name and
  a future algorithm must not collide.
- **No `layers/` directory of extracted trees.** That is the OverlayFS layout,
  and §6 chooses not to build it. Under explicit extraction the only durable
  artifact per layer is the compressed blob, which is exactly what FR-5.4 asks
  to be cached.
- **`staging/` is a sibling of `blobs/`, on the same filesystem**, so committing
  a blob is a `link(2)`, not a copy (§9). A file in `staging/` is *never* a
  valid blob and is never read by anything but the download that created it.
- **No `refs/` tag index.** A tag is mutable and Forge resolves it online every
  run (§0). Caching a tag→digest map would create a second source of truth whose
  staleness rules nobody asked for. Blobs are immutable and are the only thing
  cached.
- **`0700` on `images/`**, matching `containers/`: a blob can contain a setuid
  binary, and the cache is root's.
- **`--root` stays independent of `--image-root`.** They can be pointed at
  different filesystems, which is exactly what a machine with a small root
  partition wants. The one constraint the code enforces is that `staging/` and
  `blobs/` are both under `--image-root`, so the commit link never crosses a
  device.

**`NewCache` performs no I/O**, unlike `rootfs.NewStore`, which creates its root
eagerly. The directories are created on first write. A `forge run -rootfs …`
invocation should not create an image cache it will never put anything in, and a
`forge run alpine` will create it a few milliseconds later anyway.

---

## 5. The OCI pull lifecycle

Concretely, for `sudo forge run alpine:3.20 /bin/sh`:

```
cli.parseRunSpec
  positional[0] is not a path → Spec.Image = "alpine:3.20"      §7.1
runtime.Run
  spec.Validate()                          pure; image xor rootfs, no syscalls
  id := NewID()
  cleanup := newCleanupStack(log)

  1  resolveImage                          ─ registry, no container yet ─────────
     image.ParseReference("alpine:3.20")
         → {registry-1.docker.io, library/alpine, tag 3.20}
     client.Resolve(ctx, ref, image.HostPlatform())
         GET /v2/library/alpine/manifests/3.20
             Accept: oci.image.index, oci.image.manifest,
                     docker.distribution.manifest.list.v2, .v2
         401 + WWW-Authenticate → GET <realm>?service=…&scope=repository:…:pull
             → anonymous token, cached per repository for this Client
         200 → body is an index; hash it, compare with Docker-Content-Digest
         select the manifest whose platform is linux/amd64             FR-5.2
         GET /v2/library/alpine/manifests/sha256:<hex>
         200 → hash it, compare with the digest we asked for           FR-5.2
         → Manifest{Config: desc, Layers: [desc…]}

  2  fetch the config blob                 ─ same path as any other blob ────────
     cache.Has(config.Digest)?  no → 3, yes → 4
  3  download missing blobs                                            FR-5.4
     for each descriptor in [config] + layers, in order:
         if cache.Has(d.Digest) { continue }              ← the cache's whole job
         st := cache.Stage()                              staging/<random>.part
         defer st.Discard()                               idempotent
         client.FetchBlob(ctx, ref, d, st)                verifies as it streams
         st.Commit(d.Digest)                              re-verifies, then links
  4  read the config
     image.ParseConfig(cache.ReadAll(config.Digest))
         → Env, Cmd, Entrypoint, WorkingDir
     merge into the spec (runtime policy, §3)

  ── nothing on the host has been created yet; nothing is on the cleanup stack ──

     img.apply(spec)               command, env and workdir defaults from the
                                   image; the caller still overrides each

  ── nothing on the host has been created yet; the cleanup stack is empty ──────

  5  construct rootfs                                                  FR-5.3
     rootfs.Store.Prepare(id)      → <root>/<id>/rootfs
     cleanup.push("removing the container root filesystem", …)  ← BEFORE unpack
     image.BuildRootfs(ctx, cache, manifest.Layers, dir.Rootfs)
                                   layers applied base first, verified as they
                                   decompress
  6  prepare filesystem
     spec.Rootfs = dir.Rootfs      the self-bind pivot_root needs (ADR-0010)
     plan := mountPlan(spec, dir)  with Source == Root

  7  prepareCgroup                         unchanged from Stage 3
  8  prepareNetwork                        unchanged from Stage 4
  ── clone(2) ──────────────────────────────────────────────────────────────────
  9  start / attach cgroup / move interface unchanged from Stages 1–4
 10  send payload                          the child pivots, then resolves a
                                           bare command name against the
                                           container's own PATH (§7.3)
 11  wait                                  unchanged
 12  cleanup.unwind()                      network → cgroup → rootfs
```

Two properties of that order are load-bearing:

**The pull happens before anything is created.** Steps 1–4 touch the network and
the shared cache and nothing else. A reference that does not resolve, a registry
that is down, a platform that is not published, a disk that is full — every one
of them fails with an empty cleanup stack and a host that is bit-for-bit
unchanged apart from a possibly-larger cache. No container ID has been used, no
directory made, no address leased.

**The unpack happens after the rootfs cleanup is registered.** Extraction is the
one step in Stage 5 that writes into a per-container directory, and it is
inserted immediately after the `cleanup.push` that removes that directory —
never before. That single placement is what makes partial extraction safe (§9),
and it is the same rule Stages 3 and 4 follow: *register the release the moment
the resource exists, never later*.

---

## 6. FR-5.3: OverlayFS or explicit extraction

**Decision: explicit layer extraction.** Recorded as ADR-0003, moving it from
Proposed to Accepted.

Each layer's tar stream is applied, base layer first, directly into
`<root>/<id>/rootfs`. Whiteouts are applied as deletions at the moment they are
read. The result is an ordinary directory tree; the container's init bind-mounts
it onto itself to satisfy `pivot_root`'s mount-point precondition, exactly as
ADR-0010 predicted in Stage 2.

### What the alternative would have been

OverlayFS: extract each layer once into `images/layers/<diffID>/`, then mount

```
overlay on <root>/<id>/rootfs
  lowerdir=layers/<L3>:layers/<L2>:layers/<L1>   (topmost first)
  upperdir=<root>/<id>/diff  workdir=<root>/<id>/work
```

child-side, before `pivot_root`, so the kernel destroys the mount with the
namespace and there is nothing to unmount.

### Why extraction wins here

1. **It works everywhere Forge already works.** OverlayFS as an upper layer is
   refused on several filesystems Forge is otherwise happy on, and
   overlay-on-overlay is the default state inside a CI container — which is
   precisely where SSOT §7 says the privileged suite runs. Choosing overlay
   would mean the headline feature of Stage 5 is the first thing that cannot be
   tested in the project's own CI.
2. **The cache stays free of privileged, filesystem-specific artifacts.**
   Overlay's lower layers must encode whiteouts as character devices `0:0` and
   opaque directories as `trusted.overlay.opaque` xattrs. That needs `mknod`,
   needs xattr support in the backing filesystem, needs the *trusted* namespace
   (root-only), and produces a cache that cannot be inspected, copied, or backed
   up by an unprivileged user. Extraction needs none of it: a whiteout is an
   `os.RemoveAll` and is gone.
3. **It adds no kernel mechanism to a stage that is about a file format.**
   Stage 5's subject is the OCI image: manifests, digests, tar layers,
   whiteouts. Reading the extractor teaches all of it. An overlay mount teaches
   union filesystems, which is a fine lesson and is not this one — and PRD §2's
   test is whether a reader finishes the stage understanding *OCI image
   unpacking*.
4. **Zero new teardown surface.** A container's rootfs stays a plain directory.
   `rootfs.Store.Remove` already refuses to delete through a live mount, the
   cleanup stack already removes the tree, and FR-2.3's promise of no orphaned
   mounts needs no new clause. An overlay mount made child-side would be safe
   too — but "safe because the namespace died" is a claim that needs a test, and
   this one needs none.
5. **The cost is bounded and visible.** Extraction costs one decompress-and-write
   per container start, and one copy of the image per running container. For the
   images Forge targets — `alpine:3.20` is about 4 MiB compressed, `busybox`
   under 2 — that is tens of milliseconds and single-digit megabytes.

### The trade-offs being accepted

| Cost | Size | Mitigation, or why it is tolerated |
|---|---|---|
| Start-up time is O(image size), not O(1) | ~40 ms for alpine; seconds for a 500 MB image | Documented in the README. Forge is not a container-density product. |
| Disk is O(image size × running containers) | The same | The blob cache is still shared, so the *network* cost is O(1) — which is what FR-5.4 actually requires. |
| No copy-on-write, so no "diff since image" | Loses a Stage 6 `forge diff` we never promised | Not in the PRD. |
| Extraction runs as root and writes attacker-influenced paths | Real, and the one that matters | §9's containment rules, and the test suite that hits them. Note this risk exists identically in the overlay design, which must extract layers too. |

### The middle option, rejected explicitly

Extract each layer once to `layers/<diffID>/` and hard-link its files into each
container's rootfs. It has overlay's disk savings and none of its mount
requirements — and it is a correctness trap: a container writing to a
hard-linked file mutates the shared cache, and therefore every other container
built from that layer. Making it safe requires breaking links on write, which is
copy-on-write, which is overlay. Rejected.

### The seam left behind

`runtime.prepareFilesystem` calls a single function to turn a list of layer
digests into a populated `dir.Rootfs`. A later stage that adds overlay replaces
that call and the `mount.Plan.Source` it produces, and touches nothing else —
not the registry, not the store, not the cache layout, not the CLI. ADR-0003
records overlay as the expected successor rather than a rejected idea.

---

## 7. FR-5.5: `forge run <image> <cmd>`

### 7.1 The positional grammar

SSOT §9 says the positional `<image>` form arrives in Stage 5. Until now the
first positional has been the *command*, and Stages 1–4 must keep working, so
the two grammars have to coexist unambiguously:

```
forge run [flags] <image> [cmd] [args...]      new: image, cmd optional
forge run [flags] -rootfs <dir> <cmd> [args…]  Stage 2–4, unchanged
forge run [flags] <cmd-path> [args...]         Stage 1, unchanged
```

The rule, applied in `cli.parseRunSpec` in this order:

1. `-rootfs` given → the positionals are the command. Exactly Stage 2–4.
2. Otherwise, the first positional begins with `/`, `./` or `../` → it is a
   command path, run against the host's filesystem. Exactly Stage 1.
3. Otherwise → it is an image reference; the rest is the command.

This is decidable without a lookahead and without a new flag, because the two
namespaces cannot overlap: a command Forge accepts must be an absolute path
(`runtime.ErrNotAPath`, Stage 1), and a registry reference can never begin with
`/` — `docker.io/library/alpine` has slashes but not a leading one. `-rootfs`
together with an image reference is refused as a usage error rather than
resolved by precedence, for the same reason `mountPlan` refuses a bind mount
that collides with a default: the caller more likely made a mistake than a
choice.

### 7.2 What the image config contributes

`runtime` merges `image.Config` into the `Spec` before anything is created. The
precedence rule is one sentence: **the image supplies the default, the caller
overrides it.**

| Field | With no flag | With a flag |
|---|---|---|
| Command | `Entrypoint` + `Cmd` from the config | positionals replace `Cmd`, keeping `Entrypoint`; a config with neither and no positional is a usage error |
| `Env` | the image's `Env` | merged per key, caller wins; the Stage 1–4 hard-coded `PATH` becomes the fallback only when the image sets none |
| `WorkingDir` | the image's, or `/` | `-workdir` |
| Hostname, limits, network | unchanged — no image field feeds them | unchanged |

`User`, `Volumes`, `ExposedPorts` and `StopSignal` are read and ignored, and
that is stated in the package doc so a reader knows it is a decision.

### 7.3 Bare command names

With an image there is finally a `PATH` that means something inside the
container, so `forge run alpine ls` works. Stage 5 narrows Stage 1's rule rather
than deleting it:

- **With an image**, a bare name is accepted by `Spec.Validate` and resolved
  child-side, in `runtime.resolveCommand`, after `pivot_root` and against the
  container's own `PATH` from the image's environment. A name that resolves to
  nothing fails with `ErrCommandNotFound`, naming every directory searched.
- **Without an image** — `-rootfs`, or the host's filesystem — `ErrNotAPath`
  stands, with its original message. Nothing about Stages 1–4 moves.

That asymmetry is the honest one: searching a `PATH` is safe exactly when Forge
knows which filesystem it is searching. Three properties make the search safe,
and all three are properties of *where* it happens rather than of the code:

1. It runs after `pivot_root`, so every directory it looks in is inside the
   container. There is no host path it could reach.
2. It runs in the child, the only process whose `/` is the container's.
3. The `PATH` it reads is the container's environment, which came from the
   image.

`decodeInitPayload` refuses a bare name in a payload with no mount plan. The
parent already refuses it, so this is defence in depth of the same kind the
mount plan and the network interface get — and for the same reason: this is the
process that would act on it, and without a pivot the search would run over the
host's directories and `execve` the host's binary.

### 7.4 Backward compatibility

Three grammars coexist, and the rule that separates them is decidable without a
lookahead and without a new flag (`cli.splitImageAndCommand`):

| Invocation | Read as | Stage |
|---|---|---|
| `forge run -rootfs <dir> <cmd> [args…]` | the positionals are the command | 2–4, unchanged |
| `forge run <path> [args...]` where `<path>` starts `/`, `./` or `../` | the positionals are the command | 1, unchanged |
| `forge run <image> [cmd] [args...]` | an image reference, then the command | 5, new |

The two namespaces cannot overlap, which is what makes this unambiguous rather
than merely conventional: a command Forge accepts without an image must be a
path (`ErrNotAPath`, Stage 1), and a registry reference can never begin with
`/` — `docker.io/library/alpine` has slashes, but not a leading one.

What is guaranteed, and asserted:

- **A container with no image skips lifecycle steps 1 to 4 entirely.** No
  reference is parsed, no socket is opened, and `image.NewCache` performs no
  I/O, so a runner used only with `-rootfs` never creates a blob cache
  directory. (`TestNewRunnerCreatesNoImageCache`)
- **`Spec.Env` semantics are unchanged for a container with no image.** The CLI
  supplies `DefaultEnv` exactly as before; only an image run gets the merge.
- **`ErrNotAPath` still fires for a bare name** with `-rootfs` and on the host
  filesystem, with its original wording. (`TestValidateBareCommandNames`)
- **The mount plan is built the same way.** With an image, `Plan.Source` becomes
  equal to `Plan.Root` — the self-bind ADR-0010 predicted in Stage 2 — and
  nothing in `internal/mount` needed changing to accept it.
- **One behaviour did change, deliberately:** `forge run echo` used to be an
  error and now names an image called `echo`. There is no way to keep both
  readings, the new one is what every other container runtime does, and the old
  one was a refusal rather than a working invocation.

## 8. Cleanup and rollback order

Stage 5 adds **no new entry to the container cleanup stack.** That is the design,
not a happy accident: every durable thing Stage 5 creates is either
content-addressed and shared (a blob, which no single run may delete) or inside
the container directory (extracted files, which the Stage 2 cleanup already
removes wholesale).

The stack at its fullest, and the order it unwinds in:

```
registration (acquisition order)          unwind (reverse)
1  remove the container root filesystem ┐  3  release the network
2  destroy the cgroup                   │  2  destroy the cgroup
3  release the network                  ┘  1  remove the container root filesystem
       ↑                                          ↑
   unchanged from Stage 4              now also deletes the unpacked layers
```

The rootfs step is registered first and therefore runs last, which is still
correct with images in the picture: it is the step that deletes the files a
still-running container has open, so it must not happen while there is one.

Rollback that lives *outside* the stack, because it is scoped to a single
function rather than to a run:

| Resource | Released by | Idempotent because |
|---|---|---|
| A staging file | `defer st.Discard()` in the download loop | `Discard` closes and removes, tolerating both being already done; `Commit` marks it discarded, so the deferred call after a successful commit does nothing |
| A partially written blob | Nothing — it never exists | Bytes are only ever visible under `blobs/` via `link(2)` from a fully verified staging file |
| A quarantined corrupt blob | `Cache.Remove(digest)` | `os.Remove` with `ErrNotExist` treated as success. It takes no `ctx`, unlike `rootfs.Store.Remove`: there is no mount table to consult and nothing to cancel, so the signature stays as small as the operation |
| Stale staging files from a killed forge | `Store.PruneStaging(ctx, 24h)` at the start of a pull | Removing a file that is gone is success; the age bound is what keeps it from deleting a concurrent run's download (§9) |
| An HTTP body | `defer resp.Body.Close()`, logged at WARN on failure | SSOT §13.7 |

Two rules that are worth stating because breaking either is easy:

- **A run never deletes a blob it did not just prove is corrupt.** The cache is
  shared with every concurrent and future run. "Clean up after yourself" is
  wrong here; the only legitimate deletions are a verification failure (§9) and
  a future `forge image prune`.
- **A failed unpack does not try to undo itself file by file.** It returns, and
  the cleanup stack removes the whole tree. Partial removal of a partial
  extraction is strictly more code and strictly less certain.

---

## 9. Failure scenarios

| Scenario | What happens | Why it is safe |
|---|---|---|
| **Interrupted download** (connection reset, `ctx` cancelled, process killed) | `Fetch` returns; the deferred `Discard` removes the staging file. If forge was `SIGKILL`ed the file survives, and the next pull's `PruneStaging(24h)` removes it. | A staging file is never under `blobs/`, is never opened by anything but its own download, and its name is random — so it can neither be mistaken for a blob nor collide with another run. |
| **Truncated response** (fewer bytes than the descriptor's `Size`) | `Fetch` compares both the running hash and the byte count against the descriptor and returns `ErrDigestMismatch`; nothing is committed. | The length check catches the case a hash alone would also catch, but reports it in the language the operator needs: "the registry sent 3 of 5 MB". |
| **Digest mismatch on a manifest** | `Resolve` fails before any blob is requested. For a digest reference, the computed hash is compared to what the user asked for; for a tag, to `Docker-Content-Digest` when the registry sends it. The manifest is never stored. | A manifest is the root of trust for every layer under it. Storing an unverified one would make every later verification circular. |
| **Digest mismatch on a blob** | Detected in flight by `Fetch`, and again by `Commit` before the link. Either way the staging file is discarded and the run fails with the descriptor, the expected digest and the computed one in the message. | Two checks, one for the wire and one for the disk write between them; neither is trusted to cover the other's window. |
| **Corrupt layer in the cache** (bit rot, a crash mid-`link`, a filesystem that lied) | `UnpackLayer` hashes the blob as it decompresses. On mismatch it stops, calls `Cache.Remove(digest)` to quarantine the bad blob, and returns `ErrCorruptLayer` naming it. The next run re-downloads it. | This is the reason verification happens at *use* and not only at *ingest*. Decompression already reads every byte, so the check costs one hash pass and no extra I/O. Deleting is safe precisely because the blob is content-addressed: a blob whose bytes do not hash to its name is definitively wrong, not merely suspect. |
| **Malformed tar / unsupported media type** | `UnpackLayer` fails with `ErrCorruptLayer` or `ErrUnsupportedLayerType` (zstd, foreign layers). The blob is *not* removed for an unsupported type — those bytes are fine, Forge simply cannot read them. | Distinguishing "wrong bytes" from "bytes I don't implement" keeps the quarantine rule honest. |
| **Path escape in a layer** (`../../etc/shadow`, a symlink `etc → /etc` followed by a write to `etc/passwd`, an absolute member name) | Refused with `ErrEscapesRoot`. Every member's cleaned path is checked for `..` and for absoluteness, and every parent directory is resolved within the destination before a write — a symlink whose target leaves the tree is never traversed, only replaced. | This is the extractor's single most consequential branch and gets the same treatment `mount.checkNoEscape` and `mount.resolveDestination` got in Stage 2, including the same style of test table. Root is writing files whose names came from the internet. |
| **Whiteouts** (`.wh.foo`, `.wh..wh..opq`) | `.wh.foo` removes `foo` from the tree built so far and creates nothing; `.wh..wh..opq` empties its directory before the rest of that layer is applied. Both are applied at the moment they are read, so ordering within a layer is preserved. | Deletion is the whole of the semantics under explicit extraction — no character devices, no xattrs (§6). |
| **Partial extraction** (ENOSPC on layer 3 of 5) | The unpack returns; `Run` returns; the cleanup stack removes `<root>/<id>/` entirely. Blobs already committed stay in the cache and make the retry cheap. | The rootfs cleanup was registered before the first byte was extracted (§5, §8). There is no intermediate state it does not cover. |
| **Duplicate pull** (the same image run twice) | Every `store.Has` returns true; no blob is transferred. The tag is still resolved over the network, which is one small request. | FR-5.4 exactly. The manifest request is deliberately not cached — a tag is mutable, and a stale `alpine:3.20` would be a bug the user cannot see. |
| **Concurrent pulls of the same image** (two `forge run`s at once) | Both resolve the manifest. Both may find a blob missing and download it. Each writes to its *own* staging file. `Commit` uses `link(2)` and treats `EEXIST` as success: the loser discards its copy and proceeds with the winner's — which is byte-identical, because both are verified against the same digest. | `link(2)` is atomic and never replaces, so a blob's inode is stable once created and a concurrent reader can never see a file being overwritten. No lock file exists, so no lock can be leaked by a `SIGKILL` — the same reasoning that made Stage 4's leases `O_CREAT|O_EXCL` files rather than a lock (Stage 4 shipped without an ADR recording it; ADR-0021 now carries the argument for this stage). The cost is a duplicated download in a narrow window, which is bandwidth, not correctness. |
| **Concurrent runs of the same image** | Nothing shared. Each container extracts into its own `<root>/<id>/rootfs`. | The consequence of §6's decision that is worth the disk it costs. |
| **Registry down, DNS failure, 5xx** | `ErrRegistryUnavailable`, wrapping the transport error. Idempotent GETs are retried a small fixed number of times with backoff; 4xx are not. | Nothing has been created; the run fails clean. |
| **401 that a token does not fix** (a private image) | `ErrUnauthorized`, with a message saying Forge pulls anonymously and this image needs credentials. | Scoping decision from §0, surfaced where the user meets it rather than as a bare HTTP status. |
| **No manifest for this platform** | `ErrNoMatchingPlatform`, listing the platforms the index does offer. | An arm64 host asking for an amd64-only image should be told that, not handed a manifest that will `ENOEXEC` inside the container. |

---

## 10. What is deliberately not solved

- **Cache growth.** Blobs are never collected. A `forge image prune` needs to
  know which blobs are reachable from which manifests, which needs a manifest
  index, which is a state store — Stage 6. Until then the cache is bounded by
  what the operator pulls, and `rm -rf /var/lib/forge/images` is a safe,
  documented reset.
- **Resumable downloads.** A failed 400 MB layer restarts from zero. Range
  requests are an optimization with real complexity (partial hashes, server
  support detection) and no educational payoff.
- **Layer parallelism.** Layers are fetched sequentially. Concurrency here is a
  worker pool over an independent-work loop — well understood, and it would make
  the pull sequence in §5 read as a scheduler rather than as a list of steps.
  Recorded as the first optimization to make once the stage is correct.

---

## 11. Test strategy

### The fixture that makes it all cheap

`internal/image/fake_test.go` holds two helpers, and every test is built on
them, so the same bytes exercise the reference parser, the client, the cache and
the extractor.

The first is a synthetic image builder: given a list of tar entries it produces
gzipped (or uncompressed) layers, an image config, a manifest and an index, all
correctly digested, in memory.

The second is an `httptest.Server` implementing the slice of the Distribution
Spec Forge uses — `/v2/`, manifests by tag and digest, blobs by digest, and a
token endpoint that issues an anonymous token after a 401 challenge. It can also
be told to truncate a blob, swap its contents, block one mid-transfer, misreport
a `Docker-Content-Digest`, or fail a fixed number of requests with a 503.
Because it is a real HTTP server on loopback, `image.Client` runs against it
completely unmodified — no injected fake, and therefore no interface introduced
for testability alone.

### Unit tests (no root, no network — SSOT §7)

All implemented, and all of the following pass under `-race`.

| Area | What is asserted |
|---|---|
| `ParseReference` | Two tables, ~30 cases: `alpine`, `alpine:3.20`, `library/alpine`, two-component names that must *not* get the `library/` prefix, `docker.io/library/alpine`, `ghcr.io/o/r:tag`, deep repository paths, `localhost/r`, `localhost:5000/r`, `127.0.0.1:5000/team/r:v2`, `r@sha256:…`, `r:tag@sha256:…`. Rejections: empty, `:` alone, `alpine:` with no tag, uppercase repositories, components that start or end with a separator, whitespace, a NUL byte, an unknown digest algorithm, a short digest, an uppercase digest, an over-long tag, `alpine:3.20/rc1`. Plus `String()` round-tripping and `Host()`'s Docker Hub special case. |
| Platform selection | An index with amd64/arm64/windows entries; the right manifest chosen, `ErrNoMatchingPlatform` naming what *is* offered, attestation manifests (`unknown/unknown`) skipped, and a variantless arm64 image matching a v8 host. |
| `Client.Resolve` | Tag, digest, index-then-manifest, an unknown tag (404 relaying the registry's own message), 401-then-token with the token cached for the second call, a digest request answered with other bytes, a lying `Docker-Content-Digest`, a missing one, a manifest over the size cap, and zstd and foreign layers refused before any blob is fetched. |
| `Client.FetchBlob` | The correct blob; a truncated one (the error names both byte counts); one whose contents were swapped for others of the same length; a blob the registry does not have. |
| Retry policy | A transient 503 recovered from within `MaxAttempts`; exhausted attempts reported as `ErrRegistryUnavailable` with exactly the allowed number of requests made; an unreachable registry; a persistent 401 whose message explains that Forge pulls anonymously; an auth scheme Forge does not implement, named. |
| `Cache` | `Path` arithmetic and rejection of a digest containing `/`, `..`, uppercase or the wrong length; `Has` before and after; staged bytes invisible until `Commit`; `Commit` with the wrong digest leaving `blobs/` empty; `Write`/`Commit` after `Commit`; committing over an existing blob (`EEXIST` is success and the original inode survives); `Verify` catching bit rot; `PruneStaging` respecting the age bound. |
| Concurrency | 16 goroutines committing one blob, and 8 concurrent `Pull`s of an uncached three-layer image through one shared cache. Every caller succeeds, every blob verifies, and `staging/` is empty. |
| `Pull` | Every blob cached and verified; a duplicate pull fetching zero blobs while still making one manifest request; a blob that fails verification never reaching the cache; a pull cancelled mid-layer leaving no partial blob and no staging file, with the completed blobs kept and the retry succeeding. |
| Containment | `entryPath` against absolute names, `..`, deep `..`, and a NUL byte; `resolveWithin` rebasing absolute and relative symlinks, refusing one that climbs out, not following the final component, and failing a symlink loop rather than hanging. |
| `UnpackLayer` | Every entry type; missing parents created; an uncompressed tar accepted by sniffing; modes including setuid preserved; a 0500 directory still receiving its contents; a FIFO created; a character device failing with a message that says it needs root; entries whose ownership could not be applied counted; `../escape`, `/absolute` and an escaping hard link refused with nothing written outside; a truncated tar reported as `ErrCorruptLayer`; a corrupt cached blob quarantined. |
| Layer semantics | Three layers where the middle overwrites and the top whiteouts; whiteouts applied in read order so a delete-then-recreate ends present; an opaque whiteout emptying a directory without removing it; entries changing type between layers; a write through a symlink from a lower layer landing inside the tree while the host's own file is untouched. |
| `BuildRootfs` | Layers applied in order; a failure part-way leaving the destination empty but the directory itself intact; a cancelled build rolling back; a missing layer blob; argument validation. |
| `image.Config` | `Command(nil)` = entrypoint+cmd; an override replacing cmd but keeping entrypoint; cmd alone; neither, giving nil; `Environ` merging per key with the caller winning and order preserved. |
| Cleanup | `Discard`, `Remove` and `PruneStaging` each run three times; `Discard` after `Commit` keeping the blob; `PruneStaging` on a cache that was never written to, on a concurrently removed file, and under a cancelled context. The cleanup stack's reverse order, its running everything despite a failure, its idempotent unwind and its `cancel`. |

Statement coverage of `internal/image` is 82.6%. What is uncovered is almost
entirely I/O error branches — a failed `Chmod`, a `Close` that errors — which
cannot be provoked without a fault-injecting filesystem.

### Integration tests — real registries, no mocks

`test/integration/stage5_test.go`, behind the `integration` build tag. Every
manifest and every layer byte comes from Docker Hub: the real anonymous token
dance, the real 307 to a CDN, the real gzipped tars. Nothing is mocked, because
the failures worth catching at this level are the ones a mock cannot have.

| # | Test | What it asserts |
|---|---|---|
| 1 | `TestPullAlpineLatest` | a cold pull of `alpine:latest` transfers bytes, caches every blob, and leaves no staging file |
| 2 | `TestPullAlpine320` | the same for `alpine:3.20`, plus that a pinned tag resolves to the same digest twice |
| 3 | `TestRunCommandFromImage` | alpine's own `/bin/sh`, from alpine's own layers, in its own namespaces |
| 3 | `TestRunBareCommandNameFromImage` | `cat /etc/alpine-release` — a bare name resolved against the container's PATH (§7.3) |
| 3 | `TestRunUsesTheImageCommand` | no command given runs the image's own `Cmd` |
| 4 | `TestManifestAndBlobDigestsVerify` | a tag and the digest it resolves to name identical content, and every cached blob re-hashes to its name |
| 5 | `TestDigestMismatchIsDetectedInFlight` | one byte of a real layer flipped in transit is caught, nothing corrupt is cached |
| 5 | `TestUnknownDigestIsNotFound` | an absent digest is a 404, not a mismatch — only one of them means tampering |
| 6 | `TestCachedLayersAreReused` | a warm pull fetches nothing, and the blobs are not even rewritten |
| 7 | `TestNoDuplicateDownloads` | measured at the socket: zero blob requests and zero blob bytes on a warm pull, and one manifest request per pull |
| 8 | `TestConcurrentPulls` | six concurrent pulls of an uncached image through one cache all succeed and every blob verifies |
| 9 | `TestMultiLayerImageRootfs` | a real multi-layer image: files from the base layer *and* from a layer above it |
| 10 | `TestRootfsCorrectness` | regular files, a symlink still a symlink, an executable still executable, nested directories |
| 10 | `TestRootfsCorrectnessFromInsideTheContainer` | the same tree as the container sees it, which is the only view that proves the pivot landed on the layers |
| 11 | `TestCleanupAfterContainerExit` | no container directory, no mount under the root, and the shared cache untouched |
| 12 | `TestCorruptCacheRecovery` | bit rot caught at use, the blob quarantined, the partial tree emptied, and the next pull repairing exactly that blob |
| 13 | `TestInterruptedPullRecovery` | a transfer cancelled mid-body leaves no partial blob and no staging file, and the retry completes |

**The three instruments, and why they are not mocks.** Detecting tampering,
proving no duplicate download, and interrupting a transfer all need something a
registry will not do on request. Each is met by a `http.RoundTripper` or a proxy
between Forge and the real Docker Hub that counts what passes or alters one byte
of it. The bytes are genuine; only the interference is synthetic, and the
interference is precisely what digest verification defends against.

**Parallel-safe and self-cleaning.** Every test is `t.Parallel()`. Tests that
only need an image to *exist* share one run-wide cache, removed by `TestMain`,
so alpine is transferred once for the whole suite. Tests whose subject is what
gets downloaded — or that deliberately corrupt a blob — each get a private cache
under `t.TempDir()`. Container roots are `t.TempDir()` throughout, and the
cleanup assertions check Forge removed its own trees rather than relying on the
test framework.

**Skips, not failures.** A host that cannot reach Docker Hub skips; a suite that
goes red on a train is one people learn to ignore. The five container tests skip
without root, like every other stage's.

**Timing.** The suite does real network I/O, so `make test-integration` passes
`-timeout 30m`. On a normal link it finishes in well under a minute; the
measured 5m48s above was on a deliberately slow one.

Stages 1–4's suites are re-run unchanged. `-rootfs` keeps its exact Stage 2–4
behaviour, which is what makes "existing functionality continues to work" an
assertion rather than a claim.

---

## 12. The open questions, resolved

The first three were open at design time and are now decided in code; each is a
named constant in `registry.go` or `cache.go` with the reasoning attached.

1. **Manifest size cap: 4 MiB**, overridable with `ClientConfig.MaxManifestBytes`.
   The spec gives no number. A manifest has to be buffered to be hashed before
   it can be trusted enough to parse, so without a cap a hostile registry could
   make Forge allocate without bound. One byte over the cap is read so that "at
   the limit" and "over it" are distinguishable.
2. **Retry policy: three attempts, exponential from 200 ms**, `Retry-After`
   honoured up to 10 s, idempotent GETs only. Transport failures and 5xx and 429
   are retried; 4xx are not, because a 404 will still be a 404. A cancelled
   context is the caller's decision and is never retried.
3. **`PruneStaging` age bound: 24 hours**, passed by `Pull` rather than baked
   into `PruneStaging`, which takes it as an argument. The bound only has to
   exceed the longest plausible download; anything shorter risks deleting a
   concurrent run's live one, which is the single way this design could corrupt
   a pull that was going to succeed.
4. **`-pull=never|missing|always` is still deferred** to Stage 6, where a state
   store makes "what is cached" a question Forge can answer directly. It would
   give offline runs for a fully cached image, which is the one thing §0's
   "resolve the tag every run" rule costs.

Two decisions were taken during implementation that the design had not
anticipated, both in the extractor:

5. **A directory entry follows an existing symlink rather than replacing it.**
   A layer that declares `lib/` over a lower layer's `lib -> usr/lib` must not
   destroy the link: images built with a merged `/usr` depend on it. Writes
   *through* such a link are still rebased against the destination, so nothing
   escapes. `resolveWithin` and `resolveDir` are the two halves of that rule.
6. **Directory modes are applied in a second pass, deepest first.** A layer
   containing a 0500 directory followed by entries inside it is legal and
   common, and honouring the mode immediately would make those writes fail.

# 0005. Container ID generation scheme

Date: 2026-07-27
Status: Accepted

## Context

Every container needs an identifier. SSOT §6 requires each log line for a
container operation to carry a `container_id`, so an ID is needed from Stage 1
even though the state store that will persist it does not arrive until Stage 6.

SSOT §8 specifies the shape: "12-character lowercase hex (like Docker's short
ID), generated from a random UUID, truncated — documented in
`internal/runtime`."

That specification is internally inconsistent in one respect. A UUIDv4 carries
128 bits, six of which are fixed version and variant markers. Truncating its hex
form to 12 characters keeps only the first 48 bits and discards every one of
those markers, so the result is indistinguishable from 48 bits read directly
from the CSPRNG. Implementing a UUID layer would therefore add code with no
observable effect on the output.

## Decision

Generate container IDs as 12 lowercase hex characters encoding 6 bytes read
from `crypto/rand`. Implemented as `runtime.NewID`.

Do not implement a UUID type. SSOT §8 is amended in the same change to describe
the derivation as "48 bits from a CSPRNG, hex-encoded" rather than "from a
random UUID, truncated"; the observable format it specifies — 12 lowercase hex
characters — is unchanged.

`crypto/rand` is used rather than `math/rand` so IDs cannot be predicted from
one another. A failure to read the system CSPRNG is returned as an error rather
than falling back to a weaker source.

## Consequences

Easier:

- The implementation is six lines with no dependency, and the output matches
  the format SSOT §8 specifies exactly.
- IDs are unpredictable, so a container ID appearing in a log or a path does
  not reveal anything about other containers.

Harder:

- 48 bits gives a 50% collision probability at roughly 2×10^7 IDs by the
  birthday bound. For a single-host educational runtime this is not a practical
  concern, but it is a real bound and it is the reason Stage 6 must treat a
  duplicate ID in the state store as an error rather than assuming uniqueness.
- IDs are not sortable or time-ordered, so `forge ps` in Stage 6 will need to
  sort on a stored timestamp rather than on the ID.

Revisit if Forge ever needs globally unique IDs across hosts, which would make
a full UUID or a ULID worth the extra bytes.

package runtime

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
)

// IDLen is the length of a container ID in hex characters, per SSOT §8.
const IDLen = 12

// idBytes is the number of random bytes behind an ID. Two hex characters
// encode one byte.
const idBytes = IDLen / 2

// NewID returns a fresh container ID: 12 lowercase hex characters, like
// Docker's short ID.
//
// The 48 bits of entropy give a 50% collision chance at roughly 2×10^7
// concurrent containers, which is far beyond what a single host running an
// educational runtime will ever hold. See ADR-0005.
//
// It reads from crypto/rand rather than math/rand so IDs are not predictable
// from one another; a failure to read the system CSPRNG is fatal to the
// operation and is returned rather than papered over.
func NewID() (string, error) {
	b := make([]byte, idBytes)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generating container id: %w", err)
	}
	return hex.EncodeToString(b), nil
}

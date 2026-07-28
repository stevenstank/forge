package runtime_test

import (
	"strings"
	"testing"

	"github.com/stevenstank/forge/internal/runtime"
)

func TestNewIDShape(t *testing.T) {
	t.Parallel()

	id, err := runtime.NewID()
	if err != nil {
		t.Fatalf("NewID() = %v", err)
	}
	if len(id) != runtime.IDLen {
		t.Errorf("NewID() = %q, length %d, want %d", id, len(id), runtime.IDLen)
	}
	const lowerHex = "0123456789abcdef"
	if strings.Trim(id, lowerHex) != "" {
		t.Errorf("NewID() = %q, want only lowercase hex characters", id)
	}
}

func TestNewIDIsUnique(t *testing.T) {
	t.Parallel()

	const iterations = 1000

	seen := make(map[string]struct{}, iterations)
	for range iterations {
		id, err := runtime.NewID()
		if err != nil {
			t.Fatalf("NewID() = %v", err)
		}
		if _, dup := seen[id]; dup {
			t.Fatalf("NewID() returned duplicate %q within %d draws", id, iterations)
		}
		seen[id] = struct{}{}
	}
}

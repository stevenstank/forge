package image

import (
	"errors"
	"io"
	"log/slog"
	"slices"
	"testing"

	"github.com/stevenstank/forge/internal/logging"
)

// The cleanup stack is the same model internal/runtime uses, and the same two
// properties are what make it worth having: reverse order, and everything runs
// even when something fails.

func testStack() *cleanupStack {
	return newCleanupStack(logging.New(io.Discard, slog.LevelError))
}

func TestCleanupStackUnwindsInReverseOrder(t *testing.T) {
	t.Parallel()

	var order []string
	stack := testStack()

	for _, name := range []string{"first", "second", "third"} {
		stack.push(name, func() error {
			order = append(order, name)
			return nil
		})
	}

	stack.unwind()

	want := []string{"third", "second", "first"}
	if !slices.Equal(order, want) {
		t.Errorf("unwind order = %v, want %v", order, want)
	}
}

// A cleanup that fails must not prevent the ones registered before it, or a
// failed operation leaks precisely what the unwind existed to release.
func TestCleanupStackRunsEverythingDespiteAFailure(t *testing.T) {
	t.Parallel()

	var ran []string
	stack := testStack()

	stack.push("bottom", func() error { ran = append(ran, "bottom"); return nil })
	stack.push("broken", func() error { ran = append(ran, "broken"); return errors.New("boom") })
	stack.push("top", func() error { ran = append(ran, "top"); return nil })

	stack.unwind()

	want := []string{"top", "broken", "bottom"}
	if !slices.Equal(ran, want) {
		t.Errorf("ran = %v, want %v", ran, want)
	}
}

// unwind is idempotent so a function can defer it and still call it explicitly
// on an error path.
func TestCleanupStackUnwindsOnce(t *testing.T) {
	t.Parallel()

	calls := 0
	stack := testStack()
	stack.push("counted", func() error { calls++; return nil })

	stack.unwind()
	stack.unwind()

	if calls != 1 {
		t.Errorf("cleanup ran %d times, want 1", calls)
	}
}

// cancel is the success path: everything worked, so there is nothing to roll
// back, and the deferred unwind must do nothing.
func TestCleanupStackCancel(t *testing.T) {
	t.Parallel()

	calls := 0
	stack := testStack()
	stack.push("counted", func() error { calls++; return nil })

	stack.cancel()
	stack.unwind()

	if calls != 0 {
		t.Errorf("cleanup ran %d times after cancel, want 0", calls)
	}
}

func TestParseDigest(t *testing.T) {
	t.Parallel()

	algorithm, hex, err := parseDigest("sha256:" + zeroHex)
	if err != nil {
		t.Fatalf("parseDigest() = %v", err)
	}
	if algorithm != "sha256" || hex != zeroHex {
		t.Errorf("parseDigest() = (%q, %q)", algorithm, hex)
	}

	for _, bad := range []string{"", "sha256", zeroHex, "sha512:" + zeroHex, "sha256:xyz"} {
		if err := validateDigest(bad); !errors.Is(err, ErrInvalidDigest) {
			t.Errorf("validateDigest(%q) = %v, want %v", bad, err, ErrInvalidDigest)
		}
	}
}

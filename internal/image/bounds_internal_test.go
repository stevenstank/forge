package image

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"os"
	"testing"
)

// A manifest's Size is the compressed size, and it is the only size Forge
// verifies. These are the two bounds that stop a layer which hashes to its own
// digest perfectly well from consuming unbounded disk and time.
//
// Both were measured before they were bounded: 512 MiB of zeros compresses to
// 510 KB, and 100,000 empty entries compress to 700 KB and take 85 seconds to
// extract. Neither is caught by anything else in the pipeline — a decompression
// bomb is not corrupt, so digest verification passes it through.
//
// These tests lower the bounds rather than reaching them, and so must not run
// in parallel with anything that unpacks.

// withBounds lowers the extraction limits for one test and restores them.
func withBounds(t *testing.T, maxBytes int64, maxEntries int) {
	t.Helper()

	origBytes, origEntries := maxLayerBytes, maxLayerEntries
	maxLayerBytes, maxLayerEntries = maxBytes, maxEntries
	t.Cleanup(func() { maxLayerBytes, maxLayerEntries = origBytes, origEntries })
}

// gzipTar renders entries as a gzipped tar, which is what a layer blob is.
func gzipTar(t *testing.T, write func(*tar.Writer)) []byte {
	t.Helper()

	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	archive := tar.NewWriter(gz)

	write(archive)

	if err := archive.Close(); err != nil {
		t.Fatalf("closing tar: %v", err)
	}
	if err := gz.Close(); err != nil {
		t.Fatalf("closing gzip: %v", err)
	}
	return buf.Bytes()
}

func TestLayerByteBudgetIsEnforced(t *testing.T) {
	withBounds(t, 4<<10, maxLayerEntries)

	// A single member well past the budget, which is the shape of a zero bomb.
	body := bytes.Repeat([]byte{0}, 64<<10)
	layer := gzipTar(t, func(w *tar.Writer) {
		h := &tar.Header{Name: "big", Typeflag: tar.TypeReg, Mode: 0o644, Size: int64(len(body))}
		if err := w.WriteHeader(h); err != nil {
			t.Fatalf("header: %v", err)
		}
		if _, err := w.Write(body); err != nil {
			t.Fatalf("body: %v", err)
		}
	})

	dest := t.TempDir()
	_, err := applyLayer(context.Background(), bytes.NewReader(layer), dest, nil)
	if !errors.Is(err, ErrLayerTooLarge) {
		t.Fatalf("applyLayer() = %v, want %v", err, ErrLayerTooLarge)
	}

	// The bound must stop the write, not merely report it afterwards.
	if info, statErr := os.Stat(dest + "/big"); statErr == nil && info.Size() > 8<<10 {
		t.Errorf("%d bytes were written despite a %d byte budget", info.Size(), 4<<10)
	}
}

func TestLayerEntryBudgetIsEnforced(t *testing.T) {
	withBounds(t, maxLayerBytes, 16)

	layer := gzipTar(t, func(w *tar.Writer) {
		for i := range 64 {
			h := &tar.Header{Name: fmt.Sprintf("f%03d", i), Typeflag: tar.TypeReg, Mode: 0o644}
			if err := w.WriteHeader(h); err != nil {
				t.Fatalf("header: %v", err)
			}
		}
	})

	dest := t.TempDir()
	_, err := applyLayer(context.Background(), bytes.NewReader(layer), dest, nil)
	if !errors.Is(err, ErrLayerTooLarge) {
		t.Fatalf("applyLayer() = %v, want %v", err, ErrLayerTooLarge)
	}

	// Empty entries write no bytes at all, which is exactly why the byte bound
	// cannot be the only one.
	entries, readErr := os.ReadDir(dest)
	if readErr != nil {
		t.Fatalf("reading %s = %v", dest, readErr)
	}
	if len(entries) > 17 {
		t.Errorf("%d entries were created despite a 16 entry budget", len(entries))
	}
}

// A layer inside the bounds is untouched by them.
func TestLayersWithinTheBoundsAreUnaffected(t *testing.T) {
	withBounds(t, 1<<20, 64)

	layer := gzipTar(t, func(w *tar.Writer) {
		for i := range 8 {
			body := fmt.Sprintf("contents of file %d", i)
			h := &tar.Header{
				Name: fmt.Sprintf("f%d", i), Typeflag: tar.TypeReg,
				Mode: 0o644, Size: int64(len(body)),
			}
			if err := w.WriteHeader(h); err != nil {
				t.Fatalf("header: %v", err)
			}
			if _, err := w.Write([]byte(body)); err != nil {
				t.Fatalf("body: %v", err)
			}
		}
	})

	dest := t.TempDir()
	stats, err := applyLayer(context.Background(), bytes.NewReader(layer), dest, nil)
	if err != nil {
		t.Fatalf("applyLayer() = %v", err)
	}
	if stats.Files != 8 {
		t.Errorf("Files = %d, want 8", stats.Files)
	}
}

// budgetReader is the mechanism both bounds rest on: it must stop exactly at
// the budget and remember that it did, so an exhausted budget is never
// mistaken for a stream that ended.
func TestBudgetReader(t *testing.T) {
	t.Parallel()

	source := bytes.Repeat([]byte("x"), 100)

	within := &budgetReader{r: bytes.NewReader(source), remaining: 1000}
	got, err := readAllFrom(within)
	if err != nil || len(got) != 100 {
		t.Errorf("within budget: read %d bytes, err %v", len(got), err)
	}
	if within.exceeded {
		t.Error("exceeded = true for a stream that fitted")
	}

	tight := &budgetReader{r: bytes.NewReader(source), remaining: 40}
	got, err = readAllFrom(tight)
	if err != nil {
		t.Errorf("over budget: err %v", err)
	}
	if len(got) != 40 {
		t.Errorf("over budget: read %d bytes, want exactly the 40 allowed", len(got))
	}
	if !tight.exceeded {
		t.Error("exceeded = false after the budget ran out")
	}
}

func readAllFrom(r *budgetReader) ([]byte, error) {
	var out bytes.Buffer
	buf := make([]byte, 7)
	for {
		n, err := r.Read(buf)
		out.Write(buf[:n])
		if err != nil {
			if errors.Is(err, os.ErrClosed) {
				return out.Bytes(), err
			}
			return out.Bytes(), nil
		}
	}
}

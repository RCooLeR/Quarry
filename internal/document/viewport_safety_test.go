package document

import (
	"errors"
	"math"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestVisiblePageRejectsHostileLimitsBeforeAllocation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "source.txt")
	if err := os.WriteFile(path, []byte("one\ntwo\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	doc, err := OpenFile(path)
	if err != nil {
		t.Fatal(err)
	}
	defer doc.Close()

	tests := []struct {
		name  string
		count int
		opts  VisibleLineOptions
	}{
		{name: "line count", count: maxVisiblePageLines + 1},
		{name: "page bytes", count: 1, opts: VisibleLineOptions{MaxBytes: maxVisiblePageBytes + 1}},
		{name: "line bytes", count: 1, opts: VisibleLineOptions{MaxLineBytes: maxVisibleLineBytes + 1}},
		{name: "long-line probe", count: 1, opts: VisibleLineOptions{LongLineLimitBytes: maxLongLineProbeBytes + 1}},
		{name: "negative horizontal", count: 1, opts: VisibleLineOptions{HorizontalByteOffset: -1}},
		{name: "line-number overflow", count: 2, opts: VisibleLineOptions{FirstLineNumber: math.MaxInt64}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			page, err := doc.VisiblePageFromOffset(0, tt.count, tt.opts)
			if !errors.Is(err, ErrVisiblePageLimit) {
				t.Fatalf("error = %v, want ErrVisiblePageLimit", err)
			}
			if len(page.Lines) != 0 {
				t.Fatalf("hostile request materialized lines: %#v", page)
			}
		})
	}

	page, err := doc.VisiblePageFromOffset(0, 1, VisibleLineOptions{MaxBytes: 16, MaxLineBytes: 8, HorizontalByteOffset: math.MaxInt})
	if err != nil {
		t.Fatalf("large non-allocating horizontal offset: %v", err)
	}
	if len(page.Lines) != 1 || page.Lines[0].Text != "" || page.Lines[0].DisplayOffset != int64(len("one")) {
		t.Fatalf("large horizontal offset page = %#v", page)
	}
}

func TestReadRangeNeverReturnsStaleCachedBytesAfterDetectedSourceChange(t *testing.T) {
	path := filepath.Join(t.TempDir(), "source.txt")
	if err := os.WriteFile(path, []byte("old bytes"), 0o600); err != nil {
		t.Fatal(err)
	}
	doc, err := OpenFile(path)
	if err != nil {
		t.Fatal(err)
	}
	defer doc.Close()

	if got, err := doc.ReadRange(0, doc.Size()); err != nil || string(got) != "old bytes" {
		t.Fatalf("initial read = %q, %v", got, err)
	}
	if err := os.WriteFile(path, []byte("new bytes"), 0o600); err != nil {
		t.Fatal(err)
	}
	changedTime := doc.OriginalFileState().ModTime.Add(2 * time.Second)
	if err := os.Chtimes(path, changedTime, changedTime); err != nil {
		t.Fatal(err)
	}

	if got, err := doc.ReadRange(0, doc.Size()); !errors.Is(err, ErrSourceChanged) || got != nil {
		t.Fatalf("cached read after source change = %q, %v; want ErrSourceChanged and no bytes", got, err)
	}
	direct := make([]byte, doc.Size())
	if n, err := doc.ReadAt(direct, 0); err != nil || n != len(direct) || string(direct) != "new bytes" {
		t.Fatalf("direct read = %q (%d), %v", direct, n, err)
	}
	if doc.cache.bytes != 0 || len(doc.cache.chunks) != 0 {
		t.Fatalf("stale cache was not cleared: bytes=%d chunks=%d", doc.cache.bytes, len(doc.cache.chunks))
	}
}

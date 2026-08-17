package manualedit

import (
	"bytes"
	"context"
	"errors"
	"io"
	"testing"

	"github.com/quarry/quarry-wails3/internal/sourceio"
)

type memReaderAtSize struct {
	data []byte
}

type changedShortReaderAtSize struct{ size int64 }

func (r changedShortReaderAtSize) ReadAt([]byte, int64) (int, error) {
	return 0, sourceio.ErrSourceChanged
}

func (r changedShortReaderAtSize) Size() int64 { return r.size }

func TestPieceTableRangeEqualsPreservesShortReadCause(t *testing.T) {
	table := NewPieceTable(4)
	matches, err := table.rangeEquals(changedShortReaderAtSize{size: 4}, 0, 4, []byte("test"))
	if matches || !errors.Is(err, ErrSourceModifiedDuringOperation) || !errors.Is(err, sourceio.ErrSourceChanged) {
		t.Fatalf("range comparison = %t, %v; want both source-change causes", matches, err)
	}
}

func (r memReaderAtSize) ReadAt(p []byte, off int64) (int, error) {
	if off >= int64(len(r.data)) {
		return 0, io.EOF
	}
	n := copy(p, r.data[off:])
	if n < len(p) {
		return n, io.EOF
	}
	return n, nil
}

func (r memReaderAtSize) Size() int64 {
	return int64(len(r.data))
}

type nopSyncBuffer struct {
	bytes.Buffer
}

func (b *nopSyncBuffer) Sync() error {
	return nil
}

// shrunkReaderAtSize reports a larger Size() than it actually serves, simulating
// a source that was truncated/replaced after the piece table was built.
type shrunkReaderAtSize struct {
	data         []byte
	reportedSize int64
}

func (r shrunkReaderAtSize) ReadAt(p []byte, off int64) (int, error) {
	if off >= int64(len(r.data)) {
		return 0, io.EOF
	}
	n := copy(p, r.data[off:])
	if n < len(p) {
		return n, io.EOF
	}
	return n, nil
}

func (r shrunkReaderAtSize) Size() int64 { return r.reportedSize }

// WriteTo must fail loudly (not silently truncate) when an original piece reads
// short because the source shrank since staging.
func TestPieceTableWriteToFailsOnShortSource(t *testing.T) {
	full := []byte("alpha bravo charlie delta echo foxtrot")
	pt := NewPieceTable(int64(len(full)))
	// No edits: the whole file is one original piece of length len(full).
	src := shrunkReaderAtSize{data: full[:10], reportedSize: int64(len(full))}
	var out nopSyncBuffer
	_, err := pt.WriteTo(context.Background(), src, &out, WriteOptions{})
	if !errors.Is(err, ErrSourceModifiedDuringOperation) {
		t.Fatalf("err = %v, want ErrSourceModifiedDuringOperation", err)
	}
}

func TestPieceTableReadRangeFailsOnShortOriginalPieceWithoutInventingBytes(t *testing.T) {
	full := []byte("alpha bravo charlie delta")
	tests := []struct {
		name      string
		start     int64
		end       int64
		available int
	}{
		{name: "short at range start", start: 18, end: int64(len(full)), available: 10},
		{name: "short in range middle", start: 6, end: 20, available: 11},
		{name: "short at range end", start: 0, end: int64(len(full)), available: len(full) - 1},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pt := NewPieceTable(int64(len(full)))
			src := shrunkReaderAtSize{data: full[:tt.available], reportedSize: int64(len(full))}
			got, err := pt.ReadRange(src, tt.start, tt.end)
			if !errors.Is(err, ErrSourceModifiedDuringOperation) {
				t.Fatalf("err = %v, want ErrSourceModifiedDuringOperation", err)
			}
			if got != nil {
				t.Fatalf("returned bytes on source drift = %q; want nil and no synthesized NULs", got)
			}
		})
	}
}

type nilErrorShortReader struct {
	data         []byte
	reportedSize int64
}

func (r nilErrorShortReader) ReadAt(p []byte, off int64) (int, error) {
	if off >= int64(len(r.data)) {
		return 0, nil
	}
	return copy(p, r.data[off:]), nil
}

func (r nilErrorShortReader) Size() int64 { return r.reportedSize }

func TestPieceTableReadRangeRejectsShortReaderWithNilError(t *testing.T) {
	full := []byte("0123456789")
	pt := NewPieceTable(int64(len(full)))
	got, err := pt.ReadRange(nilErrorShortReader{data: full[:4], reportedSize: int64(len(full))}, 0, int64(len(full)))
	if !errors.Is(err, ErrSourceModifiedDuringOperation) {
		t.Fatalf("err = %v, want ErrSourceModifiedDuringOperation", err)
	}
	if got != nil {
		t.Fatalf("returned bytes on invalid partial ReaderAt = %q, want nil", got)
	}
}

func TestPieceTableReplaceWritesUpdatedBytes(t *testing.T) {
	src := []byte("alpha bravo charlie")
	pt := NewPieceTable(int64(len(src)))
	if err := pt.Replace(6, 11, []byte("delta")); err != nil {
		t.Fatal(err)
	}

	var out nopSyncBuffer
	written, err := pt.WriteTo(context.Background(), memReaderAtSize{data: src}, &out, WriteOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if written != int64(len("alpha delta charlie")) {
		t.Fatalf("written = %d", written)
	}
	if out.String() != "alpha delta charlie" {
		t.Fatalf("out = %q", out.String())
	}
}

func TestPieceTableInsertAtEnd(t *testing.T) {
	src := []byte("hello")
	pt := NewPieceTable(int64(len(src)))
	if err := pt.Replace(5, 5, []byte(" world")); err != nil {
		t.Fatal(err)
	}

	var out nopSyncBuffer
	if _, err := pt.WriteTo(context.Background(), memReaderAtSize{data: src}, &out, WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	if out.String() != "hello world" {
		t.Fatalf("out = %q", out.String())
	}
}

func TestPieceTableDeleteRange(t *testing.T) {
	src := []byte("one two three")
	pt := NewPieceTable(int64(len(src)))
	if err := pt.Replace(3, 8, nil); err != nil {
		t.Fatal(err)
	}

	var out nopSyncBuffer
	if _, err := pt.WriteTo(context.Background(), memReaderAtSize{data: src}, &out, WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	if out.String() != "onethree" {
		t.Fatalf("out = %q", out.String())
	}
}

func TestPieceTableRejectsOversizedInsertedText(t *testing.T) {
	pt := NewPieceTable(3)
	pt.SetMaxInsertedBytes(4)
	if err := pt.Replace(1, 1, []byte("abcdef")); !errors.Is(err, ErrInsertedTextTooLarge) {
		t.Fatalf("err = %v, want ErrInsertedTextTooLarge", err)
	}
}

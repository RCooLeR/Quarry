package manualedit

import (
	"bytes"
	"context"
	"errors"
	"io"
	"testing"
)

type memReaderAtSize struct {
	data []byte
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

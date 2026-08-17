package sourceio

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/quarry/quarry-wails3/internal/document"
)

func TestVerifiedDocumentReaderCachesVerifiedBlockForOverlappingReads(t *testing.T) {
	path := filepath.Join(t.TempDir(), "source.bin")
	original := make([]byte, 2*fingerprintChunkBytes)
	for i := range original {
		original[i] = byte(i % 251)
	}
	if err := os.WriteFile(path, original, 0o600); err != nil {
		t.Fatal(err)
	}
	doc, err := document.OpenFile(path)
	if err != nil {
		t.Fatal(err)
	}
	defer doc.Close()
	expected, err := ExpectDocumentContext(context.Background(), doc)
	if err != nil {
		t.Fatal(err)
	}
	reader, err := NewVerifiedDocumentReader(context.Background(), expected, doc)
	if err != nil {
		t.Fatal(err)
	}

	readAt := reader.readAt
	sum256 := reader.sum256
	readCalls := 0
	hashCalls := 0
	reader.readAt = func(dst []byte, offset int64) (int, error) {
		readCalls++
		return readAt(dst, offset)
	}
	reader.sum256 = func(src []byte) [sha256.Size]byte {
		hashCalls++
		return sum256(src)
	}

	assertRead := func(offset int64, length int) {
		t.Helper()
		got := make([]byte, length)
		n, err := reader.ReadAt(got, offset)
		if err != nil {
			t.Fatalf("ReadAt(%d, %d) error = %v", offset, length, err)
		}
		if n != length {
			t.Fatalf("ReadAt(%d, %d) read %d bytes", offset, length, n)
		}
		if want := original[offset : offset+int64(length)]; !bytes.Equal(got, want) {
			t.Fatalf("ReadAt(%d, %d) returned bytes outside the captured generation", offset, length)
		}
	}

	assertRead(37, 4096)
	assertRead(2048, 8192)
	assertRead(fingerprintChunkBytes-257, 257)
	if readCalls != 1 || hashCalls != 1 {
		t.Fatalf("one cached block used %d physical reads and %d hashes; want 1 and 1", readCalls, hashCalls)
	}
}

func TestVerifiedDocumentReaderRejectsMutationInNotYetCachedBlock(t *testing.T) {
	path := filepath.Join(t.TempDir(), "source.bin")
	original := bytes.Repeat([]byte{'a'}, 2*fingerprintChunkBytes)
	changed := append([]byte(nil), original...)
	changed[fingerprintChunkBytes+17] = 'b'
	if err := os.WriteFile(path, original, 0o600); err != nil {
		t.Fatal(err)
	}
	doc, err := document.OpenFile(path)
	if err != nil {
		t.Fatal(err)
	}
	defer doc.Close()
	expected, err := ExpectDocumentContext(context.Background(), doc)
	if err != nil {
		t.Fatal(err)
	}
	reader, err := NewVerifiedDocumentReader(context.Background(), expected, doc)
	if err != nil {
		t.Fatal(err)
	}

	first := make([]byte, 32)
	if n, err := reader.ReadAt(first, 0); n != len(first) || err != nil {
		t.Fatalf("first-block read = %d, %v", n, err)
	}
	if !bytes.Equal(first, original[:len(first)]) {
		t.Fatal("first cached block did not return the captured generation")
	}
	if err := os.WriteFile(path, changed, 0o600); err != nil {
		t.Fatal(err)
	}

	second := make([]byte, 32)
	n, err := reader.ReadAt(second, fingerprintChunkBytes)
	if n != 0 || !errors.Is(err, ErrSourceChanged) {
		t.Fatalf("not-yet-cached changed block read = %d, %v; want no bytes and ErrSourceChanged", n, err)
	}
	if !bytes.Equal(second, make([]byte, len(second))) {
		t.Fatalf("unverified bytes escaped to caller: %q", second)
	}
}

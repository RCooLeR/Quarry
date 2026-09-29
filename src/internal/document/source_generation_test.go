package document

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestOpenedDocumentRejectsUnexpectedShortReadsAndChangedGeneration(t *testing.T) {
	path := filepath.Join(t.TempDir(), "source.txt")
	if err := os.WriteFile(path, []byte("original bytes"), 0o600); err != nil {
		t.Fatal(err)
	}
	doc, err := OpenFile(path)
	if err != nil {
		t.Fatal(err)
	}
	defer doc.Close()

	if err := os.Truncate(path, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := doc.ReadRange(0, doc.Size()); !errors.Is(err, ErrSourceChanged) {
		t.Fatalf("short read error = %v, want ErrSourceChanged", err)
	}
	if err := doc.ValidateUnchanged(); !errors.Is(err, ErrSourceChanged) {
		t.Fatalf("generation error = %v, want ErrSourceChanged", err)
	}
}

func TestOpenedDocumentRejectsSameSizeRewriteWithRestoredMtime(t *testing.T) {
	path := filepath.Join(t.TempDir(), "source.txt")
	original := []byte("original bytes")
	changed := []byte("modified bytes")
	if len(original) != len(changed) {
		t.Fatal("fixture must preserve source size")
	}
	if err := os.WriteFile(path, original, 0o600); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	doc, err := OpenFile(path)
	if err != nil {
		t.Fatal(err)
	}
	defer doc.Close()
	if !doc.HasMutationGeneration() {
		t.Skip("filesystem does not expose a strong source mutation generation")
	}

	if err := os.WriteFile(path, changed, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, info.ModTime(), info.ModTime()); err != nil {
		t.Fatal(err)
	}
	if err := doc.ValidateUnchanged(); !errors.Is(err, ErrSourceChanged) {
		t.Fatalf("generation error = %v, want ErrSourceChanged", err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, changed) {
		t.Fatalf("validation changed source: got %q, want %q", got, changed)
	}
	current, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if !current.ModTime().Equal(info.ModTime()) {
		t.Fatalf("fixture did not restore mtime: got %s, want %s", current.ModTime().Format(time.RFC3339Nano), info.ModTime().Format(time.RFC3339Nano))
	}
}

func TestValidatedReadRejectsRewriteAfterPhysicalRead(t *testing.T) {
	path := filepath.Join(t.TempDir(), "source.txt")
	original := []byte("original bytes")
	changed := []byte("modified bytes")
	if err := os.WriteFile(path, original, 0o600); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	doc, err := OpenFile(path)
	if err != nil {
		t.Fatal(err)
	}
	defer doc.Close()
	if !doc.HasMutationGeneration() {
		t.Skip("filesystem does not expose a strong source mutation generation")
	}

	var mutationErr error
	buffer := make([]byte, len(original))
	n, err := doc.readAtValidated(buffer, 0, func() {
		mutationErr = os.WriteFile(path, changed, 0o600)
		if mutationErr == nil {
			mutationErr = os.Chtimes(path, info.ModTime(), info.ModTime())
		}
	})
	if mutationErr != nil {
		t.Fatalf("rewrite at validated-read seam: %v", mutationErr)
	}
	if !errors.Is(err, ErrSourceChanged) {
		t.Fatalf("validated read error = %v, want ErrSourceChanged", err)
	}
	if n != 0 {
		t.Fatalf("validated read exposed %d bytes from a changed generation", n)
	}
}

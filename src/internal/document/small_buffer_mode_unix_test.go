//go:build linux || darwin

package document

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

func TestInMemoryBufferWriteCopyFallsBackToPrivateModeWhenSourcePathDisappears(t *testing.T) {
	previousUmask := syscall.Umask(0)
	t.Cleanup(func() { syscall.Umask(previousUmask) })

	dir := t.TempDir()
	sourcePath := filepath.Join(dir, "source.txt")
	if err := os.WriteFile(sourcePath, []byte("original"), 0o644); err != nil {
		t.Fatal(err)
	}
	doc, err := OpenFile(sourcePath)
	if err != nil {
		t.Fatal(err)
	}
	buffer, err := LoadInMemoryBuffer(doc, 1024)
	if err != nil {
		_ = doc.Close()
		t.Fatal(err)
	}
	if err := doc.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(sourcePath); err != nil {
		t.Fatal(err)
	}

	outputPath := filepath.Join(dir, "copy.txt")
	if _, err := buffer.WriteCopy(outputPath, "edited"); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(outputPath)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("mode = %03o, want private fallback 600", got)
	}
}

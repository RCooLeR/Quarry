//go:build !windows

package document

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestInMemoryBufferWriteCopyPreservesTrailingSpaceInSelectedPath(t *testing.T) {
	dir := t.TempDir()
	sourcePath := filepath.Join(dir, "source.txt")
	if err := os.WriteFile(sourcePath, []byte("original"), 0o600); err != nil {
		t.Fatal(err)
	}
	buffer := &InMemoryBuffer{
		Path:          sourcePath,
		OriginalSize:  8,
		WindowStart:   0,
		WindowEnd:     8,
		EditableLimit: 8,
		Text:          "original",
		Encoding:      "UTF-8",
	}
	selected := filepath.Join(dir, "copy.txt ")
	if _, err := buffer.WriteCopyContext(context.Background(), selected, "changed"); err != nil {
		t.Fatal(err)
	}
	if got, err := os.ReadFile(selected); err != nil || string(got) != "changed" {
		t.Fatalf("selected output = %q, %v", got, err)
	}
	if _, err := os.Stat(filepath.Join(dir, "copy.txt")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("trimmed sibling unexpectedly exists: %v", err)
	}
}

//go:build windows

package document

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestOpenFileWindowsRejectsDirectory(t *testing.T) {
	if _, err := OpenFile(t.TempDir()); !errors.Is(err, ErrNonRegularSource) {
		t.Fatalf("OpenFile(directory) error = %v, want ErrNonRegularSource", err)
	}
}

func TestOpenRegularSourceWindowsRejectsCharacterDevice(t *testing.T) {
	if file, err := openRegularSource("NUL"); !errors.Is(err, ErrNonRegularSource) {
		if file != nil {
			_ = file.Close()
		}
		t.Fatalf("openRegularSource(NUL) error = %v, want ErrNonRegularSource", err)
	}
}

func TestOpenFileWindowsAcceptsRegularFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "regular.txt")
	if err := os.WriteFile(path, []byte("regular source\r\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	doc, err := OpenFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := doc.Close(); err != nil {
		t.Fatal(err)
	}
}

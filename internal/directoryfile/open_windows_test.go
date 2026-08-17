//go:build windows

package directoryfile

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestOpenWindowsAcceptsDirectory(t *testing.T) {
	file, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestOpenWindowsRejectsRegularFileAndDevice(t *testing.T) {
	regular := filepath.Join(t.TempDir(), "file.txt")
	if err := os.WriteFile(regular, []byte("file"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{regular, "NUL"} {
		file, err := Open(path)
		if file != nil {
			_ = file.Close()
		}
		if !errors.Is(err, ErrNotDirectory) {
			t.Fatalf("Open(%q) error = %v, want ErrNotDirectory", path, err)
		}
	}
}

func TestOpenWindowsRejectsDirectoryExchangedAfterPreflight(t *testing.T) {
	parent := t.TempDir()
	path := filepath.Join(parent, "directory")
	held := filepath.Join(parent, "directory.held")
	if err := os.Mkdir(path, 0o700); err != nil {
		t.Fatal(err)
	}
	file, err := openDirectory(path, false, func() error {
		if err := os.Rename(path, held); err != nil {
			return err
		}
		return os.Mkdir(path, 0o700)
	})
	if file != nil {
		_ = file.Close()
	}
	if !errors.Is(err, ErrPathChanged) {
		t.Fatalf("open exchanged directory error = %v, want ErrPathChanged", err)
	}
}

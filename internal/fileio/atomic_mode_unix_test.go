//go:build linux

package fileio

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

func TestWriteFileAtomicDefaultModeIsPrivateUnderPermissiveUmask(t *testing.T) {
	previousUmask := syscall.Umask(0)
	t.Cleanup(func() { syscall.Umask(previousUmask) })

	path := filepath.Join(t.TempDir(), "private-output.txt")
	if _, err := WriteFileAtomic(path, []byte("sensitive"), AtomicWriteOptions{}); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("mode = %03o, want private 600", got)
	}
}

func TestWriteFileAtomicOverwritePreservesModeUnderRestrictiveUmask(t *testing.T) {
	path := filepath.Join(t.TempDir(), "shared-output.txt")
	if err := os.WriteFile(path, []byte("old"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o755); err != nil {
		t.Fatal(err)
	}

	previousUmask := syscall.Umask(0o077)
	t.Cleanup(func() { syscall.Umask(previousUmask) })

	if _, err := WriteFileAtomic(path, []byte("new"), AtomicWriteOptions{Overwrite: true}); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o755 {
		t.Fatalf("mode = %03o, want preserved 755", got)
	}
}

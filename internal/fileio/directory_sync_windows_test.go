//go:build windows

package fileio

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/quarry/quarry-wails3/internal/directoryfile"
	"golang.org/x/sys/windows"
)

func TestNormalizeDirectorySyncErrorSuppressesOnlyInvalidHandle(t *testing.T) {
	if err := normalizeDirectorySyncError(windows.ERROR_INVALID_HANDLE); err != nil {
		t.Fatalf("ERROR_INVALID_HANDLE was not treated as unsupported directory sync: %v", err)
	}
	wrapped := &os.PathError{Op: "sync", Path: `C:\\data`, Err: windows.ERROR_INVALID_HANDLE}
	if err := normalizeDirectorySyncError(wrapped); err != nil {
		t.Fatalf("wrapped ERROR_INVALID_HANDLE was not treated as unsupported directory sync: %v", err)
	}

	for _, want := range []error{
		windows.ERROR_ACCESS_DENIED,
		windows.ERROR_WRITE_FAULT,
		windows.ERROR_CRC,
		errors.New("filesystem finalization failed"),
	} {
		if got := normalizeDirectorySyncError(want); !errors.Is(got, want) {
			t.Errorf("normalizeDirectorySyncError(%v) = %v; want preserved failure", want, got)
		}
	}
}

func TestSyncDirectoryPathWindowsRejectsRegularFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "not-a-directory.txt")
	if err := os.WriteFile(path, []byte("file"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := syncDirectoryPath(path); !errors.Is(err, directoryfile.ErrNotDirectory) {
		t.Fatalf("syncDirectoryPath(regular file) error = %v, want ErrNotDirectory", err)
	}
}

func TestSyncDirectoryPathWindowsAcceptsDirectory(t *testing.T) {
	if err := syncDirectoryPath(t.TempDir()); err != nil {
		t.Fatalf("syncDirectoryPath(directory) error = %v", err)
	}
}

func TestSyncAtomicOutputDirectoryPreservesRealFlushFailure(t *testing.T) {
	original := flushAtomicOutputDirectory
	t.Cleanup(func() { flushAtomicOutputDirectory = original })

	flushAtomicOutputDirectory = func(windows.Handle) error {
		return fmt.Errorf("simulated device flush: %w", windows.ERROR_WRITE_FAULT)
	}
	if err := syncAtomicOutputDirectory(windows.InvalidHandle); !errors.Is(err, windows.ERROR_WRITE_FAULT) {
		t.Fatalf("sync error = %v, want ERROR_WRITE_FAULT", err)
	}

	flushAtomicOutputDirectory = func(windows.Handle) error {
		return fmt.Errorf("unsupported directory flush: %w", windows.ERROR_INVALID_HANDLE)
	}
	if err := syncAtomicOutputDirectory(windows.InvalidHandle); err != nil {
		t.Fatalf("unsupported directory flush error = %v, want nil", err)
	}
}

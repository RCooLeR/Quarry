//go:build windows

package document

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/quarry/quarry-wails3/internal/directoryfile"
	"golang.org/x/sys/windows"
)

func TestNormalizeIndexCacheDirectorySyncErrorSuppressesOnlyInvalidHandle(t *testing.T) {
	if err := normalizeIndexCacheDirectorySyncError(windows.ERROR_INVALID_HANDLE); err != nil {
		t.Fatalf("ERROR_INVALID_HANDLE was not treated as unsupported directory sync: %v", err)
	}
	wrapped := &os.PathError{Op: "sync", Path: `C:\\cache`, Err: windows.ERROR_INVALID_HANDLE}
	if err := normalizeIndexCacheDirectorySyncError(wrapped); err != nil {
		t.Fatalf("wrapped ERROR_INVALID_HANDLE was not treated as unsupported directory sync: %v", err)
	}

	for _, want := range []error{
		windows.ERROR_ACCESS_DENIED,
		windows.ERROR_WRITE_FAULT,
		windows.ERROR_CRC,
		errors.New("cache directory finalization failed"),
	} {
		got := normalizeIndexCacheDirectorySyncError(fmt.Errorf("sync cache directory: %w", want))
		if !errors.Is(got, want) {
			t.Errorf("normalizeIndexCacheDirectorySyncError(%v) = %v; want preserved failure", want, got)
		}
	}
}

func TestSyncIndexCacheDirectoryWindowsRejectsRegularFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "not-a-directory.txt")
	if err := os.WriteFile(path, []byte("file"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := syncIndexCacheDirectory(path); !errors.Is(err, directoryfile.ErrNotDirectory) {
		t.Fatalf("syncIndexCacheDirectory(regular file) error = %v, want ErrNotDirectory", err)
	}
}

func TestSyncIndexCacheDirectoryWindowsAcceptsDirectory(t *testing.T) {
	if err := syncIndexCacheDirectory(t.TempDir()); err != nil {
		t.Fatalf("syncIndexCacheDirectory(directory) error = %v", err)
	}
}

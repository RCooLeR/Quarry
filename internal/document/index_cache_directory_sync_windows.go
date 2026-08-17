//go:build windows

package document

import (
	"errors"

	"github.com/quarry/quarry-wails3/internal/directoryfile"

	"golang.org/x/sys/windows"
)

// normalizeIndexCacheDirectorySyncError treats only the unsupported-directory
// handle result as best effort. Access, media, and I/O failures must reach the
// cache publisher instead of being silently erased.
func normalizeIndexCacheDirectorySyncError(err error) error {
	if errors.Is(err, windows.ERROR_INVALID_HANDLE) {
		return nil
	}
	return err
}

func syncIndexCacheDirectory(path string) error {
	directory, err := directoryfile.OpenForSync(path)
	if err != nil {
		return err
	}
	return errors.Join(
		normalizeIndexCacheDirectorySyncError(directory.Sync()),
		directory.Close(),
	)
}

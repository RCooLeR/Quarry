//go:build windows

package fileio

import (
	"errors"

	"github.com/quarry/quarry-wails3/internal/directoryfile"

	"golang.org/x/sys/windows"
)

// normalizeDirectorySyncError keeps Windows directory syncing best-effort only
// for the exact unsupported-handle result returned by FlushFileBuffers. Any
// other failure may describe real lost durability and must reach the caller.
func normalizeDirectorySyncError(err error) error {
	if errors.Is(err, windows.ERROR_INVALID_HANDLE) {
		return nil
	}
	return err
}

func syncDirectoryPath(path string) error {
	directory, err := directoryfile.OpenForSync(path)
	if err != nil {
		return err
	}
	return errors.Join(
		normalizeDirectorySyncError(directory.Sync()),
		directory.Close(),
	)
}

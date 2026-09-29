//go:build windows

package fileio

import (
	"errors"
	"os"

	"golang.org/x/sys/windows"
)

func publishExistingNoClobber(tempPath string, finalPath string) error {
	tempName, err := windows.UTF16PtrFromString(tempPath)
	if err != nil {
		return err
	}
	finalName, err := windows.UTF16PtrFromString(finalPath)
	if err != nil {
		return err
	}

	// Deliberately omit MOVEFILE_REPLACE_EXISTING. WRITE_THROUGH makes the
	// move itself synchronous; the shared caller also flushes the parent.
	err = windows.MoveFileEx(tempName, finalName, windows.MOVEFILE_WRITE_THROUGH)
	if err == nil {
		return nil
	}
	if errors.Is(err, windows.ERROR_FILE_EXISTS) || errors.Is(err, windows.ERROR_ALREADY_EXISTS) {
		return existingOutputError(finalPath)
	}
	if _, statErr := os.Lstat(finalPath); statErr == nil {
		return existingOutputError(finalPath)
	}
	return err
}

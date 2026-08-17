//go:build darwin

package fileio

import (
	"errors"
	"fmt"
	"os"

	"golang.org/x/sys/unix"
)

func publishExistingNoClobber(tempPath string, finalPath string) error {
	err := unix.RenamexNp(tempPath, finalPath, unix.RENAME_EXCL)
	if err == nil {
		return nil
	}
	if errors.Is(err, unix.EEXIST) {
		return existingOutputError(finalPath)
	}
	if errors.Is(err, unix.ENOSYS) || errors.Is(err, unix.EINVAL) || errors.Is(err, unix.ENOTSUP) {
		return fmt.Errorf("%w: %v", ErrAtomicNoClobberUnavailable, err)
	}
	if _, statErr := os.Lstat(finalPath); statErr == nil {
		return existingOutputError(finalPath)
	}
	return err
}

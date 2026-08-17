//go:build !windows

package fileio

import (
	"errors"

	"github.com/quarry/quarry-wails3/internal/directoryfile"
)

func syncDirectoryPath(path string) error {
	directory, err := directoryfile.OpenForSync(path)
	if err != nil {
		return err
	}
	return errors.Join(directory.Sync(), directory.Close())
}

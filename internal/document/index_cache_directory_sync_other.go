//go:build !windows

package document

import (
	"errors"

	"github.com/quarry/quarry-wails3/internal/directoryfile"
)

func syncIndexCacheDirectory(path string) error {
	directory, err := directoryfile.OpenForSync(path)
	if err != nil {
		return err
	}
	return errors.Join(directory.Sync(), directory.Close())
}

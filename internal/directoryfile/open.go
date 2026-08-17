// Package directoryfile opens untrusted directory pathnames without allowing
// a FIFO or another non-directory substitution to block or be admitted.
package directoryfile

import (
	"errors"
	"fmt"
	"os"
)

var (
	ErrNotDirectory = errors.New("path does not identify a directory")
	ErrPathChanged  = errors.New("directory path changed while opening")
)

// Open returns a stable read-only directory handle suitable for bounded scans.
func Open(path string) (*os.File, error) {
	return openDirectory(path, false, nil)
}

// OpenForSync returns a stable directory handle with the platform access
// needed to request namespace durability.
func OpenForSync(path string) (*os.File, error) {
	return openDirectory(path, true, nil)
}

func validateDirectory(file *os.File, path string) (os.FileInfo, error) {
	if file == nil {
		return nil, &os.PathError{Op: "open", Path: path, Err: os.ErrInvalid}
	}
	info, err := file.Stat()
	if err != nil {
		return nil, &os.PathError{Op: "stat", Path: path, Err: err}
	}
	if !info.IsDir() {
		return nil, &os.PathError{Op: "open", Path: path, Err: ErrNotDirectory}
	}
	return info, nil
}

func validateDirectoryPreflight(path string, info os.FileInfo) error {
	if info == nil || !info.IsDir() {
		return &os.PathError{Op: "open", Path: path, Err: ErrNotDirectory}
	}
	// Windows FileInfo values resolve file IDs lazily. Force that resolution
	// before a deterministic or real stat/open race can exchange the pathname.
	if !os.SameFile(info, info) {
		return &os.PathError{Op: "stat", Path: path, Err: ErrPathChanged}
	}
	return nil
}

func closeAfterError(file *os.File, err error) error {
	if file == nil {
		return err
	}
	if closeErr := file.Close(); closeErr != nil {
		return errors.Join(err, fmt.Errorf("close rejected directory handle: %w", closeErr))
	}
	return err
}

package document

import (
	"errors"
	"fmt"
	"os"
)

// ErrNonRegularSource is returned when a path resolves to a directory, pipe,
// device, socket, or another non-regular object. Quarry's document machinery
// relies on stable positional reads and a finite size, so these objects cannot
// safely be treated as editor sources.
var ErrNonRegularSource = errors.New("document source is not a regular file")

func validateRegularSourceFile(file *os.File, path string) (os.FileInfo, error) {
	if file == nil {
		return nil, errors.New("opened document source is required")
	}
	info, err := file.Stat()
	if err != nil {
		return nil, &os.PathError{Op: "stat", Path: path, Err: err}
	}
	if !info.Mode().IsRegular() {
		return nil, &os.PathError{Op: "open", Path: path, Err: ErrNonRegularSource}
	}
	return info, nil
}

func closeSourceAfterError(file *os.File, err error) error {
	if file == nil {
		return err
	}
	if closeErr := file.Close(); closeErr != nil {
		return errors.Join(err, fmt.Errorf("close rejected document source: %w", closeErr))
	}
	return err
}

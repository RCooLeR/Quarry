//go:build !windows

package directoryfile

import (
	"errors"
	"os"

	"golang.org/x/sys/unix"
)

func classifyOpenFailure(path string, preflight os.FileInfo, openErr error) error {
	current, statErr := os.Stat(path)
	if statErr != nil || current == nil || !current.IsDir() || !os.SameFile(preflight, current) {
		return &os.PathError{Op: "open", Path: path, Err: errors.Join(ErrPathChanged, openErr, statErr)}
	}
	return &os.PathError{Op: "open", Path: path, Err: openErr}
}

// openDirectory uses both O_DIRECTORY and O_NONBLOCK. O_DIRECTORY alone is
// not sufficient on every supported Unix path: opening a FIFO can still wait
// for a writer before the type error reaches the caller.
func openDirectory(path string, _ bool, afterPreflight func() error) (*os.File, error) {
	preflight, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	if err := validateDirectoryPreflight(path, preflight); err != nil {
		return nil, err
	}
	if afterPreflight != nil {
		if err := afterPreflight(); err != nil {
			return nil, err
		}
	}

	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NONBLOCK|unix.O_DIRECTORY, 0)
	if err != nil {
		return nil, classifyOpenFailure(path, preflight, err)
	}
	file := os.NewFile(uintptr(fd), path)
	if file == nil {
		_ = unix.Close(fd)
		return nil, &os.PathError{Op: "open", Path: path, Err: os.ErrInvalid}
	}
	opened, err := validateDirectory(file, path)
	if err != nil {
		return nil, closeAfterError(file, err)
	}
	if !os.SameFile(preflight, opened) {
		return nil, closeAfterError(file, &os.PathError{Op: "open", Path: path, Err: ErrPathChanged})
	}
	current, err := os.Stat(path)
	if err != nil {
		return nil, closeAfterError(file, &os.PathError{Op: "stat", Path: path, Err: err})
	}
	if !current.IsDir() || !os.SameFile(opened, current) {
		return nil, closeAfterError(file, &os.PathError{Op: "open", Path: path, Err: ErrPathChanged})
	}

	flags, err := unix.FcntlInt(file.Fd(), unix.F_GETFL, 0)
	if err != nil {
		return nil, closeAfterError(file, &os.PathError{Op: "fcntl", Path: path, Err: err})
	}
	if flags&unix.O_NONBLOCK != 0 {
		if _, err := unix.FcntlInt(file.Fd(), unix.F_SETFL, flags&^unix.O_NONBLOCK); err != nil {
			return nil, closeAfterError(file, &os.PathError{Op: "fcntl", Path: path, Err: err})
		}
	}
	return file, nil
}

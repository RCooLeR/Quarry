//go:build !windows

package document

import (
	"os"

	"golang.org/x/sys/unix"
)

// openRegularSource opens with O_NONBLOCK first. This is important even after
// a pathname preflight: a regular file can be exchanged for a FIFO between the
// check and open. The opened handle is authoritative, and O_NONBLOCK is removed
// only after that handle has been proven to be a regular file.
func openRegularSource(path string) (*os.File, error) {
	if info, err := os.Stat(path); err != nil {
		return nil, err
	} else if !info.Mode().IsRegular() {
		return nil, &os.PathError{Op: "open", Path: path, Err: ErrNonRegularSource}
	}

	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NONBLOCK, 0)
	if err != nil {
		return nil, &os.PathError{Op: "open", Path: path, Err: err}
	}
	file := os.NewFile(uintptr(fd), path)
	if file == nil {
		_ = unix.Close(fd)
		return nil, &os.PathError{Op: "open", Path: path, Err: os.ErrInvalid}
	}
	if _, err := validateRegularSourceFile(file, path); err != nil {
		return nil, closeSourceAfterError(file, err)
	}

	flags, err := unix.FcntlInt(file.Fd(), unix.F_GETFL, 0)
	if err != nil {
		return nil, closeSourceAfterError(file, &os.PathError{Op: "fcntl", Path: path, Err: err})
	}
	if flags&unix.O_NONBLOCK != 0 {
		if _, err := unix.FcntlInt(file.Fd(), unix.F_SETFL, flags&^unix.O_NONBLOCK); err != nil {
			return nil, closeSourceAfterError(file, &os.PathError{Op: "fcntl", Path: path, Err: err})
		}
	}
	return file, nil
}

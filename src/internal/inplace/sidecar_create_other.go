//go:build !windows

package inplace

import (
	"os"

	"golang.org/x/sys/unix"
)

func openExclusiveSidecar(path string) (*os.File, error) {
	// POSIX requires O_CREAT|O_EXCL to fail when the final component already
	// exists, including when it is a symbolic link. It therefore never follows
	// or truncates an existing sidecar alias.
	fd, err := unix.Open(path, unix.O_RDWR|unix.O_CREAT|unix.O_EXCL|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0o600)
	if err != nil {
		return nil, &os.PathError{Op: "open", Path: path, Err: err}
	}
	return os.NewFile(uintptr(fd), path), nil
}

func openRecoverySidecar(path string) (*os.File, error) {
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
	if err != nil {
		return nil, &os.PathError{Op: "open", Path: path, Err: err}
	}
	return os.NewFile(uintptr(fd), path), nil
}

func openSourceNoFollow(path string, writable bool) (*os.File, error) {
	flags := unix.O_RDONLY | unix.O_CLOEXEC | unix.O_NOFOLLOW | unix.O_NONBLOCK
	if writable {
		flags = unix.O_RDWR | unix.O_CLOEXEC | unix.O_NOFOLLOW | unix.O_NONBLOCK
	}
	fd, err := unix.Open(path, flags, 0)
	if err != nil {
		return nil, &os.PathError{Op: "open", Path: path, Err: err}
	}
	return os.NewFile(uintptr(fd), path), nil
}

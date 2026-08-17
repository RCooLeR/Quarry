//go:build !windows

package logger

import (
	"errors"
	"fmt"
	"os"

	"golang.org/x/sys/unix"
)

func openLogForRead(path string) (*os.File, error) {
	return openLogNoFollow(path, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
}

func openLogForAppend(path string) (*os.File, error) {
	return openLogNoFollow(path, unix.O_CREAT|unix.O_RDWR|unix.O_APPEND|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0o600)
}

func openLogNoFollow(path string, flags int, mode uint32) (*os.File, error) {
	fd, err := unix.Open(path, flags, mode)
	if err != nil {
		if errors.Is(err, unix.ELOOP) {
			return nil, fmt.Errorf("%w %q: log must not be a symbolic link", ErrUnsafeLogPath, path)
		}
		return nil, &os.PathError{Op: "open", Path: path, Err: err}
	}
	var stat unix.Stat_t
	if err := unix.Fstat(fd, &stat); err != nil {
		_ = unix.Close(fd)
		return nil, &os.PathError{Op: "stat", Path: path, Err: err}
	}
	if stat.Nlink != 1 {
		_ = unix.Close(fd)
		return nil, fmt.Errorf("%w %q: log must have exactly one filesystem link", ErrUnsafeLogPath, path)
	}
	file := os.NewFile(uintptr(fd), path)
	if file == nil {
		_ = unix.Close(fd)
		return nil, fmt.Errorf("%w %q: create retained file handle", ErrUnsafeLogPath, path)
	}
	info, err := file.Stat()
	if err != nil {
		_ = file.Close()
		return nil, err
	}
	if !info.Mode().IsRegular() {
		_ = file.Close()
		return nil, fmt.Errorf("%w %q: log must be a regular file", ErrUnsafeLogPath, path)
	}
	return file, nil
}

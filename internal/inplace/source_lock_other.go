//go:build !windows

package inplace

import (
	"os"

	"golang.org/x/sys/unix"
)

func lockSourceFile(f *os.File) error {
	return unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB)
}

func unlockSourceFile(f *os.File) error {
	return unix.Flock(int(f.Fd()), unix.LOCK_UN)
}

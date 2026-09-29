//go:build windows

package inplace

import (
	"os"

	"golang.org/x/sys/windows"
)

func lockSourceFile(f *os.File) error {
	var overlapped windows.Overlapped
	return windows.LockFileEx(
		windows.Handle(f.Fd()),
		windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY,
		0,
		mathMaxUint32,
		mathMaxUint32,
		&overlapped,
	)
}

func unlockSourceFile(f *os.File) error {
	var overlapped windows.Overlapped
	return windows.UnlockFileEx(
		windows.Handle(f.Fd()),
		0,
		mathMaxUint32,
		mathMaxUint32,
		&overlapped,
	)
}

const mathMaxUint32 = ^uint32(0)

//go:build windows

package document

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/quarry/quarry-wails3/internal/winacl"
	"golang.org/x/sys/windows"
)

func acquireIndexCacheDirectoryLock(dir string) (func() error, error) {
	path := filepath.Join(dir, ".quarry-index.lock")
	handle, err := winacl.OpenFile(
		path,
		windows.GENERIC_READ|windows.GENERIC_WRITE,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE,
		windows.OPEN_ALWAYS,
		windows.FILE_ATTRIBUTE_NORMAL|windows.FILE_FLAG_OPEN_REPARSE_POINT,
	)
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(handle), path)
	if file == nil {
		_ = windows.CloseHandle(handle)
		return nil, errors.New("create private Windows index-cache lock handle")
	}
	entryInfo, lstatErr := os.Lstat(path)
	openedInfo, statErr := file.Stat()
	if lstatErr != nil || statErr != nil || !entryInfo.Mode().IsRegular() || !openedInfo.Mode().IsRegular() || !os.SameFile(entryInfo, openedInfo) {
		_ = file.Close()
		return nil, errors.New("line-index cache lock is not an owned regular file")
	}
	overlapped := new(windows.Overlapped)
	flags := uint32(windows.LOCKFILE_EXCLUSIVE_LOCK | windows.LOCKFILE_FAIL_IMMEDIATELY)
	if err := windows.LockFileEx(windows.Handle(file.Fd()), flags, 0, 1, 0, overlapped); err != nil {
		_ = file.Close()
		return nil, fmt.Errorf("%w: %v", ErrIndexCacheBusy, err)
	}
	return func() error {
		return errors.Join(windows.UnlockFileEx(windows.Handle(file.Fd()), 0, 1, 0, overlapped), file.Close())
	}, nil
}

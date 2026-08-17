//go:build windows

package document

import (
	"errors"
	"os"

	"github.com/quarry/quarry-wails3/internal/winacl"
	"golang.org/x/sys/windows"
)

func createIndexCacheTempFile(path string) (*os.File, error) {
	handle, err := winacl.OpenFile(
		path,
		windows.GENERIC_WRITE,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE,
		windows.CREATE_NEW,
		windows.FILE_ATTRIBUTE_NORMAL|windows.FILE_FLAG_OPEN_REPARSE_POINT,
	)
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(handle), path)
	if file == nil {
		_ = windows.CloseHandle(handle)
		return nil, errors.New("create private Windows index-cache temp handle")
	}
	return file, nil
}

//go:build windows

package fileio

import (
	"errors"
	"fmt"
	"os"

	"github.com/quarry/quarry-wails3/internal/winacl"
	"golang.org/x/sys/windows"
)

// Windows does not apply Unix mode bits as an access-control policy. Quarry
// therefore creates every operation-owned output with a protected DACL that
// grants full control only to the effective user and LocalSystem. Supplying the
// descriptor to the create call is important: tightening an inherited DACL
// after creation would leave a disclosure window in a shared directory.
func privateOutputSecurityDescriptor() (*windows.SECURITY_DESCRIPTOR, error) {
	return winacl.Descriptor()
}

// Install the same create-time ACL policy behind the existing package-local
// open seam. Its production callers create only exclusive, operation-owned
// temp/final files; tests can still replace openPath deterministically.
func init() {
	openPath = openPrivateOutputPath
}

func openPrivateOutputPath(path string, flag int, perm os.FileMode) (*os.File, error) {
	if flag&os.O_CREATE == 0 {
		return os.OpenFile(path, flag, perm)
	}
	const supported = os.O_WRONLY | os.O_CREATE | os.O_EXCL
	if flag != supported {
		return nil, fmt.Errorf("private Windows output creation requires exclusive write-only creation (flags %#x)", flag)
	}
	attributes := uint32(windows.FILE_ATTRIBUTE_NORMAL | windows.FILE_FLAG_OPEN_REPARSE_POINT)
	if perm.Perm()&0o200 == 0 {
		attributes |= windows.FILE_ATTRIBUTE_READONLY
	}
	handle, err := winacl.OpenFile(
		path,
		windows.GENERIC_WRITE,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE,
		windows.CREATE_NEW,
		attributes,
	)
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(handle), path)
	if file == nil {
		_ = windows.CloseHandle(handle)
		return nil, errors.New("create private Windows output file handle")
	}
	return file, nil
}

func requirePrivateOutputACLVolume(handle windows.Handle) error {
	return winacl.RequireVolume(handle)
}

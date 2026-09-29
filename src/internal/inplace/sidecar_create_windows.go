//go:build windows

package inplace

import (
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/windows"
)

func openExclusiveSidecar(path string) (*os.File, error) {
	createPath, err := sidecarCreatePath(path)
	if err != nil {
		return nil, &os.PathError{Op: "open", Path: path, Err: err}
	}
	name, err := windows.UTF16PtrFromString(createPath)
	if err != nil {
		return nil, &os.PathError{Op: "open", Path: path, Err: err}
	}
	handle, err := windows.CreateFile(
		name,
		windows.GENERIC_READ|windows.GENERIC_WRITE,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_DELETE,
		nil,
		windows.CREATE_NEW,
		windows.FILE_ATTRIBUTE_NORMAL|windows.FILE_FLAG_OPEN_REPARSE_POINT,
		0,
	)
	if err != nil {
		return nil, &os.PathError{Op: "open", Path: path, Err: err}
	}
	// FILE_SHARE_DELETE lets Apply unlink the identity-checked sidecar while
	// retaining this exact handle, closing the pathname-substitution window that
	// would otherwise be required by Windows delete semantics.
	return os.NewFile(uintptr(handle), path), nil
}

func openRecoverySidecar(path string) (*os.File, error) {
	openPath, err := sidecarCreatePath(path)
	if err != nil {
		return nil, &os.PathError{Op: "open", Path: path, Err: err}
	}
	name, err := windows.UTF16PtrFromString(openPath)
	if err != nil {
		return nil, &os.PathError{Op: "open", Path: path, Err: err}
	}
	handle, err := windows.CreateFile(
		name,
		windows.GENERIC_READ,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_DELETE,
		nil,
		windows.OPEN_EXISTING,
		windows.FILE_ATTRIBUTE_NORMAL|windows.FILE_FLAG_OPEN_REPARSE_POINT,
		0,
	)
	if err != nil {
		return nil, &os.PathError{Op: "open", Path: path, Err: err}
	}
	return os.NewFile(uintptr(handle), path), nil
}

func openSourceNoFollow(path string, writable bool) (*os.File, error) {
	openPath, err := sidecarCreatePath(path)
	if err != nil {
		return nil, &os.PathError{Op: "open", Path: path, Err: err}
	}
	name, err := windows.UTF16PtrFromString(openPath)
	if err != nil {
		return nil, &os.PathError{Op: "open", Path: path, Err: err}
	}
	access := uint32(windows.GENERIC_READ)
	if writable {
		access |= windows.GENERIC_WRITE
	}
	handle, err := windows.CreateFile(
		name,
		access,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil,
		windows.OPEN_EXISTING,
		windows.FILE_ATTRIBUTE_NORMAL|windows.FILE_FLAG_OPEN_REPARSE_POINT,
		0,
	)
	if err != nil {
		return nil, &os.PathError{Op: "open", Path: path, Err: err}
	}
	return os.NewFile(uintptr(handle), path), nil
}

func sidecarCreatePath(path string) (string, error) {
	// CreateFileW needs an extended-length absolute path on Windows systems
	// where long-path support is not globally enabled. Match os.OpenFile's
	// behavior so the safer sidecar creation does not regress long source paths.
	if len(path) < 248 || strings.HasPrefix(path, `\\?\`) || strings.HasPrefix(path, `\??\`) || strings.HasPrefix(path, `\\.\`) {
		return path, nil
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	if after, ok := strings.CutPrefix(abs, `\\`); ok {
		return `\\?\UNC\` + after, nil
	}
	return `\\?\` + abs, nil
}

//go:build windows

package logger

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"unsafe"

	"github.com/quarry/quarry-wails3/internal/winacl"
	"golang.org/x/sys/windows"
)

type fileAttributeTagInfo struct {
	attributes uint32
	reparseTag uint32
}

func openLogForRead(path string) (*os.File, error) {
	return openWindowsLog(path, windows.GENERIC_READ, windows.OPEN_EXISTING)
}

func openLogForAppend(path string) (*os.File, error) {
	// FILE_APPEND_DATA without FILE_WRITE_DATA makes each Write append at the
	// filesystem boundary while GENERIC_READ permits the partial-line check.
	return openWindowsLog(path, windows.GENERIC_READ|windows.FILE_APPEND_DATA, windows.OPEN_ALWAYS)
}

func openWindowsLog(path string, access uint32, disposition uint32) (*os.File, error) {
	openPath, err := windowsLongLogPath(path)
	if err != nil {
		return nil, &os.PathError{Op: "open", Path: path, Err: err}
	}
	name, err := windows.UTF16PtrFromString(openPath)
	if err != nil {
		return nil, &os.PathError{Op: "open", Path: path, Err: err}
	}
	share := uint32(windows.FILE_SHARE_READ | windows.FILE_SHARE_WRITE | windows.FILE_SHARE_DELETE)
	attributes := uint32(windows.FILE_ATTRIBUTE_NORMAL | windows.FILE_FLAG_OPEN_REPARSE_POINT)
	var handle windows.Handle
	if disposition == windows.OPEN_ALWAYS {
		// The descriptor is used only if Windows creates the file. OPEN_ALWAYS
		// preserves an existing log's owner and DACL rather than silently
		// rewriting user-managed access policy.
		handle, err = winacl.OpenFile(openPath, access, share, disposition, attributes)
	} else {
		handle, err = windows.CreateFile(name, access, share, nil, disposition, attributes, 0)
	}
	if err != nil {
		return nil, &os.PathError{Op: "open", Path: path, Err: err}
	}
	if err := validateWindowsLogHandle(handle, path); err != nil {
		_ = windows.CloseHandle(handle)
		return nil, err
	}
	file := os.NewFile(uintptr(handle), path)
	if file == nil {
		_ = windows.CloseHandle(handle)
		return nil, fmt.Errorf("%w %q: create retained file handle", ErrUnsafeLogPath, path)
	}
	return file, nil
}

func windowsLongLogPath(path string) (string, error) {
	if len(path) < 248 || strings.HasPrefix(path, `\\?\`) || strings.HasPrefix(path, `\??\`) || strings.HasPrefix(path, `\\.\`) {
		return path, nil
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	if strings.HasPrefix(abs, `\\`) {
		return `\\?\UNC\` + strings.TrimPrefix(abs, `\\`), nil
	}
	return `\\?\` + abs, nil
}

func validateWindowsLogHandle(handle windows.Handle, path string) error {
	fileType, err := windows.GetFileType(handle)
	if err != nil {
		return &os.PathError{Op: "stat", Path: path, Err: err}
	}
	if fileType != windows.FILE_TYPE_DISK {
		return fmt.Errorf("%w %q: log must be a disk file", ErrUnsafeLogPath, path)
	}
	var tag fileAttributeTagInfo
	if err := windows.GetFileInformationByHandleEx(
		handle,
		windows.FileAttributeTagInfo,
		(*byte)(unsafe.Pointer(&tag)),
		uint32(unsafe.Sizeof(tag)),
	); err != nil {
		return &os.PathError{Op: "stat", Path: path, Err: err}
	}
	if tag.attributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 || tag.attributes&windows.FILE_ATTRIBUTE_DIRECTORY != 0 {
		return fmt.Errorf("%w %q: log must not be a directory or reparse point", ErrUnsafeLogPath, path)
	}
	var info windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(handle, &info); err != nil {
		return &os.PathError{Op: "stat", Path: path, Err: err}
	}
	if info.NumberOfLinks != 1 {
		return fmt.Errorf("%w %q: log must have exactly one filesystem link", ErrUnsafeLogPath, path)
	}
	return nil
}

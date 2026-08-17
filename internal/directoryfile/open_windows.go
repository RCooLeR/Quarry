//go:build windows

package directoryfile

import (
	"errors"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/windows"
)

func changedOpenFailure(path string, openErr error) error {
	return &os.PathError{Op: "open", Path: path, Err: errors.Join(ErrPathChanged, openErr)}
}

func openDirectory(path string, forSync bool, afterPreflight func() error) (*os.File, error) {
	preflight, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	if err := validateDirectoryPreflight(path, preflight); err != nil {
		return nil, err
	}
	if afterPreflight != nil {
		if err := afterPreflight(); err != nil {
			return nil, err
		}
	}

	openPath, err := windowsDirectoryPath(path)
	if err != nil {
		return nil, &os.PathError{Op: "open", Path: path, Err: err}
	}
	name, err := windows.UTF16PtrFromString(openPath)
	if err != nil {
		return nil, &os.PathError{Op: "open", Path: path, Err: err}
	}
	access := uint32(windows.FILE_LIST_DIRECTORY | windows.FILE_TRAVERSE | windows.SYNCHRONIZE)
	if forSync {
		access |= windows.FILE_WRITE_DATA
	}
	handle, err := windows.CreateFile(
		name,
		access,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil,
		windows.OPEN_EXISTING,
		windows.FILE_FLAG_BACKUP_SEMANTICS,
		0,
	)
	if err != nil {
		// Do not path-stat again after a failed Windows open: a directory could
		// have been exchanged for a named-pipe/device namespace, and the opened
		// handle boundary is the authoritative nonblocking classifier.
		return nil, changedOpenFailure(path, err)
	}
	fileType, err := windows.GetFileType(handle)
	if err != nil {
		_ = windows.CloseHandle(handle)
		return nil, &os.PathError{Op: "stat", Path: path, Err: err}
	}
	var handleInfo windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(handle, &handleInfo); err != nil {
		_ = windows.CloseHandle(handle)
		return nil, &os.PathError{Op: "stat", Path: path, Err: err}
	}
	if fileType != windows.FILE_TYPE_DISK || handleInfo.FileAttributes&windows.FILE_ATTRIBUTE_DIRECTORY == 0 {
		_ = windows.CloseHandle(handle)
		return nil, &os.PathError{Op: "open", Path: path, Err: ErrNotDirectory}
	}

	file := os.NewFile(uintptr(handle), path)
	if file == nil {
		_ = windows.CloseHandle(handle)
		return nil, &os.PathError{Op: "open", Path: path, Err: os.ErrInvalid}
	}
	opened, err := validateDirectory(file, path)
	if err != nil {
		return nil, closeAfterError(file, err)
	}
	if !os.SameFile(preflight, opened) {
		return nil, closeAfterError(file, &os.PathError{Op: "open", Path: path, Err: ErrPathChanged})
	}
	current, err := os.Stat(path)
	if err != nil {
		return nil, closeAfterError(file, &os.PathError{Op: "stat", Path: path, Err: err})
	}
	if !current.IsDir() || !os.SameFile(opened, current) {
		return nil, closeAfterError(file, &os.PathError{Op: "open", Path: path, Err: ErrPathChanged})
	}
	return file, nil
}

func windowsDirectoryPath(path string) (string, error) {
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

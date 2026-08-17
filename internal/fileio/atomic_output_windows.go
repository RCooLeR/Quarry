//go:build windows

package fileio

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"unsafe"

	"golang.org/x/sys/windows"
)

type atomicOutputPlatformState struct {
	dir       windows.Handle
	finalBase string
}

var flushAtomicOutputDirectory = windows.FlushFileBuffers

var syncAtomicOutputDirectory = func(handle windows.Handle) error {
	// Flush the retained directory handle so a renamed/replaced pathname cannot
	// redirect finalization to another directory. Windows commonly reports
	// ERROR_INVALID_HANDLE because FlushFileBuffers does not support this
	// directory handle; only that exact capability result is best-effort. I/O,
	// device, permission, and filesystem errors remain durability failures.
	return normalizeDirectorySyncError(flushAtomicOutputDirectory(handle))
}

type fileRenameInformation struct {
	Flags          uint32
	RootDirectory  windows.Handle
	FileNameLength uint32
	FileName       [1]uint16
}

type fileDispositionInformation struct {
	DeleteFile byte
}

func openAtomicOutputTemp(finalPath string, mode os.FileMode) (*os.File, string, atomicOutputPlatformState, error) {
	descriptor, err := privateOutputSecurityDescriptor()
	if err != nil {
		return nil, "", atomicOutputPlatformState{}, err
	}
	dirPath, finalBase := filepath.Split(finalPath)
	tempPathPrefix := dirPath
	if dirPath == "" {
		dirPath = "."
		tempPathPrefix = "." + string(filepath.Separator)
	}
	dirPtr, err := windows.UTF16PtrFromString(dirPath)
	if err != nil {
		return nil, "", atomicOutputPlatformState{}, err
	}
	dirHandle, err := windows.CreateFile(
		dirPtr,
		windows.FILE_LIST_DIRECTORY|windows.FILE_WRITE_DATA|windows.FILE_TRAVERSE|windows.SYNCHRONIZE,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil,
		windows.OPEN_EXISTING,
		windows.FILE_FLAG_BACKUP_SEMANTICS,
		0,
	)
	if err != nil {
		return nil, "", atomicOutputPlatformState{}, err
	}
	if err := requirePrivateOutputACLVolume(dirHandle); err != nil {
		_ = windows.CloseHandle(dirHandle)
		return nil, "", atomicOutputPlatformState{}, err
	}
	state := atomicOutputPlatformState{dir: dirHandle, finalBase: finalBase}

	for attempt := 0; attempt < 128; attempt++ {
		name, err := randomAtomicOutputName(finalBase)
		if err != nil {
			_ = windows.CloseHandle(dirHandle)
			return nil, "", atomicOutputPlatformState{}, err
		}
		tempPath := tempPathPrefix + name
		handle, err := createAtomicOutputTempAt(dirHandle, name, descriptor)
		if isWindowsNameCollision(err) {
			continue
		}
		if err != nil {
			_ = windows.CloseHandle(dirHandle)
			return nil, "", atomicOutputPlatformState{}, err
		}
		file := os.NewFile(uintptr(handle), tempPath)
		if file == nil {
			_ = disposeAtomicOutputHandle(handle)
			_ = windows.CloseHandle(dirHandle)
			return nil, "", atomicOutputPlatformState{}, errors.New("create atomic output file handle")
		}
		return file, tempPath, state, nil
	}

	_ = windows.CloseHandle(dirHandle)
	return nil, "", atomicOutputPlatformState{}, errors.New("could not allocate a unique atomic output name")
}

func publishAtomicOutput(ctx context.Context, o *AtomicOutput) error {
	process := windows.CurrentProcess()
	var retained windows.Handle
	if err := windows.DuplicateHandle(
		process,
		windows.Handle(o.file.Fd()),
		process,
		&retained,
		0,
		false,
		windows.DUPLICATE_SAME_ACCESS,
	); err != nil {
		return err
	}

	closeErr := o.file.Close()
	o.file = nil
	if closeErr != nil {
		cleanupErr := disposeAtomicOutputHandle(retained)
		return errors.Join(closeErr, cleanupErr)
	}
	if err := ctx.Err(); err != nil {
		cleanupErr := disposeAtomicOutputHandle(retained)
		if cleanupErr == nil {
			o.tempPath = ""
		}
		return errors.Join(err, cleanupErr)
	}

	renameErr := renameAtomicOutputHandle(retained, o.platform.dir, o.platform.finalBase)
	if renameErr != nil {
		cleanupErr := disposeAtomicOutputHandle(retained)
		if cleanupErr == nil {
			o.tempPath = ""
		}
		if isWindowsNameCollision(renameErr) {
			renameErr = fmt.Errorf("%w: %s", ErrExists, o.finalPath)
		}
		return errors.Join(renameErr, cleanupErr)
	}
	afterAtomicOutputPublish()

	retainedFile := os.NewFile(uintptr(retained), "published Quarry output")
	if retainedFile == nil {
		cleanupErr := disposeAtomicOutputHandle(retained)
		dirSyncErr := syncAtomicOutputDirectory(o.platform.dir)
		dirCloseErr := closeAtomicOutputDir(&o.platform)
		o.tempPath = ""
		o.published = cleanupErr != nil
		drift := &OutputPathDriftError{
			Path:       o.finalPath,
			RolledBack: cleanupErr == nil,
			Detail:     "could not retain the published file for identity verification",
		}
		if cleanupErr != nil || dirSyncErr != nil {
			uncertain := &PublicationError{FinalPath: o.finalPath, LocationUncertain: true, Err: drift}
			return errors.Join(drift, uncertain, cleanupErr, dirSyncErr, dirCloseErr)
		}
		return errors.Join(drift, dirCloseErr)
	}
	ownedInfo, ownedErr := retainedFile.Stat()
	namedInfo, namedErr := os.Stat(o.finalPath)
	if ownedErr != nil || namedErr != nil || !os.SameFile(ownedInfo, namedInfo) {
		dispositionErr := markAtomicOutputDelete(retained)
		retainedCloseErr := retainedFile.Close()
		dirSyncErr := syncAtomicOutputDirectory(o.platform.dir)
		dirCloseErr := closeAtomicOutputDir(&o.platform)
		o.tempPath = ""
		rollbackErr := errors.Join(dispositionErr, retainedCloseErr, dirSyncErr)
		o.published = rollbackErr != nil
		detail := "exact path resolves to another object"
		if ownedErr != nil {
			detail = ownedErr.Error()
		} else if namedErr != nil {
			detail = namedErr.Error()
		}
		drift := &OutputPathDriftError{Path: o.finalPath, RolledBack: rollbackErr == nil, Detail: detail}
		if rollbackErr != nil {
			uncertain := &PublicationError{FinalPath: o.finalPath, LocationUncertain: true, Err: drift}
			return errors.Join(drift, uncertain, rollbackErr, dirCloseErr)
		}
		return errors.Join(drift, dirCloseErr)
	}

	o.published = true
	o.tempPath = ""
	handleCloseErr := retainedFile.Close()
	dirSyncErr := syncAtomicOutputDirectory(o.platform.dir)
	dirCloseErr := closeAtomicOutputDir(&o.platform)
	if err := errors.Join(handleCloseErr, dirSyncErr, dirCloseErr); err != nil {
		return &PublicationError{FinalPath: o.finalPath, Durable: dirSyncErr == nil, Err: err}
	}
	return nil
}

func cleanupAtomicOutput(o *AtomicOutput) error {
	var cleanupErr error
	if o.file != nil {
		handle := windows.Handle(o.file.Fd())
		dispositionErr := markAtomicOutputDelete(handle)
		closeErr := o.file.Close()
		o.file = nil
		cleanupErr = errors.Join(dispositionErr, closeErr)
		if cleanupErr == nil {
			o.tempPath = ""
		}
	} else if !o.published && o.tempPath != "" {
		cleanupErr = fmt.Errorf("refuse path-based cleanup after losing owned output handle %q", o.tempPath)
	}
	return errors.Join(cleanupErr, closeAtomicOutputDir(&o.platform))
}

func createAtomicOutputTempAt(dir windows.Handle, name string, descriptor *windows.SECURITY_DESCRIPTOR) (windows.Handle, error) {
	objectName, err := windows.NewNTUnicodeString(name)
	if err != nil {
		return 0, err
	}
	attributes := &windows.OBJECT_ATTRIBUTES{
		RootDirectory:      dir,
		ObjectName:         objectName,
		Attributes:         windows.OBJ_CASE_INSENSITIVE,
		SecurityDescriptor: descriptor,
	}
	attributes.Length = uint32(unsafe.Sizeof(*attributes))
	var status windows.IO_STATUS_BLOCK
	var allocationSize int64
	var handle windows.Handle
	err = windows.NtCreateFile(
		&handle,
		windows.GENERIC_READ|windows.GENERIC_WRITE|windows.DELETE|windows.SYNCHRONIZE,
		attributes,
		&status,
		&allocationSize,
		windows.FILE_ATTRIBUTE_NORMAL,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_DELETE,
		windows.FILE_CREATE,
		windows.FILE_NON_DIRECTORY_FILE|windows.FILE_SYNCHRONOUS_IO_NONALERT,
		0,
		0,
	)
	return handle, err
}

func renameAtomicOutputHandle(handle windows.Handle, dir windows.Handle, finalBase string) error {
	name, err := windows.UTF16FromString(finalBase)
	if err != nil {
		return err
	}
	var layout fileRenameInformation
	nameBytes := (len(name) - 1) * 2
	buffer := make([]byte, int(unsafe.Offsetof(layout.FileName))+len(name)*2)
	info := (*fileRenameInformation)(unsafe.Pointer(&buffer[0]))
	info.RootDirectory = dir
	info.FileNameLength = uint32(nameBytes)
	copy(unsafe.Slice(&info.FileName[0], len(name)), name)
	var status windows.IO_STATUS_BLOCK
	return windows.NtSetInformationFile(handle, &status, &buffer[0], uint32(len(buffer)), windows.FileRenameInformation)
}

func isWindowsNameCollision(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, windows.ERROR_FILE_EXISTS) || errors.Is(err, windows.ERROR_ALREADY_EXISTS) {
		return true
	}
	status, ok := err.(windows.NTStatus)
	return ok && (status == windows.STATUS_OBJECT_NAME_COLLISION || status == windows.STATUS_OBJECT_NAME_EXISTS)
}

func markAtomicOutputDelete(handle windows.Handle) error {
	info := fileDispositionInformation{DeleteFile: 1}
	return windows.SetFileInformationByHandle(
		handle,
		windows.FileDispositionInfo,
		(*byte)(unsafe.Pointer(&info)),
		uint32(unsafe.Sizeof(info)),
	)
}

func disposeAtomicOutputHandle(handle windows.Handle) error {
	if handle == 0 || handle == windows.InvalidHandle {
		return nil
	}
	return errors.Join(markAtomicOutputDelete(handle), windows.CloseHandle(handle))
}

func closeAtomicOutputDir(state *atomicOutputPlatformState) error {
	if state == nil || state.dir == 0 || state.dir == windows.InvalidHandle {
		return nil
	}
	err := windows.CloseHandle(state.dir)
	state.dir = 0
	return err
}

func randomAtomicOutputName(base string) (string, error) {
	var random [16]byte
	if _, err := rand.Read(random[:]); err != nil {
		return "", err
	}
	return "." + base + ".quarry-" + hex.EncodeToString(random[:]), nil
}

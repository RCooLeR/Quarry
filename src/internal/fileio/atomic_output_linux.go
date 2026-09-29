//go:build linux

package fileio

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
)

type atomicOutputPlatformState struct {
	dirFD     int
	finalBase string
}

func openAtomicOutputTemp(finalPath string, mode os.FileMode) (*os.File, string, atomicOutputPlatformState, error) {
	dirPath, finalBase := filepath.Split(finalPath)
	if dirPath == "" {
		dirPath = "."
	}
	dirFD, err := unix.Open(dirPath, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NONBLOCK, 0)
	if err != nil {
		return nil, "", atomicOutputPlatformState{}, err
	}
	state := atomicOutputPlatformState{dirFD: dirFD, finalBase: finalBase}
	fd, err := unix.Openat(dirFD, ".", unix.O_RDWR|unix.O_CLOEXEC|unix.O_TMPFILE, uint32(mode.Perm()))
	if err != nil {
		_ = unix.Close(dirFD)
		return nil, "", atomicOutputPlatformState{}, fmt.Errorf("%w: %v", ErrSecureAtomicOutputUnavailable, err)
	}
	file := os.NewFile(uintptr(fd), "anonymous Quarry output")
	if file == nil {
		_ = unix.Close(fd)
		_ = unix.Close(dirFD)
		return nil, "", atomicOutputPlatformState{}, errors.New("create anonymous atomic output file handle")
	}
	return file, "", state, nil
}

func publishAtomicOutput(ctx context.Context, o *AtomicOutput) error {
	retained, err := unix.FcntlInt(o.file.Fd(), unix.F_DUPFD_CLOEXEC, 0)
	if err != nil {
		return err
	}
	closeErr := o.file.Close()
	o.file = nil
	if closeErr != nil {
		return errors.Join(closeErr, unix.Close(retained))
	}
	if err := ctx.Err(); err != nil {
		return errors.Join(err, unix.Close(retained))
	}

	if err := unix.Linkat(retained, "", o.platform.dirFD, o.platform.finalBase, unix.AT_EMPTY_PATH); err != nil {
		closeRetainedErr := unix.Close(retained)
		if errors.Is(err, unix.EEXIST) {
			err = fmt.Errorf("%w: %s", ErrExists, o.finalPath)
		}
		return errors.Join(err, closeRetainedErr)
	}
	afterAtomicOutputPublish()

	matches, verifyErr := publishedLinuxPathMatches(retained, o.finalPath)
	if verifyErr != nil || !matches {
		rollbackErr := rollbackPublishedLinuxOutput(retained, &o.platform)
		retainedCloseErr := unix.Close(retained)
		dirCloseErr := closeAtomicOutputDir(&o.platform)
		o.tempPath = ""
		o.published = rollbackErr != nil
		detail := "exact path resolves to another object"
		if verifyErr != nil {
			detail = verifyErr.Error()
		}
		drift := &OutputPathDriftError{Path: o.finalPath, RolledBack: rollbackErr == nil, Detail: detail}
		if rollbackErr != nil {
			uncertain := &PublicationError{FinalPath: o.finalPath, LocationUncertain: true, Err: drift}
			return errors.Join(drift, uncertain, rollbackErr, retainedCloseErr, dirCloseErr)
		}
		return errors.Join(drift, retainedCloseErr, dirCloseErr)
	}
	o.published = true

	dirSyncErr := unix.Fsync(o.platform.dirFD)
	retainedCloseErr := unix.Close(retained)
	dirCloseErr := closeAtomicOutputDir(&o.platform)
	if err := errors.Join(dirSyncErr, retainedCloseErr, dirCloseErr); err != nil {
		return &PublicationError{FinalPath: o.finalPath, Durable: dirSyncErr == nil, Err: err}
	}
	return nil
}

func publishedLinuxPathMatches(retained int, path string) (bool, error) {
	var owned unix.Stat_t
	if err := unix.Fstat(retained, &owned); err != nil {
		return false, err
	}
	var named unix.Stat_t
	if err := unix.Stat(path, &named); err != nil {
		return false, err
	}
	return owned.Dev == named.Dev && owned.Ino == named.Ino, nil
}

func rollbackPublishedLinuxOutput(retained int, state *atomicOutputPlatformState) error {
	if state == nil || state.dirFD < 0 {
		return errors.New("atomic output parent handle is unavailable")
	}
	var owned unix.Stat_t
	if err := unix.Fstat(retained, &owned); err != nil {
		return err
	}
	var named unix.Stat_t
	if err := unix.Fstatat(state.dirFD, state.finalBase, &named, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		return err
	}
	if owned.Dev != named.Dev || owned.Ino != named.Ino {
		return errors.New("refuse to remove a substituted handle-relative final entry")
	}
	if err := unix.Unlinkat(state.dirFD, state.finalBase, 0); err != nil {
		return err
	}
	return unix.Fsync(state.dirFD)
}

func cleanupAtomicOutput(o *AtomicOutput) error {
	var fileErr error
	if o.file != nil {
		fileErr = o.file.Close()
		o.file = nil
	}
	return errors.Join(fileErr, closeAtomicOutputDir(&o.platform))
}

func closeAtomicOutputDir(state *atomicOutputPlatformState) error {
	if state == nil || state.dirFD < 0 {
		return nil
	}
	err := unix.Close(state.dirFD)
	state.dirFD = -1
	return err
}

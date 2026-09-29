package main

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/quarry/quarry-wails3/internal/session"
)

var ErrServiceStopped = errors.New("file service is shutting down")

// filePreflightTimeout bounds validation-only lease waits that happen before a
// native save dialog. The actual transform starts a registered service job
// after the user chooses a destination and has its own cancellation owner.
const filePreflightTimeout = 30 * time.Second

func (s *FileService) ensureServiceRunning() error {
	if s == nil {
		return ErrServiceStopped
	}
	s.serviceMu.Lock()
	stopping := s.serviceStopping
	s.serviceMu.Unlock()
	if stopping {
		return ErrServiceStopped
	}
	return nil
}

// stopServiceAdmission raises the irreversible service gate before any
// subsystem cancellation starts. It returns whether this caller owns the
// shutdown sequence plus the completion channel shared by later callers.
func (s *FileService) stopServiceAdmission() (bool, <-chan struct{}) {
	s.serviceMu.Lock()
	if s.serviceStopDone == nil {
		s.serviceStopDone = make(chan struct{})
	}
	done := s.serviceStopDone
	if s.serviceStopping {
		s.serviceMu.Unlock()
		return false, done
	}
	s.serviceStopping = true
	s.serviceMu.Unlock()
	return true, done
}

func (s *FileService) finishServiceShutdown() {
	s.serviceMu.Lock()
	if s.serviceStopDone != nil {
		select {
		case <-s.serviceStopDone:
		default:
			close(s.serviceStopDone)
		}
	}
	s.serviceMu.Unlock()
}

var serviceLeaseHook struct {
	sync.RWMutex
	fn func(operation string, fileID string, snapshot session.FileSnapshot)
}

func notifyServiceLease(operation string, fileID string, lease *session.Lease) {
	serviceLeaseHook.RLock()
	hook := serviceLeaseHook.fn
	serviceLeaseHook.RUnlock()
	if hook != nil {
		hook(operation, fileID, lease.Snapshot())
	}
}

func installServiceLeaseHook(hook func(string, string, session.FileSnapshot)) func() {
	serviceLeaseHook.Lock()
	previous := serviceLeaseHook.fn
	serviceLeaseHook.fn = hook
	serviceLeaseHook.Unlock()
	return func() {
		serviceLeaseHook.Lock()
		serviceLeaseHook.fn = previous
		serviceLeaseHook.Unlock()
	}
}

func normalizeSessionLeaseError(fileID string, err error) error {
	if errors.Is(err, session.ErrUnknownFile) {
		return fmt.Errorf("unknown file id %q", fileID)
	}
	return err
}

func (s *FileService) acquireReadFile(fileID string) (*session.Lease, *session.File, error) {
	return s.acquireReadFileContext(context.Background(), fileID)
}

func (s *FileService) acquireReadFileContext(ctx context.Context, fileID string) (*session.Lease, *session.File, error) {
	if err := validateRPCFileID(fileID); err != nil {
		return nil, nil, err
	}
	if err := s.ensureServiceRunning(); err != nil {
		return nil, nil, err
	}
	lease, err := s.reg.AcquireReadContext(ctx, fileID)
	if err != nil {
		return nil, nil, normalizeSessionLeaseError(fileID, err)
	}
	file := lease.File()
	if file == nil || file.Doc == nil {
		lease.Release()
		return nil, nil, fmt.Errorf("unknown file id %q", fileID)
	}
	return lease, file, nil
}

func (s *FileService) acquireExclusiveFile(fileID string) (*session.Lease, *session.File, error) {
	return s.acquireExclusiveFileContext(context.Background(), fileID)
}

func (s *FileService) acquireExclusiveFileContext(ctx context.Context, fileID string) (*session.Lease, *session.File, error) {
	if err := validateRPCFileID(fileID); err != nil {
		return nil, nil, err
	}
	if err := s.ensureServiceRunning(); err != nil {
		return nil, nil, err
	}
	lease, err := s.reg.AcquireExclusiveContext(ctx, fileID)
	if err != nil {
		return nil, nil, normalizeSessionLeaseError(fileID, err)
	}
	file := lease.File()
	if file == nil || file.Doc == nil {
		lease.Release()
		return nil, nil, fmt.Errorf("unknown file id %q", fileID)
	}
	return lease, file, nil
}

// beginFilePreflight registers validation-only work before it attempts a read
// lease. Close/refresh/shutdown can therefore cancel a queued waiter, and the
// timeout prevents a direct native-dialog RPC from waiting forever even when
// no lifecycle event arrives. The returned finish must always be called.
func (s *FileService) beginFilePreflight(fileID string) (context.Context, func(), error) {
	if err := validateRPCFileID(fileID); err != nil {
		return nil, nil, err
	}
	if err := s.ensureServiceRunning(); err != nil {
		return nil, nil, err
	}
	timeoutCtx, cancelTimeout := context.WithTimeout(context.Background(), filePreflightTimeout)
	ctx, finishRun, err := s.beginForegroundRun(timeoutCtx, fileID)
	if err != nil {
		cancelTimeout()
		return nil, nil, err
	}
	var once sync.Once
	finish := func() {
		once.Do(func() {
			finishRun()
			cancelTimeout()
		})
	}
	return ctx, finish, nil
}

// acquireReadFilePreflight is the common validation-only lease boundary for
// save-copy, CSV transforms, harvest, and cached SQL-summary dialogs.
func (s *FileService) acquireReadFilePreflight(fileID string) (*session.Lease, *session.File, func(), error) {
	ctx, finish, err := s.beginFilePreflight(fileID)
	if err != nil {
		return nil, nil, nil, err
	}
	lease, file, err := s.acquireReadFileContext(ctx, fileID)
	if err != nil {
		finish()
		return nil, nil, nil, err
	}
	var once sync.Once
	cleanup := func() {
		once.Do(func() {
			lease.Release()
			finish()
		})
	}
	return lease, file, cleanup, nil
}

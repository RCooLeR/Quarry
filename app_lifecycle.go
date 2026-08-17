package main

import (
	"errors"
	"sync"
)

const closeRequestedEvent = "quarry:close-requested"

var (
	errNoCloseRequest  = errors.New("no application close request is pending")
	errCloseInProgress = errors.New("application close is already in progress")
)

type closeLifecycleState uint8

const (
	closeIdle closeLifecycleState = iota
	closeRequested
	closeArmed
	closeInProgress
)

// AppLifecycle coordinates native close attempts with the frontend's
// Save copy / Discard / Cancel flow. Native close and application quit are
// rejected until ApproveClose arms exactly one subsequent close attempt.
type AppLifecycle struct {
	mu           sync.Mutex
	state        closeLifecycleState
	quit         func()
	notify       func()
	shutdown     func()
	shutdownOnce sync.Once
}

func (l *AppLifecycle) bindShutdown(shutdown func()) {
	l.mu.Lock()
	l.shutdown = shutdown
	inProgress := l.state == closeInProgress
	l.mu.Unlock()
	if inProgress && shutdown != nil {
		l.shutdownOnce.Do(shutdown)
	}
}

func newAppLifecycle() *AppLifecycle {
	return &AppLifecycle{}
}

func (l *AppLifecycle) bind(quit func(), notify func()) {
	l.mu.Lock()
	l.quit = quit
	l.notify = notify
	pending := l.state == closeRequested
	l.mu.Unlock()

	// This is only expected if a platform asks to quit during startup. Keep
	// that request fail-closed, then surface it as soon as the window exists.
	if pending && notify != nil {
		notify()
	}
}

// interceptClose is used by both the native window hook and ShouldQuit. It
// returns true only after a frontend approval has armed one close attempt.
func (l *AppLifecycle) interceptClose() bool {
	l.mu.Lock()
	var notify func()
	switch l.state {
	case closeArmed:
		l.state = closeInProgress
		shutdown := l.shutdown
		l.mu.Unlock()
		if shutdown != nil {
			l.shutdownOnce.Do(shutdown)
		}
		return true
	case closeInProgress:
		l.mu.Unlock()
		return true
	case closeRequested:
		l.mu.Unlock()
		return false
	case closeIdle:
		l.state = closeRequested
		notify = l.notify
		l.mu.Unlock()
		if notify != nil {
			notify()
		}
		return false
	default:
		l.mu.Unlock()
		return false
	}
}

// ApproveClose arms one application quit after the frontend has resolved every
// dirty or staged tab. Calling it without a native close request fails closed.
func (l *AppLifecycle) ApproveClose() error {
	l.mu.Lock()
	if l.state != closeRequested {
		if l.state == closeArmed || l.state == closeInProgress {
			l.mu.Unlock()
			return errCloseInProgress
		}
		l.mu.Unlock()
		return errNoCloseRequest
	}
	if l.quit == nil {
		l.mu.Unlock()
		return errors.New("application close is not available")
	}
	quit := l.quit
	l.state = closeArmed
	l.mu.Unlock()

	quit()
	return nil
}

// CancelClose releases a pending native close request. It is idempotent after
// a cancellation, but it cannot revoke an approval that shutdown has consumed.
func (l *AppLifecycle) CancelClose() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	switch l.state {
	case closeRequested:
		l.state = closeIdle
		return nil
	case closeIdle:
		return nil
	default:
		return errCloseInProgress
	}
}

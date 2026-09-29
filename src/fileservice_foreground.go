package main

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/quarry/quarry-wails3/internal/session"
)

var (
	ErrForegroundRunLimit    = errors.New("too many foreground operations are active")
	ErrForegroundRunsStopped = errors.New("foreground operations are shutting down")
)

const (
	maxForegroundRunsPerFile = 4
	maxForegroundRunsTotal   = session.DefaultMaxOpenFiles * maxForegroundRunsPerFile
)

type foregroundRun struct {
	id      uint64
	fileIDs []string
	cancel  context.CancelFunc
	done    chan struct{}
	once    sync.Once
}

// beginForegroundRun registers one bounded cancellation owner under every
// source ID before the caller attempts any document lease. Close/refresh can
// therefore block new registrations, cancel existing work, and then drain
// leases without a registration-after-cancel race. finish is idempotent and
// must be deferred by every successful caller.
func (s *FileService) beginForegroundRun(parent context.Context, fileIDs ...string) (context.Context, func(), error) {
	// Validate the complete caller-controlled ID set before consulting the
	// parent context, allocating registration maps, or touching service state.
	// This also keeps invalid-input behavior safe for a nil service receiver.
	for _, fileID := range fileIDs {
		if err := validateRPCFileID(fileID); err != nil {
			return nil, nil, err
		}
	}
	if parent == nil {
		parent = context.Background()
	}
	if err := parent.Err(); err != nil {
		return nil, nil, err
	}
	unique := make([]string, 0, len(fileIDs))
	seen := make(map[string]struct{}, len(fileIDs))
	for _, fileID := range fileIDs {
		if _, ok := seen[fileID]; ok {
			continue
		}
		seen[fileID] = struct{}{}
		unique = append(unique, fileID)
	}
	if len(unique) == 0 {
		return nil, nil, errors.New("foreground operation requires a file id")
	}
	if err := s.ensureServiceRunning(); err != nil {
		return nil, nil, err
	}

	ctx, cancel := context.WithCancel(parent)
	s.foregroundMu.Lock()
	if s.foregroundRuns == nil {
		s.foregroundRuns = make(map[string]map[uint64]*foregroundRun)
	}
	if s.foregroundBlocked == nil {
		s.foregroundBlocked = make(map[string]struct{})
	}
	if s.foregroundStopping {
		s.foregroundMu.Unlock()
		cancel()
		return nil, nil, ErrForegroundRunsStopped
	}
	for _, fileID := range unique {
		if _, blocked := s.foregroundBlocked[fileID]; blocked {
			s.foregroundMu.Unlock()
			cancel()
			return nil, nil, fmt.Errorf("%w for %q", session.ErrTransitioning, fileID)
		}
		if len(s.foregroundRuns[fileID]) >= maxForegroundRunsPerFile {
			s.foregroundMu.Unlock()
			cancel()
			return nil, nil, fmt.Errorf("%w for %q (limit %d)", ErrForegroundRunLimit, fileID, maxForegroundRunsPerFile)
		}
	}
	if s.foregroundRunCount >= maxForegroundRunsTotal || s.foregroundSeq == ^uint64(0) {
		s.foregroundMu.Unlock()
		cancel()
		return nil, nil, fmt.Errorf("%w (global limit %d)", ErrForegroundRunLimit, maxForegroundRunsTotal)
	}
	s.foregroundSeq++
	run := &foregroundRun{
		id:      s.foregroundSeq,
		fileIDs: unique,
		cancel:  cancel,
		done:    make(chan struct{}),
	}
	for _, fileID := range unique {
		bucket := s.foregroundRuns[fileID]
		if bucket == nil {
			bucket = make(map[uint64]*foregroundRun)
			s.foregroundRuns[fileID] = bucket
		}
		bucket[run.id] = run
	}
	s.foregroundRunCount++
	s.foregroundMu.Unlock()

	finish := func() {
		run.once.Do(func() {
			run.cancel()
			s.foregroundMu.Lock()
			for _, fileID := range run.fileIDs {
				bucket := s.foregroundRuns[fileID]
				if bucket[run.id] == run {
					delete(bucket, run.id)
					if len(bucket) == 0 {
						delete(s.foregroundRuns, fileID)
					}
				}
			}
			s.foregroundRunCount--
			close(run.done)
			s.foregroundMu.Unlock()
		})
	}
	return ctx, finish, nil
}

// blockForegroundRuns closes the registration gate for one source and
// cancels a stable snapshot outside the service mutex. The gate remains closed
// until unblockForegroundRuns, which a reversible lifecycle refusal must call.
func (s *FileService) blockForegroundRuns(fileID string) {
	s.foregroundMu.Lock()
	if s.foregroundBlocked == nil {
		s.foregroundBlocked = make(map[string]struct{})
	}
	s.foregroundBlocked[fileID] = struct{}{}
	bucket := s.foregroundRuns[fileID]
	runs := make([]*foregroundRun, 0, len(bucket))
	for _, run := range bucket {
		runs = append(runs, run)
	}
	s.foregroundMu.Unlock()
	for _, run := range runs {
		run.cancel()
	}
	// Public interactive-search IDs are a second ownership surface over the
	// same foreground runs. Drain them synchronously at the lifecycle barrier,
	// rather than relying on their cancellation watcher to win a scheduling
	// race after close or refresh has already returned.
	s.stopSearchRequestsForFile(fileID)
}

func (s *FileService) unblockForegroundRuns(fileID string) {
	s.foregroundMu.Lock()
	delete(s.foregroundBlocked, fileID)
	s.foregroundMu.Unlock()
}

// stopForegroundRuns permanently closes registration and returns unique
// completion signals for a bounded shutdown join.
func (s *FileService) stopForegroundRuns() []<-chan struct{} {
	s.foregroundMu.Lock()
	s.foregroundStopping = true
	seen := make(map[uint64]struct{}, s.foregroundRunCount)
	runs := make([]*foregroundRun, 0, s.foregroundRunCount)
	for _, bucket := range s.foregroundRuns {
		for id, run := range bucket {
			if _, ok := seen[id]; ok {
				continue
			}
			seen[id] = struct{}{}
			runs = append(runs, run)
		}
	}
	s.foregroundMu.Unlock()
	done := make([]<-chan struct{}, 0, len(runs))
	for _, run := range runs {
		run.cancel()
		done = append(done, run.done)
	}
	return done
}

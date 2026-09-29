package main

import (
	"context"
	"errors"
	"math"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/quarry/quarry-wails3/internal/fileio"
)

type recordedJobEvent struct {
	name string
	data map[string]any
}

func installJobEventRecorder(t *testing.T, svc *FileService) (*sync.Mutex, *[]recordedJobEvent) {
	t.Helper()
	var mu sync.Mutex
	events := make([]recordedJobEvent, 0, 8)
	manager := svc.jobs()
	manager.mu.Lock()
	manager.emit = func(name string, value any) {
		data, _ := value.(map[string]any)
		mu.Lock()
		events = append(events, recordedJobEvent{name: name, data: data})
		mu.Unlock()
	}
	manager.mu.Unlock()
	return &mu, &events
}

func activeJobIDForTest(t *testing.T, svc *FileService) string {
	t.Helper()
	manager := svc.jobs()
	manager.mu.Lock()
	defer manager.mu.Unlock()
	if manager.active == nil {
		t.Fatal("expected an active job")
	}
	return manager.active.id
}

func waitJobResult[T any](t *testing.T, result <-chan struct {
	value T
	err   error
}) (T, error) {
	t.Helper()
	select {
	case got := <-result:
		return got.value, got.err
	case <-time.After(5 * time.Second):
		var zero T
		t.Fatal("background job did not return")
		return zero, nil
	}
}

func TestServiceJobPanicAlwaysReleasesOwnership(t *testing.T) {
	svc := NewFileService()
	mu, events := installJobEventRecorder(t, svc)

	_, err := runServiceJob(svc, jobSpec{Title: "panic test"}, func(context.Context, func(int64, int64, string)) (int, error) {
		panic("deliberate")
	})
	if err == nil || !strings.Contains(err.Error(), "panicked") {
		t.Fatalf("panic error = %v", err)
	}
	manager := svc.jobs()
	manager.mu.Lock()
	active := manager.active
	manager.mu.Unlock()
	if active != nil {
		t.Fatal("panicked job retained active ownership")
	}

	value, err := runServiceJob(svc, jobSpec{Title: "next"}, func(context.Context, func(int64, int64, string)) (int, error) {
		return 42, nil
	})
	if err != nil || value != 42 {
		t.Fatalf("job after panic = %d, %v", value, err)
	}

	mu.Lock()
	defer mu.Unlock()
	foundPanicEnd := false
	for _, event := range *events {
		if event.name == "quarry:job-end" && event.data["status"] == "panicked" {
			foundPanicEnd = true
		}
	}
	if !foundPanicEnd {
		t.Fatalf("events = %#v, want a panicked terminal event", *events)
	}
}

func TestServiceJobSequenceExhaustionDoesNotAdmitUnsafeSequence(t *testing.T) {
	svc := NewFileService()
	manager := svc.jobs()
	var eventMu sync.Mutex
	events := make([]recordedJobEvent, 0, 2)
	manager.mu.Lock()
	manager.seq = maxJobProgressValue - 1
	manager.emit = func(name string, value any) {
		data, _ := value.(map[string]any)
		eventMu.Lock()
		events = append(events, recordedJobEvent{name: name, data: data})
		eventMu.Unlock()
	}
	manager.mu.Unlock()

	called := 0
	value, err := runServiceJob(svc, jobSpec{Title: "last addressable job"}, func(context.Context, func(int64, int64, string)) (int, error) {
		called++
		return 1, nil
	})
	if err != nil || value != 1 || called != 1 {
		t.Fatalf("last addressable job = %d, called=%d, err=%v", value, called, err)
	}

	value, err = runServiceJob(svc, jobSpec{Title: "must not start"}, func(context.Context, func(int64, int64, string)) (int, error) {
		called++
		return 2, nil
	})
	if value != 0 || !errors.Is(err, ErrJobSequenceExhausted) {
		t.Fatalf("job after sequence exhaustion = %d, err=%v", value, err)
	}
	if called != 1 {
		t.Fatalf("exhausted job callback ran; total calls=%d", called)
	}
	manager.mu.Lock()
	sequence, active := manager.seq, manager.active
	manager.mu.Unlock()
	if sequence != maxJobProgressValue || active != nil {
		t.Fatalf("job state after exhaustion: sequence=%d active=%+v", sequence, active)
	}
	eventMu.Lock()
	defer eventMu.Unlock()
	if len(events) != 2 { // start + end for the last valid job only
		t.Fatalf("job events = %d, want 2 from the last valid job only", len(events))
	}
	for _, event := range events {
		if event.data["id"] != "job9007199254740991" || event.data["sequence"] != maxJobProgressValue {
			t.Fatalf("last safe job event = %#v", event)
		}
	}
}

func TestServiceJobRejectsStaleCancelAndOverlappingJob(t *testing.T) {
	svc := NewFileService()
	firstID := ""
	manager := svc.jobs()
	manager.mu.Lock()
	manager.emit = func(name string, value any) {
		if name != "quarry:job-start" || firstID != "" {
			return
		}
		firstID, _ = value.(map[string]any)["id"].(string)
	}
	manager.mu.Unlock()
	if _, err := runServiceJob(svc, jobSpec{Title: "first"}, func(context.Context, func(int64, int64, string)) (int, error) {
		return 1, nil
	}); err != nil {
		t.Fatal(err)
	}
	if firstID == "" {
		t.Fatal("first job ID was not emitted")
	}

	started := make(chan struct{})
	release := make(chan struct{})
	result := make(chan struct {
		value int
		err   error
	}, 1)
	go func() {
		value, err := runServiceJob(svc, jobSpec{Title: "second"}, func(ctx context.Context, _ func(int64, int64, string)) (int, error) {
			close(started)
			select {
			case <-ctx.Done():
				return 0, ctx.Err()
			case <-release:
				return 2, nil
			}
		})
		result <- struct {
			value int
			err   error
		}{value: value, err: err}
	}()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("second job did not start")
	}
	secondID := activeJobIDForTest(t, svc)
	if secondID == firstID {
		t.Fatalf("job ID was reused: %q", secondID)
	}
	if err := svc.CancelJob(firstID); !errors.Is(err, ErrJobIDMismatch) {
		t.Fatalf("stale cancel error = %v, want ErrJobIDMismatch", err)
	}
	if _, err := runServiceJob(svc, jobSpec{Title: "overlap"}, func(context.Context, func(int64, int64, string)) (int, error) {
		return 0, nil
	}); !errors.Is(err, ErrJobAlreadyRunning) {
		t.Fatalf("overlap error = %v, want ErrJobAlreadyRunning", err)
	}
	close(release)
	value, err := waitJobResult(t, result)
	if err != nil || value != 2 {
		t.Fatalf("second job was affected by stale cancel: value=%d err=%v", value, err)
	}
}

func TestServiceJobFileLifecycleAndShutdownCancel(t *testing.T) {
	t.Run("matching file only", func(t *testing.T) {
		svc := NewFileService()
		started := make(chan struct{})
		result := make(chan struct {
			value int
			err   error
		}, 1)
		go func() {
			value, err := runServiceJob(svc, jobSpec{Title: "analysis", Kind: jobKindSQLAnalysis, FileID: "f1"}, func(ctx context.Context, _ func(int64, int64, string)) (int, error) {
				close(started)
				<-ctx.Done()
				return 0, ctx.Err()
			})
			result <- struct {
				value int
				err   error
			}{value: value, err: err}
		}()
		<-started
		svc.cancelJobForFile("f2")
		select {
		case got := <-result:
			t.Fatalf("other-file cancellation ended job: %v", got.err)
		default:
		}
		svc.cancelJobForFile("f1")
		_, err := waitJobResult(t, result)
		if !errors.Is(err, ErrJobCancelled) || !errors.Is(err, context.Canceled) {
			t.Fatalf("file cancellation error = %v", err)
		}
	})

	t.Run("shutdown", func(t *testing.T) {
		svc := NewFileService()
		started := make(chan struct{})
		result := make(chan struct {
			value int
			err   error
		}, 1)
		go func() {
			value, err := runServiceJob(svc, jobSpec{Title: "shutdown test"}, func(ctx context.Context, _ func(int64, int64, string)) (int, error) {
				close(started)
				<-ctx.Done()
				return 0, ctx.Err()
			})
			result <- struct {
				value int
				err   error
			}{value: value, err: err}
		}()
		<-started
		svc.shutdown()
		_, err := waitJobResult(t, result)
		if !errors.Is(err, ErrJobCancelled) {
			t.Fatalf("shutdown cancellation error = %v", err)
		}
		if _, err := runServiceJob(svc, jobSpec{Title: "late"}, func(context.Context, func(int64, int64, string)) (int, error) {
			return 1, nil
		}); !errors.Is(err, ErrServiceStopped) {
			t.Fatalf("post-shutdown error = %v, want ErrServiceStopped", err)
		}
	})
}

func TestServiceJobProgressIsBoundedMonotonicAndLateSafe(t *testing.T) {
	svc := NewFileService()
	mu, events := installJobEventRecorder(t, svc)
	var lateProgress func(int64, int64, string)
	_, err := runServiceJob(svc, jobSpec{Title: "progress", Total: 10}, func(_ context.Context, progress func(int64, int64, string)) (int, error) {
		lateProgress = progress
		progress(-5, -2, strings.Repeat("é", 200))
		progress(math.MaxInt64, 10, "done")
		progress(5, 10, "regressed")
		return 1, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	eventCountAtEnd := len(*events)
	progressEvents := make([]recordedJobEvent, 0, 3)
	for _, event := range *events {
		if event.name == "quarry:job-progress" {
			progressEvents = append(progressEvents, event)
		}
	}
	mu.Unlock()
	if len(progressEvents) != 3 {
		t.Fatalf("progress event count = %d, want 3", len(progressEvents))
	}
	previous := int64(0)
	for _, event := range progressEvents {
		completed, _ := event.data["completed"].(int64)
		total, _ := event.data["total"].(int64)
		note, _ := event.data["note"].(string)
		if completed < previous || completed < 0 || completed > 10 {
			t.Fatalf("invalid progress sequence at %#v", event.data)
		}
		if total != 10 {
			t.Fatalf("progress total = %d, want 10", total)
		}
		if len(note) > maxJobNoteBytes {
			t.Fatalf("note length = %d, limit %d", len(note), maxJobNoteBytes)
		}
		previous = completed
	}
	lateProgress(10, 10, "too late")
	mu.Lock()
	defer mu.Unlock()
	if len(*events) != eventCountAtEnd {
		t.Fatalf("late progress emitted after terminal event: before=%d after=%d", eventCountAtEnd, len(*events))
	}
}

func TestServiceJobEventEmitterFailureDoesNotLeakOwnership(t *testing.T) {
	svc := NewFileService()
	manager := svc.jobs()
	manager.mu.Lock()
	manager.emit = func(string, any) { panic("transport failure") }
	manager.mu.Unlock()
	value, err := runServiceJob(svc, jobSpec{Title: "event failure"}, func(_ context.Context, progress func(int64, int64, string)) (int, error) {
		progress(1, 1, "done")
		return 7, nil
	})
	if err != nil || value != 7 {
		t.Fatalf("job result = %d, %v", value, err)
	}
	manager.mu.Lock()
	defer manager.mu.Unlock()
	if manager.active != nil {
		t.Fatal("event emitter panic leaked active ownership")
	}
}

func TestServiceJobCommitMakesLateUserCancelStale(t *testing.T) {
	svc := NewFileService()
	committed := make(chan struct{})
	release := make(chan struct{})
	ctxCanceled := make(chan struct{}, 1)
	result := make(chan struct {
		value int
		err   error
	}, 1)
	go func() {
		value, err := runServiceJob(svc, jobSpec{Title: "commit"}, func(ctx context.Context, _ func(int64, int64, string)) (int, error) {
			jobID, ok := serviceJobID(ctx)
			if !ok || !svc.jobs().commit(jobID, func() bool { return true }) {
				return 0, errors.New("could not commit")
			}
			close(committed)
			<-release
			select {
			case <-ctx.Done():
				ctxCanceled <- struct{}{}
			default:
			}
			return 9, nil
		})
		result <- struct {
			value int
			err   error
		}{value: value, err: err}
	}()
	<-committed
	jobID := activeJobIDForTest(t, svc)
	if err := svc.CancelJob(jobID); !errors.Is(err, ErrJobAlreadyCommitted) {
		t.Fatalf("late cancel error = %v, want ErrJobAlreadyCommitted", err)
	}
	close(release)
	value, err := waitJobResult(t, result)
	if err != nil || value != 9 {
		t.Fatalf("committed job result = %d, %v", value, err)
	}
	select {
	case <-ctxCanceled:
		t.Fatal("late user cancel canceled committed job context")
	default:
	}
}

func TestServiceJobAtomicOutputCancellationWinsBeforePublication(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "cancel-first.txt")
	svc := NewFileService()
	ready := make(chan struct{})
	continueCommit := make(chan struct{})
	result := make(chan error, 1)
	go func() {
		_, err := runServiceJob(svc, jobSpec{Title: "cancel-first output"}, func(ctx context.Context, _ func(int64, int64, string)) (string, error) {
			out, err := fileio.OpenAtomicOutput(path, nil, 0o600)
			if err != nil {
				return "", err
			}
			defer out.Cleanup()
			if _, err := out.Write([]byte("complete but unpublished")); err != nil {
				return "", err
			}
			close(ready)
			<-continueCommit
			if err := out.CommitContext(ctx); err != nil {
				return "", err
			}
			return path, nil
		})
		result <- err
	}()
	<-ready
	jobID := activeJobIDForTest(t, svc)
	if err := svc.CancelJob(jobID); err != nil {
		t.Fatalf("CancelJob = %v", err)
	}
	close(continueCommit)
	if err := <-result; !errors.Is(err, ErrJobCancelled) || !errors.Is(err, context.Canceled) {
		t.Fatalf("job error = %v, want canceled", err)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("cancel-first race published output: %v", err)
	}
}

func TestServiceJobPublicationErrorMakesCancellationStale(t *testing.T) {
	svc := NewFileService()
	published := make(chan struct{})
	release := make(chan struct{})
	wantPublication := &fileio.PublicationError{
		FinalPath: "uncertain-output.txt",
		Durable:   false,
		Err:       errors.New("simulated directory sync failure"),
	}
	result := make(chan error, 1)
	go func() {
		_, err := runServiceJob(svc, jobSpec{Title: "publication evidence"}, func(ctx context.Context, _ func(int64, int64, string)) (int, error) {
			jobID, ok := serviceJobID(ctx)
			if !ok {
				return 0, errors.New("missing job identity")
			}
			err := svc.jobs().publishOutput(jobID, func() error { return wantPublication })
			close(published)
			<-release
			return 0, err
		})
		result <- err
	}()
	<-published
	jobID := activeJobIDForTest(t, svc)
	if err := svc.CancelJob(jobID); !errors.Is(err, ErrJobAlreadyCommitted) {
		t.Fatalf("CancelJob after PublicationError = %v, want ErrJobAlreadyCommitted", err)
	}
	close(release)
	err := <-result
	var gotPublication *fileio.PublicationError
	if !errors.As(err, &gotPublication) || gotPublication != wantPublication {
		t.Fatalf("job error = %v, want original PublicationError", err)
	}
}

func TestCommittedServiceJobRejectsLifecycleAndShutdownCancellation(t *testing.T) {
	svc := NewFileService()
	committed := make(chan struct{})
	release := make(chan struct{})
	ctxCanceled := make(chan struct{}, 1)
	result := make(chan error, 1)
	go func() {
		_, err := runServiceJob(svc, jobSpec{Title: "committed lifecycle", FileID: "f1"}, func(ctx context.Context, _ func(int64, int64, string)) (int, error) {
			jobID, ok := serviceJobID(ctx)
			if !ok || !svc.jobs().commit(jobID, func() bool { return true }) {
				return 0, errors.New("could not commit")
			}
			close(committed)
			<-release
			select {
			case <-ctx.Done():
				ctxCanceled <- struct{}{}
			default:
			}
			return 1, nil
		})
		result <- err
	}()
	<-committed
	if canceled := svc.jobs().cancelFile("f1"); canceled {
		t.Fatal("lifecycle cancellation claimed a committed job")
	}
	shutdownDone := make(chan struct{})
	go func() {
		svc.shutdown()
		close(shutdownDone)
	}()
	select {
	case <-shutdownDone:
		t.Fatal("shutdown returned before committed job completed")
	case <-time.After(25 * time.Millisecond):
	}
	close(release)
	select {
	case <-shutdownDone:
	case <-time.After(2 * time.Second):
		t.Fatal("shutdown did not join committed job completion")
	}
	if err := <-result; err != nil {
		t.Fatalf("committed job result = %v", err)
	}
	select {
	case <-ctxCanceled:
		t.Fatal("lifecycle/shutdown canceled committed job context")
	default:
	}
}

func TestServiceShutdownJoinsCooperativeJobCleanup(t *testing.T) {
	svc := NewFileService()
	started := make(chan struct{})
	cancelObserved := make(chan struct{})
	releaseCleanup := make(chan struct{})
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(releaseCleanup) }) }
	t.Cleanup(release)
	result := make(chan error, 1)
	go func() {
		_, err := runServiceJob(svc, jobSpec{Title: "shutdown cleanup"}, func(ctx context.Context, _ func(int64, int64, string)) (int, error) {
			close(started)
			<-ctx.Done()
			close(cancelObserved)
			<-releaseCleanup
			return 0, ctx.Err()
		})
		result <- err
	}()
	<-started
	shutdownDone := make(chan struct{})
	go func() {
		svc.shutdown()
		close(shutdownDone)
	}()
	select {
	case <-cancelObserved:
	case <-time.After(2 * time.Second):
		t.Fatal("job did not observe shutdown cancellation")
	}
	select {
	case <-shutdownDone:
		t.Fatal("shutdown returned before cooperative cleanup completed")
	case <-time.After(25 * time.Millisecond):
	}
	release()
	select {
	case <-shutdownDone:
	case <-time.After(2 * time.Second):
		t.Fatal("shutdown did not join completed job cleanup")
	}
	if err := <-result; !errors.Is(err, ErrJobCancelled) || !errors.Is(err, context.Canceled) {
		t.Fatalf("job result = %v, want canceled", err)
	}
}

func TestWaitForServiceCleanupHasOneGlobalTimeout(t *testing.T) {
	never := make(chan struct{})
	started := time.Now()
	waitForServiceCleanup([]<-chan struct{}{never, never}, 20*time.Millisecond)
	elapsed := time.Since(started)
	if elapsed < 10*time.Millisecond || elapsed > time.Second {
		t.Fatalf("bounded cleanup wait took %v", elapsed)
	}
}

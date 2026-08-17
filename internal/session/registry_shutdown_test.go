package session

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestRegistryShutdownBlocksAdmissionCancelsIndexerAndEventuallyClosesHandle(t *testing.T) {
	const source = "alpha\nbeta\n"
	path := filepath.Join(t.TempDir(), "shutdown-source.txt")
	if err := os.WriteFile(path, []byte(source), 0o600); err != nil {
		t.Fatal(err)
	}

	reg := New()
	file, added, err := reg.OpenWithStatus(path)
	if err != nil {
		t.Fatal(err)
	}
	if !added {
		t.Fatal("first open was not installed")
	}

	indexCtx, cancelIndex := context.WithCancel(context.Background())
	indexExited := make(chan struct{})
	go func() {
		<-indexCtx.Done()
		close(indexExited)
	}()
	file.indexMu.Lock()
	file.cancelIndex = cancelIndex
	file.indexMu.Unlock()

	lease, err := reg.AcquireRead(file.ID)
	if err != nil {
		t.Fatal(err)
	}

	deadlineCtx, cancelDeadline := context.WithTimeout(context.Background(), 20*time.Millisecond)
	err = reg.Shutdown(deadlineCtx)
	cancelDeadline()
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("shutdown with retained lease error = %v, want deadline", err)
	}
	select {
	case <-indexExited:
	case <-time.After(time.Second):
		t.Fatal("registry shutdown did not synchronously cancel the active index owner")
	}

	if _, _, err := reg.OpenWithStatus(path); !errors.Is(err, ErrRegistryStopped) {
		t.Fatalf("open after shutdown error = %v, want ErrRegistryStopped", err)
	}
	if _, err := reg.AcquireRead(file.ID); !errors.Is(err, ErrRegistryStopped) {
		t.Fatalf("read lease after shutdown error = %v, want ErrRegistryStopped", err)
	}
	if _, err := reg.AcquireExclusive(file.ID); !errors.Is(err, ErrRegistryStopped) {
		t.Fatalf("exclusive lease after shutdown error = %v, want ErrRegistryStopped", err)
	}
	if _, err := reg.BeginTransition(file.ID); !errors.Is(err, ErrRegistryStopped) {
		t.Fatalf("transition after shutdown error = %v, want ErrRegistryStopped", err)
	}

	lease.Release()
	finishCtx, cancelFinish := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancelFinish()
	if err := reg.CloseAll(finishCtx); err != nil {
		t.Fatalf("eventual shutdown drain: %v", err)
	}
	if _, err := file.Doc.ReadRange(0, 1); err == nil {
		t.Fatal("document handle remained readable after shutdown drain")
	}
	reg.mu.Lock()
	openCount := reg.openCount
	fileCount := len(reg.files)
	reg.mu.Unlock()
	if openCount != 0 || fileCount != 0 {
		t.Fatalf("retained registry state after shutdown: open=%d files=%d", openCount, fileCount)
	}
	got, err := os.ReadFile(path)
	if err != nil || string(got) != source {
		t.Fatalf("source after registry shutdown = %q, %v", got, err)
	}
}

func TestRegistryShutdownEmptyIsIdempotentAndIrreversible(t *testing.T) {
	reg := New()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := reg.Shutdown(ctx); err != nil {
		t.Fatal(err)
	}
	if err := reg.Shutdown(ctx); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "late.txt")
	if err := os.WriteFile(path, []byte("late"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := reg.OpenWithStatus(path); !errors.Is(err, ErrRegistryStopped) {
		t.Fatalf("late open error = %v, want ErrRegistryStopped", err)
	}
}

func TestRegistryShutdownDrainsHandleDetachedByConcurrentClose(t *testing.T) {
	const source = "detached close remains shutdown-owned\n"
	path := filepath.Join(t.TempDir(), "detached-close.txt")
	if err := os.WriteFile(path, []byte(source), 0o600); err != nil {
		t.Fatal(err)
	}

	reg := New()
	file, _, err := reg.OpenWithStatus(path)
	if err != nil {
		t.Fatal(err)
	}
	lease, err := reg.AcquireRead(file.ID)
	if err != nil {
		t.Fatal(err)
	}
	handle, err := reg.BeginClose(file.ID)
	if err != nil {
		lease.Release()
		t.Fatal(err)
	}
	if _, ok := reg.Get(file.ID); ok {
		lease.Release()
		t.Fatal("detached close handle left its file ID available")
	}

	done := reg.BeginShutdown()
	select {
	case <-done:
		lease.Release()
		t.Fatal("shutdown completed while the detached close still had a lease")
	case <-time.After(20 * time.Millisecond):
	}

	// Do not call handle.Finish here: shutdown must adopt and finish every
	// already-detached handle itself.
	lease.Release()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("shutdown did not drain the detached close handle")
	}
	if err := handle.Finish(); err != nil {
		t.Fatalf("idempotent detached handle finish: %v", err)
	}
	if _, err := file.Doc.ReadRange(0, 1); err == nil {
		t.Fatal("detached document handle remained readable after shutdown")
	}
	reg.mu.Lock()
	openCount := reg.openCount
	closingCount := len(reg.closing)
	reg.mu.Unlock()
	if openCount != 0 || closingCount != 0 {
		t.Fatalf("retained state after detached close drain: open=%d closing=%d", openCount, closingCount)
	}
	got, err := os.ReadFile(path)
	if err != nil || string(got) != source {
		t.Fatalf("source after detached close drain = %q, %v", got, err)
	}
}

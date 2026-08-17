package session

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestEditSessionContextCancellationDoesNotInstallSession(t *testing.T) {
	registry := New()
	file, _ := openLeaseTestFile(t, registry, "source bytes")
	lease, err := registry.AcquireExclusive(file.ID)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	edit, err := lease.File().EditSessionContext(ctx)
	if edit != nil || !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled edit session = %v, %v; want nil/context.Canceled", edit, err)
	}
	if lease.File().Edit != nil {
		t.Fatal("canceled fingerprint installed an edit session")
	}
	lease.Release()
	if err := registry.Close(file.ID); err != nil {
		t.Fatal(err)
	}
}

func openLeaseTestFile(t *testing.T, registry *Registry, content string) (*File, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "source.txt")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	file, err := registry.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	return file, path
}

func TestReadLeaseKeepsGenerationAliveUntilCloseDrain(t *testing.T) {
	registry := New()
	file, _ := openLeaseTestFile(t, registry, "stable generation")
	lease, err := registry.AcquireRead(file.ID)
	if err != nil {
		t.Fatal(err)
	}
	handle, err := registry.BeginClose(file.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := registry.AcquireRead(file.ID); !errors.Is(err, ErrUnknownFile) {
		t.Fatalf("new lease after BeginClose = %v, want ErrUnknownFile", err)
	}

	finishStarted := make(chan struct{})
	finishDone := make(chan error, 1)
	go func() {
		close(finishStarted)
		finishDone <- handle.Finish()
	}()
	<-finishStarted
	if got, err := lease.File().Doc.ReadRange(0, 6); err != nil || string(got) != "stable" {
		t.Fatalf("leased generation was closed early: %q, %v", got, err)
	}
	select {
	case err := <-finishDone:
		t.Fatalf("close completed with a live read lease: %v", err)
	case <-time.After(25 * time.Millisecond):
	}

	lease.Release()
	select {
	case err := <-finishDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("close did not finish after the read lease drained")
	}
	if _, err := file.Doc.ReadRange(0, 1); err == nil {
		t.Fatal("closed document still accepted reads")
	}
}

func TestTransitionRejectsNewWorkAndWaitsForReaderBeforeReopen(t *testing.T) {
	registry := New()
	file, path := openLeaseTestFile(t, registry, "generation one")
	readLease, err := registry.AcquireRead(file.ID)
	if err != nil {
		t.Fatal(err)
	}
	transition, err := registry.BeginTransition(file.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer transition.Abort()
	if _, err := registry.AcquireRead(file.ID); !errors.Is(err, ErrTransitioning) {
		t.Fatalf("new read during transition = %v, want ErrTransitioning", err)
	}

	waitDone := make(chan struct {
		lease *Lease
		err   error
	}, 1)
	go func() {
		lease, err := transition.Wait()
		waitDone <- struct {
			lease *Lease
			err   error
		}{lease: lease, err: err}
	}()
	if got, err := readLease.File().Doc.ReadRange(0, 10); err != nil || string(got) != "generation" {
		t.Fatalf("transition closed active reader: %q, %v", got, err)
	}
	readLease.Release()

	var result struct {
		lease *Lease
		err   error
	}
	select {
	case result = <-waitDone:
	case <-time.After(2 * time.Second):
		t.Fatal("transition did not acquire after reader release")
	}
	if result.err != nil {
		t.Fatal(result.err)
	}
	defer result.lease.Release()
	if err := os.WriteFile(path, []byte("generation two"), 0o600); err != nil {
		t.Fatal(err)
	}
	reopened, err := registry.ReopenUnderLease(result.lease)
	if err != nil {
		t.Fatal(err)
	}
	if result.lease.Snapshot().Generation != 2 || reopened != result.lease.File() {
		t.Fatalf("reopened lease = %+v, file=%p reopened=%p", result.lease.Snapshot(), result.lease.File(), reopened)
	}
}

func TestCloseSupersedesPreparedTransitionWithoutDeadlock(t *testing.T) {
	registry := New()
	file, _ := openLeaseTestFile(t, registry, "source")
	transition, err := registry.BeginTransition(file.ID)
	if err != nil {
		t.Fatal(err)
	}
	closeHandle, err := registry.BeginClose(file.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := transition.Wait(); !errors.Is(err, ErrFileClosing) {
		t.Fatalf("transition after close = %v, want ErrFileClosing", err)
	}
	if err := closeHandle.Finish(); err != nil {
		t.Fatal(err)
	}
}

func TestExclusiveLeaseSerializesEditOwnership(t *testing.T) {
	registry := New()
	file, _ := openLeaseTestFile(t, registry, "source")
	first, err := registry.AcquireExclusive(file.ID)
	if err != nil {
		t.Fatal(err)
	}
	secondDone := make(chan *Lease, 1)
	errDone := make(chan error, 1)
	go func() {
		second, err := registry.AcquireExclusive(file.ID)
		if err != nil {
			errDone <- err
			return
		}
		secondDone <- second
	}()
	if _, err := first.File().EditSession(); err != nil {
		t.Fatal(err)
	}
	first.Release()
	select {
	case err := <-errDone:
		t.Fatal(err)
	case second := <-secondDone:
		if second.File().Edit == nil {
			t.Fatal("second exclusive lease did not observe serialized edit state")
		}
		second.Release()
	case <-time.After(2 * time.Second):
		t.Fatal("second exclusive lease did not acquire")
	}
	if err := registry.Close(file.ID); err != nil {
		t.Fatal(err)
	}
}

func TestQueuedExclusiveLeaseHasPriorityOverNewReaders(t *testing.T) {
	registry := New()
	file, _ := openLeaseTestFile(t, registry, "source")
	firstReader, err := registry.AcquireRead(file.ID)
	if err != nil {
		t.Fatal(err)
	}
	writerDone := make(chan struct {
		lease *Lease
		err   error
	}, 1)
	go func() {
		lease, err := registry.AcquireExclusive(file.ID)
		writerDone <- struct {
			lease *Lease
			err   error
		}{lease: lease, err: err}
	}()
	entry := registry.files[file.ID]
	waitForRegistryEntry(t, entry, func(entry *registryEntry) bool { return entry.waitingWriters == 1 }, "writer to queue")

	readerDone := make(chan struct {
		lease *Lease
		err   error
	}, 1)
	go func() {
		lease, err := registry.AcquireRead(file.ID)
		readerDone <- struct {
			lease *Lease
			err   error
		}{lease: lease, err: err}
	}()
	firstReader.Release()

	var writer *Lease
	select {
	case result := <-readerDone:
		if result.lease != nil {
			result.lease.Release()
		}
		t.Fatalf("new reader bypassed the queued writer: %v", result.err)
	case result := <-writerDone:
		if result.err != nil {
			t.Fatal(result.err)
		}
		writer = result.lease
	case <-time.After(2 * time.Second):
		t.Fatal("queued writer did not acquire")
	}
	select {
	case result := <-readerDone:
		if result.lease != nil {
			result.lease.Release()
		}
		t.Fatalf("reader acquired while writer lease was active: %v", result.err)
	default:
	}
	writer.Release()
	select {
	case result := <-readerDone:
		if result.err != nil {
			t.Fatal(result.err)
		}
		result.lease.Release()
	case <-time.After(2 * time.Second):
		t.Fatal("reader did not resume after writer release")
	}
	if err := registry.Close(file.ID); err != nil {
		t.Fatal(err)
	}
}

func TestAcquireReadContextCancellationDoesNotLeakReader(t *testing.T) {
	registry := New()
	file, _ := openLeaseTestFile(t, registry, "source")
	writer, err := registry.AcquireExclusive(file.ID)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	readerDone := make(chan error, 1)
	go func() {
		lease, err := registry.AcquireReadContext(ctx, file.ID)
		if lease != nil {
			lease.Release()
		}
		readerDone <- err
	}()
	entry := registry.files[file.ID]
	waitForRegistryEntry(t, entry, func(entry *registryEntry) bool { return entry.writer }, "writer ownership")
	cancel()
	select {
	case err := <-readerDone:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("queued reader error = %v, want context.Canceled", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("queued reader did not wake after cancellation")
	}
	entry.mu.Lock()
	readers := entry.readers
	entry.mu.Unlock()
	if readers != 0 {
		t.Fatalf("reader count after cancellation = %d, want 0", readers)
	}
	writer.Release()
	probe, err := registry.AcquireRead(file.ID)
	if err != nil {
		t.Fatalf("reader admission remained blocked: %v", err)
	}
	probe.Release()
	if err := registry.Close(file.ID); err != nil {
		t.Fatal(err)
	}
}

func TestAcquireExclusiveContextCancellationClearsWriterPriority(t *testing.T) {
	registry := New()
	file, _ := openLeaseTestFile(t, registry, "source")
	firstReader, err := registry.AcquireRead(file.ID)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	writerDone := make(chan error, 1)
	go func() {
		lease, err := registry.AcquireExclusiveContext(ctx, file.ID)
		if lease != nil {
			lease.Release()
		}
		writerDone <- err
	}()
	entry := registry.files[file.ID]
	waitForRegistryEntry(t, entry, func(entry *registryEntry) bool { return entry.waitingWriters == 1 }, "cancellable writer to queue")
	cancel()
	select {
	case err := <-writerDone:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("queued writer error = %v, want context.Canceled", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("queued writer did not wake after cancellation")
	}
	waitForRegistryEntry(t, entry, func(entry *registryEntry) bool { return entry.waitingWriters == 0 }, "canceled writer accounting to clear")
	secondReader, err := registry.AcquireRead(file.ID)
	if err != nil {
		t.Fatalf("canceled writer left reader priority blocked: %v", err)
	}
	secondReader.Release()
	firstReader.Release()
	if err := registry.Close(file.ID); err != nil {
		t.Fatal(err)
	}
}

func TestQueuedExclusiveLeaseIsInterruptedByTransition(t *testing.T) {
	registry := New()
	file, _ := openLeaseTestFile(t, registry, "source")
	reader, err := registry.AcquireRead(file.ID)
	if err != nil {
		t.Fatal(err)
	}
	writerErr := make(chan error, 1)
	go func() {
		lease, err := registry.AcquireExclusive(file.ID)
		if lease != nil {
			lease.Release()
		}
		writerErr <- err
	}()
	entry := registry.files[file.ID]
	waitForRegistryEntry(t, entry, func(entry *registryEntry) bool { return entry.waitingWriters == 1 }, "writer to queue")
	transition, err := registry.BeginTransition(file.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer transition.Abort()
	select {
	case err := <-writerErr:
		if !errors.Is(err, ErrTransitioning) {
			t.Fatalf("queued writer error = %v, want ErrTransitioning", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("queued writer did not observe transition")
	}
	reader.Release()
	lease, err := transition.Wait()
	if err != nil {
		t.Fatal(err)
	}
	lease.Release()
	if err := registry.Close(file.ID); err != nil {
		t.Fatal(err)
	}
}

func TestQueuedExclusiveLeaseIsInterruptedByClose(t *testing.T) {
	registry := New()
	file, _ := openLeaseTestFile(t, registry, "source")
	reader, err := registry.AcquireRead(file.ID)
	if err != nil {
		t.Fatal(err)
	}
	writerErr := make(chan error, 1)
	go func() {
		lease, err := registry.AcquireExclusive(file.ID)
		if lease != nil {
			lease.Release()
		}
		writerErr <- err
	}()
	entry := registry.files[file.ID]
	waitForRegistryEntry(t, entry, func(entry *registryEntry) bool { return entry.waitingWriters == 1 }, "writer to queue")
	handle, err := registry.BeginClose(file.ID)
	if err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-writerErr:
		if !errors.Is(err, ErrFileClosing) {
			t.Fatalf("queued writer error = %v, want ErrFileClosing", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("queued writer did not observe close")
	}
	reader.Release()
	if err := handle.Finish(); err != nil {
		t.Fatal(err)
	}
}

func TestLeaseReleaseIsIdempotent(t *testing.T) {
	registry := New()
	file, _ := openLeaseTestFile(t, registry, "source")
	read, err := registry.AcquireRead(file.ID)
	if err != nil {
		t.Fatal(err)
	}
	read.Release()
	read.Release()
	entry := registry.files[file.ID]
	entry.mu.Lock()
	readers := entry.readers
	entry.mu.Unlock()
	if readers != 0 {
		t.Fatalf("reader count after duplicate Release = %d, want 0", readers)
	}
	exclusive, err := registry.AcquireExclusive(file.ID)
	if err != nil {
		t.Fatal(err)
	}
	exclusive.Release()
	exclusive.Release()
	entry.mu.Lock()
	writer := entry.writer
	entry.mu.Unlock()
	if writer {
		t.Fatal("writer remained active after duplicate Release")
	}
	if err := registry.Close(file.ID); err != nil {
		t.Fatal(err)
	}
}

func TestCloseWaitsForActiveTransitionLease(t *testing.T) {
	registry := New()
	file, _ := openLeaseTestFile(t, registry, "source")
	transition, err := registry.BeginTransition(file.ID)
	if err != nil {
		t.Fatal(err)
	}
	lease, err := transition.Wait()
	if err != nil {
		t.Fatal(err)
	}
	closeStarted := make(chan struct{})
	closeDone := make(chan error, 1)
	go func() {
		handle, err := registry.BeginClose(file.ID)
		if err != nil {
			closeDone <- err
			return
		}
		close(closeStarted)
		closeDone <- handle.Finish()
	}()
	select {
	case <-closeStarted:
	case err := <-closeDone:
		t.Fatalf("close setup failed: %v", err)
	case <-time.After(2 * time.Second):
		t.Fatal("close did not begin")
	}
	if _, err := registry.ReopenUnderLease(lease); !errors.Is(err, ErrFileClosing) {
		t.Fatalf("reopen under close-superseded lease = %v, want ErrFileClosing", err)
	}
	select {
	case err := <-closeDone:
		t.Fatalf("close completed before transition lease release: %v", err)
	default:
	}
	lease.Release()
	select {
	case err := <-closeDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("close did not finish after transition lease release")
	}
}

func TestBeginCloseUnderTransitionHandsOffExclusiveLease(t *testing.T) {
	registry := New()
	file, _ := openLeaseTestFile(t, registry, "source")
	transition, err := registry.BeginTransition(file.ID)
	if err != nil {
		t.Fatal(err)
	}
	lease, err := transition.Wait()
	if err != nil {
		t.Fatal(err)
	}
	handle, err := registry.BeginCloseUnderTransition(lease)
	if err != nil {
		lease.Release()
		t.Fatal(err)
	}
	if _, err := registry.AcquireRead(file.ID); !errors.Is(err, ErrUnknownFile) {
		t.Fatalf("lease after close handoff = %v, want ErrUnknownFile", err)
	}
	finishDone := make(chan error, 1)
	go func() { finishDone <- handle.Finish() }()
	select {
	case err := <-finishDone:
		t.Fatalf("close finished before transition lease release: %v", err)
	case <-time.After(25 * time.Millisecond):
	}
	lease.Release()
	select {
	case err := <-finishDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("close did not finish after transition lease release")
	}
}

func waitForRegistryEntry(t *testing.T, entry *registryEntry, predicate func(*registryEntry) bool, description string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		entry.mu.Lock()
		ready := predicate(entry)
		entry.mu.Unlock()
		if ready {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", description)
		}
		time.Sleep(time.Millisecond)
	}
}

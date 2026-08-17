package session

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/quarry/quarry-wails3/internal/manualedit"
)

func TestRegistryLimitRejectsInsteadOfEvictingDirtySession(t *testing.T) {
	dir := t.TempDir()
	paths := []string{
		filepath.Join(dir, "one.txt"),
		filepath.Join(dir, "two.txt"),
		filepath.Join(dir, "three.txt"),
	}
	for _, path := range paths {
		if err := os.WriteFile(path, []byte("abc"), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	registry := NewWithLimit(2)
	first, err := registry.Open(paths[0])
	if err != nil {
		t.Fatal(err)
	}
	second, err := registry.Open(paths[1])
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = registry.Close(second.ID) }()

	lease, err := registry.AcquireExclusive(second.ID)
	if err != nil {
		t.Fatal(err)
	}
	edit, err := lease.File().EditSession()
	if err != nil {
		lease.Release()
		t.Fatal(err)
	}
	if err := edit.ApplyVerifiedEdit(manualedit.Edit{Start: 1, End: 2, Text: []byte("X")}, []byte("b")); err != nil {
		lease.Release()
		t.Fatal(err)
	}
	lease.Release()

	alias, added, err := registry.OpenWithStatus(paths[0])
	if err != nil || added || alias != first {
		t.Fatalf("alias at capacity = %#v, added %v, error %v; want retained first session", alias, added, err)
	}
	if _, err := registry.Open(paths[2]); !errors.Is(err, ErrOpenFileLimit) {
		t.Fatalf("third Open error = %v, want ErrOpenFileLimit", err)
	}

	retained, err := registry.AcquireRead(second.ID)
	if err != nil {
		t.Fatalf("dirty session was evicted: %v", err)
	}
	if retained.File().Edit == nil || !retained.File().Edit.HasEdits() {
		retained.Release()
		t.Fatal("dirty session lost its staged edit after capacity rejection")
	}
	retained.Release()
	if data, err := os.ReadFile(paths[1]); err != nil || string(data) != "abc" {
		t.Fatalf("dirty source = %q, %v; want unchanged", data, err)
	}

	if err := registry.Close(first.ID); err != nil {
		t.Fatal(err)
	}
	third, err := registry.Open(paths[2])
	if err != nil {
		t.Fatalf("Open after releasing a slot: %v", err)
	}
	if err := registry.Close(third.ID); err != nil {
		t.Fatal(err)
	}
}

func TestRegistryLimitCountsClosingHandleUntilItIsActuallyClosed(t *testing.T) {
	dir := t.TempDir()
	firstPath := filepath.Join(dir, "one.txt")
	secondPath := filepath.Join(dir, "two.txt")
	for _, path := range []string{firstPath, secondPath} {
		if err := os.WriteFile(path, []byte("content"), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	registry := NewWithLimit(1)
	first, err := registry.Open(firstPath)
	if err != nil {
		t.Fatal(err)
	}
	lease, err := registry.AcquireRead(first.ID)
	if err != nil {
		t.Fatal(err)
	}
	handle, err := registry.BeginClose(first.ID)
	if err != nil {
		lease.Release()
		t.Fatal(err)
	}
	finished := make(chan error, 1)
	started := make(chan struct{})
	go func() {
		close(started)
		finished <- handle.Finish()
	}()
	<-started

	if _, err := registry.Open(secondPath); !errors.Is(err, ErrOpenFileLimit) {
		lease.Release()
		t.Fatalf("Open during draining close = %v, want ErrOpenFileLimit", err)
	}
	select {
	case err := <-finished:
		lease.Release()
		t.Fatalf("Finish returned before the retained lease was released: %v", err)
	case <-time.After(20 * time.Millisecond):
	}

	lease.Release()
	select {
	case err := <-finished:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Finish did not complete after lease release")
	}
	second, err := registry.Open(secondPath)
	if err != nil {
		t.Fatalf("Open after close completed: %v", err)
	}
	if err := registry.Close(second.ID); err != nil {
		t.Fatal(err)
	}
}

func TestRegistryLimitsConcurrentPreinstallationOpenHandles(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bounded-open.txt")
	if err := os.WriteFile(path, []byte("content"), 0o600); err != nil {
		t.Fatal(err)
	}

	registry := NewWithLimit(DefaultMaxOpenFiles)
	registry.mu.Lock()
	registry.opening = registry.maxOpening
	registry.mu.Unlock()
	if _, _, err := registry.OpenWithStatus(path); !errors.Is(err, ErrOpenInFlightLimit) {
		t.Fatalf("Open at concurrent-open ceiling = %v, want ErrOpenInFlightLimit", err)
	}
	registry.mu.Lock()
	opening := registry.opening
	openCount := registry.openCount
	registry.mu.Unlock()
	if opening != registry.maxOpening || openCount != 0 {
		t.Fatalf("rejected concurrent open changed ownership: opening=%d max=%d retained=%d", opening, registry.maxOpening, openCount)
	}

	registry.mu.Lock()
	registry.opening--
	registry.mu.Unlock()
	file, added, err := registry.OpenWithStatus(path)
	if err != nil || !added {
		t.Fatalf("Open after releasing an in-flight slot = %#v, added %v, error %v", file, added, err)
	}
	if err := registry.Close(file.ID); err != nil {
		t.Fatal(err)
	}
}

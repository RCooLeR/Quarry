package document

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/quarry/quarry-wails3/internal/lineindex"
)

func TestConcurrentStartIndexingIsSingleFlight(t *testing.T) {
	oldPersist := persistentIndexCacheEnabled()
	SetPersistentIndexCache(false)
	defer SetPersistentIndexCache(oldPersist)
	path := filepath.Join(t.TempDir(), "source.txt")
	if err := os.WriteFile(path, []byte("one\ntwo\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	doc, err := OpenFile(path)
	if err != nil {
		t.Fatal(err)
	}
	defer doc.Close()

	originalBuild := buildDocumentLineIndex
	started := make(chan struct{})
	release := make(chan struct{})
	var calls atomic.Int32
	buildDocumentLineIndex = func(idx *lineindex.Index, ctx context.Context, reader io.Reader, encoding string) error {
		if calls.Add(1) == 1 {
			close(started)
		}
		<-release
		return originalBuild(idx, ctx, reader, encoding)
	}
	t.Cleanup(func() { buildDocumentLineIndex = originalBuild })

	start := make(chan struct{})
	errs := make(chan error, 2)
	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			errs <- doc.StartIndexing(context.Background())
		}()
	}
	close(start)
	<-started
	close(release)
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("StartIndexing: %v", err)
		}
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("build calls = %d, want one", got)
	}
}

func TestCloseWaitsForActiveIndexBuild(t *testing.T) {
	oldPersist := persistentIndexCacheEnabled()
	SetPersistentIndexCache(false)
	defer SetPersistentIndexCache(oldPersist)
	path := filepath.Join(t.TempDir(), "source.txt")
	if err := os.WriteFile(path, []byte("one\ntwo\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	doc, err := OpenFile(path)
	if err != nil {
		t.Fatal(err)
	}

	originalBuild := buildDocumentLineIndex
	started := make(chan struct{})
	release := make(chan struct{})
	buildDocumentLineIndex = func(idx *lineindex.Index, ctx context.Context, reader io.Reader, encoding string) error {
		close(started)
		<-release
		return originalBuild(idx, ctx, reader, encoding)
	}
	t.Cleanup(func() { buildDocumentLineIndex = originalBuild })

	indexErr := make(chan error, 1)
	go func() { indexErr <- doc.StartIndexing(context.Background()) }()
	<-started
	closeStarted := make(chan struct{})
	closeErr := make(chan error, 1)
	go func() {
		close(closeStarted)
		closeErr <- doc.Close()
	}()
	<-closeStarted

	select {
	case err := <-closeErr:
		t.Fatalf("Close returned before active index build exited: %v", err)
	case <-time.After(20 * time.Millisecond):
	}
	close(release)
	if err := <-indexErr; !errors.Is(err, errDocumentClosed) {
		t.Fatalf("index error = %v, want errDocumentClosed", err)
	}
	if err := <-closeErr; err != nil {
		t.Fatalf("Close: %v", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("source missing after close: %v", err)
	}
}

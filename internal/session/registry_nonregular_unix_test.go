//go:build !windows

package session

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/quarry/quarry-wails3/internal/document"
	"golang.org/x/sys/unix"
)

func TestRejectedNonRegularOpenReleasesConcurrentOpenCapacity(t *testing.T) {
	dir := t.TempDir()
	fifoPath := filepath.Join(dir, "source.fifo")
	if err := unix.Mkfifo(fifoPath, 0o600); err != nil {
		t.Fatal(err)
	}
	regularPath := filepath.Join(dir, "source.txt")
	if err := os.WriteFile(regularPath, []byte("regular\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	registry := NewWithLimit(1)
	rejected := make(chan error, 1)
	go func() {
		_, _, err := registry.OpenWithStatus(fifoPath)
		rejected <- err
	}()
	select {
	case err := <-rejected:
		if !errors.Is(err, document.ErrNonRegularSource) {
			t.Fatalf("OpenWithStatus(FIFO) error = %v, want document.ErrNonRegularSource", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("OpenWithStatus(FIFO) waited for a writer")
	}

	registry.mu.Lock()
	opening := registry.opening
	retained := registry.openCount
	registry.mu.Unlock()
	if opening != 0 || retained != 0 {
		t.Fatalf("rejected open retained registry capacity: opening=%d retained=%d", opening, retained)
	}

	file, added, err := registry.OpenWithStatus(regularPath)
	if err != nil || !added {
		t.Fatalf("regular open after rejection = %#v, added %v, error %v", file, added, err)
	}
	if err := registry.Close(file.ID); err != nil {
		t.Fatal(err)
	}
}

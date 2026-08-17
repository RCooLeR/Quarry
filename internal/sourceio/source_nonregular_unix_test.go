//go:build !windows

package sourceio

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/quarry/quarry-wails3/internal/regularfile"
	"golang.org/x/sys/unix"
)

func TestOpenContextRejectsFIFOWithoutWaitingForWriter(t *testing.T) {
	path := filepath.Join(t.TempDir(), "source.fifo")
	if err := unix.Mkfifo(path, 0o600); err != nil {
		t.Fatal(err)
	}

	type result struct {
		handle *Handle
		err    error
	}
	done := make(chan result, 1)
	go func() {
		handle, err := OpenContext(context.Background(), path, nil)
		done <- result{handle: handle, err: err}
	}()
	select {
	case result := <-done:
		if result.handle != nil {
			_ = result.handle.Close()
		}
		if !errors.Is(result.err, regularfile.ErrNotRegular) {
			t.Fatalf("OpenContext(FIFO) error = %v, want regularfile.ErrNotRegular", result.err)
		}
		if _, err := os.Lstat(path); err != nil {
			t.Fatalf("rejected FIFO was mutated: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("OpenContext blocked waiting for a FIFO writer")
	}
}

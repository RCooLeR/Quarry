//go:build !windows

package inplace

import (
	"path/filepath"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func TestSyncParentDirectoryRejectsFIFOWithoutWaitingForWriter(t *testing.T) {
	fifoDir := filepath.Join(t.TempDir(), "parent.fifo")
	if err := unix.Mkfifo(fifoDir, 0o600); err != nil {
		t.Fatal(err)
	}

	done := make(chan error, 1)
	go func() { done <- syncParentDirectory(filepath.Join(fifoDir, "source.txt")) }()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("syncParentDirectory accepted a FIFO parent")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("syncParentDirectory blocked waiting for a FIFO writer")
	}
}

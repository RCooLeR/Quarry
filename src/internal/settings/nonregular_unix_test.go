//go:build !windows

package settings

import (
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/quarry/quarry-wails3/internal/regularfile"
	"golang.org/x/sys/unix"
)

func TestLoadFileRejectsFIFOWithoutWaitingForWriter(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settings.fifo")
	if err := unix.Mkfifo(path, 0o600); err != nil {
		t.Fatal(err)
	}

	done := make(chan error, 1)
	go func() {
		_, err := LoadFile(path)
		done <- err
	}()
	select {
	case err := <-done:
		if !errors.Is(err, regularfile.ErrNotRegular) {
			t.Fatalf("LoadFile(FIFO) error = %v, want regularfile.ErrNotRegular", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("LoadFile blocked waiting for a FIFO writer")
	}
}

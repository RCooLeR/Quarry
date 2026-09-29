//go:build !windows

package manualedit

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

func TestApplyFileEditRejectsFIFOWithoutCreatingOutput(t *testing.T) {
	dir := t.TempDir()
	source := filepath.Join(dir, "source.fifo")
	output := filepath.Join(dir, "output.txt")
	if err := unix.Mkfifo(source, 0o600); err != nil {
		t.Fatal(err)
	}

	done := make(chan error, 1)
	go func() {
		_, err := ApplyFileEdit(context.Background(), source, output, Edit{Start: 0, End: 0, Text: []byte("x")}, FileOptions{})
		done <- err
	}()
	select {
	case err := <-done:
		if !errors.Is(err, regularfile.ErrNotRegular) {
			t.Fatalf("ApplyFileEdit(FIFO) error = %v, want regularfile.ErrNotRegular", err)
		}
		if _, statErr := os.Lstat(output); !errors.Is(statErr, os.ErrNotExist) {
			t.Fatalf("rejected FIFO created output: %v", statErr)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("ApplyFileEdit blocked waiting for a FIFO writer")
	}
}

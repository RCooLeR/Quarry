//go:build !windows

package inplace

import (
	"errors"
	"path/filepath"
	"testing"

	"golang.org/x/sys/unix"
)

func TestRecoverRejectsFIFOWithoutOpeningIt(t *testing.T) {
	dir := t.TempDir()
	sidecarPath := filepath.Join(dir, "recovery.fifo")
	if err := unix.Mkfifo(sidecarPath, 0o600); err != nil {
		t.Skipf("FIFO creation is unavailable: %v", err)
	}
	rolledBack, err := Recover(filepath.Join(dir, "source.bin"), sidecarPath)
	if rolledBack || !errors.Is(err, ErrUnsafeSidecar) {
		t.Fatalf("Recover FIFO = %v, %v; want false/ErrUnsafeSidecar", rolledBack, err)
	}
}

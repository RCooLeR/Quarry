//go:build !darwin && !linux && !windows

package fileio

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestPublishExistingNoClobberFailsClosedOnUnsupportedHost(t *testing.T) {
	dir := t.TempDir()
	tempPath := filepath.Join(dir, "output.txt.quarry.tmp")
	finalPath := filepath.Join(dir, "output.txt")
	if err := os.WriteFile(tempPath, []byte("complete output"), 0o600); err != nil {
		t.Fatal(err)
	}

	err := PublishExistingNoClobber(tempPath, finalPath)
	if !errors.Is(err, ErrAtomicNoClobberUnavailable) {
		t.Fatalf("publication error = %v, want ErrAtomicNoClobberUnavailable", err)
	}
	assertFileContents(t, tempPath, "complete output")
	if _, statErr := os.Lstat(finalPath); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("final path error = %v, want no publication", statErr)
	}
}

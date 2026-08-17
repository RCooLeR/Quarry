//go:build !windows

package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestInspectInPlaceRecoveryPreservesTrailingSpaceOnPOSIX(t *testing.T) {
	path := filepath.Join(t.TempDir(), "source.txt ")
	if err := os.WriteFile(path, []byte("source"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(sidecarPath(path), []byte("invalid evidence"), 0o600); err != nil {
		t.Fatal(err)
	}
	state, err := NewFileService().InspectInPlaceRecovery(path)
	if err != nil {
		t.Fatal(err)
	}
	if state.SourcePath != path || state.SidecarPath != sidecarPath(path) || !state.Detected {
		t.Fatalf("trailing-space path changed: %+v", state)
	}
}

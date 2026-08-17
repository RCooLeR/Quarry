package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/quarry/quarry-wails3/internal/document"
)

func TestGetTailWindowReturnsLastRecordsInsteadOfEmptyEOF(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "tail.log")
	var content strings.Builder
	for i := 1; i <= 200; i++ {
		fmt.Fprintf(&content, "record-%03d\n", i)
	}
	if err := os.WriteFile(path, []byte(content.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	service := NewFileService()
	meta, err := service.OpenFile(path)
	if err != nil {
		t.Fatal(err)
	}
	defer service.CloseFile(meta.FileID)

	window, err := service.GetTailWindow(meta.FileID, 64)
	if err != nil {
		t.Fatal(err)
	}
	if window.Text == "" || !strings.Contains(window.Text, "record-200") {
		t.Fatalf("tail window = %q, want final record", window.Text)
	}
	if !window.AtEOF || window.StartByte >= meta.Size {
		t.Fatalf("tail metadata = %+v, want non-empty EOF window", window)
	}
}

func TestGetTailWindowHandlesEmptyFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "empty.log")
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	service := NewFileService()
	meta, err := service.OpenFile(path)
	if err != nil {
		t.Fatal(err)
	}
	defer service.CloseFile(meta.FileID)
	window, err := service.GetTailWindow(meta.FileID, 64)
	if err != nil {
		t.Fatal(err)
	}
	if window.Text != "" || !window.AtBOF || !window.AtEOF {
		t.Fatalf("empty tail window = %+v", window)
	}
}

func TestFileStateDetectsChangeAndRefreshRebindsGeneration(t *testing.T) {
	path := filepath.Join(t.TempDir(), "follow.log")
	if err := os.WriteFile(path, []byte("one\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	service := NewFileService()
	meta, err := service.OpenFile(path)
	if err != nil {
		t.Fatal(err)
	}
	defer service.CloseFile(meta.FileID)

	initial, err := service.FileState(meta.FileID)
	if err != nil {
		t.Fatal(err)
	}
	if initial.ChangedFromOpen || !initial.SameOpenedFile {
		t.Fatalf("initial state = %+v", initial)
	}
	if err := os.WriteFile(path, []byte("one\ntwo\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	changed, err := service.FileState(meta.FileID)
	if err != nil {
		t.Fatal(err)
	}
	if !changed.ChangedFromOpen || changed.Size <= initial.Size {
		t.Fatalf("changed state = %+v, initial = %+v", changed, initial)
	}
	refreshed, err := service.RefreshFile(meta.FileID)
	if err != nil {
		t.Fatal(err)
	}
	if refreshed.Size != changed.Size {
		t.Fatalf("refreshed metadata size = %d, want %d", refreshed.Size, changed.Size)
	}
	rebound, err := service.FileState(meta.FileID)
	if err != nil {
		t.Fatal(err)
	}
	if rebound.ChangedFromOpen || !rebound.SameOpenedFile {
		t.Fatalf("rebound state = %+v", rebound)
	}
}

func TestFileStateDetectsRenameAndRecreateRotation(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "follow.log")
	rotated := filepath.Join(dir, "follow.log.1")
	if err := os.WriteFile(path, []byte("old\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	service := NewFileService()
	meta, err := service.OpenFile(path)
	if err != nil {
		t.Fatal(err)
	}
	defer service.CloseFile(meta.FileID)
	if err := os.Rename(path, rotated); err != nil {
		t.Skipf("platform/filesystem cannot rotate an open file: %v", err)
	}
	if err := os.WriteFile(path, []byte("new\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	state, err := service.FileState(meta.FileID)
	if err != nil {
		t.Fatal(err)
	}
	if state.SameOpenedFile {
		t.Fatalf("rotation state = %+v, want pathname identity mismatch", state)
	}
}

func TestFileStateDetectsSameSizeRewriteWithRestoredModTime(t *testing.T) {
	path := filepath.Join(t.TempDir(), "follow.log")
	const original = "one\ntwo\n"
	const rewritten = "ONE\ntwo\n"
	if err := os.WriteFile(path, []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}
	openedInfo, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	service := NewFileService()
	meta, err := service.OpenFile(path)
	if err != nil {
		t.Fatal(err)
	}
	defer service.CloseFile(meta.FileID)

	file, ok := service.reg.Get(meta.FileID)
	if !ok {
		t.Fatal("opened session is missing")
	}
	if !file.Doc.HasMutationGeneration() {
		t.Skip("filesystem does not expose a strong mutation generation")
	}
	if err := os.WriteFile(path, []byte(rewritten), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, openedInfo.ModTime(), openedInfo.ModTime()); err != nil {
		t.Fatal(err)
	}

	state, err := service.FileState(meta.FileID)
	if err != nil {
		t.Fatal(err)
	}
	if !state.SameOpenedFile || !state.ChangedFromOpen || state.Size != int64(len(original)) || state.ModTimeNanos != openedInfo.ModTime().UnixNano() {
		t.Fatalf("restored-time rewrite state = %+v, want same file with changed generation", state)
	}
	if _, err := file.Doc.ReadRange(0, 1); !errors.Is(err, document.ErrSourceChanged) {
		t.Fatalf("retained document read error = %v, want ErrSourceChanged", err)
	}
}

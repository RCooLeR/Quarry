package main

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/quarry/quarry-wails3/internal/document"
	"github.com/quarry/quarry-wails3/internal/session"
)

func TestFileServiceOpenDeduplicatesIdentityAndPreservesFirstPath(t *testing.T) {
	workingDir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	dir, err := os.MkdirTemp(workingDir, ".fileservice-open-identity-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	path := filepath.Join(dir, "source.txt")
	if err := os.WriteFile(path, []byte("source\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	relativePath, err := filepath.Rel(workingDir, path)
	if err != nil {
		t.Fatal(err)
	}
	svc := NewFileService()
	first, err := svc.OpenFile(path)
	if err != nil {
		t.Fatal(err)
	}
	second, err := svc.OpenFile(relativePath)
	if err != nil {
		t.Fatal(err)
	}
	if second.FileID != first.FileID {
		t.Fatalf("alias file IDs = %q and %q, want one session", first.FileID, second.FileID)
	}
	if first.Path != path || second.Path != path {
		t.Fatalf("open paths = %q and %q, want preserved first spelling %q", first.Path, second.Path, path)
	}
	if err := svc.CloseFile(first.FileID); err != nil {
		t.Fatal(err)
	}
}

func TestFileServiceConcurrentOpenReturnsOneSession(t *testing.T) {
	path := filepath.Join(t.TempDir(), "source.txt")
	if err := os.WriteFile(path, []byte("source\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	svc := NewFileService()
	// Exercise the maximum admitted candidate population. Additional callers
	// are intentionally rejected before opening another OS handle and are
	// covered by the registry limit regression.
	const workers = session.DefaultMaxConcurrentOpens
	type result struct {
		meta FileMeta
		err  error
	}
	start := make(chan struct{})
	results := make(chan result, workers)
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			meta, err := svc.OpenFile(path)
			results <- result{meta: meta, err: err}
		}()
	}
	close(start)
	wg.Wait()
	close(results)
	var fileID string
	for result := range results {
		if result.err != nil {
			t.Fatal(result.err)
		}
		if fileID == "" {
			fileID = result.meta.FileID
		}
		if result.meta.FileID != fileID {
			t.Fatalf("concurrent OpenFile IDs = %q and %q", fileID, result.meta.FileID)
		}
	}
	if fileID == "" {
		t.Fatal("concurrent OpenFile returned no metadata")
	}
	if paths := svc.reg.Paths(); len(paths) != 1 || paths[0] != path {
		t.Fatalf("registry paths = %q, want [%q]", paths, path)
	}
	if err := svc.CloseFile(fileID); err != nil {
		t.Fatal(err)
	}
}

func TestFileServiceAliasOpenChecksRecoveryAtRetainedPath(t *testing.T) {
	dir := t.TempDir()
	firstPath := filepath.Join(dir, "first.txt")
	aliasPath := filepath.Join(dir, "alias.txt")
	if err := os.WriteFile(firstPath, []byte("source\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(firstPath, aliasPath); err != nil {
		t.Fatal(err)
	}
	svc := NewFileService()
	first, err := svc.OpenFile(firstPath)
	if err != nil {
		t.Fatal(err)
	}
	evidence := []byte("unresolved recovery evidence")
	if err := os.WriteFile(sidecarPath(firstPath), evidence, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.OpenFile(aliasPath); !errors.Is(err, ErrInPlaceRecoveryPending) {
		t.Fatalf("alias open error = %v, want ErrInPlaceRecoveryPending", err)
	}
	if _, err := svc.FileSize(first.FileID); err != nil {
		t.Fatalf("blocked alias open damaged existing session: %v", err)
	}
	if got, err := os.ReadFile(sidecarPath(firstPath)); err != nil || !bytes.Equal(got, evidence) {
		t.Fatalf("recovery evidence changed: %q, %v", got, err)
	}
	if _, err := os.Lstat(sidecarPath(aliasPath)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("alias recovery path unexpectedly changed: %v", err)
	}
	if err := svc.CloseFile(first.FileID); err != nil {
		t.Fatal(err)
	}
}

func TestRefreshFileRejectsCandidateDetachedBeforeInstallation(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "rotating.log")
	detached := filepath.Join(dir, "rotating.old")
	if err := os.WriteFile(path, []byte("generation one\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	svc := NewFileService()
	meta, err := svc.OpenFile(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if _, retained := svc.reg.Get(meta.FileID); retained {
			_ = svc.CloseFile(meta.FileID)
		}
	})
	before, ok := svc.reg.Snapshot(meta.FileID)
	if !ok {
		t.Fatal("opened session snapshot is missing")
	}

	hookCalls := 0
	svc.refreshFileCandidate = func(candidate *session.File) {
		if candidate == nil || candidate.ID != meta.FileID {
			return
		}
		hookCalls++
		if err := os.Rename(path, detached); err != nil {
			t.Fatalf("detach refresh candidate: %v", err)
		}
		if err := os.WriteFile(path, []byte("generation two\n"), 0o600); err != nil {
			t.Fatalf("install rotated source: %v", err)
		}
	}
	refreshed, err := svc.RefreshFile(meta.FileID)
	if refreshed != (FileMeta{}) || !errors.Is(err, document.ErrSourceChanged) {
		t.Fatalf("RefreshFile = %+v, %v; want detached-candidate source-change refusal", refreshed, err)
	}
	if hookCalls != 1 {
		t.Fatalf("candidate hook calls = %d, want 1", hookCalls)
	}
	after, ok := svc.reg.Snapshot(meta.FileID)
	if !ok || after.Generation != before.Generation || after.Doc != before.Doc {
		t.Fatalf("detached candidate replaced generation: before=%+v after=%+v present=%v", before, after, ok)
	}
	if got, readErr := os.ReadFile(path); readErr != nil || string(got) != "generation two\n" {
		t.Fatalf("rotated source changed: %q, %v", got, readErr)
	}
}

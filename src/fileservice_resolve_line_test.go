package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/quarry/quarry-wails3/internal/document"
)

func TestResolveLineDistinguishesZeroNotReadyAndMissing(t *testing.T) {
	path := filepath.Join(t.TempDir(), "lines.txt")
	if err := os.WriteFile(path, []byte("one\ntwo\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	service := NewFileService()
	opened, err := service.reg.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer service.CloseFile(opened.ID)

	lineOne, err := service.ResolveLine(opened.ID, 1)
	if err != nil {
		t.Fatal(err)
	}
	if !lineOne.Found || !lineOne.Exact || lineOne.Offset != 0 || lineOne.ResolvedLine != 1 || lineOne.Limited {
		t.Fatalf("line one resolution = %+v, want exact found offset zero", lineOne)
	}

	notReady, err := service.ResolveLine(opened.ID, 99)
	if err != nil {
		t.Fatal(err)
	}
	if notReady.Found || notReady.IndexComplete {
		t.Fatalf("not-ready resolution = %+v", notReady)
	}

	if err := opened.Doc.StartIndexing(context.Background()); err != nil {
		t.Fatal(err)
	}
	missing, err := service.ResolveLine(opened.ID, 99)
	if err != nil {
		t.Fatal(err)
	}
	if missing.Found || !missing.IndexComplete || missing.Limited {
		t.Fatalf("missing-line resolution = %+v", missing)
	}
	lineTwo, err := service.ResolveLine(opened.ID, 2)
	if err != nil {
		t.Fatal(err)
	}
	if !lineTwo.Found || !lineTwo.Exact || lineTwo.Offset != 4 || lineTwo.ResolvedLine != 2 || lineTwo.Limited {
		t.Fatalf("line two resolution = %+v, want exact offset four", lineTwo)
	}
}

func TestResolveLineReturnsBoundedFallbackForHugeLogicalLine(t *testing.T) {
	path := filepath.Join(t.TempDir(), "huge-logical-line.txt")
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR|os.O_TRUNC, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	newlineOffset := resolveLineExactScanBytes + 1
	if err := file.Truncate(newlineOffset + 2); err != nil {
		file.Close()
		t.Fatal(err)
	}
	if _, err := file.WriteAt([]byte{'\n', 'z'}, newlineOffset); err != nil {
		file.Close()
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}

	service := NewFileService()
	meta, err := service.OpenFile(path)
	if err != nil {
		t.Fatal(err)
	}
	defer service.CloseFile(meta.FileID)
	opened, ok := service.reg.Get(meta.FileID)
	if !ok {
		t.Fatal("opened session missing")
	}
	if err := opened.Doc.StartIndexing(context.Background()); err != nil {
		t.Fatal(err)
	}

	resolved, err := service.ResolveLine(meta.FileID, 2)
	if err != nil {
		t.Fatal(err)
	}
	if !resolved.Found || resolved.Exact || !resolved.IndexComplete || !resolved.Limited {
		t.Fatalf("resolution = %+v, want explicit bounded fallback", resolved)
	}
	if resolved.Offset != 0 || resolved.ResolvedLine != 1 {
		t.Fatalf("fallback = %+v, want actual line 1 at offset zero", resolved)
	}
}

func TestResolveLinePropagatesExactSourceReadFailure(t *testing.T) {
	path := filepath.Join(t.TempDir(), "lines.txt")
	if err := os.WriteFile(path, []byte("one\ntwo\nthree\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	service := NewFileService()
	meta, err := service.OpenFile(path)
	if err != nil {
		t.Fatal(err)
	}
	defer service.CloseFile(meta.FileID)
	opened, ok := service.reg.Get(meta.FileID)
	if !ok {
		t.Fatal("opened session missing")
	}
	if err := opened.Doc.StartIndexing(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := os.Truncate(path, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := service.ResolveLine(meta.FileID, 2); !errors.Is(err, document.ErrSourceChanged) {
		t.Fatalf("error = %v, want document.ErrSourceChanged", err)
	}
}

func TestResolveLineRejectsSameSizeRewriteWithRestoredModTime(t *testing.T) {
	path := filepath.Join(t.TempDir(), "lines.txt")
	original := []byte("one\ntwo\nthree\n")
	rewritten := []byte("ONE\ntwo\nthree\n")
	if err := os.WriteFile(path, original, 0o600); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	service := NewFileService()
	meta, err := service.OpenFile(path)
	if err != nil {
		t.Fatal(err)
	}
	defer service.CloseFile(meta.FileID)
	opened, _ := service.reg.Get(meta.FileID)
	if !opened.Doc.HasMutationGeneration() {
		t.Skip("filesystem does not expose a strong source mutation generation")
	}
	if err := os.WriteFile(path, rewritten, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, info.ModTime(), info.ModTime()); err != nil {
		t.Fatal(err)
	}

	resolution, err := service.ResolveLine(meta.FileID, 2)
	if !errors.Is(err, document.ErrSourceChanged) {
		t.Fatalf("error = %v, want document.ErrSourceChanged", err)
	}
	if resolution != (LineResolution{}) {
		t.Fatalf("stale resolution = %+v, want empty", resolution)
	}
}

func TestResolveLineRejectsInvalidInput(t *testing.T) {
	service := NewFileService()
	if _, err := service.ResolveLine("missing", 1); err == nil {
		t.Fatal("expected unknown file error")
	}
	path := filepath.Join(t.TempDir(), "line.txt")
	if err := os.WriteFile(path, []byte("one\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	meta, err := service.OpenFile(path)
	if err != nil {
		t.Fatal(err)
	}
	defer service.CloseFile(meta.FileID)
	if _, err := service.ResolveLine(meta.FileID, 0); err == nil {
		t.Fatal("expected non-positive line error")
	}
}

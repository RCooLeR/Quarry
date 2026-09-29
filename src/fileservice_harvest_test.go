package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/quarry/quarry-wails3/internal/document"
	"github.com/quarry/quarry-wails3/internal/fileio"
	"github.com/quarry/quarry-wails3/internal/regexutil"
	"github.com/quarry/quarry-wails3/internal/sourceio"
)

func TestHarvestMatchesToPathPublishesCompleteOutput(t *testing.T) {
	path := writeTempFile(t, "source.txt", []byte("one id=12\ntwo id=34\n"))
	svc := NewFileService()
	meta, err := svc.OpenFile(path)
	if err != nil {
		t.Fatal(err)
	}
	defer svc.CloseFile(meta.FileID)
	f, _ := svc.reg.Get(meta.FileID)
	re, err := regexutil.Compile([]byte(`id=\d{2}`), false)
	if err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(t.TempDir(), "matches.txt")

	result, err := svc.harvestMatchesToPath(context.Background(), f, re, false, dst, nil)
	if err != nil {
		t.Fatal(err)
	}
	if result.RecordsWritten != 2 || result.OutputPath != dst {
		t.Fatalf("result = %+v", result)
	}
	if got, err := os.ReadFile(dst); err != nil || string(got) != "id=12\nid=34\n" {
		t.Fatalf("output = %q, err %v", got, err)
	}
}

func TestFlattenHarvestMatchNewlinesCollapsesEveryLineEnding(t *testing.T) {
	input := []byte("alpha\r\nbeta\rgamma\ndelta")
	got := flattenHarvestMatchNewlines(input)
	if string(got) != "alpha beta gamma delta" {
		t.Fatalf("flattened match = %q", got)
	}
}

func TestHarvestRejectsNonExactRegexBeforeDialogOrOutputCreation(t *testing.T) {
	path := writeTempFile(t, "source.txt", []byte("id=12\nid=34\n"))
	svc := NewFileService()
	meta, err := svc.OpenFile(path)
	if err != nil {
		t.Fatal(err)
	}
	defer svc.CloseFile(meta.FileID)

	if _, err := svc.HarvestMatchesViaDialog(meta.FileID, `id=\d+`, false); !errors.Is(err, regexutil.ErrUnboundedRegex) {
		t.Fatalf("dialog preflight error = %v, want ErrUnboundedRegex", err)
	}

	f, _ := svc.reg.Get(meta.FileID)
	re, err := regexutil.Compile([]byte(`id=\d+`), false)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	dst := filepath.Join(dir, "matches.txt")
	if _, err := svc.harvestMatchesToPath(context.Background(), f, re, false, dst, nil); !errors.Is(err, regexutil.ErrUnboundedRegex) {
		t.Fatalf("direct preflight error = %v, want ErrUnboundedRegex", err)
	}
	assertNoPublishedOrTempOutput(t, dst)
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("regex preflight created artifacts: %v", entries)
	}
}

func TestHarvestMatchesToPathRejectsChangedSourceGeneration(t *testing.T) {
	path := writeTempFile(t, "source.txt", []byte("id=12\nid=34\n"))
	svc := NewFileService()
	meta, err := svc.OpenFile(path)
	if err != nil {
		t.Fatal(err)
	}
	defer svc.CloseFile(meta.FileID)
	f, _ := svc.reg.Get(meta.FileID)
	if err := os.Truncate(path, 0); err != nil {
		t.Fatal(err)
	}
	re, _ := regexutil.Compile([]byte(`id=\d{2}`), false)
	dst := filepath.Join(t.TempDir(), "matches.txt")

	if _, err := svc.harvestMatchesToPath(context.Background(), f, re, false, dst, nil); !errors.Is(err, document.ErrSourceChanged) {
		t.Fatalf("error = %v, want ErrSourceChanged", err)
	}
	assertNoPublishedOrTempOutput(t, dst)
}

func TestHarvestDialogRejectsRefreshedSessionGeneration(t *testing.T) {
	path := writeTempFile(t, "source.txt", []byte("id=12\n"))
	svc := NewFileService()
	meta, err := svc.OpenFile(path)
	if err != nil {
		t.Fatal(err)
	}
	defer svc.CloseFile(meta.FileID)
	dst := filepath.Join(t.TempDir(), "matches.txt")
	previous := harvestSaveDialog
	harvestSaveDialog = func(string, string) (string, error) {
		if err := os.WriteFile(path, []byte("id=34\n"), 0o600); err != nil {
			return "", err
		}
		if _, err := svc.RefreshFile(meta.FileID); err != nil {
			return "", err
		}
		return dst, nil
	}
	t.Cleanup(func() { harvestSaveDialog = previous })

	if _, err := svc.HarvestMatchesViaDialog(meta.FileID, `id=\d{2}`, false); !errors.Is(err, sourceio.ErrSourceChanged) {
		t.Fatalf("harvest error = %v, want sourceio.ErrSourceChanged", err)
	}
	assertNoPublishedOrTempOutput(t, dst)
}

func TestHarvestMatchesToPathRejectsSourceAliases(t *testing.T) {
	sourceBytes := []byte("one id=12\ntwo id=34\n")
	path := writeTempFile(t, "source.txt", sourceBytes)
	svc := NewFileService()
	meta, err := svc.OpenFile(path)
	if err != nil {
		t.Fatal(err)
	}
	defer svc.CloseFile(meta.FileID)
	f, _ := svc.reg.Get(meta.FileID)
	re, _ := regexutil.Compile([]byte(`id=\d{2}`), false)

	tests := []struct {
		name string
		dst  func(t *testing.T) string
	}{
		{name: "same path", dst: func(*testing.T) string { return path }},
		{name: "hard link", dst: func(t *testing.T) string {
			dst := filepath.Join(filepath.Dir(path), "source-hardlink.txt")
			if err := os.Link(path, dst); err != nil {
				t.Skipf("hard links unavailable: %v", err)
			}
			return dst
		}},
		{name: "symlink", dst: func(t *testing.T) string {
			dst := filepath.Join(filepath.Dir(path), "source-symlink.txt")
			if err := os.Symlink(path, dst); err != nil {
				t.Skipf("symlinks unavailable: %v", err)
			}
			return dst
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := svc.harvestMatchesToPath(context.Background(), f, re, false, tt.dst(t), nil)
			if !errors.Is(err, fileio.ErrSourceAlias) {
				t.Fatalf("error = %v, want ErrSourceAlias", err)
			}
			got, readErr := os.ReadFile(path)
			if readErr != nil || string(got) != string(sourceBytes) {
				t.Fatalf("source = %q, err %v", got, readErr)
			}
		})
	}
}

func TestHarvestMatchesToPathPreservesExistingDestinationAndCleansCancellation(t *testing.T) {
	path := writeTempFile(t, "source.txt", []byte("id=12\nid=34\n"))
	svc := NewFileService()
	meta, err := svc.OpenFile(path)
	if err != nil {
		t.Fatal(err)
	}
	defer svc.CloseFile(meta.FileID)
	f, _ := svc.reg.Get(meta.FileID)
	re, _ := regexutil.Compile([]byte(`id=\d{2}`), false)
	dir := t.TempDir()
	dst := filepath.Join(dir, "matches.txt")
	if err := os.WriteFile(dst, []byte("existing"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.harvestMatchesToPath(context.Background(), f, re, false, dst, nil); !errors.Is(err, fileio.ErrExists) {
		t.Fatalf("existing destination error = %v, want ErrExists", err)
	}
	if got, err := os.ReadFile(dst); err != nil || string(got) != "existing" {
		t.Fatalf("existing destination = %q, err %v", got, err)
	}

	if err := os.Remove(dst); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := svc.harvestMatchesToPath(ctx, f, re, false, dst, nil); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancel error = %v, want context.Canceled", err)
	}
	if _, err := os.Stat(dst); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("cancelled harvest published output: %v", err)
	}
	temps, err := filepath.Glob(filepath.Join(dir, ".matches.txt.quarry-*"))
	if err != nil {
		t.Fatal(err)
	}
	if len(temps) != 0 {
		t.Fatalf("cancelled harvest left temps: %v", temps)
	}
}

func TestHarvestMatchesToPathRejectsEveryOpenSource(t *testing.T) {
	activePath := writeTempFile(t, "active.txt", []byte("id=12\n"))
	otherPath := writeTempFile(t, "other.txt", []byte("other source"))
	svc := NewFileService()
	active, err := svc.OpenFile(activePath)
	if err != nil {
		t.Fatal(err)
	}
	defer svc.CloseFile(active.FileID)
	other, err := svc.OpenFile(otherPath)
	if err != nil {
		t.Fatal(err)
	}
	defer svc.CloseFile(other.FileID)
	f, _ := svc.reg.Get(active.FileID)
	re, _ := regexutil.Compile([]byte(`id=\d{2}`), false)

	if _, err := svc.harvestMatchesToPath(context.Background(), f, re, false, otherPath, nil); !errors.Is(err, fileio.ErrSourceAlias) {
		t.Fatalf("error = %v, want ErrSourceAlias for another open source", err)
	}
	if got, err := os.ReadFile(otherPath); err != nil || string(got) != "other source" {
		t.Fatalf("other source = %q, err %v", got, err)
	}
}

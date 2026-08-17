package main

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/quarry/quarry-wails3/internal/document"
)

func TestSearchRejectsSameSizeRewriteWithRestoredModTime(t *testing.T) {
	path := filepath.Join(t.TempDir(), "search.txt")
	original := []byte("alpha target omega\n")
	rewritten := []byte("ALPHA target omega\n")
	if len(original) != len(rewritten) {
		t.Fatal("fixture must preserve size")
	}
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

	hit, err := service.findNext(meta.FileID, "target", 0, false, true, false)
	if !errors.Is(err, document.ErrSourceChanged) {
		t.Fatalf("find error = %v, want document.ErrSourceChanged", err)
	}
	if hit != (SearchHit{}) {
		t.Fatalf("stale find result = %+v, want empty", hit)
	}
	page, err := service.searchAllPage(meta.FileID, "target", false, true, false, 10, 0, false)
	if !errors.Is(err, document.ErrSourceChanged) {
		t.Fatalf("search-all error = %v, want document.ErrSourceChanged", err)
	}
	if len(page.Hits) != 0 || page.Continuation != nil {
		t.Fatalf("stale search-all result = %+v, want no hits/continuation", page)
	}
}

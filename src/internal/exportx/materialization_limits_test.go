package exportx

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/quarry/quarry-wails3/internal/document"
)

func TestClipboardLimitIsAbsoluteAtCoreBoundary(t *testing.T) {
	dir := t.TempDir()
	sourcePath := filepath.Join(dir, "source.txt")
	if err := os.WriteFile(sourcePath, []byte("abc"), 0o600); err != nil {
		t.Fatal(err)
	}
	doc, err := document.OpenFile(sourcePath)
	if err != nil {
		t.Fatal(err)
	}
	defer doc.Close()

	if _, text, err := CopyByteRange(context.Background(), doc, 0, doc.Size(), MaxClipboardBytes); err != nil || text != "abc" {
		t.Fatalf("absolute maximum rejected: text=%q err=%v", text, err)
	}
	for _, requested := range []int64{-1, MaxClipboardBytes + 1, int64(^uint64(0) >> 1)} {
		_, _, err := CopyByteRange(context.Background(), doc, 0, 1, requested)
		if !errors.Is(err, ErrMaterializedLimit) {
			t.Fatalf("maximum=%d error = %v, want ErrMaterializedLimit", requested, err)
		}
	}
	if _, _, err := CopyLineRange(context.Background(), doc, 1, int64(^uint64(0)>>1), MaxClipboardBytes+1); !errors.Is(err, ErrMaterializedLimit) {
		t.Fatalf("line-range preflight error = %v, want ErrMaterializedLimit", err)
	}
}

func TestVisibleSelectionRejectsOversizedInputBeforeOutputCreation(t *testing.T) {
	outputPath := filepath.Join(t.TempDir(), "selection.txt")
	text := strings.Repeat("a", MaxVisibleSelectionUTF8Bytes+1)
	_, err := ExportVisibleText(context.Background(), outputPath, text, "UTF-8")
	if !errors.Is(err, ErrMaterializedLimit) {
		t.Fatalf("error = %v, want ErrMaterializedLimit", err)
	}
	if _, statErr := os.Stat(outputPath); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("oversized selection created output: %v", statErr)
	}
}

func TestVisibleSelectionSizeMaximumIncludesUTF16BOM(t *testing.T) {
	text := strings.Repeat("a", MaxVisibleSelectionUTF8Bytes)
	got, err := visibleSelectionOutputSize(text, textEncodingUTF16LE, 2)
	if err != nil {
		t.Fatalf("maximum selection rejected: %v", err)
	}
	if got != MaxVisibleSelectionOutputBytes {
		t.Fatalf("output size = %d, want %d", got, MaxVisibleSelectionOutputBytes)
	}
	if _, err := visibleSelectionOutputSize(text+"a", textEncodingUTF16LE, 2); !errors.Is(err, ErrMaterializedLimit) {
		t.Fatalf("maximum-plus-one error = %v, want ErrMaterializedLimit", err)
	}
}

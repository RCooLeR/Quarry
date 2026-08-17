package exportx

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/quarry/quarry-wails3/internal/document"
)

func TestExportByteRangesPublishesOnlySelectedNoncontiguousBytes(t *testing.T) {
	dir := t.TempDir()
	source := filepath.Join(dir, "source.sql")
	output := filepath.Join(dir, "table.sql")
	if err := os.WriteFile(source, []byte("AAA---BBB---CCC"), 0o600); err != nil {
		t.Fatal(err)
	}
	doc, err := document.OpenFile(source)
	if err != nil {
		t.Fatal(err)
	}
	defer doc.Close()

	summary, err := ExportByteRanges(context.Background(), doc, source, output, [][2]int64{{0, 3}, {6, 9}, {12, 15}}, Options{ComputeSHA256: true})
	if err != nil {
		t.Fatal(err)
	}
	if summary.BytesWritten != 9 || summary.SHA256 == "" {
		t.Fatalf("summary = %+v", summary)
	}
	got, err := os.ReadFile(output)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "AAABBBCCC" {
		t.Fatalf("output = %q", got)
	}
}

func TestExportByteRangesRejectsInvalidPlanWithoutOutput(t *testing.T) {
	dir := t.TempDir()
	source := filepath.Join(dir, "source.sql")
	output := filepath.Join(dir, "table.sql")
	if err := os.WriteFile(source, []byte("0123456789"), 0o600); err != nil {
		t.Fatal(err)
	}
	doc, err := document.OpenFile(source)
	if err != nil {
		t.Fatal(err)
	}
	defer doc.Close()
	for _, ranges := range [][][2]int64{
		nil,
		{{2, 2}},
		{{2, 6}, {5, 8}},
		{{0, 11}},
	} {
		if _, err := ExportByteRanges(context.Background(), doc, source, output, ranges, Options{}); err == nil {
			t.Fatalf("invalid ranges %v were accepted", ranges)
		}
		if _, statErr := os.Stat(output); !errors.Is(statErr, os.ErrNotExist) {
			t.Fatalf("invalid plan created output: %v", statErr)
		}
	}
}

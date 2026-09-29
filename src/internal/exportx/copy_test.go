package exportx

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/quarry/quarry-wails3/internal/document"
	"github.com/quarry/quarry-wails3/internal/encodingx"
)

func TestCopyByteRangePreservesEmbeddedByteOrderMark(t *testing.T) {
	for _, encoding := range []string{"UTF-8", "UTF-16LE", "UTF-16BE"} {
		t.Run(encoding, func(t *testing.T) {
			prefix, err := encodingx.EncodeString(encoding, "prefix")
			if err != nil {
				t.Fatal(err)
			}
			selected, err := encodingx.EncodeString(encoding, "\ufefftext")
			if err != nil {
				t.Fatal(err)
			}
			data := append(append(encodingx.BOMBytes(encoding), prefix...), selected...)
			path := filepath.Join(t.TempDir(), "embedded-bom.txt")
			if err := os.WriteFile(path, data, 0o600); err != nil {
				t.Fatal(err)
			}
			doc, err := document.OpenFile(path)
			if err != nil {
				t.Fatal(err)
			}
			defer doc.Close()
			start := int64(len(data) - len(selected))
			_, text, err := CopyByteRange(context.Background(), doc, start, int64(len(data)), 1024)
			if err != nil {
				t.Fatal(err)
			}
			if text != "\ufefftext" {
				t.Fatalf("copied text=%q, want embedded U+FEFF retained", text)
			}
		})
	}
}

func TestCopyByteRangeReadsSelectedText(t *testing.T) {
	dir := t.TempDir()
	srcPath := filepath.Join(dir, "source.txt")
	if err := os.WriteFile(srcPath, []byte("alpha\nbravo\ncharlie\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	doc, err := document.OpenFile(srcPath)
	if err != nil {
		t.Fatal(err)
	}
	defer doc.Close()

	summary, text, err := CopyByteRange(context.Background(), doc, 6, 12, DefaultClipboardMaxBytes)
	if err != nil {
		t.Fatal(err)
	}
	if summary.Mode != "byte-range" {
		t.Fatalf("mode = %q, want byte-range", summary.Mode)
	}
	if summary.BytesRead != 6 {
		t.Fatalf("bytes read = %d, want 6", summary.BytesRead)
	}
	if text != "bravo\n" {
		t.Fatalf("text = %q, want bravo\\n", text)
	}
}

func TestCopyLineRangeDecodesWindows1252(t *testing.T) {
	dir := t.TempDir()
	srcPath := filepath.Join(dir, "source.txt")
	encoded := []byte("alpha\r\ncaf\xe9\r\nna\xefve\r\n")
	if err := os.WriteFile(srcPath, encoded, 0o600); err != nil {
		t.Fatal(err)
	}

	doc, err := document.OpenFile(srcPath)
	if err != nil {
		t.Fatal(err)
	}
	defer doc.Close()

	summary, text, err := CopyLineRange(context.Background(), doc, 2, 3, DefaultClipboardMaxBytes)
	if err != nil {
		t.Fatal(err)
	}
	if !summary.UsedLineRange {
		t.Fatal("expected line-range metadata")
	}
	if text != "caf\u00e9\r\nna\u00efve\r\n" {
		t.Fatalf("text = %q, want decoded line range", text)
	}
}

func TestCopyByteRangeRejectsOversizedClipboardReads(t *testing.T) {
	dir := t.TempDir()
	srcPath := filepath.Join(dir, "source.txt")
	if err := os.WriteFile(srcPath, []byte("0123456789"), 0o600); err != nil {
		t.Fatal(err)
	}

	doc, err := document.OpenFile(srcPath)
	if err != nil {
		t.Fatal(err)
	}
	defer doc.Close()

	_, _, err = CopyByteRange(context.Background(), doc, 0, doc.Size(), 4)
	var tooLarge *RangeTooLargeError
	if !errors.As(err, &tooLarge) {
		t.Fatalf("err = %v, want RangeTooLargeError", err)
	}
	if tooLarge.RequestedBytes != 10 || tooLarge.MaxBytes != 4 {
		t.Fatalf("tooLarge = %+v, want requested=10 max=4", tooLarge)
	}
}

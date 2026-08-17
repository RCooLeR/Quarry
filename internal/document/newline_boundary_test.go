package document

import (
	"bytes"
	"context"
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"
	"unicode/utf16"

	"github.com/quarry/quarry-wails3/internal/lineindex"
)

func TestExactNavigationTreatsCRLFAcrossScanBoundaryAsOneBreak(t *testing.T) {
	data := bytes.Repeat([]byte{'a'}, exactScanChunkSize+8)
	data[exactScanChunkSize-1] = '\r'
	data[exactScanChunkSize] = '\n'
	path := filepath.Join(t.TempDir(), "crlf-seam.txt")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	doc, err := OpenFile(path)
	if err != nil {
		t.Fatal(err)
	}
	defer doc.Close()
	buildDocumentIndexForTest(t, doc)

	between := int64(exactScanChunkSize)
	if line, ok, err := doc.ExactOffsetToLine(between); err != nil || !ok || line != 1 {
		t.Fatalf("line between CR/LF = %d, %v, %v; want line 1", line, ok, err)
	}
	after := int64(exactScanChunkSize + 1)
	if line, ok, err := doc.ExactOffsetToLine(after); err != nil || !ok || line != 2 {
		t.Fatalf("line after CRLF = %d, %v, %v; want line 2", line, ok, err)
	}
	if offset, ok, err := doc.ExactLineToOffset(2); err != nil || !ok || offset != after {
		t.Fatalf("line 2 offset = %d, %v, %v; want %d", offset, ok, err, after)
	}
	if offset, ok, err := doc.LineStartOffset(2); err != nil || !ok || offset != after {
		t.Fatalf("line start 2 = %d, %v, %v; want %d", offset, ok, err, after)
	}
}

func TestExactOffsetResolvesStandaloneCRWithBoundedLookahead(t *testing.T) {
	data := bytes.Repeat([]byte{'a'}, exactScanChunkSize+8)
	data[exactScanChunkSize-1] = '\r'
	data[exactScanChunkSize] = 'x'
	path := filepath.Join(t.TempDir(), "cr-seam.txt")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	doc, err := OpenFile(path)
	if err != nil {
		t.Fatal(err)
	}
	defer doc.Close()
	buildDocumentIndexForTest(t, doc)

	offset := int64(exactScanChunkSize)
	if line, ok, err := doc.ExactOffsetToLine(offset); err != nil || !ok || line != 2 {
		t.Fatalf("line after standalone CR = %d, %v, %v; want line 2", line, ok, err)
	}
	if start, ok, err := doc.ExactLineToOffset(2); err != nil || !ok || start != offset {
		t.Fatalf("line 2 offset = %d, %v, %v; want %d", start, ok, err, offset)
	}
}

func TestUTF16NavigationIgnoresUnalignedPatternsAndCombinesCRLF(t *testing.T) {
	for _, encoding := range []string{"UTF-16LE", "UTF-16BE"} {
		t.Run(encoding, func(t *testing.T) {
			var phantomRunes []rune
			if encoding == "UTF-16LE" {
				phantomRunes = []rune{0x0A41, 0x4200, 0x0D43, 0x4400}
			} else {
				phantomRunes = []rune{0x4100, 0x0A42, 0x4300, 0x0D44}
			}
			prefixUnits := exactScanChunkSize/2 - 2
			runes := make([]rune, 0, prefixUnits+8)
			runes = append(runes, phantomRunes...)
			for len(runes) < prefixUnits {
				runes = append(runes, 'a')
			}
			runes = append(runes, '\r', '\n', 'z')
			data := encodeDocumentUTF16(encoding, runes, true)
			path := filepath.Join(t.TempDir(), "utf16-seam.txt")
			if err := os.WriteFile(path, data, 0o600); err != nil {
				t.Fatal(err)
			}
			doc, err := OpenFile(path)
			if err != nil {
				t.Fatal(err)
			}
			defer doc.Close()
			if doc.Metadata().Encoding != encoding {
				t.Fatalf("encoding = %q, want %q", doc.Metadata().Encoding, encoding)
			}
			buildDocumentIndexForTest(t, doc)

			// CR ends exactly at the 1 MiB scanner seam; LF begins the next chunk.
			lineTwo := int64(exactScanChunkSize + 2)
			if progress := doc.IndexProgress(); progress.Lines != 1 {
				t.Fatalf("indexed breaks = %d, want one real CRLF", progress.Lines)
			}
			offset, ok, err := doc.ExactLineToOffset(2)
			if err != nil || !ok || offset != lineTwo {
				t.Fatalf("line 2 offset = %d, %v, %v; want %d", offset, ok, err, lineTwo)
			}
			if offset&1 != 0 {
				t.Fatalf("UTF-16 line offset is unaligned: %d", offset)
			}
		})
	}
}

func TestVisiblePageResolvesNewlineAtBoundedReadSeam(t *testing.T) {
	for _, tt := range []struct {
		name       string
		content    string
		wantNext   int64
		secondText string
	}{
		{name: "CRLF", content: "a\r\nb", wantNext: 3, secondText: "b"},
		{name: "standalone CR", content: "a\rb", wantNext: 2, secondText: "b"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "visible.txt")
			if err := os.WriteFile(path, []byte(tt.content), 0o600); err != nil {
				t.Fatal(err)
			}
			doc, err := OpenFile(path)
			if err != nil {
				t.Fatal(err)
			}
			defer doc.Close()

			page, err := doc.VisiblePageFromOffset(0, 2, VisibleLineOptions{MaxBytes: 2, MaxLineBytes: 2})
			if err != nil {
				t.Fatal(err)
			}
			if len(page.Lines) != 1 || page.Lines[0].Text != "a" || page.NextOffset != tt.wantNext {
				t.Fatalf("first page = %#v, want text a and next %d", page, tt.wantNext)
			}
			page, err = doc.VisiblePageFromOffset(page.NextOffset, 1, VisibleLineOptions{MaxBytes: 2, MaxLineBytes: 2})
			if err != nil {
				t.Fatal(err)
			}
			if len(page.Lines) != 1 || page.Lines[0].Text != tt.secondText {
				t.Fatalf("second page = %#v, want %q", page, tt.secondText)
			}
		})
	}
}

func TestBoundedLineStartDistinguishesCRLFInteriorFromStandaloneCR(t *testing.T) {
	for _, tt := range []struct {
		name    string
		content string
		offset  int64
		want    int64
	}{
		{name: "between CR and LF", content: "aaa\r\nbbb", offset: 4, want: 0},
		{name: "after CRLF", content: "aaa\r\nbbb", offset: 5, want: 5},
		{name: "after standalone CR", content: "aaa\rbbb", offset: 4, want: 4},
	} {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "line-start.txt")
			if err := os.WriteFile(path, []byte(tt.content), 0o600); err != nil {
				t.Fatal(err)
			}
			doc, err := OpenFile(path)
			if err != nil {
				t.Fatal(err)
			}
			defer doc.Close()
			got, exceeded, err := doc.findLineStartWithinLimit(tt.offset, 16)
			if err != nil || exceeded || got != tt.want {
				t.Fatalf("line start = %d, exceeded=%v, err=%v; want %d", got, exceeded, err, tt.want)
			}
		})
	}
}

func buildDocumentIndexForTest(t *testing.T, doc *FileDocument) {
	t.Helper()
	file, err := os.Open(doc.Path())
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	idx := lineindex.New(4096)
	if err := idx.BuildWithEncoding(context.Background(), file, doc.Metadata().Encoding); err != nil {
		t.Fatal(err)
	}
	doc.idx = idx
}

func encodeDocumentUTF16(encoding string, runes []rune, bom bool) []byte {
	units := utf16.Encode(runes)
	start := 0
	if bom {
		start = 2
	}
	out := make([]byte, start+len(units)*2)
	if bom {
		if encoding == "UTF-16BE" {
			out[0], out[1] = 0xfe, 0xff
		} else {
			out[0], out[1] = 0xff, 0xfe
		}
	}
	for i, unit := range units {
		if encoding == "UTF-16BE" {
			binary.BigEndian.PutUint16(out[start+i*2:], unit)
		} else {
			binary.LittleEndian.PutUint16(out[start+i*2:], unit)
		}
	}
	return out
}

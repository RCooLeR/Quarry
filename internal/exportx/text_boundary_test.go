package exportx

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"unicode/utf8"

	"github.com/quarry/quarry-wails3/internal/document"
	"github.com/quarry/quarry-wails3/internal/encodingx"
)

func TestTextRangeRejectsEveryUTF8RuneInteriorEndpointAndRawRemainsExact(t *testing.T) {
	dir := t.TempDir()
	data := []byte{'A', 0xC2, 0xA2, 0xE2, 0x82, 0xAC, 0xF0, 0x9F, 0x98, 0x80, 'Z'}
	sourcePath, doc := openTextBoundaryFixture(t, dir, "utf8.txt", data)

	for runeStart := 0; runeStart < len(data); {
		_, width := utf8.DecodeRune(data[runeStart:])
		if width <= 0 {
			t.Fatalf("invalid fixture at %d", runeStart)
		}
		for inside := 1; inside < width; inside++ {
			offset := runeStart + inside
			for _, endpoint := range []string{"start", "end"} {
				endpoint := endpoint
				outPath := filepath.Join(dir, fmt.Sprintf("utf8-%s-%d.txt", endpoint, offset))
				start, end := int64(offset), int64(len(data))
				if endpoint == "end" {
					start, end = 0, int64(offset)
				}
				_, err := ExportByteRangeText(context.Background(), doc, sourcePath, outPath, start, end, "UTF-8", "UTF-8", Options{})
				if !errors.Is(err, ErrUnalignedTextRange) {
					t.Fatalf("%s offset %d: error = %v, want ErrUnalignedTextRange", endpoint, offset, err)
				}
				assertTextOutputAbsent(t, outPath)
			}
		}
		runeStart += width
	}

	for runeStart := 0; runeStart < len(data); {
		_, width := utf8.DecodeRune(data[runeStart:])
		outPath := filepath.Join(dir, fmt.Sprintf("utf8-valid-%d.txt", runeStart))
		_, err := ExportByteRangeText(context.Background(), doc, sourcePath, outPath, int64(runeStart), int64(runeStart+width), "UTF-8", "UTF-8", Options{})
		if err != nil {
			t.Fatalf("aligned rune [%d,%d): %v", runeStart, runeStart+width, err)
		}
		got, err := os.ReadFile(outPath)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(got, data[runeStart:runeStart+width]) || !utf8.Valid(got) {
			t.Fatalf("aligned rune [%d,%d) = % x, want valid % x", runeStart, runeStart+width, got, data[runeStart:runeStart+width])
		}
		runeStart += width
	}

	// Raw export deliberately retains byte-coordinate semantics, including a
	// fragment made only of UTF-8 continuation bytes.
	rawStart, rawEnd := int64(7), int64(9)
	rawPath := filepath.Join(dir, "raw-fragment.bin")
	if _, err := ExportByteRange(context.Background(), doc, sourcePath, rawPath, rawStart, rawEnd, Options{}); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(rawPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(raw, data[rawStart:rawEnd]) || utf8.Valid(raw) {
		t.Fatalf("raw fragment = % x, want exact invalid UTF-8 bytes % x", raw, data[rawStart:rawEnd])
	}
}

func TestVisibleRangeTextUsesTheSameUTF8BoundaryPolicy(t *testing.T) {
	dir := t.TempDir()
	data := []byte{'x', 0xF0, 0x9F, 0x98, 0x80, 'y'}
	sourcePath, doc := openTextBoundaryFixture(t, dir, "visible.txt", data)
	outPath := filepath.Join(dir, "visible-out.txt")
	manifestPath := filepath.Join(dir, "visible-out.manifest.json")

	_, err := ExportVisibleRangeText(context.Background(), doc, sourcePath, outPath, 2, int64(len(data)), "UTF-8", "UTF-8", Options{
		WriteManifest: true,
		ManifestPath:  manifestPath,
	})
	if !errors.Is(err, ErrUnalignedTextRange) {
		t.Fatalf("error = %v, want ErrUnalignedTextRange", err)
	}
	assertTextOutputAbsent(t, outPath)
	assertTextOutputAbsent(t, manifestPath)
}

func TestTextRangeUTF8BOMEdgesAreRejectedOrStrippedDeterministically(t *testing.T) {
	dir := t.TempDir()
	data := append([]byte{0xEF, 0xBB, 0xBF}, 'A')
	sourcePath, doc := openTextBoundaryFixture(t, dir, "utf8-bom.txt", data)

	for _, offset := range []int64{1, 2} {
		for _, endpoint := range []string{"start", "end"} {
			outPath := filepath.Join(dir, fmt.Sprintf("bom-%s-%d.txt", endpoint, offset))
			start, end := offset, int64(len(data))
			if endpoint == "end" {
				start, end = 0, offset
			}
			_, err := ExportByteRangeText(context.Background(), doc, sourcePath, outPath, start, end, "UTF-8", "UTF-8", Options{})
			if !errors.Is(err, ErrUnalignedTextRange) {
				t.Fatalf("%s BOM offset %d: error = %v, want ErrUnalignedTextRange", endpoint, offset, err)
			}
			assertTextOutputAbsent(t, outPath)
		}
	}

	fullPath := filepath.Join(dir, "bom-full.txt")
	summary, err := ExportByteRangeText(context.Background(), doc, sourcePath, fullPath, 0, int64(len(data)), "UTF-8", "UTF-8", Options{})
	if err != nil {
		t.Fatal(err)
	}
	if summary.StartOffset != 3 || summary.EndOffset != int64(len(data)) {
		t.Fatalf("summary offsets = [%d,%d), want [3,%d)", summary.StartOffset, summary.EndOffset, len(data))
	}
	got, err := os.ReadFile(fullPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "A" {
		t.Fatalf("full output = % x, want A without source BOM", got)
	}

	contentPath := filepath.Join(dir, "bom-content.txt")
	if _, err := ExportByteRangeText(context.Background(), doc, sourcePath, contentPath, 3, 4, "UTF-8", "UTF-8", Options{}); err != nil {
		t.Fatal(err)
	}
	got, err = os.ReadFile(contentPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "A" {
		t.Fatalf("content output = %q", got)
	}

	bomOnlyPath := filepath.Join(dir, "bom-only-range.txt")
	if _, err := ExportByteRangeText(context.Background(), doc, sourcePath, bomOnlyPath, 0, 3, "UTF-8", "UTF-8", Options{}); err != nil {
		t.Fatal(err)
	}
	got, err = os.ReadFile(bomOnlyPath)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("BOM-only range output = % x, want empty UTF-8 content", got)
	}
}

func TestTextRangeUTF16RejectsCodeUnitAndSurrogateSeams(t *testing.T) {
	const content = "A\U0001F600B\r\nC"
	for _, encodingName := range []string{"UTF-16LE", "UTF-16BE"} {
		encodingName := encodingName
		t.Run(encodingName, func(t *testing.T) {
			dir := t.TempDir()
			encoded, err := encodingx.EncodeString(encodingName, content)
			if err != nil {
				t.Fatal(err)
			}
			data := append(append([]byte{}, encodingx.BOMBytes(encodingName)...), encoded...)
			sourcePath, doc := openTextBoundaryFixture(t, dir, "utf16.txt", data)

			for offset := int64(1); offset < int64(len(data)); offset += 2 {
				for _, endpoint := range []string{"start", "end"} {
					outPath := filepath.Join(dir, fmt.Sprintf("unit-%s-%d.txt", endpoint, offset))
					start, end := offset, int64(len(data))
					if endpoint == "end" {
						start, end = 0, offset
					}
					_, err := ExportByteRangeText(context.Background(), doc, sourcePath, outPath, start, end, encodingName, "UTF-8", Options{})
					if !errors.Is(err, ErrUnalignedTextRange) {
						t.Fatalf("%s code-unit offset %d: error = %v, want ErrUnalignedTextRange", endpoint, offset, err)
					}
					assertTextOutputAbsent(t, outPath)
				}
			}

			// Byte 6 is aligned to a code unit but falls between the high and
			// low surrogate encoding the emoji.
			for _, endpoint := range []string{"start", "end"} {
				outPath := filepath.Join(dir, "surrogate-"+endpoint+".txt")
				start, end := int64(6), int64(len(data))
				if endpoint == "end" {
					start, end = 0, 6
				}
				_, err := ExportByteRangeText(context.Background(), doc, sourcePath, outPath, start, end, encodingName, "UTF-8", Options{})
				if !errors.Is(err, ErrUnalignedTextRange) {
					t.Fatalf("%s surrogate seam: error = %v, want ErrUnalignedTextRange", endpoint, err)
				}
				assertTextOutputAbsent(t, outPath)
			}

			aligned := []struct {
				name       string
				start, end int64
				want       string
			}{
				{name: "emoji", start: 4, end: 8, want: "\U0001F600"},
				{name: "cr", start: 10, end: 12, want: "\r"},
				{name: "lf", start: 12, end: 14, want: "\n"},
				{name: "crlf", start: 10, end: 14, want: "\r\n"},
			}
			for _, tc := range aligned {
				outPath := filepath.Join(dir, tc.name+".txt")
				if _, err := ExportByteRangeText(context.Background(), doc, sourcePath, outPath, tc.start, tc.end, encodingName, "UTF-8", Options{}); err != nil {
					t.Fatalf("%s aligned export: %v", tc.name, err)
				}
				got, err := os.ReadFile(outPath)
				if err != nil {
					t.Fatal(err)
				}
				if string(got) != tc.want || !utf8.Valid(got) {
					t.Fatalf("%s = % x (%q), want valid UTF-8 %q", tc.name, got, got, tc.want)
				}
			}

			fullPath := filepath.Join(dir, "full-same-encoding.txt")
			summary, err := ExportByteRangeText(context.Background(), doc, sourcePath, fullPath, 0, int64(len(data)), encodingName, encodingName, Options{})
			if err != nil {
				t.Fatal(err)
			}
			if summary.StartOffset != 2 {
				t.Fatalf("full summary start = %d, want 2 after source BOM", summary.StartOffset)
			}
			got, err := os.ReadFile(fullPath)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.HasPrefix(got, encodingx.BOMBytes(encodingName)) {
				t.Fatalf("full output lacks %s BOM: % x", encodingName, got)
			}
			decoded, err := encodingx.DecodeBytes(encodingName, got)
			if err != nil {
				t.Fatal(err)
			}
			if decoded != content {
				t.Fatalf("full decoded output = %q, want %q", decoded, content)
			}

			bomOnlyPath := filepath.Join(dir, "bom-only-range.txt")
			if _, err := ExportByteRangeText(context.Background(), doc, sourcePath, bomOnlyPath, 0, 2, encodingName, encodingName, Options{}); err != nil {
				t.Fatal(err)
			}
			got, err = os.ReadFile(bomOnlyPath)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(got, encodingx.BOMBytes(encodingName)) {
				t.Fatalf("BOM-only %s output = % x, want one target BOM", encodingName, got)
			}
		})
	}
}

func TestTextRangeRejectsMalformedUnicodeAndMismatchedBOMWithoutPublication(t *testing.T) {
	utf8Cases := map[string][]byte{
		"invalid-leader":       {'A', 0xFF, 'B'},
		"invalid-continuation": {'A', 0xC2, 'B'},
		"overlong":             {'A', 0xE0, 0x80, 0x80, 'B'},
		"surrogate":            {'A', 0xED, 0xA0, 0x80, 'B'},
		"out-of-range":         {'A', 0xF4, 0x90, 0x80, 0x80, 'B'},
		"truncated":            {'A', 0xE2, 0x82},
	}
	for name, data := range utf8Cases {
		name, data := name, data
		t.Run("UTF-8/"+name, func(t *testing.T) {
			dir := t.TempDir()
			sourcePath, doc := openTextBoundaryFixture(t, dir, "invalid.txt", data)
			outPath := filepath.Join(dir, "out.txt")
			_, err := ExportByteRangeText(context.Background(), doc, sourcePath, outPath, 0, int64(len(data)), "UTF-8", "UTF-8", Options{})
			if !errors.Is(err, ErrInvalidSourceText) {
				t.Fatalf("error = %v, want ErrInvalidSourceText", err)
			}
			assertTextOutputAbsent(t, outPath)
		})
	}

	for _, encodingName := range []string{"UTF-16LE", "UTF-16BE"} {
		encodingName := encodingName
		for _, tc := range []struct {
			name  string
			units []uint16
		}{
			{name: "unpaired-high", units: []uint16{0x0041, 0xD83D, 0x0042}},
			{name: "terminal-high", units: []uint16{0x0041, 0xD83D}},
			{name: "unpaired-low", units: []uint16{0x0041, 0xDE00, 0x0042}},
		} {
			tc := tc
			t.Run(encodingName+"/"+tc.name, func(t *testing.T) {
				dir := t.TempDir()
				data := append(append([]byte{}, encodingx.BOMBytes(encodingName)...), encodeUTF16Units(encodingName, tc.units...)...)
				sourcePath, doc := openTextBoundaryFixture(t, dir, "invalid.txt", data)
				outPath := filepath.Join(dir, "out.txt")
				_, err := ExportByteRangeText(context.Background(), doc, sourcePath, outPath, 0, int64(len(data)), encodingName, "UTF-8", Options{})
				if !errors.Is(err, ErrInvalidSourceText) {
					t.Fatalf("error = %v, want ErrInvalidSourceText", err)
				}
				assertTextOutputAbsent(t, outPath)
			})
		}
	}

	for _, tc := range []struct {
		declared string
		actual   string
	}{
		{declared: "UTF-16LE", actual: "UTF-16BE"},
		{declared: "UTF-16BE", actual: "UTF-16LE"},
	} {
		tc := tc
		t.Run("mismatched-BOM/"+tc.declared, func(t *testing.T) {
			dir := t.TempDir()
			encoded, err := encodingx.EncodeString(tc.actual, "A")
			if err != nil {
				t.Fatal(err)
			}
			data := append(append([]byte{}, encodingx.BOMBytes(tc.actual)...), encoded...)
			sourcePath, doc := openTextBoundaryFixture(t, dir, "mismatch.txt", data)
			outPath := filepath.Join(dir, "out.txt")
			_, err = ExportByteRangeText(context.Background(), doc, sourcePath, outPath, 0, int64(len(data)), tc.declared, "UTF-8", Options{})
			if !errors.Is(err, ErrInvalidSourceText) {
				t.Fatalf("error = %v, want ErrInvalidSourceText", err)
			}
			assertTextOutputAbsent(t, outPath)
		})
	}
}

func TestTextRangeWindowsCodePagesAreStrictAndByteAligned(t *testing.T) {
	validCases := []struct {
		name         string
		encodingName string
		data         []byte
		start, end   int64
		want         string
	}{
		{name: "1251", encodingName: "Windows-1251", data: []byte{'A', 0xDF, 'B'}, start: 1, end: 2, want: "\u042F"},
		{name: "1252", encodingName: "Windows-1252", data: []byte{'A', 0xE9, 'B'}, start: 1, end: 2, want: "\u00E9"},
	}
	for _, tc := range validCases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			sourcePath, doc := openTextBoundaryFixture(t, dir, "codepage.txt", tc.data)
			outPath := filepath.Join(dir, "out.txt")
			if _, err := ExportByteRangeText(context.Background(), doc, sourcePath, outPath, tc.start, tc.end, tc.encodingName, "UTF-8", Options{}); err != nil {
				t.Fatal(err)
			}
			got, err := os.ReadFile(outPath)
			if err != nil {
				t.Fatal(err)
			}
			if string(got) != tc.want || !utf8.Valid(got) {
				t.Fatalf("output = % x (%q), want valid UTF-8 %q", got, got, tc.want)
			}
		})
	}

	invalidCases := []struct {
		name         string
		encodingName string
		undefined    byte
	}{
		{name: "1251", encodingName: "Windows-1251", undefined: 0x98},
		{name: "1252", encodingName: "Windows-1252", undefined: 0x81},
	}
	for _, tc := range invalidCases {
		tc := tc
		t.Run(tc.name+"-undefined", func(t *testing.T) {
			dir := t.TempDir()
			data := []byte{'A', tc.undefined, 'B'}
			sourcePath, doc := openTextBoundaryFixture(t, dir, "undefined.txt", data)
			outPath := filepath.Join(dir, "out.txt")
			_, err := ExportByteRangeText(context.Background(), doc, sourcePath, outPath, 0, int64(len(data)), tc.encodingName, "UTF-8", Options{})
			if !errors.Is(err, ErrInvalidSourceText) {
				t.Fatalf("error = %v, want ErrInvalidSourceText", err)
			}
			assertTextOutputAbsent(t, outPath)
		})
	}
}

func TestVisibleSelectionRejectsInvalidGoStringBeforeOutputCreation(t *testing.T) {
	dir := t.TempDir()
	invalid := string([]byte{'A', 0xFF, 'B'})
	for _, encodingName := range []string{"UTF-8", "UTF-16LE", "Windows-1252"} {
		outPath := filepath.Join(dir, encodingName+".txt")
		_, err := ExportVisibleText(context.Background(), outPath, invalid, encodingName)
		if !errors.Is(err, ErrInvalidSourceText) {
			t.Fatalf("%s error = %v, want ErrInvalidSourceText", encodingName, err)
		}
		assertTextOutputAbsent(t, outPath)
	}
}

func TestTextRangeRevalidatesStreamAfterEndpointProbe(t *testing.T) {
	dir := t.TempDir()
	valid := []byte("abcdef")
	mutated := []byte{'a', 'b', 'c', 0xFF, 'e', 'f'}
	doc := &changingTextReaderAt{valid: valid, changed: mutated}
	outPath := filepath.Join(dir, "out.txt")

	_, err := ExportByteRangeText(context.Background(), doc, "", outPath, 0, int64(len(valid)), "UTF-8", "UTF-8", Options{})
	if !errors.Is(err, ErrInvalidSourceText) {
		t.Fatalf("error = %v, want ErrInvalidSourceText after source changes", err)
	}
	assertTextOutputAbsent(t, outPath)
}

func TestTextValidatorCarriesRunesCodeUnitsAndSurrogatesAcrossReadSeams(t *testing.T) {
	utf8Data := []byte{'A', 0xC2, 0xA2, 0xE2, 0x82, 0xAC, 0xF0, 0x9F, 0x98, 0x80, 'Z'}
	utf16LEData := encodeUTF16Units("UTF-16LE", 0x0041, 0xD83D, 0xDE00, 0x0042)
	utf16BEData := encodeUTF16Units("UTF-16BE", 0x0041, 0xD83D, 0xDE00, 0x0042)
	for _, tc := range []struct {
		name string
		kind textEncodingKind
		data []byte
	}{
		{name: "UTF-8", kind: textEncodingUTF8, data: utf8Data},
		{name: "UTF-16LE", kind: textEncodingUTF16LE, data: utf16LEData},
		{name: "UTF-16BE", kind: textEncodingUTF16BE, data: utf16BEData},
	} {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			reader := newValidatingTextReader(&oneByteReader{data: tc.data}, tc.kind, 0)
			got, err := io.ReadAll(reader)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(got, tc.data) {
				t.Fatalf("validated bytes = % x, want % x", got, tc.data)
			}
		})
	}

	invalid := newValidatingTextReader(&oneByteReader{data: []byte{'A', 0xF0, 0x9F}}, textEncodingUTF8, 0)
	if _, err := io.ReadAll(invalid); !errors.Is(err, ErrInvalidSourceText) {
		t.Fatalf("truncated seam error = %v, want ErrInvalidSourceText", err)
	}
}

func TestTextRangePreservesValidUTF8RuneAcrossCopyChunkBoundary(t *testing.T) {
	dir := t.TempDir()
	data := bytes.Repeat([]byte{'a'}, 64*1024-2)
	data = append(data, 0xF0, 0x9F, 0x98, 0x80, 'z')
	sourcePath, doc := openTextBoundaryFixture(t, dir, "chunk-seam.txt", data)
	outPath := filepath.Join(dir, "out.txt")

	if _, err := ExportByteRangeText(context.Background(), doc, sourcePath, outPath, 0, int64(len(data)), "UTF-8", "UTF-8", Options{}); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(outPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, data) || !utf8.Valid(got) {
		t.Fatal("valid UTF-8 rune crossing the copy chunk was not preserved exactly")
	}
}

func TestTextRangeValidationIsMemoryBoundedAndHonorsPreCanceledContext(t *testing.T) {
	dir := t.TempDir()
	const size = int64(1024*1024 + 17)
	doc := &boundedASCIIReaderAt{size: size}
	outPath := filepath.Join(dir, "bounded.txt")

	if _, err := ExportByteRangeText(context.Background(), doc, "", outPath, 0, size, "UTF-8", "UTF-8", Options{}); err != nil {
		t.Fatal(err)
	}
	if got := doc.maxRead.Load(); got > 64*1024 {
		t.Fatalf("largest source read = %d, want <= 64 KiB", got)
	}
	info, err := os.Stat(outPath)
	if err != nil {
		t.Fatal(err)
	}
	if info.Size() != size {
		t.Fatalf("output size = %d, want %d", info.Size(), size)
	}

	canceledDoc := &boundedASCIIReaderAt{size: size}
	canceledPath := filepath.Join(dir, "canceled.txt")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = ExportByteRangeText(ctx, canceledDoc, "", canceledPath, 0, size, "UTF-8", "UTF-8", Options{})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want context.Canceled", err)
	}
	if calls := canceledDoc.calls.Load(); calls != 0 {
		t.Fatalf("pre-canceled export performed %d source reads, want 0", calls)
	}
	assertTextOutputAbsent(t, canceledPath)
}

func openTextBoundaryFixture(t *testing.T, dir string, name string, data []byte) (string, *document.FileDocument) {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	doc, err := document.OpenFile(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = doc.Close() })
	return path, doc
}

func assertTextOutputAbsent(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("output %q exists after rejected text export; stat error = %v", path, err)
	}
}

func encodeUTF16Units(encodingName string, units ...uint16) []byte {
	data := make([]byte, len(units)*2)
	for i, unit := range units {
		if encodingName == "UTF-16LE" {
			binary.LittleEndian.PutUint16(data[i*2:], unit)
		} else {
			binary.BigEndian.PutUint16(data[i*2:], unit)
		}
	}
	return data
}

type changingTextReaderAt struct {
	valid   []byte
	changed []byte
	calls   atomic.Int64
}

func (r *changingTextReaderAt) Size() int64 {
	return int64(len(r.valid))
}

func (r *changingTextReaderAt) ReadAt(buf []byte, offset int64) (int, error) {
	data := r.changed
	if r.calls.Add(1) == 1 {
		data = r.valid
	}
	if offset < 0 || offset >= int64(len(data)) {
		return 0, io.EOF
	}
	n := copy(buf, data[offset:])
	if n != len(buf) {
		return n, io.EOF
	}
	return n, nil
}

type boundedASCIIReaderAt struct {
	size    int64
	calls   atomic.Int64
	maxRead atomic.Int64
}

type oneByteReader struct {
	data []byte
	next int
}

func (r *oneByteReader) Read(buf []byte) (int, error) {
	if r.next >= len(r.data) {
		return 0, io.EOF
	}
	if len(buf) == 0 {
		return 0, nil
	}
	buf[0] = r.data[r.next]
	r.next++
	return 1, nil
}

func (r *boundedASCIIReaderAt) Size() int64 {
	return r.size
}

func (r *boundedASCIIReaderAt) ReadAt(buf []byte, offset int64) (int, error) {
	r.calls.Add(1)
	for {
		old := r.maxRead.Load()
		if int64(len(buf)) <= old || r.maxRead.CompareAndSwap(old, int64(len(buf))) {
			break
		}
	}
	if offset < 0 || offset >= r.size {
		return 0, io.EOF
	}
	available := r.size - offset
	n := len(buf)
	if int64(n) > available {
		n = int(available)
	}
	for i := 0; i < n; i++ {
		buf[i] = 'a'
	}
	if n != len(buf) {
		return n, io.EOF
	}
	return n, nil
}

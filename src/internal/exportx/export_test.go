package exportx

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/quarry/quarry-wails3/internal/document"
	"github.com/quarry/quarry-wails3/internal/fileio"
)

func TestExportByteRangeWritesSelectedBytes(t *testing.T) {
	dir := t.TempDir()
	srcPath := filepath.Join(dir, "source.txt")
	outPath := filepath.Join(dir, "output.txt")
	if err := os.WriteFile(srcPath, []byte("alpha\nbravo\ncharlie\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	doc, err := document.OpenFile(srcPath)
	if err != nil {
		t.Fatal(err)
	}
	defer doc.Close()

	summary, err := ExportByteRange(context.Background(), doc, srcPath, outPath, 6, 12, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if summary.Mode != "byte-range" {
		t.Fatalf("mode = %q, want byte-range", summary.Mode)
	}
	if summary.BytesWritten != 6 {
		t.Fatalf("bytes written = %d, want 6", summary.BytesWritten)
	}

	got, err := os.ReadFile(outPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "bravo\n" {
		t.Fatalf("output = %q, want bravo\\n", string(got))
	}
}

func TestExportByteRangeUsesWholeSourceWhileEditableSliceIsDirty(t *testing.T) {
	dir := t.TempDir()
	srcPath := filepath.Join(dir, "huge-window.log")
	outPath := filepath.Join(dir, "export.txt")
	content := []byte("head original\nslice original\noutside original\n")
	if err := os.WriteFile(srcPath, content, 0o600); err != nil {
		t.Fatal(err)
	}

	doc, err := document.OpenFile(srcPath)
	if err != nil {
		t.Fatal(err)
	}
	defer doc.Close()

	sliceStart := int64(len("head original\n"))
	sliceEnd := sliceStart + int64(len("slice original\n"))
	buf, err := document.LoadInMemoryWindow(doc, sliceStart, sliceEnd, sliceEnd-sliceStart)
	if err != nil {
		t.Fatal(err)
	}
	editedSlice := strings.Replace(buf.Text, "original", "edited", 1)
	if _, _, encoded, err := buf.EncodedReplacement(editedSlice); err != nil {
		t.Fatal(err)
	} else if strings.Contains(string(encoded), "original") {
		t.Fatalf("edited replacement still contains original marker: %q", string(encoded))
	}

	summary, err := ExportByteRange(context.Background(), doc, srcPath, outPath, 0, doc.Size(), Options{ComputeSHA256: true})
	if err != nil {
		t.Fatal(err)
	}
	if summary.BytesWritten != int64(len(content)) {
		t.Fatalf("bytes written = %d, want %d", summary.BytesWritten, len(content))
	}

	got, err := os.ReadFile(outPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(content) {
		t.Fatalf("export output = %q, want original source %q", string(got), string(content))
	}
	if strings.Contains(string(got), "slice edited") {
		t.Fatalf("export leaked dirty editable-slice text: %q", string(got))
	}
	sourceAfter, err := os.ReadFile(srcPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(sourceAfter) != string(content) {
		t.Fatalf("source changed to %q, want immutable original", string(sourceAfter))
	}
}

func TestExportByteRangeComputesSHA256WhenRequested(t *testing.T) {
	dir := t.TempDir()
	srcPath := filepath.Join(dir, "source.txt")
	outPath := filepath.Join(dir, "output.txt")
	if err := os.WriteFile(srcPath, []byte("alpha\nbravo\ncharlie\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	doc, err := document.OpenFile(srcPath)
	if err != nil {
		t.Fatal(err)
	}
	defer doc.Close()

	summary, err := ExportByteRange(context.Background(), doc, srcPath, outPath, 6, 12, Options{ComputeSHA256: true})
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256([]byte("bravo\n"))
	want := hex.EncodeToString(sum[:])
	if summary.SHA256 != want {
		t.Fatalf("sha256 = %q, want %q", summary.SHA256, want)
	}
}

func TestExportByteRangeWritesManifestWhenRequested(t *testing.T) {
	dir := t.TempDir()
	srcPath := filepath.Join(dir, "source.txt")
	outPath := filepath.Join(dir, "output.txt")
	if err := os.WriteFile(srcPath, []byte("alpha\nbravo\ncharlie\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	doc, err := document.OpenFile(srcPath)
	if err != nil {
		t.Fatal(err)
	}
	defer doc.Close()

	summary, err := ExportByteRange(context.Background(), doc, srcPath, outPath, 6, 12, Options{ComputeSHA256: true, WriteManifest: true})
	if err != nil {
		t.Fatal(err)
	}
	manifest := readExportManifest(t, summary.ManifestPath)
	if manifest.Mode != "byte-range" || manifest.SourcePath != srcPath || manifest.OutputPath != outPath {
		t.Fatalf("manifest = %+v, want byte-range source/output", manifest)
	}
	if manifest.StartOffset != 6 || manifest.EndOffset != 12 || manifest.BytesWritten != 6 {
		t.Fatalf("manifest range = %+v, want offsets 6-12 and 6 bytes", manifest)
	}
	if manifest.ChecksumAlgorithm != "sha256" || manifest.SHA256 != summary.SHA256 || manifest.SHA256 == "" {
		t.Fatalf("manifest checksum = %q/%q, summary = %q", manifest.ChecksumAlgorithm, manifest.SHA256, summary.SHA256)
	}
}

func TestExportByteRangeRejectsExistingManifestBeforeWritingOutput(t *testing.T) {
	dir := t.TempDir()
	srcPath := filepath.Join(dir, "source.txt")
	outPath := filepath.Join(dir, "output.txt")
	manifestPath := exportManifestPathFor(outPath, "")
	if err := os.WriteFile(srcPath, []byte("alpha"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(manifestPath, []byte("existing manifest"), 0o600); err != nil {
		t.Fatal(err)
	}

	doc, err := document.OpenFile(srcPath)
	if err != nil {
		t.Fatal(err)
	}
	defer doc.Close()

	_, err = ExportByteRange(context.Background(), doc, srcPath, outPath, 0, doc.Size(), Options{WriteManifest: true})
	if !errors.Is(err, fileio.ErrExists) {
		t.Fatalf("err = %v, want ErrExists", err)
	}
	if _, err := os.Stat(outPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("output stat err = %v, want not exist", err)
	}
	if got, err := os.ReadFile(manifestPath); err != nil || string(got) != "existing manifest" {
		t.Fatalf("manifest changed: %q, %v", got, err)
	}
}

func TestExportVisibleRangeUsesOffsets(t *testing.T) {
	dir := t.TempDir()
	srcPath := filepath.Join(dir, "source.txt")
	outPath := filepath.Join(dir, "visible.txt")
	if err := os.WriteFile(srcPath, []byte("0123456789abcdef"), 0o600); err != nil {
		t.Fatal(err)
	}

	doc, err := document.OpenFile(srcPath)
	if err != nil {
		t.Fatal(err)
	}
	defer doc.Close()

	summary, err := ExportVisibleRange(context.Background(), doc, srcPath, outPath, 4, 10, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if summary.Mode != "visible-range" {
		t.Fatalf("mode = %q, want visible-range", summary.Mode)
	}

	got, err := os.ReadFile(outPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "456789" {
		t.Fatalf("output = %q, want 456789", string(got))
	}
}

func TestExportLineRangeWritesExactLinesWithoutCompletedIndex(t *testing.T) {
	dir := t.TempDir()
	srcPath := filepath.Join(dir, "lines.txt")
	outPath := filepath.Join(dir, "lines-out.txt")
	if err := os.WriteFile(srcPath, []byte("one\ntwo\nthree\nfour\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	doc, err := document.OpenFile(srcPath)
	if err != nil {
		t.Fatal(err)
	}
	defer doc.Close()

	summary, err := ExportLineRange(context.Background(), doc, srcPath, outPath, 2, 3, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if !summary.UsedLineRange {
		t.Fatal("expected line-range metadata")
	}
	if summary.StartLine != 2 || summary.EndLine != 3 {
		t.Fatalf("lines = %d-%d, want 2-3", summary.StartLine, summary.EndLine)
	}

	got, err := os.ReadFile(outPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "two\nthree\n" {
		t.Fatalf("output = %q, want two/three lines", string(got))
	}
}

func TestExportLineRangeWritesVeryLongSingleLineWithoutCompletedIndex(t *testing.T) {
	dir := t.TempDir()
	srcPath := filepath.Join(dir, "long-line.log")
	outPath := filepath.Join(dir, "long-line-out.log")
	firstLine := strings.Repeat("x", 256*1024) + "needle" + strings.Repeat("y", 256*1024) + "\n"
	if err := os.WriteFile(srcPath, []byte(firstLine+"second\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	doc, err := document.OpenFile(srcPath)
	if err != nil {
		t.Fatal(err)
	}
	defer doc.Close()

	summary, err := ExportLineRange(context.Background(), doc, srcPath, outPath, 1, 1, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if summary.BytesWritten != int64(len(firstLine)) {
		t.Fatalf("bytes written = %d, want %d", summary.BytesWritten, len(firstLine))
	}
	if !summary.UsedLineRange || summary.StartLine != 1 || summary.EndLine != 1 {
		t.Fatalf("summary = %#v, want line-range metadata for line 1", summary)
	}

	got, err := os.ReadFile(outPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != firstLine {
		t.Fatalf("output length = %d, want exact first long line length %d", len(got), len(firstLine))
	}
	if strings.Contains(string(got), "second") {
		t.Fatal("line-range export included the next line")
	}
}

func TestExportByteRangeRejectsConflictingPaths(t *testing.T) {
	dir := t.TempDir()
	srcPath := filepath.Join(dir, "source.txt")
	outPath := filepath.Join(dir, "output.txt")
	if err := os.WriteFile(srcPath, []byte("alpha"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(outPath, []byte("existing"), 0o600); err != nil {
		t.Fatal(err)
	}

	doc, err := document.OpenFile(srcPath)
	if err != nil {
		t.Fatal(err)
	}
	defer doc.Close()

	if _, err := ExportByteRange(context.Background(), doc, srcPath, srcPath, 0, 2, Options{}); err == nil {
		t.Fatal("expected same-path error")
	}
	if _, err := ExportByteRange(context.Background(), doc, srcPath, outPath, 0, 2, Options{}); err == nil {
		t.Fatal("expected existing-output error")
	}
}

func TestExportByteRangeCancelDeletesPartialOutput(t *testing.T) {
	dir := t.TempDir()
	srcPath := filepath.Join(dir, "source.txt")
	outPath := filepath.Join(dir, "output.txt")
	if err := os.WriteFile(srcPath, []byte(strings.Repeat("abcdef", 4096)), 0o600); err != nil {
		t.Fatal(err)
	}

	doc, err := document.OpenFile(srcPath)
	if err != nil {
		t.Fatal(err)
	}
	defer doc.Close()

	ctx, cancel := context.WithCancel(context.Background())
	_, err = ExportByteRange(ctx, doc, srcPath, outPath, 0, doc.Size(), Options{
		Progress: func(done int64, total int64) {
			if done > 0 {
				cancel()
			}
		},
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if _, err := os.Stat(outPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("partial output should be deleted, stat err = %v", err)
	}
}

func TestSplitBySizeWritesSequentialParts(t *testing.T) {
	dir := t.TempDir()
	srcPath := filepath.Join(dir, "source.txt")
	basePath := filepath.Join(dir, "split.txt")
	if err := os.WriteFile(srcPath, []byte("abcdefghijkl"), 0o600); err != nil {
		t.Fatal(err)
	}

	doc, err := document.OpenFile(srcPath)
	if err != nil {
		t.Fatal(err)
	}
	defer doc.Close()

	summary, err := SplitBySize(context.Background(), doc, srcPath, basePath, 5, SplitOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if summary.Parts != 3 {
		t.Fatalf("parts = %d, want 3", summary.Parts)
	}

	want := []string{"abcde", "fghij", "kl"}
	for i, path := range summary.Outputs {
		got, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if string(got) != want[i] {
			t.Fatalf("part %d = %q, want %q", i+1, string(got), want[i])
		}
	}
}

func TestCheckedSplitPartCountUsesExactIntegerArithmetic(t *testing.T) {
	tests := []struct {
		name    string
		total   int64
		part    int64
		want    int
		wantErr bool
	}{
		{name: "zero", total: 0, part: 1, want: 0},
		{name: "exact multiple", total: 12, part: 4, want: 3},
		{name: "one byte remainder", total: 13, part: 4, want: 4},
		{name: "above float exactness", total: 1<<53 + 1, part: 1 << 53, want: 2},
		{name: "max int exact", total: math.MaxInt64, part: math.MaxInt64, want: 1},
		{name: "max int remainder", total: math.MaxInt64, part: math.MaxInt64 - 1, want: 2},
		{name: "negative total", total: -1, part: 1, wantErr: true},
		{name: "zero part", total: 1, part: 0, wantErr: true},
		{name: "negative part", total: 1, part: -1, wantErr: true},
		{name: "too many parts", total: maxSplitParts + 1, part: 1, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := checkedSplitPartCount(tt.total, tt.part)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("checkedSplitPartCount(%d, %d) succeeded with %d", tt.total, tt.part, got)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got != tt.want {
				t.Fatalf("count = %d, want %d", got, tt.want)
			}
		})
	}
}

func TestCheckedSplitPartRangesCoverHugeSizesExactly(t *testing.T) {
	tests := []struct {
		total int64
		part  int64
	}{
		{total: 1<<53 + 1, part: 1 << 53},
		{total: math.MaxInt64, part: math.MaxInt64 - 1},
		{total: 10_001, part: 2},
	}
	for _, tt := range tests {
		count, err := checkedSplitPartCount(tt.total, tt.part)
		if err != nil {
			t.Fatal(err)
		}
		var previousEnd int64
		for part := 0; part < count; part++ {
			start, end, err := checkedSplitPartRange(tt.total, tt.part, part)
			if err != nil {
				t.Fatal(err)
			}
			if start != previousEnd {
				t.Fatalf("total=%d part=%d index=%d starts at %d after prior end %d", tt.total, tt.part, part, start, previousEnd)
			}
			if end <= start || end > tt.total {
				t.Fatalf("invalid range [%d,%d) for total %d", start, end, tt.total)
			}
			previousEnd = end
		}
		if previousEnd != tt.total {
			t.Fatalf("covered through %d, want %d", previousEnd, tt.total)
		}
	}
}

type fixedSizeReader struct{ size int64 }

func (r fixedSizeReader) Size() int64 { return r.size }

func (r fixedSizeReader) ReadAt([]byte, int64) (int, error) {
	return 0, errors.New("read must not be reached for a rejected split")
}

func TestSplitBySizeRejectsExcessivePartCountBeforeCreatingArtifacts(t *testing.T) {
	dir := t.TempDir()
	basePath := filepath.Join(dir, "split.bin")
	_, err := SplitBySize(context.Background(), fixedSizeReader{size: maxSplitParts + 1}, "source.bin", basePath, 1, SplitOptions{})
	if err == nil {
		t.Fatal("expected excessive part-count error")
	}
	entries, readErr := os.ReadDir(dir)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if len(entries) != 0 {
		t.Fatalf("split created artifacts before rejecting hostile count: %v", entries)
	}
}

func TestSplitBySizeUsesWholeSourceWhileEditableSliceIsDirty(t *testing.T) {
	dir := t.TempDir()
	srcPath := filepath.Join(dir, "huge-window.log")
	basePath := filepath.Join(dir, "split.txt")
	content := []byte("head original\nslice original\noutside original\n")
	if err := os.WriteFile(srcPath, content, 0o600); err != nil {
		t.Fatal(err)
	}

	doc, err := document.OpenFile(srcPath)
	if err != nil {
		t.Fatal(err)
	}
	defer doc.Close()

	sliceStart := int64(len("head original\n"))
	sliceEnd := sliceStart + int64(len("slice original\n"))
	buf, err := document.LoadInMemoryWindow(doc, sliceStart, sliceEnd, sliceEnd-sliceStart)
	if err != nil {
		t.Fatal(err)
	}
	editedSlice := strings.Replace(buf.Text, "original", "edited", 1)
	if _, _, encoded, err := buf.EncodedReplacement(editedSlice); err != nil {
		t.Fatal(err)
	} else if strings.Contains(string(encoded), "original") {
		t.Fatalf("edited replacement still contains original marker: %q", string(encoded))
	}

	summary, err := SplitBySize(context.Background(), doc, srcPath, basePath, int64(len("head original\nslice ")), SplitOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if summary.Parts != 3 {
		t.Fatalf("parts = %d, want 3", summary.Parts)
	}

	var combined strings.Builder
	for _, path := range summary.Outputs {
		part, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		combined.Write(part)
	}
	if combined.String() != string(content) {
		t.Fatalf("split output = %q, want original source %q", combined.String(), string(content))
	}
	if strings.Contains(combined.String(), "slice edited") {
		t.Fatalf("split leaked dirty editable-slice text: %q", combined.String())
	}
	sourceAfter, err := os.ReadFile(srcPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(sourceAfter) != string(content) {
		t.Fatalf("source changed to %q, want immutable original", string(sourceAfter))
	}
}

func TestSplitBySizeComputesPartSHA256WhenRequested(t *testing.T) {
	dir := t.TempDir()
	srcPath := filepath.Join(dir, "source.txt")
	basePath := filepath.Join(dir, "split.txt")
	if err := os.WriteFile(srcPath, []byte("abcdefghijkl"), 0o600); err != nil {
		t.Fatal(err)
	}

	doc, err := document.OpenFile(srcPath)
	if err != nil {
		t.Fatal(err)
	}
	defer doc.Close()

	summary, err := SplitBySize(context.Background(), doc, srcPath, basePath, 5, SplitOptions{ComputeSHA256: true})
	if err != nil {
		t.Fatal(err)
	}
	if summary.ChecksumAlgorithm != "sha256" {
		t.Fatalf("checksum algorithm = %q, want sha256", summary.ChecksumAlgorithm)
	}
	if !summary.Complete || summary.Failure != "" {
		t.Fatalf("summary status = %+v", summary)
	}
	if len(summary.OutputChecksums) != 3 {
		t.Fatalf("checksums = %d, want 3", len(summary.OutputChecksums))
	}
	sum := sha256.Sum256([]byte("abcde"))
	want := hex.EncodeToString(sum[:])
	if summary.OutputChecksums[0].SHA256 != want || summary.OutputChecksums[0].Path != partPath(basePath, 1) {
		t.Fatalf("first checksum = %+v, want path %q sha %q", summary.OutputChecksums[0], partPath(basePath, 1), want)
	}

	manifest := readSplitManifest(t, summary.ManifestPath)
	if manifest.Mode != "split-size" || manifest.Parts != 3 || !manifest.Complete || manifest.Failure != "" {
		t.Fatalf("manifest = %+v, want split-size with 3 parts", manifest)
	}
	if manifest.OutputChecksums[0].SHA256 != want || manifest.OutputChecksums[0].Path != partPath(basePath, 1) {
		t.Fatalf("manifest first checksum = %+v, want path %q sha %q", manifest.OutputChecksums[0], partPath(basePath, 1), want)
	}
}

func TestSplitByLineCountWritesExactLines(t *testing.T) {
	dir := t.TempDir()
	srcPath := filepath.Join(dir, "source.txt")
	basePath := filepath.Join(dir, "split.txt")
	if err := os.WriteFile(srcPath, []byte("one\ntwo\nthree\nfour\nfive\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	doc, err := document.OpenFile(srcPath)
	if err != nil {
		t.Fatal(err)
	}
	defer doc.Close()

	summary, err := SplitByLineCount(context.Background(), doc, srcPath, basePath, 2, SplitOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if summary.Parts != 3 || !summary.Complete || summary.Failure != "" {
		t.Fatalf("parts = %d, want 3", summary.Parts)
	}

	want := []string{"one\ntwo\n", "three\nfour\n", "five\n"}
	for i, path := range summary.Outputs {
		got, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if string(got) != want[i] {
			t.Fatalf("part %d = %q, want %q", i+1, string(got), want[i])
		}
	}
}

func TestSplitByLineCountComputesPartSHA256WhenRequested(t *testing.T) {
	dir := t.TempDir()
	srcPath := filepath.Join(dir, "source.txt")
	basePath := filepath.Join(dir, "split.txt")
	if err := os.WriteFile(srcPath, []byte("one\ntwo\nthree\nfour\nfive\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	doc, err := document.OpenFile(srcPath)
	if err != nil {
		t.Fatal(err)
	}
	defer doc.Close()

	summary, err := SplitByLineCount(context.Background(), doc, srcPath, basePath, 2, SplitOptions{ComputeSHA256: true})
	if err != nil {
		t.Fatal(err)
	}
	if summary.ChecksumAlgorithm != "sha256" {
		t.Fatalf("checksum algorithm = %q, want sha256", summary.ChecksumAlgorithm)
	}
	if len(summary.OutputChecksums) != 3 {
		t.Fatalf("checksums = %d, want 3", len(summary.OutputChecksums))
	}
	sum := sha256.Sum256([]byte("one\ntwo\n"))
	want := hex.EncodeToString(sum[:])
	if summary.OutputChecksums[0].SHA256 != want || summary.OutputChecksums[0].Path != partPath(basePath, 1) {
		t.Fatalf("first checksum = %+v, want path %q sha %q", summary.OutputChecksums[0], partPath(basePath, 1), want)
	}

	manifest := readSplitManifest(t, summary.ManifestPath)
	if manifest.Mode != "split-lines" || manifest.Parts != 3 || !manifest.Complete || manifest.Failure != "" {
		t.Fatalf("manifest = %+v, want split-lines with 3 parts", manifest)
	}
	if manifest.OutputChecksums[0].SHA256 != want || manifest.OutputChecksums[0].Path != partPath(basePath, 1) {
		t.Fatalf("manifest first checksum = %+v, want path %q sha %q", manifest.OutputChecksums[0], partPath(basePath, 1), want)
	}
}

func TestSplitBySizeRejectsExistingManifestBeforeWritingParts(t *testing.T) {
	dir := t.TempDir()
	srcPath := filepath.Join(dir, "source.txt")
	basePath := filepath.Join(dir, "split.txt")
	manifestPath := splitManifestPathFor(basePath, "")
	if err := os.WriteFile(srcPath, []byte("abcdef"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(manifestPath, []byte("existing manifest"), 0o600); err != nil {
		t.Fatal(err)
	}

	doc, err := document.OpenFile(srcPath)
	if err != nil {
		t.Fatal(err)
	}
	defer doc.Close()

	summary, err := SplitBySize(context.Background(), doc, srcPath, basePath, 3, SplitOptions{ComputeSHA256: true})
	if !errors.Is(err, fileio.ErrExists) {
		t.Fatalf("err = %v, want ErrExists", err)
	}
	if summary.Complete || summary.Failure == "" || len(summary.Outputs) != 0 {
		t.Fatalf("summary = %+v", summary)
	}
	if matches, matchErr := filepath.Glob(filepath.Join(dir, "split.part*.txt")); matchErr != nil {
		t.Fatal(matchErr)
	} else if len(matches) != 0 {
		t.Fatalf("split parts created despite manifest conflict: %v", matches)
	}
	got, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "existing manifest" {
		t.Fatalf("manifest changed to %q", got)
	}
}

func TestEnsureManifestAvailableUsesKindInRequiredPathError(t *testing.T) {
	if err := ensureExportManifestAvailable(""); err == nil || err.Error() != "export manifest path is required" {
		t.Fatalf("export manifest error = %v", err)
	}
	if err := ensureSplitManifestAvailable(""); err == nil || err.Error() != "split manifest path is required" {
		t.Fatalf("split manifest error = %v", err)
	}
}

func TestSplitManifestDoesNotTouchLegacyDeterministicTemp(t *testing.T) {
	dir := t.TempDir()
	srcPath := filepath.Join(dir, "source.txt")
	basePath := filepath.Join(dir, "split.txt")
	manifestPath := filepath.Join(dir, "manifest.json")
	legacyTempPath := manifestPath + ".quarry.tmp"
	if err := os.WriteFile(srcPath, []byte("abcdef"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(legacyTempPath, []byte("unowned sentinel"), 0o600); err != nil {
		t.Fatal(err)
	}
	doc, err := document.OpenFile(srcPath)
	if err != nil {
		t.Fatal(err)
	}
	defer doc.Close()

	summary, err := SplitBySize(context.Background(), doc, srcPath, basePath, 3, SplitOptions{ManifestPath: manifestPath})
	if err != nil {
		t.Fatal(err)
	}
	if !summary.Complete {
		t.Fatalf("summary = %+v, want complete", summary)
	}
	if got, err := os.ReadFile(legacyTempPath); err != nil || string(got) != "unowned sentinel" {
		t.Fatalf("legacy temp = %q, %v", got, err)
	}
}

func TestExportManifestDoesNotTouchLegacyDeterministicTemp(t *testing.T) {
	dir := t.TempDir()
	srcPath := filepath.Join(dir, "source.txt")
	outPath := filepath.Join(dir, "output.txt")
	manifestPath := filepath.Join(dir, "manifest.json")
	legacyTempPath := manifestPath + ".quarry.tmp"
	if err := os.WriteFile(srcPath, []byte("alpha"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(legacyTempPath, []byte("unowned sentinel"), 0o600); err != nil {
		t.Fatal(err)
	}
	doc, err := document.OpenFile(srcPath)
	if err != nil {
		t.Fatal(err)
	}
	defer doc.Close()

	if _, err := ExportByteRange(context.Background(), doc, srcPath, outPath, 0, doc.Size(), Options{ManifestPath: manifestPath}); err != nil {
		t.Fatal(err)
	}
	if got, err := os.ReadFile(legacyTempPath); err != nil || string(got) != "unowned sentinel" {
		t.Fatalf("legacy temp = %q, %v", got, err)
	}
	if _, err := os.Stat(manifestPath); err != nil {
		t.Fatalf("manifest not published: %v", err)
	}
}

func readSplitManifest(t *testing.T, path string) SplitSummary {
	t.Helper()
	if path == "" {
		t.Fatal("manifest path is empty")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var summary SplitSummary
	if err := json.Unmarshal(data, &summary); err != nil {
		t.Fatalf("decode split manifest: %v", err)
	}
	if summary.ManifestPath != path {
		t.Fatalf("manifest path = %q, want %q", summary.ManifestPath, path)
	}
	return summary
}

func readExportManifest(t *testing.T, path string) Summary {
	t.Helper()
	if path == "" {
		t.Fatal("manifest path is empty")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var summary Summary
	if err := json.Unmarshal(data, &summary); err != nil {
		t.Fatalf("decode export manifest: %v", err)
	}
	if summary.ManifestPath != path {
		t.Fatalf("manifest path = %q, want %q", summary.ManifestPath, path)
	}
	return summary
}

func TestSplitBySizeRejectsExistingPartOutput(t *testing.T) {
	dir := t.TempDir()
	srcPath := filepath.Join(dir, "source.txt")
	basePath := filepath.Join(dir, "split.txt")
	if err := os.WriteFile(srcPath, []byte("abcdef"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(partPath(basePath, 1), []byte("existing"), 0o600); err != nil {
		t.Fatal(err)
	}

	doc, err := document.OpenFile(srcPath)
	if err != nil {
		t.Fatal(err)
	}
	defer doc.Close()

	summary, err := SplitBySize(context.Background(), doc, srcPath, basePath, 3, SplitOptions{})
	if !errors.Is(err, fileio.ErrExists) {
		t.Fatalf("error = %v, want ErrExists", err)
	}
	if summary.Complete || summary.Failure == "" || len(summary.Outputs) != 0 {
		t.Fatalf("summary = %+v", summary)
	}
	assertFileBytes(t, partPath(basePath, 1), []byte("existing"))
}

func TestSplitBySizeExistingLaterPartPreservesVerifiedEarlierPart(t *testing.T) {
	dir := t.TempDir()
	srcPath := filepath.Join(dir, "source.txt")
	basePath := filepath.Join(dir, "split.txt")
	if err := os.WriteFile(srcPath, []byte("abcdef"), 0o600); err != nil {
		t.Fatal(err)
	}
	conflictPath := partPath(basePath, 2)
	if err := os.WriteFile(conflictPath, []byte("existing"), 0o600); err != nil {
		t.Fatal(err)
	}

	doc, err := document.OpenFile(srcPath)
	if err != nil {
		t.Fatal(err)
	}
	defer doc.Close()

	summary, err := SplitBySize(context.Background(), doc, srcPath, basePath, 3, SplitOptions{})
	if !errors.Is(err, fileio.ErrExists) {
		t.Fatalf("error = %v, want ErrExists", err)
	}
	var incomplete *SplitIncompleteError
	if !errors.As(err, &incomplete) || summary.Complete || len(summary.Outputs) != 1 || len(incomplete.Outputs) != 1 {
		t.Fatalf("summary = %+v, error = %v", summary, err)
	}
	assertFileBytes(t, partPath(basePath, 1), []byte("abc"))
	if got, readErr := os.ReadFile(conflictPath); readErr != nil {
		t.Fatal(readErr)
	} else if string(got) != "existing" {
		t.Fatalf("conflicting part = %q, want existing", string(got))
	}
}

func TestSplitByLineCountExistingLaterPartPreservesVerifiedEarlierPart(t *testing.T) {
	dir := t.TempDir()
	srcPath := filepath.Join(dir, "source.txt")
	basePath := filepath.Join(dir, "split.txt")
	if err := os.WriteFile(srcPath, []byte("one\ntwo\nthree\nfour\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	conflictPath := partPath(basePath, 2)
	if err := os.WriteFile(conflictPath, []byte("existing"), 0o600); err != nil {
		t.Fatal(err)
	}

	doc, err := document.OpenFile(srcPath)
	if err != nil {
		t.Fatal(err)
	}
	defer doc.Close()

	summary, splitErr := SplitByLineCount(context.Background(), doc, srcPath, basePath, 2, SplitOptions{})
	if !errors.Is(splitErr, fileio.ErrExists) {
		t.Fatalf("error = %v, want ErrExists", splitErr)
	}
	if summary.Complete || summary.Failure == "" || len(summary.Outputs) != 1 {
		t.Fatalf("summary = %+v", summary)
	}
	assertFileBytes(t, partPath(basePath, 1), []byte("one\ntwo\n"))
	got, err := os.ReadFile(conflictPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "existing" {
		t.Fatalf("conflicting part = %q, want existing", string(got))
	}
}

func TestSplitBySizeCancelPreservesVerifiedParts(t *testing.T) {
	dir := t.TempDir()
	srcPath := filepath.Join(dir, "source.txt")
	basePath := filepath.Join(dir, "split.txt")
	if err := os.WriteFile(srcPath, []byte(strings.Repeat("abcdef", 4096)), 0o600); err != nil {
		t.Fatal(err)
	}

	doc, err := document.OpenFile(srcPath)
	if err != nil {
		t.Fatal(err)
	}
	defer doc.Close()

	ctx, cancel := context.WithCancel(context.Background())
	var canceled bool
	summary, err := SplitBySize(ctx, doc, srcPath, basePath, 4096, SplitOptions{
		Progress: func(done int64, total int64, parts int) {
			if !canceled && parts >= 1 && done > 0 {
				canceled = true
				cancel()
			}
		},
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if summary.Complete || summary.Failure == "" || len(summary.Outputs) != 1 {
		t.Fatalf("summary = %+v", summary)
	}
	want := []byte(strings.Repeat("abcdef", 4096))[:4096]
	assertFileBytes(t, partPath(basePath, 1), want)
}

func TestSplitByLineCountCancelPreservesVerifiedParts(t *testing.T) {
	dir := t.TempDir()
	srcPath := filepath.Join(dir, "source.txt")
	basePath := filepath.Join(dir, "split.txt")
	if err := os.WriteFile(srcPath, []byte("one\ntwo\nthree\nfour\nfive\nsix\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	doc, err := document.OpenFile(srcPath)
	if err != nil {
		t.Fatal(err)
	}
	defer doc.Close()

	ctx, cancel := context.WithCancel(context.Background())
	var canceled bool
	summary, err := SplitByLineCount(ctx, doc, srcPath, basePath, 2, SplitOptions{
		Progress: func(done int64, total int64, parts int) {
			if !canceled && parts >= 1 && done > 0 {
				canceled = true
				cancel()
			}
		},
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if summary.Complete || summary.Failure == "" || len(summary.Outputs) != 1 {
		t.Fatalf("summary = %+v", summary)
	}
	assertFileBytes(t, partPath(basePath, 1), []byte("one\ntwo\n"))
}

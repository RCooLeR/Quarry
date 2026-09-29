package exportx

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/quarry/quarry-wails3/internal/document"
	"github.com/quarry/quarry-wails3/internal/fileio"
)

type shortReaderAtSize struct {
	data []byte
	size int64
}

func (r shortReaderAtSize) Size() int64 { return r.size }

func (r shortReaderAtSize) ReadAt(p []byte, off int64) (int, error) {
	if off < 0 || off >= int64(len(r.data)) {
		return 0, io.EOF
	}
	n := copy(p, r.data[off:])
	if n != len(p) {
		return n, io.EOF
	}
	return n, nil
}

func TestExportByteRangeRejectsShortSourceWithoutPublishing(t *testing.T) {
	dir := t.TempDir()
	srcPath := filepath.Join(dir, "source.bin")
	outPath := filepath.Join(dir, "output.bin")
	if err := os.WriteFile(srcPath, []byte("abc"), 0o600); err != nil {
		t.Fatal(err)
	}
	doc := shortReaderAtSize{data: []byte("abc"), size: 8}

	summary, err := ExportByteRange(context.Background(), doc, srcPath, outPath, 0, doc.Size(), Options{})
	if !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("error = %v, want io.ErrUnexpectedEOF", err)
	}
	if summary.BytesWritten != 3 {
		t.Fatalf("partial bytes = %d, want 3", summary.BytesWritten)
	}
	if _, statErr := os.Lstat(outPath); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("short source published output: %v", statErr)
	}
	assertFileBytes(t, srcPath, []byte("abc"))
}

func TestExportByteRangeTextRejectsShortSourceWithoutPublishing(t *testing.T) {
	dir := t.TempDir()
	srcPath := filepath.Join(dir, "source.txt")
	outPath := filepath.Join(dir, "output.txt")
	if err := os.WriteFile(srcPath, []byte("abc"), 0o600); err != nil {
		t.Fatal(err)
	}
	doc := shortReaderAtSize{data: []byte("abc"), size: 8}

	_, err := ExportByteRangeText(context.Background(), doc, srcPath, outPath, 0, doc.Size(), "UTF-8", "UTF-8", Options{})
	if !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("error = %v, want io.ErrUnexpectedEOF", err)
	}
	if _, statErr := os.Lstat(outPath); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("short source published output: %v", statErr)
	}
	assertFileBytes(t, srcPath, []byte("abc"))
}

func TestSingleFileExportsRejectSourceAliasesAndExistingDestinations(t *testing.T) {
	type exporter func(context.Context, document.ReaderAtSize, string, string) error
	exporters := map[string]exporter{
		"raw": func(ctx context.Context, doc document.ReaderAtSize, src, dst string) error {
			_, err := ExportByteRange(ctx, doc, src, dst, 0, doc.Size(), Options{})
			return err
		},
		"text": func(ctx context.Context, doc document.ReaderAtSize, src, dst string) error {
			_, err := ExportByteRangeText(ctx, doc, src, dst, 0, doc.Size(), "UTF-8", "UTF-8", Options{})
			return err
		},
	}

	for name, export := range exporters {
		name, export := name, export
		t.Run(name, func(t *testing.T) {
			t.Run("same", func(t *testing.T) {
				dir := t.TempDir()
				srcPath, doc := openExportSource(t, dir)
				defer doc.Close()
				if err := export(context.Background(), doc, srcPath, srcPath); !errors.Is(err, fileio.ErrSourceAlias) {
					t.Fatalf("error = %v, want ErrSourceAlias", err)
				}
				assertFileBytes(t, srcPath, []byte("alpha\nbeta\n"))
			})

			t.Run("hard-link", func(t *testing.T) {
				dir := t.TempDir()
				srcPath, doc := openExportSource(t, dir)
				defer doc.Close()
				dstPath := filepath.Join(dir, "hard-link.txt")
				if err := os.Link(srcPath, dstPath); err != nil {
					t.Skipf("hard links unavailable: %v", err)
				}
				if err := export(context.Background(), doc, srcPath, dstPath); !errors.Is(err, fileio.ErrSourceAlias) {
					t.Fatalf("error = %v, want ErrSourceAlias", err)
				}
				assertFileBytes(t, srcPath, []byte("alpha\nbeta\n"))
			})

			t.Run("symlink", func(t *testing.T) {
				dir := t.TempDir()
				srcPath, doc := openExportSource(t, dir)
				defer doc.Close()
				dstPath := filepath.Join(dir, "symlink.txt")
				if err := os.Symlink(srcPath, dstPath); err != nil {
					t.Skipf("symlinks unavailable: %v", err)
				}
				if err := export(context.Background(), doc, srcPath, dstPath); !errors.Is(err, fileio.ErrSourceAlias) {
					t.Fatalf("error = %v, want ErrSourceAlias", err)
				}
				assertFileBytes(t, srcPath, []byte("alpha\nbeta\n"))
			})

			t.Run("existing", func(t *testing.T) {
				dir := t.TempDir()
				srcPath, doc := openExportSource(t, dir)
				defer doc.Close()
				dstPath := filepath.Join(dir, "existing.txt")
				if err := os.WriteFile(dstPath, []byte("sentinel"), 0o600); err != nil {
					t.Fatal(err)
				}
				if err := export(context.Background(), doc, srcPath, dstPath); !errors.Is(err, fileio.ErrExists) {
					t.Fatalf("error = %v, want ErrExists", err)
				}
				assertFileBytes(t, dstPath, []byte("sentinel"))
			})
		})
	}
}

func TestExportManifestRacePreservesCompetitorAndReportsPublishedOutput(t *testing.T) {
	dir := t.TempDir()
	srcPath := filepath.Join(dir, "source.txt")
	outPath := filepath.Join(dir, "output.txt")
	manifestPath := filepath.Join(dir, "manifest.json")
	source := []byte(strings.Repeat("abcdef", 400_000))
	if err := os.WriteFile(srcPath, source, 0o600); err != nil {
		t.Fatal(err)
	}
	doc, err := document.OpenFile(srcPath)
	if err != nil {
		t.Fatal(err)
	}
	defer doc.Close()

	createdCompetitor := false
	summary, err := ExportByteRange(context.Background(), doc, srcPath, outPath, 0, doc.Size(), Options{
		ManifestPath: manifestPath,
		Progress: func(done int64, total int64) {
			if createdCompetitor || done == 0 {
				return
			}
			createdCompetitor = true
			if writeErr := os.WriteFile(manifestPath, []byte("competitor"), 0o600); writeErr != nil {
				t.Fatalf("create competing manifest: %v", writeErr)
			}
		},
	})
	var publication *fileio.PublicationError
	if !errors.As(err, &publication) || publication.FinalPath != outPath {
		t.Fatalf("error = %v, want output PublicationError", err)
	}
	if summary.BytesWritten != int64(len(source)) {
		t.Fatalf("bytes written = %d, want %d", summary.BytesWritten, len(source))
	}
	assertFileBytes(t, outPath, source)
	assertFileBytes(t, manifestPath, []byte("competitor"))
}

func TestExportManifestRejectsOutputAndSourceAliasesBeforePublication(t *testing.T) {
	dir := t.TempDir()
	srcPath, doc := openExportSource(t, dir)
	defer doc.Close()
	outPath := filepath.Join(dir, "output.txt")

	if _, err := ExportByteRange(context.Background(), doc, srcPath, outPath, 0, doc.Size(), Options{ManifestPath: outPath}); !errors.Is(err, fileio.ErrSourceAlias) {
		t.Fatalf("output-alias manifest error = %v, want ErrSourceAlias", err)
	}
	if _, err := os.Lstat(outPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("output created before alias rejection: %v", err)
	}

	manifestPath := filepath.Join(dir, "source-manifest-link")
	if err := os.Link(srcPath, manifestPath); err != nil {
		t.Skipf("hard links unavailable: %v", err)
	}
	if _, err := ExportByteRange(context.Background(), doc, srcPath, outPath, 0, doc.Size(), Options{ManifestPath: manifestPath}); !errors.Is(err, fileio.ErrSourceAlias) {
		t.Fatalf("source-alias manifest error = %v, want ErrSourceAlias", err)
	}
	assertFileBytes(t, srcPath, []byte("alpha\nbeta\n"))
}

func TestExportPreservesExactOutputAndManifestPathSpelling(t *testing.T) {
	dir := t.TempDir()
	srcPath, doc := openExportSource(t, dir)
	defer doc.Close()
	outPath := filepath.Join(dir, " output.txt")
	manifestPath := filepath.Join(dir, " manifest.json")

	summary, err := ExportByteRange(context.Background(), doc, srcPath, outPath, 0, doc.Size(), Options{ManifestPath: manifestPath})
	if err != nil {
		t.Fatal(err)
	}
	if summary.OutputPath != outPath || summary.ManifestPath != manifestPath {
		t.Fatalf("summary paths = %q / %q, want exact %q / %q", summary.OutputPath, summary.ManifestPath, outPath, manifestPath)
	}
	if _, err := os.Stat(outPath); err != nil {
		t.Fatalf("exact output path missing: %v", err)
	}
	if _, err := os.Stat(manifestPath); err != nil {
		t.Fatalf("exact manifest path missing: %v", err)
	}
	if runtime.GOOS != "windows" {
		for _, path := range []string{outPath, manifestPath} {
			info, err := os.Stat(path)
			if err != nil {
				t.Fatal(err)
			}
			if got := info.Mode().Perm(); got != 0o600 {
				t.Fatalf("mode for %q = %04o, want 0600", path, got)
			}
		}
	}
	if _, err := os.Lstat(filepath.Join(dir, "output.txt")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("trimmed output path unexpectedly exists: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(dir, "manifest.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("trimmed manifest path unexpectedly exists: %v", err)
	}
}

func TestSplitCancelNeverDeletesSubstitutedPublishedPart(t *testing.T) {
	dir := t.TempDir()
	srcPath := filepath.Join(dir, "source.txt")
	basePath := filepath.Join(dir, "split.txt")
	partOne := partPath(basePath, 1)
	movedPart := partOne + ".moved"
	if err := os.WriteFile(srcPath, []byte("abcdef"), 0o600); err != nil {
		t.Fatal(err)
	}
	doc, err := document.OpenFile(srcPath)
	if err != nil {
		t.Fatal(err)
	}
	defer doc.Close()

	ctx, cancel := context.WithCancel(context.Background())
	substituted := false
	summary, err := SplitBySize(ctx, doc, srcPath, basePath, 3, SplitOptions{
		Progress: func(done int64, total int64, completedParts int) {
			if substituted || completedParts < 1 {
				return
			}
			substituted = true
			if renameErr := os.Rename(partOne, movedPart); renameErr != nil {
				t.Fatalf("move published part: %v", renameErr)
			}
			if writeErr := os.WriteFile(partOne, []byte("unowned sentinel"), 0o600); writeErr != nil {
				t.Fatalf("substitute published path: %v", writeErr)
			}
			cancel()
		},
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want context.Canceled", err)
	}
	var incomplete *SplitIncompleteError
	if !errors.As(err, &incomplete) || summary.Complete || summary.Failure == "" || len(summary.Outputs) != 1 {
		t.Fatalf("summary = %+v, error = %v", summary, err)
	}
	assertFileBytes(t, partOne, []byte("unowned sentinel"))
	assertFileBytes(t, movedPart, []byte("abc"))
}

func TestSplitManifestRacePreservesCompetitorAndVerifiedParts(t *testing.T) {
	dir := t.TempDir()
	srcPath := filepath.Join(dir, "source.txt")
	basePath := filepath.Join(dir, "split.txt")
	manifestPath := filepath.Join(dir, "manifest.json")
	if err := os.WriteFile(srcPath, []byte("abcdef"), 0o600); err != nil {
		t.Fatal(err)
	}
	doc, err := document.OpenFile(srcPath)
	if err != nil {
		t.Fatal(err)
	}
	defer doc.Close()

	createdCompetitor := false
	summary, err := SplitBySize(context.Background(), doc, srcPath, basePath, 3, SplitOptions{
		ManifestPath: manifestPath,
		Progress: func(done int64, total int64, completedParts int) {
			if createdCompetitor || completedParts < 1 {
				return
			}
			createdCompetitor = true
			if writeErr := os.WriteFile(manifestPath, []byte("competitor"), 0o600); writeErr != nil {
				t.Fatalf("create competing manifest: %v", writeErr)
			}
		},
	})
	if !errors.Is(err, fileio.ErrExists) {
		t.Fatalf("error = %v, want ErrExists", err)
	}
	var incomplete *SplitIncompleteError
	if !errors.As(err, &incomplete) || summary.Complete || len(summary.Outputs) != 2 || len(incomplete.Outputs) != 2 {
		t.Fatalf("summary = %+v, error = %v", summary, err)
	}
	assertFileBytes(t, partPath(basePath, 1), []byte("abc"))
	assertFileBytes(t, partPath(basePath, 2), []byte("def"))
	assertFileBytes(t, manifestPath, []byte("competitor"))
}

func TestSplitManifestRejectsSourceAndPartAliasesBeforePublication(t *testing.T) {
	dir := t.TempDir()
	srcPath, doc := openExportSource(t, dir)
	defer doc.Close()
	basePath := filepath.Join(dir, "split.txt")

	if summary, err := SplitBySize(context.Background(), doc, srcPath, basePath, 3, SplitOptions{ManifestPath: srcPath}); !errors.Is(err, fileio.ErrSourceAlias) {
		t.Fatalf("source-alias error = %v, summary = %+v", err, summary)
	}
	if _, err := os.Lstat(partPath(basePath, 1)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("source alias created part: %v", err)
	}

	partOne := partPath(basePath, 1)
	if summary, err := SplitBySize(context.Background(), doc, srcPath, basePath, 3, SplitOptions{ManifestPath: partOne}); !errors.Is(err, fileio.ErrSourceAlias) {
		t.Fatalf("part-alias error = %v, summary = %+v", err, summary)
	}
	if _, err := os.Lstat(partOne); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("part alias created output: %v", err)
	}
}

func TestSplitPreservesExactPathsAndPrivateModes(t *testing.T) {
	dir := t.TempDir()
	srcPath, doc := openExportSource(t, dir)
	defer doc.Close()
	basePath := filepath.Join(dir, " split.txt")
	manifestPath := filepath.Join(dir, " split-manifest.json")

	summary, err := SplitBySize(context.Background(), doc, srcPath, basePath, 6, SplitOptions{ManifestPath: manifestPath})
	if err != nil {
		t.Fatal(err)
	}
	if !summary.Complete || summary.Failure != "" || summary.SourcePath != srcPath || summary.BasePath != basePath || summary.ManifestPath != manifestPath {
		t.Fatalf("summary = %+v", summary)
	}
	if len(summary.Outputs) != 2 || summary.Outputs[0] != partPath(basePath, 1) {
		t.Fatalf("outputs = %v", summary.Outputs)
	}
	manifest := readSplitManifest(t, manifestPath)
	if !manifest.Complete || manifest.Failure != "" || manifest.BasePath != basePath {
		t.Fatalf("manifest = %+v", manifest)
	}
	if runtime.GOOS != "windows" {
		for _, path := range append(append([]string(nil), summary.Outputs...), manifestPath) {
			info, err := os.Stat(path)
			if err != nil {
				t.Fatal(err)
			}
			if got := info.Mode().Perm(); got != 0o600 {
				t.Fatalf("mode for %q = %04o, want 0600", path, got)
			}
		}
	}
	if _, err := os.Lstat(filepath.Join(dir, "split.part0001.txt")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("trimmed part path unexpectedly exists: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(dir, "split-manifest.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("trimmed manifest path unexpectedly exists: %v", err)
	}
}

func TestSplitPublicationErrorRecordsThePublishedPart(t *testing.T) {
	outputPath := "published.part0001.bin"
	publication := &fileio.PublicationError{FinalPath: outputPath, Durable: false, Err: errors.New("directory sync failed")}
	if !splitPartWasPublished(publication, outputPath) {
		t.Fatal("PublicationError was not recognized as a published split part")
	}
	summary := SplitSummary{ManifestPath: "split-manifest.json"}
	recordSplitPart(&summary, outputPath, Summary{BytesWritten: 7, SHA256: "checksum"}, true)
	failed, err := failSplit(summary, publication)
	var incomplete *SplitIncompleteError
	if !errors.As(err, &incomplete) || len(failed.Outputs) != 1 || failed.Outputs[0] != outputPath || failed.BytesWritten != 7 {
		t.Fatalf("summary = %+v, error = %v", failed, err)
	}
	if len(failed.OutputChecksums) != 1 || failed.OutputChecksums[0].SHA256 != "checksum" {
		t.Fatalf("checksums = %+v", failed.OutputChecksums)
	}
}

func openExportSource(t *testing.T, dir string) (string, *document.FileDocument) {
	t.Helper()
	srcPath := filepath.Join(dir, "source.txt")
	if err := os.WriteFile(srcPath, []byte("alpha\nbeta\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	doc, err := document.OpenFile(srcPath)
	if err != nil {
		t.Fatal(err)
	}
	return srcPath, doc
}

package exportx

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/quarry/quarry-wails3/internal/document"
	"github.com/quarry/quarry-wails3/internal/fileio"
)

func TestExportByteRangeManifestPublishFailureLeavesPublishedOutputAndPreservesSource(t *testing.T) {
	dir := t.TempDir()
	srcPath := filepath.Join(dir, "source.txt")
	outPath := filepath.Join(dir, "output.txt")
	manifestPath := filepath.Join(dir, "missing-dir", "manifest.json")
	source := []byte("alpha\nbravo\ncharlie\n")
	if err := os.WriteFile(srcPath, source, 0o600); err != nil {
		t.Fatal(err)
	}

	doc, err := document.OpenFile(srcPath)
	if err != nil {
		t.Fatal(err)
	}
	defer doc.Close()

	summary, err := ExportByteRange(context.Background(), doc, srcPath, outPath, 6, 12, Options{
		ComputeSHA256: true,
		ManifestPath:  manifestPath,
	})
	if err == nil {
		t.Fatal("expected manifest publish failure")
	}
	var publication *fileio.PublicationError
	if !errors.As(err, &publication) {
		t.Fatalf("error = %v, want PublicationError", err)
	}
	if publication.FinalPath != outPath || !publication.Durable {
		t.Fatalf("publication = %+v, want durable output %q", publication, outPath)
	}
	if summary.OutputPath != outPath || summary.ManifestPath != manifestPath || summary.BytesWritten != 6 {
		t.Fatalf("summary = %+v", summary)
	}
	if got, readErr := os.ReadFile(outPath); readErr != nil || string(got) != "bravo\n" {
		t.Fatalf("published output = %q, %v", got, readErr)
	}
	assertFileBytes(t, srcPath, source)
}

func TestExportByteRangeTextManifestFailureNeverPathDeletesPublishedOutput(t *testing.T) {
	dir := t.TempDir()
	srcPath := filepath.Join(dir, "source.txt")
	outPath := filepath.Join(dir, "output.txt")
	manifestPath := filepath.Join(dir, "missing-dir", "manifest.json")
	source := []byte("alpha\nbravo\ncharlie\n")
	if err := os.WriteFile(srcPath, source, 0o600); err != nil {
		t.Fatal(err)
	}

	doc, err := document.OpenFile(srcPath)
	if err != nil {
		t.Fatal(err)
	}
	defer doc.Close()

	_, err = ExportByteRangeText(context.Background(), doc, srcPath, outPath, 6, 12, "UTF-8", "UTF-8", Options{
		WriteManifest: true,
		ManifestPath:  manifestPath,
	})
	if err == nil {
		t.Fatal("expected manifest failure")
	}
	var publication *fileio.PublicationError
	if !errors.As(err, &publication) || publication.FinalPath != outPath {
		t.Fatalf("error = %v, want output PublicationError", err)
	}
	assertFileBytes(t, srcPath, source)
	if got, readErr := os.ReadFile(outPath); readErr != nil || string(got) != "bravo\n" {
		t.Fatalf("published output = %q, %v; want written range", got, readErr)
	}
}

func TestSplitBySizeManifestPublishFailurePreservesVerifiedParts(t *testing.T) {
	dir := t.TempDir()
	srcPath := filepath.Join(dir, "source.txt")
	basePath := filepath.Join(dir, "split.txt")
	manifestPath := filepath.Join(dir, "missing-dir", "manifest.json")
	source := []byte("abcdefghijkl")
	if err := os.WriteFile(srcPath, source, 0o600); err != nil {
		t.Fatal(err)
	}

	doc, err := document.OpenFile(srcPath)
	if err != nil {
		t.Fatal(err)
	}
	defer doc.Close()

	summary, err := SplitBySize(context.Background(), doc, srcPath, basePath, 5, SplitOptions{
		ComputeSHA256: true,
		ManifestPath:  manifestPath,
	})
	if err == nil {
		t.Fatal("expected manifest publish failure")
	}
	var incomplete *SplitIncompleteError
	if !errors.As(err, &incomplete) {
		t.Fatalf("error = %v, want SplitIncompleteError", err)
	}
	if summary.Complete || summary.Failure == "" || len(summary.Outputs) != 3 || len(incomplete.Outputs) != 3 {
		t.Fatalf("summary = %+v, error = %+v", summary, incomplete)
	}
	assertFileBytes(t, partPath(basePath, 1), []byte("abcde"))
	assertFileBytes(t, partPath(basePath, 2), []byte("fghij"))
	assertFileBytes(t, partPath(basePath, 3), []byte("kl"))
	assertFileBytes(t, srcPath, source)
}

func assertFileBytes(t *testing.T, path string, want []byte) {
	t.Helper()
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(want) {
		t.Fatalf("%s changed to %q, want %q", path, got, want)
	}
}

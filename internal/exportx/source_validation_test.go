package exportx

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/quarry/quarry-wails3/internal/document"
	"github.com/quarry/quarry-wails3/internal/fileio"
	"github.com/quarry/quarry-wails3/internal/sourceio"
)

func TestExportByteRangeAutomaticallyRejectsRestoredTimestampMutation(t *testing.T) {
	dir := t.TempDir()
	sourcePath := filepath.Join(dir, "source.bin")
	outputPath := filepath.Join(dir, "selection.bin")
	original := bytes.Repeat([]byte{'a'}, 2*1024*1024)
	changed := append([]byte(nil), original...)
	changed[len(changed)-1] = 'b'
	if err := os.WriteFile(sourcePath, original, 0o600); err != nil {
		t.Fatal(err)
	}
	openedInfo, err := os.Stat(sourcePath)
	if err != nil {
		t.Fatal(err)
	}
	doc, err := document.OpenFile(sourcePath)
	if err != nil {
		t.Fatal(err)
	}
	defer doc.Close()

	mutated := false
	_, err = ExportByteRange(context.Background(), doc, sourcePath, outputPath, 0, doc.Size(), Options{
		Progress: func(done, total int64) {
			if mutated || done != total {
				return
			}
			mutated = true
			if writeErr := os.WriteFile(sourcePath, changed, 0o600); writeErr != nil {
				t.Fatalf("mutate source: %v", writeErr)
			}
			if chtimesErr := os.Chtimes(sourcePath, openedInfo.ModTime(), openedInfo.ModTime()); chtimesErr != nil {
				t.Fatalf("restore source timestamp: %v", chtimesErr)
			}
		},
	})
	if !mutated {
		t.Fatal("test did not mutate the source before publication")
	}
	if !errors.Is(err, sourceio.ErrSourceChanged) {
		t.Fatalf("export error = %v, want sourceio.ErrSourceChanged", err)
	}
	if _, statErr := os.Lstat(outputPath); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("changed-source export published output: %v", statErr)
	}
	gotSource, readErr := os.ReadFile(sourcePath)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if !bytes.Equal(gotSource, changed) {
		t.Fatal("rejected export modified the source")
	}
}

func TestExportVisibleTextOperationValidationFailureDoesNotPublish(t *testing.T) {
	dir := t.TempDir()
	outputPath := filepath.Join(dir, "selection.txt")
	validationErr := errors.New("visible selection generation changed")
	validatorCalls := 0

	_, err := ExportVisibleTextWithOptions(context.Background(), outputPath, "selected text", "UTF-8", Options{
		ValidateSource: func(context.Context) error {
			validatorCalls++
			if _, statErr := os.Lstat(outputPath); !errors.Is(statErr, os.ErrNotExist) {
				t.Fatalf("final output visible before operation validation: %v", statErr)
			}
			return validationErr
		},
	})
	if !errors.Is(err, validationErr) {
		t.Fatalf("export error = %v, want operation validation error", err)
	}
	if validatorCalls != 1 {
		t.Fatalf("validator calls = %d, want 1", validatorCalls)
	}
	if _, statErr := os.Lstat(outputPath); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("rejected visible selection published final output: %v", statErr)
	}
	entries, readErr := os.ReadDir(dir)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if len(entries) != 0 {
		t.Fatalf("rejected visible selection left output artifacts: %v", entries)
	}
}

func TestExportByteRangesOperationValidationFailureDoesNotPublish(t *testing.T) {
	dir := t.TempDir()
	sourcePath := filepath.Join(dir, "source.sql")
	outputPath := filepath.Join(dir, "table.sql")
	source := []byte("CREATE TABLE t;\nINSERT INTO t VALUES (1);\n")
	if err := os.WriteFile(sourcePath, source, 0o600); err != nil {
		t.Fatal(err)
	}
	doc, err := document.OpenFile(sourcePath)
	if err != nil {
		t.Fatal(err)
	}
	defer doc.Close()

	validationErr := errors.New("SQL analysis generation changed")
	validatorCalls := 0
	_, err = ExportByteRanges(context.Background(), doc, sourcePath, outputPath, [][2]int64{{0, doc.Size()}}, Options{
		ValidateSource: func(context.Context) error {
			validatorCalls++
			if _, statErr := os.Lstat(outputPath); !errors.Is(statErr, os.ErrNotExist) {
				t.Fatalf("final output visible before operation validation: %v", statErr)
			}
			return validationErr
		},
	})
	if !errors.Is(err, validationErr) {
		t.Fatalf("export error = %v, want operation validation error", err)
	}
	if validatorCalls != 1 {
		t.Fatalf("validator calls = %d, want 1", validatorCalls)
	}
	if _, err := os.Lstat(outputPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("rejected export published final output: %v", err)
	}
	gotSource, err := os.ReadFile(sourcePath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(gotSource, source) {
		t.Fatalf("source changed to %q", gotSource)
	}
}

func TestExportByteRangesRevalidatesDocumentAfterOperationCallback(t *testing.T) {
	dir := t.TempDir()
	sourcePath := filepath.Join(dir, "source.sql")
	outputPath := filepath.Join(dir, "table.sql")
	original := []byte("INSERT INTO t VALUES (1);\n")
	changed := []byte("INSERT INTO t VALUES (9);\n")
	if len(original) != len(changed) {
		t.Fatal("fixture must preserve source size")
	}
	if err := os.WriteFile(sourcePath, original, 0o600); err != nil {
		t.Fatal(err)
	}
	openedInfo, err := os.Stat(sourcePath)
	if err != nil {
		t.Fatal(err)
	}
	doc, err := document.OpenFile(sourcePath)
	if err != nil {
		t.Fatal(err)
	}
	defer doc.Close()

	_, err = ExportByteRanges(context.Background(), doc, sourcePath, outputPath, [][2]int64{{0, doc.Size()}}, Options{
		ValidateSource: func(context.Context) error {
			if err := os.WriteFile(sourcePath, changed, 0o600); err != nil {
				return err
			}
			forced := openedInfo.ModTime().Add(2 * time.Second)
			return os.Chtimes(sourcePath, forced, forced)
		},
	})
	if !errors.Is(err, document.ErrSourceChanged) {
		t.Fatalf("export error = %v, want document.ErrSourceChanged", err)
	}
	if _, err := os.Lstat(outputPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("changed-source export published final output: %v", err)
	}
	gotSource, err := os.ReadFile(sourcePath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(gotSource, changed) {
		t.Fatalf("source changed again: got %q, want %q", gotSource, changed)
	}
}

func TestExportManifestRevalidatesDocumentAfterOperationCallback(t *testing.T) {
	dir := t.TempDir()
	sourcePath := filepath.Join(dir, "source.sql")
	outputPath := filepath.Join(dir, "table.sql")
	manifestPath := filepath.Join(dir, "table.manifest.json")
	original := []byte("INSERT INTO t VALUES (1);\n")
	changed := []byte("INSERT INTO t VALUES (9);\n")
	if len(original) != len(changed) {
		t.Fatal("fixture must preserve source size")
	}
	if err := os.WriteFile(sourcePath, original, 0o600); err != nil {
		t.Fatal(err)
	}
	openedInfo, err := os.Stat(sourcePath)
	if err != nil {
		t.Fatal(err)
	}
	doc, err := document.OpenFile(sourcePath)
	if err != nil {
		t.Fatal(err)
	}
	defer doc.Close()

	validatorCalls := 0
	summary, err := ExportByteRange(context.Background(), doc, sourcePath, outputPath, 0, doc.Size(), Options{
		ManifestPath: manifestPath,
		ValidateSource: func(context.Context) error {
			validatorCalls++
			if validatorCalls != 2 {
				return nil
			}
			if writeErr := os.WriteFile(sourcePath, changed, 0o600); writeErr != nil {
				return writeErr
			}
			forced := openedInfo.ModTime().Add(2 * time.Second)
			return os.Chtimes(sourcePath, forced, forced)
		},
	})
	if !errors.Is(err, document.ErrSourceChanged) {
		t.Fatalf("export error = %v, want document.ErrSourceChanged", err)
	}
	if validatorCalls != 2 {
		t.Fatalf("validator calls = %d, want output and manifest validation", validatorCalls)
	}
	var publication *fileio.PublicationError
	if !errors.As(err, &publication) || publication.FinalPath != outputPath || !publication.Durable {
		t.Fatalf("error = %T %v, want durable published-output warning for %q", err, err, outputPath)
	}
	if summary.OutputPath != outputPath || summary.ManifestPath != manifestPath || summary.BytesWritten != int64(len(original)) {
		t.Fatalf("summary = %+v", summary)
	}
	gotOutput, readErr := os.ReadFile(outputPath)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if !bytes.Equal(gotOutput, original) {
		t.Fatalf("published output = %q, want original generation %q", gotOutput, original)
	}
	if _, statErr := os.Lstat(manifestPath); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("stale operation published success manifest: %v", statErr)
	}
}

func TestSplitManifestRevalidatesSourceAfterLastPartProgress(t *testing.T) {
	dir := t.TempDir()
	sourcePath := filepath.Join(dir, "source.sql")
	basePath := filepath.Join(dir, "split.sql")
	original := []byte("abcdef")
	changed := []byte("uvwxyz")
	if err := os.WriteFile(sourcePath, original, 0o600); err != nil {
		t.Fatal(err)
	}
	openedInfo, err := os.Stat(sourcePath)
	if err != nil {
		t.Fatal(err)
	}
	doc, err := document.OpenFile(sourcePath)
	if err != nil {
		t.Fatal(err)
	}
	defer doc.Close()

	mutated := false
	summary, err := SplitBySize(context.Background(), doc, sourcePath, basePath, 3, SplitOptions{
		Progress: func(_ int64, _ int64, completedParts int) {
			if mutated || completedParts != 2 {
				return
			}
			mutated = true
			if writeErr := os.WriteFile(sourcePath, changed, 0o600); writeErr != nil {
				t.Fatalf("mutate source after final part: %v", writeErr)
			}
			forced := openedInfo.ModTime().Add(2 * time.Second)
			if chtimesErr := os.Chtimes(sourcePath, forced, forced); chtimesErr != nil {
				t.Fatalf("advance source generation after final part: %v", chtimesErr)
			}
		},
	})
	if !mutated {
		t.Fatal("test did not mutate the source after the final part")
	}
	if !errors.Is(err, document.ErrSourceChanged) {
		t.Fatalf("split error = %v, want document.ErrSourceChanged", err)
	}
	var incomplete *SplitIncompleteError
	if !errors.As(err, &incomplete) {
		t.Fatalf("split error = %T %v, want SplitIncompleteError", err, err)
	}
	if summary.Complete || len(summary.Outputs) != 2 || summary.Failure == "" {
		t.Fatalf("summary = %+v, want two preserved parts and no completion", summary)
	}
	first, readErr := os.ReadFile(summary.Outputs[0])
	if readErr != nil {
		t.Fatal(readErr)
	}
	second, readErr := os.ReadFile(summary.Outputs[1])
	if readErr != nil {
		t.Fatal(readErr)
	}
	if string(first) != "abc" || string(second) != "def" {
		t.Fatalf("preserved parts = %q and %q, want original source generation", first, second)
	}
	if _, statErr := os.Lstat(summary.ManifestPath); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("changed-source split published success manifest: %v", statErr)
	}
}

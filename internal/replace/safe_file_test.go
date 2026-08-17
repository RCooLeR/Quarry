package replace

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/quarry/quarry-wails3/internal/document"
	"github.com/quarry/quarry-wails3/internal/fileio"
	"github.com/quarry/quarry-wails3/internal/sourceio"
)

func TestReplaceBatchPlainFileAtomicPublishesPrivateCopy(t *testing.T) {
	dir := t.TempDir()
	sourcePath := filepath.Join(dir, "source.txt")
	outputPath := filepath.Join(dir, " output.txt")
	source := []byte("hello world\n")
	if err := os.WriteFile(sourcePath, source, 0o600); err != nil {
		t.Fatal(err)
	}

	summary, err := replaceBatchPlainFileAtomic(context.Background(), sourcePath, outputPath,
		[]BatchRule{{Name: "greeting", Find: []byte("hello"), Replace: []byte("goodbye")}},
		FileOptions{}, BatchOptions{ChunkSize: 4})
	if err != nil {
		t.Fatal(err)
	}
	if summary.OutputPath != outputPath || summary.TempPath != "" || summary.ManifestPath != "" || summary.Swapped || !summary.Complete || !summary.Published {
		t.Fatalf("summary = %#v", summary)
	}
	if summary.Matches != 1 || summary.Conflicts != 0 || summary.BytesWritten != int64(len("goodbye world\n")) {
		t.Fatalf("summary counts = %#v", summary)
	}
	assertReplaceFileBytes(t, sourcePath, source)
	assertReplaceFileBytes(t, outputPath, []byte("goodbye world\n"))
	info, err := os.Stat(outputPath)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); runtime.GOOS != "windows" && got != 0o600 {
		t.Fatalf("mode = %o, want 600", got)
	}
	assertNoReplaceScratch(t, dir)
}

func TestReplaceBatchRegexpFileAtomicPublishesCompleteCopy(t *testing.T) {
	dir := t.TempDir()
	sourcePath := filepath.Join(dir, "source.sql")
	outputPath := filepath.Join(dir, "output.sql")
	if err := os.WriteFile(sourcePath, []byte("id=12 id=34\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	summary, err := replaceBatchRegexpFileAtomic(context.Background(), sourcePath, outputPath,
		[]BatchRule{{Name: "number", Find: []byte(`[0-9]{2}`), Replace: []byte("0")}},
		FileOptions{}, RegexOptions{ChunkSize: 8, MaxMatchWindow: 16})
	if err != nil {
		t.Fatal(err)
	}
	if summary.Matches != 2 || summary.BytesWritten != int64(len("id=0 id=0\n")) {
		t.Fatalf("summary = %#v", summary)
	}
	assertReplaceFileBytes(t, outputPath, []byte("id=0 id=0\n"))
	assertNoReplaceScratch(t, dir)
}

func TestAtomicBatchReplaceRejectsSourceAliasesAndExistingDestination(t *testing.T) {
	t.Run("same path", func(t *testing.T) {
		dir := t.TempDir()
		sourcePath := filepath.Join(dir, "source.txt")
		if err := os.WriteFile(sourcePath, []byte("source"), 0o600); err != nil {
			t.Fatal(err)
		}
		_, err := replaceBatchPlainFileAtomic(context.Background(), sourcePath, sourcePath,
			[]BatchRule{{Name: "x", Find: []byte("x"), Replace: []byte("y")}}, FileOptions{}, BatchOptions{})
		if !errors.Is(err, fileio.ErrSourceAlias) {
			t.Fatalf("error = %v, want ErrSourceAlias", err)
		}
		assertReplaceFileBytes(t, sourcePath, []byte("source"))
	})

	t.Run("hard link", func(t *testing.T) {
		dir := t.TempDir()
		sourcePath := filepath.Join(dir, "source.txt")
		outputPath := filepath.Join(dir, "alias.txt")
		if err := os.WriteFile(sourcePath, []byte("source"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Link(sourcePath, outputPath); err != nil {
			t.Skipf("hard links unavailable: %v", err)
		}
		_, err := replaceBatchPlainFileAtomic(context.Background(), sourcePath, outputPath,
			[]BatchRule{{Name: "x", Find: []byte("x"), Replace: []byte("y")}}, FileOptions{}, BatchOptions{})
		if !errors.Is(err, fileio.ErrSourceAlias) {
			t.Fatalf("error = %v, want ErrSourceAlias", err)
		}
		assertReplaceFileBytes(t, sourcePath, []byte("source"))
	})

	t.Run("existing destination", func(t *testing.T) {
		dir := t.TempDir()
		sourcePath := filepath.Join(dir, "source.txt")
		outputPath := filepath.Join(dir, "output.txt")
		if err := os.WriteFile(sourcePath, []byte("source"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(outputPath, []byte("sentinel"), 0o600); err != nil {
			t.Fatal(err)
		}
		_, err := replaceBatchPlainFileAtomic(context.Background(), sourcePath, outputPath,
			[]BatchRule{{Name: "x", Find: []byte("x"), Replace: []byte("y")}}, FileOptions{}, BatchOptions{})
		if !errors.Is(err, fileio.ErrExists) {
			t.Fatalf("error = %v, want ErrExists", err)
		}
		assertReplaceFileBytes(t, outputPath, []byte("sentinel"))
		assertReplaceFileBytes(t, sourcePath, []byte("source"))
	})
}

func TestAtomicBatchReplaceDestinationRacePreservesCompetitor(t *testing.T) {
	dir := t.TempDir()
	sourcePath := filepath.Join(dir, "source.txt")
	outputPath := filepath.Join(dir, "output.txt")
	source := []byte(strings.Repeat("abc", 100_000))
	if err := os.WriteFile(sourcePath, source, 0o600); err != nil {
		t.Fatal(err)
	}
	created := false
	summary, err := replaceBatchPlainFileAtomic(context.Background(), sourcePath, outputPath,
		[]BatchRule{{Name: "a", Find: []byte("a"), Replace: []byte("z")}}, FileOptions{}, BatchOptions{
			ChunkSize: 32 * 1024,
			Progress: func(p Progress) {
				if created || p.BytesProcessed == 0 {
					return
				}
				created = true
				if writeErr := os.WriteFile(outputPath, []byte("competitor"), 0o600); writeErr != nil {
					t.Fatalf("create competitor: %v", writeErr)
				}
			},
		})
	if !errors.Is(err, fileio.ErrExists) {
		t.Fatalf("error = %v, want ErrExists", err)
	}
	if !summary.Complete || summary.Published || summary.TempPath != "" {
		t.Fatalf("raced publication summary = %#v", summary)
	}
	assertReplaceFileBytes(t, outputPath, []byte("competitor"))
	assertReplaceFileBytes(t, sourcePath, source)
	assertNoReplaceScratch(t, dir)
}

func TestAtomicBatchReplaceCancellationAndSourceChangeDoNotPublish(t *testing.T) {
	t.Run("canceled", func(t *testing.T) {
		dir := t.TempDir()
		sourcePath := filepath.Join(dir, "source.txt")
		outputPath := filepath.Join(dir, "output.txt")
		source := []byte(strings.Repeat("abc", 500_000))
		if err := os.WriteFile(sourcePath, source, 0o600); err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithCancel(context.Background())
		_, err := replaceBatchPlainFileAtomic(ctx, sourcePath, outputPath,
			[]BatchRule{{Name: "a", Find: []byte("a"), Replace: []byte("z")}}, FileOptions{}, BatchOptions{
				ChunkSize: 32 * 1024,
				Progress:  func(p Progress) { cancel() },
			})
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("error = %v, want context.Canceled", err)
		}
		if _, statErr := os.Lstat(outputPath); !errors.Is(statErr, os.ErrNotExist) {
			t.Fatalf("canceled output became visible: %v", statErr)
		}
		assertReplaceFileBytes(t, sourcePath, source)
		assertNoReplaceScratch(t, dir)
	})

	t.Run("source truncated", func(t *testing.T) {
		dir := t.TempDir()
		sourcePath := filepath.Join(dir, "source.txt")
		outputPath := filepath.Join(dir, "output.txt")
		source := []byte(strings.Repeat("abc", 500_000))
		if err := os.WriteFile(sourcePath, source, 0o600); err != nil {
			t.Fatal(err)
		}
		changed := false
		_, err := replaceBatchPlainFileAtomic(context.Background(), sourcePath, outputPath,
			[]BatchRule{{Name: "a", Find: []byte("a"), Replace: []byte("z")}}, FileOptions{}, BatchOptions{
				ChunkSize: 32 * 1024,
				Progress: func(p Progress) {
					if changed {
						return
					}
					changed = true
					if truncateErr := os.Truncate(sourcePath, 0); truncateErr != nil {
						t.Fatalf("truncate source: %v", truncateErr)
					}
				},
			})
		if !errors.Is(err, ErrSourceModifiedDuringOperation) {
			t.Fatalf("error = %v, want ErrSourceModifiedDuringOperation", err)
		}
		if _, statErr := os.Lstat(outputPath); !errors.Is(statErr, os.ErrNotExist) {
			t.Fatalf("changed-source output became visible: %v", statErr)
		}
		assertNoReplaceScratch(t, dir)
	})
}

func TestAtomicBatchReplaceRejectsSourcePathReplacementDuringStreaming(t *testing.T) {
	dir := t.TempDir()
	sourcePath := filepath.Join(dir, "source.sql")
	heldPath := filepath.Join(dir, "streamed-generation.sql")
	outputPath := filepath.Join(dir, "output.sql")
	source := []byte(strings.Repeat("INSERT INTO t VALUES (1);\n", 100_000))
	substitute := []byte("INSERT INTO t VALUES (99);\n")
	if err := os.WriteFile(sourcePath, source, 0o600); err != nil {
		t.Fatal(err)
	}
	originalInfo, err := os.Stat(sourcePath)
	if err != nil {
		t.Fatal(err)
	}
	doc, err := document.OpenFile(sourcePath)
	if err != nil {
		t.Fatal(err)
	}
	defer doc.Close()
	expected, err := sourceio.ExpectDocument(doc)
	if err != nil {
		t.Fatal(err)
	}
	mutated := false
	pathReplaced := false
	var mutationErr error
	_, err = replaceBatchPlainFileAtomic(context.Background(), sourcePath, outputPath,
		[]BatchRule{{Name: "value", Find: []byte("1"), Replace: []byte("2")}},
		FileOptions{ExpectedSource: expected}, BatchOptions{
			ChunkSize: 32 * 1024,
			Progress: func(Progress) {
				if mutated {
					return
				}
				mutated = true
				if renameErr := os.Rename(sourcePath, heldPath); renameErr != nil {
					changed := originalInfo.ModTime().Add(2 * time.Second)
					mutationErr = os.Chtimes(sourcePath, changed, changed)
					return
				}
				pathReplaced = true
				mutationErr = os.WriteFile(sourcePath, substitute, 0o600)
			},
		})
	if mutationErr != nil {
		t.Fatalf("replace source path during operation: %v", mutationErr)
	}
	if !mutated {
		t.Fatal("replace did not reach the streaming mutation point")
	}
	if !errors.Is(err, sourceio.ErrSourceChanged) {
		t.Fatalf("error = %v, want source-generation rejection", err)
	}
	if _, statErr := os.Stat(outputPath); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("failed replacement published output: %v", statErr)
	}
	if pathReplaced {
		assertReplaceFileBytes(t, heldPath, source)
		assertReplaceFileBytes(t, sourcePath, substitute)
	} else {
		assertReplaceFileBytes(t, sourcePath, source)
	}
	assertNoReplaceScratch(t, dir)
}

func TestAtomicBatchReplaceSwapFailsBeforeFilesystemAccess(t *testing.T) {
	dir := t.TempDir()
	sourcePath := filepath.Join(dir, "missing-source.txt")
	outputPath := filepath.Join(dir, "missing", "output.txt")
	_, err := replaceBatchPlainFileAtomic(context.Background(), sourcePath, outputPath,
		[]BatchRule{{Name: "x", Find: []byte("x"), Replace: []byte("y")}}, FileOptions{SwapOriginal: true}, BatchOptions{})
	if !errors.Is(err, ErrSwapOriginalDisabled) {
		t.Fatalf("error = %v, want ErrSwapOriginalDisabled", err)
	}
	if _, statErr := os.Lstat(filepath.Dir(outputPath)); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("disabled swap touched filesystem: %v", statErr)
	}
}

func TestAtomicBatchCleanupPreservesRetainedTempEvidenceOnFailure(t *testing.T) {
	sentinel := errors.New("temporary output is still open")
	summary := FileSummary{TempPath: "retained.quarry-temp"}
	if err := cleanupAtomicBatchOutput(&summary, func() error { return sentinel }); !errors.Is(err, sentinel) {
		t.Fatalf("cleanup error = %v, want sentinel", err)
	}
	if summary.TempPath != "retained.quarry-temp" {
		t.Fatalf("failed cleanup lost temp evidence: %#v", summary)
	}
	if err := cleanupAtomicBatchOutput(&summary, func() error { return nil }); err != nil {
		t.Fatal(err)
	}
	if summary.TempPath != "" {
		t.Fatalf("successful cleanup retained stale temp evidence: %#v", summary)
	}
}

func assertNoReplaceScratch(t *testing.T, dir string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.Contains(entry.Name(), ".quarry-") || strings.HasSuffix(entry.Name(), ".quarry.tmp") || strings.HasSuffix(entry.Name(), ".quarry.manifest.json") {
			t.Fatalf("unexpected replace scratch artifact %q", entry.Name())
		}
	}
}

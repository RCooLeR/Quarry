package manualedit

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/quarry/quarry-wails3/internal/document"
	"github.com/quarry/quarry-wails3/internal/fileio"
)

func TestApplyFileEditPublishesExactCopyWithoutManifestOrNamedScratch(t *testing.T) {
	dir := t.TempDir()
	sourcePath := filepath.Join(dir, "source.txt")
	// A leading space is intentional: selected output paths must be used exactly,
	// not trimmed or reconstructed from a normalized display value.
	outputPath := filepath.Join(dir, " edited copy.txt")
	writeManualTestFile(t, sourcePath, []byte("alpha bravo charlie"))

	summary, err := ApplyFileEdit(context.Background(), sourcePath, outputPath, Edit{
		Start: 6,
		End:   11,
		Text:  []byte("delta"),
	}, FileOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if summary.OutputPath != outputPath {
		t.Fatalf("OutputPath = %q, want exact %q", summary.OutputPath, outputPath)
	}
	if summary.TempPath != "" || summary.ManifestPath != "" {
		t.Fatalf("copy-only save exposed temp/manifest paths: %+v", summary)
	}
	if summary.BackupPath != "" || summary.Swapped {
		t.Fatalf("copy-only save reported source replacement: %+v", summary)
	}
	if !summary.Complete || !summary.Published {
		t.Fatalf("successful summary is not complete/published: %+v", summary)
	}
	want := "alpha delta charlie"
	if summary.BytesWritten != int64(len(want)) {
		t.Fatalf("BytesWritten = %d, want %d", summary.BytesWritten, len(want))
	}
	assertManualFileContent(t, sourcePath, "alpha bravo charlie")
	assertManualFileContent(t, outputPath, want)
	assertManualDirEntries(t, dir, filepath.Base(sourcePath), filepath.Base(outputPath))

	if runtime.GOOS != "windows" {
		info, err := os.Stat(outputPath)
		if err != nil {
			t.Fatal(err)
		}
		if got := info.Mode().Perm(); got != 0o600 {
			t.Fatalf("output mode = %o, want 0600", got)
		}
	}
}

func TestWriteSessionToFilePublishesExactStagedEdits(t *testing.T) {
	dir := t.TempDir()
	sourcePath := filepath.Join(dir, "source.txt")
	outputPath := filepath.Join(dir, "session.txt")
	const source = "alpha bravo charlie"
	writeManualTestFile(t, sourcePath, []byte(source))

	session := newBoundManualTestSession(t, sourcePath, 1)
	if err := session.ApplyVerifiedEdit(Edit{Start: 6, End: 11, Text: []byte("delta")}, []byte("bravo")); err != nil {
		t.Fatal(err)
	}
	if err := session.ApplyVerifiedEdit(Edit{Start: 0, End: 0, Text: []byte(">> ")}, nil); err != nil {
		t.Fatal(err)
	}

	summary, err := WriteSessionToFile(context.Background(), sourcePath, outputPath, session, FileOptions{SourceGeneration: 1})
	if err != nil {
		t.Fatal(err)
	}
	want := ">> alpha delta charlie"
	if summary.OutputPath != outputPath || summary.BytesWritten != int64(len(want)) || !summary.Complete || !summary.Published {
		t.Fatalf("summary = %+v, want exact completed publication", summary)
	}
	assertManualFileContent(t, sourcePath, source)
	assertManualFileContent(t, outputPath, want)
}

func TestSwapOriginalFailsBeforeAnyFilesystemAccess(t *testing.T) {
	dir := t.TempDir()
	sourcePath := filepath.Join(dir, "source.txt")
	outputPath := filepath.Join(dir, "output.txt")
	backupPath := filepath.Join(dir, "backup.txt")
	writeManualTestFile(t, sourcePath, []byte("source sentinel"))
	writeManualTestFile(t, outputPath, []byte("output sentinel"))
	writeManualTestFile(t, backupPath, []byte("backup sentinel"))

	originalOpenSource := openSourceFile
	originalOpenAtomic := openAtomicOutput
	openSourceCalls := 0
	openAtomicCalls := 0
	openSourceFile = func(string) (*os.File, error) {
		openSourceCalls++
		return nil, errors.New("unexpected source open")
	}
	openAtomicOutput = func(string, []string, os.FileMode) (atomicOutput, error) {
		openAtomicCalls++
		return nil, errors.New("unexpected output open")
	}
	t.Cleanup(func() {
		openSourceFile = originalOpenSource
		openAtomicOutput = originalOpenAtomic
	})

	applySummary, err := ApplyFileEdit(context.Background(), sourcePath, outputPath, Edit{
		Start: 0,
		End:   1,
		Text:  []byte("x"),
	}, FileOptions{SwapOriginal: true, BackupPath: backupPath})
	if !errors.Is(err, ErrSwapOriginalDisabled) {
		t.Fatalf("ApplyFileEdit error = %v, want ErrSwapOriginalDisabled", err)
	}
	if applySummary.OutputPath != outputPath || applySummary.Published || applySummary.Complete {
		t.Fatalf("ApplyFileEdit summary = %+v", applySummary)
	}

	session := NewSession(int64(len("source sentinel")), DefaultMaxInsertedBytes)
	if err := session.ApplyEdit(Edit{Start: 0, End: 1, Text: []byte("x")}); err != nil {
		t.Fatal(err)
	}
	sessionSummary, err := WriteSessionToFile(context.Background(), sourcePath, outputPath, session, FileOptions{
		SwapOriginal: true,
		BackupPath:   backupPath,
	})
	if !errors.Is(err, ErrSwapOriginalDisabled) {
		t.Fatalf("WriteSessionToFile error = %v, want ErrSwapOriginalDisabled", err)
	}
	if sessionSummary.OutputPath != outputPath || sessionSummary.Published || sessionSummary.Complete {
		t.Fatalf("WriteSessionToFile summary = %+v", sessionSummary)
	}
	if openSourceCalls != 0 || openAtomicCalls != 0 {
		t.Fatalf("disabled swap touched filesystem seams: source opens=%d output opens=%d", openSourceCalls, openAtomicCalls)
	}
	assertManualFileContent(t, sourcePath, "source sentinel")
	assertManualFileContent(t, outputPath, "output sentinel")
	assertManualFileContent(t, backupPath, "backup sentinel")
}

func TestApplyFileEditCancellationNeverPublishesOrPreservesPartial(t *testing.T) {
	for _, deletePartial := range []bool{false, true} {
		t.Run("delete-partial-"+map[bool]string{false: "false", true: "true"}[deletePartial], func(t *testing.T) {
			dir := t.TempDir()
			sourcePath := filepath.Join(dir, "source.txt")
			outputPath := filepath.Join(dir, "output.txt")
			writeManualTestFile(t, sourcePath, []byte(strings.Repeat("abcdef", 4096)))

			ctx, cancel := context.WithCancel(context.Background())
			summary, err := ApplyFileEdit(ctx, sourcePath, outputPath, Edit{
				Start: 0,
				End:   0,
				Text:  []byte("prefix\n"),
			}, FileOptions{
				DeletePartialOnCancel: deletePartial,
				Progress: func(Progress) {
					cancel()
				},
			})
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("error = %v, want context.Canceled", err)
			}
			if summary.Complete || summary.Published || summary.TempPath != "" || summary.ManifestPath != "" {
				t.Fatalf("canceled summary = %+v", summary)
			}
			if _, err := os.Lstat(outputPath); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("final output exists after cancellation: %v", err)
			}
			assertManualDirEntries(t, dir, filepath.Base(sourcePath))
		})
	}
}

func TestApplyFileEditCancellationAfterExactStreamStillPreventsPublication(t *testing.T) {
	dir := t.TempDir()
	sourcePath := filepath.Join(dir, "source.txt")
	outputPath := filepath.Join(dir, "output.txt")
	const source = "replace all of this"
	writeManualTestFile(t, sourcePath, []byte(source))

	ctx, cancel := context.WithCancel(context.Background())
	summary, err := ApplyFileEdit(ctx, sourcePath, outputPath, Edit{
		Start: 0,
		End:   int64(len(source)),
		Text:  []byte("complete bytes, canceled before commit"),
	}, FileOptions{Progress: func(Progress) { cancel() }})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want context.Canceled", err)
	}
	if summary.Complete || summary.Published {
		t.Fatalf("canceled pre-publication summary = %+v", summary)
	}
	if _, err := os.Lstat(outputPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("final output exists after cancellation: %v", err)
	}
	assertManualFileContent(t, sourcePath, source)
	assertManualDirEntries(t, dir, filepath.Base(sourcePath))
}

func TestManualEditRefusesExistingDestinationWithoutChangingIt(t *testing.T) {
	dir := t.TempDir()
	sourcePath := filepath.Join(dir, "source.txt")
	outputPath := filepath.Join(dir, "output.txt")
	writeManualTestFile(t, sourcePath, []byte("source sentinel"))
	writeManualTestFile(t, outputPath, []byte("destination sentinel"))

	summary, err := ApplyFileEdit(context.Background(), sourcePath, outputPath, Edit{
		Start: 0,
		End:   6,
		Text:  []byte("edited"),
	}, FileOptions{})
	if !errors.Is(err, fileio.ErrExists) {
		t.Fatalf("error = %v, want fileio.ErrExists", err)
	}
	if summary.Complete || summary.Published || summary.BytesWritten != 0 {
		t.Fatalf("existing-destination summary = %+v", summary)
	}
	assertManualFileContent(t, sourcePath, "source sentinel")
	assertManualFileContent(t, outputPath, "destination sentinel")
	assertManualDirEntries(t, dir, filepath.Base(sourcePath), filepath.Base(outputPath))
}

func TestManualEditRefusesSourceAliases(t *testing.T) {
	dir := t.TempDir()
	sourcePath := filepath.Join(dir, "source.txt")
	writeManualTestFile(t, sourcePath, []byte("source sentinel"))

	tests := []struct {
		name   string
		output func(t *testing.T) string
	}{
		{name: "same path", output: func(*testing.T) string { return sourcePath }},
		{name: "hard link", output: func(t *testing.T) string {
			path := filepath.Join(dir, "source-hard-link.txt")
			if err := os.Link(sourcePath, path); err != nil {
				t.Skipf("hard links unavailable: %v", err)
			}
			return path
		}},
		{name: "symbolic link", output: func(t *testing.T) string {
			path := filepath.Join(dir, "source-symbolic-link.txt")
			if err := os.Symlink(sourcePath, path); err != nil {
				t.Skipf("symbolic links unavailable: %v", err)
			}
			return path
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			outputPath := tt.output(t)
			summary, err := ApplyFileEdit(context.Background(), sourcePath, outputPath, Edit{
				Start: 0,
				End:   6,
				Text:  []byte("edited"),
			}, FileOptions{})
			if !errors.Is(err, fileio.ErrSourceAlias) {
				t.Fatalf("error = %v, want fileio.ErrSourceAlias", err)
			}
			if summary.Complete || summary.Published || summary.BytesWritten != 0 {
				t.Fatalf("alias summary = %+v", summary)
			}
			assertManualFileContent(t, sourcePath, "source sentinel")
		})
	}
}

func TestManualEditDestinationRaceCannotClobberCompetitor(t *testing.T) {
	dir := t.TempDir()
	sourcePath := filepath.Join(dir, "source.txt")
	outputPath := filepath.Join(dir, "output.txt")
	const source = "source sentinel"
	const competitor = "competitor owns this pathname"
	writeManualTestFile(t, sourcePath, []byte(source))

	created := false
	var createErr error
	summary, err := ApplyFileEdit(context.Background(), sourcePath, outputPath, Edit{
		Start: 0,
		End:   int64(len(source)),
		Text:  []byte("edited copy"),
	}, FileOptions{Progress: func(Progress) {
		if !created {
			created = true
			createErr = os.WriteFile(outputPath, []byte(competitor), 0o600)
		}
	}})
	if createErr != nil {
		t.Fatalf("create raced destination: %v", createErr)
	}
	if !errors.Is(err, fileio.ErrExists) {
		t.Fatalf("error = %v, want raced fileio.ErrExists", err)
	}
	if summary.Complete || summary.Published {
		t.Fatalf("raced-destination summary = %+v", summary)
	}
	assertManualFileContent(t, sourcePath, source)
	assertManualFileContent(t, outputPath, competitor)
	assertManualDirEntries(t, dir, filepath.Base(sourcePath), filepath.Base(outputPath))
}

func TestManualEditNeverDeletesLegacyOrUnownedScratchPaths(t *testing.T) {
	dir := t.TempDir()
	sourcePath := filepath.Join(dir, "source.txt")
	outputPath := filepath.Join(dir, "output.txt")
	legacyTemp := outputPath + ".quarry.tmp"
	legacyManifest := outputPath + ".quarry.manifest.json"
	writeManualTestFile(t, sourcePath, []byte("alpha bravo"))
	writeManualTestFile(t, legacyTemp, []byte("unowned temp sentinel"))
	writeManualTestFile(t, legacyManifest, []byte("unowned manifest sentinel"))

	if _, err := ApplyFileEdit(context.Background(), sourcePath, outputPath, Edit{
		Start: 6,
		End:   11,
		Text:  []byte("delta"),
	}, FileOptions{}); err != nil {
		t.Fatal(err)
	}
	assertManualFileContent(t, outputPath, "alpha delta")
	assertManualFileContent(t, legacyTemp, "unowned temp sentinel")
	assertManualFileContent(t, legacyManifest, "unowned manifest sentinel")
}

func TestWriteSessionToFileRejectsChangedSourceSizeBeforeOutputAllocation(t *testing.T) {
	dir := t.TempDir()
	sourcePath := filepath.Join(dir, "source.txt")
	outputPath := filepath.Join(dir, "output.txt")
	const source = "alpha bravo charlie"
	writeManualTestFile(t, sourcePath, []byte(source))

	session := newBoundManualTestSession(t, sourcePath, 1)
	if err := session.ApplyVerifiedEdit(Edit{Start: 0, End: 0, Text: []byte(">> ")}, nil); err != nil {
		t.Fatal(err)
	}
	writeManualTestFile(t, sourcePath, []byte("short"))

	summary, err := WriteSessionToFile(context.Background(), sourcePath, outputPath, session, FileOptions{SourceGeneration: 1})
	if !errors.Is(err, ErrSourceModifiedDuringOperation) {
		t.Fatalf("error = %v, want ErrSourceModifiedDuringOperation", err)
	}
	if summary.BytesWritten != 0 || summary.Complete || summary.Published {
		t.Fatalf("changed-source summary = %+v", summary)
	}
	if _, err := os.Lstat(outputPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("output exists after stale-session rejection: %v", err)
	}
}

func TestApplyFileEditRejectsInvalidEditBeforeOutputAllocation(t *testing.T) {
	dir := t.TempDir()
	sourcePath := filepath.Join(dir, "source.txt")
	outputPath := filepath.Join(dir, "output.txt")
	writeManualTestFile(t, sourcePath, []byte("hello"))

	summary, err := ApplyFileEdit(context.Background(), sourcePath, outputPath, Edit{
		Start: 0,
		End:   0,
		Text:  []byte("toolong"),
	}, FileOptions{MaxInsertedBytes: 4})
	if !errors.Is(err, ErrInsertedTextTooLarge) {
		t.Fatalf("error = %v, want ErrInsertedTextTooLarge", err)
	}
	if summary.BytesWritten != 0 || summary.Complete || summary.Published {
		t.Fatalf("invalid-edit summary = %+v", summary)
	}
	if _, err := os.Lstat(outputPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("output exists after invalid edit: %v", err)
	}
}

func TestApplyFileEditValidatesEditBeforeFingerprintPass(t *testing.T) {
	dir := t.TempDir()
	sourcePath := filepath.Join(dir, "source.txt")
	outputPath := filepath.Join(dir, "output.txt")
	writeManualTestFile(t, sourcePath, []byte("hello"))

	ctx, cancel := context.WithCancel(context.Background())
	originalOpenSource := openSourceFile
	openSourceFile = func(path string) (*os.File, error) {
		file, err := originalOpenSource(path)
		cancel()
		return file, err
	}
	t.Cleanup(func() { openSourceFile = originalOpenSource })

	_, err := ApplyFileEdit(ctx, sourcePath, outputPath, Edit{Text: []byte("toolong")}, FileOptions{MaxInsertedBytes: 4})
	if !errors.Is(err, ErrInsertedTextTooLarge) {
		t.Fatalf("error = %v, want edit validation before canceled fingerprint", err)
	}
	assertPathDoesNotExist(t, outputPath)
}

func TestApplyFileEditPreCanceledContextIsNotSourceDrift(t *testing.T) {
	dir := t.TempDir()
	sourcePath := filepath.Join(dir, "source.txt")
	outputPath := filepath.Join(dir, "output.txt")
	writeManualTestFile(t, sourcePath, []byte("hello"))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := ApplyFileEdit(ctx, sourcePath, outputPath, Edit{Start: 0, End: 1, Text: []byte("H")}, FileOptions{})
	if !errors.Is(err, context.Canceled) || errors.Is(err, ErrSourceModifiedDuringOperation) {
		t.Fatalf("error = %v, want only context cancellation", err)
	}
	assertPathDoesNotExist(t, outputPath)
}

func writeManualTestFile(t *testing.T, path string, data []byte) {
	t.Helper()
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
}

func newBoundManualTestSession(t *testing.T, path string, generation uint64) *Session {
	t.Helper()
	doc, err := document.OpenFile(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = doc.Close() })
	session, err := NewSourceBoundSession(doc, path, generation, DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	return session
}

func assertManualFileContent(t *testing.T, path string, want string) {
	t.Helper()
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != want {
		t.Fatalf("%s = %q, want %q", path, string(got), want)
	}
}

func assertManualDirEntries(t *testing.T, dir string, want ...string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	got := make(map[string]struct{}, len(entries))
	for _, entry := range entries {
		got[entry.Name()] = struct{}{}
	}
	if len(got) != len(want) {
		t.Fatalf("directory entries = %v, want %v", mapKeys(got), want)
	}
	for _, name := range want {
		if _, ok := got[name]; !ok {
			t.Fatalf("directory entries = %v, missing %q", mapKeys(got), name)
		}
	}
}

func mapKeys(values map[string]struct{}) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	return keys
}

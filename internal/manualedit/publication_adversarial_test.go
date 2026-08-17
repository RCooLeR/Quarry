package manualedit

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/quarry/quarry-wails3/internal/fileio"
)

func TestManualEditPropagatesShortWriteAndCleansOwnedOutput(t *testing.T) {
	dir := t.TempDir()
	sourcePath := filepath.Join(dir, "source.txt")
	outputPath := filepath.Join(dir, "output.txt")
	const source = "source bytes"
	const replacement = "replacement bytes"
	writeManualTestFile(t, sourcePath, []byte(source))

	fake := &scriptedAtomicOutput{
		writeFn: func(p []byte) (int, error) {
			return len(p) - 1, nil
		},
	}
	installManualAtomicOutput(t, func(path string, sources []string, mode os.FileMode) (atomicOutput, error) {
		if path != outputPath || len(sources) != 1 || sources[0] != sourcePath || mode.Perm() != 0o600 {
			t.Fatalf("atomic open = path %q sources %v mode %o", path, sources, mode.Perm())
		}
		return fake, nil
	})

	summary, err := ApplyFileEdit(context.Background(), sourcePath, outputPath, Edit{
		Start: 0,
		End:   int64(len(source)),
		Text:  []byte(replacement),
	}, FileOptions{})
	if !errors.Is(err, io.ErrShortWrite) {
		t.Fatalf("error = %v, want io.ErrShortWrite", err)
	}
	if summary.BytesWritten != int64(len(replacement)-1) || summary.Complete || summary.Published {
		t.Fatalf("short-write summary = %+v", summary)
	}
	if fake.commitCalls != 0 || fake.cleanupCalls != 1 {
		t.Fatalf("commit calls=%d cleanup calls=%d, want 0/1", fake.commitCalls, fake.cleanupCalls)
	}
	assertManualFileContent(t, sourcePath, source)
	if _, err := os.Lstat(outputPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("output exists after short write: %v", err)
	}
}

func TestManualEditPropagatesAtomicSyncCloseAndPublishFailures(t *testing.T) {
	tests := []struct {
		name string
		err  error
	}{
		{name: "sync", err: errors.New("forced output sync failure")},
		{name: "close", err: errors.New("forced output close failure")},
		{name: "publish", err: errors.New("forced atomic publish failure")},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			sourcePath := filepath.Join(dir, "source.txt")
			outputPath := filepath.Join(dir, "output.txt")
			const source = "source bytes"
			writeManualTestFile(t, sourcePath, []byte(source))

			fake := &scriptedAtomicOutput{commitErr: tt.err}
			installManualAtomicOutput(t, func(string, []string, os.FileMode) (atomicOutput, error) {
				return fake, nil
			})

			summary, err := ApplyFileEdit(context.Background(), sourcePath, outputPath, Edit{
				Start: 0,
				End:   int64(len(source)),
				Text:  []byte("edited bytes"),
			}, FileOptions{})
			if !errors.Is(err, tt.err) {
				t.Fatalf("error = %v, want %v", err, tt.err)
			}
			if summary.Complete || summary.Published || summary.BytesWritten != int64(len("edited bytes")) {
				t.Fatalf("commit-failure summary = %+v", summary)
			}
			if fake.commitCalls != 1 || fake.cleanupCalls != 1 {
				t.Fatalf("commit calls=%d cleanup calls=%d, want 1/1", fake.commitCalls, fake.cleanupCalls)
			}
			assertManualFileContent(t, sourcePath, source)
			if _, err := os.Lstat(outputPath); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("output exists after commit failure: %v", err)
			}
		})
	}
}

func TestManualEditJoinsWriteAndCleanupFailures(t *testing.T) {
	dir := t.TempDir()
	sourcePath := filepath.Join(dir, "source.txt")
	outputPath := filepath.Join(dir, "output.txt")
	writeManualTestFile(t, sourcePath, []byte("source bytes"))
	writeErr := errors.New("forced write failure")
	cleanupErr := errors.New("forced owned-output cleanup failure")
	fake := &scriptedAtomicOutput{
		writeFn:    func([]byte) (int, error) { return 0, writeErr },
		cleanupErr: cleanupErr,
	}
	installManualAtomicOutput(t, func(string, []string, os.FileMode) (atomicOutput, error) {
		return fake, nil
	})

	_, err := ApplyFileEdit(context.Background(), sourcePath, outputPath, Edit{
		Start: 0,
		End:   int64(len("source bytes")),
		Text:  []byte("edited bytes"),
	}, FileOptions{})
	if !errors.Is(err, writeErr) || !errors.Is(err, cleanupErr) {
		t.Fatalf("error = %v, want joined write and cleanup failures", err)
	}
	if fake.commitCalls != 0 || fake.cleanupCalls != 1 {
		t.Fatalf("commit calls=%d cleanup calls=%d, want 0/1", fake.commitCalls, fake.cleanupCalls)
	}
}

func TestManualEditReportsCompletePublishedOutputOnPublicationError(t *testing.T) {
	dir := t.TempDir()
	sourcePath := filepath.Join(dir, "source.txt")
	outputPath := filepath.Join(dir, "output.txt")
	const source = "source bytes"
	const edited = "edited bytes"
	writeManualTestFile(t, sourcePath, []byte(source))
	finalizationErr := errors.New("forced directory sync failure")

	fake := &scriptedAtomicOutput{}
	fake.commitFn = func(context.Context) error {
		if err := os.WriteFile(outputPath, fake.data.Bytes(), 0o600); err != nil {
			return err
		}
		return &fileio.PublicationError{
			FinalPath: outputPath,
			Durable:   false,
			Err:       finalizationErr,
		}
	}
	installManualAtomicOutput(t, func(string, []string, os.FileMode) (atomicOutput, error) {
		return fake, nil
	})

	summary, err := ApplyFileEdit(context.Background(), sourcePath, outputPath, Edit{
		Start: 0,
		End:   int64(len(source)),
		Text:  []byte(edited),
	}, FileOptions{})
	if !errors.Is(err, finalizationErr) {
		t.Fatalf("error = %v, want publication finalization failure", err)
	}
	var publicationErr *fileio.PublicationError
	if !errors.As(err, &publicationErr) {
		t.Fatalf("error = %T %v, want *fileio.PublicationError", err, err)
	}
	if !summary.Complete || !summary.Published || summary.BytesWritten != int64(len(edited)) {
		t.Fatalf("publication-error summary = %+v", summary)
	}
	assertManualFileContent(t, sourcePath, source)
	assertManualFileContent(t, outputPath, edited)
}

func TestManualEditRejectsUnexpectedExactByteCountBeforeCommit(t *testing.T) {
	dir := t.TempDir()
	sourcePath := filepath.Join(dir, "source.txt")
	outputPath := filepath.Join(dir, "output.txt")
	const source = "source bytes"
	writeManualTestFile(t, sourcePath, []byte(source))

	session := newBoundManualTestSession(t, sourcePath, 1)
	if err := session.ApplyVerifiedEdit(Edit{Start: 0, End: int64(len(source)), Text: []byte("edited bytes")}, []byte(source)); err != nil {
		t.Fatal(err)
	}
	mutated := false
	summary, err := WriteSessionToFile(context.Background(), sourcePath, outputPath, session, FileOptions{
		SourceGeneration: 1,
		Progress: func(Progress) {
			if !mutated {
				mutated = true
				// Sessions are single-owner structures. This deliberately violates
				// that contract inside the callback to prove publication still has an
				// independent exact-byte-count gate.
				session.table.size++
			}
		},
	})
	if !errors.Is(err, ErrIncompleteManualEditOutput) {
		t.Fatalf("error = %v, want ErrIncompleteManualEditOutput", err)
	}
	if summary.Complete || summary.Published {
		t.Fatalf("incomplete summary = %+v", summary)
	}
	if _, err := os.Lstat(outputPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("output exists after exact-count rejection: %v", err)
	}
	assertManualDirEntries(t, dir, filepath.Base(sourcePath))
}

func TestManualEditRevalidatesSourceAfterOutputSync(t *testing.T) {
	dir := t.TempDir()
	sourcePath := filepath.Join(dir, "source.txt")
	outputPath := filepath.Join(dir, "output.txt")
	const source = "abcdefghij"
	const replacement = "abcdeXghij"
	writeManualTestFile(t, sourcePath, []byte(source))
	openedInfo, err := os.Stat(sourcePath)
	if err != nil {
		t.Fatal(err)
	}

	published := false
	fake := &scriptedAtomicOutput{
		beforeValidateFn: func(context.Context) error {
			if err := os.WriteFile(sourcePath, []byte(replacement), 0o600); err != nil {
				return err
			}
			return os.Chtimes(sourcePath, openedInfo.ModTime(), openedInfo.ModTime())
		},
		commitFn: func(context.Context) error {
			published = true
			return nil
		},
	}
	installManualAtomicOutput(t, func(string, []string, os.FileMode) (atomicOutput, error) {
		return fake, nil
	})

	summary, err := ApplyFileEdit(context.Background(), sourcePath, outputPath, Edit{
		Start: 0,
		End:   1,
		Text:  []byte("A"),
	}, FileOptions{})
	if !errors.Is(err, ErrSourceModifiedDuringOperation) {
		t.Fatalf("error = %v, want ErrSourceModifiedDuringOperation", err)
	}
	if published || summary.Complete || summary.Published {
		t.Fatalf("stale source published=%t summary=%+v", published, summary)
	}
	if fake.commitCalls != 1 || fake.cleanupCalls != 1 {
		t.Fatalf("commit calls=%d cleanup calls=%d, want 1/1", fake.commitCalls, fake.cleanupCalls)
	}
	if _, err := os.Stat(outputPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("stale manual edit exposed an output: %v", err)
	}
	assertManualFileContent(t, sourcePath, replacement)
}

func TestManualEditShortSourceReadCannotPublish(t *testing.T) {
	dir := t.TempDir()
	sourcePath := filepath.Join(dir, "source.bin")
	outputPath := filepath.Join(dir, "output.bin")
	const oneMiB = 1 << 20
	source := bytes.Repeat([]byte("a"), 3*oneMiB)
	writeManualTestFile(t, sourcePath, source)

	mutator, err := os.OpenFile(sourcePath, os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer mutator.Close()
	truncated := false
	var truncateErr error
	summary, err := ApplyFileEdit(context.Background(), sourcePath, outputPath, Edit{
		Start: int64(len(source)),
		End:   int64(len(source)),
		Text:  []byte("!"),
	}, FileOptions{Progress: func(p Progress) {
		if !truncated && p.BytesWritten >= oneMiB {
			truncated = true
			truncateErr = mutator.Truncate(oneMiB)
		}
	}})
	if truncateErr != nil {
		t.Fatalf("truncate source during stream: %v", truncateErr)
	}
	if !errors.Is(err, ErrSourceModifiedDuringOperation) {
		t.Fatalf("error = %v, want ErrSourceModifiedDuringOperation", err)
	}
	if summary.Complete || summary.Published || summary.BytesWritten != oneMiB {
		t.Fatalf("short-source summary = %+v", summary)
	}
	if _, err := os.Lstat(outputPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("output exists after source short read: %v", err)
	}
	assertManualDirEntries(t, dir, filepath.Base(sourcePath))
}

func TestManualEditSourcePathSubstitutionCannotPublish(t *testing.T) {
	dir := t.TempDir()
	sourcePath := filepath.Join(dir, "source.bin")
	replacementPath := filepath.Join(dir, "source-replacement.bin")
	outputPath := filepath.Join(dir, "output.bin")
	source := bytes.Repeat([]byte("a"), 2*(1<<20))
	replacement := bytes.Repeat([]byte("z"), len(source))
	writeManualTestFile(t, sourcePath, source)
	writeManualTestFile(t, replacementPath, replacement)

	// Model an adversary replacing sourcePath after streaming but before the
	// final identity check. On Windows the real open source handle denies such a
	// rename; this seam exercises the same platform-independent rejection branch.
	originalStatSource := statSourcePath
	statSourcePath = func(path string) (os.FileInfo, error) {
		if path == sourcePath {
			return os.Stat(replacementPath)
		}
		return originalStatSource(path)
	}
	t.Cleanup(func() { statSourcePath = originalStatSource })

	summary, err := ApplyFileEdit(context.Background(), sourcePath, outputPath, Edit{
		Start: int64(len(source)),
		End:   int64(len(source)),
		Text:  []byte("!"),
	}, FileOptions{})
	if !errors.Is(err, ErrSourceModifiedDuringOperation) {
		t.Fatalf("error = %v, want ErrSourceModifiedDuringOperation", err)
	}
	if summary.Complete || summary.Published {
		t.Fatalf("source-substitution summary = %+v", summary)
	}
	if _, err := os.Lstat(outputPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("output exists after source substitution: %v", err)
	}
	assertManualFileContent(t, sourcePath, string(source))
	assertManualFileContent(t, replacementPath, string(replacement))
}

type scriptedAtomicOutput struct {
	data             bytes.Buffer
	writeFn          func([]byte) (int, error)
	beforeValidateFn func(context.Context) error
	commitFn         func(context.Context) error
	commitErr        error
	cleanupErr       error
	commitCalls      int
	cleanupCalls     int
}

func (o *scriptedAtomicOutput) Write(p []byte) (int, error) {
	if o.writeFn != nil {
		return o.writeFn(p)
	}
	return o.data.Write(p)
}

func (o *scriptedAtomicOutput) CommitContextValidated(ctx context.Context, validate func(context.Context) error) error {
	o.commitCalls++
	if o.beforeValidateFn != nil {
		if err := o.beforeValidateFn(ctx); err != nil {
			return err
		}
	}
	if validate == nil {
		return errors.New("validator is required")
	}
	if err := validate(ctx); err != nil {
		return err
	}
	if o.commitFn != nil {
		return o.commitFn(ctx)
	}
	return o.commitErr
}

func (o *scriptedAtomicOutput) Cleanup() error {
	o.cleanupCalls++
	return o.cleanupErr
}

func installManualAtomicOutput(t *testing.T, factory func(string, []string, os.FileMode) (atomicOutput, error)) {
	t.Helper()
	original := openAtomicOutput
	openAtomicOutput = factory
	t.Cleanup(func() {
		openAtomicOutput = original
	})
}

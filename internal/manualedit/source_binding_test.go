package manualedit

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/quarry/quarry-wails3/internal/document"
	"github.com/quarry/quarry-wails3/internal/sourceio"
)

func TestSourceBoundSessionContextCancellationIsNotSourceDrift(t *testing.T) {
	path := filepath.Join(t.TempDir(), "source.txt")
	writeManualTestFile(t, path, []byte("abcdef"))
	doc, err := document.OpenFile(path)
	if err != nil {
		t.Fatal(err)
	}
	defer doc.Close()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	session, err := NewSourceBoundSessionContext(ctx, doc, path, 1, DefaultLimits())
	if session != nil || !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled constructor = session %v error %v, want nil/context.Canceled", session, err)
	}
	if errors.Is(err, ErrSessionSourceChanged) {
		t.Fatalf("cancellation was misclassified as source drift: %v", err)
	}
}

func TestSourceBoundSessionPreflightsVerifierAgainstTransientLimit(t *testing.T) {
	path := filepath.Join(t.TempDir(), "source.txt")
	writeManualTestFile(t, path, []byte("abcdef"))
	doc, err := document.OpenFile(path)
	if err != nil {
		t.Fatal(err)
	}
	defer doc.Close()
	retained, buffer, err := sourceio.VerificationMemoryBoundsForSize(doc.Size())
	if err != nil {
		t.Fatal(err)
	}
	required, err := checkedMemorySum(sourceCompareBufferBytes, sourceWriteBufferBytes, retained, buffer)
	if err != nil {
		t.Fatal(err)
	}
	limits := DefaultLimits()
	limits.MaxTransientBytes = required - 1
	if _, err := NewSourceBoundSessionContext(context.Background(), doc, path, 1, limits); !errors.Is(err, ErrTransientMemoryLimit) {
		t.Fatalf("preflight error = %v, want ErrTransientMemoryLimit", err)
	}
}

func TestSourcePreviewRangeBatchBoundsAggregateDirectIOAcrossDistributedRanges(t *testing.T) {
	path := filepath.Join(t.TempDir(), "source.txt")
	writeManualTestFile(t, path, nil)
	const sourceSize int64 = 20 << 20
	if err := os.Truncate(path, sourceSize); err != nil {
		t.Fatal(err)
	}
	session := newBoundManualTestSession(t, path, 1)
	base := session.binding.reader
	counter := &countingPreviewReader{base: base}
	session.binding.reader = counter

	const rangeCount = DefaultMaxEditCount
	const rangeBytes = 240
	ranges := make([]Range, rangeCount)
	for index := range ranges {
		start := int64(index) * (sourceSize - rangeBytes) / (rangeCount - 1)
		ranges[index] = Range{Start: start, End: start + rangeBytes}
	}
	previews, err := session.ReadSourcePreviewRangesContext(
		context.Background(), ranges, rangeCount, rangeBytes, rangeCount*rangeBytes,
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(previews) != rangeCount {
		t.Fatalf("preview count = %d, want %d", len(previews), rangeCount)
	}
	if counter.calls != rangeCount {
		t.Fatalf("direct source read calls = %d, want %d bounded tiny reads", counter.calls, rangeCount)
	}
	if want := int64(rangeCount * rangeBytes); counter.requestedBytes != want {
		t.Fatalf("direct source bytes = %d, want %d (no per-range verification-block replay)", counter.requestedBytes, want)
	}

	beforeCalls := counter.calls
	if _, err := session.ReadSourcePreviewRangesContext(
		context.Background(), ranges, rangeCount-1, rangeBytes, rangeCount*rangeBytes,
	); err == nil {
		t.Fatal("range-count overflow was accepted")
	}
	if _, err := session.ReadSourcePreviewRangesContext(
		context.Background(), ranges[:2], 2, rangeBytes, 2*rangeBytes-1,
	); err == nil {
		t.Fatal("aggregate-byte overflow was accepted")
	}
	if counter.calls != beforeCalls {
		t.Fatalf("rejected preview limits performed %d source reads", counter.calls-beforeCalls)
	}
}

func TestSourcePreviewRangeBatchRejectsRewriteAtReadSeam(t *testing.T) {
	path := filepath.Join(t.TempDir(), "source.txt")
	original := bytes.Repeat([]byte("a"), 512)
	rewritten := bytes.Repeat([]byte("b"), len(original))
	writeManualTestFile(t, path, original)
	before, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	session := newBoundManualTestSession(t, path, 1)
	base := session.binding.reader
	var mutationErr error
	rewrite := func(data []byte) {
		if mutationErr != nil {
			return
		}
		mutationErr = os.WriteFile(path, data, 0o600)
		if mutationErr == nil {
			mutationErr = os.Chtimes(path, before.ModTime(), before.ModTime())
		}
	}
	session.binding.reader = &countingPreviewReader{
		base: base,
		beforeRead: func(call int) {
			if call == 2 {
				rewrite(rewritten)
			}
		},
		afterRead: func(call int) {
			if call == 2 {
				rewrite(original)
			}
		},
	}

	previews, err := session.ReadSourcePreviewRangesContext(
		context.Background(),
		[]Range{{Start: 0, End: 16}, {Start: 16, End: 32}},
		2,
		16,
		32,
	)
	if mutationErr != nil {
		t.Fatalf("same-size restored-time mutation: %v", mutationErr)
	}
	if previews != nil || !errors.Is(err, ErrSessionSourceChanged) {
		t.Fatalf("mutation-seam previews = %v, error = %v; want nil/ErrSessionSourceChanged", previews, err)
	}
	assertManualFileContent(t, path, string(original))
}

type countingPreviewReader struct {
	base           document.ReaderAtSize
	calls          int
	requestedBytes int64
	beforeRead     func(call int)
	afterRead      func(call int)
}

func (r *countingPreviewReader) Size() int64 { return r.base.Size() }

func (r *countingPreviewReader) ReadAt(buffer []byte, offset int64) (int, error) {
	r.calls++
	r.requestedBytes += int64(len(buffer))
	if r.beforeRead != nil {
		r.beforeRead(r.calls)
	}
	n, err := r.base.ReadAt(buffer, offset)
	if r.afterRead != nil {
		r.afterRead(r.calls)
	}
	return n, err
}

func TestSourceBoundSessionRequiresAndChecksExpectedOldBytes(t *testing.T) {
	dir := t.TempDir()
	sourcePath := filepath.Join(dir, "source.txt")
	writeManualTestFile(t, sourcePath, []byte("abcdef"))
	session := newBoundManualTestSession(t, sourcePath, 7)

	if err := session.ApplyEdit(Edit{Start: 1, End: 3, Text: []byte("XY")}); !errors.Is(err, ErrExpectedBytesRequired) {
		t.Fatalf("unverified edit error = %v, want ErrExpectedBytesRequired", err)
	}
	if err := session.ApplyVerifiedEdit(Edit{Start: 1, End: 3, Text: []byte("XY")}, []byte("zz")); !errors.Is(err, ErrExpectedBytesMismatch) {
		t.Fatalf("wrong expected bytes error = %v, want ErrExpectedBytesMismatch", err)
	}
	if session.HasEdits() {
		t.Fatal("rejected expected bytes created an edit")
	}
	if err := session.ApplyVerifiedEdit(Edit{Start: 1, End: 3, Text: []byte("XY")}, []byte("bc")); err != nil {
		t.Fatal(err)
	}
}

func TestWriteSessionRejectsGenerationMismatchBeforeOutput(t *testing.T) {
	dir := t.TempDir()
	sourcePath := filepath.Join(dir, "source.txt")
	outputPath := filepath.Join(dir, "output.txt")
	writeManualTestFile(t, sourcePath, []byte("abcdef"))
	session := newBoundManualTestSession(t, sourcePath, 7)
	if err := session.ApplyVerifiedEdit(Edit{Start: 1, End: 3, Text: []byte("XY")}, []byte("bc")); err != nil {
		t.Fatal(err)
	}

	summary, err := WriteSessionToFile(context.Background(), sourcePath, outputPath, session, FileOptions{SourceGeneration: 8})
	if !errors.Is(err, ErrSessionGenerationChanged) {
		t.Fatalf("error = %v, want ErrSessionGenerationChanged", err)
	}
	if summary.BytesWritten != 0 || summary.Published || summary.Complete {
		t.Fatalf("generation-mismatch summary = %+v", summary)
	}
	assertPathDoesNotExist(t, outputPath)
}

func TestWriteSessionRejectsSameSizeRewriteWithRestoredTimestampOutsideEdit(t *testing.T) {
	dir := t.TempDir()
	sourcePath := filepath.Join(dir, "source.txt")
	outputPath := filepath.Join(dir, "output.txt")
	const original = "alpha bravo charlie"
	const rewritten = "ALPHA bravo charlie"
	writeManualTestFile(t, sourcePath, []byte(original))
	before, err := os.Stat(sourcePath)
	if err != nil {
		t.Fatal(err)
	}
	session := newBoundManualTestSession(t, sourcePath, 1)
	if err := session.ApplyVerifiedEdit(Edit{Start: 6, End: 11, Text: []byte("delta")}, []byte("bravo")); err != nil {
		t.Fatal(err)
	}
	writeManualTestFile(t, sourcePath, []byte(rewritten))
	if err := os.Chtimes(sourcePath, before.ModTime(), before.ModTime()); err != nil {
		t.Fatal(err)
	}

	summary, err := WriteSessionToFile(context.Background(), sourcePath, outputPath, session, FileOptions{SourceGeneration: 1})
	if !errors.Is(err, ErrSessionSourceChanged) {
		t.Fatalf("error = %v, want ErrSessionSourceChanged", err)
	}
	if summary.BytesWritten != 0 || summary.Published || summary.Complete {
		t.Fatalf("same-size-rewrite summary = %+v", summary)
	}
	assertManualFileContent(t, sourcePath, rewritten)
	assertPathDoesNotExist(t, outputPath)
}

func TestWriteSessionRejectsReplacementWithCopiedMetadata(t *testing.T) {
	dir := t.TempDir()
	sourcePath := filepath.Join(dir, "source.txt")
	replacementPath := filepath.Join(dir, "replacement.txt")
	outputPath := filepath.Join(dir, "output.txt")
	const source = "alpha bravo charlie"
	writeManualTestFile(t, sourcePath, []byte(source))
	before, err := os.Stat(sourcePath)
	if err != nil {
		t.Fatal(err)
	}
	session := newBoundManualTestSession(t, sourcePath, 1)
	if err := session.ApplyVerifiedEdit(Edit{Start: 6, End: 11, Text: []byte("delta")}, []byte("bravo")); err != nil {
		t.Fatal(err)
	}

	writeManualTestFile(t, replacementPath, []byte(source))
	if err := os.Chtimes(replacementPath, before.ModTime(), before.ModTime()); err != nil {
		t.Fatal(err)
	}
	oldPath := filepath.Join(dir, "old-source.txt")
	if err := os.Rename(sourcePath, oldPath); err != nil {
		t.Fatalf("rename retained source aside: %v", err)
	}
	if err := os.Rename(replacementPath, sourcePath); err != nil {
		t.Fatalf("install replacement source: %v", err)
	}

	_, err = WriteSessionToFile(context.Background(), sourcePath, outputPath, session, FileOptions{SourceGeneration: 1})
	if !errors.Is(err, ErrSessionSourceChanged) {
		t.Fatalf("error = %v, want ErrSessionSourceChanged", err)
	}
	assertPathDoesNotExist(t, outputPath)
}

func TestWriteSessionRejectsSourceSymlinkRetarget(t *testing.T) {
	dir := t.TempDir()
	firstTarget := filepath.Join(dir, "first.txt")
	secondTarget := filepath.Join(dir, "second.txt")
	sourcePath := filepath.Join(dir, "source-link.txt")
	outputPath := filepath.Join(dir, "output.txt")
	const source = "alpha bravo charlie"
	writeManualTestFile(t, firstTarget, []byte(source))
	writeManualTestFile(t, secondTarget, []byte(source))
	if err := os.Symlink(firstTarget, sourcePath); err != nil {
		t.Skipf("symbolic links unavailable: %v", err)
	}
	session := newBoundManualTestSession(t, sourcePath, 1)
	if err := session.ApplyVerifiedEdit(Edit{Start: 6, End: 11, Text: []byte("delta")}, []byte("bravo")); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(sourcePath); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(secondTarget, sourcePath); err != nil {
		t.Fatal(err)
	}

	_, err := WriteSessionToFile(context.Background(), sourcePath, outputPath, session, FileOptions{SourceGeneration: 1})
	if !errors.Is(err, ErrSessionSourceChanged) {
		t.Fatalf("error = %v, want ErrSessionSourceChanged", err)
	}
	assertPathDoesNotExist(t, outputPath)
}

func TestWriteSessionPostStreamFingerprintRejectsRestoredTimestampMutation(t *testing.T) {
	dir := t.TempDir()
	sourcePath := filepath.Join(dir, "source.txt")
	outputPath := filepath.Join(dir, "output.txt")
	const original = "alpha bravo charlie"
	const rewritten = "ALPHA bravo charlie"
	writeManualTestFile(t, sourcePath, []byte(original))
	before, err := os.Stat(sourcePath)
	if err != nil {
		t.Fatal(err)
	}
	session := newBoundManualTestSession(t, sourcePath, 1)
	if err := session.ApplyVerifiedEdit(Edit{Start: 6, End: 11, Text: []byte("delta")}, []byte("bravo")); err != nil {
		t.Fatal(err)
	}

	mutated := false
	summary, err := WriteSessionToFile(context.Background(), sourcePath, outputPath, session, FileOptions{
		SourceGeneration: 1,
		Progress: func(Progress) {
			if mutated {
				return
			}
			mutated = true
			if writeErr := os.WriteFile(sourcePath, []byte(rewritten), 0o600); writeErr != nil {
				t.Errorf("rewrite source during stream: %v", writeErr)
				return
			}
			if timeErr := os.Chtimes(sourcePath, before.ModTime(), before.ModTime()); timeErr != nil {
				t.Errorf("restore source timestamp: %v", timeErr)
			}
		},
	})
	if !errors.Is(err, ErrSessionSourceChanged) {
		t.Fatalf("error = %v, want ErrSessionSourceChanged", err)
	}
	if !mutated {
		t.Fatal("progress hook did not mutate the source")
	}
	if summary.Published || summary.Complete {
		t.Fatalf("post-stream-drift summary = %+v", summary)
	}
	assertPathDoesNotExist(t, outputPath)
}

func TestManualEditSourceSpanVerifierRejectsTransientRewriteBeforeBytesEscape(t *testing.T) {
	tests := []struct {
		name      string
		wantError error
		run       func(*testing.T, string, string, int64, func(Progress)) (FileSummary, error)
	}{
		{
			name:      "source-bound session",
			wantError: ErrSessionSourceChanged,
			run: func(t *testing.T, sourcePath string, outputPath string, size int64, progress func(Progress)) (FileSummary, error) {
				t.Helper()
				session := newBoundManualTestSession(t, sourcePath, 1)
				if err := session.ApplyVerifiedEdit(Edit{Start: size, End: size, Text: []byte("!")}, nil); err != nil {
					t.Fatal(err)
				}
				return WriteSessionToFile(context.Background(), sourcePath, outputPath, session, FileOptions{
					SourceGeneration: 1,
					Progress:         progress,
				})
			},
		},
		{
			name:      "direct file edit",
			wantError: ErrSourceModifiedDuringOperation,
			run: func(t *testing.T, sourcePath string, outputPath string, size int64, progress func(Progress)) (FileSummary, error) {
				t.Helper()
				return ApplyFileEdit(context.Background(), sourcePath, outputPath, Edit{
					Start: size,
					End:   size,
					Text:  []byte("!"),
				}, FileOptions{Progress: progress})
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			sourcePath := filepath.Join(dir, "source.bin")
			outputPath := filepath.Join(dir, "output.bin")
			original := bytes.Repeat([]byte{'a'}, 3*sourceWriteBufferBytes)
			writeManualTestFile(t, sourcePath, original)
			openedInfo, err := os.Stat(sourcePath)
			if err != nil {
				t.Fatal(err)
			}

			rewriteSecondBlock := func(value byte) error {
				file, err := os.OpenFile(sourcePath, os.O_WRONLY, 0)
				if err != nil {
					return err
				}
				block := bytes.Repeat([]byte{value}, sourceWriteBufferBytes)
				written, writeErr := file.WriteAt(block, sourceWriteBufferBytes)
				if writeErr == nil && written != len(block) {
					writeErr = errors.New("short adversarial source rewrite")
				}
				closeErr := file.Close()
				if writeErr != nil {
					return errors.Join(writeErr, closeErr)
				}
				if closeErr != nil {
					return closeErr
				}
				return os.Chtimes(sourcePath, openedInfo.ModTime(), openedInfo.ModTime())
			}

			progressCalls := 0
			sourceChanged := false
			var adversaryErr error
			progress := func(Progress) {
				progressCalls++
				switch progressCalls {
				case 1:
					adversaryErr = rewriteSecondBlock('b')
					sourceChanged = adversaryErr == nil
				case 2:
					// Vulnerable raw readers reach this callback only after the
					// changed block has already escaped to the output. Restore the
					// source so their final whole-file digest cannot expose the mix.
					adversaryErr = rewriteSecondBlock('a')
					sourceChanged = adversaryErr != nil
				}
			}

			summary, err := tt.run(t, sourcePath, outputPath, int64(len(original)), progress)
			if adversaryErr != nil {
				t.Fatalf("adversarial rewrite: %v", adversaryErr)
			}
			if sourceChanged {
				if restoreErr := rewriteSecondBlock('a'); restoreErr != nil {
					t.Fatalf("restore adversarial source: %v", restoreErr)
				}
			}

			if !errors.Is(err, tt.wantError) || !errors.Is(err, sourceio.ErrSourceChanged) {
				t.Fatalf("error = %v, want %v and sourceio.ErrSourceChanged", err, tt.wantError)
			}
			if progressCalls != 1 {
				t.Fatalf("progress calls = %d, want 1; changed source bytes escaped to the output writer", progressCalls)
			}
			if summary.BytesWritten != sourceWriteBufferBytes || summary.Complete || summary.Published {
				t.Fatalf("transient-rewrite summary = %+v", summary)
			}
			assertPathDoesNotExist(t, outputPath)
			got, readErr := os.ReadFile(sourcePath)
			if readErr != nil {
				t.Fatal(readErr)
			}
			if !bytes.Equal(got, original) {
				t.Fatal("adversarial writer did not restore the source bytes")
			}
		})
	}
}

func TestUnboundSessionCannotBePublished(t *testing.T) {
	dir := t.TempDir()
	sourcePath := filepath.Join(dir, "source.txt")
	outputPath := filepath.Join(dir, "output.txt")
	writeManualTestFile(t, sourcePath, []byte("abc"))
	session := NewSession(3, DefaultMaxInsertedBytes)
	if err := session.ApplyEdit(Edit{Start: 3, End: 3, Text: []byte("d")}); err != nil {
		t.Fatal(err)
	}
	_, err := WriteSessionToFile(context.Background(), sourcePath, outputPath, session, FileOptions{SourceGeneration: 1})
	if !errors.Is(err, ErrSessionSourceUnbound) {
		t.Fatalf("error = %v, want ErrSessionSourceUnbound", err)
	}
	assertPathDoesNotExist(t, outputPath)
}

func assertPathDoesNotExist(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("path %q exists after rejected save: %v", path, err)
	}
}

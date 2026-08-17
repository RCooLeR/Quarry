package sourceio

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/quarry/quarry-wails3/internal/document"
)

type cancelingReaderAt struct {
	cancel context.CancelFunc
}

func (r *cancelingReaderAt) ReadAt(p []byte, _ int64) (int, error) {
	for i := range p {
		p[i] = 'x'
	}
	r.cancel()
	return len(p), nil
}

func TestExpectDocumentClassifiesRetainedGenerationChanges(t *testing.T) {
	path := filepath.Join(t.TempDir(), "source.csv")
	if err := os.WriteFile(path, []byte("id,value\n1,original\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	doc, err := document.OpenFile(path)
	if err != nil {
		t.Fatal(err)
	}
	defer doc.Close()
	changed := info.ModTime().Add(2 * time.Second)
	if err := os.Chtimes(path, changed, changed); err != nil {
		t.Fatal(err)
	}

	_, err = ExpectDocument(doc)
	if !errors.Is(err, ErrSourceChanged) {
		t.Fatalf("error = %v, want sourceio.ErrSourceChanged", err)
	}
	if !errors.Is(err, document.ErrSourceChanged) {
		t.Fatalf("error = %v, want preserved document.ErrSourceChanged", err)
	}
}

func TestExpectationRejectsSameSizeRewriteWithRestoredMtime(t *testing.T) {
	path := filepath.Join(t.TempDir(), "source.csv")
	original := []byte("id,value\n1,original\n")
	changed := []byte("id,value\n9,altered!\n")
	if len(changed) != len(original) {
		t.Fatalf("fixture sizes differ: %d != %d", len(changed), len(original))
	}
	if err := os.WriteFile(path, original, 0o600); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	doc, err := document.OpenFile(path)
	if err != nil {
		t.Fatal(err)
	}
	defer doc.Close()
	expected, err := ExpectDocumentContext(context.Background(), doc)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, changed, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, info.ModTime(), info.ModTime()); err != nil {
		t.Fatal(err)
	}

	handle, err := OpenContext(context.Background(), path, expected)
	if err != nil {
		t.Fatal(err)
	}
	defer handle.Close()
	if _, err := io.Copy(io.Discard, handle); !errors.Is(err, ErrSourceChanged) {
		t.Fatalf("stream error = %v, want ErrSourceChanged", err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, changed) {
		t.Fatalf("rejected source changed again: got %q, want %q", got, changed)
	}
}

func TestExpectationValidatesRetainedDocumentBytes(t *testing.T) {
	path := filepath.Join(t.TempDir(), "source.bin")
	original := []byte("0123456789abcdef")
	changed := []byte("0123456789abcdeF")
	if err := os.WriteFile(path, original, 0o600); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	doc, err := document.OpenFile(path)
	if err != nil {
		t.Fatal(err)
	}
	defer doc.Close()
	expected, err := ExpectDocumentContext(context.Background(), doc)
	if err != nil {
		t.Fatal(err)
	}
	if err := expected.ValidateDocumentContext(context.Background(), doc); err != nil {
		t.Fatalf("unchanged validation failed: %v", err)
	}
	if err := os.WriteFile(path, changed, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, info.ModTime(), info.ModTime()); err != nil {
		t.Fatal(err)
	}
	if err := expected.ValidateDocumentContext(context.Background(), doc); !errors.Is(err, ErrSourceChanged) {
		t.Fatalf("changed validation error = %v, want ErrSourceChanged", err)
	}
}

func TestHandleRejectsMixedReadEvenWhenSourceIsRestoredBeforeValidation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "source.bin")
	original := bytes.Repeat([]byte{'a'}, 2*fingerprintChunkBytes)
	if err := os.WriteFile(path, original, 0o600); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	doc, err := document.OpenFile(path)
	if err != nil {
		t.Fatal(err)
	}
	defer doc.Close()
	expected, err := ExpectDocumentContext(context.Background(), doc)
	if err != nil {
		t.Fatal(err)
	}
	handle, err := OpenContext(context.Background(), path, expected)
	if err != nil {
		t.Fatal(err)
	}
	defer handle.Close()

	first := make([]byte, fingerprintChunkBytes)
	if _, err := io.ReadFull(handle, first); err != nil {
		t.Fatal(err)
	}
	writer, err := os.OpenFile(path, os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	changedTail := bytes.Repeat([]byte{'b'}, fingerprintChunkBytes)
	if _, err := writer.WriteAt(changedTail, fingerprintChunkBytes); err != nil {
		_ = writer.Close()
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	second := make([]byte, fingerprintChunkBytes)
	if n, err := io.ReadFull(handle, second); n != 0 || !errors.Is(err, ErrSourceChanged) {
		t.Fatalf("changed block read = %d, %v; want rejection before bytes escape", n, err)
	}
	if err := os.WriteFile(path, original, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, info.ModTime(), info.ModTime()); err != nil {
		t.Fatal(err)
	}
	if err := handle.ValidateContext(context.Background()); !errors.Is(err, ErrSourceChanged) {
		t.Fatalf("validation error = %v, want retained mixed-stream rejection", err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, original) {
		t.Fatal("validation modified the restored source")
	}
}

func TestFingerprintReaderAtCancellationIsPromptAndClassified(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	reader := &cancelingReaderAt{cancel: cancel}
	_, err := fingerprintReaderAt(ctx, reader, 2*fingerprintChunkBytes)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want context.Canceled", err)
	}
}

func TestFingerprintProgressIsBoundedMonotonicAndFinal(t *testing.T) {
	const total = int64(100_003)
	updates := make([]int64, 0, maxFingerprintProgressCallbacks)
	reporter := newFingerprintProgressReporter(total, func(completed, gotTotal int64) {
		if gotTotal != total {
			t.Fatalf("progress total = %d, want %d", gotTotal, total)
		}
		updates = append(updates, completed)
	})
	for completed := int64(0); completed <= total; completed++ {
		reporter.report(completed)
	}
	reporter.report(total) // duplicate final reports are suppressed
	if len(updates) == 0 || int64(len(updates)) > maxFingerprintProgressCallbacks {
		t.Fatalf("progress callbacks = %d, want 1..%d", len(updates), maxFingerprintProgressCallbacks)
	}
	previous := int64(-1)
	for _, completed := range updates {
		if completed <= previous || completed < 0 || completed > total {
			t.Fatalf("non-monotonic progress after %d: %d", previous, completed)
		}
		previous = completed
	}
	if previous != total {
		t.Fatalf("final progress = %d, want %d", previous, total)
	}

	emptyCalls := 0
	empty := newFingerprintProgressReporter(0, func(completed, gotTotal int64) {
		emptyCalls++
		if completed != 0 || gotTotal != 0 {
			t.Fatalf("empty progress = %d/%d", completed, gotTotal)
		}
	})
	empty.report(0)
	empty.report(0)
	if emptyCalls != 1 {
		t.Fatalf("empty progress callbacks = %d, want 1", emptyCalls)
	}
}

func TestChunkFingerprintProgressCancellationReturnsNoExpectationData(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	data := bytes.Repeat([]byte{'x'}, 2*fingerprintChunkBytes)
	callbacks := 0
	_, chunkSize, chunks, err := fingerprintReaderAtWithChunksProgress(ctx, bytes.NewReader(data), int64(len(data)), func(completed, total int64) {
		callbacks++
		if completed <= 0 || total != int64(len(data)) {
			t.Fatalf("progress = %d/%d", completed, total)
		}
		cancel()
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want context.Canceled", err)
	}
	if chunkSize != 0 || chunks != nil {
		t.Fatalf("cancelled fingerprint leaked partial expectation data: chunkSize=%d chunks=%d", chunkSize, len(chunks))
	}
	if callbacks != 1 {
		t.Fatalf("progress callbacks before cancellation = %d, want 1", callbacks)
	}
}

func TestVerifiedDocumentReaderRejectsChangedBlockBeforeReturningBytes(t *testing.T) {
	path := filepath.Join(t.TempDir(), "source.bin")
	original := bytes.Repeat([]byte{'a'}, 2*fingerprintChunkBytes)
	changed := append([]byte(nil), original...)
	changed[fingerprintChunkBytes+17] = 'b'
	if err := os.WriteFile(path, original, 0o600); err != nil {
		t.Fatal(err)
	}
	doc, err := document.OpenFile(path)
	if err != nil {
		t.Fatal(err)
	}
	defer doc.Close()
	expected, err := ExpectDocumentContext(context.Background(), doc)
	if err != nil {
		t.Fatal(err)
	}
	reader, err := NewVerifiedDocumentReader(context.Background(), expected, doc)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, changed, 0o600); err != nil {
		t.Fatal(err)
	}

	buf := make([]byte, 32)
	n, err := reader.ReadAt(buf, fingerprintChunkBytes)
	if n != 0 || !errors.Is(err, ErrSourceChanged) {
		t.Fatalf("verified read = %d, %v; want no returned bytes and ErrSourceChanged", n, err)
	}
	if !bytes.Equal(buf, make([]byte, len(buf))) {
		t.Fatalf("unverified bytes escaped to caller: %q", buf)
	}
}

func TestHandlePreviewSeekReturnsOnlyExpectedGeneration(t *testing.T) {
	path := filepath.Join(t.TempDir(), "source.bin")
	original := bytes.Repeat([]byte("0123456789abcdef"), 128*1024)
	if err := os.WriteFile(path, original, 0o600); err != nil {
		t.Fatal(err)
	}
	doc, err := document.OpenFile(path)
	if err != nil {
		t.Fatal(err)
	}
	defer doc.Close()
	expected, err := ExpectDocumentContext(context.Background(), doc)
	if err != nil {
		t.Fatal(err)
	}
	handle, err := OpenContext(context.Background(), path, expected)
	if err != nil {
		t.Fatal(err)
	}
	defer handle.Close()

	preview := make([]byte, 4096)
	if _, err := io.ReadFull(handle, preview); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(preview, original[:len(preview)]) {
		t.Fatal("preview did not come from the expected source generation")
	}
	if _, err := handle.Seek(0, io.SeekStart); err != nil {
		t.Fatal(err)
	}
	var full bytes.Buffer
	if _, err := io.Copy(&full, handle); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(full.Bytes(), original) {
		t.Fatal("restarted transform did not consume the expected generation")
	}
	if err := handle.ValidateContext(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestHandleRejectsTransientlyChangedPreviewBlock(t *testing.T) {
	path := filepath.Join(t.TempDir(), "source.bin")
	original := bytes.Repeat([]byte{'a'}, fingerprintChunkBytes)
	changed := bytes.Repeat([]byte{'b'}, fingerprintChunkBytes)
	if err := os.WriteFile(path, original, 0o600); err != nil {
		t.Fatal(err)
	}
	doc, err := document.OpenFile(path)
	if err != nil {
		t.Fatal(err)
	}
	defer doc.Close()
	expected, err := ExpectDocumentContext(context.Background(), doc)
	if err != nil {
		t.Fatal(err)
	}
	handle, err := OpenContext(context.Background(), path, expected)
	if err != nil {
		t.Fatal(err)
	}
	defer handle.Close()
	if err := os.WriteFile(path, changed, 0o600); err != nil {
		t.Fatal(err)
	}

	preview := make([]byte, 128)
	n, err := handle.Read(preview)
	if n != 0 || !errors.Is(err, ErrSourceChanged) {
		t.Fatalf("changed preview read = %d, %v; want ErrSourceChanged before bytes escape", n, err)
	}
	if err := os.WriteFile(path, original, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := handle.Seek(0, io.SeekStart); !errors.Is(err, ErrSourceChanged) {
		// Once unverified bytes were observed, the handle remains poisoned even
		// if the pathname is restored; callers must restart the operation.
		t.Fatalf("poisoned handle seek error = %v, want ErrSourceChanged", err)
	}
}

func TestBoundedFingerprintLayoutHasAbsoluteMemoryLimit(t *testing.T) {
	maximum := int64(maxFingerprintChunks) * maxFingerprintChunkBytes
	chunkSize, chunks, err := boundedFingerprintLayout(maximum)
	if err != nil {
		t.Fatal(err)
	}
	if chunkSize != maxFingerprintChunkBytes || chunks != maxFingerprintChunks {
		t.Fatalf("maximum layout = chunk %d count %d", chunkSize, chunks)
	}
	if _, _, err := boundedFingerprintLayout(maximum + 1); !errors.Is(err, ErrSourceVerificationLimit) {
		t.Fatalf("oversized layout error = %v, want ErrSourceVerificationLimit", err)
	}
}

func TestVerificationMemoryBoundsForSizeMatchesFingerprintLayout(t *testing.T) {
	for _, size := range []int64{0, 1, fingerprintChunkBytes, fingerprintChunkBytes + 1, 17 * fingerprintChunkBytes} {
		retained, buffer, err := VerificationMemoryBoundsForSize(size)
		if err != nil {
			t.Fatalf("size %d: %v", size, err)
		}
		chunkSize, chunkCount, err := boundedFingerprintLayout(size)
		if err != nil {
			t.Fatalf("layout size %d: %v", size, err)
		}
		if retained != int64(chunkCount)*sha256.Size || buffer != chunkSize {
			t.Fatalf("size %d bounds = retained %d buffer %d, want %d/%d",
				size, retained, buffer, int64(chunkCount)*sha256.Size, chunkSize)
		}
	}

	maximum := int64(maxFingerprintChunks) * maxFingerprintChunkBytes
	if _, _, err := VerificationMemoryBoundsForSize(maximum + 1); !errors.Is(err, ErrSourceVerificationLimit) {
		t.Fatalf("oversized preflight error = %v, want ErrSourceVerificationLimit", err)
	}
}

func TestFingerprintLimitRetainsTypedCauseWhenClassified(t *testing.T) {
	cause := fmt.Errorf("%w: fixture", ErrSourceVerificationLimit)
	err := classifyFingerprintError("fingerprint retained source", cause)
	if !errors.Is(err, ErrSourceChanged) {
		t.Fatalf("classified error = %v, want ErrSourceChanged", err)
	}
	if !errors.Is(err, ErrSourceVerificationLimit) {
		t.Fatalf("classified error = %v, want ErrSourceVerificationLimit", err)
	}
}

package replace

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/quarry/quarry-wails3/internal/regexutil"
)

type observedRegexSyncWriter struct {
	bytes.Buffer
	writeCalls  int
	syncCalls   int
	bytesAtSync int
}

func (w *observedRegexSyncWriter) Write(p []byte) (int, error) {
	w.writeCalls++
	return w.Buffer.Write(p)
}

func (w *observedRegexSyncWriter) Sync() error {
	w.syncCalls++
	w.bytesAtSync = w.Len()
	return nil
}

type shortRegexSyncWriter struct {
	writeCalls int
	syncCalls  int
	writeErr   error
	cancel     context.CancelFunc
}

func (w *shortRegexSyncWriter) Write(p []byte) (int, error) {
	w.writeCalls++
	if w.cancel != nil {
		w.cancel()
	}
	if len(p) == 0 {
		return 0, w.writeErr
	}
	return len(p) - 1, w.writeErr
}

func (w *shortRegexSyncWriter) Sync() error {
	w.syncCalls++
	return nil
}

type cancelOnSuccessfulRegexWrite struct {
	bytes.Buffer
	cancel    context.CancelFunc
	syncCalls int
}

func (w *cancelOnSuccessfulRegexWrite) Write(p []byte) (int, error) {
	n, err := w.Buffer.Write(p)
	w.cancel()
	return n, err
}

func (w *cancelOnSuccessfulRegexWrite) Sync() error {
	w.syncCalls++
	return nil
}

type cancelOnRegexSyncWriter struct {
	bytes.Buffer
	cancel    context.CancelFunc
	syncCalls int
	syncErr   error
}

func (w *cancelOnRegexSyncWriter) Sync() error {
	w.syncCalls++
	w.cancel()
	return w.syncErr
}

func TestDenseRegexOutputWritesAreBufferedAndFlushedBeforeSync(t *testing.T) {
	source := bytes.Repeat([]byte("a"), 1_000_000)
	want := bytes.Repeat([]byte("xy"), len(source))
	const writeBufferSize = 1024

	for _, batch := range []bool{false, true} {
		t.Run(regexVariantName(batch), func(t *testing.T) {
			writer := &observedRegexSyncWriter{}
			matches, conflicts, err := runRegexWriterVariant(t, context.Background(), source, writer, batch, []byte("xy"), RegexOptions{
				ChunkSize:       4 * 1024,
				MaxMatchWindow:  1,
				WriteBufferSize: writeBufferSize,
			})
			if err != nil {
				t.Fatal(err)
			}
			if matches != int64(len(source)) || conflicts != 0 {
				t.Fatalf("matches/conflicts = %d/%d, want %d/0", matches, conflicts, len(source))
			}
			if !bytes.Equal(writer.Bytes(), want) {
				t.Fatalf("output bytes = %d, want %d exact bytes", writer.Len(), len(want))
			}
			maximumWrites := (len(want)+writeBufferSize-1)/writeBufferSize + 2
			if writer.writeCalls > maximumWrites || writer.writeCalls >= len(source)/100 {
				t.Fatalf("destination writes = %d, maximum %d for %d dense matches", writer.writeCalls, maximumWrites, matches)
			}
			if writer.syncCalls != 1 {
				t.Fatalf("sync calls = %d, want 1", writer.syncCalls)
			}
			if writer.bytesAtSync != len(want) {
				t.Fatalf("bytes visible at Sync = %d, want %d", writer.bytesAtSync, len(want))
			}
		})
	}
}

func TestRegexOutputRejectsShortWritesWithoutSync(t *testing.T) {
	tests := []struct {
		name        string
		source      []byte
		replacement []byte
	}{
		{name: "buffer-flush", source: bytes.Repeat([]byte("a"), 64), replacement: []byte("x")},
		{name: "direct-large-write", source: []byte("a"), replacement: bytes.Repeat([]byte("x"), 32)},
	}
	for _, tt := range tests {
		for _, batch := range []bool{false, true} {
			t.Run(tt.name+"/"+regexVariantName(batch), func(t *testing.T) {
				writer := &shortRegexSyncWriter{}
				_, _, err := runRegexWriterVariant(t, context.Background(), tt.source, writer, batch, tt.replacement, RegexOptions{
					ChunkSize:       64,
					MaxMatchWindow:  1,
					WriteBufferSize: 8,
				})
				if !errors.Is(err, io.ErrShortWrite) {
					t.Fatalf("error = %v, want io.ErrShortWrite", err)
				}
				if writer.writeCalls == 0 {
					t.Fatal("short-write destination was not exercised")
				}
				if writer.syncCalls != 0 {
					t.Fatalf("sync calls = %d after short write", writer.syncCalls)
				}
			})
		}
	}
}

func TestRegexOutputPreservesWriterAndCancellationErrors(t *testing.T) {
	writeFailure := errors.New("regex destination write failed")
	syncFailure := errors.New("regex destination sync failed")
	source := bytes.Repeat([]byte("a"), 64)

	for _, batch := range []bool{false, true} {
		t.Run(regexVariantName(batch)+"/write-error-wins", func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			writer := &shortRegexSyncWriter{writeErr: writeFailure, cancel: cancel}
			_, _, err := runRegexWriterVariant(t, ctx, source, writer, batch, []byte("x"), RegexOptions{
				ChunkSize:       64,
				MaxMatchWindow:  1,
				WriteBufferSize: 8,
			})
			if !errors.Is(err, writeFailure) {
				t.Fatalf("error = %v, want concrete write failure", err)
			}
			if writer.syncCalls != 0 {
				t.Fatalf("sync calls = %d after write failure", writer.syncCalls)
			}
		})

		t.Run(regexVariantName(batch)+"/successful-write-cancellation", func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			writer := &cancelOnSuccessfulRegexWrite{cancel: cancel}
			_, _, err := runRegexWriterVariant(t, ctx, source, writer, batch, []byte("x"), RegexOptions{
				ChunkSize:       64,
				MaxMatchWindow:  1,
				WriteBufferSize: 8,
			})
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("error = %v, want context.Canceled", err)
			}
			if writer.syncCalls != 0 {
				t.Fatalf("sync calls = %d after cancellation", writer.syncCalls)
			}
		})

		t.Run(regexVariantName(batch)+"/sync-error-wins", func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			writer := &cancelOnRegexSyncWriter{cancel: cancel, syncErr: syncFailure}
			_, _, err := runRegexWriterVariant(t, ctx, []byte("a"), writer, batch, []byte("x"), RegexOptions{
				ChunkSize:       1,
				MaxMatchWindow:  1,
				WriteBufferSize: 8,
			})
			if !errors.Is(err, syncFailure) {
				t.Fatalf("error = %v, want concrete sync failure", err)
			}
			if writer.syncCalls != 1 {
				t.Fatalf("sync calls = %d, want 1", writer.syncCalls)
			}
		})
	}
}

func TestRegexWriteBufferBudgetIsValidatedBeforeOutput(t *testing.T) {
	for _, requested := range []int{-1, MaxRegexWriteBufferBytes + 1} {
		for _, batch := range []bool{false, true} {
			t.Run(regexVariantName(batch), func(t *testing.T) {
				writer := &observedRegexSyncWriter{}
				_, _, err := runRegexWriterVariant(t, context.Background(), []byte("a"), writer, batch, []byte("x"), RegexOptions{
					WriteBufferSize: requested,
				})
				if !errors.Is(err, regexutil.ErrRegexResourceLimit) {
					t.Fatalf("error = %v, want ErrRegexResourceLimit", err)
				}
				if writer.writeCalls != 0 || writer.syncCalls != 0 || writer.Len() != 0 {
					t.Fatalf("invalid buffer touched output: writes=%d syncs=%d bytes=%d", writer.writeCalls, writer.syncCalls, writer.Len())
				}
			})
		}
	}
}

func runRegexWriterVariant(t *testing.T, ctx context.Context, source []byte, dst syncWriter, batch bool, replacement []byte, opts RegexOptions) (int64, int64, error) {
	t.Helper()
	dir := t.TempDir()
	sourcePath := filepath.Join(dir, "source.txt")
	if err := os.WriteFile(sourcePath, source, 0o600); err != nil {
		t.Fatal(err)
	}
	file, err := os.Open(sourcePath)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	if batch {
		return replaceBatchRegexp(ctx, file, dst, []BatchRule{{Name: "dense", Find: []byte(`a`), Replace: replacement}}, opts)
	}
	matches, err := replaceRegexp(ctx, file, dst, []byte(`a`), replacement, opts)
	return matches, 0, err
}

func regexVariantName(batch bool) string {
	if batch {
		return "batch"
	}
	return "single"
}

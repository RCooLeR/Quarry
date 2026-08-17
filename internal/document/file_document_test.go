package document

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/quarry/quarry-wails3/internal/encodingx"
	"github.com/quarry/quarry-wails3/internal/lineindex"
)

func TestOpenFileDetectsUTF8WhenSampleEndsInsideRune(t *testing.T) {
	path := filepath.Join(t.TempDir(), "split-rune.txt")
	content := bytes.Repeat([]byte{'a'}, openSampleSize-1)
	content = append(content, []byte("€ tail")...)
	if err := os.WriteFile(path, content, 0o600); err != nil {
		t.Fatal(err)
	}
	doc, err := OpenFile(path)
	if err != nil {
		t.Fatal(err)
	}
	defer doc.Close()
	if got := doc.Metadata().Encoding; got != "UTF-8" {
		t.Fatalf("encoding = %q, want UTF-8", got)
	}
	if doc.Metadata().Binary {
		t.Fatal("valid UTF-8 prefix seam was classified as binary")
	}
}

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "quarry-document-test-home-*")
	if err != nil {
		panic(err)
	}
	_ = os.Setenv("QUARRY_HOME", dir)
	code := m.Run()
	_ = os.RemoveAll(dir)
	os.Exit(code)
}

func TestOpenFileDetectsTextMetadata(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "dump.sql")
	if err := os.WriteFile(path, []byte("CREATE TABLE users (\r\nid INT\r\n);\r\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	doc, err := OpenFile(path)
	if err != nil {
		t.Fatal(err)
	}
	defer doc.Close()

	meta := doc.Metadata()
	if meta.Encoding != "UTF-8" {
		t.Fatalf("encoding = %q, want UTF-8", meta.Encoding)
	}
	if meta.LineEnding != "CRLF" {
		t.Fatalf("line ending = %q, want CRLF", meta.LineEnding)
	}
	if meta.FileType != "SQL" {
		t.Fatalf("file type = %q, want SQL", meta.FileType)
	}
	if meta.Binary {
		t.Fatal("text SQL file detected as binary")
	}
}

func TestOpenFileDetectsBinaryMetadata(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "blob.dat")
	if err := os.WriteFile(path, []byte{0x00, 0x01, 0x02, 0x03}, 0o600); err != nil {
		t.Fatal(err)
	}

	doc, err := OpenFile(path)
	if err != nil {
		t.Fatal(err)
	}
	defer doc.Close()

	meta := doc.Metadata()
	if !meta.Binary {
		t.Fatal("binary sample was not detected")
	}
	if meta.FileType != "binary" {
		t.Fatalf("file type = %q, want binary", meta.FileType)
	}
}

func TestOpenFileDetectsCommonTextFileTypes(t *testing.T) {
	tests := []struct {
		name string
		body string
		want string
	}{
		{"settings.yaml", "server:\n  port: 8080\n", "YAML"},
		{"app.conf", "listen=127.0.0.1\n", "config"},
		{"main.py", "print('hello')\n", "Python"},
		{"unknown.weird", "ordinary text\nstill text\n", "text"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, tt.name)
			if err := os.WriteFile(path, []byte(tt.body), 0o600); err != nil {
				t.Fatal(err)
			}

			doc, err := OpenFile(path)
			if err != nil {
				t.Fatal(err)
			}
			defer doc.Close()

			meta := doc.Metadata()
			if meta.Binary {
				t.Fatal("text file detected as binary")
			}
			if meta.FileType != tt.want {
				t.Fatalf("file type = %q, want %q", meta.FileType, tt.want)
			}
		})
	}
}

func TestOpenFileDetectsConfigBasename(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "Dockerfile")
	if err := os.WriteFile(path, []byte("FROM scratch\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	doc, err := OpenFile(path)
	if err != nil {
		t.Fatal(err)
	}
	defer doc.Close()

	if got := doc.Metadata().FileType; got != "config" {
		t.Fatalf("file type = %q, want config", got)
	}
}

func TestReadAfterCloseReturnsDocumentClosed(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "closed.txt")
	if err := os.WriteFile(path, []byte("alpha\nbeta\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	doc, err := OpenFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := doc.ReadRange(0, 5); err != nil {
		t.Fatal(err)
	}
	if err := doc.Close(); err != nil {
		t.Fatal(err)
	}
	if err := doc.Close(); err != nil {
		t.Fatalf("second close should be idempotent: %v", err)
	}

	if _, err := doc.ReadRange(0, 5); !errors.Is(err, errDocumentClosed) {
		t.Fatalf("ReadRange after close err = %v, want errDocumentClosed", err)
	}
	buf := make([]byte, 5)
	if _, err := doc.ReadAt(buf, 0); !errors.Is(err, errDocumentClosed) {
		t.Fatalf("ReadAt after close err = %v, want errDocumentClosed", err)
	}
	if err := doc.StartIndexing(context.Background()); !errors.Is(err, errDocumentClosed) {
		t.Fatalf("StartIndexing after close err = %v, want errDocumentClosed", err)
	}
}

func TestReadRangeRejectsOverDefaultLimit(t *testing.T) {
	file, err := os.CreateTemp(t.TempDir(), "range-limit-*")
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()

	doc := &FileDocument{file: file, size: defaultReadRangeMaxBytes + 1}
	if _, err := doc.ReadRange(0, doc.size); !errors.Is(err, ErrReadRangeTooLarge) {
		t.Fatalf("ReadRange err = %v, want ErrReadRangeTooLarge", err)
	}
}

func TestStartIndexingProvidesApproxLineMapping(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "indexed.txt")
	if err := os.WriteFile(path, []byte("one\ntwo\nthree\nfour\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	doc, err := OpenFile(path)
	if err != nil {
		t.Fatal(err)
	}
	defer doc.Close()

	if err := doc.StartIndexing(context.Background()); err != nil {
		t.Fatal(err)
	}
	progress := doc.IndexProgress()
	if !progress.Done {
		t.Fatal("index should be done")
	}
	if progress.Lines != 4 {
		t.Fatalf("indexed lines = %d, want 4", progress.Lines)
	}

	line, ok := doc.ApproxOffsetToLine(8)
	if !ok {
		t.Fatal("expected line estimate")
	}
	if line < 2 || line > 3 {
		t.Fatalf("line = %d, want around 2-3", line)
	}
}

func TestExactLineToOffsetAfterIndexing(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "exact.txt")
	if err := os.WriteFile(path, []byte("one\ntwo\nthree\nfour\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	doc, err := OpenFile(path)
	if err != nil {
		t.Fatal(err)
	}
	defer doc.Close()

	if _, ok, err := doc.ExactLineToOffset(3); err != nil || ok {
		t.Fatalf("exact line before index = _, %v, %v; want not ready", ok, err)
	}

	if err := doc.StartIndexing(context.Background()); err != nil {
		t.Fatal(err)
	}
	offset, ok, err := doc.ExactLineToOffset(3)
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("expected exact line offset")
	}
	if offset != int64(len("one\ntwo\n")) {
		t.Fatalf("offset = %d, want %d", offset, len("one\ntwo\n"))
	}

	if _, ok, err := doc.ExactLineToOffset(99); err != nil || ok {
		t.Fatalf("line beyond EOF = _, %v, %v; want not found", ok, err)
	}
}

func TestLookupLineStartPreservesOffsetZeroAndPendingIndex(t *testing.T) {
	path := filepath.Join(t.TempDir(), "pending-line-index.txt")
	if err := os.WriteFile(path, []byte("one\ntwo\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	doc, err := OpenFile(path)
	if err != nil {
		t.Fatal(err)
	}
	defer doc.Close()

	lineOne, err := doc.LookupLineStart(context.Background(), 1, 0)
	if err != nil {
		t.Fatal(err)
	}
	if lineOne.Status != LineStartLookupExact || !lineOne.HasPosition || lineOne.Line != 1 || lineOne.Offset != 0 {
		t.Fatalf("line one lookup = %#v, want exact position at offset zero", lineOne)
	}

	pending, err := doc.LookupLineStart(context.Background(), 2, exactScanChunkSize)
	if err != nil {
		t.Fatal(err)
	}
	if pending.Status != LineStartLookupPending || pending.HasPosition {
		t.Fatalf("pending lookup = %#v, want unavailable incomplete index", pending)
	}
}

func TestLookupLineStartHardCapReturnsHonestHugeLineFallback(t *testing.T) {
	const budget = int64(exactScanChunkSize + 37)
	content := bytes.Repeat([]byte{'x'}, int(budget+1))
	content = append(content, '\n', 'z')
	path := filepath.Join(t.TempDir(), "huge-first-line.txt")
	if err := os.WriteFile(path, content, 0o600); err != nil {
		t.Fatal(err)
	}
	doc, err := OpenFile(path)
	if err != nil {
		t.Fatal(err)
	}
	defer doc.Close()
	if err := doc.StartIndexing(context.Background()); err != nil {
		t.Fatal(err)
	}

	var requestedBytes int64
	var readCalls int
	lookup, err := doc.lookupLineStart(context.Background(), 2, budget, func(p []byte, off int64) (int, error) {
		readCalls++
		requestedBytes += int64(len(p))
		return doc.ReadAt(p, off)
	})
	if err != nil {
		t.Fatal(err)
	}
	if lookup.Status != LineStartLookupLimited || !lookup.HasPosition || lookup.Line != 1 || lookup.Offset != 0 {
		t.Fatalf("lookup = %#v, want limited fallback to exact line 1 offset 0", lookup)
	}
	if requestedBytes != budget {
		t.Fatalf("requested source bytes = %d, want hard cap %d", requestedBytes, budget)
	}
	if readCalls != 2 {
		t.Fatalf("read calls = %d, want one full chunk plus one capped remainder", readCalls)
	}
}

func TestLookupLineStartCancellationReturnsFallbackWithoutAnotherRead(t *testing.T) {
	content := bytes.Repeat([]byte{'x'}, 2*exactScanChunkSize)
	content = append(content, '\n', 'z')
	path := filepath.Join(t.TempDir(), "cancel-exact-line.txt")
	if err := os.WriteFile(path, content, 0o600); err != nil {
		t.Fatal(err)
	}
	doc, err := OpenFile(path)
	if err != nil {
		t.Fatal(err)
	}
	defer doc.Close()
	if err := doc.StartIndexing(context.Background()); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var requestedBytes int64
	var readCalls int
	lookup, err := doc.lookupLineStart(ctx, 2, int64(len(content)), func(p []byte, off int64) (int, error) {
		readCalls++
		requestedBytes += int64(len(p))
		n, readErr := doc.ReadAt(p, off)
		cancel()
		return n, readErr
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want context.Canceled", err)
	}
	if lookup.Status != LineStartLookupLimited || !lookup.HasPosition || lookup.Line != 1 || lookup.Offset != 0 {
		t.Fatalf("lookup = %#v, want cancellation fallback to line 1 offset 0", lookup)
	}
	if readCalls != 1 || requestedBytes != exactScanChunkSize {
		t.Fatalf("reads = %d calls, %d bytes; want one %d-byte read", readCalls, requestedBytes, exactScanChunkSize)
	}
}

func TestLookupLineStartCapEdgeEOFAndProvenMissing(t *testing.T) {
	content := []byte("abc\n")
	path := filepath.Join(t.TempDir(), "line-at-eof.txt")
	if err := os.WriteFile(path, content, 0o600); err != nil {
		t.Fatal(err)
	}
	doc, err := OpenFile(path)
	if err != nil {
		t.Fatal(err)
	}
	defer doc.Close()
	if err := doc.StartIndexing(context.Background()); err != nil {
		t.Fatal(err)
	}

	limited, err := doc.LookupLineStart(context.Background(), 2, int64(len(content)-1))
	if err != nil {
		t.Fatal(err)
	}
	if limited.Status != LineStartLookupLimited || limited.Line != 1 || limited.Offset != 0 {
		t.Fatalf("below-edge lookup = %#v, want limited line 1 fallback", limited)
	}

	exact, err := doc.LookupLineStart(context.Background(), 2, int64(len(content)))
	if err != nil {
		t.Fatal(err)
	}
	if exact.Status != LineStartLookupExact || !exact.HasPosition || exact.Line != 2 || exact.Offset != int64(len(content)) {
		t.Fatalf("at-edge lookup = %#v, want exact empty line 2 at EOF", exact)
	}

	readCalls := 0
	missing, err := doc.lookupLineStart(context.Background(), 3, 0, func([]byte, int64) (int, error) {
		readCalls++
		return 0, errors.New("unexpected read")
	})
	if err != nil {
		t.Fatal(err)
	}
	if missing.Status != LineStartLookupAbsent || missing.HasPosition {
		t.Fatalf("missing lookup = %#v, want line proven absent", missing)
	}
	if readCalls != 0 {
		t.Fatalf("proven-missing lookup issued %d source reads", readCalls)
	}
}

func TestStartIndexingMarksEmptyDocumentDone(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "empty.txt")
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}

	doc, err := OpenFile(path)
	if err != nil {
		t.Fatal(err)
	}
	defer doc.Close()

	if err := doc.StartIndexing(context.Background()); err != nil {
		t.Fatal(err)
	}
	if progress := doc.IndexProgress(); !progress.Done {
		t.Fatalf("progress = %#v, want done", progress)
	}
}

func TestOpenFileLoadsCompletedIndexCache(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "cached.txt")
	if err := os.WriteFile(path, []byte("one\ntwo\nthree\nfour\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	doc, err := OpenFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := doc.StartIndexing(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := doc.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(indexCachePath(path)); err != nil {
		t.Fatalf("expected central index cache: %v", err)
	}
	if _, err := os.Stat(legacyIndexCachePath(path)); !os.IsNotExist(err) {
		t.Fatalf("legacy index sidecar was written or stat failed unexpectedly: %v", err)
	}

	cached, err := OpenFile(path)
	if err != nil {
		t.Fatal(err)
	}
	defer cached.Close()
	if progress := cached.IndexProgress(); !progress.Done {
		t.Fatalf("progress = %#v, want cached done index", progress)
	}
	offset, ok, err := cached.ExactLineToOffset(3)
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("expected exact cached line mapping")
	}
	if offset != int64(len("one\ntwo\n")) {
		t.Fatalf("offset = %d, want %d", offset, len("one\ntwo\n"))
	}
}

func TestOpenFileWithOptionsReportsMetadataAndCacheStages(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "progress-cached.txt")
	if err := os.WriteFile(path, []byte("one\ntwo\nthree\nfour\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	doc, err := OpenFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := doc.StartIndexing(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := doc.Close(); err != nil {
		t.Fatal(err)
	}

	var stages []OpenStage
	cached, err := OpenFileWithOptions(path, OpenOptions{
		Progress: func(progress OpenProgress) {
			stages = append(stages, progress.Stage)
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer cached.Close()
	for _, want := range []OpenStage{
		OpenStageOpening,
		OpenStageStat,
		OpenStageMetadataSample,
		OpenStageIndexCacheCheck,
		OpenStageIndexCacheLoaded,
		OpenStageMetadataDetected,
		OpenStageReady,
	} {
		if !openProgressStagesContain(stages, want) {
			t.Fatalf("stages = %#v, want %s", stages, want)
		}
	}
}

func TestOpenFileContextCanceledBeforeOpenDoesNotReportProgress(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "canceled.txt")
	if err := os.WriteFile(path, []byte("one\ntwo\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	var stages []OpenStage
	doc, err := OpenFileContextWithOptions(ctx, path, OpenOptions{
		Progress: func(progress OpenProgress) {
			stages = append(stages, progress.Stage)
		},
	})
	if doc != nil {
		_ = doc.Close()
		t.Fatal("expected no document when context is canceled before open")
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if len(stages) != 0 {
		t.Fatalf("stages = %#v, want no progress after pre-open cancellation", stages)
	}
}

func TestOpenFileContextCanceledDuringProgressClosesFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "cancel-during-progress.txt")
	if err := os.WriteFile(path, []byte("one\ntwo\nthree\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	var stages []OpenStage

	doc, err := OpenFileContextWithOptions(ctx, path, OpenOptions{
		Progress: func(progress OpenProgress) {
			stages = append(stages, progress.Stage)
			if progress.Stage == OpenStageStat {
				cancel()
			}
		},
	})
	if doc != nil {
		_ = doc.Close()
		t.Fatal("expected no document when context is canceled during open")
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if !openProgressStagesContain(stages, OpenStageOpening) || !openProgressStagesContain(stages, OpenStageStat) {
		t.Fatalf("stages = %#v, want opening and stat before cancellation", stages)
	}
	if openProgressStagesContain(stages, OpenStageMetadataSample) {
		t.Fatalf("stages = %#v, did not expect metadata sample after stat cancellation", stages)
	}

	// Windows cannot remove an open file; this proves the canceled open closed
	// its file handle before returning.
	if err := os.Remove(path); err != nil {
		t.Fatalf("remove after canceled open failed, likely leaked file handle: %v", err)
	}
}

func TestOpenFileDefersHugeIndexCacheHydrationUntilExplicitIndexing(t *testing.T) {
	oldPersist := persistentIndexCacheEnabled()
	SetPersistentIndexCache(true)
	defer SetPersistentIndexCache(oldPersist)

	dir := t.TempDir()
	path := filepath.Join(dir, "huge-cached.txt")
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR|os.O_TRUNC, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.WriteAt([]byte("one\ntwo\n"), 0); err != nil {
		_ = file.Close()
		t.Fatal(err)
	}
	hugeSize := int64(synchronousIndexCacheMaxSourceSize + 1)
	if err := file.Truncate(hugeSize); err != nil {
		_ = file.Close()
		t.Fatal(err)
	}
	info, err := file.Stat()
	if err != nil {
		_ = file.Close()
		t.Fatal(err)
	}
	sample, err := readSample(file, info.Size(), openSampleSize)
	if err != nil {
		_ = file.Close()
		t.Fatal(err)
	}
	sampleHash := computeIndexSampleHash(file, info.Size(), sample)
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}

	idx := lineindex.New(lineindex.EveryLinesForSize(info.Size()))
	if err := idx.RestoreSnapshot(lineindex.Snapshot{
		EveryLines: idx.EveryLines,
		Entries:    []lineindex.Entry{{Line: 1, Offset: 0}},
		Lines:      42,
		Bytes:      info.Size(),
		Done:       true,
	}); err != nil {
		t.Fatal(err)
	}
	if err := saveIndexCache(path, info.Size(), info.ModTime(), sampleHash, idx); err != nil {
		t.Fatal(err)
	}

	var stages []OpenStage
	doc, err := OpenFileWithOptions(path, OpenOptions{
		Progress: func(progress OpenProgress) {
			stages = append(stages, progress.Stage)
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer doc.Close()
	if !openProgressStagesContain(stages, OpenStageIndexCacheDeferred) {
		t.Fatalf("stages = %#v, want deferred cache stage", stages)
	}
	if openProgressStagesContain(stages, OpenStageIndexCacheCheck) {
		t.Fatalf("stages = %#v, did not expect synchronous cache check for huge file", stages)
	}
	if progress := doc.IndexProgress(); progress.Done {
		t.Fatalf("progress = %#v, want huge-file open to defer cache hydration", progress)
	}
	if doc.indexSampleHash != "" {
		t.Fatal("huge-file open computed an index-cache sample hash before explicit indexing")
	}
	if err := doc.StartIndexing(context.Background()); err != nil {
		t.Fatal(err)
	}
	if progress := doc.IndexProgress(); !progress.Done || progress.Lines != 42 {
		t.Fatalf("progress = %#v, want explicit indexing to hydrate cached done index", progress)
	}
	if doc.indexSampleHash == "" {
		t.Fatal("explicit indexing did not compute the deferred index-cache sample hash")
	}
}

func openProgressStagesContain(stages []OpenStage, want OpenStage) bool {
	for _, got := range stages {
		if got == want {
			return true
		}
	}
	return false
}

func TestOpenFileIgnoresLegacySourceAdjacentIndexCache(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "legacy-index.txt")
	body := []byte("one\ntwo\nthree\nfour\n")
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatal(err)
	}
	legacyPayload := []byte(`{"version":2,"path":"retired-source-adjacent-format"}`)
	if err := os.WriteFile(legacyIndexCachePath(path), legacyPayload, 0o600); err != nil {
		t.Fatal(err)
	}

	doc, err := OpenFile(path)
	if err != nil {
		t.Fatal(err)
	}
	defer doc.Close()
	if progress := doc.IndexProgress(); progress.Done {
		t.Fatalf("progress = %#v, legacy source-adjacent cache must be ignored", progress)
	}
}

func TestOpenFileIgnoresStaleIndexCache(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "stale.txt")
	if err := os.WriteFile(path, []byte("one\ntwo\nthree\nfour\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	doc, err := OpenFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := doc.StartIndexing(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := doc.Close(); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(path, []byte("changed\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	stale, err := OpenFile(path)
	if err != nil {
		t.Fatal(err)
	}
	defer stale.Close()
	if progress := stale.IndexProgress(); progress.Done {
		t.Fatalf("progress = %#v, want stale cache ignored", progress)
	}
}

func TestInspectExternalModificationDetectsChangedFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "changed.txt")
	if err := os.WriteFile(path, []byte("alpha\r\nbeta\r\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	doc, err := OpenFile(path)
	if err != nil {
		t.Fatal(err)
	}
	defer doc.Close()

	if err := os.WriteFile(path, []byte("alpha\nbeta\ngamma\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	change, changed, err := doc.InspectExternalModification()
	if err != nil {
		t.Fatal(err)
	}
	if !changed {
		t.Fatal("expected changed file to be detected")
	}
	if change.Original.Size == change.Current.Size {
		t.Fatal("expected size change to be reported")
	}
	if change.Metadata.LineEnding != "LF" {
		t.Fatalf("line ending = %q, want LF", change.Metadata.LineEnding)
	}
}

func TestInspectExternalModificationReturnsFalseWhenUnchanged(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "same.txt")
	if err := os.WriteFile(path, []byte("alpha\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	doc, err := OpenFile(path)
	if err != nil {
		t.Fatal(err)
	}
	defer doc.Close()

	_, changed, err := doc.InspectExternalModification()
	if err != nil {
		t.Fatal(err)
	}
	if changed {
		t.Fatal("expected unchanged file to stay unchanged")
	}
}

func TestInspectExternalModificationDetectsSameMetadataPathReplacement(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "source.txt")
	held := filepath.Join(dir, "source.held")
	original := []byte("alpha\n")
	replacement := []byte("bravo\n")
	if err := os.WriteFile(path, original, 0o600); err != nil {
		t.Fatal(err)
	}

	doc, err := OpenFile(path)
	if err != nil {
		t.Fatal(err)
	}
	defer doc.Close()
	opened, err := doc.OpenedFileInfo()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(path, held); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, replacement, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, opened.ModTime(), opened.ModTime()); err != nil {
		t.Fatal(err)
	}

	change, changed, err := doc.InspectExternalModification()
	if err != nil {
		t.Fatal(err)
	}
	if !changed {
		t.Fatal("same-size, same-mtime pathname replacement was reported unchanged")
	}
	if change.Original.Size != change.Current.Size || !change.Original.ModTime.Equal(change.Current.ModTime) {
		t.Fatalf("replacement metadata = %#v, want original metadata %#v", change.Current, change.Original)
	}
}

func TestOpenFileDoesNotTreatUTF16BOMAsBinary(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "utf16.sql")
	data := []byte{0xFF, 0xFE, 'S', 0x00, 'E', 0x00, 'L', 0x00, 'E', 0x00, 'C', 0x00, 'T', 0x00}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}

	doc, err := OpenFile(path)
	if err != nil {
		t.Fatal(err)
	}
	defer doc.Close()

	meta := doc.Metadata()
	if meta.Encoding != "UTF-16LE" {
		t.Fatalf("encoding = %q, want UTF-16LE", meta.Encoding)
	}
	if meta.Binary {
		t.Fatal("UTF-16 text with BOM detected as binary")
	}
}

func TestOpenFileDoesNotTreatUTF16NoBOMAsBinary(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "utf16-no-bom.sql")
	data, err := encodingx.EncodeString("UTF-16LE", "SELECT 1;\n")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}

	doc, err := OpenFile(path)
	if err != nil {
		t.Fatal(err)
	}
	defer doc.Close()

	meta := doc.Metadata()
	if meta.Encoding != "UTF-16LE" {
		t.Fatalf("encoding = %q, want UTF-16LE", meta.Encoding)
	}
	if meta.Binary {
		t.Fatal("UTF-16 text without BOM detected as binary")
	}
}

func TestOpenFileDetectsWindows1251Metadata(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "russian.txt")
	data, err := encodingx.EncodeString("Windows-1251", "Привет\n")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}

	doc, err := OpenFile(path)
	if err != nil {
		t.Fatal(err)
	}
	defer doc.Close()

	meta := doc.Metadata()
	if meta.Encoding != "Windows-1251" {
		t.Fatalf("encoding = %q, want Windows-1251", meta.Encoding)
	}
	if meta.Binary {
		t.Fatal("Windows-1251 text detected as binary")
	}
}

func TestVisibleLinesFromOffset(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "lines.log")
	if err := os.WriteFile(path, []byte("alpha\nbravo\r\ncharlie\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	doc, err := OpenFile(path)
	if err != nil {
		t.Fatal(err)
	}
	defer doc.Close()

	lines, err := doc.VisibleLinesFromOffset(0, 3, VisibleLineOptions{MaxBytes: 64, MaxLineBytes: 32})
	if err != nil {
		t.Fatal(err)
	}
	if len(lines) != 3 {
		t.Fatalf("len(lines) = %d, want 3", len(lines))
	}

	tests := []struct {
		i      int
		text   string
		offset int64
	}{
		{0, "alpha", 0},
		{1, "bravo", 6},
		{2, "charlie", 13},
	}
	for _, tt := range tests {
		if lines[tt.i].Text != tt.text {
			t.Fatalf("line %d text = %q, want %q", tt.i, lines[tt.i].Text, tt.text)
		}
		if lines[tt.i].Offset != tt.offset {
			t.Fatalf("line %d offset = %d, want %d", tt.i, lines[tt.i].Offset, tt.offset)
		}
	}
}

func TestVisibleLinesDecodesUTF16LE(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "utf16-lines.txt")
	body, err := encodingx.EncodeString("UTF-16LE", "alpha\r\nbeta\r\n")
	if err != nil {
		t.Fatal(err)
	}
	body = append([]byte{0xFF, 0xFE}, body...)
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatal(err)
	}

	doc, err := OpenFile(path)
	if err != nil {
		t.Fatal(err)
	}
	defer doc.Close()

	lines, err := doc.VisibleLinesFromOffset(0, 2, VisibleLineOptions{MaxBytes: 64, MaxLineBytes: 32})
	if err != nil {
		t.Fatal(err)
	}
	if len(lines) != 2 {
		t.Fatalf("len(lines) = %d, want 2", len(lines))
	}
	if lines[0].Text != "alpha" || lines[1].Text != "beta" {
		t.Fatalf("lines = %#v, want decoded UTF-16 text", lines)
	}
}

func TestVisibleLinesDecodesWindows1251(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "cp1251-lines.txt")
	body, err := encodingx.EncodeString("Windows-1251", "Привет\r\nмир\r\n")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatal(err)
	}

	doc, err := OpenFile(path)
	if err != nil {
		t.Fatal(err)
	}
	defer doc.Close()

	lines, err := doc.VisibleLinesFromOffset(0, 2, VisibleLineOptions{MaxBytes: 64, MaxLineBytes: 32})
	if err != nil {
		t.Fatal(err)
	}
	if len(lines) != 2 {
		t.Fatalf("len(lines) = %d, want 2", len(lines))
	}
	if lines[0].Text != "Привет" || lines[1].Text != "мир" {
		t.Fatalf("lines = %#v, want decoded Windows-1251 text", lines)
	}
}

func TestVisiblePageNextOffset(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "page.log")
	if err := os.WriteFile(path, []byte("one\ntwo\nthree\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	doc, err := OpenFile(path)
	if err != nil {
		t.Fatal(err)
	}
	defer doc.Close()

	page, err := doc.VisiblePageFromOffset(0, 2, VisibleLineOptions{MaxBytes: 64, MaxLineBytes: 32})
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Lines) != 2 {
		t.Fatalf("len(lines) = %d, want 2", len(page.Lines))
	}
	if page.StartOffset != 0 {
		t.Fatalf("start offset = %d, want 0", page.StartOffset)
	}
	if page.NextOffset != 8 {
		t.Fatalf("next offset = %d, want 8", page.NextOffset)
	}

	next, err := doc.VisiblePageFromOffset(page.NextOffset, 2, VisibleLineOptions{MaxBytes: 64, MaxLineBytes: 32, FirstLineNumber: 3})
	if err != nil {
		t.Fatal(err)
	}
	if len(next.Lines) != 1 || next.Lines[0].Text != "three" {
		t.Fatalf("next page lines = %#v, want three", next.Lines)
	}
}

func TestVisibleLinesTruncatesLongLine(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "long.log")
	if err := os.WriteFile(path, []byte("abcdefgh\nnext\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	doc, err := OpenFile(path)
	if err != nil {
		t.Fatal(err)
	}
	defer doc.Close()

	lines, err := doc.VisibleLinesFromOffset(0, 3, VisibleLineOptions{MaxBytes: 64, MaxLineBytes: 4})
	if err != nil {
		t.Fatal(err)
	}
	if len(lines) != 3 {
		t.Fatalf("len(lines) = %d, want 3", len(lines))
	}
	if lines[0].Text != "abcd" || !lines[0].Truncated {
		t.Fatalf("first line = %#v, want truncated abcd", lines[0])
	}
	if lines[1].Text != "efgh" || lines[1].Truncated {
		t.Fatalf("second line = %#v, want continuation efgh", lines[1])
	}
	if lines[2].Text != "next" || lines[2].Truncated {
		t.Fatalf("third line = %#v, want untruncated next", lines[2])
	}
}

func TestVisiblePageSlicesLongLineBeforeNewline(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "long-before-newline.log")
	if err := os.WriteFile(path, []byte("abcdefghijkl\nnext\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	doc, err := OpenFile(path)
	if err != nil {
		t.Fatal(err)
	}
	defer doc.Close()

	page, err := doc.VisiblePageFromOffset(0, 3, VisibleLineOptions{MaxBytes: 64, MaxLineBytes: 4})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := len(page.Lines), 3; got != want {
		t.Fatalf("len(lines) = %d, want %d", got, want)
	}
	for i, want := range []string{"abcd", "efgh", "ijkl"} {
		if got := page.Lines[i].Text; got != want {
			t.Fatalf("line %d text = %q, want %q", i, got, want)
		}
	}
	if !page.Lines[0].HasRightHidden || !page.Lines[1].HasLeftHidden || !page.Lines[1].HasRightHidden || !page.Lines[2].HasLeftHidden || page.Lines[2].HasRightHidden {
		t.Fatalf("hidden markers = %#v", page.Lines)
	}
	if got, want := page.NextOffset, int64(len("abcdefghijkl\n")); got != want {
		t.Fatalf("next offset = %d, want %d", got, want)
	}

	next, err := doc.VisiblePageFromOffset(page.NextOffset, 1, VisibleLineOptions{MaxBytes: 64, MaxLineBytes: 4, FirstLineNumber: 2})
	if err != nil {
		t.Fatal(err)
	}
	if len(next.Lines) != 1 || next.Lines[0].Text != "next" {
		t.Fatalf("next page = %#v, want next line", next.Lines)
	}
}

func TestVisiblePageContinuesLongLineWhenPageFillsMidLine(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "long-mid-page.log")
	if err := os.WriteFile(path, []byte("abcdefghijkl\nnext\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	doc, err := OpenFile(path)
	if err != nil {
		t.Fatal(err)
	}
	defer doc.Close()

	page, err := doc.VisiblePageFromOffset(0, 2, VisibleLineOptions{MaxBytes: 64, MaxLineBytes: 4})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := page.NextOffset, int64(len("abcdefgh")); got != want {
		t.Fatalf("next offset = %d, want %d", got, want)
	}

	next, err := doc.VisiblePageFromOffset(page.NextOffset, 2, VisibleLineOptions{MaxBytes: 64, MaxLineBytes: 4})
	if err != nil {
		t.Fatal(err)
	}
	if len(next.Lines) != 2 {
		t.Fatalf("len(next lines) = %d, want 2", len(next.Lines))
	}
	if next.Lines[0].Text != "ijkl" || next.Lines[1].Text != "next" {
		t.Fatalf("next lines = %#v, want remaining long-line slice then next", next.Lines)
	}
	if !next.Lines[0].HasLeftHidden || next.Lines[0].HasRightHidden {
		t.Fatalf("continued slice = %#v, want left-hidden continuation only", next.Lines[0])
	}
	if next.Lines[1].HasLeftHidden {
		t.Fatalf("next logical line = %#v, want no left-hidden continuation marker", next.Lines[1])
	}
}

func TestVisibleLinesHorizontalSlice(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "wide.log")
	if err := os.WriteFile(path, []byte("0123456789abcdef\nnext\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	doc, err := OpenFile(path)
	if err != nil {
		t.Fatal(err)
	}
	defer doc.Close()

	lines, err := doc.VisibleLinesFromOffset(0, 2, VisibleLineOptions{
		MaxBytes:             16,
		MaxLineBytes:         4,
		HorizontalByteOffset: 6,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(lines) != 1 {
		t.Fatalf("len(lines) = %d, want 1", len(lines))
	}
	if lines[0].Text != "6789" {
		t.Fatalf("text = %q, want 6789", lines[0].Text)
	}
	if !lines[0].HasLeftHidden || !lines[0].HasRightHidden {
		t.Fatalf("line = %#v, want hidden content on both sides", lines[0])
	}
	if !lines[0].Truncated {
		t.Fatalf("line = %#v, want truncated", lines[0])
	}
}

func TestVisibleLinesHorizontalSlicePastShortLine(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "short.log")
	if err := os.WriteFile(path, []byte("abc\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	doc, err := OpenFile(path)
	if err != nil {
		t.Fatal(err)
	}
	defer doc.Close()

	lines, err := doc.VisibleLinesFromOffset(0, 1, VisibleLineOptions{
		MaxBytes:             16,
		MaxLineBytes:         4,
		HorizontalByteOffset: 8,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(lines) != 1 {
		t.Fatalf("len(lines) = %d, want 1", len(lines))
	}
	if lines[0].Text != "" {
		t.Fatalf("text = %q, want empty horizontal slice", lines[0].Text)
	}
	if !lines[0].HasLeftHidden || lines[0].HasRightHidden {
		t.Fatalf("line = %#v, want only left-hidden content", lines[0])
	}
}

func TestVisibleLinesMarksReadWindowTruncation(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "continued.log")
	if err := os.WriteFile(path, []byte("abcdefghijk"), 0o600); err != nil {
		t.Fatal(err)
	}

	doc, err := OpenFile(path)
	if err != nil {
		t.Fatal(err)
	}
	defer doc.Close()

	lines, err := doc.VisibleLinesFromOffset(0, 1, VisibleLineOptions{MaxBytes: 4, MaxLineBytes: 8})
	if err != nil {
		t.Fatal(err)
	}
	if len(lines) != 1 {
		t.Fatalf("len(lines) = %d, want 1", len(lines))
	}
	if !lines[0].Truncated {
		t.Fatalf("line = %#v, want truncated because read window ended", lines[0])
	}
}

func TestVisibleLinesFillViewportWithBoundedLongLineSlices(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "huge-insert.sql")
	body := []byte("INSERT INTO logs VALUES (" + strings.Repeat("'payload',", 64))
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatal(err)
	}

	doc, err := OpenFile(path)
	if err != nil {
		t.Fatal(err)
	}
	defer doc.Close()

	lines, err := doc.VisibleLinesFromOffset(0, 4, VisibleLineOptions{MaxBytes: 64, MaxLineBytes: 16})
	if err != nil {
		t.Fatal(err)
	}
	if len(lines) != 4 {
		t.Fatalf("len(lines) = %d, want bounded long-line slices to fill viewport rows", len(lines))
	}
	if lines[0].HasLeftHidden {
		t.Fatalf("first slice = %#v, want no left-hidden marker", lines[0])
	}
	for i := 1; i < len(lines); i++ {
		if !lines[i].HasLeftHidden {
			t.Fatalf("slice %d = %#v, want left-hidden continuation marker", i, lines[i])
		}
		if lines[i].LineNumber != lines[0].LineNumber {
			t.Fatalf("slice %d line number = %d, want repeated logical line %d", i, lines[i].LineNumber, lines[0].LineNumber)
		}
		if lines[i].DisplayOffset <= lines[i-1].DisplayOffset {
			t.Fatalf("slice %d display offset = %d, previous = %d", i, lines[i].DisplayOffset, lines[i-1].DisplayOffset)
		}
	}
}

func TestVisibleLinesMarkRenderLimitExceeded(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "too-long.log")
	if err := os.WriteFile(path, []byte("abcdefghijklmnop\nnext\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	doc, err := OpenFile(path)
	if err != nil {
		t.Fatal(err)
	}
	defer doc.Close()

	lines, err := doc.VisibleLinesFromOffset(0, 5, VisibleLineOptions{
		MaxBytes:           64,
		MaxLineBytes:       4,
		LongLineLimitBytes: 8,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(lines) != 5 {
		t.Fatalf("len(lines) = %d, want 5", len(lines))
	}
	for i := 0; i < 4; i++ {
		if !lines[i].ExceedsRenderLimit || lines[i].RenderLimitBytes != 8 {
			t.Fatalf("long-line slice %d = %#v, want render-limit warning", i, lines[i])
		}
	}
	if lines[4].Text != "next" || lines[4].ExceedsRenderLimit {
		t.Fatalf("next line = %#v, want no render-limit warning", lines[4])
	}
}

func TestVisibleLinesMarkRenderLimitExceededForNonSQLPluginFileShapes(t *testing.T) {
	tests := []struct {
		name     string
		fileName string
		prefix   string
		wantType string
	}{
		{name: "log", fileName: "server.log", prefix: "2026-05-28T10:00:00Z ERROR ", wantType: "log"},
		{name: "csv", fileName: "users.csv", prefix: "id,name,notes,", wantType: "CSV"},
		{name: "source", fileName: "worker.go", prefix: "package main; // ", wantType: "Go"},
		{name: "yaml", fileName: "config.yaml", prefix: "payload: ", wantType: "YAML"},
		{name: "markup", fileName: "page.html", prefix: "<div data-long=\"", wantType: "web"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, tt.fileName)
			longLine := tt.prefix + strings.Repeat("x", 8192) + "\nnext\n"
			if err := os.WriteFile(path, []byte(longLine), 0o600); err != nil {
				t.Fatal(err)
			}

			doc, err := OpenFile(path)
			if err != nil {
				t.Fatal(err)
			}
			defer doc.Close()

			if got := doc.Metadata().FileType; got != tt.wantType {
				t.Fatalf("file type = %q, want %q", got, tt.wantType)
			}

			lines, err := doc.VisibleLinesFromOffset(0, 1, VisibleLineOptions{
				MaxBytes:           256,
				MaxLineBytes:       64,
				LongLineLimitBytes: 1024,
			})
			if err != nil {
				t.Fatal(err)
			}
			if len(lines) != 1 {
				t.Fatalf("len(lines) = %d, want 1", len(lines))
			}
			line := lines[0]
			if len(line.Text) != 64 {
				t.Fatalf("visible text length = %d, want bounded 64", len(line.Text))
			}
			if !line.Truncated || !line.HasRightHidden || line.HasLeftHidden {
				t.Fatalf("line = %#v, want right-hidden truncated first slice", line)
			}
			if !line.ExceedsRenderLimit || line.RenderLimitBytes != 1024 {
				t.Fatalf("line = %#v, want render-limit warning", line)
			}
		})
	}
}

func TestVisibleLinesMarkRenderLimitExceededFromMiddleOfLine(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "middle-long.log")
	if err := os.WriteFile(path, []byte("abcdefghijklmnop\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	doc, err := OpenFile(path)
	if err != nil {
		t.Fatal(err)
	}
	defer doc.Close()

	lines, err := doc.VisibleLinesFromOffset(6, 1, VisibleLineOptions{
		MaxBytes:           8,
		MaxLineBytes:       4,
		LongLineLimitBytes: 8,
		FirstLineNumber:    1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(lines) != 1 {
		t.Fatalf("len(lines) = %d, want 1", len(lines))
	}
	if !lines[0].ExceedsRenderLimit {
		t.Fatalf("line = %#v, want render-limit warning from mid-line slice", lines[0])
	}
}

func TestVisibleLinesDoNotMarkShortLineAsRenderLimitExceeded(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "short-render.log")
	if err := os.WriteFile(path, []byte("abcdefg"), 0o600); err != nil {
		t.Fatal(err)
	}

	doc, err := OpenFile(path)
	if err != nil {
		t.Fatal(err)
	}
	defer doc.Close()

	lines, err := doc.VisibleLinesFromOffset(0, 1, VisibleLineOptions{
		MaxBytes:           4,
		MaxLineBytes:       4,
		LongLineLimitBytes: 8,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(lines) != 1 {
		t.Fatalf("len(lines) = %d, want 1", len(lines))
	}
	if lines[0].ExceedsRenderLimit {
		t.Fatalf("line = %#v, want no render-limit warning", lines[0])
	}
}

func TestExactOffsetToLineRequiresCompletedIndex(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "exact-offset-pending.log")
	if err := os.WriteFile(path, []byte("one\ntwo\nthree\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	doc, err := OpenFile(path)
	if err != nil {
		t.Fatal(err)
	}
	defer doc.Close()

	if _, ok, err := doc.ExactOffsetToLine(5); err != nil || ok {
		t.Fatalf("ExactOffsetToLine before completed index = ok:%v err:%v, want ok:false err:nil", ok, err)
	}
}

func TestExactOffsetToLineAfterIndex(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "exact-offset.log")
	content := []byte("one\ntwo\nthree\nfour\n")
	if err := os.WriteFile(path, content, 0o600); err != nil {
		t.Fatal(err)
	}

	doc, err := OpenFile(path)
	if err != nil {
		t.Fatal(err)
	}
	defer doc.Close()
	doc.idx.EveryLines = 2

	if err := doc.StartIndexing(context.Background()); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		offset int64
		line   int64
	}{
		{offset: 0, line: 1},
		{offset: 3, line: 1},
		{offset: 4, line: 2},
		{offset: 8, line: 3},
		{offset: int64(len(content) - 1), line: 4},
		{offset: int64(len(content)), line: 5},
	}
	for _, tc := range tests {
		line, ok, err := doc.ExactOffsetToLine(tc.offset)
		if err != nil {
			t.Fatalf("offset %d: %v", tc.offset, err)
		}
		if !ok {
			t.Fatalf("offset %d: expected exact line", tc.offset)
		}
		if line != tc.line {
			t.Fatalf("offset %d: line = %d, want %d", tc.offset, line, tc.line)
		}
	}
}

func TestExactOffsetToLineWithinRejectsBeforeReadAndCapsExactIO(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "exact-offset-bounded.log")
	content := []byte(strings.Repeat("x", 128) + "\n")
	if err := os.WriteFile(path, content, 0o600); err != nil {
		t.Fatal(err)
	}

	doc, err := OpenFile(path)
	if err != nil {
		t.Fatal(err)
	}
	defer doc.Close()
	doc.idx.EveryLines = 4096
	if err := doc.StartIndexing(context.Background()); err != nil {
		t.Fatal(err)
	}

	const offset int64 = 96
	readCalls := 0
	line, ok, err := doc.exactOffsetToLine(offset, 99, func([]byte, int64) (int, error) {
		readCalls++
		return 0, errors.New("bounded lookup issued unexpected source I/O")
	})
	if err != nil || ok || line != 0 {
		t.Fatalf("over-budget lookup = line:%d ok:%v err:%v, want 0 false nil", line, ok, err)
	}
	if readCalls != 0 {
		t.Fatalf("over-budget lookup issued %d reads, want zero", readCalls)
	}

	readBytes := 0
	line, ok, err = doc.exactOffsetToLine(offset, 100, func(p []byte, off int64) (int, error) {
		readBytes += len(p)
		return doc.ReadAt(p, off)
	})
	if err != nil || !ok || line != 1 {
		t.Fatalf("at-budget lookup = line:%d ok:%v err:%v, want 1 true nil", line, ok, err)
	}
	if readBytes != 100 {
		t.Fatalf("at-budget lookup requested %d bytes, want the exact 100-byte scan plus lookahead budget", readBytes)
	}

	if _, _, err := doc.ExactOffsetToLineWithin(offset, -1); err == nil {
		t.Fatal("negative exact offset scan budget accepted")
	}
}

func TestExactOffsetToLineStoresForwardMemo(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "exact-offset-memo.log")
	content := []byte("one\ntwo\nthree\nfour\nfive\n")
	if err := os.WriteFile(path, content, 0o600); err != nil {
		t.Fatal(err)
	}

	doc, err := OpenFile(path)
	if err != nil {
		t.Fatal(err)
	}
	defer doc.Close()
	doc.idx.EveryLines = 4

	if err := doc.StartIndexing(context.Background()); err != nil {
		t.Fatal(err)
	}

	offset := int64(len("one\ntwo\nth"))
	line, ok, err := doc.ExactOffsetToLine(offset)
	if err != nil {
		t.Fatal(err)
	}
	if !ok || line != 3 {
		t.Fatalf("line = %d ok=%v, want 3 true", line, ok)
	}

	memo, ok := doc.exactOffsetMemoFor(int64(len("one\ntwo\nthree\n")), lineindex.Entry{Line: 1, Offset: 0})
	if !ok {
		t.Fatal("expected exact offset memo to be reusable for a forward lookup")
	}
	if memo.Offset != offset || memo.Line != 3 {
		t.Fatalf("memo = %#v, want offset %d line 3", memo, offset)
	}

	line, ok, err = doc.ExactOffsetToLine(int64(len("one\ntwo\nthree\nfo")))
	if err != nil {
		t.Fatal(err)
	}
	if !ok || line != 4 {
		t.Fatalf("forward line = %d ok=%v, want 4 true", line, ok)
	}
}

func TestExactOffsetToLineAfterIndexUTF16LE(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "exact-offset-utf16.log")
	body, err := encodingx.EncodeString("UTF-16LE", "one\r\ntwo\r\nthree\r\n")
	if err != nil {
		t.Fatal(err)
	}
	content := append([]byte{0xFF, 0xFE}, body...)
	if err := os.WriteFile(path, content, 0o600); err != nil {
		t.Fatal(err)
	}

	doc, err := OpenFile(path)
	if err != nil {
		t.Fatal(err)
	}
	defer doc.Close()
	doc.idx.EveryLines = 2

	if err := doc.StartIndexing(context.Background()); err != nil {
		t.Fatal(err)
	}

	line, ok, err := doc.ExactOffsetToLine(int64(len(content)))
	if err != nil {
		t.Fatal(err)
	}
	if !ok || line != 4 {
		t.Fatalf("line = %d ok=%v, want 4 true", line, ok)
	}

	offset, ok, err := doc.ExactLineToOffset(3)
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("expected exact line 3 offset")
	}
	expected := int64(2 + len([]byte{0x6f, 0x00, 0x6e, 0x00, 0x65, 0x00, 0x0d, 0x00, 0x0a, 0x00}) + len([]byte{0x74, 0x00, 0x77, 0x00, 0x6f, 0x00, 0x0d, 0x00, 0x0a, 0x00}))
	if offset != expected {
		t.Fatalf("offset = %d, want %d", offset, expected)
	}
}

func TestExactLineToOffsetStoresForwardMemo(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "exact-line-memo.log")
	content := []byte("one\ntwo\nthree\nfour\nfive\n")
	if err := os.WriteFile(path, content, 0o600); err != nil {
		t.Fatal(err)
	}

	doc, err := OpenFile(path)
	if err != nil {
		t.Fatal(err)
	}
	defer doc.Close()
	doc.idx.EveryLines = 4

	if err := doc.StartIndexing(context.Background()); err != nil {
		t.Fatal(err)
	}

	offset, ok, err := doc.ExactLineToOffset(3)
	if err != nil {
		t.Fatal(err)
	}
	if !ok || offset != int64(len("one\ntwo\n")) {
		t.Fatalf("offset = %d ok=%v, want line 3 start", offset, ok)
	}

	memo, ok := doc.exactLineStartMemoFor(5, lineindex.Entry{Line: 1, Offset: 0})
	if !ok {
		t.Fatal("expected exact line-start memo to be reusable for a forward lookup")
	}
	if memo.Line != 3 || memo.Offset != offset {
		t.Fatalf("memo = %#v, want line 3 offset %d", memo, offset)
	}

	offset, ok, err = doc.ExactLineToOffset(5)
	if err != nil {
		t.Fatal(err)
	}
	if !ok || offset != int64(len("one\ntwo\nthree\nfour\n")) {
		t.Fatalf("forward offset = %d ok=%v, want line 5 start", offset, ok)
	}
}

func TestSeedPriorityIndexAtViewportStart(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "priority-start.log")
	if err := os.WriteFile(path, []byte("one\ntwo\nthree\nfour\nfive\nsix\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	doc, err := OpenFile(path)
	if err != nil {
		t.Fatal(err)
	}
	defer doc.Close()
	doc.idx.EveryLines = 2

	doc.seedPriorityIndex(0)

	offset, ok := doc.ApproxLineToOffset(5)
	if !ok {
		t.Fatal("expected priority-seeded line anchor")
	}
	if offset != int64(len("one\ntwo\nthree\nfour\n")) {
		t.Fatalf("offset = %d, want %d", offset, len("one\ntwo\nthree\nfour\n"))
	}
}

func TestRequestPriorityIndexSeedsThroughCoalescingWorker(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "priority-worker.log")
	if err := os.WriteFile(path, []byte("one\ntwo\nthree\nfour\nfive\nsix\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	doc, err := OpenFile(path)
	if err != nil {
		t.Fatal(err)
	}
	defer doc.Close()
	doc.idx.EveryLines = 2

	doc.RequestPriorityIndex(0)

	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		offset, ok := doc.ApproxLineToOffset(5)
		if ok && offset == int64(len("one\ntwo\nthree\nfour\n")) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("expected priority worker to seed viewport anchors")
}

func TestCloseStopsAndWaitsForPriorityIndexWorker(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "priority-close.log")
	content := []byte(strings.Repeat("alpha\nbeta\ngamma\ndelta\n", 4096))
	if err := os.WriteFile(path, content, 0o600); err != nil {
		t.Fatal(err)
	}

	doc, err := OpenFile(path)
	if err != nil {
		t.Fatal(err)
	}
	doc.idx.EveryLines = 2
	doc.RequestPriorityIndex(int64(len(content) / 2))

	closed := make(chan error, 1)
	go func() {
		closed <- doc.Close()
	}()
	select {
	case err := <-closed:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Close did not return after stopping priority index worker")
	}

	workerDone := make(chan struct{})
	go func() {
		doc.priorityWG.Wait()
		close(workerDone)
	}()
	select {
	case <-workerDone:
	case <-time.After(time.Second):
		t.Fatal("priority worker still running after Close returned")
	}

	doc.RequestPriorityIndex(0)
	select {
	case got := <-doc.priorityRequests:
		t.Fatalf("closed document accepted priority request at offset %d", got)
	default:
	}

	if _, err := doc.ReadRange(0, 1); !errors.Is(err, errDocumentClosed) {
		t.Fatalf("ReadRange after Close error = %v, want errDocumentClosed", err)
	}
}

func TestCloseWaitsForFirstPriorityRequestRegistration(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "priority-first-request-close.log")
	if err := os.WriteFile(path, []byte("one\ntwo\nthree\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	doc, err := OpenFile(path)
	if err != nil {
		t.Fatal(err)
	}

	// Hold the request immediately before its lazy worker registration. The
	// request must retain lifecycleMu's read side here so Close cannot reach its
	// zero-counter Wait until registration and enqueue are complete.
	doc.priorityMu.Lock()
	requestDone := make(chan struct{})
	go func() {
		doc.RequestPriorityIndex(0)
		close(requestDone)
	}()

	barrierHeld := false
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if !doc.lifecycleMu.TryLock() {
			barrierHeld = true
			break
		}
		doc.lifecycleMu.Unlock()
		time.Sleep(time.Millisecond)
	}
	if !barrierHeld {
		doc.priorityMu.Unlock()
		<-requestDone
		_ = doc.Close()
		t.Fatal("priority request did not retain the lifecycle barrier before worker registration")
	}

	closeDone := make(chan error, 1)
	go func() { closeDone <- doc.Close() }()
	select {
	case err := <-closeDone:
		doc.priorityMu.Unlock()
		<-requestDone
		if err != nil {
			t.Fatal(err)
		}
		t.Fatal("Close returned while the first priority request was not registered")
	case <-time.After(25 * time.Millisecond):
	}

	doc.priorityMu.Unlock()
	select {
	case <-requestDone:
	case <-time.After(time.Second):
		t.Fatal("priority request did not finish after its registration barrier was released")
	}
	select {
	case err := <-closeDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Close did not finish after the first priority request registered")
	}

	select {
	case got := <-doc.priorityRequests:
		t.Fatalf("closed document retained priority request at offset %d", got)
	default:
	}
}

func TestCoalescePriorityIndexRequestKeepsLatestPendingOffset(t *testing.T) {
	requests := make(chan int64, 1)
	done := make(chan struct{})

	coalescePriorityIndexRequest(requests, done, 10)
	coalescePriorityIndexRequest(requests, done, 20)

	select {
	case got := <-requests:
		if got != 20 {
			t.Fatalf("pending offset = %d, want latest 20", got)
		}
	default:
		t.Fatal("expected one pending priority request")
	}
}

func TestCoalescePriorityIndexRequestStopsAfterClose(t *testing.T) {
	requests := make(chan int64, 1)
	done := make(chan struct{})
	close(done)

	coalescePriorityIndexRequest(requests, done, 10)

	select {
	case got := <-requests:
		t.Fatalf("unexpected pending request after close: %d", got)
	default:
	}
}

func TestLatestPriorityIndexRequestDrainsToNewestOffset(t *testing.T) {
	requests := make(chan int64, 3)
	done := make(chan struct{})
	requests <- 20
	requests <- 30

	got, ok := latestPriorityIndexRequest(requests, done, 10)
	if !ok {
		t.Fatal("expected drain to succeed")
	}
	if got != 30 {
		t.Fatalf("latest offset = %d, want 30", got)
	}
}

func TestSeedPriorityIndexUsesNearbyExactAnchorOnly(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "priority-mid.log")
	content := []byte(strings.Repeat("0000\n", (priorityIndexWindowSize/5)+32))
	if err := os.WriteFile(path, content, 0o600); err != nil {
		t.Fatal(err)
	}

	doc, err := OpenFile(path)
	if err != nil {
		t.Fatal(err)
	}
	defer doc.Close()
	doc.idx.EveryLines = 2

	reader := &errorAfterReader{data: content[:20], err: errors.New("stop after partial progress")}
	if err := doc.idx.Build(context.Background(), reader); err == nil {
		t.Fatal("expected partial build error")
	}

	targetOffset := int64(priorityIndexWindowSize) + 1024
	doc.seedPriorityIndex(targetOffset)

	if doc.priorityWindowHi == 0 || doc.priorityWindowLo == 0 {
		t.Fatalf("expected priority window seeded from nearby exact anchor, got [%d,%d)", doc.priorityWindowLo, doc.priorityWindowHi)
	}
}

type errorAfterReader struct {
	data []byte
	err  error
	read bool
}

func (r *errorAfterReader) Read(p []byte) (int, error) {
	if r.read {
		return 0, r.err
	}
	r.read = true
	n := copy(p, r.data)
	if n < len(r.data) {
		r.data = r.data[n:]
		r.read = false
		return n, nil
	}
	return n, nil
}

var _ io.Reader = (*errorAfterReader)(nil)

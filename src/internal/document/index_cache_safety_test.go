package document

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"io"
	"math"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/quarry/quarry-wails3/internal/lineindex"
	"github.com/quarry/quarry-wails3/internal/settings"
)

type indexCacheFixture struct {
	path    string
	info    os.FileInfo
	hash    string
	idx     *lineindex.Index
	entries []lineindex.Entry
}

func newIndexCacheFixture(t testing.TB, root, name string, body []byte) indexCacheFixture {
	t.Helper()
	path := filepath.Join(root, name)
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	file, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	head := body
	if len(head) > openSampleSize {
		head = head[:openSampleSize]
	}
	hash := computeIndexSampleHash(file, info.Size(), head)
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	idx := lineindex.New(lineindex.EveryLinesForSize(info.Size()))
	if err := idx.Build(context.Background(), bytes.NewReader(body)); err != nil {
		t.Fatal(err)
	}
	return indexCacheFixture{path: path, info: info, hash: hash, idx: idx, entries: idx.Entries()}
}

func (fixture indexCacheFixture) save(t testing.TB) {
	t.Helper()
	if err := saveIndexCache(fixture.path, fixture.info.Size(), fixture.info.ModTime(), fixture.hash, fixture.idx); err != nil {
		t.Fatal(err)
	}
}

func (fixture indexCacheFixture) load() (*lineindex.Index, bool) {
	dst := lineindex.New(lineindex.EveryLinesForSize(fixture.info.Size()))
	return dst, loadIndexCache(fixture.path, fixture.info.Size(), fixture.info.ModTime(), fixture.hash, dst)
}

func TestIndexCacheBinaryRoundTripAndHostileInputs(t *testing.T) {
	t.Setenv(settings.ConfigDirEnv, filepath.Join(t.TempDir(), "config"))
	fixture := newIndexCacheFixture(t, t.TempDir(), "source.txt", bytes.Repeat([]byte{'\n'}, 9_000))
	if len(fixture.entries) < 3 {
		t.Fatalf("fixture entries = %d, want at least three", len(fixture.entries))
	}
	fixture.save(t)
	if idx, ok := fixture.load(); !ok || !idx.Done() || len(idx.Entries()) != len(fixture.entries) {
		t.Fatalf("valid binary cache rejected: idx=%v ok=%v", idx, ok)
	}

	mutate := func(t *testing.T, change func([]byte) []byte) {
		t.Helper()
		fixture.save(t)
		cachePath := indexCachePath(fixture.path)
		data, err := os.ReadFile(cachePath)
		if err != nil {
			t.Fatal(err)
		}
		data = change(data)
		if err := os.WriteFile(cachePath, data, 0o600); err != nil {
			t.Fatal(err)
		}
		if _, ok := fixture.load(); ok {
			t.Fatal("hostile cache was accepted")
		}
	}
	entryStart := func(data []byte, index int) int {
		return int(indexCacheHeaderBytes) + int(binary.BigEndian.Uint32(data[16:20])) + index*int(indexCacheEntryBytes)
	}
	rechecksum := func(data []byte) []byte {
		sum := sha256.Sum256(data[:len(data)-sha256.Size])
		copy(data[len(data)-sha256.Size:], sum[:])
		return data
	}

	tests := []struct {
		name   string
		change func([]byte) []byte
	}{
		{"hostile declared count", func(data []byte) []byte {
			binary.BigEndian.PutUint32(data[20:24], math.MaxUint32)
			return data
		}},
		{"hostile declared path", func(data []byte) []byte {
			binary.BigEndian.PutUint32(data[16:20], maxIndexCacheSourcePathBytes+1)
			return data
		}},
		{"truncation", func(data []byte) []byte { return data[:len(data)-1] }},
		{"trailing byte", func(data []byte) []byte { return append(data, 0) }},
		{"checksum", func(data []byte) []byte {
			data[len(data)-1] ^= 0xff
			return data
		}},
		{"wrong version", func(data []byte) []byte {
			binary.BigEndian.PutUint32(data[8:12], indexCacheVersion+1)
			return data
		}},
		{"source size mismatch", func(data []byte) []byte {
			binary.BigEndian.PutUint64(data[24:32], uint64(fixture.info.Size()+1))
			return rechecksum(data)
		}},
		{"source hash mismatch", func(data []byte) []byte {
			data[72] ^= 1
			return rechecksum(data)
		}},
		{"wrong totals", func(data []byte) []byte {
			binary.BigEndian.PutUint64(data[48:56], 1)
			return rechecksum(data)
		}},
		{"duplicate anchor", func(data []byte) []byte {
			second := entryStart(data, 1)
			third := entryStart(data, 2)
			copy(data[third:third+8], data[second:second+8])
			return rechecksum(data)
		}},
		{"decreasing offset", func(data []byte) []byte {
			third := entryStart(data, 2)
			binary.BigEndian.PutUint64(data[third+8:third+16], 1)
			return rechecksum(data)
		}},
		{"offset beyond EOF", func(data []byte) []byte {
			third := entryStart(data, 2)
			binary.BigEndian.PutUint64(data[third+8:third+16], uint64(fixture.info.Size()+1))
			return rechecksum(data)
		}},
		{"signed anchor overflow", func(data []byte) []byte {
			third := entryStart(data, 2)
			binary.BigEndian.PutUint64(data[third:third+8], math.MaxUint64)
			return rechecksum(data)
		}},
		{"missing stride anchor", func(data []byte) []byte {
			third := entryStart(data, 2)
			withoutThird := append([]byte(nil), data[:third]...)
			withoutThird = append(withoutThird, data[third+int(indexCacheEntryBytes):]...)
			binary.BigEndian.PutUint32(withoutThird[20:24], 2)
			binary.BigEndian.PutUint64(withoutThird[64:72], uint64(len(fixture.path)+2*int(indexCacheEntryBytes)))
			return rechecksum(withoutThird)
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) { mutate(t, test.change) })
	}

	t.Run("oversized file", func(t *testing.T) {
		fixture.save(t)
		file, err := os.OpenFile(indexCachePath(fixture.path), os.O_WRONLY, 0)
		if err != nil {
			t.Fatal(err)
		}
		if err := file.Truncate(maxIndexCacheFileBytes + 1); err != nil {
			_ = file.Close()
			t.Fatal(err)
		}
		if err := file.Close(); err != nil {
			t.Fatal(err)
		}
		if _, ok := fixture.load(); ok {
			t.Fatal("oversized cache accepted")
		}
	})
}

func TestRejectedIndexCacheReadReleasesHandleBeforeNextSave(t *testing.T) {
	t.Setenv(settings.ConfigDirEnv, filepath.Join(t.TempDir(), "config"))
	fixture := newIndexCacheFixture(t, t.TempDir(), "source.txt", bytes.Repeat([]byte("row\n"), 2_000))
	fixture.save(t)

	cachePath := indexCachePath(fixture.path)
	data, err := os.ReadFile(cachePath)
	if err != nil {
		t.Fatal(err)
	}
	data[len(data)-1] ^= 0xff
	if err := os.WriteFile(cachePath, data, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, ok := fixture.load(); ok {
		t.Fatal("cache with a corrupt checksum loaded")
	}

	// On Windows this replacement fails if the rejected-load path retained its
	// read handle. Keep this as an explicit lifecycle regression instead of
	// relying on the ordering of the hostile-input table above.
	fixture.save(t)
	if _, ok := fixture.load(); !ok {
		t.Fatal("replacement cache was not readable")
	}
}

func TestIndexCacheSourceMetadataMismatchDoesNotMutateDestination(t *testing.T) {
	t.Setenv(settings.ConfigDirEnv, filepath.Join(t.TempDir(), "config"))
	fixture := newIndexCacheFixture(t, t.TempDir(), "source.txt", []byte("a\nb\n"))
	fixture.save(t)
	dst := lineindex.New(lineindex.EveryLinesForSize(fixture.info.Size()))
	if loadIndexCache(fixture.path, fixture.info.Size(), fixture.info.ModTime().Add(time.Nanosecond), fixture.hash, dst) {
		t.Fatal("cache with mismatched source mtime loaded")
	}
	if progress := dst.Progress(); progress != (lineindex.Progress{}) {
		t.Fatalf("rejected cache mutated destination: %#v", progress)
	}
	badHash := strings.Repeat("0", sha256.Size*2)
	if badHash == fixture.hash {
		badHash = strings.Repeat("1", sha256.Size*2)
	}
	if loadIndexCache(fixture.path, fixture.info.Size(), fixture.info.ModTime(), badHash, dst) {
		t.Fatal("cache with mismatched source sample loaded")
	}
}

func TestIndexCacheLoaderRejectsSymlinkEntry(t *testing.T) {
	t.Setenv(settings.ConfigDirEnv, filepath.Join(t.TempDir(), "config"))
	fixture := newIndexCacheFixture(t, t.TempDir(), "source.txt", []byte("a\n"))
	cachePath := indexCachePath(fixture.path)
	if err := os.MkdirAll(filepath.Dir(cachePath), 0o700); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(t.TempDir(), "sentinel.qix")
	if err := os.WriteFile(target, []byte("sentinel"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, cachePath); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if _, ok := fixture.load(); ok {
		t.Fatal("symlink cache entry accepted")
	}
	if got, err := os.ReadFile(target); err != nil || string(got) != "sentinel" {
		t.Fatalf("sentinel changed: %q, %v", got, err)
	}
}

func TestSaveIndexCacheRejectsInvalidSnapshot(t *testing.T) {
	t.Setenv(settings.ConfigDirEnv, t.TempDir())
	idx := lineindex.New(lineindex.EveryLinesForSize(100))
	idx.MarkDone(10, 100)
	err := saveIndexCache(filepath.Join(t.TempDir(), "source.txt"), 100, time.Time{}, strings.Repeat("0", sha256.Size*2), idx)
	if err == nil {
		t.Fatal("invalid non-empty snapshot without line-1 anchor was saved")
	}
}

func TestIndexCacheQuotaEvictsOldestMovedSourceAndPreservesUnownedEntries(t *testing.T) {
	configDir := filepath.Join(t.TempDir(), "config")
	t.Setenv(settings.ConfigDirEnv, configDir)
	oldQuota := currentPersistentIndexCacheDiskQuotaBytes()
	defer SetPersistentIndexCacheDiskQuotaBytes(oldQuota)
	sourceDir := t.TempDir()
	first := newIndexCacheFixture(t, sourceDir, "first.txt", []byte("a\nb\n"))
	second := newIndexCacheFixture(t, sourceDir, "second.txt", []byte("c\nd\n"))
	third := newIndexCacheFixture(t, sourceDir, "third.txt", []byte("e\nf\n"))
	firstBytes, err := indexCacheEncodedSize(len(first.path), len(first.entries))
	if err != nil {
		t.Fatal(err)
	}
	secondBytes, err := indexCacheEncodedSize(len(second.path), len(second.entries))
	if err != nil {
		t.Fatal(err)
	}
	thirdBytes, err := indexCacheEncodedSize(len(third.path), len(third.entries))
	if err != nil {
		t.Fatal(err)
	}
	quota := secondBytes + thirdBytes
	if firstBytes > secondBytes {
		quota = firstBytes + thirdBytes
	}
	SetPersistentIndexCacheDiskQuotaBytes(quota)
	first.save(t)
	second.save(t)
	old := time.Unix(1, 0)
	newer := time.Unix(2, 0)
	if err := os.Chtimes(indexCachePath(first.path), old, old); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(indexCachePath(second.path), newer, newer); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(first.path); err != nil {
		t.Fatal(err)
	}

	cacheDir := filepath.Dir(indexCachePath(first.path))
	unowned := filepath.Join(cacheDir, "notes.txt")
	if err := os.WriteFile(unowned, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	staleTemp := filepath.Join(cacheDir, filepath.Base(indexCachePath(first.path))+".tmp."+strings.Repeat("a", 32))
	if err := os.WriteFile(staleTemp, []byte("incomplete cache temp"), 0o600); err != nil {
		t.Fatal(err)
	}
	legacyCentral := filepath.Join(cacheDir, strings.Repeat("e", sha256.Size*2)+legacyCentralIndexCacheExtension)
	if err := os.WriteFile(legacyCentral, []byte(`{"version":2}`), 0o600); err != nil {
		t.Fatal(err)
	}
	sentinel := filepath.Join(t.TempDir(), "sentinel")
	if err := os.WriteFile(sentinel, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	unownedLink := filepath.Join(cacheDir, strings.Repeat("f", sha256.Size*2)+indexCacheExtension)
	linkCreated := os.Symlink(sentinel, unownedLink) == nil

	third.save(t)
	if _, err := os.Lstat(indexCachePath(first.path)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("oldest moved-source cache was not evicted: %v", err)
	}
	if _, err := os.Lstat(staleTemp); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("recognized stale cache temp was not cleaned: %v", err)
	}
	if _, err := os.Lstat(legacyCentral); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("retired central JSON cache was not cleaned without being read: %v", err)
	}
	for _, path := range []string{indexCachePath(second.path), indexCachePath(third.path), unowned} {
		if _, err := os.Lstat(path); err != nil {
			t.Fatalf("expected entry %q was removed: %v", path, err)
		}
	}
	if linkCreated {
		if info, err := os.Lstat(unownedLink); err != nil || info.Mode()&os.ModeSymlink == 0 {
			t.Fatalf("unowned recognized-name symlink changed: %v, %v", info, err)
		}
	}
	if got, err := os.ReadFile(sentinel); err != nil || string(got) != "keep" {
		t.Fatalf("symlink sentinel changed: %q, %v", got, err)
	}
}

func TestIndexCacheQuotaRefusesEntryLargerThanBudget(t *testing.T) {
	t.Setenv(settings.ConfigDirEnv, filepath.Join(t.TempDir(), "config"))
	oldQuota := currentPersistentIndexCacheDiskQuotaBytes()
	defer SetPersistentIndexCacheDiskQuotaBytes(oldQuota)
	SetPersistentIndexCacheDiskQuotaBytes(1)
	fixture := newIndexCacheFixture(t, t.TempDir(), "source.txt", []byte("a\n"))
	err := saveIndexCache(fixture.path, fixture.info.Size(), fixture.info.ModTime(), fixture.hash, fixture.idx)
	if !errors.Is(err, ErrIndexCacheQuota) {
		t.Fatalf("error = %v, want ErrIndexCacheQuota", err)
	}
	if _, err := os.Lstat(indexCachePath(fixture.path)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("quota-refused cache became visible: %v", err)
	}
}

func TestConcurrentDocumentsRespectIndexCacheQuota(t *testing.T) {
	configDir := filepath.Join(t.TempDir(), "config")
	t.Setenv(settings.ConfigDirEnv, configDir)
	oldQuota := currentPersistentIndexCacheDiskQuotaBytes()
	defer SetPersistentIndexCacheDiskQuotaBytes(oldQuota)
	sourceDir := t.TempDir()
	const documentCount = 12
	docs := make([]*FileDocument, 0, documentCount)
	var largest int64
	for i := 0; i < documentCount; i++ {
		path := filepath.Join(sourceDir, strings.Repeat(string(rune('a'+i)), 8)+".txt")
		if err := os.WriteFile(path, []byte("one\ntwo\nthree\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		doc, err := OpenFile(path)
		if err != nil {
			t.Fatal(err)
		}
		docs = append(docs, doc)
		encoded, err := indexCacheEncodedSize(len(path), 1)
		if err != nil {
			t.Fatal(err)
		}
		if encoded > largest {
			largest = encoded
		}
	}
	defer func() {
		for _, doc := range docs {
			_ = doc.Close()
		}
	}()
	quota := largest * 3
	SetPersistentIndexCacheDiskQuotaBytes(quota)

	start := make(chan struct{})
	errs := make(chan error, len(docs))
	var wait sync.WaitGroup
	for _, doc := range docs {
		wait.Add(1)
		go func(doc *FileDocument) {
			defer wait.Done()
			<-start
			errs <- doc.StartIndexing(context.Background())
		}(doc)
	}
	close(start)
	wait.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("StartIndexing: %v", err)
		}
	}

	cacheDir := filepath.Join(configDir, "cache", "indexes")
	entries, err := os.ReadDir(cacheDir)
	if err != nil {
		t.Fatal(err)
	}
	var total int64
	managedEntries := 0
	for _, entry := range entries {
		if !isRecognizedIndexCacheName(entry.Name()) {
			continue
		}
		managedEntries++
		info, err := os.Lstat(filepath.Join(cacheDir, entry.Name()))
		if err != nil {
			t.Fatal(err)
		}
		if !info.Mode().IsRegular() {
			t.Fatalf("managed cache is non-regular: %q", entry.Name())
		}
		total += info.Size()
	}
	if total > quota {
		t.Fatalf("cache use = %d, quota = %d", total, quota)
	}
	if managedEntries > 3 {
		t.Fatalf("managed cache entries = %d, expected at most three under quota", managedEntries)
	}
}

func TestIndexCacheDirectoryOwnershipRefusesCompetingWriter(t *testing.T) {
	t.Setenv(settings.ConfigDirEnv, filepath.Join(t.TempDir(), "config"))
	fixture := newIndexCacheFixture(t, t.TempDir(), "source.txt", []byte("one\ntwo\n"))
	cacheDir := filepath.Dir(indexCachePath(fixture.path))
	if err := os.MkdirAll(cacheDir, 0o700); err != nil {
		t.Fatal(err)
	}
	release, err := acquireIndexCacheDirectoryLock(cacheDir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = release() }()
	err = saveIndexCache(fixture.path, fixture.info.Size(), fixture.info.ModTime(), fixture.hash, fixture.idx)
	if !errors.Is(err, ErrIndexCacheBusy) {
		t.Fatalf("error = %v, want ErrIndexCacheBusy", err)
	}
	if _, err := os.Lstat(indexCachePath(fixture.path)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("competing writer published a cache: %v", err)
	}
}

func TestConcurrentIndexCacheLoadAndSaveKeepsStableDestination(t *testing.T) {
	t.Setenv(settings.ConfigDirEnv, filepath.Join(t.TempDir(), "config"))
	fixture := newIndexCacheFixture(t, t.TempDir(), "source.txt", bytes.Repeat([]byte("x\n"), 5_000))
	fixture.save(t)
	const workers = 8
	var wait sync.WaitGroup
	errs := make(chan error, workers)
	for worker := 0; worker < workers; worker++ {
		wait.Add(1)
		go func(worker int) {
			defer wait.Done()
			if worker%2 == 0 {
				errs <- saveIndexCache(fixture.path, fixture.info.Size(), fixture.info.ModTime(), fixture.hash, fixture.idx)
				return
			}
			dst := lineindex.New(fixture.idx.EveryLines)
			if !loadIndexCache(fixture.path, fixture.info.Size(), fixture.info.ModTime(), fixture.hash, dst) || !dst.Done() {
				errs <- errors.New("concurrent cache load failed")
				return
			}
			errs <- nil
		}(worker)
	}
	wait.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
}

func TestConcurrentDeferredBinaryCacheHydrationKeepsStableIndexPointer(t *testing.T) {
	oldPersist := persistentIndexCacheEnabled()
	SetPersistentIndexCache(true)
	defer SetPersistentIndexCache(oldPersist)
	t.Setenv(settings.ConfigDirEnv, filepath.Join(t.TempDir(), "config"))

	sourcePath := filepath.Join(t.TempDir(), "huge.txt")
	file, err := os.OpenFile(sourcePath, os.O_CREATE|os.O_RDWR|os.O_TRUNC, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.WriteAt([]byte("a\nb\n"), 0); err != nil {
		_ = file.Close()
		t.Fatal(err)
	}
	size := int64(synchronousIndexCacheMaxSourceSize + 1)
	if err := file.Truncate(size); err != nil {
		_ = file.Close()
		t.Fatal(err)
	}
	info, err := file.Stat()
	if err != nil {
		_ = file.Close()
		t.Fatal(err)
	}
	head, err := readSample(file, size, openSampleSize)
	if err != nil {
		_ = file.Close()
		t.Fatal(err)
	}
	hash := computeIndexSampleHash(file, size, head)
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}

	every := lineindex.EveryLinesForSize(size)
	cached := lineindex.New(every)
	if err := cached.RestoreSnapshot(lineindex.Snapshot{
		EveryLines: every,
		Entries:    []lineindex.Entry{{Line: 1, Offset: 0}},
		Lines:      2,
		Bytes:      size,
		Done:       true,
	}); err != nil {
		t.Fatal(err)
	}
	if err := saveIndexCache(sourcePath, size, info.ModTime(), hash, cached); err != nil {
		t.Fatal(err)
	}

	doc, err := OpenFile(sourcePath)
	if err != nil {
		t.Fatal(err)
	}
	defer doc.Close()
	stable := doc.idx
	start := make(chan struct{})
	errs := make(chan error, 8)
	var wait sync.WaitGroup
	for i := 0; i < cap(errs); i++ {
		wait.Add(1)
		go func() {
			defer wait.Done()
			<-start
			errs <- doc.StartIndexing(context.Background())
		}()
	}
	close(start)
	for i := 0; i < 1_000; i++ {
		_ = doc.IndexProgress()
		_, _ = doc.ApproxLineToOffset(2)
		_, _ = doc.ApproxOffsetToLine(size / 2)
	}
	wait.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("StartIndexing: %v", err)
		}
	}
	if doc.idx != stable {
		t.Fatal("deferred hydration replaced the stable Index pointer")
	}
	if progress := doc.IndexProgress(); !progress.Done || progress.Lines != 2 || progress.Bytes != size {
		t.Fatalf("progress = %#v, want hydrated binary snapshot", progress)
	}
}

func TestIndexCacheSaveRejectsSampledSourceChangeAfterBuildWithRestoredMetadata(t *testing.T) {
	oldPersist := persistentIndexCacheEnabled()
	SetPersistentIndexCache(true)
	defer SetPersistentIndexCache(oldPersist)
	t.Setenv(settings.ConfigDirEnv, filepath.Join(t.TempDir(), "config"))
	path := filepath.Join(t.TempDir(), "source.txt")
	original := []byte("one\ntwo\n")
	changed := []byte("ONE\ntwo\n")
	if len(original) != len(changed) {
		t.Fatal("test fixture must preserve source length")
	}
	if err := os.WriteFile(path, original, 0o600); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	doc, err := OpenFile(path)
	if err != nil {
		t.Fatal(err)
	}
	defer doc.Close()

	originalBuild := buildDocumentLineIndex
	buildDocumentLineIndex = func(idx *lineindex.Index, ctx context.Context, reader io.Reader, encoding string) error {
		if err := originalBuild(idx, ctx, reader, encoding); err != nil {
			return err
		}
		if err := os.WriteFile(path, changed, 0o600); err != nil {
			return err
		}
		return os.Chtimes(path, info.ModTime(), info.ModTime())
	}
	t.Cleanup(func() { buildDocumentLineIndex = originalBuild })

	if err := doc.StartIndexing(context.Background()); !errors.Is(err, ErrSourceChanged) {
		t.Fatalf("StartIndexing error = %v, want ErrSourceChanged", err)
	}
	if _, err := os.Lstat(indexCachePath(path)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("cache was saved for a sampled source generation that changed after indexing: %v", err)
	}
}

func TestIndexCacheFullSourceHashRejectsMiddleRewriteWithRestoredMtime(t *testing.T) {
	path := filepath.Join(t.TempDir(), "source.bin")
	body := bytes.Repeat([]byte{'a'}, 4*openSampleSize)
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	file, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	head := body[:openSampleSize]
	before := computeIndexSampleHash(file, int64(len(body)), head)
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	if before == "" {
		t.Fatal("full source hash is unavailable")
	}

	const mutationOffset = int64(1_100_000)
	writer, err := os.OpenFile(path, os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := writer.WriteAt(bytes.Repeat([]byte{'b'}, 16), mutationOffset); err != nil {
		_ = writer.Close()
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, info.ModTime(), info.ModTime()); err != nil {
		t.Fatal(err)
	}
	file, err = os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	after := computeIndexSampleHash(file, int64(len(body)), head)
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	if after == "" {
		t.Fatal("full source hash became unavailable")
	}
	if after == before {
		t.Fatal("cache generation hash accepted a middle rewrite with restored size and mtime")
	}
}

func TestOpenFileDoesNotHydrateCacheAfterMiddleRewriteWithRestoredMtime(t *testing.T) {
	oldPersist := persistentIndexCacheEnabled()
	SetPersistentIndexCache(true)
	defer SetPersistentIndexCache(oldPersist)
	t.Setenv(settings.ConfigDirEnv, filepath.Join(t.TempDir(), "config"))

	path := filepath.Join(t.TempDir(), "source.txt")
	body := bytes.Repeat([]byte{'a'}, 4*openSampleSize)
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	file, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	sourceHash, err := computeIndexSourceHash(context.Background(), file, info.Size())
	if closeErr := file.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		t.Fatal(err)
	}
	idx := lineindex.New(lineindex.EveryLinesForSize(info.Size()))
	if err := idx.Build(context.Background(), bytes.NewReader(body)); err != nil {
		t.Fatal(err)
	}
	if err := saveIndexCache(path, info.Size(), info.ModTime(), sourceHash, idx); err != nil {
		t.Fatal(err)
	}

	const mutationOffset = int64(2*openSampleSize + 17)
	writer, err := os.OpenFile(path, os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := writer.WriteAt([]byte{'\n'}, mutationOffset); err != nil {
		_ = writer.Close()
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, info.ModTime(), info.ModTime()); err != nil {
		t.Fatal(err)
	}

	doc, err := OpenFile(path)
	if err != nil {
		t.Fatal(err)
	}
	defer doc.Close()
	if progress := doc.IndexProgress(); progress.Done {
		t.Fatalf("stale cache hydrated after middle rewrite: %#v", progress)
	}
	got := make([]byte, 1)
	if _, err := doc.ReadAt(got, mutationOffset); err != nil {
		t.Fatal(err)
	}
	if got[0] != '\n' {
		t.Fatalf("source byte = %q, want rewritten newline", got[0])
	}
}

func BenchmarkIndexCacheStreamingSaveAndLoad(b *testing.B) {
	b.Setenv(settings.ConfigDirEnv, filepath.Join(b.TempDir(), "config"))
	oldQuota := currentPersistentIndexCacheDiskQuotaBytes()
	defer SetPersistentIndexCacheDiskQuotaBytes(oldQuota)
	SetPersistentIndexCacheDiskQuotaBytes(maxIndexCacheFileBytes * 2)
	const entryCount = lineindex.MaxIndexEntries
	const stride = int64(4096)
	entries := make([]lineindex.Entry, entryCount)
	for i := range entries {
		entries[i] = lineindex.Entry{Line: 1 + int64(i)*stride, Offset: int64(i) * stride}
	}
	size := int64(entryCount-1) * stride
	idx := lineindex.New(lineindex.EveryLinesForSize(size))
	if idx.EveryLines != stride {
		b.Skipf("test fixture stride changed to %d", idx.EveryLines)
	}
	if err := idx.RestoreSnapshot(lineindex.Snapshot{EveryLines: stride, Entries: entries, Lines: size, Bytes: size, Done: true}); err != nil {
		b.Fatal(err)
	}
	path := filepath.Join(b.TempDir(), "source.txt")
	hash := strings.Repeat("0", sha256.Size*2)
	if err := saveIndexCache(path, size, time.Unix(1, 0), hash, idx); err != nil {
		b.Fatal(err)
	}
	b.Run("save", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			if err := saveIndexCache(path, size, time.Unix(1, 0), hash, idx); err != nil {
				b.Fatal(err)
			}
		}
	})
	b.Run("load", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			dst := lineindex.New(stride)
			if !loadIndexCache(path, size, time.Unix(1, 0), hash, dst) {
				b.Fatal("load failed")
			}
		}
	})
}

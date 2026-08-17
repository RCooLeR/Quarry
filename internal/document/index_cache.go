package document

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"math"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"sync"
	"time"

	"github.com/quarry/quarry-wails3/internal/cachepath"
	"github.com/quarry/quarry-wails3/internal/lineindex"
	"github.com/quarry/quarry-wails3/internal/regularfile"
)

const (
	indexCacheVersion                uint32 = 3
	indexCacheMagic                         = "QRYIDX03"
	indexCacheExtension                     = ".qix"
	legacyCentralIndexCacheExtension        = ".quarry-index.json"
	indexCacheHeaderBytes            int64  = 104
	indexCacheChecksumBytes          int64  = sha256.Size
	indexCacheEntryBytes             int64  = 16
	maxIndexCacheFileBytes           int64  = 16 * 1024 * 1024
	maxIndexCacheSourcePathBytes            = 64 * 1024
)

var (
	ErrIndexCacheQuota = errors.New("persistent line-index cache disk quota exceeded")
	ErrIndexCacheBusy  = errors.New("persistent line-index cache is owned by another process")
	indexCacheManager  sync.Mutex
)

type indexCacheHeader struct {
	pathBytes       uint32
	entryCount      uint32
	sourceSize      int64
	modTimeUnixNano int64
	everyLines      int64
	lines           int64
	indexedBytes    int64
	payloadBytes    int64
	sampleHash      [sha256.Size]byte
}

func indexCachePath(path string) string {
	central, err := cachepath.SourcePath("indexes", path, indexCacheExtension)
	if err != nil {
		return ""
	}
	return central
}

// legacyIndexCachePath exists only so tests and migration diagnostics can name
// the retired format. Quarry never reads or writes source-adjacent index data.
func legacyIndexCachePath(path string) string {
	return path + ".quarry-index.json"
}

// computeIndexSampleHash is retained as an internal test/helper name for the
// cache format's source hash. It now computes a full, constant-memory digest;
// no sampled-only cache identity is accepted.
func computeIndexSampleHash(f *os.File, size int64, head []byte) string {
	_ = head
	digest, err := computeIndexSourceHash(context.Background(), f, size)
	if err != nil {
		return ""
	}
	return digest
}

func computeIndexSourceHash(ctx context.Context, f *os.File, size int64) (string, error) {
	if f == nil {
		return "", errors.New("line-index source file is required")
	}
	if size < 0 {
		return "", errors.New("line-index source size is negative")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	before, err := f.Stat()
	if err != nil {
		return "", err
	}
	beforeChange, err := sourceChangeTokenForFile(f)
	if err != nil {
		return "", err
	}
	if before.Size() != size {
		return "", fmt.Errorf("%w: line-index source size changed before hashing", ErrSourceChanged)
	}
	hash := newIndexSourceHash(size)
	buf := make([]byte, exactScanChunkSize)
	for offset := int64(0); offset < size; {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		want := int64(len(buf))
		if remaining := size - offset; want > remaining {
			want = remaining
		}
		n, readErr := f.ReadAt(buf[:want], offset)
		if n > 0 {
			_, _ = hash.Write(buf[:n])
			offset += int64(n)
		}
		if n != int(want) {
			if readErr == nil {
				readErr = io.ErrUnexpectedEOF
			}
			return "", fmt.Errorf("%w: line-index source hash read %d of %d bytes: %v", ErrSourceChanged, offset, size, readErr)
		}
		if readErr != nil && !errors.Is(readErr, io.EOF) {
			return "", readErr
		}
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	after, err := f.Stat()
	if err != nil {
		return "", err
	}
	afterChange, err := sourceChangeTokenForFile(f)
	if err != nil {
		return "", err
	}
	if !os.SameFile(before, after) || after.Size() != before.Size() || !after.ModTime().Equal(before.ModTime()) || (beforeChange.available && (!afterChange.available || afterChange != beforeChange)) {
		return "", fmt.Errorf("%w: line-index source changed while hashing", ErrSourceChanged)
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

func newIndexSourceHash(size int64) hash.Hash {
	digest := sha256.New()
	_, _ = digest.Write([]byte("quarry-index-source-v4-full\x00"))
	var scalar [8]byte
	binary.BigEndian.PutUint64(scalar[:], uint64(size))
	_, _ = digest.Write(scalar[:])
	return digest
}

// loadIndexCache decodes privately and only mutates dst after header, anchors,
// checksum, exact EOF, source identity, and retained-handle checks all pass.
// The global manager serializes readers with quota eviction and publication.
func loadIndexCache(path string, size int64, modTime time.Time, sampleHash string, dst *lineindex.Index) bool {
	if !persistentIndexCacheEnabled() || dst == nil {
		return false
	}
	snapshot, ok := loadIndexCacheSnapshot(path, size, modTime, sampleHash, dst.EveryLines)
	if !ok {
		return false
	}
	return dst.RestoreSnapshotOwned(snapshot) == nil
}

func loadIndexCacheSnapshot(path string, size int64, modTime time.Time, sampleHash string, expectedStride int64) (lineindex.Snapshot, bool) {
	if !persistentIndexCacheEnabled() {
		return lineindex.Snapshot{}, false
	}
	cachePath := indexCachePath(path)
	if cachePath == "" {
		return lineindex.Snapshot{}, false
	}
	expectedHash, err := decodeIndexCacheSampleHash(sampleHash)
	if err != nil {
		return lineindex.Snapshot{}, false
	}

	indexCacheManager.Lock()
	release, lockErr := acquireIndexCacheDirectoryLock(filepath.Dir(cachePath))
	if lockErr != nil {
		indexCacheManager.Unlock()
		return lineindex.Snapshot{}, false
	}
	snapshot, err := readIndexCacheFile(cachePath, path, size, modTime, expectedHash, expectedStride)
	releaseErr := release()
	indexCacheManager.Unlock()
	if err != nil || releaseErr != nil {
		return lineindex.Snapshot{}, false
	}
	return snapshot, true
}

func readIndexCacheFile(cachePath, sourcePath string, sourceSize int64, modTime time.Time, expectedHash [sha256.Size]byte, expectedStride int64) (lineindex.Snapshot, error) {
	entryInfo, err := os.Lstat(cachePath)
	if err != nil {
		return lineindex.Snapshot{}, err
	}
	if !entryInfo.Mode().IsRegular() || entryInfo.Size() < indexCacheHeaderBytes+indexCacheChecksumBytes || entryInfo.Size() > maxIndexCacheFileBytes {
		return lineindex.Snapshot{}, errors.New("line-index cache is not a bounded regular file")
	}
	file, err := regularfile.OpenNoFollow(cachePath)
	if err != nil {
		return lineindex.Snapshot{}, err
	}
	closed := false
	defer func() {
		if !closed {
			_ = file.Close()
		}
	}()
	openedInfo, err := file.Stat()
	if err != nil || !openedInfo.Mode().IsRegular() || !os.SameFile(entryInfo, openedInfo) || openedInfo.Size() != entryInfo.Size() {
		return lineindex.Snapshot{}, errors.New("line-index cache changed while opening")
	}

	var rawHeader [indexCacheHeaderBytes]byte
	if _, err := io.ReadFull(file, rawHeader[:]); err != nil {
		return lineindex.Snapshot{}, err
	}
	header, err := decodeIndexCacheHeader(rawHeader[:])
	if err != nil {
		return lineindex.Snapshot{}, err
	}
	expectedFileBytes, err := indexCacheEncodedSize(int(header.pathBytes), int(header.entryCount))
	if err != nil || expectedFileBytes != entryInfo.Size() || header.payloadBytes != expectedFileBytes-indexCacheHeaderBytes-indexCacheChecksumBytes {
		return lineindex.Snapshot{}, errors.New("line-index cache declared size does not match the file")
	}
	if int(header.pathBytes) != len(sourcePath) || header.sourceSize != sourceSize || header.indexedBytes != sourceSize || header.modTimeUnixNano != modTime.UnixNano() || header.everyLines != expectedStride || expectedStride != lineindex.EveryLinesForSize(sourceSize) || subtle.ConstantTimeCompare(header.sampleHash[:], expectedHash[:]) != 1 {
		return lineindex.Snapshot{}, errors.New("line-index cache source identity does not match")
	}

	digest := sha256.New()
	_, _ = digest.Write(rawHeader[:])
	pathBytes := make([]byte, int(header.pathBytes))
	if _, err := io.ReadFull(file, pathBytes); err != nil {
		return lineindex.Snapshot{}, err
	}
	_, _ = digest.Write(pathBytes)
	if !bytes.Equal(pathBytes, []byte(sourcePath)) {
		return lineindex.Snapshot{}, errors.New("line-index cache source path does not match")
	}

	entries := make([]lineindex.Entry, int(header.entryCount))
	var encodedEntry [indexCacheEntryBytes]byte
	for i := range entries {
		if _, err := io.ReadFull(file, encodedEntry[:]); err != nil {
			return lineindex.Snapshot{}, err
		}
		_, _ = digest.Write(encodedEntry[:])
		lineRaw := binary.BigEndian.Uint64(encodedEntry[:8])
		offsetRaw := binary.BigEndian.Uint64(encodedEntry[8:])
		if lineRaw > math.MaxInt64 || offsetRaw > math.MaxInt64 {
			return lineindex.Snapshot{}, errors.New("line-index cache anchor overflows signed range")
		}
		entries[i] = lineindex.Entry{Line: int64(lineRaw), Offset: int64(offsetRaw)}
	}

	var storedChecksum [sha256.Size]byte
	if _, err := io.ReadFull(file, storedChecksum[:]); err != nil {
		return lineindex.Snapshot{}, err
	}
	if subtle.ConstantTimeCompare(storedChecksum[:], digest.Sum(nil)) != 1 {
		return lineindex.Snapshot{}, errors.New("line-index cache checksum mismatch")
	}
	var trailing [1]byte
	if n, err := file.Read(trailing[:]); n != 0 || !errors.Is(err, io.EOF) {
		return lineindex.Snapshot{}, errors.New("line-index cache has trailing data")
	}
	finalInfo, err := file.Stat()
	if err != nil || !os.SameFile(entryInfo, finalInfo) || finalInfo.Size() != entryInfo.Size() || !finalInfo.ModTime().Equal(entryInfo.ModTime()) {
		return lineindex.Snapshot{}, errors.New("line-index cache changed while reading")
	}
	if err := file.Close(); err != nil {
		return lineindex.Snapshot{}, err
	}
	closed = true

	snapshot := lineindex.Snapshot{
		EveryLines: header.everyLines,
		Entries:    entries,
		Lines:      header.lines,
		Bytes:      header.indexedBytes,
		Done:       true,
	}
	if err := lineindex.ValidateSnapshot(snapshot, sourceSize); err != nil {
		return lineindex.Snapshot{}, err
	}
	return snapshot, nil
}

func saveIndexCache(path string, size int64, modTime time.Time, sampleHash string, idx *lineindex.Index) error {
	if !persistentIndexCacheEnabled() {
		return nil
	}
	if idx == nil {
		return errors.New("line index is required")
	}
	cachePath := indexCachePath(path)
	if cachePath == "" {
		return errors.New("central line-index cache path is unavailable")
	}
	hashBytes, err := decodeIndexCacheSampleHash(sampleHash)
	if err != nil {
		return err
	}

	indexCacheManager.Lock()
	defer indexCacheManager.Unlock()
	return idx.WithSnapshotView(func(snapshot lineindex.Snapshot) error {
		if !snapshot.Done {
			return nil
		}
		if err := lineindex.ValidateSnapshot(snapshot, size); err != nil {
			return err
		}
		if snapshot.EveryLines != lineindex.EveryLinesForSize(size) {
			return errors.New("line-index cache stride does not match the bounded source-size policy")
		}
		encodedSize, err := indexCacheEncodedSize(len(path), len(snapshot.Entries))
		if err != nil {
			return err
		}
		header := indexCacheHeader{
			pathBytes:       uint32(len(path)),
			entryCount:      uint32(len(snapshot.Entries)),
			sourceSize:      size,
			modTimeUnixNano: modTime.UnixNano(),
			everyLines:      snapshot.EveryLines,
			lines:           snapshot.Lines,
			indexedBytes:    snapshot.Bytes,
			payloadBytes:    int64(len(path)) + int64(len(snapshot.Entries))*indexCacheEntryBytes,
			sampleHash:      hashBytes,
		}
		return writeIndexCacheFile(cachePath, path, header, snapshot.Entries, encodedSize)
	})
}

func writeIndexCacheFile(cachePath, sourcePath string, header indexCacheHeader, entries []lineindex.Entry, encodedSize int64) (retErr error) {
	dir := filepath.Dir(cachePath)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	dirInfo, err := os.Lstat(dir)
	if err != nil || !dirInfo.IsDir() || dirInfo.Mode()&os.ModeSymlink != 0 {
		return errors.New("line-index cache directory is not a real directory")
	}
	release, err := acquireIndexCacheDirectoryLock(dir)
	if err != nil {
		return err
	}
	defer func() { retErr = errors.Join(retErr, release()) }()
	if err := reserveIndexCacheQuota(dir, cachePath, encodedSize); err != nil {
		return err
	}

	temp, tempPath, err := openIndexCacheTemp(dir, filepath.Base(cachePath))
	if err != nil {
		return err
	}
	tempClosed := false
	tempIdentity, err := temp.Stat()
	if err != nil {
		_ = temp.Close()
		return err
	}
	defer func() {
		if !tempClosed {
			retErr = errors.Join(retErr, temp.Close())
		}
		current, err := os.Lstat(tempPath)
		if err == nil && current.Mode().IsRegular() && os.SameFile(tempIdentity, current) {
			retErr = errors.Join(retErr, os.Remove(tempPath))
		} else if err != nil && !errors.Is(err, os.ErrNotExist) {
			retErr = errors.Join(retErr, err)
		}
	}()
	if err := temp.Chmod(0o600); err != nil {
		return err
	}

	buffered := bufio.NewWriterSize(temp, 64*1024)
	digest := sha256.New()
	payload := io.MultiWriter(buffered, digest)
	encodedHeader := encodeIndexCacheHeader(header)
	if err := writeFull(payload, encodedHeader[:]); err != nil {
		return err
	}
	if err := writeFull(payload, []byte(sourcePath)); err != nil {
		return err
	}
	var encodedEntry [indexCacheEntryBytes]byte
	for _, entry := range entries {
		binary.BigEndian.PutUint64(encodedEntry[:8], uint64(entry.Line))
		binary.BigEndian.PutUint64(encodedEntry[8:], uint64(entry.Offset))
		if err := writeFull(payload, encodedEntry[:]); err != nil {
			return err
		}
	}
	if err := writeFull(buffered, digest.Sum(nil)); err != nil {
		return err
	}
	if err := buffered.Flush(); err != nil {
		return err
	}
	if err := temp.Sync(); err != nil {
		return err
	}
	info, err := temp.Stat()
	if err != nil || info.Size() != encodedSize {
		return errors.New("line-index cache encoder wrote an unexpected byte count")
	}
	if err := temp.Close(); err != nil {
		return err
	}
	tempClosed = true
	if err := publishIndexCacheTemp(tempPath, cachePath); err != nil {
		return err
	}
	return syncIndexCacheDirectory(dir)
}

func decodeIndexCacheSampleHash(value string) ([sha256.Size]byte, error) {
	var out [sha256.Size]byte
	if len(value) != hex.EncodedLen(len(out)) {
		return out, errors.New("line-index cache sample hash must be SHA-256")
	}
	decoded, err := hex.Decode(out[:], []byte(value))
	if err != nil || decoded != len(out) {
		return out, errors.New("line-index cache sample hash must be lowercase or uppercase hexadecimal SHA-256")
	}
	return out, nil
}

func indexCacheEncodedSize(pathBytes, entryCount int) (int64, error) {
	if pathBytes < 0 || pathBytes > maxIndexCacheSourcePathBytes {
		return 0, errors.New("line-index cache source path exceeds the bounded format limit")
	}
	if entryCount < 0 || entryCount > lineindex.MaxIndexEntries {
		return 0, lineindex.ErrIndexEntryLimit
	}
	total := indexCacheHeaderBytes + int64(pathBytes) + int64(entryCount)*indexCacheEntryBytes + indexCacheChecksumBytes
	if total < 0 || total > maxIndexCacheFileBytes {
		return 0, errors.New("line-index cache exceeds the bounded file-size limit")
	}
	return total, nil
}

func encodeIndexCacheHeader(header indexCacheHeader) [indexCacheHeaderBytes]byte {
	var out [indexCacheHeaderBytes]byte
	copy(out[:8], indexCacheMagic)
	binary.BigEndian.PutUint32(out[8:12], indexCacheVersion)
	binary.BigEndian.PutUint32(out[12:16], uint32(indexCacheHeaderBytes))
	binary.BigEndian.PutUint32(out[16:20], header.pathBytes)
	binary.BigEndian.PutUint32(out[20:24], header.entryCount)
	binary.BigEndian.PutUint64(out[24:32], uint64(header.sourceSize))
	binary.BigEndian.PutUint64(out[32:40], uint64(header.modTimeUnixNano))
	binary.BigEndian.PutUint64(out[40:48], uint64(header.everyLines))
	binary.BigEndian.PutUint64(out[48:56], uint64(header.lines))
	binary.BigEndian.PutUint64(out[56:64], uint64(header.indexedBytes))
	binary.BigEndian.PutUint64(out[64:72], uint64(header.payloadBytes))
	copy(out[72:], header.sampleHash[:])
	return out
}

func decodeIndexCacheHeader(raw []byte) (indexCacheHeader, error) {
	if len(raw) != int(indexCacheHeaderBytes) || string(raw[:8]) != indexCacheMagic || binary.BigEndian.Uint32(raw[8:12]) != indexCacheVersion || binary.BigEndian.Uint32(raw[12:16]) != uint32(indexCacheHeaderBytes) {
		return indexCacheHeader{}, errors.New("unsupported line-index cache header")
	}
	unsigned := []uint64{
		binary.BigEndian.Uint64(raw[24:32]),
		binary.BigEndian.Uint64(raw[40:48]),
		binary.BigEndian.Uint64(raw[48:56]),
		binary.BigEndian.Uint64(raw[56:64]),
		binary.BigEndian.Uint64(raw[64:72]),
	}
	for _, value := range unsigned {
		if value > math.MaxInt64 {
			return indexCacheHeader{}, errors.New("line-index cache header overflows signed range")
		}
	}
	header := indexCacheHeader{
		pathBytes:       binary.BigEndian.Uint32(raw[16:20]),
		entryCount:      binary.BigEndian.Uint32(raw[20:24]),
		sourceSize:      int64(unsigned[0]),
		modTimeUnixNano: int64(binary.BigEndian.Uint64(raw[32:40])),
		everyLines:      int64(unsigned[1]),
		lines:           int64(unsigned[2]),
		indexedBytes:    int64(unsigned[3]),
		payloadBytes:    int64(unsigned[4]),
	}
	copy(header.sampleHash[:], raw[72:])
	if header.pathBytes > maxIndexCacheSourcePathBytes || header.entryCount > lineindex.MaxIndexEntries || header.everyLines <= 0 || header.lines < 0 || header.indexedBytes < 0 || header.sourceSize < 0 || header.payloadBytes < 0 {
		return indexCacheHeader{}, errors.New("line-index cache header exceeds a bounded field limit")
	}
	return header, nil
}

func writeFull(writer io.Writer, data []byte) error {
	for len(data) > 0 {
		n, err := writer.Write(data)
		if err != nil {
			return err
		}
		if n <= 0 {
			return io.ErrShortWrite
		}
		data = data[n:]
	}
	return nil
}

type indexCacheQuotaEntry struct {
	path    string
	info    os.FileInfo
	modTime time.Time
	temp    bool
}

func reserveIndexCacheQuota(dir, target string, incomingBytes int64) error {
	quota := currentPersistentIndexCacheDiskQuotaBytes()
	if incomingBytes < 0 || incomingBytes > quota {
		return fmt.Errorf("%w: cache needs %d bytes, quota is %d", ErrIndexCacheQuota, incomingBytes, quota)
	}
	dirEntries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	var total int64
	var targetBytes int64
	candidates := make([]indexCacheQuotaEntry, 0, len(dirEntries))
	for _, entry := range dirEntries {
		isFinal := isRecognizedIndexCacheName(entry.Name())
		isTemp := isRecognizedIndexCacheTempName(entry.Name())
		isLegacy := isRecognizedLegacyCentralIndexCacheName(entry.Name())
		if !isFinal && !isTemp && !isLegacy {
			continue
		}
		candidatePath := filepath.Join(dir, entry.Name())
		info, err := os.Lstat(candidatePath)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			return err
		}
		if !info.Mode().IsRegular() {
			if sameCachePath(candidatePath, target) {
				return errors.New("line-index cache target is an unowned non-regular entry")
			}
			continue
		}
		if info.Size() < 0 || total > math.MaxInt64-info.Size() {
			return fmt.Errorf("%w: existing cache size overflow", ErrIndexCacheQuota)
		}
		total += info.Size()
		if isFinal && sameCachePath(candidatePath, target) {
			targetBytes = info.Size()
			continue
		}
		candidates = append(candidates, indexCacheQuotaEntry{path: candidatePath, info: info, modTime: info.ModTime(), temp: isTemp || isLegacy})
	}
	// The cross-process directory lock proves that no other cooperating writer
	// owns a temp. Therefore every recognized temp seen here is a crash remnant,
	// and it is safe to remove after retained identity revalidation.
	for i := range candidates {
		if !candidates[i].temp || !removeOwnedIndexCacheEntry(candidates[i]) {
			continue
		}
		total -= candidates[i].info.Size()
		candidates[i].info = nil
	}
	finalBytes := total - targetBytes
	if finalBytes > math.MaxInt64-incomingBytes {
		return fmt.Errorf("%w: projected cache size overflow", ErrIndexCacheQuota)
	}
	finalBytes += incomingBytes
	if finalBytes <= quota {
		return nil
	}
	sort.Slice(candidates, func(i, j int) bool {
		if candidates[i].modTime.Equal(candidates[j].modTime) {
			return candidates[i].path < candidates[j].path
		}
		return candidates[i].modTime.Before(candidates[j].modTime)
	})
	for _, candidate := range candidates {
		if candidate.info == nil {
			continue
		}
		if !removeOwnedIndexCacheEntry(candidate) {
			continue
		}
		finalBytes -= candidate.info.Size()
		if finalBytes <= quota {
			return nil
		}
	}
	return fmt.Errorf("%w: projected cache use %d bytes, quota is %d", ErrIndexCacheQuota, finalBytes, quota)
}

func removeOwnedIndexCacheEntry(candidate indexCacheQuotaEntry) bool {
	current, err := os.Lstat(candidate.path)
	if err != nil {
		return errors.Is(err, os.ErrNotExist)
	}
	if !current.Mode().IsRegular() || !os.SameFile(candidate.info, current) || current.Size() != candidate.info.Size() || !current.ModTime().Equal(candidate.info.ModTime()) {
		return false
	}
	return os.Remove(candidate.path) == nil
}

func isRecognizedIndexCacheName(name string) bool {
	const hashChars = sha256.Size * 2
	if len(name) != hashChars+len(indexCacheExtension) || name[hashChars:] != indexCacheExtension {
		return false
	}
	for i := 0; i < hashChars; i++ {
		c := name[i]
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')) {
			return false
		}
	}
	return true
}

func isRecognizedIndexCacheTempName(name string) bool {
	const finalNameBytes = sha256.Size*2 + len(indexCacheExtension)
	const marker = ".tmp."
	const randomHexBytes = 32
	if len(name) != finalNameBytes+len(marker)+randomHexBytes || !isRecognizedIndexCacheName(name[:finalNameBytes]) || name[finalNameBytes:finalNameBytes+len(marker)] != marker {
		return false
	}
	for i := finalNameBytes + len(marker); i < len(name); i++ {
		c := name[i]
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')) {
			return false
		}
	}
	return true
}

func isRecognizedLegacyCentralIndexCacheName(name string) bool {
	const hashChars = sha256.Size * 2
	if len(name) != hashChars+len(legacyCentralIndexCacheExtension) || name[hashChars:] != legacyCentralIndexCacheExtension {
		return false
	}
	for i := 0; i < hashChars; i++ {
		c := name[i]
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')) {
			return false
		}
	}
	return true
}

func openIndexCacheTemp(dir, finalName string) (*os.File, string, error) {
	if !isRecognizedIndexCacheName(finalName) {
		return nil, "", errors.New("line-index cache target name is not owned")
	}
	var random [16]byte
	for attempt := 0; attempt < 8; attempt++ {
		if _, err := io.ReadFull(rand.Reader, random[:]); err != nil {
			return nil, "", err
		}
		path := filepath.Join(dir, finalName+".tmp."+hex.EncodeToString(random[:]))
		file, err := createIndexCacheTempFile(path)
		if err == nil {
			return file, path, nil
		}
		if !errors.Is(err, os.ErrExist) {
			return nil, "", err
		}
	}
	return nil, "", errors.New("could not create a unique line-index cache temp")
}

func sameCachePath(first, second string) bool {
	if runtime.GOOS == "windows" {
		return filepath.Clean(first) == filepath.Clean(second) || bytes.EqualFold([]byte(filepath.Clean(first)), []byte(filepath.Clean(second)))
	}
	return filepath.Clean(first) == filepath.Clean(second)
}

func publishIndexCacheTemp(tempPath, target string) error {
	if info, err := os.Lstat(target); err == nil {
		if !info.Mode().IsRegular() {
			return errors.New("line-index cache target became a non-regular entry")
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	// Keep publication as one namespace operation. In particular, never remove
	// the current cache before replacing it on Windows: a virus scanner, search
	// indexer, or backup process can briefly hold the destination without delete
	// sharing and turn a remove-then-rename fallback into a missing-cache window.
	return replaceIndexCacheFile(tempPath, target)
}
